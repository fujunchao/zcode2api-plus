package store

import (
	"testing"
	"time"

	"zcode2api/internal/model"
)

func initializationStoreFixture(t *testing.T) (*Store, *model.Account, ProxyProfile, ProxyProfile) {
	t.Helper()
	s := newTestStore(t)
	trial := addSelectionProxy(t, s, "取额", 18080, true)
	fixed := addSelectionProxy(t, s, "固定", 18081, true)
	policy := DefaultNewAccountProxyPolicy()
	policy.Enabled = true
	policy.DefaultProxyID = fixed.ID
	if _, err := s.SaveNewAccountProxyPolicy(policy); err != nil {
		t.Fatal(err)
	}
	a, _, err := s.AddAccountWithIdentity(model.ProviderZai, "新号", jwtFor("initialization"), "", AccountCreationOptions{InitializeProxy: true, Source: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return s, s.FindAny(a.ID), trial, fixed
}

func completeInitializationFixture(t *testing.T, s *Store, a *model.Account) {
	t.Helper()
	attempt, err := s.BeginInitialization(a.ID, time.Now())
	if err != nil || attempt == nil {
		t.Fatalf("无法开始：%v", err)
	}
	defer s.ReleaseInitialization(attempt)
	value := attempt.Account.Clone()
	value.Quota = map[string]map[string]any{"test-model": {"model": "test-model", "total": float64(150), "used": float64(0), "remaining": float64(150), "available": float64(150)}}
	if err := s.ObserveInitialization(attempt, model.InitialQuotaObservation{Kind: "available", Snapshot: value, CheckedAt: float64(time.Now().Unix())}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.BindInitialization(a.ID, s.Initialization(a.ID).Revision, false); err != nil || !ok {
		t.Fatalf("无法绑定：%v", err)
	}
}

func TestNewAccountInitializationFixedProxyRoutingIntent(t *testing.T) {
	for _, action := range []string{"risk-rotate", "delete", "manual-direct"} {
		t.Run(action, func(t *testing.T) {
			s, a, _, fixed := initializationStoreFixture(t)
			completeInitializationFixture(t, s, a)
			switch action {
			case "risk-rotate":
				if result, err := s.RotateAccountProxyAt(s.FindAny(a.ID), time.Now()); err != nil || result.Reason != "fixed_proxy" {
					t.Fatalf("固定出口不能自动轮换：%+v %v", result, err)
				}
				if *s.FindAny(a.ID).ProxyID != fixed.ID {
					t.Fatal("固定绑定被覆盖")
				}
			case "delete":
				if _, _, err := s.DeleteProxyProfile(fixed.ID); err != nil {
					t.Fatal(err)
				}
				got := s.FindAny(a.ID)
				if got.ProxyID != nil || got.IsSelectable(time.Now()) || got.Initialization == nil || got.Initialization.GrantedAt == 0 {
					t.Fatalf("已到账账号只能等待固定出口，不得改派或重领：%+v", got.Initialization)
				}
			case "manual-direct":
				if _, err := s.AssignProxyProfile(a.ID, ""); err != nil {
					t.Fatal(err)
				}
				got := s.FindAny(a.ID)
				if got.Initialization.Managed || got.InitializationBlocked() || got.ProxyID != nil {
					t.Fatalf("人工直连应原子解除托管：%+v", got.Initialization)
				}
			}
		})
	}
}
