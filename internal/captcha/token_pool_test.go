package captcha

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"
)

// fakeTokenSolver 记录并发/调用次数；fn 为 nil 时返回唯一 token。
type fakeTokenSolver struct {
	mu        sync.Mutex
	calls     int
	active    int
	maxActive int
	fn        func(ctx context.Context, n int) (string, error)
}

func (s *fakeTokenSolver) Solve(ctx context.Context, _ Config) (string, error) {
	s.mu.Lock()
	s.calls++
	n := s.calls
	s.active++
	if s.active > s.maxActive {
		s.maxActive = s.active
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.active--
		s.mu.Unlock()
	}()
	if s.fn != nil {
		return s.fn(ctx, n)
	}
	return fmt.Sprintf("token-%d", n), nil
}

func (s *fakeTokenSolver) Close() error { return nil }

func (s *fakeTokenSolver) snapshot() (calls, maxActive int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, s.maxActive
}

func testPoolOptions() tokenPoolOptions {
	return tokenPoolOptions{
		ttl:         time.Minute,
		min:         1,
		max:         4,
		emptySolve:  2,
		refillEvery: time.Hour,
		idleShrink:  time.Hour,
		idleStop:    2 * time.Hour,
		cpuGovernor: false,
	}
}

func newTestTokenPool(t *testing.T, solver Solver, opts tokenPoolOptions) *tokenPool {
	t.Helper()
	p := newTokenPool(solver, opts, time.Now)
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func TestTokenPoolWarmTakeIsOneShot(t *testing.T) {
	solver := &fakeTokenSolver{fn: func(_ context.Context, n int) (string, error) {
		return fmt.Sprintf("token-%d", n), nil
	}}
	opts := testPoolOptions()
	opts.emptySolve = 1
	p := newTestTokenPool(t, solver, opts)
	cfg := solveCfg("cn")
	p.SetConfig(cfg, true)
	p.launchMintsLocked(cfg, captchaConfigKey(cfg), 1)
	waitFor(t, time.Second, func() bool { return p.Stats().PoolSize == 1 }, "预解 token 入池")

	tok, err := p.Take(context.Background(), cfg)
	if err != nil || tok == nil || tok.VerifyParam != "token-1" {
		t.Fatalf("池热取 token 不符: %+v %v", tok, err)
	}
	if !tok.TakeStats.Valid || tok.TakeStats.EmptyTake {
		t.Fatalf("池热命中不应标记空池: %+v", tok.TakeStats)
	}
	// 一次性：第二次 Take 必须重新求解，不能拿到同一 token。
	tok2, err := p.Take(context.Background(), cfg)
	if err != nil || tok2 == nil || tok2.VerifyParam == tok.VerifyParam {
		t.Fatalf("token 必须一次性: %+v %v", tok2, err)
	}
	if calls, _ := solver.snapshot(); calls != 2 {
		t.Fatalf("应求解 2 次，实际 %d", calls)
	}
}

func TestTokenPoolEmptySolveUsesAtMostTwoLanes(t *testing.T) {
	release := make(chan struct{})
	solver := &fakeTokenSolver{fn: func(ctx context.Context, n int) (string, error) {
		select {
		case <-release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		return fmt.Sprintf("token-%d", n), nil
	}}
	opts := testPoolOptions()
	opts.emptySolve = 2
	p := newTestTokenPool(t, solver, opts)
	cfg := solveCfg("cn")
	p.SetConfig(cfg, true)

	done := make(chan *Token, 1)
	errCh := make(chan error, 1)
	go func() {
		tok, err := p.Take(context.Background(), cfg)
		if err != nil {
			errCh <- err
			return
		}
		done <- tok
	}()
	waitFor(t, time.Second, func() bool {
		solver.mu.Lock()
		defer solver.mu.Unlock()
		return solver.active == 2
	}, "池空应并发 2 路求解")
	close(release)
	select {
	case tok := <-done:
		if tok == nil || tok.VerifyParam == "" {
			t.Fatalf("应返回 token: %+v", tok)
		}
	case err := <-errCh:
		t.Fatalf("取 token 失败: %v", err)
	case <-time.After(time.Second):
		t.Fatal("取 token 超时")
	}
	_, maxActive := solver.snapshot()
	if maxActive > 2 {
		t.Fatalf("池空并发上限应为 2，实际 %d", maxActive)
	}
}

func TestTokenPoolConcurrentTakesUnique(t *testing.T) {
	solver := &fakeTokenSolver{fn: func(_ context.Context, n int) (string, error) {
		time.Sleep(2 * time.Millisecond)
		return fmt.Sprintf("token-%d", n), nil
	}}
	opts := testPoolOptions()
	opts.min = 2
	opts.max = 16
	p := newTestTokenPool(t, solver, opts)
	cfg := solveCfg("cn")
	p.SetConfig(cfg, true)

	const n = 10
	var wg sync.WaitGroup
	seen := make(chan string, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := p.Take(context.Background(), cfg)
			if err != nil {
				errs <- err
				return
			}
			seen <- tok.VerifyParam
		}()
	}
	wg.Wait()
	close(seen)
	close(errs)
	for err := range errs {
		t.Fatalf("并发取 token 失败: %v", err)
	}
	uniq := map[string]bool{}
	for tok := range seen {
		if uniq[tok] {
			t.Fatalf("并发请求拿到重复 token: %s", tok)
		}
		uniq[tok] = true
	}
	if len(uniq) != n {
		t.Fatalf("应拿到 %d 个不同 token，实际 %d", n, len(uniq))
	}
	if _, maxActive := solver.snapshot(); maxActive > 2 {
		t.Fatalf("并发 mint 上限应为 2，实际 %d", maxActive)
	}
}

func TestTokenPoolTTLExpiry(t *testing.T) {
	now := time.Unix(1700000000, 0)
	solver := &fakeTokenSolver{}
	p := newTokenPool(solver, testPoolOptions(), func() time.Time { return now })
	t.Cleanup(func() { _ = p.Close() })
	cfg := solveCfg("cn")
	p.SetConfig(cfg, true)
	p.launchMintsLocked(cfg, captchaConfigKey(cfg), 1)
	waitFor(t, time.Second, func() bool { return p.Stats().PoolSize == 1 }, "预解 token 入池")

	now = now.Add(time.Minute + time.Second)
	if got := p.Stats().PoolSize; got != 0 {
		t.Fatalf("TTL 过期后池大小应为 0，实际 %d", got)
	}
	tok, err := p.Take(context.Background(), cfg)
	if err != nil || tok == nil || tok.VerifyParam == "token-1" {
		t.Fatalf("过期 token 不得复用: %+v %v", tok, err)
	}
}

func TestTokenPoolConfigChangeClearsBuckets(t *testing.T) {
	solver := &fakeTokenSolver{}
	p := newTestTokenPool(t, solver, testPoolOptions())
	cfgA := solveCfg("cn")
	cfgB := solveCfg("sg")
	p.SetConfig(cfgA, true)
	p.launchMintsLocked(cfgA, captchaConfigKey(cfgA), 1)
	waitFor(t, time.Second, func() bool { return p.Stats().PoolSize == 1 }, "配置 A token 入池")

	p.SetConfig(cfgB, true)
	if got := p.Stats().PoolSize; got != 0 {
		t.Fatalf("配置变化应清空旧池，实际 %d", got)
	}
	tok, err := p.Take(context.Background(), cfgB)
	if err != nil || tok == nil || tok.Region != cfgB.Region {
		t.Fatalf("应取到新配置 token: %+v %v", tok, err)
	}
}

func TestTokenPoolRejectsDuplicateCertifyID(t *testing.T) {
	param := func() string {
		payload, _ := json.Marshal(map[string]string{"certifyId": "cert-1", "sceneId": "sc"})
		return base64.RawURLEncoding.EncodeToString(payload)
	}()
	solver := &fakeTokenSolver{fn: func(context.Context, int) (string, error) { return param, nil }}
	opts := testPoolOptions()
	opts.min = 1
	opts.max = 4
	p := newTestTokenPool(t, solver, opts)
	cfg := solveCfg("cn")
	p.SetConfig(cfg, true)
	p.launchMintsLocked(cfg, captchaConfigKey(cfg), 2)
	waitFor(t, time.Second, func() bool {
		stats := p.Stats()
		return stats.MintTotal == 2 && stats.MintFailTotal == 1
	}, "重复 certifyId 应被拒绝一次")
	if got := p.Stats().PoolSize; got != 1 {
		t.Fatalf("重复 certifyId 只能保留 1 条 token，实际 %d", got)
	}
}

func TestTokenPoolCloseStopsRefill(t *testing.T) {
	solver := &fakeTokenSolver{}
	opts := testPoolOptions()
	opts.min = 1
	opts.refillEvery = 5 * time.Millisecond
	p := newTokenPool(solver, opts, time.Now)
	cfg := solveCfg("cn")
	p.SetConfig(cfg, true)
	waitFor(t, time.Second, func() bool {
		calls, _ := solver.snapshot()
		return calls >= 1
	}, "refill 应至少 mint 一次")
	_ = p.Close()
	callsAfterClose, _ := solver.snapshot()
	time.Sleep(30 * time.Millisecond)
	callsLater, _ := solver.snapshot()
	if callsLater != callsAfterClose {
		t.Fatalf("Close 后不应继续 refill：关闭时 %d，30ms 后 %d", callsAfterClose, callsLater)
	}
}

// TestTokenPoolIdleShrinkAndStop 空闲缩容与深度空闲停补。
//
// ttl 必须用生产默认量级（95s），不能用 1h：CaptchaPoolTTL 远小于 idleStop
// 正是旧缺陷的成因——桶里那条 token 必然先过期，过期后水位又变正数，
// 于是「深空闲」期间仍按每 TTL 一次的节奏永续补货。用 1h 的 ttl 会让
// 那条 token 活到断言时刻，把缺陷掩盖掉（本用例最初就是这么漏过去的）。
func TestTokenPoolIdleShrinkAndStop(t *testing.T) {
	now := time.Unix(1700000000, 0)
	solver := &fakeTokenSolver{}
	opts := testPoolOptions()
	opts.min = 2
	opts.max = 2
	opts.ttl = 95 * time.Second
	opts.idleShrink = 120 * time.Second
	opts.idleStop = 900 * time.Second
	p := newTokenPool(solver, opts, func() time.Time { return now })
	t.Cleanup(func() { _ = p.Close() })
	cfg := solveCfg("cn")
	p.SetConfig(cfg, true)
	p.refillOnce(now)
	waitFor(t, time.Second, func() bool { return p.Stats().PoolSize == 2 }, "活跃时应补到 min=2")

	now = now.Add(3 * time.Minute)
	p.Clear()
	p.refillOnce(now)
	waitFor(t, time.Second, func() bool { return p.Stats().PoolSize == 1 }, "空闲缩容应只保留 1 条")

	callsBefore, _ := solver.snapshot()
	now = now.Add(20 * time.Minute)
	// 先确认桶里那条已按 TTL 过期：旧实现正是在这里开始无限补货。
	if size := p.Stats().PoolSize; size != 0 {
		t.Fatalf("深空闲时库存应已按 TTL 过期，实际 %d 条", size)
	}
	p.refillOnce(now)
	time.Sleep(20 * time.Millisecond)
	callsAfter, _ := solver.snapshot()
	if callsAfter != callsBefore {
		t.Fatalf("深度空闲不应继续补货：%d -> %d", callsBefore, callsAfter)
	}
	// 多推几个 TTL 周期，确保不是「刚好这一轮没补」。
	for i := 0; i < 3; i++ {
		now = now.Add(95 * time.Second)
		p.refillOnce(now)
	}
	time.Sleep(20 * time.Millisecond)
	if callsLater, _ := solver.snapshot(); callsLater != callsBefore {
		t.Fatalf("深度空闲跨多个 TTL 仍不应补货：%d -> %d", callsBefore, callsLater)
	}
	if !p.Stats().IdleStopped {
		t.Fatalf("Stats 应报告 idle_stopped=true")
	}
}

// TestTokenPoolDeepIdleStillServesTake 深度空闲只停「主动补货」，
// 真实请求的按需求解必须仍然可用——否则等于把验证码功能关掉。
func TestTokenPoolDeepIdleStillServesTake(t *testing.T) {
	now := time.Unix(1700000000, 0)
	solver := &fakeTokenSolver{}
	opts := testPoolOptions()
	opts.min = 1
	opts.max = 1
	opts.ttl = 95 * time.Second
	opts.idleShrink = 120 * time.Second
	opts.idleStop = 900 * time.Second
	opts.emptySolve = 1
	p := newTokenPool(solver, opts, func() time.Time { return now })
	t.Cleanup(func() { _ = p.Close() })
	cfg := solveCfg("cn")
	p.SetConfig(cfg, true)

	// 推进到深度空闲：主动补货已停，桶为空。
	now = now.Add(30 * time.Minute)
	p.refillOnce(now)
	time.Sleep(20 * time.Millisecond)
	if size := p.Stats().PoolSize; size != 0 {
		t.Fatalf("深空闲后不应有库存，实际 %d 条", size)
	}
	if calls, _ := solver.snapshot(); calls != 0 {
		t.Fatalf("深空闲不应触发求解，实际 %d 次", calls)
	}

	// 真实请求：Take 必须触发同步求解并拿到 token。
	tok, err := p.Take(context.Background(), cfg)
	if err != nil || tok == nil || tok.VerifyParam == "" {
		t.Fatalf("深空闲下 Take 应仍能按需求解: %+v %v", tok, err)
	}
	if !tok.TakeStats.EmptyTake {
		t.Fatalf("池空取用应标记 empty_take: %+v", tok.TakeStats)
	}
}

func TestManagerCloseStopsTokenPool(t *testing.T) {
	setBrowserEnabled(t, true)
	solver := &fakeTokenSolver{}
	m := NewManager()
	m.SetConfigProvider(func(context.Context) (Config, error) { return DefaultConfig, nil })
	m.SetSolver(solver)
	m.EnableTokenPool(true)
	if _, err := m.GetVerifyParam(context.Background()); err != nil {
		t.Fatalf("首次取 token 失败: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	callsAfterClose, _ := solver.snapshot()
	time.Sleep(30 * time.Millisecond)
	callsLater, _ := solver.snapshot()
	if callsLater != callsAfterClose {
		t.Fatalf("Manager.Close 后 token 池不应继续补解：%d -> %d", callsAfterClose, callsLater)
	}
}

func TestCPUGovernorFactor(t *testing.T) {
	samples := []cpuSample{
		{Busy: 10, Total: 100},
		{Busy: 80, Total: 200},  // delta 70/100 = 70%
		{Busy: 180, Total: 300}, // delta 100/100 = 100%
		{Busy: 190, Total: 500}, // delta 10/200 = 5%
	}
	idx := 0
	g := newCPUGovernor(func() (cpuSample, bool) {
		if idx >= len(samples) {
			return samples[len(samples)-1], true
		}
		s := samples[idx]
		idx++
		return s, true
	})
	g.Update()
	g.Update()
	if g.Percent() != 70 || g.Factor() != 0.5 {
		t.Fatalf("70%% 应降半速: %.1f %.2f", g.Percent(), g.Factor())
	}
	g.Update()
	if g.Percent() != 100 || g.Factor() != 0.25 {
		t.Fatalf("100%% 应降到四分之一: %.1f %.2f", g.Percent(), g.Factor())
	}
	g.Update()
	if g.Percent() != 5 || g.Factor() != 1 {
		t.Fatalf("低负载应恢复全速: %.1f %.2f", g.Percent(), g.Factor())
	}
}

func TestWaitPercentiles(t *testing.T) {
	p50, p95 := waitPercentiles([]int64{1, 2, 3, 4, 100})
	if p50 != 3 || p95 != 100 {
		t.Fatalf("p50/p95 不符: %v %v", p50, p95)
	}
	if p50, p95 := waitPercentiles(nil); p50 != 0 || p95 != 0 {
		t.Fatalf("空样本应为 0: %v %v", p50, p95)
	}
}
