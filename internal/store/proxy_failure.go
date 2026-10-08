package store

import "zcode2api/internal/model"

// PurgeFailedAccountProxy 处理已经确认的代理端点故障，与新号空额度规则无关。
// expected 必须是失败请求实际使用的不可变快照；旧凭据/旧绑定的迟到失败不能误删新线路。
func (s *Store) PurgeFailedAccountProxy(expected *model.Account) (bool, ProxyReassign, error) {
	empty := ProxyReassign{Assigned: map[string]string{}}
	if expected == nil || derefStr(expected.ProxyID) == "" || derefStr(expected.ProxyURL) == "" {
		return false, empty, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
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
