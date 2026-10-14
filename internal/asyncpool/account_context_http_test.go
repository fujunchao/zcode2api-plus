package asyncpool

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"zcode2api/internal/config"
)

// 从异步公开 HTTP 端点检验拒绝发生在 ticket/验证码/普通出站之前。
func TestAccountContextAsyncRejectedBeforeTicket(t *testing.T) {
	p, st, _, solver := newTestPool(t)
	t.Cleanup(p.Close)
	addJWTAccount(t, st, "synthetic-context-account")
	if err := st.SetSetting("gateway_key", "synthetic-gateway-key"); err != nil {
		t.Fatal(err)
	}
	oldEnabled := config.AsyncEnabled
	config.AsyncEnabled = true
	t.Cleanup(func() { config.AsyncEnabled = oldEnabled })

	up := &scriptedUpstream{specs: []upstreamSpec{{
		status: http.StatusOK,
		lines: []string{
			`data: {"type":"message_start","message":{"usage":{"input_tokens":1}}}`,
			`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"回答"}}`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
			`data: {"type":"message_stop"}`,
		},
	}}}
	config.UpstreamZai = up.start(t).URL + "/v1/messages"
	mux := http.NewServeMux()
	p.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	for _, field := range []string{
		"X-Highspeed-Card-ID", "X-Bigmodel-Authorization",
		"Bigmodel-Target-Type", "Bigmodel-Organization", "Bigmodel-Project",
	} {
		for _, name := range []string{field, strings.ToLower(field), strings.ToUpper(field)} {
			for _, value := range []string{"synthetic-private-context", ""} {
				status, contentType, raw := postAsyncAccountContext(t, srv, "synthetic-gateway-key", http.Header{name: {value}})
				if status != http.StatusBadRequest || !strings.Contains(contentType, "application/json") || !strings.Contains(raw, `"type":"unsupported_account_context"`) {
					t.Fatalf("异步必须在建票前按相同契约拒绝：%d %s", status, raw)
				}
				if strings.Contains(raw, "event: ticket") || value != "" && strings.Contains(raw, value) {
					t.Fatal("拒绝响应不得创建 ticket 或泄漏调用方的权益材料")
				}
			}
		}
	}
	status, _, raw := postAsyncAccountContext(t, srv, "synthetic-gateway-key", http.Header{
		"X-Bigmodel-Authorization": {"synthetic-secondary-one", "synthetic-secondary-two"},
		"X-Highspeed-Card-ID":      {"synthetic-private-card"},
	})
	if status != http.StatusBadRequest || !strings.Contains(raw, `"type":"unsupported_account_context"`) ||
		strings.Contains(raw, "synthetic-") || strings.Contains(raw, "event: ticket") {
		t.Fatalf("多值、多头异步请求也必须在建票前拒绝且不回显：%d %s", status, raw)
	}
	if up.callCount() != 0 || solver.callCount() != 0 {
		t.Fatalf("拒绝前不得调用上游或求解验证码：upstream=%d solver=%d", up.callCount(), solver.callCount())
	}
	for _, authCase := range []struct {
		key    string
		status int
	}{
		{"", http.StatusUnauthorized},
		{"invalid-synthetic-gateway-key", http.StatusForbidden},
	} {
		status, _, raw = postAsyncAccountContext(t, srv, authCase.key, http.Header{"X-Highspeed-Card-ID": {"private"}})
		if status != authCase.status || strings.Contains(raw, "unsupported_account_context") {
			t.Fatalf("异步仍应先鉴权：%d %s", status, raw)
		}
	}

	// 普通请求只继承经过验证的逻辑归因白名单，其余头继续不透传。
	status, contentType, raw := postAsyncAccountContext(t, srv, "synthetic-gateway-key", http.Header{
		"X-Custom-Safe":        {"synthetic-caller-extension"},
		"X-Zcode-Unknown":      {"synthetic-unknown-context"},
		"X-Zcode-Trace-Id":     {"synthetic-caller-trace"},
		"X-Session-Id":         {"sess_synthetic-session"},
		"X-Query-Id":           {"query_synthetic-turn"},
		"X-Zcode-Session-Type": {"subagent"},
		"X-Zcode-Query-Source": {"compact"},
		"X-Request-Id":         {"synthetic-old-request"},
		"Cookie":               {"synthetic-private-cookie"},
		"Anthropic-Beta":       {"synthetic-caller-beta"},
	})
	if status != http.StatusOK || !strings.Contains(contentType, "text/event-stream") ||
		!strings.Contains(raw, "event: ticket") || !strings.Contains(raw, "event: done") || strings.Contains(raw, "event: error") {
		t.Fatalf("未携权益头的异步请求应完整交付：%d %s", status, raw)
	}
	if up.callCount() != 1 {
		t.Fatalf("只有合法请求应调用上游：%d", up.callCount())
	}
	headers := up.header(0)
	for _, name := range []string{"X-Highspeed-Card-ID", "X-Bigmodel-Authorization", "Bigmodel-Target-Type", "Bigmodel-Organization", "Bigmodel-Project", "X-Custom-Safe", "X-Zcode-Unknown", "Cookie", "Anthropic-Beta"} {
		if len(headers.Values(name)) != 0 {
			t.Errorf("异步不应突然继承下游头 %s", name)
		}
	}
	for name, want := range map[string]string{
		"X-Zcode-Trace-Id": "synthetic-caller-trace", "X-Session-Id": "synthetic-session",
		"X-Query-Id": "synthetic-turn", "X-Zcode-Session-Type": "subagent", "X-Zcode-Query-Source": "compact",
	} {
		if got := headers.Get(name); got != want {
			t.Fatalf("异步应保留验证后的逻辑归因：%s=%q want=%q", name, got, want)
		}
	}
	if headers.Get("X-Request-Id") == "" || headers.Get("X-Request-Id") == "synthetic-old-request" {
		t.Fatal("每次实际 HTTP 派发仍须生成新 request ID，不能沿用调用者尝试 ID")
	}
	if headers.Get("Authorization") != "Bearer header.payload.signature" || headers.Get("X-Api-Key") != "header.payload.signature" {
		t.Fatal("合法异步请求仍应使用所选 JWT 的一致双头")
	}
}

func postAsyncAccountContext(t *testing.T, srv *httptest.Server, key string, headers http.Header) (int, string, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/async/v1/messages",
		strings.NewReader(`{"model":"GLM-5.3","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"合成异步上下文回归"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for name, values := range headers {
		req.Header[name] = append([]string(nil), values...)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	client := srv.Client()
	client.Timeout = 10 * time.Second
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), string(raw)
}
