package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// UpstreamErrorEvent 上游在 SSE 中正常发出的 error 事件（如 overloaded_error）。
//
// 它与「流被掐断」是两回事：error 事件能完整到达，恰好说明线路把整条流送到了。
// 交付仍按失败收尾（不能伪装成功），但断流记账——账号断流计数、回避期、线路
// 连击与熔断——必须跳过它，否则一波上游过载就会连锁移除健康线路。
// 原生透传、OpenAI 两种重编码与 async 池都用这个类型报告，调用方用
// IsUpstreamErrorEvent 区分。
type UpstreamErrorEvent struct {
	Message string
}

func (e *UpstreamErrorEvent) Error() string { return "上游串流错误: " + e.Message }

// NewUpstreamErrorEvent 从 error 事件的 JSON 载荷构造错误；缺 message 时给通用文案。
func NewUpstreamErrorEvent(payload map[string]any) *UpstreamErrorEvent {
	upstreamError, _ := payload["error"].(map[string]any)
	msg, _ := upstreamError["message"].(string)
	if msg == "" {
		msg = "上游返回错误事件"
	}
	return &UpstreamErrorEvent{Message: msg}
}

// IsUpstreamErrorEvent 判断交付失败是否源于上游的 error 事件（而非线路掐断）。
func IsUpstreamErrorEvent(err error) bool {
	var target *UpstreamErrorEvent
	return errors.As(err, &target)
}

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
			s.err = NewUpstreamErrorEvent(payload)
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
