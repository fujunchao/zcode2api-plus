package store

// LoginProxyReplacement 是账号尚未入池时的登录出口替换结果。
type LoginProxyReplacement struct {
	Removed  bool
	Next     *ProxyProfile
	Reassign ProxyReassign
}

// ReplaceFailedLoginProxy 不依赖账号 ID，只淘汰本次请求使用且尚未修改的代理。
// 删除、已有绑定改派和下一登录出口选择在同一把锁内完成；事务失败不发布部分状态。
func (s *Store) ReplaceFailedLoginProxy(id, url string, previousFailures map[string]bool) (LoginProxyReplacement, error) {
	result := LoginProxyReplacement{Reassign: ProxyReassign{Assigned: map[string]string{}}}
	if id == "" || url == "" {
		return result, nil
	}
	excluded := map[string]bool{url: true}
	for failed, skip := range previousFailures {
		if skip {
			excluded[failed] = true
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, profile := range s.listProxyProfilesLocked() {
		if profile.ID == id && profile.URL == url {
			removed, reassign, err := s.removeProxiesAvoidingLocked(map[string]bool{id: true}, excluded)
			if err != nil {
				return result, err
			}
			result.Removed, result.Reassign = len(removed) > 0, reassign
			break
		}
	}
	if next, ok := leastLoadedProxy(proxyProfilesExcluding(s.listProxyProfilesLocked(), excluded), s.proxyOccupancyLocked(), "", ""); ok {
		result.Next = &next
	}
	return result, nil
}
