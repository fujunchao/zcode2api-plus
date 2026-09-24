// 风控「账号级 / 请求级」判定的单元测试：阈值去重、冷却施加。
// 端到端行为（多账号同 body 风控 → 冷却保留 + 重放防护）在 engine_test.go 与
// asyncpool/pool_test.go，以及 replayguard_test.go。
//
// ⚠️ 曾有的「快照回滚」测试（正常回滚 / 并发放弃 / invalid 不回滚）随回滚机制一起删除：
// 账号级标记模型下请求级判定的处置是**保留冷却**，回滚等于替服务端解封账号
// （docs/analysis-auth-chain-vs-official.md §九）。
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

	sc := NewRiskScope("model|content")
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
	if sc.ContentKey() != "model|content" {
		t.Fatalf("ContentKey 应原样保存: %q", sc.ContentKey())
	}
}

// 首次命中：调度三字段进入冷却，证据章落库 —— 这是账号级与请求级共用的动作
// （请求级时账号同样被上游标记，本地冷却必须保留）。
func TestMarkRiskControlAppliesCooling(t *testing.T) {
	st := openStore(t)
	acc, err := st.AddAccount(model.ProviderZai, "rb", "sk-1")
	if err != nil {
		t.Fatal(err)
	}
	// 钉住阶梯，避免依赖环境默认值。
	if err := st.SetSetting(store.RiskCoolingStepsKey, "300,900,3600"); err != nil {
		t.Fatal(err)
	}

	secs, streak, invalid := MarkRiskControl(st, model.ProviderZai, acc.ID, "上游风控拦截 HTTP 405: x", time.Now())
	if streak != 1 || invalid {
		t.Fatalf("首次命中应为 1 次、非失效: streak=%d invalid=%v", streak, invalid)
	}
	if secs != 300 {
		t.Fatalf("第一档冷却应为 300s: %d", secs)
	}
	got := st.Find(model.ProviderZai, acc.ID)
	if got.Status != model.StatusCooling || got.CoolingUntil == nil {
		t.Fatalf("应进入冷却: status=%s until=%v", got.Status, got.CoolingUntil)
	}
	if got.LastErrorKind == nil || *got.LastErrorKind != model.ErrorKindRiskControl {
		t.Fatalf("证据必须落库: %v", got.LastErrorKind)
	}
}
