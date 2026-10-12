package adminapi

import (
	"net/http"
	"testing"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

func TestAccountSchedulingViewMatchesLocalPolicy(t *testing.T) {
	for _, hours := range []int{0, 24} {
		t.Run(time.Duration(hours).String(), func(t *testing.T) {
			old := config.RiskSilentHours
			config.RiskSilentHours = hours
			t.Cleanup(func() { config.RiskSilentHours = old })
			mux, st, _ := setup(t)
			a, err := st.AddAccount(model.ProviderZai, "schedule", "synthetic-schedule")
			if err != nil {
				t.Fatal(err)
			}
			now := float64(time.Now().Unix())
			_, err = st.Update(a.Provider, a.ID, func(a *model.Account) {
				a.Status = model.StatusCooling
				until := now + 300
				last := now - 60
				a.CoolingUntil, a.LastRiskControlAt = &until, &last
			})
			if err != nil {
				t.Fatal(err)
			}
			status, body := do(t, mux, st, http.MethodGet, "/admin/api/accounts", nil)
			if status != 200 {
				t.Fatalf("列表失败: %d", status)
			}
			view := body["accounts"].([]any)[0].(map[string]any)
			want, until := "cooling", now+300
			if hours > 0 {
				want, until = "risk_isolation", now-60+86400
			}
			if view["local_scheduling_reason"] != want || view["local_recheck_at"] != until {
				t.Fatalf("列表应显示真实时间门槛: %#v", view)
			}
			if st.Select(a.Provider, nil, "GLM-5.3") != nil {
				t.Fatal("时间门槛未过不能调度")
			}
		})
	}
}

func TestDefaultManualRecoveryKeepsHistoryWithoutExtraIsolation(t *testing.T) {
	mux, st, _ := setup(t)
	a := seedRiskControlInvalid(t, st, "recent", "synthetic-recover")
	_, err := st.Update(a.Provider, a.ID, func(a *model.Account) { at := float64(time.Now().Unix()); a.LastRiskControlAt = &at })
	if err != nil {
		t.Fatal(err)
	}
	status, _ := do(t, mux, st, http.MethodPost, "/admin/api/accounts/"+a.ID+"/recover", nil)
	got := st.FindAny(a.ID)
	if status != 200 || got.LastRiskControlAt == nil || got.CoolingUntil != nil || st.Select(a.Provider, nil, "GLM-5.3") == nil {
		t.Fatal("默认人工恢复应解除本地失效，历史不应延长隔离")
	}
}
