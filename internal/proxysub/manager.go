package proxysub

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"zcode2api/internal/config"
)

type Manager struct {
	repo            Repository
	ctx             context.Context
	stop            context.CancelFunc
	mu              sync.Mutex
	wg              sync.WaitGroup
	started, closed bool
	current         *Run
	last            *Run
	cancel          context.CancelFunc
	next            int64
	// 测试通过这些内部接缝连接受控 HTTP/代理，不提供生产绕过开关。
	fetch func(context.Context, string) ([]byte, error)
	probe func(context.Context, Candidate) Detail
	now   func() time.Time
}

func New(repo Repository, parents ...context.Context) *Manager {
	parent := context.Background()
	if len(parents) > 0 {
		parent = parents[0]
	}
	ctx, cancel := context.WithCancel(parent)
	return &Manager{repo: repo, ctx: ctx, stop: cancel, fetch: fetch, probe: probe, now: time.Now}
}

func (m *Manager) Start() {
	m.mu.Lock()
	if m.closed || m.started {
		m.mu.Unlock()
		return
	}
	if m.current == nil {
		if err := m.repo.RecoverSubscriptionRuns(); err != nil {
			m.mu.Unlock()
			return
		}
	}
	m.started = true
	m.wg.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.wg.Done()
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-m.ctx.Done():
				return
			case <-ticker.C:
				m.tick()
			}
		}
	}()
}

func (m *Manager) tick() {
	m.mu.Lock()
	s, err := m.repo.SubscriptionSnapshot()
	if err != nil || !s.State.Config.Auto || m.closed {
		m.mu.Unlock()
		return
	}
	if m.next == 0 {
		m.next = m.now().Add(time.Duration(s.State.Config.IntervalMinutes) * time.Minute).UnixMilli()
	}
	due := m.current == nil && m.now().UnixMilli() >= m.next
	m.mu.Unlock()
	if due {
		_, _ = m.Launch("import", true)
	}
}

func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	m.stop()
	if m.cancel != nil {
		m.cancel()
	}
	m.mu.Unlock()
	m.wg.Wait()
}

func cloneRun(r Run) Run { r.Details = append([]Detail{}, r.Details...); return r }

func (m *Manager) View() (map[string]any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.repo.SubscriptionSnapshot()
	if err != nil {
		return nil, ErrState
	}
	var current *Run
	if m.current != nil {
		copy := cloneRun(*m.current)
		current = &copy
	}
	var last *Run
	if m.last != nil {
		copy := cloneRun(*m.last)
		last = &copy
	}
	return map[string]any{"config": s.State.Config.Public(), "current": current, "last_result": last, "runs": s.State.Runs,
		"owned": s.OwnedCount(), "next_run_at": m.next, "import_paused": s.State.ImportPaused, "target_supported": TargetSupported()}, nil
}

func (m *Manager) Configure(c Config) (Config, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Config{}, errors.New("服务正在停止")
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	if c.Auto && !TargetSupported() {
		return Config{}, errors.New("当前上游 origin 不适用默认 ZCode 入库探针")
	}
	saved, err := m.repo.SaveSubscriptionConfig(c)
	if err != nil {
		if errors.Is(err, ErrConflict) {
			return Config{}, err
		}
		return Config{}, ErrState
	}
	if m.cancel != nil {
		m.cancel()
	}
	m.next = 0
	if saved.Auto {
		m.next = m.now().Add(time.Duration(saved.IntervalMinutes) * time.Minute).UnixMilli()
	}
	return saved, nil
}

func (m *Manager) Launch(mode string, automatic bool) (Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.ctx.Err() != nil {
		return Run{}, errors.New("服务正在停止")
	}
	if m.current != nil {
		return cloneRun(*m.current), ErrBusy
	}
	if mode != "preview" && mode != "import" {
		return Run{}, errors.New("任务模式必须是 preview 或 import")
	}
	s, err := m.repo.SubscriptionSnapshot()
	if err != nil {
		return Run{}, ErrState
	}
	c := s.State.Config
	if err = c.Validate(); err != nil {
		return Run{}, err
	}
	if c.URL == "" {
		return Run{}, errors.New("请先保存订阅地址")
	}
	if automatic && !c.Auto {
		return Run{}, errors.New("自动同步已关闭")
	}
	if !TargetSupported() {
		return Run{}, errors.New("当前上游 origin 不适用默认 ZCode 入库探针")
	}
	if s.State.ImportPaused && mode == "import" {
		return Run{}, errors.New("禁止记录达到预算，已闭锁导入，请检查订阅状态")
	}
	r := Run{ID: config.NewUUIDv4(), Revision: c.Revision, Mode: mode, Automatic: automatic, Status: "fetching", StartedAt: m.now().UnixMilli(), Details: []Detail{}}
	if err = m.repo.BeginSubscriptionRun(r); err != nil {
		return Run{}, ErrState
	}
	ctx, cancel := context.WithTimeout(m.ctx, time.Duration(c.BudgetSeconds)*time.Second)
	m.current = &r
	m.cancel = cancel
	m.wg.Add(1)
	go m.execute(ctx, c, s, cloneRun(r))
	return cloneRun(r), nil
}

func (m *Manager) Cancel(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current != nil && m.current.ID == id {
		m.cancel()
		return true
	}
	s, err := m.repo.SubscriptionSnapshot()
	if err == nil {
		for _, r := range s.State.Runs {
			if r.ID == id {
				return true
			}
		}
	}
	return false
}

