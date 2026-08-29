package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

// unifiedReq 构造一份统一请求，两个适配器都拿它做翻译，用来对比协议差异。
func unifiedReq() *Request {
	return &Request{
		Model:         "m",
		UpstreamModel: "upstream-m",
		System:        "你是助手",
		Messages: []Message{
			{Role: RoleUser, Content: "第一问"},
			{Role: RoleAssistant, Content: "第一答"},
			{Role: RoleUser, Content: "第二问"},
		},
		MaxTokens: 128,
	}
}

// TestResponsesPayload 验证 Responses 协议的翻译结果：system 走 instructions，消息走 input 部件数组。
func TestResponsesPayload(t *testing.T) {
	a := NewOpenAIResponsesAdapter(Options{BaseURL: "http://x"})
	p := a.buildPayload(unifiedReq())

	if p.Model != "upstream-m" {
		t.Fatalf("应使用上游模型名，实际 %q", p.Model)
	}
	if p.Instructions != "你是助手" {
		t.Fatalf("system 应被提到 instructions，实际 %q", p.Instructions)
	}
	if len(p.Input) != 3 {
		t.Fatalf("input 应有 3 条，实际 %d", len(p.Input))
	}
	if p.Input[0].Content[0].Type != "input_text" {
		t.Errorf("用户消息部件类型应为 input_text，实际 %q", p.Input[0].Content[0].Type)
	}
	if p.Input[1].Content[0].Type != "output_text" {
		t.Errorf("助手消息部件类型应为 output_text，实际 %q", p.Input[1].Content[0].Type)
	}
	if p.MaxOutputTokens != 128 {
		t.Errorf("max_output_tokens 应为 128，实际 %d", p.MaxOutputTokens)
	}
	// 序列化后不应出现 Anthropic 协议的字段
	raw, _ := json.Marshal(p)
	if strings.Contains(string(raw), `"messages"`) || strings.Contains(string(raw), `"max_tokens"`) {
		t.Errorf("Responses 报文串味了: %s", raw)
	}
}

// TestMessagesPayload 验证 Messages 协议的翻译：顶层 system、块数组、max_tokens 必填。
func TestMessagesPayload(t *testing.T) {
	a := NewAnthropicMessagesAdapter(Options{BaseURL: "http://x"})
	p := a.buildPayload(unifiedReq())

	if p.System != "你是助手" {
		t.Fatalf("system 应在顶层，实际 %q", p.System)
	}
	if p.Messages[0].Content[0].Type != "text" {
		t.Errorf("消息块类型应为 text，实际 %q", p.Messages[0].Content[0].Type)
	}
	if p.MaxTokens != 128 {
		t.Errorf("max_tokens 应为 128，实际 %d", p.MaxTokens)
	}
	raw, _ := json.Marshal(p)
	if strings.Contains(string(raw), `"input"`) || strings.Contains(string(raw), `"instructions"`) {
		t.Errorf("Messages 报文串味了: %s", raw)
	}
}

// TestMessagesMaxTokensDefault 验证统一层不传 max_tokens 时适配器兜底（该字段在本协议里必填）。
func TestMessagesMaxTokensDefault(t *testing.T) {
	a := NewAnthropicMessagesAdapter(Options{BaseURL: "http://x"})
	req := unifiedReq()
	req.MaxTokens = 0
	if got := a.buildPayload(req).MaxTokens; got != defaultAnthropicMaxTokens {
		t.Fatalf("应兜底为 %d，实际 %d", defaultAnthropicMaxTokens, got)
	}
}

// TestNormalizeMessages 验证消息序列归一：system 抽出、相邻同角色归并、首条补位 user。
func TestNormalizeMessages(t *testing.T) {
	system, msgs := normalizeMessages("外层system", []Message{
		{Role: RoleSystem, Content: "内层system"},
		{Role: RoleAssistant, Content: "上文"},
		{Role: RoleUser, Content: "甲"},
		{Role: RoleUser, Content: "乙"},
	})
	if !strings.Contains(system, "外层system") || !strings.Contains(system, "内层system") {
		t.Fatalf("两处 system 都应并入顶层，实际 %q", system)
	}
	if msgs[0].Role != RoleUser || msgs[0].Content[0].Text != "(继续)" {
		t.Fatalf("首条 assistant 前应补占位 user，实际 %+v", msgs[0])
	}
	if len(msgs) != 3 {
		t.Fatalf("补位 user + assistant + 归并后的 user，共 3 条，实际 %d", len(msgs))
	}
	if got := msgs[2].Content[0].Text; got != "甲\n\n乙" {
		t.Fatalf("相邻同角色应归并，实际 %q", got)
	}
}

