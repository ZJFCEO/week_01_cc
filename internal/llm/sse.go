package llm

import (
	"bufio"
	"io"
	"strings"
)

// sseEvent 是从上游读到的一个 SSE 事件。
type sseEvent struct {
	Name string // event: 行的值，可能为空
	Data string // data: 行拼接后的值（多行用 \n 连接）
}

// sseReader 按 SSE 规范逐事件读取上游响应体。
// 两种协议的事件名不同，但传输层格式一致，所以这里做成公共工具。
type sseReader struct {
	br *bufio.Reader
}

func newSSEReader(r io.Reader) *sseReader {
	return &sseReader{br: bufio.NewReaderSize(r, 64*1024)}
}

// Next 读取下一个完整事件；遇到 EOF 返回 io.EOF。
// 流转顺序：① 逐行读取 ② 空行代表一个事件结束 ③ 累积 event/data 字段后返回
func (s *sseReader) Next() (*sseEvent, error) {
	var (
		name string
		data []string
	)
	for {
		line, err := s.br.ReadString('\n')
		if err != nil {
			// ① 读到 EOF 时，如果缓冲区里还攒着未结束的事件，先把它吐出去
			if err == io.EOF {
				trimmed := strings.TrimRight(line, "\r\n")
				if trimmed != "" {
					appendSSEField(trimmed, &name, &data)
				}
				if name != "" || len(data) > 0 {
					return &sseEvent{Name: name, Data: strings.Join(data, "\n")}, nil
				}
			}
			return nil, err
		}
		trimmed := strings.TrimRight(line, "\r\n")
		// ② 空行 = 事件边界
		if trimmed == "" {
			if name == "" && len(data) == 0 {
				continue // 连续空行，跳过
			}
			return &sseEvent{Name: name, Data: strings.Join(data, "\n")}, nil
		}
		// ③ 累积字段
		appendSSEField(trimmed, &name, &data)
	}
}

// appendSSEField 解析单行 SSE 字段（event: / data: / 注释行）。
func appendSSEField(line string, name *string, data *[]string) {
	if strings.HasPrefix(line, ":") {
		return // 注释/心跳行，忽略
	}
	key, value, found := strings.Cut(line, ":")
	if !found {
		key, value = line, ""
	}
	value = strings.TrimPrefix(value, " ")
	switch key {
	case "event":
		*name = value
	case "data":
		*data = append(*data, value)
	}
}