func (m *Manager) progress(r Run) {
	m.mu.Lock()
	if m.current != nil && m.current.ID == r.ID {
		copy := cloneRun(r)
		m.current = &copy
	}
	m.mu.Unlock()
}

func (m *Manager) execute(ctx context.Context, c Config, s Snapshot, r Run) {
	defer m.wg.Done()
	accepted := []Candidate{}
	var runErr error
	// 所有出口共用收尾；提交与取消在 m.mu 上排序，终态与实际事务一致。
	defer func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.cancel != nil {
			defer m.cancel()
		}
		r.FinishedAt = m.now().UnixMilli()
		r.Status = "completed"
		if ctx.Err() != nil {
			r.Status = "canceled"
			r.Error = "任务已取消"
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				r.Status = "failed"
				r.Error = "任务超过总预算"
			}
		} else if runErr != nil {
			r.Status = "failed"
			r.Error = runErr.Error()
		}
		if r.Status != "completed" || r.Mode == "preview" {
			accepted = nil
		}
		// 取消的任务也需要持久化终态；导入路径仍使用原 context 在提交前再次检查。
		saveCtx := ctx
		if r.Status != "completed" || r.Mode == "preview" {
			saveCtx = context.Background()
		}
		saved, err := m.repo.FinishSubscriptionRun(saveCtx, r, accepted)
		if err != nil {
			r.Status = "failed"
			r.Error = "保存订阅任务失败，未导入线路"
			r.Added = 0
			if errors.Is(err, ErrConflict) {
				r.Status = "canceled"
				r.Error = "订阅配置已变化，未导入线路"
			}
			_, retryErr := m.repo.FinishSubscriptionRun(context.Background(), r, nil)
			if retryErr != nil {
				copy := cloneRun(r)
				m.current = &copy
			} else {
				m.current = nil
			}
		} else {
			r = saved
			m.current = nil
		}
		// 持久化异常时保留可见终态，不阻塞下一次手动重试。
		copy := cloneRun(r)
		m.last = &copy
		if m.current != nil && m.current.Terminal() {
			m.current = nil
		}
		m.cancel = nil
		m.next = m.now().Add(time.Duration(c.IntervalMinutes) * time.Minute).UnixMilli()
		if snap, e := m.repo.SubscriptionSnapshot(); e == nil {
			if !snap.State.Config.Auto {
				m.next = 0
			} else {
				m.next = m.now().Add(time.Duration(snap.State.Config.IntervalMinutes) * time.Minute).UnixMilli()
			}
		}
	}()

	raw, err := m.fetch(ctx, c.URL)
	if err != nil {
		runErr = err
		return
	}
	r.Status = "filtering"
	m.progress(r)
	all, counts, err := Parse(raw, c, m.now())
	if err != nil {
		runErr = err
		return
	}
	r.Received, r.Rejected, r.Expired, r.Existing = counts.Received, counts.Rejected, counts.Expired, counts.Existing
	existing := map[string]bool{}
	for _, p := range s.Profiles {
		existing[Fingerprint(p.URL)] = true
	}
	recent := map[string]int64{}
	for _, old := range s.State.Runs {
		if old.Mode != "import" || old.Status != "completed" {
			continue
		}
		for _, d := range old.Details {
			if old.FinishedAt > recent[d.Key] {
				recent[d.Key] = old.FinishedAt
			}
		}
	}
	candidates := []Candidate{}
	for _, a := range all {
		if existing[a.Key] {
			r.Existing++
			continue
		}
		if b, ok := s.State.Blocks[a.Key]; ok && b.Active(m.now()) {
			r.Blocked++
			continue
		}
		candidates = append(candidates, a)
	}
	sort.SliceStable(candidates, func(i, j int) bool { return recent[candidates[i].Key] < recent[candidates[j].Key] })
	if len(candidates) > c.Candidates {
		candidates = candidates[:c.Candidates]
	}
	r.Candidates = len(candidates)
	need := min(c.ImportLimit, max(0, c.Capacity-s.OwnedCount()))
	if need == 0 {
		m.progress(r)
		return
	}
	r.Status = "probing"
	m.progress(r)
	type probeResult struct {
		candidate Candidate
		detail    Detail
	}
	jobs := make(chan Candidate)
	results := make(chan probeResult, c.Concurrency)
	var workers sync.WaitGroup
	for range c.Concurrency {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for a := range jobs {
				d := m.probe(ctx, a)
				results <- probeResult{a, d}
			}
		}()
	}
	next, inflight := 0, 0
	done := ctx.Done()
	for (next < len(candidates) && len(accepted) < need && ctx.Err() == nil) || inflight > 0 {
		var send chan Candidate
		var a Candidate
		if next < len(candidates) && len(accepted) < need && ctx.Err() == nil && inflight < c.Concurrency {
			send = jobs
			a = candidates[next]
		}
		select {
		case send <- a:
			next++
			inflight++
		case result := <-results:
			inflight--
			r.Probed++
			if result.detail.OK {
				r.Passed++
				result.candidate.ProbedAt = m.now().UnixMilli()
				accepted = append(accepted, result.candidate)
			}
			if len(r.Details) < 50 {
				r.Details = append(r.Details, result.detail)
			}
			m.progress(r)
		case <-done:
			// 只消费一次取消信号，之后排空在途结果，避免忙循环。
			done = nil
		}
	}
	close(jobs)
	workers.Wait()
	r.Status = "committing"
	m.progress(r)
}
