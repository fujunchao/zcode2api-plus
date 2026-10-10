package asyncpool

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"zcode2api/internal/captcha"
	"zcode2api/internal/config"
)

// 通过公开 HTTP 入口观察实际出站，不直接调用私有重试函数。
// 一次瞬时限流和两次平台过载后成功：请求 ID 逐次更新，会话归因及正文保持不变。
func TestAsyncRequestIDsRefreshAcrossInPlaceRetries(t *testing.T) {
	p, st, cm, _ := newTestPool(t)
	t.Cleanup(p.Close)
	addJWTAccount(t, st, "synthetic-retry-account")
	if err := st.SetSetting("gateway_key", "synthetic-gateway-key"); err != nil {
		t.Fatal(err)
	}
	oldEnabled := config.AsyncEnabled
	config.AsyncEnabled = true
	t.Cleanup(func() { config.AsyncEnabled = oldEnabled })
	cm.SetConfigProvider(func(context.Context) (captcha.Config, error) {
		return captcha.Config{Enabled: true, SkipModelRequest: true}, nil
	})
	p.OverloadRetryDelays = []time.Duration{0, 0}

	type capturedRequest struct {
		headers http.Header
		body    string
	}
	var mu sync.Mutex
	var requests []capturedRequest
	failures := []struct {
		status int
		body   string
	}{
		{429, `{"error":{"message":"rate limited"}}`},
		{529, `{"error":{"message":"temporarily overloaded"}}`},
		{429, `{"code":1305,"msg":"temporarily overloaded"}`},
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("读取模拟上游请求失败: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		index := len(requests)
		requests = append(requests, capturedRequest{r.Header.Clone(), string(raw)})
		mu.Unlock()
		if index < len(failures) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(failures[index].status)
			_, _ = io.WriteString(w, failures[index].body)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w,
			"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n"+
				"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n"+
				"data: {\"type\":\"message_stop\"}\n\n")
	}))
	t.Cleanup(up.Close)
	config.UpstreamZai = up.URL + "/v1/messages"
	mux := http.NewServeMux()
	p.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	body := `{"model":"GLM-5.3","max_tokens":8,"stream":true,"metadata":{"user_id":"{\"session_id\":\"synthetic-session-for-retry\"}"},"messages":[{"role":"user","content":"合成的重试回归请求"}]}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/async/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "synthetic-gateway-key")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	p.Close()
	if readErr != nil || resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), "event: done") || strings.Contains(string(raw), "event: error") {
		t.Fatalf("同号重试后应完整结束: status=%d err=%v body=%s", resp.StatusCode, readErr, raw)
	}
	mu.Lock()
	observed := append([]capturedRequest(nil), requests...)
	mu.Unlock()
	if len(observed) != 4 {
		t.Fatalf("三次暂时失败后应有四次出站，实际 %d 次", len(observed))
	}
	seen := map[string]bool{}
	uuid := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	for i, call := range observed {
		id := call.headers.Get("X-Request-Id")
		if !uuid.MatchString(id) || seen[id] {
			t.Fatalf("第 %d 次出站须使用全新 UUID 请求 ID，实际 %q", i+1, id)
		}
		seen[id] = true
		for _, key := range []string{"X-Session-Id", "X-Zcode-Trace-Id", "X-Query-Id", "X-Zcode-Session-Type"} {
			if value := call.headers.Get(key); value == "" || value != observed[0].headers.Get(key) {
				t.Fatalf("同一票的 %s 不应因原地重试变化", key)
			}
		}
		if call.headers.Get("X-Session-Id") != "synthetic-session-for-retry" || call.body != observed[0].body {
			t.Fatal("刷新请求 ID 不应改变会话身份或请求正文")
		}
	}
}
