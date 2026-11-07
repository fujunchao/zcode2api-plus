package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"zcode2api/internal/config"
)

type prebufferReturnRecorder struct {
	*httptest.ResponseRecorder
	returned atomic.Bool
	late     atomic.Int32
}

func (w *prebufferReturnRecorder) WriteHeader(status int) {
	if w.returned.Load() {
		w.late.Add(1)
	}
	w.ResponseRecorder.WriteHeader(status)
}

func (w *prebufferReturnRecorder) Write(p []byte) (int, error) {
	if w.returned.Load() {
		w.late.Add(1)
	}
	return w.ResponseRecorder.Write(p)
}

// 使用实际 teeReader 的业务错误判定，以及虚拟时钟检查返回后的定时器生命周期。
func TestPrebufferPreservesEarlyUpstreamError(t *testing.T) {
	old := config.TruncatePrebufferBudget
	config.TruncatePrebufferBudget = time.Second
	t.Cleanup(func() { config.TruncatePrebufferBudget = old })
	synctest.Test(t, func(t *testing.T) {
		body := sseStart + sseFrame("error", map[string]any{
			"type": "error", "error": map[string]any{"type": "overloaded_error", "message": "synthetic overload"},
		})
		w := &prebufferReturnRecorder{ResponseRecorder: httptest.NewRecorder()}
		err := passthroughDeliver(w, Delivery{
			StatusCode: http.StatusOK, ContentType: "text/event-stream",
			Body: &teeReader{r: strings.NewReader(body), c: NewUsageCollector(true)},
		})
		w.returned.Store(true)
		if !IsUpstreamErrorEvent(err) {
			t.Errorf("应保留上游业务错误类型，实际 %v", err)
		}
		if w.Code != http.StatusOK || w.Body.String() != body {
			t.Errorf("返回前必须交付完整错误帧，实际 status=%d body=%q", w.Code, w.Body.String())
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if got := w.late.Load(); got != 0 {
			t.Errorf("交付函数返回后仍发生 %d 次响应写入", got)
		}
	})
}
