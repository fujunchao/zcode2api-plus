package store

import (
	"context"
	"zcode2api/internal/model"
)

// commitProxyStateLocked 将线路与关联账号作为一个事务提交，再发布内存状态。
// profiles=nil 表示本次只修改账号指派；pending 都是尚未发布的副本。
func (s *Store) commitProxyStateLocked(profiles []ProxyProfile, pending []*model.Account) error {
	if profiles == nil && len(pending) == 0 {
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

// removeProxiesLocked 在副本上计算改派。手动删除只用空闲线路；熔断允许共享。
func (s *Store) removeProxiesLocked(ids map[string]bool, allowShared bool) ([]string, ProxyReassign, error) {
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
	occupancy := map[string]int{}
	var candidates []ProxyProfile
	for _, p := range remaining {
		if p.Enabled {
			candidates = append(candidates, p)
		}
	}
	var pending []*model.Account
	for _, a := range s.allAccountsLocked() {
		if a.ProxyID != nil && ids[*a.ProxyID] {
			copy := a.Clone()
			copy.ProxyID, copy.ProxyURL = nil, nil
			pending = append(pending, copy)
		} else if a.ProxyID != nil {
			occupancy[*a.ProxyID]++
		}
	}
	for _, a := range pending {
		best := -1
		for i, p := range candidates {
			if !allowShared && occupancy[p.ID] > 0 {
				continue
			}
			if best < 0 || occupancy[p.ID] < occupancy[candidates[best].ID] {
				best = i
			}
		}
		if best < 0 {
			result.Direct = append(result.Direct, a.ID)
			continue
		}
		p := candidates[best]
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
