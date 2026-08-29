package llm

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// TestSSEReader 验证 SSE 解析：事件名、多行 data、注释行、以空行为边界。
func TestSSEReader(t *testing.T) {
	raw := "event: message_start\n" +
		"data: {\"a\":1}\n" +
		"\n" +
		": 这是心跳注释\n" +
		"event: content_block_delta\n" +
		"data: line1\n" +
		"data: line2\n" +
		"\n" +
		"data: [DONE]\n\n"

	r := newSSEReader(strings.NewReader(raw))

	ev, err := r.Next()
	if err != nil || ev.Name != "message_start" || ev.Data != `{"a":1}` {
		t.Fatalf("第一个事件解析有误: %+v err=%v", ev, err)
	}
	ev, err = r.Next()
	if err != nil || ev.Name != "content_block_delta" || ev.Data != "line1\nline2" {
		t.Fatalf("多行 data 应以 \\n 拼接: %+v err=%v", ev, err)
	}
	ev, err = r.Next()
	if err != nil || ev.Name != "" || ev.Data != "[DONE]" {
		t.Fatalf("匿名事件解析有误: %+v err=%v", ev, err)
	}
	if _, err = r.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("流末尾应返回 EOF，实际 %v", err)
	}
}

// TestSSEReaderNoTrailingBlankLine 验证末尾没有空行时也能吐出最后一个事件。
func TestSSEReaderNoTrailingBlankLine(t *testing.T) {
	r := newSSEReader(strings.NewReader("event: done\ndata: bye"))
	ev, err := r.Next()
	if err != nil || ev.Name != "done" || ev.Data != "bye" {
		t.Fatalf("未闭合事件应被兜底吐出: %+v err=%v", ev, err)
	}
}
