// 验证码 token 池：把浏览器求解从 JWT 请求热路径上挪走。
//
// 设计要点（P0-B）：
//   - 按 sceneId|region|prefix 分桶；配置变化清空旧桶，不跨配置复用 token。
//   - token 视为一次性：Take 取出即删除；TTL 到期由 Take/refill 惰性清理。
//   - 后台 refill 在低于目标水位时补货；池空时同步发起 1–2 路 solve 竞速，
//     取最先成功的结果，其余成功结果回填池子。
//   - CertifyIdRegistry：同一 certifyId（或同一 token 原文）允许 mint 一次，
//     防止并发请求拿到重复 token 触发上游 F008。
//   - CPU governor 按占用率缩放目标与并发；空闲 120s 缩到 1，900s 停补。
//   - Manager.Close 调用 Close 停止 refill 与在途 mint。
//
// 这里只缓存 verify_param 字符串，不落盘、不打印；日志只输出计数和脱敏错误。
package captcha

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/web"
)

// TakeStats 一次取 token 的诊断信息（per-request，不包含 token 原文）。
type TakeStats struct {
	Valid     bool
	Wait      time.Duration
	PoolSize  int
	EmptyTake bool
	MintFail  int
}

// TakeError 包装取 token 失败，同时携带诊断信息；errors.Is/As 仍可识别底层错误。
type TakeError struct {
	Err   error
	Stats TakeStats
}

func (e *TakeError) Error() string {
	if e == nil || e.Err == nil {
		return "验证码 token 获取失败"
	}
	return e.Err.Error()
}

