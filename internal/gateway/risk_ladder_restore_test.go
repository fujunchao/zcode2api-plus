package gateway

import (
	"net/http"
	"testing"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

// 产品默认值：风控命中后 24 小时本地静默隔离。2026-10-13 上午观测（上游标记在
// 12 小时零请求的闲置期后仍未解除；300s 阶梯冷却到期即被重新选中、再 405）
// 确认 v2.9.18 的「默认纯阶梯」会把热账号循环喂给请求。本文件锁定默认行为，
// 防止再被静默改回「冷却到期即入池」。
func TestDefaultRiskSilentHoursIs24(t *testing.T) {
	if config.RiskSilentHours != 24 {
		t.Fatalf("产品默认静默窗口应为 24 小时，实际 %d", config.RiskSilentHours)
	}
}

// 默认配置（不覆盖 RiskSilentHours）：405 命中后即使阶梯冷却到期，静默闸门
// 仍须挡住重新选号（不再出站）；把观测时间推到窗口外后才允许重新调用。
func TestRiskSilentDefaultBlocksAfterCoolingExpires(t *testing.T) {
	f := newFixture(t)
	f.respond = func(n int, _ *http.Request) (int, http.Header, string) {
		if n == 1 {
			return jsonResp(405, `{"code":3012,"msg":"request has been blocked due to unusual activity."}`)(n, nil)
		}
		return jsonResp(200, okUpstreamJSON)(n, nil)
	}
	acc, err := f.st.AddAccount(model.ProviderZai, "silent-default", "synthetic-silent-secret")
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := f.post(t, msgBody(), "sk-test"); status != 503 {
		t.Fatalf("单账号第一次风控后应暂不可用: %d", status)
	}
	first := f.st.FindAny(acc.ID)
	if first.Status != model.StatusCooling || first.RiskControlStreak != 1 || first.LastRiskControlAt == nil {
		t.Fatal("首次风控应记录阶梯与历史时间")
	}
	// 只推进阶梯冷却到期，不动风控观测时间：默认静默闸门必须继续挡住。
	_, err = f.st.Update(acc.Provider, acc.ID, func(a *model.Account) {
		expired := float64(time.Now().Add(-time.Second).Unix())
		a.CoolingUntil = &expired
	})
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := f.post(t, msgBody(), "sk-test"); status != 503 || f.callCount() != 1 {
		t.Fatal("默认静默期内，冷却到期不得重新出站")
	}
	// 把风控观测时间推到默认窗口之外：允许重新调用并成功。
	_, err = f.st.Update(acc.Provider, acc.ID, func(a *model.Account) {
		old := float64(time.Now().Add(-time.Duration(config.RiskSilentHours+1) * time.Hour).Unix())
		a.LastRiskControlAt = &old
	})
	if err != nil {
		t.Fatal(err)
	}
	if status, body := f.post(t, msgBody(), "sk-test"); status != 200 {
		t.Fatalf("静默窗口外应重新调用: %d %s", status, body)
	}
	got := f.st.FindAny(acc.ID)
	if f.callCount() != 2 || got.RiskControlStreak != 0 || got.LastRiskControlAt == nil {
		t.Fatal("成功应清连续计数，保留观测历史")
	}
}

// 显式关闭静默窗口（ZCODE_RISK_SILENT_HOURS=0）时回退纯阶梯：每档到期即
// 重新参与选号。这是应急回退路径，行为与 v2.9.18 默认一致。
func TestRiskLadderPureCoolingWhenSilentDisabled(t *testing.T) {
	oldHours := config.RiskSilentHours
	config.RiskSilentHours = 0
	t.Cleanup(func() { config.RiskSilentHours = oldHours })

	f := newFixture(t)
	f.respond = func(n int, _ *http.Request) (int, http.Header, string) {
		if n == 1 {
			return jsonResp(405, `{"code":3012,"msg":"request has been blocked due to unusual activity."}`)(n, nil)
		}
		return jsonResp(200, okUpstreamJSON)(n, nil)
	}
	acc, err := f.st.AddAccount(model.ProviderZai, "ladder-pure", "synthetic-ladder-secret")
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := f.post(t, msgBody(), "sk-test"); status != 503 {
		t.Fatalf("单账号第一次风控后应暂不可用: %d", status)
	}
	first := f.st.FindAny(acc.ID)
	if first.Status != model.StatusCooling || first.RiskControlStreak != 1 || first.LastRiskControlAt == nil {
		t.Fatal("首次风控应记录阶梯与历史时间")
	}
	if status, _ := f.post(t, msgBody(), "sk-test"); status != 503 || f.callCount() != 1 {
		t.Fatal("尚未冷却到期不得再次出站")
	}
	// 只推进当前阶梯，不清风控历史。静默窗口显式关闭后，到期应直接重新调用。
	_, err = f.st.Update(acc.Provider, acc.ID, func(a *model.Account) {
		expired := float64(time.Now().Add(-time.Second).Unix())
		a.CoolingUntil = &expired
	})
	if err != nil {
		t.Fatal(err)
	}
	if status, body := f.post(t, msgBody(), "sk-test"); status != 200 {
		t.Fatalf("显式关闭静默后，阶梯到期应重新调用: %d %s", status, body)
	}
	got := f.st.FindAny(acc.ID)
	if f.callCount() != 2 || got.RiskControlStreak != 0 || got.LastRiskControlAt == nil {
		t.Fatal("成功应清连续计数，保留观测历史")
	}
}

// 显式关闭静默窗口时验证阶梯升级与超档失效机制（与静默窗口正交）。
func TestRiskLadderEscalatesAndStopsAtInvalid(t *testing.T) {
	oldHours := config.RiskSilentHours
	config.RiskSilentHours = 0
	t.Cleanup(func() { config.RiskSilentHours = oldHours })

	f := newFixture(t)
	f.respond = jsonResp(405, `{"code":3012,"msg":"request has been blocked due to unusual activity."}`)
	a, err := f.st.AddAccount(model.ProviderZai, "ladder-escalate", "synthetic-escalate-secret")
	if err != nil {
		t.Fatal(err)
	}
	for hit, seconds := range []int{300, 900, 3600, 0} {
		before := float64(time.Now().Unix())
		_, _ = f.post(t, msgBody(), "sk-test")
		got := f.st.FindAny(a.ID)
		if f.callCount() != hit+1 || got.RiskControlStreak != hit+1 {
			t.Fatal("每档到期应允许下一次独立尝试")
		}
		if seconds == 0 {
			if got.Status != model.StatusInvalid || got.CoolingUntil != nil {
				t.Fatal("超出三档必须失效，不能无限重试")
			}
			if f.st.Select(a.Provider, nil, "GLM-5.3") != nil {
				t.Fatal("失效不能自动重入池")
			}
			break
		}
		if got.Status != model.StatusCooling || got.CoolingUntil == nil || *got.CoolingUntil < before+float64(seconds) || *got.CoolingUntil > before+float64(seconds)+5 {
			t.Fatalf("第%d档应冷却%d秒", hit+1, seconds)
		}
		_, err = f.st.Update(a.Provider, a.ID, func(a *model.Account) { expired := before - 1; a.CoolingUntil = &expired })
		if err != nil {
			t.Fatal(err)
		}
	}
}
