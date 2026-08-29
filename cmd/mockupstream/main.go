// Command mockupstream 是内置的「假上游」，同时实现三套真实协议，用于完全离线的功能验收。
//
//	POST /v1/responses       OpenAI Responses API 协议
//	POST /v1/messages        Anthropic Messages API 协议
//	POST /chat/completions   OpenAI Chat Completions 协议
//
// 它有两个用途：
//  1. 没有真实 Key 时也能端到端跑通六大功能点
//  2. 严格校验请求体是否真的符合各自协议（例如 Messages 协议必须带
//     anthropic-version 头和 max_tokens），从而证明「协议差异确实被适配器吃掉了」
//
// 故障注入：在任意一条消息文本里写 [[MOCK:key=value,...]] 即可，
// 这些指令会随着协议翻译一路传到这里，所以能真实地触发网关的重试与错误分流。
//
//	[[MOCK:fail=2,id=abc]]  同一个 id 的前 2 次请求返回 500
//	[[MOCK:status=429]]     直接返回指定状态码
//	[[MOCK:delay=800]]      延迟 800ms 再响应
//	[[MOCK:badjson]]        结构化场景下故意返回非法 JSON
//	[[MOCK:chunks=8]]       流式时切成 8 块下发
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------- 故障注入指令 ----------

var mockPattern = regexp.MustCompile(`\[\[MOCK:([^\]]*)\]\]`)

type directives struct {
	FailTimes int
	FailID    string
	Status    int
	DelayMs   int
	BadJSON   bool
	Chunks    int
}

// parseDirectives 从整段文本里解析 [[MOCK:...]] 指令。
func parseDirectives(text string) directives {
	d := directives{Chunks: 5}
	m := mockPattern.FindStringSubmatch(text)
	if m == nil {
		return d
	}
	for _, kv := range strings.Split(m[1], ",") {
		key, value, _ := strings.Cut(strings.TrimSpace(kv), "=")
		switch strings.TrimSpace(key) {
		case "fail":
			d.FailTimes, _ = strconv.Atoi(value)
		case "id":
			d.FailID = value
		case "status":
			d.Status, _ = strconv.Atoi(value)
		case "delay":
			d.DelayMs, _ = strconv.Atoi(value)
		case "badjson":
			d.BadJSON = true
		case "chunks":
			if n, err := strconv.Atoi(value); err == nil && n > 0 {
				d.Chunks = n
			}
		}
	}
	return d
}

// failCounter 记录每个 fail id 已经失败过几次，用来实现「前 N 次失败、第 N+1 次成功」。
type failCounter struct {
	mu sync.Mutex
	n  map[string]int
}

var counter = &failCounter{n: map[string]int{}}

// shouldFail 判断本次是否应该注入失败，并把计数 +1。
func (f *failCounter) shouldFail(d directives) bool {
	if d.FailTimes <= 0 {
		return false
	}
	key := d.FailID
	if key == "" {
		key = "default"
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := f.n[key]
	if seen < d.FailTimes {
		f.n[key] = seen + 1
		return true
	}
	return false
}

// lastRequest 记录每个协议端点最近一次收到的原始请求，供验收脚本取证：
// 直接看上游真正收到了什么，就能证明两个适配器发出的确实是两种不同协议的报文。
type lastRequest struct {
	mu   sync.Mutex
	data map[string]map[string]any
}

var probe = &lastRequest{data: map[string]map[string]any{}}

func (l *lastRequest) put(proto string, headers http.Header, body []byte) {
	h := map[string]string{}
	for _, k := range []string{"Authorization", "X-Api-Key", "Anthropic-Version", "Accept", "Content-Type"} {
		if v := headers.Get(k); v != "" {
			// Key 只回显前缀与长度，绝不回显明文
			if k == "Authorization" || k == "X-Api-Key" {
				v = maskSecret(v)
			}
			h[strings.ToLower(k)] = v
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.data[proto] = map[string]any{
		"headers": h,
		"body":    json.RawMessage(append([]byte(nil), body...)),
		"at":      time.Now().Format(time.RFC3339Nano),
	}
}

func (l *lastRequest) snapshot() map[string]map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]map[string]any{}
	for k, v := range l.data {
		out[k] = v
	}
	return out
}

// maskSecret 把密钥掩码成 "前4位***长度"，日志与探针都不会泄漏明文。
func maskSecret(v string) string {
	raw := strings.TrimPrefix(v, "Bearer ")
	if raw == "" {
		return "(empty)"
	}
	prefix := raw
	if len([]rune(prefix)) > 4 {
		prefix = string([]rune(prefix)[:4])
	}
	return fmt.Sprintf("%s***(len=%d)", prefix, len(raw))
}

func (f *failCounter) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n = map[string]int{}
}

