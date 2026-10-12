// 风控失效「手动恢复」出口的测试：判定边界（仅 invalid+risk_control 可恢复）、
// 字段转移（清冷却/错误证据/风控连击）、持久化、批量逐条语义，以及
// 「启停不是解封后门」契约回归。行为规格见 docs/plan-riskcontrol-manual-recovery-20261012.md。
package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

// markRiskControlInvalid 把账号置为「风控失效」终态（连击超档后的形态）。
func markRiskControlInvalid(t *testing.T, s *Store, id string) {
	t.Helper()
	kind := model.ErrorKindRiskControl
	msg := "request has been blocked due to unusual activity."
	now := float64(time.Now().UnixNano()) / 1e9
	if _, err := s.Update(model.ProviderZai, id, func(a *model.Account) {
		a.Status = model.StatusInvalid
		a.LastError = &msg
		a.LastErrorKind = &kind
		a.LastErrorAt = &now
		a.RiskControlStreak = 4 // 默认三档 ⇒ 第 4 次命中后失效
	}); err != nil {
		t.Fatalf("造态失败: %v", err)
	}
}

func TestRecoverAccountClearsRiskState(t *testing.T) {
	s := newTestStore(t)
	acc, err := s.AddAccount(model.ProviderZai, "risk", "risk-secret")
	if err != nil {
		t.Fatal(err)
	}
	markRiskControlInvalid(t, s, acc.ID)
	// invalid 通常无冷却，但字段本身要清干净（恢复出口同样兜住 cooling 残留）。
	until := float64(time.Now().Add(time.Hour).UnixNano()) / 1e9
	if _, err := s.Update(model.ProviderZai, acc.ID, func(a *model.Account) {
		a.CoolingUntil = &until
	}); err != nil {
		t.Fatal(err)
	}

	found, err := s.RecoverAccount(model.ProviderZai, acc.ID)
	if err != nil || !found {
		t.Fatalf("恢复失败: found=%v err=%v", found, err)
	}
	got := s.FindAny(acc.ID)
	if got.Status != model.StatusActive {
		t.Fatalf("恢复后应为 active，实际 %s", got.Status)
	}
	if got.CoolingUntil != nil {
		t.Fatalf("恢复后冷却应被清空: %v", *got.CoolingUntil)
	}
	if got.RiskControlStreak != 0 {
		t.Fatalf("恢复后风控连击应清零，实际 %d", got.RiskControlStreak)
	}
	if got.LastError != nil || got.LastErrorKind != nil || got.LastErrorAt != nil {
		t.Fatalf("恢复后不应残留错误证据: %v/%v/%v", got.LastError, got.LastErrorKind, got.LastErrorAt)
	}
	if !got.Enabled || got.ArchivedAt != nil {
		t.Fatalf("恢复不得改动 Enabled/ArchivedAt: enabled=%v archived=%v", got.Enabled, got.ArchivedAt)
	}
	if !got.IsSelectable(time.Now()) {
		t.Fatal("恢复的意义就是重新可调度：IsSelectable 应为 true")
	}
}

// 恢复与启停正交：被停用的风控失效账号恢复后 Status=active，但 Enabled 保持
// false（仍不可调度），需管理员显式启用——恢复不是「启停后门」的另一面。
func TestRecoverKeepsDisabledFlagOrthogonal(t *testing.T) {
	s := newTestStore(t)
	acc, err := s.AddAccount(model.ProviderZai, "risk-off", "risk-off-secret")
	if err != nil {
		t.Fatal(err)
	}
	markRiskControlInvalid(t, s, acc.ID)
	if _, err := s.SetEnabled(model.ProviderZai, acc.ID, false); err != nil {
		t.Fatal(err)
	}

	if _, err := s.RecoverAccount(model.ProviderZai, acc.ID); err != nil {
		t.Fatal(err)
	}
	got := s.FindAny(acc.ID)
	if got.Status != model.StatusActive || got.Enabled {
		t.Fatalf("恢复应只动状态、不动 Enabled: status=%s enabled=%v", got.Status, got.Enabled)
	}
	if got.IsSelectable(time.Now()) {
		t.Fatal("停用中的账号恢复后仍不得被调度")
	}
}

