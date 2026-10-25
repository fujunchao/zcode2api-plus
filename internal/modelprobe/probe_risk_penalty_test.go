package modelprobe_test

import (
	"testing"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

// requireNoAccountRisk 断言账号没有被巡检写入任何风控/错误状态。
// screen/target 阶段的 405 只是「目标线路嫌疑」，不应让执行巡检的账号背账。
func requireNoAccountRisk(t *testing.T, live *model.Account) {
	t.Helper()
	if live == nil {
		t.Fatal("账号丢失")
	}
	if live.RiskControlStreak != 0 || live.LastRiskControlAt != nil || live.LastErrorKind != nil || live.LastErrorAt != nil {
		t.Fatalf("巡检不应写账号风控/错误：streak=%d last_risk=%v kind=%v", live.RiskControlStreak, live.LastRiskControlAt, live.LastErrorKind)
	}
	if live.Status == model.StatusCooling || live.Status == model.StatusInvalid {
		t.Fatalf("巡检不应改变账号状态：%s", live.Status)
	}
}

// 目标线路连续的 405 只应淘汰线路，不能冷却执行巡检的健康账号。
func TestProbeTargetRiskDoesNotTouchAccount(t *testing.T) {
	f := newFixture(t)
	f.respond = func(route, account string, n int) (int, string) {
		if route == "target" {
			return 405, riskMessage
		}
		return 200, completeMessage
	}
	r := f.drive(t)
	if r.Verdict != "route_correlated" || r.DecisionID == "" {
		t.Fatalf("线路证据与退役链不应受影响：%+v", r)
	}
	if profiles := f.st.ListProxyProfiles(); len(profiles) != 1 || profiles[0].ID != f.reference.ID {
		t.Fatalf("目标线路仍应被淘汰：%+v", profiles)
	}
	for _, a := range f.accounts {
		requireNoAccountRisk(t, f.st.FindAny(a.ID))
	}
	// 账号不罚不等于不记录：run 里仍保留 405 证据供退役判定与复盘。
	riskSample := false
	for _, s := range r.Samples {
		if s.Kind == "risk_control" {
			riskSample = true
			break
		}
	}
	if !riskSample {
		t.Fatal("巡检 run 仍应记录 risk_control 证据")
	}
}

// 参考线失败才是账号侧证据：只冷却在参考线失败的那个账号，同 run 的另一个账号不受影响。
func TestProbeReferenceRiskCoolsOnlyFailingAccount(t *testing.T) {
	f := newFixture(t)
	f.respond = func(route, account string, n int) (int, string) {
		if route == "target" {
			return 405, riskMessage
		}
		if account == f.accounts[0].Secret() {
			return 405, riskMessage
		}
		return 200, completeMessage
	}
	r := f.drive(t)
	if r.Verdict != "route_correlated" || r.DecisionID == "" {
		t.Fatalf("账号侧证据不应改变线路归因与退役：%+v", r)
	}
	bad := f.st.FindAny(f.accounts[0].ID)
	if bad.LastErrorKind == nil || *bad.LastErrorKind != model.ErrorKindRiskControl || bad.LastRiskControlAt == nil || bad.RiskControlStreak < 1 {
		t.Fatalf("参考线失败的账号应保留风控记录：streak=%d kind=%v last_risk=%v", bad.RiskControlStreak, bad.LastErrorKind, bad.LastRiskControlAt)
	}
	requireNoAccountRisk(t, f.st.FindAny(f.accounts[1].ID))
}

// 回滚开关 all：恢复旧行为，目标阶段的 405 也惩罚账号。
func TestProbeRiskPenaltyAllRestoresLegacy(t *testing.T) {
	old := config.ProbeRiskPenalty
	config.ProbeRiskPenalty = "all"
	t.Cleanup(func() { config.ProbeRiskPenalty = old })

	f := newFixture(t)
	f.respond = func(route, account string, n int) (int, string) {
		if route == "target" {
			return 405, riskMessage
		}
		return 200, completeMessage
	}
	if r := f.drive(t); r.Verdict != "route_correlated" {
		t.Fatalf("回滚开关不应影响线路归因：%+v", r)
	}
	live := f.st.FindAny(f.accounts[0].ID)
	if live.LastRiskControlAt == nil || live.RiskControlStreak < 1 {
		t.Fatalf("all 应恢复「目标阶段也罚账号」的旧行为：streak=%d last_risk=%v", live.RiskControlStreak, live.LastRiskControlAt)
	}
}

// 关闭开关 off：巡检完全不写账号，但线路证据与退役链不受影响。
func TestProbeRiskPenaltyOffLeavesAccountsUntouched(t *testing.T) {
	old := config.ProbeRiskPenalty
	config.ProbeRiskPenalty = "off"
	t.Cleanup(func() { config.ProbeRiskPenalty = old })

	f := newFixture(t)
	f.respond = func(route, account string, n int) (int, string) {
		if route == "target" {
			return 405, riskMessage
		}
		if account == f.accounts[0].Secret() {
			return 405, riskMessage
		}
		return 200, completeMessage
	}
	r := f.drive(t)
	if r.Verdict != "route_correlated" || r.DecisionID == "" {
		t.Fatalf("off 只关闭账号写入，不应影响退役：%+v", r)
	}
	for _, a := range f.accounts {
		requireNoAccountRisk(t, f.st.FindAny(a.ID))
	}
}
