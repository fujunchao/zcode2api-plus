package asyncpool

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"zcode2api/internal/config"
	"zcode2api/internal/gateway"
	"zcode2api/internal/openai"
	"zcode2api/internal/web"
)

// New API rc.41 读取渠道响应中的 X-Oneapi-Request-Id；四个入口的早退同样要返回它。
// 不使用入站同名头作为 ID，也不把调用者信息写日志。
func TestDiagnosticIngressIDsAcrossProtocols(t *testing.T) {
	p, st, cm, _ := newTestPool(t)
	if err := st.SetSetting("gateway_key", "sk-diagnostic-test"); err != nil {
		t.Fatal(err)
	}
	oldEnabled := config.AsyncEnabled
	config.AsyncEnabled = true
	t.Cleanup(func() { config.AsyncEnabled = oldEnabled })
	buf := &diagBuf{}
	web.SetOut(buf)
	t.Cleanup(func() { p.Close(); web.SetOut(nil) })
	engine := gateway.NewEngine(st, cm, nil)
	mux := http.NewServeMux()
	(&gateway.Handler{Engine: engine, Auth: p.Auth}).Register(mux)
	openai.New(engine, p.Auth).Register(mux)
	p.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	seen := map[string]bool{}
	for _, tc := range []struct {
		path, protocol, body string
		status               int
	}{
		{"/v1/messages", "messages", `{"model":"GLM-5.3","messages":[{"role":"user","content":"private-body-sentinel"}]}`, 503},
		{"/v1/chat/completions", "chat_completions", `{"model":"GLM-5.3","messages":[{"role":"user","content":"private-body-sentinel"}]}`, 503},
		{"/v1/responses", "responses", `{"model":"GLM-5.3","input":"private-body-sentinel"}`, 503},
		{"/async/v1/messages", "async_messages", `{"model":"GLM-5.3","messages":[{"role":"user","content":"private-body-sentinel"}]}`, 200},
	} {
		t.Run(tc.protocol, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, srv.URL+tc.path, strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("X-Api-Key", "sk-diagnostic-test")
			req.Header.Set("X-Oneapi-Request-Id", "untrusted-caller-sentinel")
			req.Header.Set("X-Zcode2api-Request-Id", "untrusted-caller-sentinel")
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, readErr := io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if readErr != nil || resp.StatusCode != tc.status {
				t.Fatalf("早退协议改变：status=%d err=%v", resp.StatusCode, readErr)
			}
			id := resp.Header.Get("X-Zcode2api-Request-Id")
			if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(id) {
				t.Errorf("应生成独立 UUID，实际 %q", id)
			}
			if resp.Header.Get("X-Oneapi-Request-Id") != id || seen[id] {
				t.Errorf("New API 响应别名必须与唯一网关 ID 一致：%q", id)
			}
			seen[id] = true
			if !strings.Contains(buf.String(), `gateway_request_id="`+id+`"`) || !strings.Contains(buf.String(), "protocol="+tc.protocol) {
				t.Errorf("响应 ID/入口协议未进入诊断：%q %s", id, tc.protocol)
			}
		})
	}
	p.Close()
	logs := buf.String()
	for _, secret := range []string{"private-body-sentinel", "untrusted-caller-sentinel", "sk-diagnostic-test"} {
		if strings.Contains(logs, secret) {
			t.Errorf("诊断不应输出 %q", secret)
		}
	}
	if strings.Count(logs, "[ingress]") != 8 {
		t.Errorf("四个入站应有开始/结束事件：\n%s", logs)
	}
}
