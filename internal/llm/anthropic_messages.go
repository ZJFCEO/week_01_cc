package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"llmgateway/internal/apierr"
)

// ProtocolAnthropicMessages 是 Anthropic Messages API 协议标识。
const ProtocolAnthropicMessages = "anthropic_messages"

// anthropicVersion 是 Messages API 必带的版本头。
const anthropicVersion = "2023-06-01"

// structuredToolName 是「用工具调用模拟结构化输出」时使用的工具名。
const structuredToolName = "structured_output"

// defaultAnthropicMaxTokens 是 max_tokens 的兜底值。
// Messages 协议里 max_tokens 是必填字段，而统一层允许不填，差异在这里补齐。
const defaultAnthropicMaxTokens = 2048

// AnthropicMessagesAdapter 适配 Anthropic Messages API（POST /v1/messages）。
//
// 与 OpenAI Responses 协议的关键差异（全部在本文件内消化）：
//   - 鉴权：x-api-key + anthropic-version 头，而不是 Authorization: Bearer
//   - max_tokens 必填，不填直接 400
//   - messages 必须 user/assistant 严格交替且首条为 user，需要做归并/补位
//   - 没有 response_format：结构化输出要翻译成 tools + tool_choice 强制工具调用，
//     再把 tool_use.input 当作 JSON 文本还原回统一层
//   - 输出在 content[]，用量字段叫 input_tokens/cache_read_input_tokens
//   - 流式是 message_start / content_block_delta / message_delta / message_stop 四段式，
//     且结构化场景下增量走 input_json_delta.partial_json 而不是 text_delta.text
type AnthropicMessagesAdapter struct {
	tr     *transport
	apiKey string
}

func NewAnthropicMessagesAdapter(opt Options) *AnthropicMessagesAdapter {
	return &AnthropicMessagesAdapter{tr: newTransport(opt), apiKey: opt.APIKey}
}

func (a *AnthropicMessagesAdapter) Protocol() string { return ProtocolAnthropicMessages }

// ---------- 协议专属的请求/响应结构 ----------

type anContentBlock struct {
	Type  string          `json:"type"`            // text / tool_use
	Text  string          `json:"text,omitempty"`  // type=text 时有效
	ID    string          `json:"id,omitempty"`    // type=tool_use 时有效
	Name  string          `json:"name,omitempty"`  // type=tool_use 时有效
	Input json.RawMessage `json:"input,omitempty"` // type=tool_use 时的结构化载荷
}

type anMessage struct {
	Role    string           `json:"role"`
	Content []anContentBlock `json:"content"`
}

type anTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

type anToolChoice struct {
	Type string `json:"type"`           // tool / auto / any
	Name string `json:"name,omitempty"` // Type=tool 时指定强制调用哪个工具
}

type anMessagesRequest struct {
	Model       string        `json:"model"`
	System      string        `json:"system,omitempty"`
	Messages    []anMessage   `json:"messages"`
	MaxTokens   int           `json:"max_tokens"` // 必填，无 omitempty
	Temperature *float64      `json:"temperature,omitempty"`
	Stream      bool          `json:"stream,omitempty"`
	Tools       []anTool      `json:"tools,omitempty"`
	ToolChoice  *anToolChoice `json:"tool_choice,omitempty"`
}

type anUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

