package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"llmgateway/internal/apierr"
)

// ProtocolOpenAIChat 是 OpenAI Chat Completions 协议标识。
// DeepSeek 官方开放平台目前对外只提供这一套协议，所以这个适配器用于「真实 Key 端到端验证」。
const ProtocolOpenAIChat = "openai_chat"

// OpenAIChatAdapter 适配 OpenAI Chat Completions API（POST /chat/completions）。
//
// 与另外两种协议的差异：
//   - system 就放在 messages 数组里（角色 system），不像 Responses 有 instructions
//   - content 是纯字符串，不是部件数组
//   - 结构化输出只支持 response_format.type=json_object，
//     json_schema 要降级：schema 转成自然语言约束塞进 system
//   - 流式没有 event 名，全是匿名 data 行，以 data: [DONE] 收尾；
//     用量要显式打开 stream_options.include_usage 才会在最后一个 chunk 里出现
type OpenAIChatAdapter struct {
	tr     *transport
	apiKey string
}

func NewOpenAIChatAdapter(opt Options) *OpenAIChatAdapter {
	return &OpenAIChatAdapter{tr: newTransport(opt), apiKey: opt.APIKey}
}

func (a *OpenAIChatAdapter) Protocol() string { return ProtocolOpenAIChat }

type ocMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ocResponseFormat struct {
	Type string `json:"type"`
}

type ocStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type ocChatRequest struct {
	Model          string            `json:"model"`
	Messages       []ocMessage       `json:"messages"`
	MaxTokens      int               `json:"max_tokens,omitempty"`
	Temperature    *float64          `json:"temperature,omitempty"`
	Stream         bool              `json:"stream,omitempty"`
	StreamOptions  *ocStreamOptions  `json:"stream_options,omitempty"`
	ResponseFormat *ocResponseFormat `json:"response_format,omitempty"`
}

