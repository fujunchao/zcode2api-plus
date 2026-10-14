package store

import (
	"context"
	"time"

	"zcode2api/internal/web"
)

// 一个 Store 同时最多一个恢复任务；三次有界退避后保留闭锁并等待显式恢复/重启。
// 不生成任何额外模型请求，不为每个账号启动独立任务。
func (s *Store) scheduleStrictProxyRecoveryLocked() {
	if s.strictClosed || s.strictRetryCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.strictRetryCancel, s.strictRetryDone = cancel, done
	go func() {
		defer func() {
			cancel()
			s.mu.Lock()
			s.strictRetryCancel = nil
			s.mu.Unlock()
			close(done)
		}()
		for _, delay := range []time.Duration{time.Second, 5 * time.Second, 30 * time.Second} {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			if err := s.RecoverStrictProxyRetirements(); err == nil {
				web.Ok("strict-proxy", "严格删除事务已恢复，既有永久禁止记录仍然保留")
				return
			}
			web.Warn("strict-proxy", "严格删除事务恢复失败，保留退役阻断并按有界退避重试")
		}
		web.Err("strict-proxy", "严格删除事务有界重试已耗尽，需要恢复持久化后执行管理恢复")
	}()
}
