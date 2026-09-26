package asyncpool

import (
	"context"
	"net/http"
	"testing"

	"zcode2api/internal/config"
)

// 通过真实票务处理链验证：干净 EOF 也可能是截断，已发出内容后不能换号重发。
func TestAsyncChecksMessageStop(t *testing.T) {
	start := `data: {"type":"message_start","message":{"usage":{"input_tokens":5}}}`
	partial := `data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"partial"}}`
	final := `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}`
	for _, tt := range []struct {
		name     string
		lines    []string
		complete bool
		output   int
	}{
		{"中途正常EOF", []string{start, partial}, false, 0},
		{"终值已到仍不得发送done", []string{start, partial, final}, false, 42},
		{"空流不得发送done", []string{}, false, 0},
		{"完整消息", []string{start, partial, final, `data: {"type":"message_stop"}`}, true, 42},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p, st, _, _ := newTestPool(t)
			acc := addJWTAccount(t, st, "stream-completion")
			up := &scriptedUpstream{specs: []upstreamSpec{{status: http.StatusOK, lines: tt.lines}}}
			config.UpstreamZai = up.start(t).URL
			tk := insertTicket(p, "completion", map[string]any{"model": "GLM-5.3", "messages": []any{}})
			p.processTicket(context.Background(), "completion")
			events := drainEvents(tk)
			if len(events) == 0 {
				t.Fatal("没有票务事件")
			}
			last := events[len(events)-1]
			if tt.complete {
				if last.Type != "done" {
					t.Fatalf("完整消息应正常结束：%+v", events)
				}
			} else {
				if last.Type != "error" {
					t.Fatalf("缺少 message_stop 不得伪装 done：%+v", events)
				}
				for _, event := range events {
					if event.Type == "done" {
						t.Fatal("中断票务不应出现 done")
					}
				}
				if len(tt.lines) > 0 {
					errorBody := last.Data.(map[string]any)["error"].(map[string]any)
					if errorBody["type"] != "upstream_stream_interrupted" {
						t.Fatalf("已交付内容后必须按流中断终止：%+v", errorBody)
					}
				}
			}
			got := st.FindAny(acc.ID)
			wantCalls, wantTrunc := 0, 1
			if tt.complete {
				wantCalls, wantTrunc = 1, 0
			}
			if got.UseCount != wantCalls || got.StreamTruncateCount != wantTrunc || got.TotalOutputTokens != tt.output {
				t.Fatalf("完成状态与统计不符：calls=%d trunc=%d out=%d", got.UseCount, got.StreamTruncateCount, got.TotalOutputTokens)
			}
			if up.callCount() != 1 {
				t.Fatalf("同一张票不得重复生成：calls=%d", up.callCount())
			}
		})
	}
}