type ocUsage struct {
	PromptTokens         int `json:"prompt_tokens"`
	CompletionTokens     int `json:"completion_tokens"`
	TotalTokens          int `json:"total_tokens"`
	PromptCacheHitTokens int `json:"prompt_cache_hit_tokens"`
	PromptTokensDetails  struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

type ocChatResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index   int `json:"index"`
		Message struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"message"`
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *ocUsage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// buildPayload 把统一请求翻译成 Chat Completions 请求体。
// 流转顺序：① system 拼回 messages 数组首位 ② 消息直译成 role/content 字符串
// ③ json_schema 无法原生表达，降级成 json_object + system 里补 schema 说明
func (a *OpenAIChatAdapter) buildPayload(req *Request) *ocChatRequest {
	p := &ocChatRequest{
		Model:       firstNonEmpty(req.UpstreamModel, req.Model),
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		Stream:      req.Stream,
	}
	systemParts := []string{}
	if req.System != "" {
		systemParts = append(systemParts, req.System)
	}
	// ③ 结构化输出降级处理放在拼 system 之前，这样 schema 说明能进到 system 里
	if req.ResponseFormat.IsStructured() {
		p.ResponseFormat = &ocResponseFormat{Type: FormatJSONObject}
		hint := "你必须只输出一个合法的 JSON 对象，不要输出任何解释文字或 Markdown 代码围栏。"
		if req.ResponseFormat.Type == FormatJSONSchema && req.ResponseFormat.JSONSchema != nil {
			if raw, err := json.Marshal(req.ResponseFormat.JSONSchema.Schema); err == nil {
				hint += "\n输出必须满足以下 JSON Schema：\n" + string(raw)
			}
		}
		systemParts = append(systemParts, hint)
	}
	// ① system 作为首条消息
	if s := strings.Join(systemParts, "\n\n"); s != "" {
		p.Messages = append(p.Messages, ocMessage{Role: RoleSystem, Content: s})
	}
	// ② 其余消息直译
	for _, m := range req.Messages {
		p.Messages = append(p.Messages, ocMessage{Role: m.Role, Content: m.Content})
	}
	if req.Stream {
		p.StreamOptions = &ocStreamOptions{IncludeUsage: true}
	}
	return p
}

func (a *OpenAIChatAdapter) headers() map[string]string {
	h := map[string]string{}
	if a.apiKey != "" {
		h["Authorization"] = "Bearer " + a.apiKey
	}
	return h
}

// mapChatFinish 把 Chat 协议的 finish_reason 翻译成统一值。
func mapChatFinish(reason string) string {
	switch reason {
	case "length":
		return FinishLength
	case "content_filter":
		return FinishContentFilter
	case "stop", "tool_calls", "":
		return FinishStop
	default:
		return FinishStop
	}
}

func (a *OpenAIChatAdapter) toUnified(body *ocChatResponse, raw json.RawMessage) *Response {
	resp := &Response{
		ID:           firstNonEmpty(body.ID, "chatcmpl_unknown"),
		Model:        body.Model,
		FinishReason: FinishStop,
		RawUpstream:  raw,
	}
	if len(body.Choices) > 0 {
		resp.Content = body.Choices[0].Message.Content
		resp.FinishReason = mapChatFinish(body.Choices[0].FinishReason)
	}
	if body.Usage != nil {
		cached := body.Usage.PromptTokensDetails.CachedTokens
		if cached == 0 {
			cached = body.Usage.PromptCacheHitTokens // DeepSeek 用的是这个字段名
		}
		resp.Usage = Usage{
			PromptTokens:     body.Usage.PromptTokens,
			CompletionTokens: body.Usage.CompletionTokens,
			TotalTokens:      body.Usage.TotalTokens,
			CachedTokens:     cached,
			ReasoningTokens:  body.Usage.CompletionTokensDetails.ReasoningTokens,
		}
		if resp.Usage.TotalTokens == 0 {
			resp.Usage.TotalTokens = resp.Usage.PromptTokens + resp.Usage.CompletionTokens
		}
	}
	return resp
}

func (a *OpenAIChatAdapter) Invoke(ctx context.Context, req *Request) (*Response, error) {
	ctx, cancel := a.tr.callTimeout(ctx)
	defer cancel()

	httpResp, aerr := a.tr.post(ctx, "/chat/completions", a.buildPayload(req), a.headers())
	if aerr != nil {
		return nil, aerr
	}
	defer httpResp.Body.Close()

	var body ocChatResponse
	raw, aerr := decodeJSON(httpResp.Body, &body)
	if aerr != nil {
		return nil, aerr
	}
	if body.Error != nil && body.Error.Message != "" {
		return nil, apierr.New(apierr.CodeUpstreamError, "上游返回错误: %s", body.Error.Message)
	}
	return a.toUnified(&body, raw), nil
}

// Stream 流式调用。本协议的 SSE 没有 event 名，靠 data: [DONE] 收尾。
func (a *OpenAIChatAdapter) Stream(ctx context.Context, req *Request) (<-chan StreamEvent, error) {
	payload := a.buildPayload(req)
	payload.Stream = true
	payload.StreamOptions = &ocStreamOptions{IncludeUsage: true}

	h := a.headers()
	h["Accept"] = "text/event-stream"
	httpResp, aerr := a.tr.post(ctx, "/chat/completions", payload, h)
	if aerr != nil {
		return nil, aerr
	}

	out := make(chan StreamEvent, 32)
	go func() {
		defer close(out)
		defer httpResp.Body.Close()

		reader := newSSEReader(httpResp.Body)
		acc := &Response{ID: "chatcmpl_stream", FinishReason: FinishStop}
		var sb strings.Builder
		// 本协议的终止标记是 data: [DONE]，部分实现只在末个 chunk 给 finish_reason。
		// 两者都没出现就 EOF，说明流被截断。
		sawTerminal := false

		emitDone := func() {
			acc.Content = sb.String()
			if acc.Usage.TotalTokens == 0 {
				acc.Usage.TotalTokens = acc.Usage.PromptTokens + acc.Usage.CompletionTokens
			}
			out <- StreamEvent{Type: EventDone, Response: acc}
		}

		for {
			ev, err := reader.Next()
			if err != nil {
				if errors.Is(err, io.EOF) {
					if !sawTerminal {
						out <- StreamEvent{Type: EventError, Err: apierr.New(apierr.CodeUpstreamError,
							"上游流被截断：已收到 %d 字节内容，但既无 [DONE] 也无 finish_reason", sb.Len())}
						return
					}
					emitDone()
					return
				}
				out <- StreamEvent{Type: EventError, Err: apierr.FromTransport(err)}
				return
			}
			// ① [DONE] 是本协议的结束标记
			if ev.Data == "" {
				continue
			}
			if strings.TrimSpace(ev.Data) == "[DONE]" {
				sawTerminal = true
				emitDone()
				return
			}
			// ② 每个 chunk 里 choices[0].delta.content 是增量文本
			var chunk ocChatResponse
			if json.Unmarshal([]byte(ev.Data), &chunk) != nil {
				continue
			}
			if chunk.ID != "" {
				acc.ID = chunk.ID
			}
			if chunk.Model != "" {
				acc.Model = chunk.Model
			}
			if len(chunk.Choices) > 0 {
				c := chunk.Choices[0]
				if c.Delta.Content != "" {
					sb.WriteString(c.Delta.Content)
					out <- StreamEvent{Type: EventDelta, Delta: c.Delta.Content}
				}
				if c.FinishReason != "" {
					acc.FinishReason = mapChatFinish(c.FinishReason)
					sawTerminal = true
				}
			}
			// ③ 最后一个 chunk（choices 为空）携带 usage
			if chunk.Usage != nil {
				u := a.toUnified(&ocChatResponse{Usage: chunk.Usage}, nil).Usage
				acc.Usage = u
			}
		}
	}()
	return out, nil
}
