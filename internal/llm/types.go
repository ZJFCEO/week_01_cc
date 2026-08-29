// Package llm 定义网关的统一抽象层。
//
// 这里的类型是「协议中立」的：不管下游是 OpenAI Responses API 还是
// Anthropic Messages API，进到适配器之前长这样，出适配器之后也长这样。
// 各协议特有的字段（instructions / input_text / tool_use / max_tokens 必填…）
// 全部在各自的 adapter 里翻译，不允许泄漏到这一层。
package llm

import "encoding/json"

// 统一的消息角色。
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// Message 是协议中立的一轮对话消息。
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// 结构化输出的三种模式。
const (
	FormatText       = "text"        // 不约束
	FormatJSONObject = "json_object" // 只要求合法 JSON
	FormatJSONSchema = "json_schema" // 要求满足给定 JSON Schema
)

// JSONSchemaSpec 描述一个 JSON Schema 约束。
type JSONSchemaSpec struct {
	Name   string         `json:"name"`
	Strict bool           `json:"strict,omitempty"`
	Schema map[string]any `json:"schema"`
}

// ResponseFormat 是统一的结构化输出声明。
type ResponseFormat struct {
	Type       string          `json:"type"`
	JSONSchema *JSONSchemaSpec `json:"json_schema,omitempty"`
}

// IsStructured 判断本次调用是否需要走结构化输出路径。
func (r *ResponseFormat) IsStructured() bool {
	return r != nil && (r.Type == FormatJSONObject || r.Type == FormatJSONSchema)
}

// Request 是统一调用接口的入参。
type Request struct {
	Model          string          // 逻辑模型名，用于路由到适配器
	UpstreamModel  string          // 实际传给上游的模型名（路由表里配置）
	System         string          // 系统提示词
	Messages       []Message       // 对话消息
	Temperature    *float64        // 指针以便区分「没传」和「传了 0」
	MaxTokens      int             // 0 表示交给适配器取协议默认值
	Stream         bool            // 是否流式
	ResponseFormat *ResponseFormat // 结构化输出约束
}

// Usage 是统一的 Token 消耗统计，含分类维度。
// 两种协议的字段名完全不同（input_tokens vs prompt_tokens，
// cached_tokens vs cache_read_input_tokens），在适配器里归一到这里。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`     // 输入 Token
	CompletionTokens int `json:"completion_tokens"` // 输出 Token
	TotalTokens      int `json:"total_tokens"`      // 合计
	CachedTokens     int `json:"cached_tokens"`     // 输入中命中缓存的部分（分类统计）
	ReasoningTokens  int `json:"reasoning_tokens"`  // 输出中属于思维链的部分（分类统计）
}

// Add 用于把多次调用的用量累加（可观测性聚合时使用）。
func (u *Usage) Add(o Usage) {
	u.PromptTokens += o.PromptTokens
	u.CompletionTokens += o.CompletionTokens
	u.TotalTokens += o.TotalTokens
	u.CachedTokens += o.CachedTokens
	u.ReasoningTokens += o.ReasoningTokens
}

// 统一的结束原因。
const (
	FinishStop          = "stop"
	FinishLength        = "length"
	FinishContentFilter = "content_filter"
	FinishError         = "error"
)

// Response 是统一调用接口的出参。
type Response struct {
	ID           string          `json:"id"`
	Model        string          `json:"model"`
	Content      string          `json:"content"`
	FinishReason string          `json:"finish_reason"`
	Usage        Usage           `json:"usage"`
	RawUpstream  json.RawMessage `json:"-"` // 上游原始返回，排查协议问题时用
}

// EventType 是统一流式事件的类型。
type EventType string

const (
	EventDelta EventType = "delta" // 增量文本
	EventDone  EventType = "done"  // 结束，携带完整用量
	EventError EventType = "error" // 出错
)

// StreamEvent 是适配器向上吐出的统一流式事件。
// 两种协议的 SSE 事件名/结构完全不同（response.output_text.delta vs
// content_block_delta），在适配器里归一成这一种事件。
type StreamEvent struct {
	Type     EventType
	Delta    string    // Type==EventDelta 时有效
	Response *Response // Type==EventDone 时有效
	Err      error     // Type==EventError 时有效
}
