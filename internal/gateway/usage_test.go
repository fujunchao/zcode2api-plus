package gateway

import (
	"testing"

	"zcode2api/internal/model"
)

func TestUsageCollectorSSE(t *testing.T) {
	u := NewUsageCollector(true)
	u.Feed([]byte(`data: {"type":"message_start","message":{"usage":{"input_tokens":11,"output_tokens":1,"cache_creation_input_tokens":2,"cache_read_input_tokens":5}}}` + "\n"))
	// 故意把一行拆到两个 Feed，验证跨块缓冲
	u.Feed([]byte(`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}` + "\n" + `data: {"type":"message_delta","us`))
	u.Feed([]byte(`age":{"output_tokens":22}}` + "\n"))
	u.Feed([]byte(`data: {"type":"message_delta","usage":{"output_tokens":30}}` + "\n" + "data: [DONE]\n\n"))
	u.Finish()

	got := u.AsDict()
	if got.Input != 11 || got.CacheCreation != 2 || got.CacheRead != 5 {
		t.Fatalf("输入侧统计不符: %+v", got)
	}
	if got.Output != 30 {
		t.Fatalf("output 为累计值应取最大而非累加: %d", got.Output)
	}
}

func TestUsageCollectorJSON(t *testing.T) {
	u := NewUsageCollector(false)
	u.Feed([]byte(`{"id":"msg_1","usage":{"input_tokens":7,"output_tokens":8,"cache_creation_input_tokens":0,"cache_read_input_tokens":3}}`))
	u.Finish()
	got := u.AsDict()
	if got.Input != 7 || got.Output != 8 || got.CacheRead != 3 {
		t.Fatalf("JSON 统计不符: %+v", got)
	}
}

func TestUsageCollectorFinishWithoutUsage(t *testing.T) {
	u := NewUsageCollector(false)
	u.Feed([]byte(`{"id":"msg_1","choices":[]}`))
	u.Finish()
	if got := u.AsDict(); got != (model.Usage{}) {
		t.Fatalf("无 usage 字段应全为 0: %+v", got)
	}

	u2 := NewUsageCollector(false)
	u2.Feed([]byte(`not json at all`))
	u2.Finish()
	if got := u2.AsDict(); got != (model.Usage{}) {
		t.Fatalf("坏 JSON 应全为 0: %+v", got)
	}
}

func TestUsageCollectorNegativeClamped(t *testing.T) {
	u := NewUsageCollector(true)
	u.FeedLine(`data: {"type":"message_start","message":{"usage":{"input_tokens":-3.7,"output_tokens":null}}}`)
	u.Finish()
	got := u.AsDict()
	if got.Input != 0 || got.Output != 0 {
		t.Fatalf("负数/空值应截为 0: %+v", got)
	}
}

func TestUsageCollectorIgnoresNoise(t *testing.T) {
	u := NewUsageCollector(true)
	u.FeedLine("event: message_start")
	u.FeedLine(": keepalive")
	u.FeedLine("data: [DONE]")
	u.FeedLine("data: not-json")
	u.Finish()
	if got := u.AsDict(); got != (model.Usage{}) {
		t.Fatalf("噪音行不应计入: %+v", got)
	}
}

// UsageComplete 是「上游有没有交出终值」的判据，决定交付中断时这笔用量还能不能计入。
// 判据必须同时要求 stop_reason 与 output_tokens：只看前者会把 output 记成 0，
// 只看后者会把中途的增量更新当成终值。偏保守是刻意的——不确定时沿用「不计入」。
func TestUsageCollectorUsageComplete(t *testing.T) {
	start := `data: {"type":"message_start","message":{"usage":{"input_tokens":9}}}`
	final := `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}`

	t.Run("中途的 usage 更新不算终值", func(t *testing.T) {
		u := NewUsageCollector(true)
		u.FeedLine(start)
		u.FeedLine(`data: {"type":"message_delta","usage":{"output_tokens":17}}`)
		if u.UsageComplete() {
			t.Fatal("没有 stop_reason 的 message_delta 不应判为终值")
		}
	})

	t.Run("只有 stop_reason 没有 usage 不算终值", func(t *testing.T) {
		u := NewUsageCollector(true)
		u.FeedLine(start)
		u.FeedLine(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`)
		if u.UsageComplete() {
			t.Fatal("usage 缺失时不应判为终值（会把 output 记成 0）")
		}
	})

	t.Run("stop_reason 与 usage 齐备即终值", func(t *testing.T) {
		u := NewUsageCollector(true)
		u.FeedLine(start)
		u.FeedLine(final)
		if !u.UsageComplete() {
			t.Fatal("stop_reason + output_tokens 应判为终值")
		}
		if got := u.AsDict(); got.Input != 9 || got.Output != 42 {
			t.Fatalf("计数不符: %+v", got)
		}
	})

	t.Run("非 SSE 解析出 usage 即终值", func(t *testing.T) {
		u := NewUsageCollector(false)
		u.Feed([]byte(`{"id":"msg_1","usage":{"input_tokens":7,"output_tokens":8}}`))
		if u.UsageComplete() {
			t.Fatal("Finish 之前不应判为终值")
		}
		u.Finish()
		if !u.UsageComplete() {
			t.Fatal("完整 JSON 解析出 usage 后应判为终值")
		}
	})

	t.Run("非 SSE 无 usage 不算终值", func(t *testing.T) {
		u := NewUsageCollector(false)
		u.Feed([]byte(`{"id":"msg_1","choices":[]}`))
		u.Finish()
		if u.UsageComplete() {
			t.Fatal("没有 usage 字段不应判为终值")
		}
	})
}
