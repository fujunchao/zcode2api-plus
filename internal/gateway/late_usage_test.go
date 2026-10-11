package gateway

import (
	"strings"
	"testing"

	"zcode2api/internal/model"
)

// 输入/cache 可能在后续累计 usage 补齐；不能只统计开始事件。
func TestLateUsageCollectorAccumulatesInputAndCache(t *testing.T) {
	for _, initial := range []string{
		`{"id":"synthetic"}`,
		`{"usage":{"input_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`,
	} {
		t.Run(initial, func(t *testing.T) {
			u := NewUsageCollector(true)
			u.FeedLine(`data: {"type":"message_start","message":` + initial + `}`)
			u.FeedLine(`data: {"type":"message_delta","usage":{"input_tokens":11,"cache_read_input_tokens":90,"cache_creation_input_tokens":2,"output_tokens":3}}`)
			if u.UsageComplete() {
				t.Fatal("有中间用量不等于上游已交付终值")
			}
			final := `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":17,"cache_read_input_tokens":101,"cache_creation_input_tokens":5,"output_tokens":7}}`
			u.FeedLine(final)
			u.FeedLine(final)
			u.FeedLine(`data: {"type":"message_delta","usage":{"input_tokens":0,"cache_read_input_tokens":-1,"cache_creation_input_tokens":null,"output_tokens":6}}`)
			u.FeedLine(`data: {"type":"message_stop"}`)
			u.Finish()
			want := model.Usage{Input: 17, Output: 7, CacheRead: 101, CacheCreation: 5}
			if got := u.AsDict(); got != want {
				t.Errorf("迟到的累计用量应取最大且不重复相加：got=%+v want=%+v", got, want)
			}
			if !u.UsageComplete() || u.StreamError() != nil {
				t.Fatal("完整结束判据不应因累积输入/cache 而改变")
			}
		})
	}
}

func TestLateUsageCollectorFragmentedAndPreservesCounters(t *testing.T) {
	stream := "event: message_start\r\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":4,"cache_read_input_tokens":8,"cache_creation_input_tokens":2}}}` + "\r\n\r\n" +
		`data: {"type":"message_delta","usage":{"input_tokens":"17","cache_read_input_tokens":"101","cache_creation_input_tokens":"5","output_tokens":"7"},"delta":{"stop_reason":"end_turn"}}` + "\r\n\r\n" +
		`data: {"type":"message_delta","usage":{"input_tokens":"invalid","cache_read_input_tokens":null,"cache_creation_input_tokens":-10,"output_tokens":0}}` + "\r\n\r\n" +
		"event: message_stop\r\n" + `data: {"type":"message_stop"}` + "\r\n\r\n"
	u := NewUsageCollector(true)
	for raw := []byte(stream); len(raw) > 0; {
		n := min(7, len(raw))
		u.Feed(raw[:n])
		raw = raw[n:]
	}
	u.Finish()
	if got := u.AsDict(); got != (model.Usage{Input: 17, Output: 7, CacheRead: 101, CacheCreation: 5}) {
		t.Errorf("跨分块补报/非法后续值不能丢失累计计数：%+v", got)
	}
	if u.StreamError() != nil || !u.UsageComplete() {
		t.Fatal("CRLF 分块流应完整结束")
	}
}

func TestLateUsageWithoutOutputDoesNotBecomeComplete(t *testing.T) {
	u := NewUsageCollector(true)
	u.FeedLine(`data: {"type":"message_start","message":{}}`)
	u.FeedLine(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":17,"cache_read_input_tokens":101,"cache_creation_input_tokens":5}}`)
	u.Finish()
	if got := u.AsDict(); got.Input != 17 || got.CacheRead != 101 || got.CacheCreation != 5 {
		t.Errorf("观测到的输入/cache 应保留：%+v", got)
	}
	if u.UsageComplete() || u.StreamError() == nil {
		t.Fatal("没有最终 output 和 message_stop，不能因输入补报判为完整成功")
	}
}

func TestLateUsageMatchesJSONAccounting(t *testing.T) {
	usage := `{"input_tokens":17,"output_tokens":7,"cache_read_input_tokens":101,"cache_creation_input_tokens":5}`
	sse := NewUsageCollector(true)
	sse.Feed([]byte("data: {\"type\":\"message_start\",\"message\":{}}\n\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":` + usage + "}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"))
	sse.Finish()
	buffered := NewUsageCollector(false)
	buffered.Feed([]byte(strings.Join([]string{`{"type":"message","role":"assistant","content":[],"stop_reason":"end_turn","usage":`, usage, `}`}, "")))
	buffered.Finish()
	if sse.AsDict() != buffered.AsDict() || !sse.responseComplete() || !buffered.responseComplete() {
		t.Fatalf("同一用量在 SSE/JSON 不应不同：sse=%+v json=%+v", sse.AsDict(), buffered.AsDict())
	}
}
