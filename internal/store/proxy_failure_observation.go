package store

import (
	"context"
	"time"

	"zcode2api/internal/model"
	"zcode2api/internal/proxy"
)

const (
	proxyFailureRecheckLimit   = 1024
	proxyFailureRecheckTTL     = 10 * time.Minute
	proxyFailureRecheckBackoff = 5 * time.Minute
)

// 只保留通知所需的身份摘要；不保存凭据、代理认证或错误正文。
type proxyFailureRecheck struct {
	AccountID, Provider string
	Route               ProxyEgress
	PolicyRevision      uint64
	At, NextAt          time.Time
	Pending             bool
	// InFlight 表示已被巡检取走但结果尚未落库；冷启动门禁靠它堵住取走到落库之间的空窗。
	InFlight   bool
	InFlightAt time.Time
	// Fresh 标记新导入线路的首次检查：这类通知没有绑定账号，只校验配置代次。
	Fresh bool
}

// ObserveProxyEndpointFailure 只提交网络复核通知，绝不删除配置或改变账号绑定。
// 通知按节点合并并限频；真正删除仍须由统一巡检完成两次新鲜网络检查和独立对照。
func (s *Store) ObserveProxyEndpointFailure(ctx context.Context, expected *model.Account, reason string, at time.Time, route ...ProxyEgress) string {
	if ctx.Err() != nil || expected == nil ||
		(reason != proxy.FailureEndpointUnreachable && reason != proxy.FailureClaimSuspicious) {
		return "ignored"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx.Err() != nil || s.strictClosed {
		return "ignored"
	}
	if len(route) == 0 || route[0].ID == "" || route[0].ID != derefStr(expected.ProxyID) || !s.proxyDispatchCurrentLocked(expected, route[0]) {
		return "stale_snapshot"
	}
	if task := s.initializations[expected.ID]; task != nil && task.Managed && task.State != "completed" {
		return "initializing"
	}
	p := s.modelProbe.Policy
	if !p.NetworkEnabled {
		return "observed"
	}
	s.pruneProxyFailureRechecksLocked(at)
	if s.proxyFailureRechecks == nil {
		s.proxyFailureRechecks = map[string]proxyFailureRecheck{}
	}
	id := route[0].ID
	previous, exists := s.proxyFailureRechecks[id]
	if exists && previous.Route.Generation == route[0].Generation && previous.PolicyRevision == p.Revision && !previous.Pending && at.Before(previous.NextAt) {
		return "rate_limited"
	}
	if !exists && len(s.proxyFailureRechecks) >= proxyFailureRecheckLimit {
		// 满载时仅记录新异常，不驱逐已有节点的冷却；否则大池轮换会打散限频。
		return "capacity_limited"
	}
	if exists && at.Before(previous.At) {
		at = previous.At // 并发请求进入锁的顺序不一定等于产生通知时间的顺序。
	}
	s.proxyFailureRechecks[id] = proxyFailureRecheck{AccountID: expected.ID, Provider: expected.Provider, Route: route[0], PolicyRevision: p.Revision, At: at, Pending: true}
	return "recheck_pending"
}

func (s *Store) pruneProxyFailureRechecksLocked(now time.Time) {
	for id, hint := range s.proxyFailureRechecks {
		// 管理器可能刚取完 now 就收到新通知，不能将几微秒后的合法通知当成过期。
		if hint.PolicyRevision != s.modelProbe.Policy.Revision || !now.Before(hint.At.Add(proxyFailureRecheckTTL)) {
			delete(s.proxyFailureRechecks, id)
		}
	}
}

// TakeProxyNetworkRechecks 由统一网络巡检消费；不复用通知作为首轮失败证据。
// 消费时再次检查原请求身份，配置、凭据或绑定已经变化的通知直接丢弃。
func (s *Store) TakeProxyNetworkRechecks(policy ModelProbeSettings, now time.Time) []ProxyProfile {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.strictClosed || policy.Revision != s.modelProbe.Policy.Revision {
		return nil
	}
	s.pruneProxyFailureRechecksLocked(now)
	if !s.modelProbe.Policy.NetworkEnabled {
		s.proxyFailureRechecks = nil
		return nil
	}
	var out []ProxyProfile
	for _, p := range s.listProxyProfilesLocked() {
		hint, ok := s.proxyFailureRechecks[p.ID]
		if !ok || !hint.Pending {
			continue
		}
		if !p.Enabled || p.Generation != hint.Route.Generation {
			delete(s.proxyFailureRechecks, p.ID)
			continue
		}
		// Fresh 通知来自刚导入、可能还没有绑定账号的线路：只校验配置代次。
		// 有账号的通知识仍然要求「派发身份未变」，避免旧快照驱动的复核误伤新配置。
		if hint.AccountID != "" {
			account := s.findLocked(hint.Provider, hint.AccountID)
			if !s.proxyDispatchCurrentLocked(account, hint.Route) {
				delete(s.proxyFailureRechecks, p.ID)
				continue
			}
		}
		hint.Pending, hint.InFlight, hint.InFlightAt = false, true, now
		hint.NextAt = now.Add(proxyFailureRecheckBackoff)
		s.proxyFailureRechecks[p.ID] = hint
		out = append(out, p)
	}
	return out
}

// queueFreshProxyChecksLocked 为新导入线路登记一次冷启动检查；调用方须持有 s.mu。
// 与故障通知共用队列，不新增调度器；没有绑定账号，因此不设置 AccountID。
func (s *Store) queueFreshProxyChecksLocked(profiles []ProxyProfile, now time.Time) {
	if len(profiles) == 0 {
		return
	}
	if s.proxyFailureRechecks == nil {
		s.proxyFailureRechecks = map[string]proxyFailureRecheck{}
	}
	revision := s.modelProbe.Policy.Revision
	for _, p := range profiles {
		if p.ID == "" || !p.Enabled {
			continue
		}
		if _, exists := s.proxyFailureRechecks[p.ID]; exists {
			continue
		}
		if len(s.proxyFailureRechecks) >= proxyFailureRecheckLimit {
			return
		}
		s.proxyFailureRechecks[p.ID] = proxyFailureRecheck{
			Route: ProxyEgress{
				ID: p.ID, Label: p.Name, Mode: "proxy",
				Generation: p.Generation, identity: strictProxyIdentity(p.URL),
			},
			PolicyRevision: revision,
			At:             now,
			Pending:        true,
			Fresh:          true,
		}
	}
}
