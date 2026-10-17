package store

import (
	"testing"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

// setQuota 用一条额度列替换账号快照（测试直接改 live 对象，与既有选号用例同法）。
func setQuota(a *model.Account, remaining float64, fields map[string]any) {
	entry := map[string]any{"model": "GLM-5.3", "remaining": remaining}
	for k, v := range fields {
		entry[k] = v
	}
	a.Quota = map[string]map[string]any{"GLM-5.3": entry}
}

// 到期门槛的第一原则：窗口内会作废的额度优先于「剩余更多但到期更远」的账号。
func TestSelectPrefersSoonExpiringQuota(t *testing.T) {
	s := newTestStore(t)
	soon, _ := s.AddAccount(model.ProviderZai, "soon", "h1.p.s3")
	far, _ := s.AddAccount(model.ProviderZai, "far", "h2.p.s3")

	now := time.Now()
	setQuota(soon, 1_000_000, map[string]any{
		"period":       "monthly",
		"period_start": float64(now.Add(-28 * 24 * time.Hour).Unix()),
		"period_end":   float64(now.Add(2 * 24 * time.Hour).Unix()),
	})
	setQuota(far, 10_000_000, map[string]any{
		"period":     "one_time",
		"expires_at": float64(now.Add(30 * 24 * time.Hour).Unix()),
	})

	for i := range 3 {
		acc := s.Select(model.ProviderZai, nil, "GLM-5.3")
		if acc == nil || acc.ID != soon.ID {
			t.Fatalf("第 %d 次应优先选中 2 天后到期的账号: %v", i, acc)
		}
	}
}

// 到期比 promo 更急时必须压过 promo：one_time 距离 20 天不紧急，
// 套餐 2 天后结束才是真损耗。
func TestSelectExpireBeatsPromo(t *testing.T) {
	s := newTestStore(t)
	promo, _ := s.AddAccount(model.ProviderZai, "promo", "h1.p.s3")
	plan, _ := s.AddAccount(model.ProviderZai, "plan", "h2.p.s3")

	now := time.Now()
	setQuota(promo, 5_000_000, map[string]any{
		"period":     "one_time",
		"expires_at": float64(now.Add(20 * 24 * time.Hour).Unix()),
	})
	setQuota(plan, 1_000_000, map[string]any{
		"period":       "monthly",
		"period_start": float64(now.Add(-28 * 24 * time.Hour).Unix()),
		"period_end":   float64(now.Add(2 * 24 * time.Hour).Unix()),
	})

	if acc := s.Select(model.ProviderZai, nil, "GLM-5.3"); acc == nil || acc.ID != plan.ID {
		t.Fatalf("2 天后到期的套餐应压过 20 天后到期的 one_time: %v", acc)
	}
}

// promo 层不得被到期门槛削弱：没有更急的账号时，one_time 仍然优先。
func TestSelectPromoStillWinsWhenNoExpiring(t *testing.T) {
	s := newTestStore(t)
	promo, _ := s.AddAccount(model.ProviderZai, "promo", "h1.p.s3")
	regular, _ := s.AddAccount(model.ProviderZai, "regular", "h2.p.s3")

	now := time.Now()
	setQuota(promo, 5_000_000, map[string]any{
		"period":     "one_time",
		"expires_at": float64(now.Add(20 * 24 * time.Hour).Unix()),
	})
	setQuota(regular, 10_000_000, map[string]any{
		"period":       "monthly",
		"period_start": float64(now.Add(-24 * time.Hour).Unix()),
		"period_end":   float64(now.Add(30 * 24 * time.Hour).Unix()),
	})

	if acc := s.Select(model.ProviderZai, nil, "GLM-5.3"); acc == nil || acc.ID != promo.ID {
		t.Fatalf("没有更急的到期项时 promo 仍应优先: %v", acc)
	}
}

// 全池都在窗口内时，门槛是 no-op：并列组内仍逐个轮转，不能变成「只打一个号」。
func TestSelectExpireTierKeepsRotation(t *testing.T) {
	s := newTestStore(t)
	ids := map[string]bool{}
	now := time.Now()
	for _, name := range []string{"e1", "e2", "e3"} {
		a, _ := s.AddAccount(model.ProviderZai, name, "h"+name+".p.s3")
		setQuota(a, 5_000_000, map[string]any{
			"period":       "monthly",
			"period_start": float64(now.Add(-28 * 24 * time.Hour).Unix()),
			"period_end":   float64(now.Add(2 * 24 * time.Hour).Unix()),
		})
		ids[a.ID] = true
	}
	seen := map[string]bool{}
	for range 3 {
		acc := s.Select(model.ProviderZai, nil, "GLM-5.3")
		if acc == nil || !ids[acc.ID] {
			t.Fatalf("应选中池内账号: %v", acc)
		}
		seen[acc.ID] = true
	}
	if len(seen) != 3 {
		t.Fatalf("窗口内全部并列时应各被选中一次: %v", seen)
	}
}

// 日额度不算「会作废」：到期门槛必须放过它，回到「剩余最多优先」。
func TestSelectExpireIgnoresDailyRefresh(t *testing.T) {
	s := newTestStore(t)
	daily, _ := s.AddAccount(model.ProviderZai, "daily", "h1.p.s3")
	monthly, _ := s.AddAccount(model.ProviderZai, "monthly", "h2.p.s3")

	now := time.Now()
	setQuota(daily, 1_000_000, map[string]any{
		"period":       "daily",
		"period_start": float64(now.Add(-12 * time.Hour).Unix()),
		"period_end":   float64(now.Add(12 * time.Hour).Unix()),
	})
	setQuota(monthly, 10_000_000, map[string]any{
		"period":       "monthly",
		"period_start": float64(now.Add(-24 * time.Hour).Unix()),
		"period_end":   float64(now.Add(30 * 24 * time.Hour).Unix()),
	})

	if acc := s.Select(model.ProviderZai, nil, "GLM-5.3"); acc == nil || acc.ID != monthly.ID {
		t.Fatalf("日额度不应触发门槛，仍按剩余最多选择: %v", acc)
	}
}

// 关闭开关（0）后行为必须与旧版一致：按剩余最多选，不看到期。
func TestSelectExpireTierDisabled(t *testing.T) {
	old := config.SelectExpireFirstDays
	config.SelectExpireFirstDays = 0
	t.Cleanup(func() { config.SelectExpireFirstDays = old })

	s := newTestStore(t)
	soon, _ := s.AddAccount(model.ProviderZai, "soon", "h1.p.s3")
	far, _ := s.AddAccount(model.ProviderZai, "far", "h2.p.s3")

	now := time.Now()
	setQuota(soon, 1_000_000, map[string]any{
		"period":       "monthly",
		"period_start": float64(now.Add(-28 * 24 * time.Hour).Unix()),
		"period_end":   float64(now.Add(2 * 24 * time.Hour).Unix()),
	})
	setQuota(far, 10_000_000, map[string]any{
		"period":       "monthly",
		"period_start": float64(now.Add(-24 * time.Hour).Unix()),
		"period_end":   float64(now.Add(30 * 24 * time.Hour).Unix()),
	})

	if acc := s.Select(model.ProviderZai, nil, "GLM-5.3"); acc == nil || acc.ID != far.ID {
		t.Fatalf("关闭到期门槛后应按剩余最多选择: %v", acc)
	}
}
