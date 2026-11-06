// worker 寿命回收与关闭兜底的用例。
//
// 背景：浏览器一旦拉起就活到进程退出（上游 Python 版同构），而成功路径刻意
// 复用同一页面，页面状态只增不减。真机实测单只 renderer 跑 3 天后
// RSS 174M→1509M、CPU 3.4%→30.7%（findings/browser-lifecycle-audit-20261106.md）。
// 这里验证：命中阈值后确实在两次求解之间重建、不打断在途请求，以及
// Close 卡死时看门狗会强制 Kill（否则会留下杀不掉的孤儿浏览器）。
package captcha

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recyclableWorker 在 scriptWorker 基础上实现 WorkerRecycler。
type recyclableWorker struct {
	*scriptWorker
	rMu          sync.Mutex
	solves       int
	recycleAfter int
}

func (w *recyclableWorker) Solve(ctx context.Context) (string, error) {
	w.rMu.Lock()
	w.solves++
	w.rMu.Unlock()
	return w.scriptWorker.Solve(ctx)
}

func (w *recyclableWorker) RecycleReason(time.Time) string {
	w.rMu.Lock()
	defer w.rMu.Unlock()
	if w.recycleAfter > 0 && w.solves >= w.recycleAfter {
		return "测试阈值"
	}
	return ""
}

// TestPoolRecyclesWorkerAtThreshold 达到阈值后必须重建 worker，
// 且重建计数要落到 Stats.WorkerRestarts 上（观测依赖它）。
func TestPoolRecyclesWorkerAtThreshold(t *testing.T) {
	var mu sync.Mutex
	created := 0
	factory := func(context.Context) (Worker, error) {
		mu.Lock()
		defer mu.Unlock()
		created++
		w := &recyclableWorker{scriptWorker: &scriptWorker{id: created}, recycleAfter: 1}
		return w, nil
	}
	p := newTestPool(t, factory, 1)
	if err := p.Start(); err != nil {
		t.Fatalf("启动失败: %v", err)
	}

	// 第一次求解：worker 达到阈值，本次结果照常返回，随后回收重建。
	got, err := p.Solve(context.Background())
	if err != nil || got == "" {
		t.Fatalf("首次求解应成功: %q %v", got, err)
	}
	waitFor(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return created == 2
	}, "达到阈值后应重建 worker")

	if n := p.StatsSnapshot().WorkerRestarts; n != 1 {
		t.Fatalf("应记录 1 次重建，实际 %d", n)
	}
	// 新 worker 上线后仍可服务。
	if got, err := p.Solve(context.Background()); err != nil || got == "" {
		t.Fatalf("重建后应能继续求解: %q %v", got, err)
	}
}

// hungCloseWorker 的 Close 永久阻塞，模拟浏览器假死（CDP 不回应）时
// rod 的 browser.Close / launcher.Cleanup 卡住不返回。
type hungCloseWorker struct {
	killCalls atomic.Int32
	release   chan struct{}
}

func (w *hungCloseWorker) Solve(context.Context) (string, error) { return "token", nil }

func (w *hungCloseWorker) Close() error {
	<-w.release
	return nil
}

func (w *hungCloseWorker) Kill() { w.killCalls.Add(1) }

// TestPoolKillsWorkerWhenCloseHangs 关闭超过看门狗时限必须补一刀 Kill。
//
// rod 的 Launcher.Cleanup 是「<-l.exit 等进程退出」再删目录，本身不杀进程；
// 不补 Kill 的话这只浏览器在进程内永生，而池随后重建一只，内存直接翻倍。
func TestPoolKillsWorkerWhenCloseHangs(t *testing.T) {
	w := &hungCloseWorker{release: make(chan struct{})}
	t.Cleanup(func() { close(w.release) })

	p := newTestPool(t, func(context.Context) (Worker, error) { return w, nil }, 1)
	p.closeWatchdog = 100 * time.Millisecond

	p.closeWorker(w)
	waitFor(t, 2*time.Second, func() bool { return w.killCalls.Load() == 1 }, "关闭超时应调用 Kill")
}

// TestRodWorkerRecycleReason 三个阈值分别命中；0 表示不限制。
func TestRodWorkerRecycleReason(t *testing.T) {
	now := time.Now()

	// 存活时长。
	w := &RodWorker{createdAt: now, maxAge: time.Hour}
	if reason := w.RecycleReason(now.Add(30 * time.Minute)); reason != "" {
		t.Fatalf("未到存活上限不应回收: %q", reason)
	}
	if reason := w.RecycleReason(now.Add(2 * time.Hour)); reason == "" {
		t.Fatal("超过存活上限应回收")
	}

	// 求解次数。
	w2 := &RodWorker{createdAt: now, maxSolves: 2}
	w2.solves = 1
	if reason := w2.RecycleReason(now); reason != "" {
		t.Fatalf("次数未到不应回收: %q", reason)
	}
	w2.solves = 2
	if reason := w2.RecycleReason(now); reason == "" {
		t.Fatal("次数到上限应回收")
	}

	// 全部为 0：不限制（保持旧行为，可 env 关掉）。
	w3 := &RodWorker{createdAt: now}
	if reason := w3.RecycleReason(now.Add(10000 * time.Hour)); reason != "" {
		t.Fatalf("阈值为 0 应不限制: %q", reason)
	}
}

// TestProcessRSSBytesReadsSelf 读自身 RSS；不支持 /proc 的平台跳过。
func TestProcessRSSBytesReadsSelf(t *testing.T) {
	rss, ok := processRSSBytes(os.Getpid())
	if !ok {
		t.Skip("当前平台不可读 /proc，跳过")
	}
	if rss <= 0 {
		t.Fatalf("自身 RSS 应为正数: %d", rss)
	}
	if _, ok := processRSSBytes(0); ok {
		t.Fatal("pid=0 应返回 false")
	}
	if _, ok := processRSSBytes(-1); ok {
		t.Fatal("负数 pid 应返回 false")
	}
}
