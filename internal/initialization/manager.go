// Package initialization 推进持久新号任务；不删除共享代理、不自动重开耗尽轮次。
package initialization

import (
	"context"
	"errors"
	"sync"
	"time"

	"zcode2api/internal/captcha"
	"zcode2api/internal/claim"
	"zcode2api/internal/model"
	"zcode2api/internal/quota"
	"zcode2api/internal/store"
	"zcode2api/internal/web"
)

type Manager struct {
	Store   *store.Store
	Quota   *quota.Service
	Captcha *captcha.Manager
	Acquire func(context.Context) (func(), error)
	ctx     context.Context
	cancel  context.CancelFunc
	start   sync.Once
	wake    chan struct{}
	done    chan struct{}
}

func New(parent context.Context, st *store.Store, qs *quota.Service, cm *captcha.Manager, acquire func(context.Context) (func(), error)) *Manager {
	ctx, cancel := context.WithCancel(parent)
	return &Manager{Store: st, Quota: qs, Captcha: cm, Acquire: acquire, ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1), done: make(chan struct{})}
}

func (m *Manager) Start() { m.start.Do(func() { go m.loop() }) }
func (m *Manager) Wake() {
	m.Start()
	select {
	case m.wake <- struct{}{}:
	default:
	}
}
func (m *Manager) Close() { m.cancel(); m.Start(); <-m.done }

func (m *Manager) loop() {
	defer close(m.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if m.ctx.Err() != nil {
			return
		}
		policy, err := m.Store.NewAccountProxyPolicy()
		if err == nil && policy.Enabled && m.Store.ClaimAutoEnabled() {
			for _, id := range m.Store.InitializationQueue() {
				if m.ctx.Err() != nil {
					return
				}
				if err := m.Step(m.ctx, id); err != nil && m.ctx.Err() == nil && !errors.Is(err, store.ErrInitializationConflict) && !errors.Is(err, store.ErrInitializationUnavailable) {
					web.Warn("initialization", "初始化步骤未提交，已保留任务等待复查")
				}
			}
		}
		select {
		case <-m.ctx.Done():
			return
		case <-m.wake:
		case <-ticker.C:
		}
	}
}

// Step 一次只尝试一条代理，释放共享领取槽位后才轮到下一账号/下一候选。
func (m *Manager) Step(ctx context.Context, id string) error {
	t := m.Store.Initialization(id)
	if t == nil || !t.Managed || t.Paused {
		return nil
	}
	if t.GrantedAt > 0 {
		_, err := m.Store.BindInitialization(id, t.Revision, false)
		return err
	}
	if m.Acquire != nil {
		release, err := m.Acquire(ctx)
		if err != nil {
			return err
		}
		defer release()
	}
	a, err := m.Store.BeginInitialization(id, time.Now())
	if err != nil || a == nil {
		return err
	}
	defer m.Store.ReleaseInitialization(a)
	deadline := time.Now().Add(time.Duration(a.Task.Policy.AttemptTimeoutSeconds) * time.Second)
	if a.Task.Deadline > 0 && time.UnixMilli(a.Task.Deadline).Before(deadline) {
		deadline = time.UnixMilli(a.Task.Deadline)
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	// 外部解析或注入依赖异常不能终结工作者；预写意图使恢复时先查余额。
	defer func() {
		if recover() != nil {
			web.Warn("initialization", "初始化步骤异常，保留任务供下一次核验")
		}
	}()
	first := m.Quota.ObserveInitialization(ctx, a)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := m.Store.ObserveInitialization(a, first); err != nil {
		return err
	}
	if first.Kind == "available" {
		return m.bind(id)
	}
	if a.Task.State == "verifying" || a.Task.Uncertain {
		return m.Store.EndInitialization(a, "quota_pending", "先前请求结果仍未确认，请复查后重试", 0, false)
	}
	if first.Kind == "allocated" || a.Account.UseCount > 0 || (a.Account.ClaimView() != nil && a.Account.ClaimView().ClaimedAt != nil) {
		return m.Store.EndInitialization(a, "historical_quota", "已有套餐或使用记录，不作为未激活新号重领", 0, false)
	}
	if first.Kind != "missing" {
		return m.Store.EndInitialization(a, "quota_unknown", "未取得有效额度证据", 0, first.ProxyFailure)
	}
	svc := claim.NewServiceContext(ctx, m.Captcha)
	svc.Store = m.Store
	svc.Initialization = a
	outcome := svc.RunInitialization(a.Account)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	captchaSec, retrySec, _ := m.Store.ClaimCooldowns()
	if outcome.NextAt == 0 && outcome.Reason != "no_plans" {
		wait := retrySec
		if outcome.Reason == "captcha_required" {
			wait = captchaSec
		}
		outcome.NextAt = time.Now().Add(time.Duration(wait) * time.Second).UnixMilli()
	}
	if outcome.Claimed || outcome.Reason == "captcha_required" {
		if err := m.Store.SaveInitializationClaim(a, outcome.Succeeded, outcome.NextAt, outcome.Reason); err != nil {
			return err
		}
	}
	last := model.InitialQuotaObservation{Kind: "unknown"}
	for check := 0; check < 3; check++ {
		last = m.Quota.ObserveInitialization(ctx, a)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := m.Store.ObserveInitialization(a, last); err != nil {
			return err
		}
		if last.Kind == "available" {
			return m.bind(id)
		}
		if !outcome.Succeeded && outcome.Reason != "already_claimed" {
			break
		}
		if check < 2 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
	}
	retry := (outcome.Reason == "no_plans" && last.Kind == "missing") || outcome.Reason == "proxy_unreachable"
	nextAt := outcome.NextAt
	if retry {
		nextAt = 0
	}
	return m.Store.EndInitialization(a, outcome.Reason, "本轮未确认可用额度："+outcome.Reason, nextAt, retry)
}

func (m *Manager) bind(id string) error {
	t := m.Store.Initialization(id)
	if t == nil {
		return store.ErrInitializationConflict
	}
	_, err := m.Store.BindInitialization(id, t.Revision, false)
	return err
}
