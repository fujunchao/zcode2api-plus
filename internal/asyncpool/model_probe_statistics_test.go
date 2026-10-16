package asyncpool

import (
	"context"
	"testing"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/store"
)

func TestModelProbeAccountStatisticsAsyncHTTP(t *testing.T) {
	for _, tc := range []struct {
		name    string
		spec    upstreamSpec
		success bool
	}{
		{"完整流", upstreamSpec{status: 200, lines: []string{
			`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_stats","role":"assistant","content":[],"usage":{"input_tokens":1}}}`,
			`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
			`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
		}}, true},
		{"鉴权失败", upstreamSpec{status: 401, body: `{}`}, false},
		{"200额度错误", upstreamSpec{status: 200, body: `{"code":"1005","message":"quota"}`}, false},
		{"连接失败", upstreamSpec{reset: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, st, _, _ := newTestPool(t)
			a := addJWTAccount(t, st, "合成异步统计账号")
			if _, err := st.Update(a.Provider, a.ID, func(live *model.Account) {
				live.Quota = map[string]map[string]any{store.ModelProbeModel: {"available": 10000, "remaining": 10000}}
			}); err != nil {
				t.Fatal(err)
			}
			up := &scriptedUpstream{specs: []upstreamSpec{tc.spec}}
			config.UpstreamZai = up.start(t).URL
			tk := insertTicket(p, "ticket-statistics", map[string]any{"model": store.ModelProbeModel, "max_tokens": 8, "messages": []any{map[string]any{"role": "user", "content": "synthetic"}}})
			p.processTicket(context.Background(), "ticket-statistics")
			_ = drainEvents(tk)
			if up.callCount() == 0 {
				t.Fatal("夹具没有触达真实异步HTTP")
			}
			view, err := st.ModelProbeSummary(time.Now())
			if err != nil || len(view.Accounts) != 1 || view.Accounts[0].RecentTotal != up.callCount() || (view.Accounts[0].RecentSuccess > 0) != tc.success || view.Accounts[0].ActiveRequests != 0 {
				t.Fatalf("异步统计必须和实际模型尝试一致：calls=%d summary=%+v err=%v", up.callCount(), view, err)
			}
		})
	}
}
