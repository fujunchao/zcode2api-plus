package store

import (
	"context"
	"zcode2api/internal/model"
	"zcode2api/internal/proxy"
	"zcode2api/internal/proxysub"
)

// commitProxyStateLocked 将线路与关联账号作为一个事务提交，再发布内存状态。
// profiles=nil 表示本次只修改账号指派；pending 都是尚未发布的副本。
func (s *Store) commitProxyStateLocked(profiles []ProxyProfile, pending []*model.Account) error {
	return s.commitProxyStateWithHistoryLocked(profiles, pending, nil)
}

// history 非 nil 时和线路改派一起落库；失败时两者均不发布到内存。
func (s *Store) commitProxyStateWithHistoryLocked(profiles []ProxyProfile, pending []*model.Account, history riskProxyHistory, reasons ...string) error {
	var sub *proxysub.State
	var err error
	if profiles != nil {
		reason := "automatic"
		if len(reasons) > 0 {
			reason = reasons[0]
		}
		sub, err = s.subscriptionMutationLocked(profiles, reason)
		if err != nil {
			return err
		}
	}
	return s.commitProxyBundleLocked(context.Background(), profiles, pending, history, sub)
}

// 已退役且不再被任何线路/账号引用的端点才释放缓存；已有流不会被关闭。
func (s *Store) retireProxyTransportsLocked(oldProfiles, profiles []ProxyProfile) {
	used := map[string]bool{}
	for _, p := range profiles {
		used[p.URL] = true
	}
	for _, a := range s.allAccountsLocked() {
		if a.ProxyURL != nil {
			used[*a.ProxyURL] = true
		}
	}
	for _, p := range oldProfiles {
		if !used[p.URL] {
			proxy.ForgetTransport(p.URL)
		}
	}
}

// removeProxiesLocked 在副本上统一按空闲优先、最少绑定计算改派。
// excludedURL 用于空额度/端点故障清理时避开相同出口；所有删除入口共用同一选线策略。
func (s *Store) removeProxiesLocked(ids map[string]bool, excludedURL string, reasons ...string) ([]string, ProxyReassign, error) {
	excluded := map[string]bool{}
	if excludedURL != "" {
		excluded[excludedURL] = true
	}
	return s.removeProxiesAvoidingLocked(ids, excluded, reasons...)
}

func (s *Store) removeProxiesAvoidingLocked(ids map[string]bool, excluded map[string]bool, reasons ...string) ([]string, ProxyReassign, error) {
	result := ProxyReassign{Assigned: map[string]string{}}
	remaining := []ProxyProfile{}
	var removed []string
	for _, p := range s.listProxyProfilesLocked() {
		if ids[p.ID] {
			removed = append(removed, p.ID)
		} else {
			remaining = append(remaining, p)
		}
	}
	if len(removed) == 0 {
		return nil, result, nil
	}
	assignable := proxyProfilesExcluding(remaining, excluded)
	occupancy := s.proxyOccupancyLocked()
	var pending []*model.Account
	for _, a := range s.allAccountsLocked() {
		if a.ProxyID != nil && ids[*a.ProxyID] {
			copy := a.Clone()
			copy.ProxyID, copy.ProxyURL = nil, nil
			pending = append(pending, copy)
		}
	}
	for _, a := range pending {
		p, ok := leastLoadedProxy(assignable, occupancy, "", "")
		if !ok {
			result.Direct = append(result.Direct, a.ID)
			continue
		}
		id, url := p.ID, p.URL
		a.ProxyID, a.ProxyURL = &id, &url
		occupancy[id]++
		result.Assigned[a.ID] = id
	}
	if err := s.commitProxyStateWithHistoryLocked(remaining, pending, nil, reasons...); err != nil {
		return nil, ProxyReassign{Assigned: map[string]string{}}, err
	}
	for _, id := range removed {
		s.clearLineTruncateLocked(id)
	}
	return removed, result, nil
}
