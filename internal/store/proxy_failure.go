package store

import "zcode2api/internal/model"

// PurgeFailedAccountProxy 供已达到阈值的断流等故障处置使用，仍受统一模式和节点保护约束。
// 计费单次失败不得调用本方法，只能报告 ObserveProxyEndpointFailure。
// expected 必须是失败请求实际使用的不可变快照；旧凭据/旧绑定的迟到失败不能误删新线路。
func (s *Store) PurgeFailedAccountProxy(expected *model.Account, route ...ProxyEgress) (bool, ProxyReassign, error) {
	empty := ProxyReassign{Assigned: map[string]string{}}
	if expected == nil || derefStr(expected.ProxyID) == "" || derefStr(expected.ProxyURL) == "" {
		return false, empty, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(route) > 0 && !s.proxyDispatchCurrentLocked(expected, route[0]) {
		return false, empty, nil
	}
	live := s.findLocked(expected.Provider, expected.ID)
	if !model.NewStartPlanObservation(expected, 1, false).Matches(live) {
		return false, empty, nil
	}
	for _, profile := range s.listProxyProfilesLocked() {
		if profile.ID == *expected.ProxyID && profile.URL == *expected.ProxyURL {
			removed, reassign, err := s.removeProxiesLocked(map[string]bool{profile.ID: true}, profile.URL)
			return len(removed) > 0, reassign, err
		}
	}
	return false, empty, nil
}
