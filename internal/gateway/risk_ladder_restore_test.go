package gateway

import (
	"net/http"
	"testing"
	"time"

	"zcode2api/internal/model"
)

// 不覆盖 RiskSilentHours：验证产品默认值确实恢复阶梯，而非测试强行关闭额外隔离。
func TestRiskLadderDefaultReturnsAfterCooling(t *testing.T) {
	f := newFixture(t)
	f.respond = func(n int, _ *http.Request) (int, http.Header, string) {
		if n == 1 {
			return jsonResp(405, `{"code":3012,"msg":"request has been blocked due to unusual activity."}`)(n, nil)
		}
		return jsonResp(200, okUpstreamJSON)(n, nil)
	}
	acc, err := f.st.AddAccount(model.ProviderZai, "ladder-default", "synthetic-ladder-secret")
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
	// 只推进当前阶梯，不清风控历史。到期后不应再等另一个默认 24 小时窗口。
	_, err = f.st.Update(acc.Provider, acc.ID, func(a *model.Account) {
		expired := float64(time.Now().Add(-time.Second).Unix())
		a.CoolingUntil = &expired
	})
	if err != nil {
		t.Fatal(err)
	}
	if status, body := f.post(t, msgBody(), "sk-test"); status != 200 {
		t.Fatalf("默认阶梯到期应重新调用而非额外隔离: %d %s", status, body)
	}
	got := f.st.FindAny(acc.ID)
	if f.callCount() != 2 || got.RiskControlStreak != 0 || got.LastRiskControlAt == nil {
		t.Fatal("成功应清连续计数，保留观测历史")
	}
}