// ---------- 通用工具 ----------

// approxTokens 用「字符数/2」粗略估 Token，保证同样输入得到稳定数字，方便断言。
func approxTokens(s string) int {
	n := len([]rune(s)) / 2
	if n < 1 && len(s) > 0 {
		n = 1
	}
	return n
}

// readAndProbe 读出请求体、存入探针，并返回可重复解析的字节。
func readAndProbe(proto string, r *http.Request) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	probe.put(proto, r.Header, raw)
	return raw, nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// protocolError 按各协议自己的错误结构返回，让网关的错误归一逻辑得到真实检验。
func protocolError(w http.ResponseWriter, proto string, status int, msg string) {
	switch proto {
	case "anthropic":
		writeJSON(w, status, map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "invalid_request_error", "message": msg},
		})
	default:
		writeJSON(w, status, map[string]any{
			"error": map[string]any{"type": "invalid_request_error", "message": msg},
		})
	}
}

// buildReply 生成一段确定性的回复文本。
func buildReply(proto, prompt string) string {
	clean := strings.TrimSpace(mockPattern.ReplaceAllString(prompt, ""))
	runes := []rune(clean)
	if len(runes) > 60 {
		runes = runes[:60]
	}
	return fmt.Sprintf("【mock:%s】已收到 %d 个字符的输入：%s。这是内置假上游按 %s 协议生成的回复。",
		proto, len([]rune(clean)), string(runes), proto)
}

// genFromSchema 按 JSON Schema 造一个满足约束的样例对象，用于结构化输出。
// 覆盖 type/properties/required/enum/items，enum 一律取第一个值以保证通过校验。
func genFromSchema(schema map[string]any, seed string) any {
	if schema == nil {
		return map[string]any{"result": seed}
	}
	if enum, ok := schema["enum"].([]any); ok && len(enum) > 0 {
		return enum[0]
	}
	switch t, _ := schema["type"].(string); t {
	case "object", "":
		out := map[string]any{}
		props, _ := schema["properties"].(map[string]any)
		if len(props) == 0 {
			return map[string]any{"result": seed}
		}
		for key, raw := range props {
			sub, _ := raw.(map[string]any)
			out[key] = genFromSchema(sub, seed)
		}
		return out
	case "array":
		items, _ := schema["items"].(map[string]any)
		return []any{genFromSchema(items, seed)}
	case "integer":
		return 7
	case "number":
		return 0.86
	case "boolean":
		return true
	default:
		runes := []rune(seed)
		if len(runes) > 24 {
			runes = runes[:24]
		}
		return string(runes)
	}
}

// splitChunks 把文本按大致均匀的份数切开，用于流式逐块下发。
func splitChunks(s string, n int) []string {
	runes := []rune(s)
	if n <= 1 || len(runes) <= n {
		return []string{s}
	}
	size := (len(runes) + n - 1) / n
	var out []string
	for i := 0; i < len(runes); i += size {
		end := i + size
		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, string(runes[i:end]))
	}
	return out
}

// sseWriter 负责把事件写成 SSE 并立刻 flush。
type sseWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func newSSEWriter(w http.ResponseWriter) (*sseWriter, bool) {
	f, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	f.Flush()
	return &sseWriter{w: w, f: f}, true
}

func (s *sseWriter) send(event string, payload any) {
	data, _ := json.Marshal(payload)
	if event != "" {
		fmt.Fprintf(s.w, "event: %s\n", event)
	}
	fmt.Fprintf(s.w, "data: %s\n\n", data)
	s.f.Flush()
}

func (s *sseWriter) raw(data string) {
	fmt.Fprintf(s.w, "data: %s\n\n", data)
	s.f.Flush()
}

// preflight 处理所有协议共有的前置逻辑：延迟注入、状态码注入、失败注入。
// 返回 true 表示已经写完响应，处理函数应当直接返回。
func preflight(w http.ResponseWriter, proto string, d directives) bool {
	if d.DelayMs > 0 {
		time.Sleep(time.Duration(d.DelayMs) * time.Millisecond)
	}
	if d.Status > 0 && d.Status != 200 {
		protocolError(w, proto, d.Status, fmt.Sprintf("mock 注入的状态码 %d", d.Status))
		return true
	}
	if counter.shouldFail(d) {
		protocolError(w, proto, http.StatusInternalServerError, "mock 注入的上游临时故障，请重试")
		return true
	}
	return false
}

