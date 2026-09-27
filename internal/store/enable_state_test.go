package store

import (
	"testing"
	"time"

	"zcode2api/internal/model"
)

// 停用 → 启用不得成为「解除封禁」的后门：风控/凭据失效必须保持 invalid，
// 未到期的冷却在启用后仍是 cooling；只有真正的 disabled 才恢复调度。
// 单账号 SetEnabled、批量 BatchSetEnabled 与归档共用同一转移，逐一验证往返。
func TestEnableRoundTripKeepsInvalidAndCooling(t *testing.T) {
	future := float64(time.Now().Add(time.Hour).UnixNano()) / 1e9
	past := float64(time.Now().Add(-time.Hour).UnixNano()) / 1e9
	riskKind := model.ErrorKindRiskControl
	cases := []struct {
		name        string
		prepare     func(a *model.Account)
		wantOff     string // 停用后的状态
		wantOn      string // 再启用后的状态
		wantCooling *float64
	}{
		{"active", func(a *model.Account) {}, model.StatusDisabled, model.StatusActive, nil},
		{"风控失效", func(a *model.Account) {
			a.Status = model.StatusInvalid
			a.LastErrorKind = &riskKind
		}, model.StatusInvalid, model.StatusInvalid, nil},
		{"未到期冷却", func(a *model.Account) {
			a.Status = model.StatusCooling
			a.CoolingUntil = &future
		}, model.StatusDisabled, model.StatusCooling, &future},
		{"已到期冷却", func(a *model.Account) {
			a.Status = model.StatusCooling
			a.CoolingUntil = &past
		}, model.StatusDisabled, model.StatusActive, &past},
	}
	toggles := map[string]func(s *Store, id string, enabled bool) error{
		"single": func(s *Store, id string, enabled bool) error {
			_, err := s.SetEnabled(model.ProviderZai, id, enabled)
			return err
		},
		"batch": func(s *Store, id string, enabled bool) error {
			_, err := s.BatchSetEnabled([]string{id}, enabled, false)
			return err
		},
		"archive": func(s *Store, id string, enabled bool) error {
			// 归档是另一个停用入口：归档再取消归档、启用，也不得复活失效账号。
			if !enabled {
				_, err := s.SetArchived(model.ProviderZai, id, true)
				return err
			}
			if _, err := s.SetArchived(model.ProviderZai, id, false); err != nil {
				return err
			}
			_, err := s.SetEnabled(model.ProviderZai, id, true)
			return err
		},
	}
	for toggleName, toggle := range toggles {
		for _, tc := range cases {
			t.Run(toggleName+"/"+tc.name, func(t *testing.T) {
				s := newTestStore(t)
				acc, err := s.AddAccount(model.ProviderZai, "acc", "header.payload.sig")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.Update(model.ProviderZai, acc.ID, tc.prepare); err != nil {
					t.Fatal(err)
				}
				if err := toggle(s, acc.ID, false); err != nil {
					t.Fatal(err)
				}
				got := s.FindAny(acc.ID)
				if got.Enabled || got.Status != tc.wantOff || got.IsSelectable(time.Now()) {
					t.Fatalf("停用后应为 %s 且不可调度：enabled=%v status=%s", tc.wantOff, got.Enabled, got.Status)
				}
				if err := toggle(s, acc.ID, true); err != nil {
					t.Fatal(err)
				}
				got = s.FindAny(acc.ID)
				if !got.Enabled || got.Status != tc.wantOn {
					t.Fatalf("启用后应为 %s：enabled=%v status=%s", tc.wantOn, got.Enabled, got.Status)
				}
				if (tc.wantCooling == nil) != (got.CoolingUntil == nil) ||
					(tc.wantCooling != nil && *got.CoolingUntil != *tc.wantCooling) {
					t.Fatalf("冷却截止时间不得被启停改写：got=%v want=%v", got.CoolingUntil, tc.wantCooling)
				}
				if tc.wantOn != model.StatusActive && got.IsSelectable(time.Now()) {
					t.Fatalf("%s 账号启用后仍不得被调度", tc.wantOn)
				}
			})
		}
	}
}
