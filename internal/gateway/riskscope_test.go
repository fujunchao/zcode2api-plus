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

	"zcode2api/internal/config"
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
	now := time.Now()
	a := &model.Account{ID: "acc-1"}
	b := &model.Account{ID: "acc-2"}

	if sc.Verdict(a, now) {
		t.Fatal("第 1 个账号不该直接判定为请求级")
	}
	if sc.Verdict(a, now) {
		t.Fatal("同一个账号重复命中不得计入阈值——那不是第二个身份")
	}
	if sc.Accounts() != 1 {
		t.Fatalf("按 ID 去重后应只有 1 个账号: %d", sc.Accounts())
	}
	if !sc.Verdict(b, now) {
		t.Fatal("第 2 个不同账号应触发请求级判定")
	}
	if sc.ContentKey() != "model|content" {
		t.Fatalf("ContentKey 应原样保存: %q", sc.ContentKey())
	}
}

// 风控静默期内的账号（近期已被上游标记）命中 405 不计入请求级判定：它的 405 只反映
// 账号自身状态、与内容无关。2026-10-12 晚污染池里「连抽两个热账号」被误判成请求级，
// 错误地停止换号并封锁同内容——那 33 分钟全池风暴的直接机制。
func TestRiskScopeVerdictExcludesHotAccounts(t *testing.T) {
	old := RiskControlRequestLevelThreshold
	RiskControlRequestLevelThreshold = 2
	t.Cleanup(func() { RiskControlRequestLevelThreshold = old })
	oldHours := config.RiskSilentHours
	config.RiskSilentHours = 24
	t.Cleanup(func() { config.RiskSilentHours = oldHours })

	now := time.Now()
	blockedAt := float64(now.Add(-1*time.Hour).UnixNano()) / 1e9
	hot := &model.Account{ID: "hot-1", LastRiskControlAt: &blockedAt}
	hot2 := &model.Account{ID: "hot-2", LastRiskControlAt: &blockedAt}
	cleanA := &model.Account{ID: "clean-1"}
	cleanB := &model.Account{ID: "clean-2"}

	sc := NewRiskScope("model|content")
	if sc.Verdict(hot, now) || sc.Verdict(hot2, now) {
		t.Fatal("热账号的命中不得计入请求级判定")
	}
	if sc.Accounts() != 0 {
		t.Fatalf("热账号不计入，计数应为 0: %d", sc.Accounts())
	}
	if sc.Verdict(cleanA, now) {
		t.Fatal("第 1 个干净账号不该直接判定为请求级")
	}
	if !sc.Verdict(cleanB, now) {
		t.Fatal("第 2 个干净账号应触发请求级判定")
	}
	if sc.Accounts() != 2 {
		t.Fatalf("应只统计干净账号: %d", sc.Accounts())
	}

	// 静默期已过的账号视为干净：命中照常计入（恢复后再次被拒属新证据）。
	oldBlocked := float64(now.Add(-25*time.Hour).UnixNano()) / 1e9
	recovered := &model.Account{ID: "recovered-1", LastRiskControlAt: &oldBlocked}
	sc2 := NewRiskScope("model|content")
	if sc2.Verdict(recovered, now) {
		t.Fatal("静默期已过的账号应计入判定（第 1 个仍不该直接请求级）")
	}
	if sc2.Accounts() != 1 {
		t.Fatalf("静默期已过的账号应被计入: %d", sc2.Accounts())
	}

	// 闸门关闭（应急回退）时退回旧行为：任何命中都计入。
	config.RiskSilentHours = 0
	sc3 := NewRiskScope("model|content")
	if sc3.Verdict(hot, now) {
		t.Fatal("关闭后第 1 个仍不该直接请求级")
	}
	if sc3.Accounts() != 1 {
		t.Fatalf("闸门关闭时热账号也应计入: %d", sc3.Accounts())
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

	secs, streak, invalid := MarkRiskControl(st, acc, "上游风控拦截 HTTP 405: x", time.Now())
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
	// 静默期证据章：命中时刻必须落库（选号硬闸门与恢复护栏的唯一判据来源）。
	if got.LastRiskControlAt == nil {
		t.Fatal("命中时刻必须落库（LastRiskControlAt），否则静默期闸门失效")
	}
}
