package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"llmgateway/internal/apierr"
)

// TestResponsesStreamTruncated 复现「断流被当成成功」：
// 上游吐了半段文字就断开，没有发 response.completed。
func TestResponsesStreamTruncated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"delta\":\"半段\"}\n\n")
		w.(http.Flusher).Flush()
		// 故意不发 response.completed，直接返回让连接断开
	}))
	defer srv.Close()

	a := NewOpenAIResponsesAdapter(Options{BaseURL: srv.URL})
	ch, err := a.Stream(context.Background(), &Request{Model: "m", Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	var gotDone, gotErr bool
	var finish string
	for ev := range ch {
		switch ev.Type {
		case EventDone:
			gotDone, finish = true, ev.Response.FinishReason
		case EventError:
			gotErr = true
			if code := apierr.From(ev.Err).Code; code != apierr.CodeUpstreamError {
				t.Errorf("断流应归一为 UPSTREAM_ERROR，实际 %s", code)
			}
		}
	}
	if gotDone && finish == FinishStop {
		t.Fatalf("断流不该被当成正常结束（finish_reason=%q）", finish)
	}
	if !gotErr {
		t.Fatal("断流应产生 error 事件")
	}
}

// TestMessagesStreamTruncated 同上，Anthropic 协议：没有 message_stop 就断开。
func TestMessagesStreamTruncated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: message_start\ndata: {\"message\":{\"id\":\"m1\",\"usage\":{\"input_tokens\":5}}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"delta\":{\"type\":\"text_delta\",\"text\":\"半段\"}}\n\n")
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	a := NewAnthropicMessagesAdapter(Options{BaseURL: srv.URL})
	ch, err := a.Stream(context.Background(), &Request{Model: "m", Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	var gotErr bool
	for ev := range ch {
		if ev.Type == EventDone && ev.Response.FinishReason == FinishStop {
			t.Fatal("缺少 message_stop 时不该判定为正常结束")
		}
		if ev.Type == EventError {
			gotErr = true
		}
	}
	if !gotErr {
		t.Fatal("断流应产生 error 事件")
	}
}
