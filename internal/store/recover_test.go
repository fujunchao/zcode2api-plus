// 风控失效「手动恢复」出口的测试：判定边界（仅 invalid+risk_control 可恢复）、
// 字段转移（清冷却/错误证据/风控连击）、持久化、批量逐条语义，以及
// 「启停不是解封后门」契约回归。行为规格见 docs/plan-riskcontrol-manual-recovery-20261012.md。
package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

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
