package store

import "zcode2api/internal/model"

// UpdateForRequest 在同一把锁内核对出站凭据及其持久代次，再应用上游健康结果。
// 旧凭据、删除、停用或归档时不执行回调、不落库，返回 false；管理员操作仍走原 Update。
// expected 必须是实际请求持有的快照，不能在响应到达后重新 SnapshotAccount。
// fn 内不得调用 Store 的其他方法。
func (s *Store) UpdateForRequest(expected *model.Account, fn func(*model.Account)) (bool, error) {
	if expected == nil {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	live := s.findLocked(expected.Provider, expected.ID)
	if !live.AcceptsRequestResult(expected) {
		return false, nil
	}
	fn(live)
	err := s.persistAccountLocked(live)
	logPersistFailure("request_update", expected.Provider+"/"+expected.ID, err)
	return true, err
}
