// Manager 的 Solver 实现：惰性启动浏览器池、按验证码配置键复用/重建池、
// 启动失败进入冷却。对应 Python 版 app/captcha.py 的 _solve_browser 与
// _ensure_browser_pool。冷却期内与配置缺失时返回 ErrUnavailable，
// 由网关映射为 503 captcha_required（人工回填兜底）。
package captcha

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/web"
)

// BrowserSolver 浏览器池求解器（注入 Manager.SetSolver）。
type BrowserSolver struct {
	mu sync.Mutex
	// pool 当前池与它绑定的配置键（scene|region|prefix，对齐 _browser_config_key）。
	pool    *Pool
	poolKey string
	// inFlight 当前池正在锁外执行的求解数；>0 时配置变更不立即 Stop，
	// 而是标记 retired，由最后一个归还者负责关闭（避免中止在途求解）。
	inFlight int
	retired  *Pool
	// failureUntil 启动失败后的冷却截止（单调时钟）。
	failureUntil time.Time
	// closed 求解器已关闭。启动是在锁外做的，启动期间可能有人调用 Close——
	// 没有这个标记，启动完成后的池会被挂到一个已关闭的求解器上、再也没人回收。
	closed bool

	// starting 非 nil 表示已有调用者正在锁外启动「startingKey」这个配置键的池，
	// 关闭即表示启动结束。作用是避免并发冷启动时各自拉起一个浏览器进程
	//（每个都是真进程、启动还要等就绪），同时仍然不持锁做启动。
	starting    chan struct{}
	startingKey string

	// now 可注入时钟（测试用）。
	now func() time.Time
	// newPool 池构造函数；测试注入假工厂，默认绑定真实 rod 工厂。
	newPool func(cfg Config) *Pool
}

// NewBrowserSolver 创建浏览器求解器；池在首次 Solve 时惰性启动。
func NewBrowserSolver() *BrowserSolver {
	return &BrowserSolver{
		now: time.Now,
		newPool: func(cfg Config) *Pool {
			return NewPool(NewRodWorkerFactory(cfg), config.CaptchaBrowserWorkers)
		},
	}
}

// SetPoolFactory 注入池构造函数（测试用）。
func (s *BrowserSolver) SetPoolFactory(f func(cfg Config) *Pool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.newPool = f
}

// SetNow 注入时钟（测试用）。
func (s *BrowserSolver) SetNow(fn func() time.Time) { s.now = fn }

// Solve 返回一个浏览器求解的 verify_param。
//
// 错误语义：
//   - 配置缺失 / 冷却期内 / 启动失败 → ErrUnavailable（管理器原样上抛，
//     网关 503 captcha_required；对齐 Python 浏览器路径返回 None 后无
//     Node 兜底的形态——jsdom 求解已被上游 F001 判死，不移植）；
//   - 池已启动但本次求解失败 → 带原因的错误（交给网关有限重试；
//     池自身负责替换超时或退出的 worker）。
//
// 并发语义：锁只保护池的选取与重建；实际求解在锁外执行，
// 因此多个调用者可并发使用池内不同槽位（并发上限由池的 size 决定）。
func (s *BrowserSolver) Solve(ctx context.Context, cfg Config) (string, error) {
	if ctx == nil {
		ctx = context.Background() // GetVerifyParam(nil) 允许 nil ctx
	}
	key := strings.Join([]string{
		strings.TrimSpace(cfg.SceneID),
		strings.TrimSpace(cfg.Region),
		strings.TrimSpace(cfg.Prefix),
	}, "|")
	if key == "||" {
		return "", fmt.Errorf("%w：验证码配置缺少 sceneId、region 或 prefix", ErrUnavailable)
	}

	pool, release, err := s.acquirePool(ctx, cfg, key)
	if err != nil {
		return "", err
	}
	defer release()

	param, solveErr := pool.Solve(ctx)
	if solveErr != nil {
		web.Warn("captcha", "真实浏览器验证码求解失败: "+redact(solveErr.Error()))
		return "", fmt.Errorf("真实浏览器验证码求解失败: %w", solveErr)
	}
	if strings.TrimSpace(param) == "" {
		return "", errors.New("真实浏览器验证码求解器返回空结果")
	}
	return param, nil
}

