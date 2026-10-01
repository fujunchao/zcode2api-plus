package store

import "zcode2api/internal/model"

// proxyOccupancyLocked 统计所有账号的线路绑定，包括停用和归档账号。
// 手工 ProxyURL 不占用命名线路，与后台绑定数口径一致。
func (s *Store) proxyOccupancyLocked() map[string]int {
	occupancy := map[string]int{}
	for _, a := range s.allAccountsLocked() {
		if id := derefStr(a.ProxyID); id != "" {
			occupancy[id]++
		}
	}
	return occupancy
}

// leastLoadedProxy 选启用且绑定最少的线路：零绑定自然优先，同负载按列表顺序。
// 风控换线同时排除旧 ID 和旧 URL，避免不同配置仍指向同一代理地址。
func leastLoadedProxy(profiles []ProxyProfile, occupancy map[string]int, excludeID, excludeURL string) (ProxyProfile, bool) {
	var best ProxyProfile
	found := false
	for _, p := range profiles {
		if !p.Enabled || p.ID == excludeID || (excludeURL != "" && p.URL == excludeURL) {
			continue
		}
		if !found || occupancy[p.ID] < occupancy[best.ID] {
			best, found = p, true
		}
	}
	return best, found
}

// PickAvailableProxyProfile 为登录预选出口：空闲优先，否则共享绑定最少的线路。
// 不预占、不写账号；登录会话在建号前就需要固定兑换、刷新等出站请求的出口。
func (s *Store) PickAvailableProxyProfile() (ProxyProfile, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return leastLoadedProxy(s.listProxyProfilesLocked(), s.proxyOccupancyLocked(), "", "")
}

// RotateAccountProxy 为风控账号换一条线路。expected 必须是本次上游请求使用的快照。
// 若期间已改派或账号已删除则不覆盖；无替代线路时保留原出口。只提交代理字段，
// 不复位风控状态，也不影响原线路绑定的其他账号。
func (s *Store) RotateAccountProxy(expected *model.Account) (ProxyProfile, bool, error) {
	if expected == nil {
		return ProxyProfile{}, false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	live := s.findLocked(expected.Provider, expected.ID)
	if live == nil || derefStr(live.ProxyID) != derefStr(expected.ProxyID) ||
		derefStr(live.ProxyURL) != derefStr(expected.ProxyURL) {
		return ProxyProfile{}, false, nil
	}
	p, ok := leastLoadedProxy(s.listProxyProfilesLocked(), s.proxyOccupancyLocked(),
		derefStr(live.ProxyID), derefStr(live.ProxyURL))
	if !ok {
		return ProxyProfile{}, false, nil
	}
	pending := live.Clone()
	pending.ProxyID, pending.ProxyURL = &p.ID, &p.URL
	if err := s.commitProxyStateLocked(nil, []*model.Account{pending}); err != nil {
		return ProxyProfile{}, false, err
	}
	return p, true, nil
}
