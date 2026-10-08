package store

import (
	"context"
	"zcode2api/internal/model"
)

// commitProxyStateLocked 将线路与关联账号作为一个事务提交，再发布内存状态。
// profiles=nil 表示本次只修改账号指派；pending 都是尚未发布的副本。
func (s *Store) commitProxyStateLocked(profiles []ProxyProfile, pending []*model.Account) error {
	return s.commitProxyStateWithHistoryLocked(profiles, pending, nil)
}

// history 非 nil 时和线路改派一起落库；失败时两者均不发布到内存。
func (s *Store) commitProxyStateWithHistoryLocked(profiles []ProxyProfile, pending []*model.Account, history riskProxyHistory) error {
	if profiles == nil && len(pending) == 0 && history == nil {
		return nil
	}
	var raw []byte
	var err error
	if profiles != nil {
		raw, err = marshalJSON(profiles)
		if err != nil {
			return err
		}
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if history != nil {
		data, err := marshalJSON(history)
		if err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT OR REPLACE INTO meta (key,value) VALUES (?,?)", riskProxyHistoryKey, string(data)); err != nil {
			return err
		}
	}
	if profiles != nil {
		if _, err = tx.Exec("INSERT OR REPLACE INTO meta (key,value) VALUES ('proxy_profiles',?)", string(raw)); err != nil {
			return err
		}
	}
	for _, a := range pending {
		if err = persistAccountOnTx(tx, a); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if history != nil {
		s.riskProxyHistory = history
	}
	if profiles != nil {
		s.settings["proxy_profiles"] = string(raw)
		s.publishSettings()
	}
	for _, a := range pending {
		live := s.findLocked(a.Provider, a.ID)
		live.ProxyID, live.ProxyURL = a.ProxyID, a.ProxyURL
	}
	return nil
}

// removeProxiesLocked 在副本上统一按空闲优先、最少绑定计算改派。
// excludedURL 用于领取风控清理时避开相同出口；所有删除入口共用同一选线策略。
func (s *Store) removeProxiesLocked(ids map[string]bool, excludedURL string) ([]string, ProxyReassign, error) {
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
		p, ok := leastLoadedProxy(remaining, occupancy, "", excludedURL)
		if !ok {
			result.Direct = append(result.Direct, a.ID)
			continue
		}
		id, url := p.ID, p.URL
		a.ProxyID, a.ProxyURL = &id, &url
		occupancy[id]++
		result.Assigned[a.ID] = id
	}
	if err := s.commitProxyStateLocked(remaining, pending); err != nil {
		return nil, ProxyReassign{Assigned: map[string]string{}}, err
	}
	for _, id := range removed {
		s.clearLineTruncateLocked(id)
	}
	return removed, result, nil
}
