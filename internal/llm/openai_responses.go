package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"llmgateway/internal/apierr"
)

// ProtocolOpenAIResponses 是 OpenAI Responses API 协议标识。
const ProtocolOpenAIResponses = "openai_responses"

// OpenAIResponsesAdapter 适配 OpenAI Responses API（POST /v1/responses）。
//
// 与 Anthropic Messages 协议的关键差异（全部在本文件内消化）：
//   - 鉴权：Authorization: Bearer <key>
//   - 系统提示词是顶层独立字段 instructions，不放进消息数组
//   - 消息数组叫 input，每条消息的 content 是「部件数组」，
//     且用户侧部件类型是 input_text、助手侧是 output_text
//   - 结构化输出是一等公民：text.format 直接声明 json_object / json_schema
//   - 输出在 output[].content[].text，用量字段叫 input_tokens/output_tokens
//   - 流式事件名是 response.output_text.delta / response.completed
type OpenAIResponsesAdapter struct {
	tr     *transport
	apiKey string
}

func NewOpenAIResponsesAdapter(opt Options) *OpenAIResponsesAdapter {
	return &OpenAIResponsesAdapter{tr: newTransport(opt), apiKey: opt.APIKey}
}

func (a *OpenAIResponsesAdapter) Protocol() string { return ProtocolOpenAIResponses }

// ---------- 协议专属的请求/响应结构 ----------

type oaContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type oaInputItem struct {
	Role    string          `json:"role"`
	Content []oaContentPart `json:"content"`
}

type oaTextFormat struct {
	Type   string         `json:"type"`
	Name   string         `json:"name,omitempty"`
	Strict bool           `json:"strict,omitempty"`
	Schema map[string]any `json:"schema,omitempty"`
}

type oaTextConfig struct {
	Format oaTextFormat `json:"format"`
}

type oaResponsesRequest struct {
	Model           string        `json:"model"`
	Input           []oaInputItem `json:"input"`
	Instructions    string        `json:"instructions,omitempty"`
	MaxOutputTokens int           `json:"max_output_tokens,omitempty"`
	Temperature     *float64      `json:"temperature,omitempty"`
	Stream          bool          `json:"stream,omitempty"`
	Text            *oaTextConfig `json:"text,omitempty"`
}

type oaUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	TotalTokens        int `json:"total_tokens"`
	InputTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