func (e *TakeError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// TakeStatsFromError 取出 TakeError 中的诊断；普通错误返回零值。
func TakeStatsFromError(err error) TakeStats {
	var te *TakeError
	if errors.As(err, &te) && te != nil {
		return te.Stats
	}
	return TakeStats{}
}

// TokenPoolStats token 池累计指标（后台 /admin/api/monitor 与测试使用）。
type TokenPoolStats struct {
	Enabled         bool    `json:"enabled"`
	PoolSize        int     `json:"pool_size"`
	TakeTotal       int64   `json:"take_total"`
	EmptyTakeTotal  int64   `json:"empty_take_total"`
	MintTotal       int64   `json:"mint_total"`
	MintFailTotal   int64   `json:"mint_fail_total"`
	WaitP50MS       float64 `json:"wait_p50_ms"`
	WaitP95MS       float64 `json:"wait_p95_ms"`
	Inflight        int     `json:"inflight"`
	Target          int     `json:"target"`
	IdleSeconds     float64 `json:"idle_seconds"`
	CPUPercent      float64 `json:"cpu_percent"`
	WaitSampleCount int     `json:"wait_sample_count"`
}

// poolToken 一条预解 token；参数不出日志、不落盘。
type poolToken struct {
	param     string
	region    string
	certifyID string
	mintedAt  time.Time
	expiresAt time.Time
}

// tokenPoolOptions 池参数；零值由 newTokenPool 填默认。
type tokenPoolOptions struct {
	ttl         time.Duration
	min         int
	max         int
	emptySolve  int
	refillEvery time.Duration
	idleShrink  time.Duration
	idleStop    time.Duration
	cpuGovernor bool
}

func defaultTokenPoolOptions() tokenPoolOptions {
	return tokenPoolOptions{
		ttl:         time.Duration(config.CaptchaPoolTTL) * time.Millisecond,
		min:         config.CaptchaPoolMin,
		max:         config.CaptchaPoolMax,
		emptySolve:  config.CaptchaPoolEmptySolve,
		refillEvery: time.Duration(config.CaptchaPoolRefillMS) * time.Millisecond,
		idleShrink:  time.Duration(config.CaptchaPoolIdleShrinkSec) * time.Second,
		idleStop:    time.Duration(config.CaptchaPoolIdleStopSec) * time.Second,
		cpuGovernor: config.CaptchaCPUGovernorEnabled,
	}
}

func (o tokenPoolOptions) normalized() tokenPoolOptions {
	def := defaultTokenPoolOptions()
	if o.ttl <= 0 {
		o.ttl = def.ttl
	}
	if o.min < 1 {
		o.min = def.min
	}
	if o.max < o.min {
		o.max = o.min
	}
	if o.emptySolve < 1 {
		o.emptySolve = def.emptySolve
	}
	if o.emptySolve > 2 {
		o.emptySolve = 2
	}
	if o.refillEvery <= 0 {
		o.refillEvery = def.refillEvery
	}
	if o.idleShrink <= 0 {
		o.idleShrink = def.idleShrink
	}
	if o.idleStop <= o.idleShrink {
		o.idleStop = def.idleStop
	}
	return o
}

// tokenPool 预解 token 池。零值不可用，经 newTokenPool 构造。
type tokenPool struct {
	opts   tokenPoolOptions
	now    func() time.Time
	mu     sync.Mutex
	solver Solver

	closed     bool
	stopCh     chan struct{}
	mintCtx    context.Context
	mintCancel context.CancelFunc
	wg         sync.WaitGroup

	cfg    Config
	key    string
	active bool

	buckets   map[string][]poolToken
	issued    map[string]time.Time // dedupe key -> 可再次 mint 的时间
	inflight  map[string]int
	lastErr   map[string]error
	target    int
	lastTake  time.Time
	lastEmpty time.Time
	lastDecay time.Time
	idleLog   bool

	roundActive bool
	roundKey    string
	roundErr    error

	waitCh      chan struct{}
	waitSamples []int64 // 成功/失败 Take 的等待毫秒数，容量有界

	takeTotal      int64
	emptyTakeTotal int64
	mintTotal      int64
	mintFailTotal  int64

	governor *cpuGovernor
}

// newTokenPool 创建池并启动 refill 循环；配置由 SetConfig 注入。
func newTokenPool(solver Solver, opts tokenPoolOptions, now func() time.Time) *tokenPool {
	opts = opts.normalized()
	if now == nil {
		now = time.Now
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &tokenPool{
		opts:       opts,
		now:        now,
		solver:     solver,
		stopCh:     make(chan struct{}),
		mintCtx:    ctx,
		mintCancel: cancel,
		buckets:    make(map[string][]poolToken),
		issued:     make(map[string]time.Time),
		inflight:   make(map[string]int),
		lastErr:    make(map[string]error),
		target:     opts.min,
		lastTake:   now(),
		lastDecay:  now(),
		waitCh:     make(chan struct{}),
	}
	if opts.cpuGovernor {
		p.governor = newCPUGovernor(nil)
	}
	p.wg.Add(1)
	go p.loop()
	web.Ok("captcha", fmt.Sprintf(
		"token 池已启用：ttl=%s min=%d max=%d empty_solve=%d refill=%s",
		opts.ttl, opts.min, opts.max, opts.emptySolve, opts.refillEvery))
	return p
}

// SetSolver 更新求解器（Manager 先 Enable 后 SetSolver 的兼容路径）。
func (p *tokenPool) SetSolver(s Solver) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.solver = s
	p.mu.Unlock()
}

// SetNow 注入时钟（测试用）。
func (p *tokenPool) SetNow(fn func() time.Time) {
	if p == nil || fn == nil {
		return
	}
	p.mu.Lock()
	p.now = fn
	p.mu.Unlock()
}

// Ready 报告池是否已绑定指定配置并可服务 Take。
func (p *tokenPool) Ready(cfg Config) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.closed && p.active && p.solver != nil && p.key == captchaConfigKey(cfg)
}

// SetConfig 绑定/刷新当前配置；verified=false（配置接口失败无 last-good）时停用池。
// 配置键变化会清空所有旧桶与 CertifyIdRegistry，避免跨 scene/region/prefix 复用。
func (p *tokenPool) SetConfig(cfg Config, verified bool) {
	if p == nil {
		return
	}
	key := captchaConfigKey(cfg)
	now := p.now()
	var logMsg string
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	changed := key != p.key
	if changed {
		p.buckets = make(map[string][]poolToken)
		p.issued = make(map[string]time.Time)
		p.target = p.opts.min
		p.idleLog = false
		p.lastTake = now
		p.lastDecay = now
		// 旧配置的池空轮次作废；旧 mint 完成后只按 inflight 归零，不再影响新配置。
		p.roundActive = false
		p.roundErr = nil
	}
	p.key = key
	p.cfg = cfg
	p.active = verified && cfg.Enabled
	if !p.active {
		p.buckets = make(map[string][]poolToken)
		p.issued = make(map[string]time.Time)
	}
	if changed && verified {
		logMsg = fmt.Sprintf("验证码配置变化，已清空 token 池（scene=%s region=%s prefix=%s）",
			cfg.SceneID, cfg.Region, cfg.Prefix)
	}
	p.mu.Unlock()
	if logMsg != "" {
		web.Ok("captcha", logMsg)
	}
}

