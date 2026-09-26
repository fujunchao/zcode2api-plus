package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// sseState 观察 Anthropic 消息的协议终态，与 usage 终值独立：收到最终用量不等于
// 收到 message_stop。只观察、不改写字节，因此原生接口仍能透传正常 SSE。
// 选号引擎与异步池都通过 UsageCollector 使用它，避免再次出现两种 EOF 判据。
type sseState struct {
	event   string
	data    strings.Builder
	stopped bool
	err     error
}

func (s *sseState) feedLine(line string) {
	line = strings.TrimSuffix(line, "\r")
	switch {
	case line == "":
		s.finishEvent()
	case strings.HasPrefix(line, "event:"):
		s.event = strings.TrimSpace(line[len("event:"):])
	case strings.HasPrefix(line, "data:"):
		if s.data.Len() > 0 {
			s.data.WriteByte('\n')
		}
		s.data.WriteString(strings.TrimSpace(line[len("data:"):]))
	}
}

// finishEvent 兼容显式 event 字段、JSON 的 type 字段以及多行 data；EOF 时也会
// 调用一次，容忍最后一个完整事件没有尾部空行，与 OpenAI 重编码器保持一致。
func (s *sseState) finishEvent() {
	event, data := s.event, s.data.String()
	s.event, s.data = "", strings.Builder{}
	if data == "" || data == "[DONE]" {
		return // [DONE] 是 OpenAI 标记，不能替代 Anthropic 的 message_stop。
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		if s.err == nil {
			s.err = fmt.Errorf("上游串流事件不是合法 JSON: %w", err)
		}
		return
	}
	if event == "" {
		event, _ = payload["type"].(string)
	}
	switch event {
	case "message_stop":
		s.stopped = true
	case "error":
		if s.err == nil {
			s.err = fmt.Errorf("上游串流返回错误事件")
		}
	}
}

func (s *sseState) result() error {
	if s.err != nil {
		return s.err
	}
	if !s.stopped {
		return fmt.Errorf("上游串流在 message_stop 之前结束: %w", io.ErrUnexpectedEOF)
	}
	return nil
}
