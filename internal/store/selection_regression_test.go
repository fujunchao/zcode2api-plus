package store

import (
	"testing"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

func TestSelectAvoidKeepsOnlyUsableModel(t *testing.T) {
	s := newTestStore(t)
	a, _ := s.AddAccount(model.ProviderZai, "flash", "test-flash")
	b, _ := s.AddAccount(model.ProviderZai, "other", "test-other")
	_, _ = s.Update(a.Provider, a.ID, func(a *model.Account) {
		a.TruncateAvoidUntil = float64(time.Now().Add(time.Minute).Unix())
		a.Quota = map[string]map[string]any{"glm-5.3-flash": {"remaining": 100.0}}
	})
	for _, state := range []string{"disabled", "absent", "exhausted"} {
		t.Run(state, func(t *testing.T) {
			_, _ = s.Update(b.Provider, b.ID, func(b *model.Account) {
				b.DisabledModels = nil
				b.Quota = map[string]map[string]any{}
				switch state {
				case "disabled":
					b.SetDisabledModels([]string{"glm-5.3-flash"})
				case "absent":
					b.Quota["GLM-5.3"] = map[string]any{"remaining": 100.0}
				case "exhausted":
					b.Quota["glm-5.3-flash"] = map[string]any{"remaining": 0.0}
				}
			})
			got := s.Select(model.ProviderZai, nil, "glm-5.3-flash")
			if got == nil || got.ID != a.ID {
				t.Fatal("短回避不能排除唯一可用模型账号")
			}
		})
	}
}

// 风控静默期是选号**硬闸门**：被上游标记的账号（LastRiskControlAt 在窗口内）直接
// 跳过，而不是冷却到期后照常入池再次踩雷；池内账号全在静默期时宁可返回 nil
// （走 no_available_account 出口），也不把必然被拒的账号喂给请求。
// 依据：2026-10-12 晚事故——本地 300s 冷却 ≪ 上游标记（实测 ≥14h），冷却到期即
// 再选中、再 405，33 分钟烧穿可用的干净账号。
func TestSelectSkipsRiskSilentAccounts(t *testing.T) {
	old := config.RiskSilentHours
	config.RiskSilentHours = 24
	t.Cleanup(func() { config.RiskSilentHours = old })

	s := newTestStore(t)
	hot, err := s.AddAccount(model.ProviderZai, "hot", "hot-secret")
	if err != nil {
		t.Fatal(err)
	}
	clean, err := s.AddAccount(model.ProviderZai, "clean", "clean-secret")
	if err != nil {
		t.Fatal(err)
	}
	hotAt := float64(time.Now().Add(-1*time.Hour).UnixNano()) / 1e9
	if _, err := s.Update(model.ProviderZai, hot.ID, func(a *model.Account) {
		a.LastRiskControlAt = &hotAt
	}); err != nil {
		t.Fatal(err)
	}

	// 1) 热账号在静默期、干净账号正常 → 轮转只可能选中干净账号。
	for i := 0; i < 3; i++ {
		got := s.Select(model.ProviderZai, nil, "")
		if got == nil || got.ID != clean.ID {
			t.Fatalf("静默期内的账号不应被选中: %#v", got)
		}
	}

	// 2) 池内全部在静默期 → nil（不降级喂必拒账号）。
	if _, err := s.Update(model.ProviderZai, clean.ID, func(a *model.Account) {
		a.LastRiskControlAt = &hotAt
	}); err != nil {
		t.Fatal(err)
	}
	if got := s.Select(model.ProviderZai, nil, ""); got != nil {
		t.Fatalf("池内全在静默期时应返回 nil，实际 %s", got.ID)
	}

	// 3) 静默期已过 → 重新参与轮转。
	oldAt := float64(time.Now().Add(-25*time.Hour).UnixNano()) / 1e9
	if _, err := s.Update(model.ProviderZai, hot.ID, func(a *model.Account) {
		a.LastRiskControlAt = &oldAt
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(model.ProviderZai, clean.ID, func(a *model.Account) {
		a.LastRiskControlAt = nil
	}); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		if sel := s.Select(model.ProviderZai, nil, ""); sel != nil {
			seen[sel.ID] = true
		}
	}
	if !seen[hot.ID] || !seen[clean.ID] {
		t.Fatalf("静默期结束后两个账号都应恢复轮转: %v", seen)
	}

	// 4) 应急回退：window=0 关闭闸门，静默期内账号照常入池（旧行为）。
	config.RiskSilentHours = 0
	if _, err := s.Update(model.ProviderZai, hot.ID, func(a *model.Account) {
		a.LastRiskControlAt = &hotAt
	}); err != nil {
		t.Fatal(err)
	}
	sawHot := false
	for i := 0; i < 4 && !sawHot; i++ {
		if sel := s.Select(model.ProviderZai, nil, ""); sel != nil && sel.ID == hot.ID {
			sawHot = true
		}
	}
	if !sawHot {
		t.Fatal("window=0 时静默期内账号应照常参与轮转")
	}
}

// 池状态分解必须能回答「静默期内有多少账号被闸门拦下」——它是 no_available_account
// 出口文案的数据来源（此前整池冷却事故就是因为文案没有如实报出成因）。
func TestPoolStatsCountsRiskSilent(t *testing.T) {
	old := config.RiskSilentHours
	config.RiskSilentHours = 24
	t.Cleanup(func() { config.RiskSilentHours = old })

	s := newTestStore(t)
	hot, err := s.AddAccount(model.ProviderZai, "hot", "hot-secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddAccount(model.ProviderZai, "clean", "clean-secret"); err != nil {
		t.Fatal(err)
	}
	hotAt := float64(time.Now().Add(-30*time.Minute).UnixNano()) / 1e9
	if _, err := s.Update(model.ProviderZai, hot.ID, func(a *model.Account) {
		a.LastRiskControlAt = &hotAt
	}); err != nil {
		t.Fatal(err)
	}

	stat := s.PoolStats(model.ProviderZai, "")
	if stat.Total != 2 || stat.RiskSilent != 1 || stat.Active != 1 || stat.Cooling != 0 {
		t.Fatalf("静默期分桶不符: total=%d silent=%d active=%d cooling=%d",
			stat.Total, stat.RiskSilent, stat.Active, stat.Cooling)
	}
}
