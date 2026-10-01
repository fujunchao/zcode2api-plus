package asyncpool

import (
	"context"
	"net/http"
	"testing"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

func TestAsyncRiskProxyRotationUsesNewEgressAfterCooling(t *testing.T) {
	for _, risk := range []bool{false, true} {
		t.Run(map[bool]string{false: "普通405不换线", true: "风控405换线"}[risk], func(t *testing.T) {
			p, st, _, _ := newTestPool(t)
			a := addJWTAccount(t, st, "risk")
			body := `{"error":{"message":"method not allowed"}}`
			if risk {
				body = `{"error":{"message":"blocked due to unusual activity"}}`
			}
			oldUp := &scriptedUpstream{specs: []upstreamSpec{{status: 405, body: body}}}
			newUp := &scriptedUpstream{specs: []upstreamSpec{{status: http.StatusOK,
				lines: []string{`data: {"id":"ok"}`, `data: {"type":"message_stop"}`}}}}
			oldURL, newURL := oldUp.start(t).URL, newUp.start(t).URL
			config.UpstreamZai = oldURL
			old, _ := st.AddProxyProfile("old", oldURL, true)
			next, _ := st.AddProxyProfile("next", newURL, true)
			if _, err := st.AssignProxyProfile(a.ID, old.ID); err != nil {
				t.Fatal(err)
			}
			request := map[string]any{"model": "GLM-5.3", "messages": []any{}}
			insertTicket(p, "risk", request)
			p.processTicket(context.Background(), "risk")
			got := st.FindAny(a.ID)
			if oldUp.callCount() != 1 || newUp.callCount() != 0 {
				t.Fatal("异步换线不得立即重试同一账号")
			}
			if !risk {
				if got.ProxyID == nil || *got.ProxyID != old.ID {
					t.Fatal("普通 405 不应更换代理")
				}
				return
			}
			if got.ProxyID == nil || *got.ProxyID != next.ID || got.ProxyURL == nil || *got.ProxyURL != next.URL ||
				got.Status != model.StatusCooling || got.RiskControlStreak != 1 || got.CoolingUntil == nil {
				t.Fatal("异步风控应换线并保留冷却")
			}
			_, _ = st.Update(a.Provider, a.ID, func(live *model.Account) {
				expired := float64(time.Now().Add(-time.Second).Unix())
				live.CoolingUntil = &expired
			})
			tk := insertTicket(p, "after-cooling", request)
			p.processTicket(context.Background(), "after-cooling")
			events := drainEvents(tk)
			if len(events) == 0 || events[len(events)-1].Type != "done" || oldUp.callCount() != 1 || newUp.callCount() != 1 {
				t.Fatalf("冷却后异步请求应从新代理完成: events=%v old=%d new=%d", events, oldUp.callCount(), newUp.callCount())
			}
		})
	}
}
