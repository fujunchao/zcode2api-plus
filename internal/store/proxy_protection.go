package store

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
func (s *Store) ProxyAutoDeleteProtections() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	byIdentity := s.proxyAutoDeleteProtectionsLocked()
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
