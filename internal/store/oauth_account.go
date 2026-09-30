package store

import (
	"errors"
	"strings"
	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

// SaveOAuthAccount 统一 CLI/后台的授权入库；普通添加仍保留“重复项不修改”语义。
func (s *Store) SaveOAuthAccount(name, token, email string) (*model.Account, bool, error) {
	token = strings.TrimSpace(token)
	candidate := accountWithIdentity(model.ProviderZai, name, token, email)
	if candidate.Name == "oauth-login" && candidate.Email != nil {
		candidate.Name = *candidate.Email
	}
	if token == "" || candidate.Mode != "jwt" {
		return nil, false, errors.New("OAuth 登录结果缺少有效 JWT")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	live := s.duplicateLocked(model.ProviderZai, candidate)
	if live == nil {
		mid := config.NewDeviceMid()
		candidate.VirtualDeviceMid = &mid
		if err := s.persistAccountLocked(candidate); err != nil {
			return nil, false, err
		}
		s.accounts[model.ProviderZai] = append(s.accounts[model.ProviderZai], candidate)
		return candidate.Clone(), true, nil
	}
	// 在副本上先落库；失败不能让正在运行的服务提前换上未持久化的凭据。
	apply := func(a *model.Account) {
		a.Mode = "jwt"
		a.JWTToken = &token
		a.Status = model.StatusActive
		a.CoolingUntil = nil
		a.RiskControlStreak = 0
		a.LastError, a.LastErrorKind, a.LastErrorAt = nil, nil, nil
		if candidate.UserID != nil {
			a.UserID = candidate.UserID
		}
		if (a.Email == nil || *a.Email == "") && candidate.Email != nil {
			a.Email = candidate.Email
		}
		if a.Name == "oauth-login" && candidate.Email != nil {
			a.Name = *candidate.Email
		}
	}
	next := live.Clone()
	apply(next)
	if err := s.persistAccountLocked(next); err != nil {
		return nil, false, err
	}
	apply(live)
	return live.Clone(), false, nil
}
