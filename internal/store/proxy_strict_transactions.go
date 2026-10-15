package store

import (
	"context"
	"encoding/json"
	"fmt"
	"zcode2api/internal/model"
	"zcode2api/internal/proxy"
)

// 编辑入口把人工意图与账号补丁同事务保存，避免单独清除 pending 产生竞态。
func (s *Store) editAccountProxyIntent(provider, id string, edit AccountEdit, apply func(*model.Account)) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	live := s.findLocked(provider, id)
	if live == nil {
		return false, nil
	}
	if s.initializationManagedLocked(live.ID) {
		return s.updateManagedAccountLocked(live, apply, edit.SetProxyURL)
	}
	a := live.Clone()
	apply(a)
	if edit.SetProxyURL && derefStr(a.ProxyURL) != "" && s.strictProxyBlockedLocked(derefStr(a.ProxyURL)) {
		return false, ErrProxyRetired
	}
	next := s.cloneStrictProxyLocked()
	trackStrictAccountVersions(&next, a)
	if edit.SetProxyURL {
		intent := "manual_proxy"
		if derefStr(a.ProxyURL) == "" {
			intent = "explicit_direct"
		}
		setStrictIntent(&next, a, intent)
	}
	if edit.SetSecret {
		delete(next.Pending, a.ID)
		delete(next.PendingAvoid, a.ID)
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return false, err
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := persistAccountOnTx(tx, a); err != nil {
		return false, err
	}
	if _, err := tx.Exec("INSERT OR REPLACE INTO meta (key,value) VALUES (?,?)", strictProxyStateKey, string(raw)); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	apply(live)
	s.strictProxy = next
	live.CredentialGeneration = next.Credentials[live.ID].Generation
	return true, nil
}

// rebindStrictPendingLocked 在新增/启用线路事务内补绑；显式直连及旧空绑定不在等待集合。
func (s *Store) rebindStrictPendingLocked(profiles []ProxyProfile, pending []*model.Account, next *strictProxyState) []*model.Account {
	byID := map[string]*model.Account{}
	for _, a := range pending {
		byID[a.ID] = a
	}
	occupancy := s.proxyOccupancyLocked()
	for _, a := range pending {
		if live := s.findLocked(a.Provider, a.ID); live != nil {
			occupancy[derefStr(live.ProxyID)]--
		}
		occupancy[derefStr(a.ProxyID)]++
	}
	for _, live := range s.allAccountsLocked() {
		if s.initializationManagedLocked(live.ID) || s.isModelProbeAccountLocked(live.ID) {
			delete(next.Pending, live.ID)
			delete(next.PendingAvoid, live.ID)
			continue
		}
		credential, waiting := next.Pending[live.ID]
		if !waiting {
			continue
		}
		a := byID[live.ID]
		if a == nil {
			a = live.Clone()
		}
		if credential != strictCredential(a) || next.Intents[a.ID] != "auto_pending_proxy" || derefStr(a.ProxyID) != "" || derefStr(a.ProxyURL) != "" {
			delete(next.Pending, a.ID)
			delete(next.PendingAvoid, a.ID)
			continue
		}
		allowed := make([]ProxyProfile, 0, len(profiles))
		for _, p := range profiles {
			blocked := false
			for _, identity := range next.PendingAvoid[a.ID] {
				if strictProxyIdentity(p.URL) == identity {
					blocked = true
					break
				}
			}
			if !blocked {
				allowed = append(allowed, p)
			}
		}
		p, ok := s.chooseProxyLocked(allowed, occupancy, "", "")
		if !ok {
			continue
		}
		a.ProxyID, a.ProxyURL = &p.ID, &p.URL
		if byID[a.ID] == nil {
			pending = append(pending, a)
			byID[a.ID] = a
		}
		occupancy[p.ID]++
		delete(next.Pending, a.ID)
		delete(next.PendingAvoid, a.ID)
		next.Intents[a.ID] = "auto_proxy"
	}
	return pending
}

