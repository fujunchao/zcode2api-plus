package asyncpool

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zcode2api/internal/captcha"
	"zcode2api/internal/config"
	"zcode2api/internal/gateway"
	"zcode2api/internal/upstream"
)

func asyncErrorFromSSE(t *testing.T, raw string) map[string]any {
	t.Helper()
	for _, frame := range strings.Split(raw, "\n\n") {
		if data, ok := strings.CutPrefix(frame, "event: error\ndata: "); ok {
			var payload map[string]any
			if err := json.Unmarshal([]byte(data), &payload); err != nil {
				t.Fatal(err)
			}
			if object, ok := payload["error"].(map[string]any); ok {
				return object
			}
		}
	}
	t.Fatalf("未找到结构化票据错误事件：%s", raw)
	return nil
}

func TestAsyncRetryAdviceUsesAcceptedTicketTerminalAndDecreasingWait(t *testing.T) {
	// 阈值置 1：本用例只验证「票据终态 + 等待建议」，需要首个 405 就落进请求级
	// 分支；默认阈值 3 时同一条线路上的账号会继续换号，不会登记重放防护。
	oldThreshold := gateway.RiskControlRequestLevelThreshold
	gateway.RiskControlRequestLevelThreshold = 1
	t.Cleanup(func() { gateway.RiskControlRequestLevelThreshold = oldThreshold })

	p, st, cm, _ := newTestPool(t)
	oldEnabled := config.AsyncEnabled
	config.AsyncEnabled, config.AsyncMaxRetries = true, 3
	t.Cleanup(func() { p.Close(); config.AsyncEnabled = oldEnabled })
	for i := range 3 {
		addJWTAccountN(t, st, "retry-fixture", i)
	}
	if err := st.SetSetting("gateway_key", "synthetic-key"); err != nil {
		t.Fatal(err)
	}
	cm.SetConfigProvider(func(context.Context) (captcha.Config, error) {
		return captcha.Config{Enabled: true, SkipModelRequest: true}, nil
	})
	var clockOffset atomic.Int64
	base := time.Now()
	p.replay = gateway.NewReplayGuard(time.Minute, func() time.Time { return base.Add(time.Duration(clockOffset.Load())) })
	var calls atomic.Int32
	var captureMu sync.Mutex
	var capturedHeaders []http.Header
	riskBody := `{"code":3012,"msg":"synthetic risk-control rejection"}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if upstream.MetadataSessionID(body) != "http-session" || r.Header.Get("X-Session-Id") != "http-session" {
			t.Error("异步换号必须沿用入站快照并保持头体会话一致")
		}
		captureMu.Lock()
		capturedHeaders = append(capturedHeaders, r.Header.Clone())
		captureMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n <= 2 {
			w.WriteHeader(http.StatusMethodNotAllowed)
			_, _ = io.WriteString(w, riskBody)
			return
		}
		if n == 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"code":3009,"msg":"synthetic concurrency limit"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, completeProxyFixtureSSE)
	}))
	t.Cleanup(up.Close)
	config.UpstreamZai = up.URL
	srv := newTestMux(t, p)
	post := func(content string) (http.Header, string) {
		t.Helper()
		payload, err := json.Marshal(map[string]any{"model": "GLM-5.3", "messages": []any{map[string]any{"role": "user", "content": content}}})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/async/v1/messages", strings.NewReader(string(payload)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer synthetic-key")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Session-Id", "sess_http-session")
		req.Header.Set("X-Zcode-Trace-Id", "http-trace")
		req.Header.Set("X-Query-Id", "query_http-turn")
		req.Header.Set("X-Zcode-Query-Source", "compact")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil || resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), "event: ticket") {
			t.Fatalf("接受票据后仍通过 SSE 终态表达结果，不回改 HTTP 状态：%d %v %s", resp.StatusCode, readErr, raw)
		}
		return resp.Header.Clone(), string(raw)
	}
	for i, wantWait := range []float64{60, 40, 20} {
		if i > 0 {
			clockOffset.Store(int64(time.Duration(i)*20*time.Second + time.Nanosecond))
		}
		header, raw := post("相同的合成请求")
		errorObject := asyncErrorFromSSE(t, raw)
		details, _ := errorObject["details"].(map[string]any)
		if details["retry_policy"] != "wait_same_gateway" || details["retry_after_seconds"] != wantWait {
			t.Fatalf("等待建议应向上取整真实剩余时间，不能命中续期：%v", details)
		}
		if i == 0 {
			if errorObject["type"] != "upstream_error" || errorObject["message"] != riskBody {
				t.Fatal("首次达到本地阈值必须保留原始上游错误关联")
			}
		} else if errorObject["type"] != "risk_control_cooldown" {
			t.Fatal("后续命中应明确为本地保护终态")
		}
		if header.Get("Retry-After") != "" || calls.Load() != 1 {
			t.Fatal("已发送响应头的票据不能回改状态/头，也不能为重投继续消耗账号")
		}
		if len(details) != 2 {
			t.Fatal("等待建议不能暴露其他请求、账号、内容摘要或线路引用")
		}
	}
	_, raw := post("另一个正常请求")
	if !strings.Contains(raw, "event: done") || strings.Contains(raw, "wait_same_gateway") || calls.Load() != 4 {
		t.Fatalf("局部风控保护不能全局关闭其他请求的 3009 原地重试（calls=%d）", calls.Load())
	}
	captureMu.Lock()
	observed := append([]http.Header(nil), capturedHeaders...)
	captureMu.Unlock()
	seen := map[string]bool{}
	for _, header := range observed {
		if header.Get("X-Zcode-Trace-Id") != "http-trace" || header.Get("X-Query-Id") != "http-turn" || header.Get("X-Zcode-Query-Source") != "compact" {
			t.Fatal("入站快照必须跨 405 换号和 3009 同号重试保持用途与逻辑归因")
		}
		requestID := header.Get("X-Request-Id")
		if requestID == "" || seen[requestID] {
			t.Fatal("每个真实 HTTP 尝试必须有新的 request ID")
		}
		seen[requestID] = true
	}
	if len(observed) != 4 || observed[0].Get("Authorization") == observed[1].Get("Authorization") || observed[2].Get("Authorization") != observed[3].Get("Authorization") {
		t.Fatal("405 应按原预算换号，3009 应按原预算同号重试")
	}
}