// ---------- 协议一：OpenAI Responses API ----------

type oaFormat struct {
	Type   string         `json:"type"`
	Name   string         `json:"name"`
	Schema map[string]any `json:"schema"`
}

type oaRequest struct {
	Model        string `json:"model"`
	Instructions string `json:"instructions"`
	Input        []struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"input"`
	MaxOutputTokens int  `json:"max_output_tokens"`
	Stream          bool `json:"stream"`
	Text            *struct {
		Format oaFormat `json:"format"`
	} `json:"text"`
	// 下面两个字段属于别的协议，一旦出现说明适配器串味了
	Messages  json.RawMessage `json:"messages"`
	MaxTokens *int            `json:"max_tokens"`
}

func handleResponses(w http.ResponseWriter, r *http.Request) {
	raw, err := readAndProbe("openai_responses", r)
	if err != nil {
		protocolError(w, "openai", http.StatusBadRequest, "读取请求体失败")
		return
	}
	var req oaRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		protocolError(w, "openai", http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	// ① 协议校验：Responses 协议用 input + max_output_tokens，
	//    出现 messages / max_tokens 说明适配器把 Anthropic 那套写串了
	if len(req.Messages) > 0 || req.MaxTokens != nil {
		protocolError(w, "openai", http.StatusBadRequest, "Responses 协议不接受 messages/max_tokens 字段，应使用 input/max_output_tokens")
		return
	}
	if len(req.Input) == 0 {
		protocolError(w, "openai", http.StatusBadRequest, "input 不能为空")
		return
	}
	if req.Model == "" {
		protocolError(w, "openai", http.StatusBadRequest, "model 不能为空")
		return
	}
	// ② 拼出全部输入文本，同时校验部件类型
	var sb strings.Builder
	sb.WriteString(req.Instructions)
	for _, item := range req.Input {
		for _, part := range item.Content {
			if part.Type != "input_text" && part.Type != "output_text" {
				protocolError(w, "openai", http.StatusBadRequest,
					"Responses 协议的 content 部件类型必须是 input_text/output_text，收到 "+part.Type)
				return
			}
			sb.WriteString(part.Text)
		}
	}
	prompt := sb.String()
	d := parseDirectives(prompt)
	if preflight(w, "openai", d) {
		return
	}

	// ③ 生成回复：结构化时按 text.format 里的 schema 造 JSON
	content := buildReply("openai_responses", prompt)
	if req.Text != nil && (req.Text.Format.Type == "json_object" || req.Text.Format.Type == "json_schema") {
		if d.BadJSON {
			content = "抱歉，我先解释一下：{ 这不是合法 JSON"
		} else {
			obj := genFromSchema(req.Text.Format.Schema, buildReply("openai_responses", prompt))
			raw, _ := json.Marshal(obj)
			content = string(raw)
		}
	}

	promptTok := approxTokens(prompt)
	outTok := approxTokens(content)
	usage := map[string]any{
		"input_tokens":          promptTok,
		"input_tokens_details":  map[string]any{"cached_tokens": promptTok / 5},
		"output_tokens":         outTok,
		"output_tokens_details": map[string]any{"reasoning_tokens": outTok / 10},
		"total_tokens":          promptTok + outTok,
	}
	full := map[string]any{
		"id":     "resp_" + strconv.FormatInt(time.Now().UnixNano(), 36),
		"object": "response",
		"model":  req.Model,
		"status": "completed",
		"output": []any{map[string]any{
			"type":    "message",
			"role":    "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": content}},
		}},
		"usage": usage,
	}

	// ④ 非流式直接返回
	if !req.Stream {
		writeJSON(w, http.StatusOK, full)
		return
	}
	// ⑤ 流式：按 Responses 协议的事件名逐块下发
	s, ok := newSSEWriter(w)
	if !ok {
		protocolError(w, "openai", http.StatusInternalServerError, "不支持流式")
		return
	}
	s.send("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": full["id"], "status": "in_progress"}})
	for _, chunk := range splitChunks(content, d.Chunks) {
		s.send("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "delta": chunk})
		time.Sleep(15 * time.Millisecond)
	}
	s.send("response.completed", map[string]any{"type": "response.completed", "response": full})
}

// ---------- 协议二：Anthropic Messages API ----------

type anRequest struct {
	Model    string `json:"model"`
	System   string `json:"system"`
	Messages []struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"messages"`
	MaxTokens *int `json:"max_tokens"`
	Stream    bool `json:"stream"`
	Tools     []struct {
		Name        string         `json:"name"`
		InputSchema map[string]any `json:"input_schema"`
	} `json:"tools"`
	ToolChoice *struct {
		Type string `json:"type"`
		Name string `json:"name"`
	} `json:"tool_choice"`
	// 下面两个字段属于 Responses 协议，出现即说明适配器串味了
	Input           json.RawMessage `json:"input"`
	MaxOutputTokens *int            `json:"max_output_tokens"`
}