// Clear 清空所有桶（配置切换或人工失效用）。
func (p *tokenPool) Clear() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.buckets = make(map[string][]poolToken)
	p.issued = make(map[string]time.Time)
	p.mu.Unlock()
}

// Take 取一条未过期 token；池空时发起有界并发求解并等待最先成功的结果。
func (p *tokenPool) Take(ctx context.Context, cfg Config) (*Token, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	start := time.Now()
	key := captchaConfigKey(cfg)
	p.mu.Lock()
	if p.closed {
		stats := TakeStats{Valid: true, Wait: time.Since(start)}
		p.mu.Unlock()
		return nil, &TakeError{Err: ErrPoolClosed, Stats: stats}
	}
	if !p.active || p.key != key || p.solver == nil {
		stats := TakeStats{Valid: true, Wait: time.Since(start), PoolSize: p.poolSizeLocked(key)}
		p.mu.Unlock()
		return nil, &TakeError{Err: ErrUnavailable, Stats: stats}
	}
	now := p.now()
	p.pruneLocked(now)
	p.takeTotal++
	p.lastTake = now
	if tok, ok := p.popLocked(key); ok {
		stats := p.finishTakeLocked(start, false, 0)
		p.mu.Unlock()
		return tokenFromPool(tok, stats), nil
	}
	p.emptyTakeTotal++
	p.lastEmpty = now
	p.bumpTargetLocked()
	mintFailStart := p.mintFailTotal
	waitCh := p.waitCh
	p.mu.Unlock()

	web.Warn("captcha", fmt.Sprintf("token 池为空，触发并发求解（captcha_empty_take=%d）", p.emptyCount()))

	startedRound := false
	for {
		p.mu.Lock()
		if p.closed {
			stats := p.finishTakeLocked(start, true, mintFailStart)
			p.mu.Unlock()
			return nil, &TakeError{Err: ErrPoolClosed, Stats: stats}
		}
		p.pruneLocked(p.now())
		if tok, ok := p.popLocked(key); ok {
			stats := p.finishTakeLocked(start, true, mintFailStart)
			p.mu.Unlock()
			return tokenFromPool(tok, stats), nil
		}
		if p.roundActive && p.roundKey == key {
			waitCh = p.waitCh
			p.mu.Unlock()
			select {
			case <-waitCh:
				continue
			case <-ctx.Done():
				return nil, p.cancelTake(start, mintFailStart, ctx.Err())
			case <-p.stopCh:
				return nil, p.cancelTake(start, mintFailStart, ErrPoolClosed)
			}
		}
		if !startedRound && p.inflight[key] > 0 {
			// 后台 refill 已在补货：先等它，避免同一个池再叠 2 路求解。
			waitCh = p.waitCh
			p.mu.Unlock()
			select {
			case <-waitCh:
				continue
			case <-ctx.Done():
				return nil, p.cancelTake(start, mintFailStart, ctx.Err())
			case <-p.stopCh:
				return nil, p.cancelTake(start, mintFailStart, ErrPoolClosed)
			}
		}
		if !startedRound {
			startedRound = true
			p.startEmptyRoundLocked()
			waitCh = p.waitCh
			p.mu.Unlock()
			continue
		}
		// 本轮已结束且没有 token：返回本轮错误（可能为空池/求解失败）。
		err := p.roundErr
		if err == nil {
			err = ErrUnavailable
		}
		stats := p.finishTakeLocked(start, true, mintFailStart)
		p.mu.Unlock()
		return nil, &TakeError{Err: fmt.Errorf("%w: %s", ErrUnavailable, redact(err.Error())), Stats: stats}
	}
}

