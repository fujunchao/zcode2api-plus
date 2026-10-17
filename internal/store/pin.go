package store

import (
	"errors"
	"fmt"
	"time"

	"zcode2api/internal/model"
	"zcode2api/internal/web"
)

// 单号钉住：把全部流量临时钉在一个账号上，用于真机复验（例如换凭据后确认 405 是否解除）。
//
// 设计约束：
//   - **进程内**，不落库、不进 accounts.data：调试用的钉子不该跨重启存活；
//   - 只影响选号，不改账号状态：钉住的号若在冷却/停用/已归档，选号照常回落，请求不失败；
//   - 不推进轮转游标：解除钉住后，正常轮询的顺序不被这段插曲打乱；
//   - 有 TTL（默认 30 分钟，上限 24 小时）：忘记解除不会永久烧一个号。

// ErrPinArchived 归档账号不可钉住：归档即停止调用，钉它只会让请求全部回落。
var ErrPinArchived = errors.New("账号已归档，先恢复到账号池再钉住")

// PinnedAccount 当前钉住状态的对外视图。
type PinnedAccount struct {
	ID    string  `json:"id"`
	Name  string  `json:"name"`
	Until float64 `json:"until"` // Unix 秒；到期后自动失效
}

// PinAccount 钉住指定账号（ID 或名称）。minutes <= 0 时按 30 分钟；调用方负责钳上限。
// 返回 (nil, nil) 表示账号不存在；返回 ErrPinArchived 表示拒绝。
func (s *Store) PinAccount(provider, idOrName string, minutes int, now time.Time) (*PinnedAccount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	acc := s.findLocked(provider, idOrName)
	if acc == nil {
		return nil, nil
	}
	if acc.ArchivedAt != nil {
		return nil, ErrPinArchived
	}
	if minutes <= 0 {
		minutes = 30
	}
	s.pinID = acc.ID
	s.pinUntil = now.Add(time.Duration(minutes) * time.Minute)
	s.pinMissLogged = false
	web.Warn(fmt.Sprintf("已钉住账号 %s（%d 分钟），期间全部模型请求优先打到该账号", acc.Name, minutes))
	return &PinnedAccount{ID: acc.ID, Name: acc.Name, Until: float64(s.pinUntil.UnixNano()) / 1e9}, nil
}

// UnpinAccount 解除钉住（幂等）。
func (s *Store) UnpinAccount() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pinID == "" {
		return
	}
	s.pinID, s.pinUntil, s.pinMissLogged = "", time.Time{}, false
	web.Warn("已解除账号钉住")
}

// PinnedAccount 返回当前有效的钉住状态；已到期时顺手清理。
func (s *Store) PinnedAccount(now time.Time) *PinnedAccount {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pinID == "" {
		return nil
	}
	if !now.Before(s.pinUntil) {
		s.pinID, s.pinUntil, s.pinMissLogged = "", time.Time{}, false
		return nil
	}
	name := ""
	if acc := s.findAnyLocked(s.pinID); acc != nil {
		name = acc.Name
	} else {
		// 账号被删除：钉子自然失效。
		s.pinID, s.pinUntil, s.pinMissLogged = "", time.Time{}, false
		return nil
	}
	return &PinnedAccount{ID: s.pinID, Name: name, Until: float64(s.pinUntil.UnixNano()) / 1e9}
}

// pinHitLocked 在已筛出的可用池里找钉住的账号。找不到时记录一次原因（每次钉住只记一条，
// 避免高 QPS 下刷屏），然后让调用方回落正常选号。
func (s *Store) pinHitLocked(base []*model.Account, skipIDs map[string]bool, now time.Time) *model.Account {
	if s.pinID == "" || !now.Before(s.pinUntil) {
		return nil
	}
	for _, a := range base {
		if a.ID == s.pinID {
			return a
		}
	}
	if !s.pinMissLogged {
		s.pinMissLogged = true
		reason := "不可调度（冷却/停用/线路不可用）"
		if skipIDs[s.pinID] {
			reason = "本请求已尝试过"
		}
		name := s.pinID
		if acc := s.findAnyLocked(s.pinID); acc != nil {
			name = acc.Name
		}
		web.Warn(fmt.Sprintf("钉住账号 %s 本次未能命中（%s），已回落正常选号", name, reason))
	}
	return nil
}
