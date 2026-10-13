package proxysub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type memoryRepository struct {
	mu       sync.Mutex
	state    State
	profiles []Existing
	finished chan Run
	imported []Candidate
}

func (f *memoryRepository) SubscriptionSnapshot() (Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, _ := json.Marshal(f.state)
	var state State
	_ = json.Unmarshal(raw, &state)
	return Snapshot{State: state, Profiles: append([]Existing{}, f.profiles...)}, nil
}
func (f *memoryRepository) SaveSubscriptionConfig(c Config) (Config, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c.Revision != f.state.Config.Revision {
		return c, ErrConflict
	}
	c.Revision++
	f.state.Config = c
	return c, nil
}
func (f *memoryRepository) BeginSubscriptionRun(r Run) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state.Runs = append([]Run{r}, f.state.Runs...)
	return nil
}
func (f *memoryRepository) FinishSubscriptionRun(ctx context.Context, r Run, passed []Candidate) (Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Status == "completed" && (ctx.Err() != nil || r.Revision != f.state.Config.Revision) {
		return r, ErrConflict
	}
	if len(passed) > 0 {
		r.Added = min(len(passed), f.state.Config.ImportLimit)
		f.imported = append(f.imported, passed[:r.Added]...)
	}
	for i := range f.state.Runs {
		if f.state.Runs[i].ID == r.ID {
			f.state.Runs[i] = r
		}
	}
	f.finished <- r
	return r, nil
}
func (f *memoryRepository) RecoverSubscriptionRuns() error   { return nil }
func (f *memoryRepository) UnblockSubscription(string) error { return nil }

func testManager(t *testing.T, n int) (*Manager, *memoryRepository) {
	t.Helper()
	rows := []any{}
	for i := range n {
		rows = append(rows, fixture(fmt.Sprintf("http://8.8.8.8:%d", 8000+i), time.Now()))
	}
	raw, _ := json.Marshal(rows)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(raw) }))
	t.Cleanup(server.Close)
	repo := &memoryRepository{state: NewState(), finished: make(chan Run, 10)}
	repo.state.Config.URL = server.URL + "/synthetic-token"
	m := New(repo)
	t.Cleanup(m.Close)
	m.probe = func(ctx context.Context, c Candidate) Detail {
		return Detail{Key: c.Key, Endpoint: c.URL, OK: true, Reason: "合成通过"}
	}
	return m, repo
}

func awaitRun(t *testing.T, repo *memoryRepository) Run {
	t.Helper()
	select {
	case r := <-repo.finished:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("订阅任务未按时结束")
		return Run{}
	}
}

func TestSubscriptionManagerPreviewAndImport(t *testing.T) {
	m, repo := testManager(t, 20)
	for _, mode := range []string{"preview", "import"} {
		if _, err := m.Launch(mode, false); err != nil {
			t.Fatal(err)
		}
		r := awaitRun(t, repo)
		if r.Status != "completed" || r.Passed < 10 || r.Probed > 13 {
			t.Fatalf("工作队列未按需停止: %+v", r)
		}
		if mode == "preview" && r.Added != 0 {
			t.Fatal("预览产生写入")
		}
		if mode == "import" && r.Added != 10 {
			t.Fatal("未按新增上限导入")
		}
	}
}

func TestSubscriptionManagerCancelAndRevision(t *testing.T) {
	for _, change := range []string{"cancel", "config", "close"} {
		t.Run(change, func(t *testing.T) {
			m, repo := testManager(t, 20)
			entered := make(chan struct{}, 8)
			m.probe = func(ctx context.Context, c Candidate) Detail {
				entered <- struct{}{}
				<-ctx.Done()
				return Detail{Key: c.Key, Endpoint: c.URL, Reason: "取消"}
			}
			r, err := m.Launch("import", false)
			if err != nil {
				t.Fatal(err)
			}
			<-entered
			if _, err = m.Launch("preview", false); !errors.Is(err, ErrBusy) {
				t.Fatal("应拒绝重叠任务")
			}
			switch change {
			case "cancel":
				if !m.Cancel(r.ID) {
					t.Fatal("取消失败")
				}
			case "config":
				s, _ := repo.SubscriptionSnapshot()
				s.State.Config.IntervalMinutes = 15
				if _, err = m.Configure(s.State.Config); err != nil {
					t.Fatal(err)
				}
			case "close":
				m.Close()
			}
			r = awaitRun(t, repo)
			if r.Status != "canceled" || r.Added != 0 {
				t.Fatalf("取消后仍导入 %+v", r)
			}
		})
	}
}

func TestSubscriptionManagerWorkerBoundAndScheduling(t *testing.T) {
	m, repo := testManager(t, 20)
	repo.state.Config.Concurrency = 3
	var active, peak atomic.Int32
	m.probe = func(ctx context.Context, c Candidate) Detail {
		n := active.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(3 * time.Millisecond)
		active.Add(-1)
		return Detail{Key: c.Key, Endpoint: c.URL, OK: false, Reason: "合成失败"}
	}
	now := time.Now()
	m.now = func() time.Time { return now }
	m.tick()
	select {
	case <-repo.finished:
		t.Fatal("关闭时不应运行")
	default:
	}
	c := repo.state.Config
	c.Auto = true
	if _, err := m.Configure(c); err != nil {
		t.Fatal(err)
	}
	now = now.Add(11 * time.Minute)
	m.tick()
	r := awaitRun(t, repo)
	if r.Probed != 20 || peak.Load() > 3 || active.Load() != 0 {
		t.Fatalf("并发/收尾异常: %+v peak=%d", r, peak.Load())
	}
	m.tick()
	select {
	case <-repo.finished:
		t.Fatal("不应立即重叠排期")
	default:
	}
}

func TestSubscriptionManagerAdvancesCandidates(t *testing.T) {
	m, repo := testManager(t, 6)
	repo.state.Config.Candidates = 2
	repo.state.Config.ImportLimit = 2
	m.probe = func(ctx context.Context, c Candidate) Detail { return Detail{Key: c.Key, Reason: "失败"} }
	keys := map[string]bool{}
	for range 3 {
		if _, err := m.Launch("import", false); err != nil {
			t.Fatal(err)
		}
		r := awaitRun(t, repo)
		for _, d := range r.Details {
			if keys[d.Key] {
				t.Fatal("后续候选饥饿")
			}
			keys[d.Key] = true
		}
	}
	if len(keys) != 6 {
		t.Fatal("候选未推进")
	}
}