// acquirePool 取（必要时启动/重建）可用的池，并登记一次在途使用。
// 返回的 release 必须在求解结束后调用：它负责在池已被配置变更淘汰时关闭旧池，
// 从而保证「不中止在途求解」与「旧池最终被回收」两者兼得。
//
// 锁的边界：`pool.Start()` / `pool.Stop()` 都可能阻塞数十秒（启动总超时 90s、
// 停机超时 10s），因此一律在锁外执行。持锁做这两件事会把所有求解者、以及停机
// 时的 Close 一并钉住——后者会让容器等不到优雅退出而被强杀。
func (s *BrowserSolver) acquirePool(ctx context.Context, cfg Config, key string) (*Pool, func(), error) {
	for {
		// 已取消的调用者不必进入临界区，更不该为它淘汰掉正在服务的旧池。
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}

		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return nil, nil, fmt.Errorf("%w：求解器已关闭", ErrUnavailable)
		}
		if s.now().Before(s.failureUntil) {
			s.mu.Unlock()
			return nil, nil, fmt.Errorf("%w：浏览器池冷却中", ErrUnavailable)
		}
		if s.pool != nil && s.poolKey == key && s.pool.IsStarted() {
			s.inFlight++
			release := s.releaseAfterUse()
			s.mu.Unlock()
			return s.pool, release, nil
		}
		// 同键已有调用者在启动：等它结束再重试，避免重复拉起浏览器进程。
		if s.starting != nil && s.startingKey == key {
			wait := s.starting
			s.mu.Unlock()
			select {
			case <-wait:
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
			continue
		}
		// 由本调用者负责启动：先在锁内登记，并摘掉旧池的引用（免得它在锁外
		// 启动期间被别的调用者取走当成当前池用）；真正要关的池留到锁外再停。
		done := make(chan struct{})
		s.starting, s.startingKey = done, key
		stopNow := s.retireCurrentLocked()
		newPool := s.newPool
		s.mu.Unlock()

		if stopNow != nil {
			stopNow.Stop()
		}
		pool := newPool(cfg)
		startErr := pool.Start()

		s.mu.Lock()
		s.starting, s.startingKey = nil, ""
		close(done) // 先唤醒等待者，它们会在下一次循环里看到结果
		switch {
		case startErr != nil:
			cooldown := time.Duration(config.CaptchaBrowserFailureCooldown) * time.Second
			s.failureUntil = s.now().Add(cooldown)
			s.mu.Unlock()
			web.Warn("captcha", fmt.Sprintf(
				"真实浏览器池不可用，%s 内回退人工回填: %s", cooldown, redact(startErr.Error())))
			return nil, nil, fmt.Errorf("%w：浏览器池启动失败", ErrUnavailable)
		case s.closed:
			s.mu.Unlock()
			pool.Stop()
			return nil, nil, fmt.Errorf("%w：求解器已关闭", ErrUnavailable)
		case s.now().Before(s.failureUntil):
			// 启动期间有别的调用者启动失败并进入冷却。
			s.mu.Unlock()
			pool.Stop()
			return nil, nil, fmt.Errorf("%w：浏览器池冷却中", ErrUnavailable)
		}
		s.pool = pool
		s.poolKey = key
		s.inFlight++
		release := s.releaseAfterUse()
		s.mu.Unlock()

		web.Ok("captcha", fmt.Sprintf("真实浏览器验证码池已就绪（%d 个 worker）", config.CaptchaBrowserWorkers))
		return pool, release, nil
	}
}

// releaseAfterUse 生成「归还一次在途使用」的闭包。最后一个归还者负责关闭已被
// 淘汰的旧池；关闭可能阻塞到停机超时，故同样在锁外做。
func (s *BrowserSolver) releaseAfterUse() func() {
	return func() {
		s.mu.Lock()
		s.inFlight--
		var pending *Pool
		if s.inFlight == 0 && s.retired != nil {
			pending = s.retired
			s.retired = nil
		}
		s.mu.Unlock()
		if pending != nil {
			pending.Stop()
		}
	}
}

// retireCurrentLocked 摘掉当前池的引用（调用方持锁），返回需要在锁外关闭的池。
//
// 有在途求解时把当前池标记为 retired 延迟关闭（对齐 _stop_browser_pool：不中止
// 在途求解），由最后一个归还者回收；否则立即摘除。上一轮 retired 若至今无人回收
// （inFlight 一直没归零过），这里一并交出去关闭，避免旧池越堆越多。
func (s *BrowserSolver) retireCurrentLocked() (stopNow *Pool) {
	if s.pool == nil {
		return nil
	}
	if s.inFlight > 0 {
		stopNow = s.retired
		s.retired = s.pool
	} else {
		stopNow = s.pool
	}
	s.pool = nil
	s.poolKey = ""
	return stopNow
}

// Close 关闭浏览器池（Manager.Close 转发）。
// Stop 可能阻塞至 shutdownTimeout（默认 10s），故在锁外执行，
// 避免阻塞其他仍在使用求解器的调用者。
func (s *BrowserSolver) Close() error {
	s.mu.Lock()
	pool := s.pool
	retired := s.retired
	s.pool = nil
	s.poolKey = ""
	s.retired = nil
	// 置位后，正在锁外启动的池会在提交前看到它并自行关闭，不会挂到一个已关闭的
	// 求解器上；后续 Solve 也直接短路，不再拉起新浏览器。
	s.closed = true
	s.mu.Unlock()

	if pool != nil {
		pool.Stop()
	}
	if retired != nil {
		retired.Stop()
	}
	return nil
}
