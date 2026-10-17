// 账号级 405 的「换过线没有」记录：用来区分线路的锅与账号的锅。
//
// 2026-10-17 日志：405 过的账号换一条线路即可成功（5/5），说明首次 405 不应直接
// 冷却账号。只有当同一账号在**不同线路**上仍然 405，才有理由怀疑账号本身。
//
// 纯内存态：重启丢失最坏只多试一次，不值得为它增加 Account 持久化字段（那要同步
// 五处 JSON 契约）。
package store

import (
	"sort"
	"time"
)

const (
	account405TTL   = 2 * time.Hour
	account405Limit = 8192
)

type account405Mark struct {
	Identity string
	At       time.Time
}

// NoteAccount405 记录一次账号级 405 及其出口线路，返回本次是否应按「线路归因」处置
// （即暂不冷却账号）。
//
// 返回 true 的两种情形：
//   - 该账号在 TTL 内没有 405 记录（首次）；
//   - 上一次 405 也在同一条线路上（同一条脏线重复命中，仍属线路问题）。
//
// 返回 false：TTL 内换过一条线路仍然 405 —— 这时才升级为账号级冷却。
func (s *Store) NoteAccount405(accID, identity string, now time.Time) bool {
	if accID == "" {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.account405 == nil {
		s.account405 = map[string]account405Mark{}
	}
	first := true
	if prev, ok := s.account405[accID]; ok && now.Sub(prev.At) <= account405TTL {
		if prev.Identity != "" && identity != "" && prev.Identity != identity {
			first = false
		}
	}
	s.account405[accID] = account405Mark{Identity: identity, At: now}
	if len(s.account405) > account405Limit {
		s.pruneAccount405Locked(now)
	}
	return first
}

// pruneAccount405Locked 清理过期记录；仍超限时按时间淘汰最旧的。
func (s *Store) pruneAccount405Locked(now time.Time) {
	cut := now.Add(-account405TTL)
	for id, mark := range s.account405 {
		if mark.At.Before(cut) {
			delete(s.account405, id)
		}
	}
	if len(s.account405) <= account405Limit {
		return
	}
	type entry struct {
		id string
		at time.Time
	}
	list := make([]entry, 0, len(s.account405))
	for id, mark := range s.account405 {
		list = append(list, entry{id: id, at: mark.At})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].at.Before(list[j].at) })
	for i := 0; i < len(list)-account405Limit; i++ {
		delete(s.account405, list[i].id)
	}
}