func (p *tokenPool) cancelTake(start time.Time, mintFailStart int64, err error) error {
	p.mu.Lock()
	stats := p.finishTakeLocked(start, true, mintFailStart)
	p.mu.Unlock()
	return &TakeError{Err: err, Stats: stats}
}

// finishTakeLocked 记录等待分布并生成本次 Take 的诊断（调用方持锁）。
func (p *tokenPool) finishTakeLocked(start time.Time, empty bool, mintFailStart int64) TakeStats {
	wait := time.Since(start)
	p.recordWaitLocked(wait)
	return TakeStats{
		Valid:     true,
		Wait:      wait,
		PoolSize:  p.poolSizeLocked(p.key),
		EmptyTake: empty,
		MintFail:  int(p.mintFailTotal - mintFailStart),
	}
}

// recordWaitLocked 保留最近 4096 次等待样本，供 p50/p95 快照使用。
func (p *tokenPool) recordWaitLocked(wait time.Duration) {
	p.waitSamples = append(p.waitSamples, wait.Milliseconds())
	if len(p.waitSamples) > 4096 {
		p.waitSamples = append([]int64(nil), p.waitSamples[len(p.waitSamples)-2048:]...)
	}
}

func (p *tokenPool) emptyCount() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.emptyTakeTotal
}

// startEmptyRoundLocked 发起一轮池空竞速（最多 2 路）；已有轮次时不重复发起。
func (p *tokenPool) startEmptyRoundLocked() {
	if p.roundActive {
		return
	}
	p.roundActive = true
	p.roundKey = p.key
	p.roundErr = nil
	if p.solver == nil {
		p.roundActive = false
		p.roundErr = ErrUnavailable
		p.signalLocked()
		return
	}
	p.launchMintsLocked(p.cfg, p.key, p.opts.emptySolve)
}

// launchMintsLocked 启动 n 个后台 mint；结果由 mint 完成后写入桶并广播。
func (p *tokenPool) launchMintsLocked(cfg Config, key string, n int) {
	if n <= 0 || p.solver == nil || p.closed {
		return
	}
	for i := 0; i < n; i++ {
		p.inflight[key]++
		p.mintTotal++
		p.wg.Add(1)
		go p.mint(cfg, key)
	}
}

// mint 执行一次后台求解；成功则入桶，失败只计数，不阻塞 refill 循环。
func (p *tokenPool) mint(cfg Config, key string) {
	defer p.wg.Done()
	param, err := p.solver.Solve(p.mintCtx, cfg)
	now := p.now()
	var logMsg string
	p.mu.Lock()
	p.inflight[key]--
	if p.inflight[key] <= 0 {
		delete(p.inflight, key)
	}
	switch {
	case err != nil:
		p.mintFailTotal++
		p.lastErr[key] = err
		logMsg = fmt.Sprintf("token 补解失败 captcha_mint_fail=%d: %s", p.mintFailTotal, redact(err.Error()))
	case strings.TrimSpace(param) == "":
		p.mintFailTotal++
		p.lastErr[key] = errors.New("求解器返回空 token")
		logMsg = fmt.Sprintf("token 补解失败 captcha_mint_fail=%d: 空结果", p.mintFailTotal)
	case p.key == key && p.active:
		tok := poolToken{
			param:     strings.TrimSpace(param),
			region:    cfg.Region,
			certifyID: extractCertifyID(param),
			mintedAt:  now,
			expiresAt: now.Add(p.opts.ttl),
		}
		dedupe := tokenDedupeKey(tok.param, tok.certifyID)
		if exp, exists := p.issued[dedupe]; exists && now.Before(exp) {
			p.mintFailTotal++
			p.lastErr[key] = errors.New("重复 certifyId/token")
			logMsg = fmt.Sprintf("token 补解失败 captcha_mint_fail=%d: 重复 certifyId/token", p.mintFailTotal)
			break
		}
		p.issued[dedupe] = now.Add(p.opts.ttl + time.Minute)
		p.buckets[key] = append(p.buckets[key], tok)
		if len(p.buckets[key]) > p.opts.max {
			p.buckets[key] = p.buckets[key][len(p.buckets[key])-p.opts.max:]
		}
		p.lastErr[key] = nil
	default:
		// 配置已变化：本次结果丢弃，不跨配置复用。
	}
	if p.roundActive && p.roundKey == key && p.inflight[key] == 0 {
		p.roundActive = false
		p.roundErr = p.lastErr[key]
	}
	p.signalLocked()
	p.mu.Unlock()
	if logMsg != "" {
		web.Warn("captcha", logMsg)
	}
}