type oaResponseBody struct {
	ID     string `json:"id"`
	Model  string `json:"model"`
	Status string `json:"status"`
	Output []struct {
		Type    string          `json:"type"`
		Role    string          `json:"role"`
		Content []oaContentPart `json:"content"`
	} `json:"output"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Usage *oaUsage `json:"usage"`
}

// ---------- 请求方向：统一请求 -> Responses 协议 ----------

// buildPayload 把统一请求翻译成 Responses API 请求体。
// 流转顺序：① 系统提示词提到顶层 instructions ② 消息转成 input 部件数组
// ③ response_format 转成 text.format ④ 其余通用参数直译
func (a *OpenAIResponsesAdapter) buildPayload(req *Request) *oaResponsesRequest {
	p := &oaResponsesRequest{
		Model:           firstNonEmpty(req.UpstreamModel, req.Model),
		MaxOutputTokens: req.MaxTokens,
		Temperature:     req.Temperature,
		Stream:          req.Stream,
	}
	// ① Responses 协议把 system 单独拎出来叫 instructions
	systemParts := []string{}
	if req.System != "" {
		systemParts = append(systemParts, req.System)
	}
	// ② 消息数组：system 角色的消息也并入 instructions，其余转成 input 部件
	for _, m := range req.Messages {
		if m.Role == RoleSystem {
			systemParts = append(systemParts, m.Content)
			continue
		}
		partType := "input_text"
		if m.Role == RoleAssistant {
			partType = "output_text" // 助手侧历史消息用 output_text 部件
		}
		p.Input = append(p.Input, oaInputItem{
			Role:    m.Role,
			Content: []oaContentPart{{Type: partType, Text: m.Content}},
		})
	}
	p.Instructions = strings.Join(systemParts, "\n\n")
	// ③ 结构化输出：本协议原生支持，直接声明即可（对照 Anthropic 适配器需要绕 tool_use）
	if req.ResponseFormat.IsStructured() {
		switch req.ResponseFormat.Type {
		case FormatJSONObject:
			p.Text = &oaTextConfig{Format: oaTextFormat{Type: FormatJSONObject}}
		case FormatJSONSchema:
			spec := req.ResponseFormat.JSONSchema
			p.Text = &oaTextConfig{Format: oaTextFormat{
				Type:   FormatJSONSchema,
				Name:   firstNonEmpty(spec.Name, "structured_output"),
				Strict: spec.Strict,
				Schema: spec.Schema,
			}}
		}
	}
	return p
}

// headers 组装本协议的鉴权头。
func (a *OpenAIResponsesAdapter) headers() map[string]string {
	h := map[string]string{}
	if a.apiKey != "" {
		h["Authorization"] = "Bearer " + a.apiKey // 与 Anthropic 的 x-api-key 不同
	}
	return h
}

// ---------- 响应方向：Responses 协议 -> 统一响应 ----------

// toUnified 把 Responses 返回体翻译成统一响应。
func (a *OpenAIResponsesAdapter) toUnified(body *oaResponseBody, raw json.RawMessage) *Response {
	var sb strings.Builder
	// ① 输出是 output[] 里 type=message 的项，其 content[] 中 type=output_text 的文本
	for _, item := range body.Output {
		if item.Type != "" && item.Type != "message" {
			continue
		}
		for _, part := range item.Content {
			if part.Type == "" || part.Type == "output_text" {
				sb.WriteString(part.Text)
			}
		}
	}
	// ② status/incomplete_details 翻译成统一的 finish_reason
	finish := FinishStop
	switch body.Status {
	case "incomplete":
		finish = FinishLength
		if body.IncompleteDetails != nil && body.IncompleteDetails.Reason == "content_filter" {
			finish = FinishContentFilter
		}
	case "failed":
		finish = FinishError
	}
	resp := &Response{
		ID:           firstNonEmpty(body.ID, "resp_unknown"),
		Model:        body.Model,
		Content:      sb.String(),
		FinishReason: finish,
		RawUpstream:  raw,
	}
	// ③ 用量字段名归一：input_tokens -> prompt_tokens，并保留缓存/思维链分类
	if body.Usage != nil {
		resp.Usage = Usage{
			PromptTokens:     body.Usage.InputTokens,
			CompletionTokens: body.Usage.OutputTokens,
			TotalTokens:      body.Usage.TotalTokens,
			CachedTokens:     body.Usage.InputTokensDetails.CachedTokens,
			ReasoningTokens:  body.Usage.OutputTokensDetails.ReasoningTokens,
		}
		if resp.Usage.TotalTokens == 0 {
			resp.Usage.TotalTokens = resp.Usage.PromptTokens + resp.Usage.CompletionTokens
		}
	}
	return resp
}

// Invoke 非流式调用。
func (a *OpenAIResponsesAdapter) Invoke(ctx context.Context, req *Request) (*Response, error) {
	ctx, cancel := a.tr.callTimeout(ctx)
	defer cancel()

	// ① 翻译请求 -> ② 发送 -> ③ 翻译响应
	httpResp, aerr := a.tr.post(ctx, "/v1/responses", a.buildPayload(req), a.headers())
	if aerr != nil {
		return nil, aerr
	}
	defer httpResp.Body.Close()

	var body oaResponseBody
	raw, aerr := decodeJSON(httpResp.Body, &body)
	if aerr != nil {
		return nil, aerr
	}
	// ④ 协议内嵌错误（HTTP 200 但 body.error 非空）也要归一
	if body.Error != nil && body.Error.Message != "" {
		return nil, apierr.New(apierr.CodeUpstreamError, "上游返回错误: %s", body.Error.Message)
	}
	return a.toUnified(&body, raw), nil
}

// Stream 流式调用。
// 流转顺序：① 以 stream=true 发起请求（此处出错可被上层重试）
// ② 起 goroutine 逐个解析 SSE 事件 ③ 把协议事件翻译成统一事件推给通道
func (a *OpenAIResponsesAdapter) Stream(ctx context.Context, req *Request) (<-chan StreamEvent, error) {
	payload := a.buildPayload(req)
	payload.Stream = true

	h := a.headers()
	h["Accept"] = "text/event-stream"
	// ① 建流阶段的错误同步返回，让上层「首 Token 之前」还能重试
	httpResp, aerr := a.tr.post(ctx, "/v1/responses", payload, h)
	if aerr != nil {
		return nil, aerr
	}

	out := make(chan StreamEvent, 32)
	go func() {
		defer close(out)
		defer httpResp.Body.Close()

		reader := newSSEReader(httpResp.Body)
		var (
			sb    strings.Builder
			final *Response
		)
		for {
			ev, err := reader.Next()
			if err != nil {
				// ② EOF：只有收到过 response.completed 才算正常结束。
				//    没收到就断开 = 流被截断，必须报错，绝不能拿半段文字当成功返回。
				if errors.Is(err, io.EOF) {
					if final == nil {
						out <- StreamEvent{Type: EventError, Err: apierr.New(apierr.CodeUpstreamError,
							"上游流被截断：已收到 %d 字节内容，但缺少 response.completed 事件", sb.Len())}
						return
					}
					out <- StreamEvent{Type: EventDone, Response: final}
					return
				}
				out <- StreamEvent{Type: EventError, Err: apierr.FromTransport(err)}
				return
			}
			if ev.Data == "" || ev.Data == "[DONE]" {
				continue
			}
			// ③ 事件名翻译：本协议的 response.output_text.delta -> 统一 delta
			switch ev.Name {
			case "response.output_text.delta":
				var d struct {
					Delta string `json:"delta"`
				}
				if json.Unmarshal([]byte(ev.Data), &d) == nil && d.Delta != "" {
					sb.WriteString(d.Delta)
					out <- StreamEvent{Type: EventDelta, Delta: d.Delta}
				}
			case "response.completed", "response.incomplete":
				var wrap struct {
					Response oaResponseBody `json:"response"`
				}
				if json.Unmarshal([]byte(ev.Data), &wrap) == nil {
					final = a.toUnified(&wrap.Response, json.RawMessage(ev.Data))
					// 流式下 output 可能为空，用累计的增量文本兜底
					if final.Content == "" {
						final.Content = sb.String()
					}
				}
			case "response.failed", "error":
				var e struct {
					Message string `json:"message"`
					Error   struct {
						Message string `json:"message"`
					} `json:"error"`
				}
				_ = json.Unmarshal([]byte(ev.Data), &e)
				msg := firstNonEmpty(e.Message, e.Error.Message, "上游流式返回错误")
				out <- StreamEvent{Type: EventError, Err: apierr.New(apierr.CodeUpstreamError, "%s", msg)}
				return
			}
		}
	}()
	return out, nil
}