func setStrictIntent(next *strictProxyState, a *model.Account, intent string) {
	next.Intents[a.ID] = intent
	delete(next.Pending, a.ID)
	delete(next.PendingAvoid, a.ID)
	if intent == "auto_pending_proxy" {
		next.Pending[a.ID] = strictCredential(a)
	}
}

func (s *Store) commitProxyIntentLocked(pending []*model.Account, intent string) error {
	next := s.cloneStrictProxyLocked()
	for _, a := range pending {
		setStrictIntent(&next, a, intent)
	}
	return s.commitProxyBundleStrictLocked(context.Background(), nil, pending, nil, nil, &next)
}

// retireStrictIdentityLocked 将所有同路由别名、裸 URL 绑定与来源墓碑原子提交。
// retiring 意图已先落恢复日志，失败时原绑定/配置不发布，但新派发仍被拒绝。
func (s *Store) retireStrictIdentityLocked(identity string) (ProxyDecision, error) {
	b := s.strictProxy.Blocks[identity]
	result := ProxyDecision{Reason: b.Reason, DecisionID: b.DecisionID, RuleVersion: b.RuleVersion}
	if b.State == "removed" {
		result.Reason = "already_retired"
		return result, nil
	}
	remaining := []ProxyProfile{}
	for _, p := range s.listProxyProfilesLocked() {
		if strictProxyIdentity(p.URL) == identity {
			result.Removed = append(result.Removed, p.ID)
		} else {
			remaining = append(remaining, p)
		}
	}
	next := s.cloneStrictProxyLocked()
	var pending []*model.Account
	var manualEndpoints []string
	occupancy := s.proxyOccupancyLocked()
	for _, live := range s.allAccountsLocked() {
		if strictProxyIdentity(derefStr(live.ProxyURL)) != identity {
			continue
		}
		// 裸 URL 是人工选定路由：保留原值并由派发准入明确拒绝，不悄悄换线。
		if derefStr(live.ProxyID) == "" {
			manualEndpoints = append(manualEndpoints, derefStr(live.ProxyURL))
			continue
		}
		a := live.Clone()
		a.ProxyID, a.ProxyURL = nil, nil
		if s.initializationManagedLocked(a.ID) {
			intent := initializationIntent
			if s.initializations[a.ID].GrantedAt > 0 {
				intent = fixedProxyIntent
			}
			setStrictIntent(&next, a, intent)
			result.Pending++
			pending = append(pending, a)
			continue
		}
		if p, ok := s.chooseProxyLocked(remaining, occupancy, "", ""); ok {
			a.ProxyID, a.ProxyURL = &p.ID, &p.URL
			occupancy[p.ID]++
			setStrictIntent(&next, a, "auto_proxy")
			result.Assigned++
		} else {
			setStrictIntent(&next, a, "auto_pending_proxy")
			result.Direct++
			result.Pending++
		}
		pending = append(pending, a)
	}
	b.State = "removed"
	next.Blocks[identity] = b
	next.Audit = append(next.Audit, fmt.Sprintf("retire:%s:rule=%s:reason=%s:removed=%d:assigned=%d:direct=%d:pending=%d", b.DecisionID, b.RuleVersion, b.Reason,
		len(result.Removed), result.Assigned, result.Direct, result.Pending))
	if len(next.Audit) > 256 {
		next.Audit = next.Audit[len(next.Audit)-256:]
	}
	sub, err := s.subscriptionMutationLocked(remaining, "strict_3012")
	if err == nil {
		err = s.commitProxyBundleStrictLocked(context.Background(), remaining, pending, nil, sub, &next)
	}
	if err != nil {
		return ProxyDecision{Reason: "persist_failed", DecisionID: b.DecisionID, RuleVersion: b.RuleVersion}, err
	}
	for _, id := range result.Removed {
		s.clearLineTruncateLocked(id)
	}
	for _, endpoint := range manualEndpoints {
		proxy.ForgetTransport(endpoint)
	}
	// SQL 已是权威 removed 状态；旧日志重启合并时不得覆盖它。
	if err := s.writeStrictJournalLocked(s.strictProxy.Blocks); err != nil {
		return result, err
	}
	return result, nil
}