// popLocked 取出桶中最早的一条未过期 token（一次性语义：取走即删除）。
func (p *tokenPool) popLocked(key string) (poolToken, bool) {
	list := p.buckets[key]
	for len(list) > 0 {
		tok := list[0]
		list = list[1:]
		if p.now().Before(tok.expiresAt) {
			p.buckets[key] = list
			// 续期 dedupe 窗口，确保同一 token 不会在短窗口内被二次交付。
			p.issued[tokenDedupeKey(tok.param, tok.certifyID)] = p.now().Add(p.opts.ttl + time.Minute)
			return tok, true
		}
	}
	p.buckets[key] = nil
	return poolToken{}, false
}

func (p *tokenPool) poolSizeLocked(key string) int {
	p.pruneLocked(p.now())
	return len(p.buckets[key])
}

func (p *tokenPool) pruneLocked(now time.Time) {
	for key, list := range p.buckets {
		kept := list[:0]
		for _, tok := range list {
			if now.Before(tok.expiresAt) {
				kept = append(kept, tok)
			}
		}
		if len(kept) == 0 {
			delete(p.buckets, key)
			continue
		}
		p.buckets[key] = kept
	}
	for key, exp := range p.issued {
		if !now.Before(exp) {
			delete(p.issued, key)
		}
	}
}

func (p *tokenPool) bumpTargetLocked() {
	if p.target < p.opts.max {
		p.target++
	}
}

func (p *tokenPool) signalLocked() {
	close(p.waitCh)
	p.waitCh = make(chan struct{})
}

// loop refill 主循环；Close 关闭 stopCh 后退出。
func (p *tokenPool) loop() {
	defer p.wg.Done()
	ticker := time.NewTicker(p.opts.refillEvery)
	defer ticker.Stop()
	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.refillOnce(p.now())
		}
	}
}

// refillOnce 执行一次水位检查；测试可直接调用以跳过 ticker。
func (p *tokenPool) refillOnce(now time.Time) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.governor != nil {
		p.governor.Update()
	}
	if p.closed || !p.active || p.solver == nil {
		p.mu.Unlock()
		return
	}
	p.pruneLocked(now)
	idle := now.Sub(p.lastTake)
	target := p.target
	deepIdle := false
	switch {
	case idle > p.opts.idleStop:
		target = 1
		deepIdle = true
		if !p.idleLog {
			p.idleLog = true
			web.Ok("captcha", "token 池空闲停补（保留 1 条 token 唤醒）")
		}
	case idle > p.opts.idleShrink:
		target = 1
		if !p.idleLog {
			p.idleLog = true
			web.Ok("captcha", "token 池空闲缩容至 1")
		}
	default:
		if p.idleLog {
			p.idleLog = false
			web.Ok("captcha", "token 池恢复补货")
		}
	}
	factor := 1.0
	if p.governor != nil {
		factor = p.governor.Factor()
	}
	target = int(math.Round(float64(target) * factor))
	if target < 1 {
		target = 1
	}
	concurrency := int(math.Ceil(float64(p.opts.emptySolve) * factor))
	if concurrency < 1 {
		concurrency = 1
	}
	size := len(p.buckets[p.key])
	inflight := p.inflight[p.key]
	need := target - size - inflight
	if deepIdle && size >= 1 {
		need = 0
	}
	if need > 0 && !p.roundActive {
		if need > concurrency {
			need = concurrency
		}
		p.launchMintsLocked(p.cfg, p.key, need)
	}
	if now.Sub(p.lastDecay) >= time.Minute {
		if now.Sub(p.lastEmpty) >= time.Minute && p.target > p.opts.min {
			p.target--
		}
		p.lastDecay = now
	}
	p.mu.Unlock()
}

