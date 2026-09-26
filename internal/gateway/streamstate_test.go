package gateway

import (
	"errors"
	"io"
	"testing"
)

func TestSSECompletionAcrossChunkBoundaries(t *testing.T) {
	for _, stream := range []string{
		"data: {\"type\":\"message_stop\"}\n\n",
		"event: message_stop\r\ndata: {}\r\n\r\n",
		"event: message_stop\ndata: {}",
		"data: {\ndata: \"type\": \"message_stop\"\ndata: }\n\n",
	} {
		u := NewUsageCollector(true)
		for i := range len(stream) {
			u.Feed([]byte{stream[i]}) // 网络块可在字段、JSON 和 CRLF 的任意位置拆开。
		}
		u.Finish()
		if err := u.StreamError(); err != nil {
			t.Fatalf("合法结束事件不应被误判：%q: %v", stream, err)
		}
		u.Finish()
		if err := u.StreamError(); err != nil {
			t.Fatalf("重复收尾应幂等：%v", err)
		}
	}
}

func TestSSECompletionRejectsIncompleteOrFailedEvents(t *testing.T) {
	for _, stream := range []string{"", "data: [DONE]\n\n", "data: {\"text\":\"message_stop\"}\n\n"} {
		u := NewUsageCollector(true)
		u.Feed([]byte(stream))
		u.Finish()
		if err := u.StreamError(); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("缺少结束事件应报告 unexpected EOF：%q: %v", stream, err)
		}
	}
	for _, stream := range []string{
		"event: error\ndata: {\"error\":{\"message\":\"failed\"}}\n\n",
		"event: message_stop\ndata: not-json\n\n",
		"event: error\ndata: {}\n\ndata: {\"type\":\"message_stop\"}\n\n",
	} {
		u := NewUsageCollector(true)
		u.Feed([]byte(stream))
		u.Finish()
		if u.StreamError() == nil {
			t.Fatalf("错误事件不能被伪装成成功：%q", stream)
		}
	}
}
