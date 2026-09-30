package adminapi

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
	"zcode2api/internal/captcha"
	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

func TestAutoClaimChecksLatestAccountHealth(t *testing.T) {
	st := newClaimStore(t)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{\"code\":0,\"data\":{\"plans\":[]}}"))
	}))
	defer upstream.Close()
	old := config.ZcodeBillingBase
	config.ZcodeBillingBase = upstream.URL
	defer func() { config.ZcodeBillingBase = old }()
	h := &Handler{Store: st, Captcha: captcha.NewManager()}
	for _, state := range []string{"cooling", "disabled", "invalid", "archived", "claim-cooling", "exhausted", "cooling-expired"} {
		t.Run(state, func(t *testing.T) {
			a, _ := st.AddAccount(model.ProviderZai, state, "test."+state+".sig")
			// 调用方持有旧快照；真正执行时必须重新检查最新状态。
			snapshot := st.FindAny(a.ID)
			_, _ = st.Update(a.Provider, a.ID, func(a *model.Account) {
				future := float64(time.Now().Add(time.Hour).Unix())
				past := float64(time.Now().Add(-time.Hour).Unix())
				switch state {
				case "archived":
					a.ArchivedAt = &future
				case "claim-cooling":
					a.SetClaimState(&model.ClaimState{NextAt: &future})
				case "cooling-expired":
					a.Status = model.StatusCooling
					a.CoolingUntil = &past
				default:
					a.Status = state
					a.CoolingUntil = &future
					if state == "disabled" {
						a.Enabled = false
					}
				}
			})
			before := calls.Load()
			if !claimSlot.acquire() {
				t.Fatal("领取槽位被占用")
			}
			h.claimUnderGate(snapshot)
			want := int32(0)
			if state == "exhausted" || state == "cooling-expired" {
				want = 1
			}
			if got := calls.Load() - before; got != want {
				t.Fatalf("上游请求数=%d，应为 %d", got, want)
			}
		})
	}
}