func handleMessages(w http.ResponseWriter, r *http.Request) {
	// ① 协议校验：Messages 协议必须带版本头
	if r.Header.Get("anthropic-version") == "" {
		protocolError(w, "anthropic", http.StatusBadRequest, "缺少 anthropic-version 请求头")
		return
	}
	raw, err := readAndProbe("anthropic_messages", r)
	if err != nil {
		protocolError(w, "anthropic", http.StatusBadRequest, "读取请求体失败")
		return
	}
	var req anRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		protocolError(w, "anthropic", http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	if len(req.Input) > 0 || req.MaxOutputTokens != nil {
		protocolError(w, "anthropic", http.StatusBadRequest, "Messages 协议不接受 input/max_output_tokens 字段")
		return
	}
	// ② max_tokens 在本协议里是必填
	if req.MaxTokens == nil || *req.MaxTokens <= 0 {
		protocolError(w, "anthropic", http.StatusBadRequest, "max_tokens: field required")
		return
	}
	if len(req.Messages) == 0 {
		protocolError(w, "anthropic", http.StatusBadRequest, "messages 不能为空")
		return
	}
	// ③ 首条必须是 user，且角色必须严格交替
	if req.Messages[0].Role != "user" {
		protocolError(w, "anthropic", http.StatusBadRequest, "messages 的第一条必须是 user 角色")
		return
	}
	for i := 1; i < len(req.Messages); i++ {
		if req.Messages[i].Role == req.Messages[i-1].Role {
			protocolError(w, "anthropic", http.StatusBadRequest, "messages 的角色必须 user/assistant 交替出现")
			return
		}
	}

	var sb strings.Builder
	sb.WriteString(req.System)
	for _, m := range req.Messages {
		for _, blk := range m.Content {
			sb.WriteString(blk.Text)
		}
	}
	prompt := sb.String()
	d := parseDirectives(prompt)
	if preflight(w, "anthropic", d) {
		return
	}

	// ④ 结构化输出在本协议里表现为「强制工具调用」
	// tool_choice 为 tool（指定工具）或 any（必须用某个工具）都算强制结构化输出
	structuredMode := req.ToolChoice != nil && len(req.Tools) > 0 &&
		(req.ToolChoice.Type == "tool" || req.ToolChoice.Type == "any")
	text := buildReply("anthropic_messages", prompt)
	var toolInput json.RawMessage
	if structuredMode {
		if d.BadJSON {
			// 故意退化成普通文本块，触发网关的结构化校验失败
			structuredMode = false
			text = "抱歉，我先解释一下：{ 这不是合法 JSON"
		} else {
			obj := genFromSchema(req.Tools[0].InputSchema, text)
			toolInput, _ = json.Marshal(obj)
		}
	}

	inTok := approxTokens(prompt)
	outText := text
	if structuredMode {
		outText = string(toolInput)
	}
	outTok := approxTokens(outText)
	msgID := "msg_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	usage := map[string]any{
		"input_tokens":            inTok,
		"output_tokens":           outTok,
		"cache_read_input_tokens": inTok / 5,
	}
	var contentBlocks []any
	stopReason := "end_turn"
	if structuredMode {
		contentBlocks = []any{map[string]any{
			"type":  "tool_use",
			"id":    "toolu_" + msgID,
			"name":  req.Tools[0].Name,
			"input": json.RawMessage(toolInput),
		}}
		stopReason = "tool_use"
	} else {
		contentBlocks = []any{map[string]any{"type": "text", "text": text}}
	}

	if !req.Stream {
		writeJSON(w, http.StatusOK, map[string]any{
			"id":          msgID,
			"type":        "message",
			"role":        "assistant",
			"model":       req.Model,
			"content":     contentBlocks,
			"stop_reason": stopReason,
			"usage":       usage,
		})
		return
	}

	// ⑤ 流式：Messages 协议的四段式事件
	s, ok := newSSEWriter(w)
	if !ok {
		protocolError(w, "anthropic", http.StatusInternalServerError, "不支持流式")
		return
	}
	s.send("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": msgID, "type": "message", "role": "assistant", "model": req.Model,
			"content": []any{}, "usage": map[string]any{
				"input_tokens": inTok, "output_tokens": 0, "cache_read_input_tokens": inTok / 5,
			},
		},
	})
	if structuredMode {
		// 结构化流式：增量走 input_json_delta.partial_json
		s.send("content_block_start", map[string]any{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "tool_use", "id": "toolu_" + msgID, "name": req.Tools[0].Name, "input": map[string]any{}},
		})
		for _, chunk := range splitChunks(string(toolInput), d.Chunks) {
			s.send("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": 0,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": chunk},
			})
			time.Sleep(15 * time.Millisecond)
		}
	} else {
		s.send("content_block_start", map[string]any{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
		for _, chunk := range splitChunks(text, d.Chunks) {
			s.send("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": 0,
				"delta": map[string]any{"type": "text_delta", "text": chunk},
			})
			time.Sleep(15 * time.Millisecond)
		}
	}
	s.send("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	s.send("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason},
		"usage": map[string]any{"output_tokens": outTok},
	})
	s.send("message_stop", map[string]any{"type": "message_stop"})
}

