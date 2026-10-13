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

// 人工恢复与静默窗口的联动：窗口内恢复转保护冷却、不直接放回轮转；
// 显式关闭（0）时恢复直接解除并立即参与调度。默认窗口值由 gateway 包的
// TestDefaultRiskSilentHoursIs24 锁定，本用例只覆盖两种显式配置。
func TestManualRecoveryRespectsSilentWindow(t *testing.T) {
	for _, hours := range []int{0, 24} {
		t.Run(time.Duration(hours).String(), func(t *testing.T) {
			old := config.RiskSilentHours
			config.RiskSilentHours = hours
			t.Cleanup(func() { config.RiskSilentHours = old })

			mux, st, _ := setup(t)
			a := seedRiskControlInvalid(t, st, "recent", "synthetic-recover")
			_, err := st.Update(a.Provider, a.ID, func(a *model.Account) { at := float64(time.Now().Unix()); a.LastRiskControlAt = &at })
			if err != nil {
				t.Fatal(err)
			}
			status, _ := do(t, mux, st, http.MethodPost, "/admin/api/accounts/"+a.ID+"/recover", nil)
			got := st.FindAny(a.ID)
			if status != 200 || got.LastRiskControlAt == nil {
				t.Fatal("恢复应保留历史观测时间")
			}
			if hours == 0 {
				if got.CoolingUntil != nil || st.Select(a.Provider, nil, "GLM-5.3") == nil {
					t.Fatal("显式关闭静默后，恢复应直接解除并立即参与调度")
				}
				return
			}
			if got.Status != model.StatusCooling || got.CoolingUntil == nil || st.Select(a.Provider, nil, "GLM-5.3") != nil {
				t.Fatal("静默窗口内恢复应转保护冷却且暂不可调度")
			}
		})
	}
}