type anMessageBody struct {
	ID         string           `json:"id"`
	Type       string           `json:"type"`
	Role       string           `json:"role"`
	Model      string           `json:"model"`
	Content    []anContentBlock `json:"content"`
	StopReason string           `json:"stop_reason"`
	Usage      *anUsage         `json:"usage"`
	Error      *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// ---------- 请求方向：统一请求 -> Messages 协议 ----------

// normalizeMessages 处理 Messages 协议对消息序列的硬性要求。
// 流转顺序：① 把 system 角色的消息抽出来 ② 归并相邻同角色消息
// ③ 若首条不是 user，补一条占位 user 消息，避免上游 400
func normalizeMessages(system string, msgs []Message) (string, []anMessage) {
	systemParts := []string{}
	if system != "" {
		systemParts = append(systemParts, system)
	}
	var flat []Message
	for _, m := range msgs {
		// ① system 消息不能出现在 messages 数组里，统一并入顶层 system
		if m.Role == RoleSystem {
			systemParts = append(systemParts, m.Content)
			continue
		}
		// ② 相邻同角色归并成一条，满足严格交替要求
		if n := len(flat); n > 0 && flat[n-1].Role == m.Role {
			flat[n-1].Content += "\n\n" + m.Content
			continue
		}
		flat = append(flat, m)
	}
	// ③ 首条必须是 user
	if len(flat) > 0 && flat[0].Role != RoleUser {
		flat = append([]Message{{Role: RoleUser, Content: "(继续)"}}, flat...)
	}
	out := make([]anMessage, 0, len(flat))
	for _, m := range flat {
		out = append(out, anMessage{
			Role:    m.Role,
			Content: []anContentBlock{{Type: "text", Text: m.Content}},
		})
	}
	return strings.Join(systemParts, "\n\n"), out
}

// buildPayload 把统一请求翻译成 Messages API 请求体。
// 流转顺序：① 消息归一 ② 补 max_tokens 默认值 ③ 结构化输出降级成强制工具调用
func (a *AnthropicMessagesAdapter) buildPayload(req *Request) *anMessagesRequest {
	system, msgs := normalizeMessages(req.System, req.Messages)
	p := &anMessagesRequest{
		Model:       firstNonEmpty(req.UpstreamModel, req.Model),
		System:      system,
		Messages:    msgs,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		Stream:      req.Stream,
	}
	// ② max_tokens 是本协议的必填项，统一层没给就兜底
	if p.MaxTokens <= 0 {
		p.MaxTokens = defaultAnthropicMaxTokens
	}
	// ③ 本协议没有 response_format：用「强制调用一个入参就是目标 schema 的工具」来约束 JSON
	if req.ResponseFormat.IsStructured() {
		schema := map[string]any{"type": "object"}
		desc := "以合法 JSON 返回最终结果"
		if req.ResponseFormat.Type == FormatJSONSchema && req.ResponseFormat.JSONSchema != nil {
			if len(req.ResponseFormat.JSONSchema.Schema) > 0 {
				schema = req.ResponseFormat.JSONSchema.Schema
			}
			if req.ResponseFormat.JSONSchema.Name != "" {
				desc = "按 " + req.ResponseFormat.JSONSchema.Name + " 结构返回最终结果"
			}
		}
		p.Tools = []anTool{{Name: structuredToolName, Description: desc, InputSchema: schema}}
		// tool_choice 用 any 而不是 {type:tool,name:...}：
		// 结构化模式下只声明了这一个工具，二者语义等价，但 any 的兼容性更好——
		// DeepSeek 的 Anthropic 兼容端点在思考模式下会拒绝 {type:tool}，
		// 报 "Thinking mode does not support this tool_choice"。
		p.ToolChoice = &anToolChoice{Type: "any"}
	}
	return p
}

// headers 组装本协议的鉴权与版本头。
func (a *AnthropicMessagesAdapter) headers() map[string]string {
	h := map[string]string{
		"anthropic-version": anthropicVersion, // Responses 协议没有这个头
	}
	if a.apiKey != "" {
		h["x-api-key"] = a.apiKey // 不是 Authorization: Bearer
	}
	return h
}

// mapStopReason 把 Messages 协议的 stop_reason 翻译成统一 finish_reason。
func mapStopReason(reason string) string {
	switch reason {
	case "max_tokens":
		return FinishLength
	case "refusal":
		return FinishContentFilter
	case "end_turn", "stop_sequence", "tool_use", "":
		return FinishStop
	default:
		return FinishStop
	}
}

// ---------- 响应方向：Messages 协议 -> 统一响应 ----------

// toUnified 把 Messages 返回体翻译成统一响应。
func (a *AnthropicMessagesAdapter) toUnified(body *anMessageBody, raw json.RawMessage) *Response {
	var sb strings.Builder
	// ① content 是块数组：text 块直接取文本；tool_use 块说明走的是结构化路径，
	//    把它的 input 原样序列化成 JSON 文本，还原成统一层的「一段 JSON 字符串」
	for _, blk := range body.Content {
		switch blk.Type {
		case "text", "":
			sb.WriteString(blk.Text)
		case "thinking", "redacted_thinking":
			// 思考块不属于最终答案，直接跳过（DeepSeek 的兼容端点会带这种块）
			continue
		case "tool_use":
			if blk.Name == structuredToolName && len(blk.Input) > 0 {
				sb.Write(blk.Input)
			}
		}
	}
	resp := &Response{
		ID:           firstNonEmpty(body.ID, "msg_unknown"),
		Model:        body.Model,
		Content:      sb.String(),
		FinishReason: mapStopReason(body.StopReason),
		RawUpstream:  raw,
	}
	// ② 用量字段名归一：input_tokens -> prompt_tokens，cache_read_input_tokens -> cached_tokens
	if body.Usage != nil {
		resp.Usage = Usage{
			PromptTokens:     body.Usage.InputTokens,
			CompletionTokens: body.Usage.OutputTokens,
			TotalTokens:      body.Usage.InputTokens + body.Usage.OutputTokens,
			CachedTokens:     body.Usage.CacheReadInputTokens,
		}
	}
	return resp
}

// Invoke 非流式调用。
func (a *AnthropicMessagesAdapter) Invoke(ctx context.Context, req *Request) (*Response, error) {
	ctx, cancel := a.tr.callTimeout(ctx)
	defer cancel()

	httpResp, aerr := a.tr.post(ctx, "/v1/messages", a.buildPayload(req), a.headers())
	if aerr != nil {
		return nil, aerr
	}
	defer httpResp.Body.Close()

	var body anMessageBody
	raw, aerr := decodeJSON(httpResp.Body, &body)
	if aerr != nil {
		return nil, aerr
	}
	if body.Error != nil && body.Error.Message != "" {
		return nil, apierr.New(apierr.CodeUpstreamError, "上游返回错误: %s", body.Error.Message)
	}
	return a.toUnified(&body, raw), nil
}

// Stream 流式调用。
// 流转顺序：① 建流（错误同步返回以便重试）② 解析四段式事件
// ③ text_delta 与 input_json_delta 统一成同一种 delta 事件
// ④ message_start 拿输入用量，message_delta 拿输出用量与 stop_reason
func (a *AnthropicMessagesAdapter) Stream(ctx context.Context, req *Request) (<-chan StreamEvent, error) {
	payload := a.buildPayload(req)
	payload.Stream = true

	h := a.headers()
	h["Accept"] = "text/event-stream"
	httpResp, aerr := a.tr.post(ctx, "/v1/messages", payload, h)
	if aerr != nil {
		return nil, aerr
	}

	out := make(chan StreamEvent, 32)
	go func() {
		defer close(out)
		defer httpResp.Body.Close()

		reader := newSSEReader(httpResp.Body)
		acc := &Response{ID: "msg_stream", FinishReason: FinishStop}
		var sb strings.Builder

		emitDone := func() {
			acc.Content = sb.String()
			acc.Usage.TotalTokens = acc.Usage.PromptTokens + acc.Usage.CompletionTokens
			out <- StreamEvent{Type: EventDone, Response: acc}
		}

		for {
			ev, err := reader.Next()
			if err != nil {
				if errors.Is(err, io.EOF) {
					emitDone()
					return
				}
				out <- StreamEvent{Type: EventError, Err: apierr.FromTransport(err)}
				return
			}
			if ev.Data == "" {
				continue
			}
			switch ev.Name {
			case "message_start":
				// ④ 输入用量只在这一条事件里出现，必须在这里抓住
				var d struct {
					Message anMessageBody `json:"message"`
				}
				if json.Unmarshal([]byte(ev.Data), &d) == nil {
					acc.ID = firstNonEmpty(d.Message.ID, acc.ID)
					acc.Model = d.Message.Model
					if d.Message.Usage != nil {
						acc.Usage.PromptTokens = d.Message.Usage.InputTokens
						acc.Usage.CachedTokens = d.Message.Usage.CacheReadInputTokens
						acc.Usage.CompletionTokens = d.Message.Usage.OutputTokens
					}
				}
			case "content_block_delta":
				// ③ 两种增量类型翻译成同一种统一 delta：
				//    普通文本走 text_delta.text；结构化输出走 input_json_delta.partial_json
				var d struct {
					Delta struct {
						Type        string `json:"type"`
						Text        string `json:"text"`
						PartialJSON string `json:"partial_json"`
					} `json:"delta"`
				}
				if json.Unmarshal([]byte(ev.Data), &d) != nil {
					continue
				}
				chunk := d.Delta.Text
				if d.Delta.Type == "input_json_delta" {
					chunk = d.Delta.PartialJSON
				}
				if chunk != "" {
					sb.WriteString(chunk)
					out <- StreamEvent{Type: EventDelta, Delta: chunk}
				}
			case "message_delta":
				// ④ 输出用量与 stop_reason 在这一条事件里给出
				var d struct {
					Delta struct {
						StopReason string `json:"stop_reason"`
					} `json:"delta"`
					Usage *anUsage `json:"usage"`
				}
				if json.Unmarshal([]byte(ev.Data), &d) == nil {
					if d.Delta.StopReason != "" {
						acc.FinishReason = mapStopReason(d.Delta.StopReason)
					}
					if d.Usage != nil {
						if d.Usage.OutputTokens > 0 {
							acc.Usage.CompletionTokens = d.Usage.OutputTokens
						}
						if d.Usage.InputTokens > 0 {
							acc.Usage.PromptTokens = d.Usage.InputTokens
						}
					}
				}
			case "message_stop":
				emitDone()
				return
			case "error":
				var d struct {
					Error struct {
						Type    string `json:"type"`
						Message string `json:"message"`
					} `json:"error"`
				}
				_ = json.Unmarshal([]byte(ev.Data), &d)
				out <- StreamEvent{Type: EventError, Err: apierr.New(apierr.CodeUpstreamError, "%s", firstNonEmpty(d.Error.Message, "上游流式返回错误"))}
				return
			}
		}
	}()
	return out, nil
}
