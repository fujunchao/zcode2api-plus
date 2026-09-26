package gateway

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/store"
)

// HTTP 正常结束不代表 Anthropic 消息完整；终值 usage 与 message_stop 也是两个独立信号。
func TestPassthroughChecksMessageStop(t *testing.T) {
	start := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5}}}\n\n"
	partial := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n"
	final := "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":42}}\n\n"
	for _, tt := range []struct {
		name     string
		body     string
		complete bool
		input    int
		output   int
	}{
		{"中途正常EOF", start + partial, false, 0, 0},
		{"只有终值没有结束事件", start + partial + final, false, 5, 42},
		{"DONE不能代替消息结束", start + partial + "data: [DONE]\n\n", false, 0, 0},
		{"完整消息", start + partial + final + "data: {\"type\":\"message_stop\"}\n\n", true, 5, 42},
		{"结束事件无尾部换行", start + final + "event: message_stop\ndata: {}", true, 5, 42},
		{"结束事件多行JSON", start + final + "event: message_stop\ndata: {\ndata: \"type\": \"message_stop\"\ndata: }\n\n", true, 5, 42},
	} {
		t.Run(tt.name, func(t *testing.T) {
			oldData := config.DataDir
			config.DataDir = t.TempDir()
			t.Cleanup(func() { config.DataDir = oldData })
			f := newDiagFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tt.body)
			}))
			acc, err := f.st.AddAccount(model.ProviderZai, "stream-completion", "sk-stream-completion")
			if err != nil {
				t.Fatal(err)
			}
			line, _ := bindToLine(t, f.st, acc, f.ups.URL)
			if err := f.st.SetSetting(store.LineTruncateStrikesKey, "0"); err != nil {
				t.Fatal(err)
			}
			f.st.BumpLineTruncate(line)
			request := msgBody()
			request["stream"] = true
			status, raw := f.post(t, request)
			if status != http.StatusOK || !strings.HasPrefix(raw, tt.body) {
				t.Fatalf("已发出的 SSE 不得丢失：status=%d body=%q", status, raw)
			}
			if tt.complete {
				if raw != tt.body {
					t.Fatalf("完整 SSE 必须保持字节级透传：%q", raw)
				}
			} else if !strings.Contains(raw[len(tt.body):], "event: error\n") {
				t.Fatalf("缺少 message_stop 必须向客户端发送错误事件：%q", raw)
			}
			got := f.st.FindAny(acc.ID)
			wantTrunc, wantStreak := 1, 2
			if tt.complete {
				wantTrunc, wantStreak = 0, 0
			}
			if got.StreamTruncateCount != wantTrunc || f.st.LineTruncateStats()[line].Streak != wantStreak {
				t.Fatalf("断流应累计而不是清零线路连击：trunc=%d stats=%+v", got.StreamTruncateCount, f.st.LineTruncateStats()[line])
			}
			if got.TotalInputTokens != tt.input || got.TotalOutputTokens != tt.output {
				t.Fatalf("只在用量完整时累计：in=%d out=%d want=%d/%d", got.TotalInputTokens, got.TotalOutputTokens, tt.input, tt.output)
			}
		})
	}
}
