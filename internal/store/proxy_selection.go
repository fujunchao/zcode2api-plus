package store

import (
	"time"
	"zcode2api/internal/model"
)

func (s *Store) proxyBindingAvailableLocked(a *model.Account) bool {
	if derefStr(a.ProxyURL) == "" {
		return derefStr(a.ProxyID) == ""
	}
	if s.strictProxyBlockedLocked(derefStr(a.ProxyURL)) {
		return false
	}
	if derefStr(a.ProxyID) == "" {
		return true // 裸 URL 的语法错误由真实派发入口明确报告，不悄悄改线。
	}
	for _, p := range s.listProxyProfilesLocked() {
		if p.ID == *a.ProxyID {
			return p.Enabled && strictProxyIdentity(p.URL) == strictProxyIdentity(derefStr(a.ProxyURL))
		}
	}
	return false
}

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
	return s.chooseProxyLocked(proxyProfilesExcluding(s.listProxyProfilesLocked(), excluded), s.proxyOccupancyLocked(), "", "")
}

func proxyProfilesExcluding(profiles []ProxyProfile, excluded map[string]bool) []ProxyProfile {
	if len(excluded) == 0 {
		return profiles
	}
	allowed := make([]ProxyProfile, 0, len(profiles))
	for _, profile := range profiles {
		blocked := excluded[profile.URL]
		for raw, skip := range excluded {
			if skip && strictProxyIdentity(raw) == strictProxyIdentity(profile.URL) {
				blocked = true
				break
			}
		}
		if !blocked {
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

// PurgeEmptyNewAccountProxy 保留旧调用契约，但空额度不再授权共享代理删除。
// 资格与业务线路健康是独立事实；即使迟到的旧调用仍持有两次空观察也不能删线。
func (s *Store) PurgeEmptyNewAccountProxy(expected *model.Account, initial, final model.StartPlanObservation, route ...ProxyEgress) (bool, ProxyReassign, error) {
	return false, ProxyReassign{Assigned: map[string]string{}}, nil
}
