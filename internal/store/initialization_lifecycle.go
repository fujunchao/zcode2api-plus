package store

import (
	"time"
	"zcode2api/internal/model"
)

// initializationProxyMutationsLocked 将通用代理事务中的人工接管和失效一起落盘。
func (s *Store) initializationProxyMutationsLocked(profiles []ProxyProfile, pending []*model.Account, next *strictProxyState) map[string]*InitializationTask {
	if profiles == nil {
		profiles = s.listProxyProfilesLocked()
	}
	accounts := map[string]*model.Account{}
	for _, a := range pending {
		accounts[a.ID] = a
	}
	updates := map[string]*InitializationTask{}
	for id, old := range s.initializations {
		if !old.Managed {
			continue
		}
		a := accounts[id]
		if a == nil {
			a = s.findAnyLocked(id)
		}
		if a == nil {
			continue
		}
		t := old.Clone()
		changed := false
		intent := next.Intents[id]
		if intent == "manual_proxy" || intent == "explicit_direct" {
			t.Managed = false
			t.State = "cancelled"
			t.Reason = "manual_override"
			changed = true
		} else if t.GrantedAt > 0 {
			available := false
			for _, p := range profiles {
				if p.ID == t.DefaultProxyID && p.Enabled && derefStr(a.ProxyID) == p.ID && strictProxyIdentity(derefStr(a.ProxyURL)) == strictProxyIdentity(p.URL) && !s.strictProxyBlockedLocked(p.URL) {
					available = true
					break
				}
			}
			if !available && (t.State != "awaiting_default" || t.Reason != "default_unavailable") {
				t.State = "awaiting_default"
				t.Reason = "default_unavailable"
				changed = true
			}
			if available && t.State == "awaiting_default" && !t.Paused {
				t.State = "completed"
				t.Reason = "ready"
				changed = true
			}
		} else if t.CurrentProxyID != "" && (next.RouteVersions[id].Generation != t.RouteGeneration || next.Generations[t.CurrentProxyID] != t.CurrentProxyGeneration || derefStr(a.ProxyID) != t.CurrentProxyID) {
			if t.Uncertain {
				t.State = "waiting"
				t.Reason = "route_changed"
			} else {
				t.State = "queued"
				t.Reason = "route_changed"
			}
			changed = true
		}
		if changed {
			t.RunID = ""
			t.Revision++
			updates[id] = t
		}
	}
	return updates
}

func (s *Store) updateManagedAccountLocked(live *model.Account, apply func(*model.Account), manualProxy bool) (bool, error) {
	a := live.Clone()
	apply(a)
	next := s.cloneStrictProxyLocked()
	trackStrictAccountVersions(&next, a)
	t := s.initializations[a.ID].Clone()
	if manualProxy {
		if derefStr(a.ProxyURL) != "" && s.strictProxyBlockedLocked(derefStr(a.ProxyURL)) {
			return false, ErrProxyRetired
		}
		intent := "manual_proxy"
		if derefStr(a.ProxyURL) == "" {
			intent = "explicit_direct"
		}
		setStrictIntent(&next, a, intent)
		t.Managed = false
		t.State = "cancelled"
		t.Reason = "manual_override"
	} else if next.Credentials[a.ID].Generation != t.CredentialGeneration {
		if t.GrantedAt > 0 && model.JWTUserID(live.Secret()) != "" && model.JWTUserID(live.Secret()) == model.JWTUserID(a.Secret()) {
			t.CredentialGeneration = next.Credentials[a.ID].Generation
		} else {
			t.State = "waiting"
			t.Reason = "credentials_changed"
			t.Paused = true
		}
	} else if !a.Enabled || a.ArchivedAt != nil {
		t.Paused = true
		if t.GrantedAt == 0 {
			t.State = "waiting"
			t.Reason = "account_disabled"
		}
	}
	t.Revision++
	t.RunID = ""
	return true, s.commitInitializationLocked(t, a, next)
}

// AcquireModelDispatch 是模型专用最终准入，不能用通用非模型客户端绕过初始化。
func (s *Store) AcquireModelDispatch(acc *model.Account, egress ProxyEgress) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if acc != nil && s.initializationManagedLocked(acc.ID) {
		live := s.snapshotLocked(s.findLocked(acc.Provider, acc.ID))
		if live == nil || live.InitializationBlocked() || !live.IsSelectable(time.Now()) {
			return nil, ErrInitializationUnavailable
		}
		if live.UsesFixedProxy() && (egress.Mode == "direct" || egress.Mode == "forced_direct" || egress.Mode == "direct_fallback") {
			return nil, ErrInitializationUnavailable
		}
	}
	return s.acquireProxyDispatchLocked(acc, egress)
}
