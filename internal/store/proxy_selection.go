package store

import (
	"time"
	"zcode2api/internal/model"
)

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
	return s.PickAvailableProxyProfileExcluding(nil)
}

// PickAvailableProxyProfileExcluding 沿用现有负载策略，只额外跳过会话已失败的 URL。
func (s *Store) PickAvailableProxyProfileExcluding(excluded map[string]bool) (ProxyProfile, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return leastLoadedProxy(proxyProfilesExcluding(s.listProxyProfilesLocked(), excluded), s.proxyOccupancyLocked(), "", "")
}

func proxyProfilesExcluding(profiles []ProxyProfile, excluded map[string]bool) []ProxyProfile {
	if len(excluded) == 0 {
		return profiles
	}
	allowed := make([]ProxyProfile, 0, len(profiles))
	for _, profile := range profiles {
		if !excluded[profile.URL] {
			allowed = append(allowed, profile)
		}
	}
	return allowed
}

// RotateAccountProxy 为风控账号换一条线路。expected 必须是本次上游请求使用的快照。
// 若期间已改派或账号已删除则不覆盖；无替代线路时保留原出口。只提交代理字段，
// 不复位风控状态，也不影响原线路绑定的其他账号。
func (s *Store) RotateAccountProxy(expected *model.Account) (ProxyProfile, bool, error) {
	result, err := s.RotateAccountProxyAt(expected, time.Now())
	return result.Next, result.Reason == "changed", err
}

// PurgeEmptyNewAccountProxy 仅供本次真正新建账号的领取收尾调用。
// initial/final 是领取前后的独立查询证据，expected 是领取开始前的账号快照。
// 全部关联账号按空闲优先/最少绑定原子改派；不改领取状态，也不在新线路循环检测。
func (s *Store) PurgeEmptyNewAccountProxy(expected *model.Account, initial, final model.StartPlanObservation) (bool, ProxyReassign, error) {
	empty := ProxyReassign{Assigned: map[string]string{}}
	if !expected.MissingStartPlanEvidence() || initial.ProxyID == "" || !initial.Missing || !final.Missing ||
		!initial.Matches(expected) || !final.Matches(expected) || final.CheckedAt <= initial.CheckedAt ||
		final.CheckedAt <= expected.StartPlanObservation.CheckedAt {
		return false, empty, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	live := s.findLocked(expected.Provider, expected.ID)
	// 本次领取成功允许检测，但历史使用、并发新快照、凭据/出口变更都要保护。
	// final 必须仍是最新观测；不能拿迟到的空回包覆盖后来的有效额度。
	if live == nil || !live.IsAutoClaimTarget(time.Now()) || live.UseCount > 0 ||
		len(live.Plans) > 0 || len(live.Plan) > 0 || live.StartPlanObservation != final || !final.Matches(live) {
		return false, empty, nil
	}
	for _, p := range s.listProxyProfilesLocked() {
		if p.ID == initial.ProxyID && p.URL == initial.ProxyURL && p.Enabled {
			removed, reassign, err := s.removeProxiesLocked(map[string]bool{p.ID: true}, p.URL)
			return len(removed) > 0, reassign, err
		}
	}
	return false, empty, nil
}
