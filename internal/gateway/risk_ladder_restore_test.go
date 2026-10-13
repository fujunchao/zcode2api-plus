package gateway

import (
	"net/http"
	"testing"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

// 默认回到纯阶梯，不把本地观测当作供应商封禁期限；显式隔离另行测试。
func TestDefaultRiskSilentHoursUsesLadder(t *testing.T) {
	if config.RiskSilentHours != 0 {
		t.Fatalf("默认应关闭额外静默窗口，实际 %d", config.RiskSilentHours)
	}
}

// 显式配置 24 小时额外隔离时，保留原有闸门与窗口外恢复语义。
func TestRiskSilentExplicitWindowBlocksAfterCoolingExpires(t *testing.T) {
	oldHours := config.RiskSilentHours
	config.RiskSilentHours = 24
	t.Cleanup(func() { config.RiskSilentHours = oldHours })

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
	// 只推进阶梯冷却到期，不动风控观测时间：显式静默闸门必须继续挡住。
	_, err = f.st.Update(acc.Provider, acc.ID, func(a *model.Account) {
		expired := float64(time.Now().Add(-time.Second).Unix())
		a.CoolingUntil = &expired
	})
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := f.post(t, msgBody(), "sk-test"); status != 503 || f.callCount() != 1 {
		t.Fatal("显式静默期内，冷却到期不得重新出站")
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

// 不覆盖默认值：阶梯到期后即使风控观测很新，也应重新参与选号。
func TestRiskLadderDefaultCooling(t *testing.T) {
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
	// 只推进当前阶梯，不清风控历史。默认不叠加额外静默，到期应重新调用。
	_, err = f.st.Update(acc.Provider, acc.ID, func(a *model.Account) {
		expired := float64(time.Now().Add(-time.Second).Unix())
		a.CoolingUntil = &expired
	})
	if err != nil {
		t.Fatal(err)
	}
	if status, body := f.post(t, msgBody(), "sk-test"); status != 200 {
		t.Fatalf("默认阶梯到期应重新调用: %d %s", status, body)
	}
	got := f.st.FindAny(acc.ID)
	if f.callCount() != 2 || got.RiskControlStreak != 0 || got.LastRiskControlAt == nil {
		t.Fatal("成功应清连续计数，保留观测历史")
	}
}

// 默认阶梯升级与超档失效机制，不因关闭额外静默而无限重试。
func TestRiskLadderEscalatesAndStopsAtInvalid(t *testing.T) {
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
