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
		// 丢弃通知只会少做一次探测，不能被解释为成功或补成删除证据。
		oldestID := ""
		var oldest time.Time
		for key, hint := range s.proxyFailureRechecks {
			if oldestID == "" || hint.At.Before(oldest) {
				oldestID, oldest = key, hint.At
			}
		}
		delete(s.proxyFailureRechecks, oldestID)
	}
	s.proxyFailureRechecks[id] = proxyFailureRecheck{AccountID: expected.ID, Provider: expected.Provider, Route: route[0], PolicyRevision: p.Revision, At: at, Pending: true}
	return "recheck_pending"
}

func (s *Store) pruneProxyFailureRechecksLocked(now time.Time) {
	for id, hint := range s.proxyFailureRechecks {
		if hint.PolicyRevision != s.modelProbe.Policy.Revision || hint.At.After(now) || !now.Before(hint.At.Add(proxyFailureRecheckTTL)) {
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
		account := s.findLocked(hint.Provider, hint.AccountID)
		if !p.Enabled || p.Generation != hint.Route.Generation || !s.proxyDispatchCurrentLocked(account, hint.Route) {
			delete(s.proxyFailureRechecks, p.ID)
			continue
		}
		hint.Pending, hint.NextAt = false, now.Add(proxyFailureRecheckBackoff)
		s.proxyFailureRechecks[p.ID] = hint
		out = append(out, p)
	}
	return out
}
