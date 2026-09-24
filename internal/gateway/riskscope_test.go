// 风控「账号级 / 请求级」判定的单元测试：阈值去重、快照回滚的两道闸。
// 端到端行为（多账号同 body 风控 → 0 冷却）在 engine_test.go 与 asyncpool/pool_test.go。
package gateway

import (
	"testing"
	"time"

	"zcode2api/internal/model"
	"zcode2api/internal/store"
)

// 阈值按不同账号 ID 计数，不按命中次数：同一账号因验证码刷新 / 瞬时限流原地重试会重复
// 进入风控分支，按次数计会把「同一个号被拒两次」误判成第二个身份。
func TestRiskScopeVerdictCountsDistinctAccounts(t *testing.T) {
	old := RiskControlRequestLevelThreshold
	RiskControlRequestLevelThreshold = 2
	t.Cleanup(func() { RiskControlRequestLevelThreshold = old })

	sc := NewRiskScope()
	a := &model.Account{ID: "acc-1"}
	b := &model.Account{ID: "acc-2"}

	if sc.Verdict(a) {
		t.Fatal("第 1 个账号不该直接判定为请求级")
	}
	if sc.Verdict(a) {
		t.Fatal("同一个账号重复命中不得计入阈值——那不是第二个身份")
	}
	if sc.Accounts() != 1 {
		t.Fatalf("按 ID 去重后应只有 1 个账号: %d", sc.Accounts())
	}
	if !sc.Verdict(b) {
		t.Fatal("第 2 个不同账号应触发请求级判定")
	}
}

// 正常回滚：调度三字段（Status / CoolingUntil / RiskControlStreak）复原，
// 但 last_error_kind 这条**证据**必须保留——判定的是「不惩罚账号」，不是「没发生过」。
func TestRollbackRiskControlRestoresSchedulingFields(t *testing.T) {
	st := openStore(t)
	acc, err := st.AddAccount(model.ProviderZai, "rb", "sk-1")
	if err != nil {
		t.Fatal(err)
	}

	_, streak, invalid, snap := MarkRiskControlWithSnapshot(st, model.ProviderZai, acc.ID, "上游风控拦截 HTTP 405: x", time.Now())
	if streak != 1 || invalid {
		t.Fatalf("首次命中应为 1 次、非失效: streak=%d invalid=%v", streak, invalid)
	}
	if got := st.Find(model.ProviderZai, acc.ID); got.Status != model.StatusCooling || got.CoolingUntil == nil {
		t.Fatalf("先要真的施加冷却: status=%s until=%v", got.Status, got.CoolingUntil)
	}

	restored, skipped := RollbackRiskControl(st, snap)
	if !restored || skipped {
		t.Fatalf("未并发改动时应回滚成功: restored=%v skipped=%v", restored, skipped)
	}
	got := st.Find(model.ProviderZai, acc.ID)
	if got.Status != model.StatusActive || got.CoolingUntil != nil {
		t.Fatalf("调度状态应复原: status=%s until=%v", got.Status, got.CoolingUntil)
	}
	if got.RiskControlStreak != 0 {
		t.Fatalf("风控阶梯应复原为 0: %d", got.RiskControlStreak)
	}
	if got.LastErrorKind == nil || *got.LastErrorKind != model.ErrorKindRiskControl {
		t.Fatalf("证据必须保留: %v", got.LastErrorKind)
	}
	if got.LastError == nil || *got.LastError == "" {
		t.Fatal("证据文案必须保留")
	}
}

// 并发守卫：回滚前被第三方（管理员改状态 / 另一路请求）动过的账号必须放弃回滚，
// 否则会用本请求的旧前像覆盖别人的决定。
func TestRollbackRiskControlSkipsWhenStateChanged(t *testing.T) {
	st := openStore(t)
	acc, err := st.AddAccount(model.ProviderZai, "rb-race", "sk-1")
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, snap := MarkRiskControlWithSnapshot(st, model.ProviderZai, acc.ID, "上游风控拦截 HTTP 405: x", time.Now())

	kind := model.ErrorKindUpstreamError
	if _, err := st.Update(model.ProviderZai, acc.ID, func(a *model.Account) {
		a.RiskControlStreak = 5
		a.LastErrorKind = &kind
	}); err != nil {
		t.Fatal(err)
	}

	restored, skipped := RollbackRiskControl(st, snap)
	if restored || !skipped {
		t.Fatalf("状态已被第三方改动时应放弃回滚: restored=%v skipped=%v", restored, skipped)
	}
	got := st.Find(model.ProviderZai, acc.ID)
	if got.RiskControlStreak != 5 {
		t.Fatalf("第三方写入不得被覆盖: %d", got.RiskControlStreak)
	}
	if got.LastErrorKind == nil || *got.LastErrorKind != model.ErrorKindUpstreamError {
		t.Fatalf("第三方的错误归类不得被覆盖: %v", got.LastErrorKind)
	}
}

// 已升级 invalid 的账号不回滚：走到 invalid 说明该账号的风控阶梯早已耗尽，本次只是压垮
// 它的最后一根稻草；复活它只会让下一个请求再吃一次同样的 405。
func TestRollbackRiskControlSkipsInvalidUpgrade(t *testing.T) {
	st := openStore(t)
	acc, err := st.AddAccount(model.ProviderZai, "rb-invalid", "sk-1")
	if err != nil {
		t.Fatal(err)
	}
	// 钉住阶梯长度（默认 3 档 → 第 4 次命中升失效），避免依赖环境变量默认值。
	if err := st.SetSetting(store.RiskCoolingStepsKey, "300,900,3600"); err != nil {
		t.Fatal(err)
	}

	var snap RiskControlSnapshot
	for i := 1; i <= 4; i++ {
		_, streak, invalid, s := MarkRiskControlWithSnapshot(st, model.ProviderZai, acc.ID, "上游风控拦截 HTTP 405: x", time.Now())
		if i == 4 {
			if !invalid {
				t.Fatalf("第 4 次命中应超过 3 档阶梯并升为失效: streak=%d invalid=%v", streak, invalid)
			}
			snap = s
		}
	}
	if got := st.Find(model.ProviderZai, acc.ID); got.Status != model.StatusInvalid {
		t.Fatalf("超档应为 invalid: %s", got.Status)
	}

	restored, skipped := RollbackRiskControl(st, snap)
	if restored || !skipped {
		t.Fatalf("invalid 不该被回滚: restored=%v skipped=%v", restored, skipped)
	}
	if got := st.Find(model.ProviderZai, acc.ID); got.Status != model.StatusInvalid {
		t.Fatalf("invalid 状态必须保持: %s", got.Status)
	}
}
