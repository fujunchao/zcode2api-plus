package store

import (
	"fmt"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/web"
)

// recentSuccessProtectionsLocked 返回「最近窗口内有过成功交付」的线路身份 → 最后成功时间。
//
// 数据源是账号的 LastUsedAt（MarkSuccess 在上游成功交付时写，含 async）：它是**事实**，
// 不是状态推断，不需要额外维护一张表。只用于巡检删除的免删判断——严格退役不看它，
// 否则脏线只要还有账号在上面成功过就永远有 24 小时免死金牌（见计划 3.4）。
func (s *Store) recentSuccessProtectionsLocked(now time.Time) map[string]time.Time {
	out := map[string]time.Time{}
	window := time.Duration(config.ProxyProtectRecentHours) * time.Hour
	if window <= 0 {
		return out
	}
	for _, a := range s.allAccountsLocked() {
		identity := strictProxyIdentity(derefStr(a.ProxyURL))
		if identity == "" || a.LastUsedAt == nil {
			continue
		}
		at := time.Unix(int64(*a.LastUsedAt), 0)
		if now.Sub(at) > window {
			continue
		}
		if prev, ok := out[identity]; !ok || at.After(prev) {
			out[identity] = at
		}
	}
	return out
}

// proxyDeleteProtectionReasonsLocked = 基础保护（手工/默认/托管）+ 最近成功。
// 只给「删除」与后台展示用；严格退役走 automaticProxyRemovalReasonLocked，仅看基础保护。
func (s *Store) proxyDeleteProtectionReasonsLocked(now time.Time) map[string]string {
	out := s.proxyAutoDeleteProtectionsLocked()
	for identity := range s.recentSuccessProtectionsLocked(now) {
		if out[identity] == "" {
			out[identity] = "recent_success"
		}
	}
	return out
}

// logRecentProtectLocked 记录一次「因为最近成功而免删」；同一线路每小时最多一条。
// 调用方持有 s.mu，故必须异步输出——web.Warn 是同步 stdout 写，阻塞时会卡住整个 Store 锁。
func (s *Store) logRecentProtectLocked(id string, at, now time.Time) {
	if s.proxyProtectLogged == nil {
		s.proxyProtectLogged = map[string]int64{}
	}
	if now.UnixMilli()-s.proxyProtectLogged[id] < time.Hour.Milliseconds() {
		return
	}
	s.proxyProtectLogged[id] = now.UnixMilli()
	go web.Warn("proxy-protect", fmt.Sprintf("id=%s reason=recent_success last_ok=%.1fh", id, now.Sub(at).Hours()))
}

// proxyAutoDeleteProtectionsLocked 按规范化路由归并保护，避免同一路由别名绕过。
// 默认策略和托管任务在决策锁内读取，不依赖前端是否提交了保护字段。
func (s *Store) proxyAutoDeleteProtectionsLocked() map[string]string {
	profiles := s.listProxyProfilesLocked()
	protected := map[string]string{}
	byID := map[string]string{}
	for _, p := range profiles {
		identity := strictProxyIdentity(p.URL)
		byID[p.ID] = identity
		if identity != "" && p.AutoDeleteProtected {
			protected[identity] = "manual"
		}
	}
	policy, err := s.initializationPolicyLocked()
	if err != nil {
		// 无法读取默认出口策略时保守保留配置，不能把损坏策略当成“不需要保护”。
		for _, identity := range byID {
			protected[identity] = "policy_unavailable"
		}
		return protected
	}
	if identity := byID[policy.DefaultProxyID]; identity != "" {
		protected[identity] = "default_proxy"
	}
	for _, task := range s.initializations {
		if !task.Managed {
			continue
		}
		for _, identity := range []string{byID[task.DefaultProxyID], task.DefaultIdentity} {
			if identity != "" && protected[identity] != "default_proxy" {
				protected[identity] = "managed_default"
			}
		}
	}
	return protected
}

// ProxyAutoDeleteProtections 返回有效保护原因，供管理端解释；不泄露代理认证或路由摘要。
// 含「最近成功」一项：巡检会因它跳过删除，后台需要能解释为什么这条线还在。
func (s *Store) ProxyAutoDeleteProtections() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	byIdentity := s.proxyDeleteProtectionReasonsLocked(time.Now())
	out := map[string]string{}
	for _, p := range s.listProxyProfilesLocked() {
		if reason := byIdentity[strictProxyIdentity(p.URL)]; reason != "" {
			out[p.ID] = reason
		}
	}
	return out
}

// 新的故障处置决定共用此门禁；已经持久化的退役事务仍按原恢复契约完成。
func (s *Store) automaticProxyRemovalReasonLocked(identity string) string {
	if s.modelProbe.Policy.Mode != "auto_retire" {
		return "observe_only"
	}
	if s.proxyAutoDeleteProtectionsLocked()[identity] != "" {
		return "protected_proxy"
	}
	return ""
}
