// Package modelprobe 用受控最小模型请求观察线路，不改业务账号的持久代理绑定。
package modelprobe

import (
	"context"
	"sync"
	"time"

	"zcode2api/internal/store"
	"zcode2api/internal/web"
)

type Manager struct {
	store           *store.Store
	ctx             context.Context
	cancel          context.CancelFunc
	now             func() time.Time
	mu              sync.Mutex
	started, closed bool
	wake            chan struct{}
	wg              sync.WaitGroup
}

// now 是测试使用的时钟接入；生产调用不传，所有冷却和预算仍由 Store 校验。
func New(parent context.Context, st *store.Store, clock ...func() time.Time) *Manager {
	ctx, cancel := context.WithCancel(parent)
	now := time.Now
	if len(clock) > 0 && clock[0] != nil {
		now = clock[0]
	}
	return &Manager{store: st, ctx: ctx, cancel: cancel, now: now, wake: make(chan struct{}, 2)}
}

func (m *Manager) Start() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started || m.closed {
		return
	}
	m.started = true
	m.wg.Add(3)
	go func() {
		defer m.wg.Done()
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-m.ctx.Done():
				return
			case <-tick.C:
				if err := m.store.ScheduleModelProbes(m.now()); err != nil {
					web.Warn("model-probe", "巡检调度状态保存失败")
				}
				m.Wake()
			}
		}
	}()
	for i := 0; i < 2; i++ {
		go func() {
			defer m.wg.Done()
			tick := time.NewTicker(time.Second)
			defer tick.Stop()
			for {
				select {
				case <-m.ctx.Done():
					return
				case <-m.wake:
				case <-tick.C:
				}
				if _, err := m.RunDue(m.ctx); err != nil {
					web.Warn("model-probe", "巡检阶段执行或保存失败，未报告业务成功")
				}
			}
		}()
	}
}

func (m *Manager) Wake() {
	for i := 0; i < 2; i++ {
		select {
		case m.wake <- struct{}{}:
		default:
		}
	}
}

func (m *Manager) Request(target string) (store.ModelProbeRun, error) {
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return store.ModelProbeRun{}, store.ErrModelProbeUnavailable
	}
	r, err := m.store.QueueModelProbe(target, "manual", m.now())
	if err == nil {
		m.Start()
		m.Wake()
	}
	return r, err
}

// RunDue 与生产工作者共用同一发送路径，返回是否取得一个阶段；不会等待未来的冷却时间。
func (m *Manager) RunDue(ctx context.Context) (bool, error) {
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	l, err := m.store.TakeModelProbe(m.now())
	if err != nil || l == nil {
		return false, err
	}
	o, kind, usage := m.send(ctx, l)
	return true, m.store.CompleteModelProbe(l, o, kind, usage, m.now())
}

func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	m.cancel()
	m.mu.Unlock()
	m.wg.Wait()
}
