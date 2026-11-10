package adminapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"zcode2api/internal/captcha"
	"zcode2api/internal/claim"
	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

func TestAutoClaimChecksLatestAccountHealth(t *testing.T) {
	st := newClaimStore(t)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 营销触达是套餐发现的第一跳：它不计入「领取相关的 billing 请求数」，
		// 本用例关心的是「这个账号有没有真的发起领取」。
		if !strings.Contains(r.URL.Path, "/billing/") {
			_, _ = w.Write([]byte("{\"code\":0,\"data\":{\"deliveries\":[]}}"))
			return
		}
		calls.Add(1)
		_, _ = w.Write([]byte("{\"code\":0,\"data\":{\"plans\":[]}}"))
	}))
	defer upstream.Close()
	old, oldMarketing := config.ZcodeBillingBase, claim.MarketingBaseURL
	config.ZcodeBillingBase, claim.MarketingBaseURL = upstream.URL, upstream.URL
	defer func() { config.ZcodeBillingBase, claim.MarketingBaseURL = old, oldMarketing }()
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
