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

// 阈值按不同「干净线路」计数，不按账号、也不按命中次数。
//
// 2026-10-17 日志：9 次请求级判定里 8 次是错的——同一份内容在 41–149 秒后换条线
// 即可成功，说明触发源是脏出口而非内容。按账号计数会把「抽到两条脏线」误判成
// 「内容被拒」，进而锁死整条请求。
func TestRiskScopeVerdictCountsDistinctCleanLines(t *testing.T) {
	old := RiskControlRequestLevelThreshold
	RiskControlRequestLevelThreshold = 3
	t.Cleanup(func() { RiskControlRequestLevelThreshold = old })

	sc := NewRiskScope("model|content")
	a := &model.Account{ID: "acc-1"}
	b := &model.Account{ID: "acc-2"}
	c := &model.Account{ID: "acc-3"}

	if sc.Verdict(a, "proxy:line-1", false) {
		t.Fatal("第 1 条线路不该直接判定为请求级")
	}
	if sc.Verdict(a, "proxy:line-1", false) {
		t.Fatal("同一条线路重复命中不得计入阈值——那不是第二条线路")
	}
	if sc.Lines() != 1 {
		t.Fatalf("按线路去重后应只有 1 条: %d", sc.Lines())
	}
	if sc.Accounts() != 1 {
		t.Fatalf("账号计数仅用于日志，应为 1: %d", sc.Accounts())
	}
	if sc.Verdict(b, "proxy:line-2", false) {
		t.Fatal("第 2 条线路仍不该直接判定为请求级")
	}
	if !sc.Verdict(c, "proxy:line-3", false) {
		t.Fatal("第 3 条不同线路应触发请求级判定")
	}
	if sc.ContentKey() != "model|content" {
		t.Fatalf("ContentKey 应原样保存: %q", sc.ContentKey())
	}
}

// 命中时已脏的线路不计入：它的 405 只证明「这条线脏」，不能当作「内容有问题」的
// 证据。否则连着抽两条脏线就会把阈值凑满，把本可成功的内容判死。
func TestRiskScopeVerdictExcludesDirtyLines(t *testing.T) {
	old := RiskControlRequestLevelThreshold
	RiskControlRequestLevelThreshold = 3
	t.Cleanup(func() { RiskControlRequestLevelThreshold = old })

	sc := NewRiskScope("model|content")
	if sc.Verdict(&model.Account{ID: "acc-1"}, "proxy:line-1", false) {
		t.Fatal("第 1 条干净线路不该直接请求级")
	}
	if sc.Verdict(&model.Account{ID: "acc-2"}, "proxy:line-2", true) {
		t.Fatal("已知脏线的命中不得计入")
	}
	if sc.Lines() != 1 {
		t.Fatalf("脏线不计入，应只有 1 条干净线路: %d", sc.Lines())
	}
	if sc.Verdict(&model.Account{ID: "acc-3"}, "proxy:line-3", false) {
		t.Fatal("只有 2 条干净线路时不该判定请求级")
	}
	if !sc.Verdict(&model.Account{ID: "acc-4"}, "proxy:line-4", false) {
		t.Fatal("第 3 条干净线路应触发请求级判定")
	}
	// 直连也参与聚合（同一份内容在直连上被拒同样是有效证据）。
	sc2 := NewRiskScope("model|content")
	sc2.Verdict(&model.Account{ID: "d-1"}, "direct", false)
	if sc2.Lines() != 1 {
		t.Fatalf("直连应作为一条线路计入: %d", sc2.Lines())
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

// 线路归因的 405：证据与命中时刻必须落库，但账号状态与阶梯不得变化。
func TestNoteRiskControlWithoutCoolingKeepsStatus(t *testing.T) {
	st := openStore(t)
	acc, err := st.AddAccount(model.ProviderZai, "nocool", "sk-1")
	if err != nil {
		t.Fatal(err)
	}
	NoteRiskControlWithoutCooling(st, acc, "上游风控拦截 HTTP 405（线路归因）: x", time.Now())
	got := st.Find(model.ProviderZai, acc.ID)
	if got.Status != model.StatusActive || got.CoolingUntil != nil {
		t.Fatalf("不应冷却账号: status=%s until=%v", got.Status, got.CoolingUntil)
	}
	if got.RiskControlStreak != 0 {
		t.Fatalf("不应推进阶梯: %d", got.RiskControlStreak)
	}
	if got.LastErrorKind == nil || *got.LastErrorKind != model.ErrorKindRiskControl {
		t.Fatalf("错误归类应落库: %v", got.LastErrorKind)
	}
	if got.LastRiskControlAt == nil {
		t.Fatal("命中时刻应落库")
	}
}
