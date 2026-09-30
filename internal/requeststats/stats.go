// Package requeststats 按完整模型 HTTP 请求统计，不混用持久化的账号尝试计数。
package requeststats

import (
	"context"
	"net/http"
	"sync"
	"time"
)

type Snapshot struct {
	Scope            string   `json:"scope"`
	StartedAt        float64  `json:"started_at"`
	Total            int64    `json:"total"`
	Succeeded        int64    `json:"succeeded"`
	Failed           int64    `json:"failed"`
	Errors           int64    `json:"errors"` // 兼容原 monitor 字段，与 failed 同值
	Canceled         int64    `json:"canceled"`
	Active           int64    `json:"active"`
	UpstreamAttempts int64    `json:"upstream_attempts"`
	Retries          int64    `json:"retries"`
	SuccessRate      *float64 `json:"success_rate"`
	AverageQPS       float64  `json:"average_qps"`
}

type Tracker struct {
	mu      sync.Mutex
	started time.Time
	value   Snapshot
}

func New() *Tracker { return &Tracker{started: time.Now()} }

func (t *Tracker) Snapshot() Snapshot {
	if t == nil {
		return Snapshot{Scope: "process"}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := t.value
	out.Scope = "process"
	out.StartedAt = float64(t.started.UnixNano()) / 1e9
	out.Errors = out.Failed
	if completed := out.Succeeded + out.Failed; completed > 0 {
		rate := 100 * float64(out.Succeeded) / float64(completed)
		out.SuccessRate = &rate
	}
	if seconds := time.Since(t.started).Seconds(); seconds > 0 {
		out.AverageQPS = float64(out.Total) / seconds
	}
	return out
}

type scopeKey struct{}
type scope struct {
	tracker      *Tracker
	attempts     int
	failed, done bool
}

// Attempt 只用于真正发出的模型上游 HTTP 尝试，不统计验证码和计费查询。
func Attempt(ctx context.Context) {
	s, _ := ctx.Value(scopeKey{}).(*scope)
	if s == nil {
		return
	}
	t := s.tracker
	t.mu.Lock()
	defer t.mu.Unlock()
	if s.done {
		return
	}
	t.value.UpstreamAttempts++
	if s.attempts > 0 {
		t.value.Retries++
	}
	s.attempts++
}

// Fail 标记 HTTP 200 内承载的协议错误；最终只在 HTTP 请求退出时结算一次。
func Fail(ctx context.Context) {
	s, _ := ctx.Value(scopeKey{}).(*scope)
	if s == nil {
		return
	}
	s.tracker.mu.Lock()
	defer s.tracker.mu.Unlock()
	if !s.done {
		s.failed = true
	}
}

func (t *Tracker) Wrap(next http.HandlerFunc) http.HandlerFunc {
	if t == nil {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		t.mu.Lock()
		t.value.Total++
		t.value.Active++
		t.mu.Unlock()
		state := &scope{tracker: t}
		ctx := context.WithValue(r.Context(), scopeKey{}, state)
		writer := &responseWriter{ResponseWriter: w}
		returned := false
		defer func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			state.done = true
			t.value.Active--
			canceled := ctx.Err() != nil
			failed := !returned || state.failed || writer.writeFailed || canceled || writer.status >= 300
			if failed {
				t.value.Failed++
			} else {
				t.value.Succeeded++
			}
			if canceled {
				t.value.Canceled++
			}
		}()
		var output http.ResponseWriter = writer
		if f, ok := w.(http.Flusher); ok {
			output = &flushingWriter{responseWriter: writer, flusher: f}
		}
		next(output, r.WithContext(ctx))
		returned = true
	}
}

type responseWriter struct {
	http.ResponseWriter
	status      int
	writeFailed bool
}

func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *responseWriter) WriteHeader(status int) {
	if status >= 200 && w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *responseWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	if err != nil {
		w.writeFailed = true
	}
	return n, err
}

type flushingWriter struct {
	*responseWriter
	flusher http.Flusher
}

func (w *flushingWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	w.flusher.Flush()
}
