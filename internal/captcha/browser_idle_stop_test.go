// 池级空闲停池与预热语义的用例。
package captcha

import (
	"context"
	"testing"
	"time"
)

// newIdleTestSolver 建一个用假 worker 池的求解器（池工厂返回已启动的假池）。
func newIdleTestSolver(t *testing.T) (*Manager, *BrowserSolver) {
	t.Helper()
	setBrowserEnabled(t, true)
	m := NewManager()
	m.SetConfigProvider(func(context.Context) (Config, error) { return DefaultConfig, nil })
	s := NewBrowserSolver()
	s.SetPoolFactory(func(Config) *Pool {
		f := &scriptFactory{}
		return newStartedFakePool(t, f.create, 1)
	})
	m.SetSolver(s)
	t.Cleanup(func() { _ = m.Close() })
	return m, s
}

// TestBrowserSolverStopsIdlePool 空闲超时释放整池，下次求解冷启动重建。
func TestBrowserSolverStopsIdlePool(t *testing.T) {
	m, s := newIdleTestSolver(t)

	now := time.Unix(1700000000, 0)
	s.now = func() time.Time { return now }
	s.lastUse = now
	s.idleStop = 10 * time.Minute
	s.minStopGap = time.Hour

	if _, err := m.GetVerifyParam(context.Background()); err != nil {
		t.Fatalf("首次求解失败: %v", err)
	}
	if got := s.Status().State; got != "online" {
		t.Fatalf("求解后应为 online，实际 %q", got)
	}

	// 未到空闲阈值：不停池。
	now = now.Add(5 * time.Minute)
	s.stopIdlePool()
	if got := s.Status().State; got != "online" {
		t.Fatalf("未超阈值不应停池，实际 %q", got)
	}

	// 超过阈值：停池。
	now = now.Add(10 * time.Minute)
	s.stopIdlePool()
	if got := s.Status().State; got != "standby" {
		t.Fatalf("超阈值应停池，实际 %q", got)
	}

	// 再次求解：冷启动重建，功能不受影响。
	if _, err := m.GetVerifyParam(context.Background()); err != nil {
		t.Fatalf("停池后应能重建: %v", err)
	}
	if got := s.Status().State; got != "online" {
		t.Fatalf("再次求解应重建池，实际 %q", got)
	}
}

// TestBrowserSolverIdleStopRespectsInFlight 有在途求解时绝不允许停池：
// 停池会中止在途请求，违反「不打断在途」这条不变式。
func TestBrowserSolverIdleStopRespectsInFlight(t *testing.T) {
	m, s := newIdleTestSolver(t)

	now := time.Unix(1700000000, 0)
	s.now = func() time.Time { return now }
	s.lastUse = now
	s.idleStop = time.Minute
	s.minStopGap = time.Minute

	if _, err := m.GetVerifyParam(context.Background()); err != nil {
		t.Fatalf("首次求解失败: %v", err)
	}

	s.mu.Lock()
	s.inFlight = 1
	s.mu.Unlock()
	now = now.Add(time.Hour)
	s.stopIdlePool()
	if got := s.Status().State; got != "online" {
		t.Fatalf("有在途求解不应停池，实际 %q", got)
	}

	s.mu.Lock()
	s.inFlight = 0
	s.mu.Unlock()
	s.stopIdlePool()
	if got := s.Status().State; got != "standby" {
		t.Fatalf("在途归零后应停池，实际 %q", got)
	}
}

// TestTokenPoolWarmDoesNotResetIdle 预热不该被当成真实流量：
// 若它更新 lastTake，空闲停补会被预热本身反复推翻。
func TestTokenPoolWarmDoesNotResetIdle(t *testing.T) {
	now := time.Unix(1700000000, 0)
	solver := &fakeTokenSolver{}
	opts := testPoolOptions()
	opts.min = 1
	opts.max = 1
	opts.ttl = 95 * time.Second
	opts.idleShrink = 120 * time.Second
	opts.idleStop = 900 * time.Second
	p := newTokenPool(solver, opts, func() time.Time { return now })
	t.Cleanup(func() { _ = p.Close() })
	cfg := solveCfg("cn")
	p.SetConfig(cfg, true)

	now = now.Add(30 * time.Minute)
	p.refillOnce(now)
	if size := p.Stats().PoolSize; size != 0 {
		t.Fatalf("深空闲应无库存，实际 %d", size)
	}

	p.Warm()
	waitFor(t, time.Second, func() bool { return p.Stats().PoolSize == 1 }, "Warm 应解出一条 token")
	if idle := p.Stats().IdleSeconds; idle < 1700 {
		t.Fatalf("Warm 不应重置空闲计时，实际 %.0fs", idle)
	}
}
