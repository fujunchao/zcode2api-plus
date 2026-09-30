package requeststats

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestRequestAccountingIncludesFailuresAndRetries(t *testing.T) {
	tracker := New()
	if tracker.Snapshot().SuccessRate != nil {
		t.Fatal("没有已完成请求时成功率必须为 null")
	}
	success := tracker.Wrap(func(w http.ResponseWriter, r *http.Request) {
		Attempt(r.Context())
		Attempt(r.Context())
		w.WriteHeader(200)
	})
	failed := tracker.Wrap(func(w http.ResponseWriter, r *http.Request) { Fail(r.Context()); w.WriteHeader(200) })
	success(httptest.NewRecorder(), httptest.NewRequest("POST", "/", nil))
	failed(httptest.NewRecorder(), httptest.NewRequest("POST", "/", nil))
	got := tracker.Snapshot()
	if got.Total != 2 || got.Succeeded != 1 || got.Failed != 1 || got.Active != 0 || got.UpstreamAttempts != 2 || got.Retries != 1 || *got.SuccessRate != 50 {
		t.Fatalf("错误的请求口径：%+v", got)
	}
}

func TestCanceledAndConcurrentRequests(t *testing.T) {
	tracker := New()
	handler := tracker.Wrap(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	handler(httptest.NewRecorder(), httptest.NewRequest("POST", "/", nil).WithContext(ctx))
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			handler(httptest.NewRecorder(), httptest.NewRequest("POST", "/", nil))
			_ = tracker.Snapshot()
		}()
	}
	wg.Wait()
	got := tracker.Snapshot()
	if got.Total != 51 || got.Succeeded != 50 || got.Failed != 1 || got.Canceled != 1 || got.Active != 0 {
		t.Fatalf("并发/取消计数错误：%+v", got)
	}
}

func TestInflightAndPanicDoNotAppearSuccessful(t *testing.T) {
	tracker := New()
	handler := tracker.Wrap(func(w http.ResponseWriter, r *http.Request) {
		got := tracker.Snapshot()
		if got.Active != 1 || got.SuccessRate != nil {
			t.Fatalf("在途请求不能参与成功率：%+v", got)
		}
		panic("test")
	})
	func() {
		defer func() { _ = recover() }()
		handler(httptest.NewRecorder(), httptest.NewRequest("POST", "/", nil))
	}()
	if got := tracker.Snapshot(); got.Failed != 1 || got.Active != 0 {
		t.Fatalf("panic 未结算：%+v", got)
	}
}