// TestStructuredTranslation 验证结构化输出在两种协议下的不同表达。
func TestStructuredTranslation(t *testing.T) {
	rf := &ResponseFormat{
		Type: FormatJSONSchema,
		JSONSchema: &JSONSchemaSpec{
			Name:   "sentiment",
			Strict: true,
			Schema: map[string]any{"type": "object"},
		},
	}
	req := unifiedReq()
	req.ResponseFormat = rf

	// Responses：原生 text.format
	oa := NewOpenAIResponsesAdapter(Options{}).buildPayload(req)
	if oa.Text == nil || oa.Text.Format.Type != FormatJSONSchema || oa.Text.Format.Name != "sentiment" {
		t.Fatalf("Responses 应走原生 text.format，实际 %+v", oa.Text)
	}

	// Messages：没有 response_format，必须翻译成强制工具调用
	an := NewAnthropicMessagesAdapter(Options{}).buildPayload(req)
	if len(an.Tools) != 1 || an.Tools[0].Name != structuredToolName {
		t.Fatalf("Messages 应翻译成 tools，实际 %+v", an.Tools)
	}
	// 只声明了一个工具时 any 等价于强制调用它，且兼容思考模式
	if an.ToolChoice == nil || an.ToolChoice.Type != "any" {
		t.Fatalf("Messages 应强制走工具调用，实际 %+v", an.ToolChoice)
	}
}

// TestAnthropicToolUseToUnified 验证 tool_use 块被还原成统一层的 JSON 文本。
func TestAnthropicToolUseToUnified(t *testing.T) {
	a := NewAnthropicMessagesAdapter(Options{})
	body := &anMessageBody{
		ID: "msg_1",
		Content: []anContentBlock{{
			Type:  "tool_use",
			Name:  structuredToolName,
			Input: json.RawMessage(`{"sentiment":"positive"}`),
		}},
		StopReason: "tool_use",
		Usage:      &anUsage{InputTokens: 10, OutputTokens: 5, CacheReadInputTokens: 2},
	}
	got := a.toUnified(body, nil)
	if got.Content != `{"sentiment":"positive"}` {
		t.Fatalf("tool_use.input 应还原成 JSON 文本，实际 %q", got.Content)
	}
	if got.FinishReason != FinishStop {
		t.Errorf("tool_use 应归一为 stop，实际 %q", got.FinishReason)
	}
	// 用量字段名归一
	if got.Usage.PromptTokens != 10 || got.Usage.CompletionTokens != 5 || got.Usage.TotalTokens != 15 || got.Usage.CachedTokens != 2 {
		t.Errorf("用量归一有误: %+v", got.Usage)
	}
}

// TestResponsesToUnified 验证 Responses 返回体的归一，包括用量字段改名。
func TestResponsesToUnified(t *testing.T) {
	raw := `{"id":"resp_1","model":"m","status":"incomplete",
	  "incomplete_details":{"reason":"max_output_tokens"},
	  "output":[{"type":"message","content":[{"type":"output_text","text":"你好"}]}],
	  "usage":{"input_tokens":8,"input_tokens_details":{"cached_tokens":3},
	           "output_tokens":4,"output_tokens_details":{"reasoning_tokens":1},"total_tokens":12}}`
	var body oaResponseBody
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatal(err)
	}
	got := NewOpenAIResponsesAdapter(Options{}).toUnified(&body, nil)
	if got.Content != "你好" {
		t.Errorf("内容提取有误: %q", got.Content)
	}
	if got.FinishReason != FinishLength {
		t.Errorf("status=incomplete 应归一为 length，实际 %q", got.FinishReason)
	}
	if got.Usage.PromptTokens != 8 || got.Usage.CachedTokens != 3 || got.Usage.ReasoningTokens != 1 {
		t.Errorf("用量归一有误: %+v", got.Usage)
	}
}

// TestMapStopReason 覆盖 Anthropic 结束原因到统一值的映射。
func TestMapStopReason(t *testing.T) {
	cases := map[string]string{
		"end_turn": FinishStop, "stop_sequence": FinishStop, "tool_use": FinishStop,
		"max_tokens": FinishLength, "refusal": FinishContentFilter, "": FinishStop,
	}
	for in, want := range cases {
		if got := mapStopReason(in); got != want {
			t.Errorf("mapStopReason(%q) = %q，期望 %q", in, got, want)
		}
	}
}
