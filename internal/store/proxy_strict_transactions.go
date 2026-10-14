package store

import (
	"context"
	"zcode2api/internal/model"
)

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
			continue
		}
		p, ok := s.chooseProxyLocked(profiles, occupancy, "", "")
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
		next.Intents[a.ID] = "auto_proxy"
	}
	return pending
}

func setStrictIntent(next *strictProxyState, a *model.Account, intent string) {
	next.Intents[a.ID] = intent
	delete(next.Pending, a.ID)
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
	result := ProxyDecision{Reason: b.Reason, DecisionID: b.DecisionID, RuleVersion: StrictProxyRuleVersion}
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
	occupancy := s.proxyOccupancyLocked()
	for _, live := range s.allAccountsLocked() {
		if strictProxyIdentity(derefStr(live.ProxyURL)) != identity {
			continue
		}
		a := live.Clone()
		a.ProxyID, a.ProxyURL = nil, nil
		if p, ok := s.chooseProxyLocked(remaining, occupancy, "", ""); ok {
			a.ProxyID, a.ProxyURL = &p.ID, &p.URL
			occupancy[p.ID]++
			setStrictIntent(&next, a, "auto_proxy")
			result.Assigned++
		} else {
			setStrictIntent(&next, a, "auto_pending_proxy")
			result.Direct++
		}
		pending = append(pending, a)
	}
	b.State = "removed"
	next.Blocks[identity] = b
	sub, err := s.subscriptionMutationLocked(remaining, "strict_3012")
	if err == nil {
		err = s.commitProxyBundleStrictLocked(context.Background(), remaining, pending, nil, sub, &next)
	}
	if err != nil {
		return ProxyDecision{Reason: "persist_failed", DecisionID: b.DecisionID, RuleVersion: StrictProxyRuleVersion}, err
	}
	for _, id := range result.Removed {
		s.clearLineTruncateLocked(id)
	}
	// SQL 已是权威 removed 状态；旧日志重启合并时不得覆盖它。
	if err := s.writeStrictJournalLocked(s.strictProxy.Blocks); err != nil {
		return result, err
	}
	return result, nil
}