// Stats 返回当前池指标快照。
func (p *tokenPool) Stats() TokenPoolStats {
	if p == nil {
		return TokenPoolStats{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	p.pruneLocked(now)
	p50, p95 := waitPercentiles(p.waitSamples)
	inflight := p.inflight[p.key]
	idle := now.Sub(p.lastTake).Seconds()
	if idle < 0 {
		idle = 0
	}
	cpu := 0.0
	if p.governor != nil {
		cpu = p.governor.Percent()
	}
	return TokenPoolStats{
		Enabled:         !p.closed && p.active,
		PoolSize:        len(p.buckets[p.key]),
		TakeTotal:       p.takeTotal,
		EmptyTakeTotal:  p.emptyTakeTotal,
		MintTotal:       p.mintTotal,
		MintFailTotal:   p.mintFailTotal,
		WaitP50MS:       p50,
		WaitP95MS:       p95,
		Inflight:        inflight,
		Target:          p.target,
		IdleSeconds:     idle,
		CPUPercent:      cpu,
		WaitSampleCount: len(p.waitSamples),
	}
}

// Close 停止 refill 与在途 mint；可重复调用。
func (p *tokenPool) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.active = false
	p.buckets = make(map[string][]poolToken)
	p.issued = make(map[string]time.Time)
	close(p.stopCh)
	p.mintCancel()
	p.signalLocked()
	p.mu.Unlock()

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		web.Warn("captcha", "token 池关闭超时，放弃等待在途求解")
	}
	stats := p.Stats()
	web.Ok("captcha", fmt.Sprintf(
		"token 池已停止：captcha_pool_size=%d take=%d empty_take=%d mint=%d mint_fail=%d wait_p50_ms=%.1f wait_p95_ms=%.1f",
		stats.PoolSize, stats.TakeTotal, stats.EmptyTakeTotal, stats.MintTotal, stats.MintFailTotal,
		stats.WaitP50MS, stats.WaitP95MS))
	return nil
}

func tokenFromPool(tok poolToken, stats TakeStats) *Token {
	return &Token{
		VerifyParam: tok.param,
		Region:      tok.region,
		TakeStats:   stats,
	}
}

func captchaConfigKey(cfg Config) string {
	return strings.Join([]string{
		strings.TrimSpace(cfg.SceneID),
		strings.TrimSpace(cfg.Region),
		strings.TrimSpace(cfg.Prefix),
	}, "|")
}

// tokenDedupeKey 优先按 certifyId 去重；解析不出时退回 token 原文摘要。
func tokenDedupeKey(param, certifyID string) string {
	if certifyID != "" {
		return "certify:" + certifyID
	}
	sum := sha256.Sum256([]byte(param))
	return "param:" + hex.EncodeToString(sum[:16])
}

// extractCertifyID 从 verify_param 中提取 certifyId。
// 实测形态是 base64(JSON{certifyId,...})；这里兼容 JSON 原文与 URL 编码，
// 解析失败时返回空串（此时退化为按 token 原文去重）。
func extractCertifyID(param string) string {
	raw := strings.TrimSpace(param)
	candidates := []string{raw}
	if unescaped, err := url.QueryUnescape(raw); err == nil && unescaped != raw {
		candidates = append(candidates, unescaped)
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
	} {
		if decoded, err := enc.DecodeString(raw); err == nil {
			candidates = append(candidates, string(decoded))
		}
	}
	for _, candidate := range candidates {
		var obj struct {
			CertifyID string `json:"certifyId"`
		}
		if err := json.Unmarshal([]byte(candidate), &obj); err == nil && strings.TrimSpace(obj.CertifyID) != "" {
			return strings.TrimSpace(obj.CertifyID)
		}
	}
	return ""
}

// waitPercentiles 返回等待样本的 p50/p95（毫秒）；空样本返回 0。
func waitPercentiles(samples []int64) (float64, float64) {
	if len(samples) == 0 {
		return 0, 0
	}
	vals := append([]int64(nil), samples...)
	sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
	index := func(percent int) int {
		// nearest-rank：ceil(percent/100 * n) - 1，n=1 时返回 0。
		i := (len(vals)*percent+99)/100 - 1
		if i < 0 {
			return 0
		}
		if i >= len(vals) {
			return len(vals) - 1
		}
		return i
	}
	return float64(vals[index(50)]), float64(vals[index(95)])
}