func TestRecoverAccountRejectsOtherStates(t *testing.T) {
	kindRisk := model.ErrorKindRiskControl
	kindAuth := model.ErrorKindAuthFailed
	future := float64(time.Now().Add(time.Hour).UnixNano()) / 1e9
	cases := []struct {
		name    string
		prepare func(a *model.Account)
	}{
		{"active", func(a *model.Account) {}},
		{"冷却中（风控第 N 次命中）", func(a *model.Account) {
			a.Status = model.StatusCooling
			a.CoolingUntil = &future
			a.LastErrorKind = &kindRisk
		}},
		{"凭据失效", func(a *model.Account) {
			a.Status = model.StatusInvalid
			a.LastErrorKind = &kindAuth
		}},
		{"额度耗尽", func(a *model.Account) { a.Status = model.StatusExhausted }},
		{"已停用", func(a *model.Account) { a.Status = model.StatusDisabled }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			acc, err := s.AddAccount(model.ProviderZai, "acc", "secret-"+tc.name)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Update(model.ProviderZai, acc.ID, tc.prepare); err != nil {
				t.Fatal(err)
			}
			before := s.FindAny(acc.ID)

			found, err := s.RecoverAccount(model.ProviderZai, acc.ID)
			if !found || !errors.Is(err, ErrAccountNotRecoverable) {
				t.Fatalf("应拒绝且返回 ErrAccountNotRecoverable: found=%v err=%v", found, err)
			}
			got := s.FindAny(acc.ID)
			if got.Status != before.Status ||
				!sameStringPtr(got.LastErrorKind, before.LastErrorKind) ||
				(got.CoolingUntil == nil) != (before.CoolingUntil == nil) {
				t.Fatalf("拒绝时字段必须零变化: before=%+v after=%+v", before, got)
			}
		})
	}
}

func TestRecoverAccountRejectsArchived(t *testing.T) {
	s := newTestStore(t)
	acc, err := s.AddAccount(model.ProviderZai, "archived-risk", "archived-risk-secret")
	if err != nil {
		t.Fatal(err)
	}
	markRiskControlInvalid(t, s, acc.ID)
	if _, err := s.SetArchived(model.ProviderZai, acc.ID, true); err != nil {
		t.Fatal(err)
	}

	found, err := s.RecoverAccount(model.ProviderZai, acc.ID)
	if !found || !errors.Is(err, ErrAccountArchived) {
		t.Fatalf("归档账号应被拒绝: found=%v err=%v", found, err)
	}
	if got := s.FindAny(acc.ID); got.Status != model.StatusInvalid {
		t.Fatalf("被拒绝的归档账号状态不得变化: %s", got.Status)
	}
}

func TestRecoverAccountNotFound(t *testing.T) {
	s := newTestStore(t)
	found, err := s.RecoverAccount(model.ProviderZai, "no-such-id")
	if found || err != nil {
		t.Fatalf("不存在应返回 (false, nil): found=%v err=%v", found, err)
	}
}

func TestRecoverAccountPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.db")
	s := openAt(t, path)
	acc, err := s.AddAccount(model.ProviderZai, "persist", "persist-secret")
	if err != nil {
		t.Fatal(err)
	}
	markRiskControlInvalid(t, s, acc.ID)
	if _, err := s.RecoverAccount(model.ProviderZai, acc.ID); err != nil {
		t.Fatal(err)
	}

	reopened := openAt(t, path)
	got := reopened.FindAny(acc.ID)
	if got == nil {
		t.Fatal("重开后账号丢失")
	}
	if got.Status != model.StatusActive || got.LastErrorKind != nil || got.CoolingUntil != nil {
		t.Fatalf("恢复结果应落库: status=%s kind=%v cooling=%v", got.Status, got.LastErrorKind, got.CoolingUntil)
	}
}

