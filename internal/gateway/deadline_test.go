package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

// 请求级预算用尽后必须在**下一次尝试之前**收手：不再选号、不再换号。
//
// 用假时钟把「第一次尝试之后」推到 deadline 之后，避免依赖真实等待：
// 上游第一次返回 503（会触发换号），此刻时钟已跳 2h，外层循环必须直接 503。
func TestRequestDeadlineStopsBeforeNextAttempt(t *testing.T) {
	f := newFixture(t)
	for _, name := range []string{"a", "b", "c"} {
		if _, err := f.st.AddAccount(model.ProviderZai, name, "sk-"+name); err != nil {
			t.Fatal(err)
		}
	}
	oldFB, oldDL := config.UpstreamFirstByteTimeout, config.RequestDeadline
	config.UpstreamFirstByteTimeout = 0
	config.RequestDeadline = time.Hour
	t.Cleanup(func() {
		config.UpstreamFirstByteTimeout, config.RequestDeadline = oldFB, oldDL
	})

	var mu sync.Mutex
	now := time.Now()
	f.eng.SetNow(func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	})
	f.respond = func(int, *http.Request) (int, http.Header, string) {
		mu.Lock()
		now = now.Add(2 * time.Hour)
		mu.Unlock()
		return 503, http.Header{"Content-Type": []string{"text/plain"}}, "upstream down"
	}

	status, body := f.post(t, msgBody(), "sk-test")
	if status != http.StatusServiceUnavailable || !strings.Contains(body, "request_deadline_exceeded") {
		t.Fatalf("预算用尽应返回 503 request_deadline_exceeded，实际 %d %s", status, body)
	}
	if got := f.callCount(); got != 1 {
		t.Fatalf("不得在预算用尽后再发起尝试，实际调用 %d 次", got)
	}
}

// 预算关闭时必须完全回到旧行为：503 照常换号，第二次成功。
func TestRequestDeadlineDisabledKeepsRetrying(t *testing.T) {
	f := newFixture(t)
	for _, name := range []string{"a", "b"} {
		if _, err := f.st.AddAccount(model.ProviderZai, name, "sk-"+name); err != nil {
			t.Fatal(err)
		}
	}
	old := config.RequestDeadline
	config.RequestDeadline = 0
	t.Cleanup(func() { config.RequestDeadline = old })

	f.respond = func(n int, _ *http.Request) (int, http.Header, string) {
		if n == 1 {
			return 503, http.Header{"Content-Type": []string{"text/plain"}}, "upstream down"
		}
		return 200, http.Header{"Content-Type": []string{"application/json"}}, okUpstreamJSON
	}
	status, body := f.post(t, msgBody(), "sk-test")
	if status != 200 {
		t.Fatalf("关闭预算后应照常换号成功，实际 %d %s", status, body)
	}
	if got := f.callCount(); got != 2 {
		t.Fatalf("应发生两次上游调用，实际 %d", got)
	}
}

// 首字节预算必须被剩余总预算收紧：否则最后一次尝试会再白等满 90s。
//
// 判定方式是行为性的：上游 3s 不回首字节，首字节预算 90s、总预算 500ms。
// 若未收紧，请求会一直等到 3s 后拿到 200；收紧后应在 ~500ms 内以预算耗尽收尾。
func TestFirstByteTimeoutClampedByRequestDeadline(t *testing.T) {
	f := newFixture(t)
	acc, err := f.st.AddAccount(model.ProviderZai, "slow-line", "sk-slow")
	if err != nil {
		t.Fatal(err)
	}
	oldFB, oldDL := config.UpstreamFirstByteTimeout, config.RequestDeadline
	config.UpstreamFirstByteTimeout = 90 * time.Second
	config.RequestDeadline = 500 * time.Millisecond
	t.Cleanup(func() {
		config.UpstreamFirstByteTimeout, config.RequestDeadline = oldFB, oldDL
	})

	f.respond = func(_ int, r *http.Request) (int, http.Header, string) {
		select {
		case <-time.After(3 * time.Second):
		case <-r.Context().Done():
		}
		return 200, http.Header{"Content-Type": []string{"application/json"}}, okUpstreamJSON
	}

	started := time.Now()
	status, body := f.post(t, msgBody(), "sk-test")
	if status != http.StatusServiceUnavailable || !strings.Contains(body, "request_deadline_exceeded") {
		t.Fatalf("应由总预算收尾，实际 %d %s", status, body)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("首字节预算未被总预算收紧，耗时 %s", elapsed)
	}
	if got := f.st.Find(model.ProviderZai, acc.ID); got.Status == model.StatusCooling {
		t.Fatalf("首字节超时不得冷却账号：%s", got.Status)
	}
}

// deadline 只拦「下一次尝试」，不得切断已经在交付的响应。
//
// 上游立即回响应头、600ms 后才写完 body，总预算 200ms：若预算被错误地
// 施加到交付阶段，这个请求会失败；正确实现应完整交付。
func TestRequestDeadlineDoesNotCutDelivery(t *testing.T) {
	f := newFixture(t)
	if _, err := f.st.AddAccount(model.ProviderZai, "slow-body", "sk-body"); err != nil {
		t.Fatal(err)
	}
	oldDL := config.RequestDeadline
	config.RequestDeadline = 200 * time.Millisecond
	t.Cleanup(func() { config.RequestDeadline = oldDL })

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		time.Sleep(600 * time.Millisecond)
		_, _ = w.Write([]byte(okUpstreamJSON))
	}))
	t.Cleanup(slow.Close)
	config.UpstreamZai = slow.URL + "/zai"

	status, body := f.post(t, msgBody(), "sk-test")
	if status != 200 {
		t.Fatalf("已开始的交付不得被总预算切断，实际 %d %s", status, body)
	}
	if !strings.Contains(body, "msg_1") {
		t.Fatalf("响应体应完整交付，实际 %s", body)
	}
}
