package store

import (
	"context"

	"zcode2api/internal/model"
)

// LoginProxyReplacement 是账号尚未入池时的登录出口替换结果。
type LoginProxyReplacement struct {
	Removed  bool
	Next     *ProxyProfile
	Reassign ProxyReassign
}

// ReplaceFailedLoginProxy 不依赖账号 ID，只淘汰本次请求使用且尚未修改的代理。
// 删除、已有绑定改派和下一登录出口选择在同一把锁内完成；事务失败不发布部分状态。
func (s *Store) ReplaceFailedLoginProxy(id, url string, previousFailures map[string]bool, expected ...ProxyProfile) (LoginProxyReplacement, error) {
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
			if len(expected) > 0 && (expected[0].ID != id || expected[0].Generation != profile.Generation || strictProxyIdentity(expected[0].URL) != strictProxyIdentity(profile.URL)) {
				break
			}
			removed, reassign, err := s.removeProxiesAvoidingLocked(map[string]bool{id: true}, excluded)
			if err != nil {
				return result, err
			}
			result.Removed, result.Reassign = len(removed) > 0, reassign
			break
		}
	}
	if next, ok := s.chooseProxyLocked(proxyProfilesExcluding(s.listProxyProfilesLocked(), excluded), s.proxyOccupancyLocked(), "", ""); ok {
		result.Next = &next
	}
	return result, nil
}

// AssignAutomaticLoginProxy 保留登录实际出口，不在登录收尾再挑一条不同线路。
// nil 表示本次确实自动直连，只建立可持久恢复的等待意图；人工直连不走这个入口。
func (s *Store) AssignAutomaticLoginProxy(expected *model.Account, profile *ProxyProfile) (bool, error) {
	if expected == nil {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	live := s.findLocked(expected.Provider, expected.ID)
	if !live.SameCredential(expected) || derefStr(live.ProxyID) != derefStr(expected.ProxyID) ||
		strictProxyIdentity(derefStr(live.ProxyURL)) != strictProxyIdentity(derefStr(expected.ProxyURL)) {
		return false, nil
	}
	if s.strictProxy.Intents[live.ID] == "explicit_direct" || derefStr(live.ProxyURL) != "" || derefStr(live.ProxyID) != "" {
		return false, nil
	}
	a := live.Clone()
	next := s.cloneStrictProxyLocked()
	if profile == nil {
		setStrictIntent(&next, a, "auto_pending_proxy")
	} else {
		allowed := false
		for _, p := range s.listProxyProfilesLocked() {
			if p.ID == profile.ID && p.Generation == profile.Generation && strictProxyIdentity(p.URL) == strictProxyIdentity(profile.URL) {
				_, allowed = s.chooseProxyLocked([]ProxyProfile{p}, nil, "", "")
				if allowed {
					a.ProxyID, a.ProxyURL = &p.ID, &p.URL
				}
				break
			}
		}
		if !allowed {
			return false, ErrProxyRetired
		}
		setStrictIntent(&next, a, "auto_proxy")
	}
	if err := s.commitProxyBundleStrictLocked(context.Background(), nil, []*model.Account{a}, nil, nil, &next); err != nil {
		return false, err
	}
	return true, nil
}