func TestBatchRecoverMixed(t *testing.T) {
	s := newTestStore(t)
	riskA, err := s.AddAccount(model.ProviderZai, "risk-a", "risk-a-secret")
	if err != nil {
		t.Fatal(err)
	}
	riskB, err := s.AddAccount(model.ProviderZai, "risk-b", "risk-b-secret")
	if err != nil {
		t.Fatal(err)
	}
	normal, err := s.AddAccount(model.ProviderZai, "normal", "normal-secret")
	if err != nil {
		t.Fatal(err)
	}
	markRiskControlInvalid(t, s, riskA.ID)
	markRiskControlInvalid(t, s, riskB.ID)

	res, err := s.BatchRecoverAccounts([]string{riskA.ID, normal.ID, "missing-id", riskB.ID}, true)
	if err != nil {
		t.Fatalf("批量恢复失败: %v", err)
	}
	if res.Total != 4 || res.Succeeded != 2 || res.NotFound != 1 || res.Failed != 1 {
		t.Fatalf("汇总计数不符: %+v", res)
	}
	if len(res.IDs) != 2 {
		t.Fatalf("IDs 应只含成功项: %v", res.IDs)
	}
	if res.Items[0].Status != BatchStatusOK || res.Items[1].Status != BatchStatusError ||
		res.Items[2].Status != BatchStatusNotFound || res.Items[3].Status != BatchStatusOK {
		t.Fatalf("逐条状态不符: %+v", res.Items)
	}
	if res.Items[1].Message == "" {
		t.Fatalf("非可恢复项应带原因: %+v", res.Items[1])
	}
	if got := s.FindAny(riskA.ID); got.Status != model.StatusActive || got.RiskControlStreak != 0 {
		t.Fatalf("批量成功项应已恢复: %+v", got)
	}
	if got := s.FindAny(normal.ID); got.Status != model.StatusActive || got.LastErrorKind != nil {
		t.Fatalf("非可恢复项不得被改动: %+v", got)
	}
}

func TestBatchRecoverStrictMissingZeroSideEffect(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.db")
	s := openAt(t, path)
	acc, err := s.AddAccount(model.ProviderZai, "risk", "strict-secret")
	if err != nil {
		t.Fatal(err)
	}
	markRiskControlInvalid(t, s, acc.ID)

	res, err := s.BatchRecoverAccounts([]string{acc.ID, "missing-id"}, false)
	if !errors.Is(err, ErrBatchMissingIDs) {
		t.Fatalf("严格模式含缺失应整体拒绝: err=%v res=%+v", err, res)
	}
	if got := s.FindAny(acc.ID); got.Status != model.StatusInvalid {
		t.Fatalf("严格模式拒绝时不得有任何改动: %s", got.Status)
	}
}

