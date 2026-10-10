package asyncpool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"zcode2api/internal/captcha"
	"zcode2api/internal/config"
	"zcode2api/internal/gateway"
	"zcode2api/internal/web"
)

// 同步/异步日志都应关联到真正发出的请求，同时不记录凭据、URL 私有部分或调用者标识。
func TestRequestAttemptSummaryRedactsBothHTTPRoutes(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/async/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			p, st, cm, _ := newTestPool(t)
			addJWTAccount(t, st, "synthetic-diagnostic-account")
			if err := st.SetSetting("gateway_key", "gateway-key-sentinel"); err != nil {
				t.Fatal(err)
			}
			oldEnabled := config.AsyncEnabled
			config.AsyncEnabled = true
			t.Cleanup(func() { config.AsyncEnabled = oldEnabled })
			cm.SetConfigProvider(func(context.Context) (captcha.Config, error) {
				return captcha.Config{Enabled: true, SkipModelRequest: true}, nil
			})
			buf := &diagBuf{}
			web.SetOut(buf)
			t.Cleanup(func() {
				p.Close()
				web.SetOut(nil)
			})

			var mu sync.Mutex
			var headers []http.Header
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				mu.Lock()
				headers = append(headers, r.Header.Clone())
				count := len(headers)
				mu.Unlock()
				if count == 1 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w,
					"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n"+
						"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n"+
						"data: {\"type\":\"message_stop\"}\n\n")
			}))
			t.Cleanup(up.Close)
			target, err := url.Parse(up.URL)
			if err != nil {
				t.Fatal(err)
			}
			target.User = url.UserPassword("url-user-sentinel", "url-password-sentinel")
			target.Path, target.RawQuery, target.Fragment = "/url-path-sentinel", "token=url-query-sentinel", "url-fragment-sentinel"
			config.UpstreamZai = target.String()
			mux := http.NewServeMux()
			p.Register(mux)
			engine := gateway.NewEngine(st, cm, nil)
			engine.RateLimitRetryDelay = 0
			(&gateway.Handler{Engine: engine, Auth: p.Auth}).Register(mux)
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)

			body, err := json.Marshal(map[string]any{
				"model": "GLM-5.3", "stream": true, "max_tokens": 8,
				"messages": []any{map[string]any{"role": "user", "content": "prompt-private-sentinel"}},
				"metadata": map[string]any{"user_id": `{"session_id":"session-private-sentinel","device_id":"device-private-sentinel"}`},
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+path, bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("x-api-key", "gateway-key-sentinel")
			req.Header.Set("X-Session-Id", "session-private-sentinel")
			req.Header.Set("X-Zcode-Trace-Id", "trace-private-sentinel")
			req.Header.Set("X-Query-Id", "query-private-sentinel")
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			response, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			p.Close()
			if readErr != nil || resp.StatusCode != http.StatusOK || !strings.Contains(string(response), "message_stop") {
				t.Fatalf("诊断不得改变成功交付: status=%d err=%v body=%s", resp.StatusCode, readErr, response)
			}
			mu.Lock()
			observed := append([]http.Header(nil), headers...)
			mu.Unlock()
			if len(observed) != 2 {
				t.Fatalf("一次限流后应重试成功，实际出站 %d 次", len(observed))
			}
			logs := ansiRE.ReplaceAllString(buf.String(), "")
			for _, want := range []string{"account_mode=jwt", fmt.Sprintf("target_origin=%q", up.URL)} {
				if !strings.Contains(logs, want) {
					t.Fatalf("逐次诊断缺少安全摘要 %q", want)
				}
			}
			for _, h := range observed {
				if want := fmt.Sprintf("upstream_request_id=%q", h.Get("X-Request-Id")); !strings.Contains(logs, want) {
					t.Fatalf("日志未关联到实际 HTTP 请求: %s", want)
				}
			}
			refs := regexp.MustCompile(` session_ref="([0-9a-f]{12})" trace_ref="([0-9a-f]{12})" query_ref="([0-9a-f]{12})"`).FindAllStringSubmatch(logs, -1)
			if len(refs) != 4 {
				t.Fatalf("两次出站的开始/响应日志都应携带脱敏归因引用，实际 %d 组", len(refs))
			}
			for _, match := range refs[1:] {
				if strings.Join(match[1:], ":") != strings.Join(refs[0][1:], ":") {
					t.Fatal("同轮重试的脱敏会话/轮次引用应保持一致")
				}
			}
			for _, secret := range []string{
				"header.payload.signature", "gateway-key-sentinel", "url-user-sentinel", "url-password-sentinel",
				"url-path-sentinel", "url-query-sentinel", "url-fragment-sentinel", "prompt-private-sentinel",
				"session-private-sentinel", "trace-private-sentinel", "query-private-sentinel", "device-private-sentinel",
			} {
				if strings.Contains(logs, secret) {
					t.Fatalf("诊断泄漏合成的敏感哨兵: %s", secret)
				}
			}
		})
	}
}