// ---------- 协议三：OpenAI Chat Completions ----------

type ocRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Stream         bool `json:"stream"`
	ResponseFormat *struct {
		Type string `json:"type"`
	} `json:"response_format"`
}

func handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	raw, err := readAndProbe("openai_chat", r)
	if err != nil {
		protocolError(w, "openai", http.StatusBadRequest, "读取请求体失败")
		return
	}
	var req ocRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		protocolError(w, "openai", http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	if len(req.Messages) == 0 {
		protocolError(w, "openai", http.StatusBadRequest, "messages 不能为空")
		return
	}
	var sb strings.Builder
	for _, m := range req.Messages {
		sb.WriteString(m.Content)
	}
	prompt := sb.String()
	d := parseDirectives(prompt)
	if preflight(w, "openai", d) {
		return
	}

	content := buildReply("openai_chat", prompt)
	if req.ResponseFormat != nil && req.ResponseFormat.Type == "json_object" {
		if d.BadJSON {
			content = "抱歉，我先解释一下：{ 这不是合法 JSON"
		} else {
			raw, _ := json.Marshal(map[string]any{"summary": content, "ok": true})
			content = string(raw)
		}
	}
	inTok, outTok := approxTokens(prompt), approxTokens(content)
	id := "chatcmpl_" + strconv.FormatInt(time.Now().UnixNano(), 36)

	if !req.Stream {
		writeJSON(w, http.StatusOK, map[string]any{
			"id": id, "object": "chat.completion", "model": req.Model,
			"choices": []any{map[string]any{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": content},
			}},
			"usage": map[string]any{
				"prompt_tokens": inTok, "completion_tokens": outTok, "total_tokens": inTok + outTok,
				"prompt_cache_hit_tokens": inTok / 5,
			},
		})
		return
	}
	s, ok := newSSEWriter(w)
	if !ok {
		protocolError(w, "openai", http.StatusInternalServerError, "不支持流式")
		return
	}
	for _, chunk := range splitChunks(content, d.Chunks) {
		s.send("", map[string]any{
			"id": id, "object": "chat.completion.chunk", "model": req.Model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": chunk}}},
		})
		time.Sleep(15 * time.Millisecond)
	}
	s.send("", map[string]any{
		"id": id, "object": "chat.completion.chunk", "model": req.Model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
		"usage": map[string]any{
			"prompt_tokens": inTok, "completion_tokens": outTok, "total_tokens": inTok + outTok,
			"prompt_cache_hit_tokens": inTok / 5,
		},
	})
	s.raw("[DONE]")
}

func main() {
	addr := flag.String("listen", ":9090", "mock 上游监听地址")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/responses", handleResponses)
	mux.HandleFunc("POST /v1/messages", handleMessages)
	mux.HandleFunc("POST /chat/completions", handleChatCompletions)
	mux.HandleFunc("POST /v1/chat/completions", handleChatCompletions)
	// 取证端点：看上游最近一次实际收到的报文（Key 已掩码）
	mux.HandleFunc("GET /mock/last-request", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, probe.snapshot())
	})
	mux.HandleFunc("POST /mock/reset", func(w http.ResponseWriter, r *http.Request) {
		counter.reset()
		writeJSON(w, http.StatusOK, map[string]any{"status": "reset"})
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})

	log.Printf("[mock] 假上游监听 %s（同时提供 Responses / Messages / Chat 三套协议）", *addr)
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("[mock] 退出: %v", err)
	}
}