func TestBatchRecoverDedup(t *testing.T) {
	s := newTestStore(t)
	acc, err := s.AddAccount(model.ProviderZai, "risk", "dedup-secret")
	if err != nil {
		t.Fatal(err)
	}
	markRiskControlInvalid(t, s, acc.ID)

	res, err := s.BatchRecoverAccounts([]string{acc.ID, acc.ID}, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 1 || res.Succeeded != 1 {
		t.Fatalf("重复 ID 应去重: %+v", res)
	}
}

// 恢复出口不得松动「启停不是解封后门」契约：未恢复的风控失效账号在停用→启用
// 往返后仍保持 invalid 且不可调度；已恢复的账号则正常参与启停。
func TestRecoverDoesNotSoftenEnabledGuard(t *testing.T) {
	s := newTestStore(t)
	recovered, err := s.AddAccount(model.ProviderZai, "recovered", "recovered-secret")
	if err != nil {
		t.Fatal(err)
	}
	stuck, err := s.AddAccount(model.ProviderZai, "stuck", "stuck-secret")
	if err != nil {
		t.Fatal(err)
	}
	markRiskControlInvalid(t, s, recovered.ID)
	markRiskControlInvalid(t, s, stuck.ID)
	if _, err := s.RecoverAccount(model.ProviderZai, recovered.ID); err != nil {
		t.Fatal(err)
	}

	for _, enabled := range []bool{false, true} {
		if _, err := s.SetEnabled(model.ProviderZai, stuck.ID, enabled); err != nil {
			t.Fatal(err)
		}
	}
	got := s.FindAny(stuck.ID)
	if got.Status != model.StatusInvalid || got.IsSelectable(time.Now()) {
		t.Fatalf("未恢复的风控失效账号仍不得被启停复活: status=%s", got.Status)
	}
	if gotR := s.FindAny(recovered.ID); gotR.Status != model.StatusActive || !gotR.IsSelectable(time.Now()) {
		t.Fatalf("已恢复账号应正常调度: %+v", gotR)
	}
}

// 恢复护栏：仍在风控静默期（上游标记未过期）的账号不得直接放回轮转——转入保护冷却，
// 到期时刻与静默期终点重合、期满自动重新参与调度。
// 依据：2026-10-12 16:31 事故——批量恢复 51 个「本地失效但上游标记未过期」的账号，
// 它们立即重回轮转并被逐对消耗，33 分钟二次烧伤全池（恢复≠解除）。
func TestRecoverAccountProtectiveCoolingWithinSilentWindow(t *testing.T) {
	oldHours := config.RiskSilentHours
	config.RiskSilentHours = 24
	t.Cleanup(func() { config.RiskSilentHours = oldHours })

	s := newTestStore(t)
	acc, err := s.AddAccount(model.ProviderZai, "risk-recent", "risk-recent-secret")
	if err != nil {
		t.Fatal(err)
	}
	markRiskControlInvalid(t, s, acc.ID)
	blockedAt := time.Now().Add(-2 * time.Hour)
	blockedF := float64(blockedAt.UnixNano()) / 1e9
	if _, err := s.Update(model.ProviderZai, acc.ID, func(a *model.Account) {
		a.LastRiskControlAt = &blockedF
	}); err != nil {
		t.Fatal(err)
	}

	found, err := s.RecoverAccount(model.ProviderZai, acc.ID)
	if err != nil || !found {
		t.Fatalf("恢复失败: found=%v err=%v", found, err)
	}
	got := s.FindAny(acc.ID)
	if got.Status != model.StatusCooling {
		t.Fatalf("静默期内的恢复应转入保护冷却: status=%s", got.Status)
	}
	if got.CoolingUntil == nil {
		t.Fatal("保护冷却必须有到期时刻")
	}
	wantUntil := float64(blockedAt.Add(24*time.Hour).UnixNano()) / 1e9
	if diff := *got.CoolingUntil - wantUntil; diff > 2 || diff < -2 {
		t.Fatalf("保护冷却到期应等于静默期终点: got=%v want=%v", *got.CoolingUntil, wantUntil)
	}
	if got.RiskControlStreak != 0 {
		t.Fatalf("恢复仍应清零风控连击: %d", got.RiskControlStreak)
	}
	// 静默期证据必须保留（恢复≠解除），且列表能读出成因。
	if got.LastRiskControlAt == nil || *got.LastRiskControlAt != blockedF {
		t.Fatalf("命中时刻不得被清除: %v", got.LastRiskControlAt)
	}
	if got.LastErrorKind == nil || *got.LastErrorKind != model.ErrorKindRiskControl {
		t.Fatalf("保护冷却的成因应可读（risk_control）: %v", got.LastErrorKind)
	}
	if got.LastError == nil || *got.LastError == "" {
		t.Fatalf("保护冷却应带可读说明: %v", got.LastError)
	}
	// 保护冷却期间不可调度（冷却未到期）且闸门也不放行。
	if got.IsSelectable(time.Now()) {
		t.Fatal("保护冷却期间不得可调度")
	}
	if sel := s.Select(model.ProviderZai, nil, ""); sel != nil {
		t.Fatalf("保护冷却期间不应被选中: %s", sel.ID)
	}
}

// 静默期已过（≥窗口）的风控失效账号：恢复照旧直接回 active 并立即参与调度。
func TestRecoverAccountPlainAfterSilentWindow(t *testing.T) {
	oldHours := config.RiskSilentHours
	config.RiskSilentHours = 24
	t.Cleanup(func() { config.RiskSilentHours = oldHours })

	s := newTestStore(t)
	acc, err := s.AddAccount(model.ProviderZai, "risk-old", "risk-old-secret")
	if err != nil {
		t.Fatal(err)
	}
	markRiskControlInvalid(t, s, acc.ID)
	blockedF := float64(time.Now().Add(-25*time.Hour).UnixNano()) / 1e9
	if _, err := s.Update(model.ProviderZai, acc.ID, func(a *model.Account) {
		a.LastRiskControlAt = &blockedF
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.RecoverAccount(model.ProviderZai, acc.ID); err != nil {
		t.Fatal(err)
	}
	got := s.FindAny(acc.ID)
	if got.Status != model.StatusActive || got.CoolingUntil != nil {
		t.Fatalf("静默期已过的恢复不应再保护冷却: status=%s until=%v", got.Status, got.CoolingUntil)
	}
	if sel := s.Select(model.ProviderZai, nil, ""); sel == nil || sel.ID != acc.ID {
		t.Fatal("静默期已过的恢复账号应立即可被选中")
	}
}

// 批量恢复的逐条 message 必须注明保护冷却：16:31 那次事故里，51 个账号「恢复成功、
// 随即全部再次 405」，而响应只写了 ok——管理员无从得知恢复意味着什么。
func TestBatchRecoverProtectiveCoolingMessage(t *testing.T) {
	oldHours := config.RiskSilentHours
	config.RiskSilentHours = 24
	t.Cleanup(func() { config.RiskSilentHours = oldHours })

	s := newTestStore(t)
	recent, err := s.AddAccount(model.ProviderZai, "recent", "recent-secret")
	if err != nil {
		t.Fatal(err)
	}
	oldAcc, err := s.AddAccount(model.ProviderZai, "old", "old-secret")
	if err != nil {
		t.Fatal(err)
	}
	markRiskControlInvalid(t, s, recent.ID)
	markRiskControlInvalid(t, s, oldAcc.ID)
	recentF := float64(time.Now().Add(-3*time.Hour).UnixNano()) / 1e9
	oldF := float64(time.Now().Add(-30*time.Hour).UnixNano()) / 1e9
	if _, err := s.Update(model.ProviderZai, recent.ID, func(a *model.Account) {
		a.LastRiskControlAt = &recentF
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(model.ProviderZai, oldAcc.ID, func(a *model.Account) {
		a.LastRiskControlAt = &oldF
	}); err != nil {
		t.Fatal(err)
	}

	res, err := s.BatchRecoverAccounts([]string{recent.ID, oldAcc.ID}, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Succeeded != 2 || len(res.Items) != 2 {
		t.Fatalf("批量恢复应两项都成功: %+v", res)
	}
	if res.Items[0].Status != BatchStatusOK || res.Items[1].Status != BatchStatusOK {
		t.Fatalf("逐条状态应为 ok: %+v", res.Items)
	}
	if res.Items[0].Message == "" || !strings.Contains(res.Items[0].Message, "保护冷却") {
		t.Fatalf("静默期内条目的 message 应注明保护冷却: %q", res.Items[0].Message)
	}
	if res.Items[1].Message != "" {
		t.Fatalf("静默期已过条目不应附加 protected 提示: %q", res.Items[1].Message)
	}
	if got := s.FindAny(recent.ID); got.Status != model.StatusCooling {
		t.Fatalf("静默期内账号应转入保护冷却: %s", got.Status)
	}
	if got := s.FindAny(oldAcc.ID); got.Status != model.StatusActive {
		t.Fatalf("静默期已过账号应直接回 active: %s", got.Status)
	}
}
