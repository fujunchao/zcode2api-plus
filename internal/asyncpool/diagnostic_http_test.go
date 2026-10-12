package asyncpool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"zcode2api/internal/captcha"
	"zcode2api/internal/config"
	"zcode2api/internal/gateway"
	"zcode2api/internal/openai"
	"zcode2api/internal/upstream"
	"zcode2api/internal/web"
)

func newDiagnosticHTTP(t *testing.T, upstream http.Handler) (*Pool, *httptest.Server, *diagBuf) {
	t.Helper()
	p, st, cm, _ := newTestPool(t)
	addJWTAccount(t, st, "diagnostic-account")
	if err := st.SetSetting("gateway_key", "sk-test"); err != nil {
		t.Fatal(err)
	}
	cm.SetConfigProvider(func(context.Context) (captcha.Config, error) {
		return captcha.Config{Enabled: true, SkipModelRequest: true}, nil
	})
	oldEnabled, oldPreserve := config.AsyncEnabled, config.PreserveClientContext
	config.AsyncEnabled, config.PreserveClientContext = true, true
	t.Cleanup(func() { config.AsyncEnabled, config.PreserveClientContext = oldEnabled, oldPreserve })
	buf := &diagBuf{}
	web.SetOut(buf)
	t.Cleanup(func() { p.Close(); web.SetOut(nil) })
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	config.UpstreamZai = up.URL
	engine := gateway.NewEngine(st, cm, nil)
	engine.RateLimitRetryDelay = 0
	mux := http.NewServeMux()
	(&gateway.Handler{Engine: engine, Auth: p.Auth}).Register(mux)
	openai.New(engine, p.Auth).Register(mux)
	p.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return p, srv, buf
}

func postDiagnosticHTTP(t *testing.T, srv *httptest.Server, path, body string) (http.Header, int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Api-Key", "sk-test")
	req.Header.Set("X-Oneapi-Request-Id", "private-incoming-id")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return resp.Header, resp.StatusCode, string(raw)
}

func TestDiagnosticStructureAcrossHTTPProtocols(t *testing.T) {
	const native = `{"model":"GLM-5.3","stream":true,"system":[{"type":"text","text":"private-system-a"},{"type":"text","text":"private-system-b"}],"messages":[{"role":"user","content":"private-user"},{"role":"assistant","content":"private-reply"},{"role":"system","content":"private-mid"},{"role":"user","content":"private-next"}],"tools":[{"name":"private_tool","input_schema":{"type":"object"}}]}`
	for _, tc := range []struct{ path, body, terminal string }{
		{"/v1/messages", native, "message_stop"},
		{"/async/v1/messages", native, "event: done"},
		{"/v1/chat/completions", `{"model":"GLM-5.3","stream":true,"messages":[{"role":"system","content":"private-system-a"},{"role":"developer","content":"private-system-b"},{"role":"user","content":"private-user"},{"role":"assistant","content":"private-reply"},{"role":"developer","content":"private-mid"},{"role":"user","content":"private-next"}],"tools":[{"type":"function","function":{"name":"private_tool","parameters":{"type":"object"}}}]}`, "data: [DONE]"},
		{"/v1/responses", `{"model":"GLM-5.3","stream":true,"instructions":"private-system-a","input":[{"type":"message","role":"developer","content":"private-system-b"},{"type":"message","role":"user","content":"private-user"},{"type":"message","role":"assistant","content":"private-reply"},{"type":"message","role":"system","content":"private-mid"},{"type":"message","role":"user","content":"private-next"}],"tools":[{"type":"function","name":"private_tool","parameters":{"type":"object"}}]}`, "response.completed"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			actual := make(chan http.Header, 1)
			p, srv, buf := newDiagnosticHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				select {
				case actual <- r.Header.Clone():
				default:
					t.Error("成功请求不应发生额外出站")
					w.WriteHeader(500)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("X-Request-Id", "d0213372-5895-489e-98d5-37e95a12786a")
				_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_diag\",\"model\":\"GLM-5.3\",\"usage\":{\"input_tokens\":4}}}\n\n"+
					"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n"+
					"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n"+
					"data: {\"type\":\"message_stop\"}\n\n")
			}))
			header, status, response := postDiagnosticHTTP(t, srv, tc.path, tc.body)
			p.Close()
			if status != 200 || !strings.Contains(response, tc.terminal) {
				t.Fatalf("流式终态改变：%d %s", status, response)
			}
			up := <-actual
			if up.Get("X-Oneapi-Request-Id") != "" {
				t.Fatal("内部关联不能发给模型上游")
			}
			logs := buf.String()
			for _, want := range []string{
				// 四条协议路径的非官方前缀 system 都回退「官方块前置」（2026-10-12 起）；
				// 期望块数按实际注入块数派生，避免与注入开关互相打架。
				"shape_stage=prepared", "preserve_context=true", "system_source=legacy_merge", "system_form=blocks",
				fmt.Sprintf("system_blocks=%d", len(upstream.ZcodeSystemBlocks())+2),
				"messages=4", "roles_user=2", "roles_assistant=1", "roles_system=1", "tools=1", "mcs_declared=true",
				`gateway_request_id="` + header.Get(gateway.GatewayRequestIDHeader) + `"`,
				`upstream_request_id="` + up.Get("X-Request-Id") + `"`,
				`response_request_id="d0213372-5895-489e-98d5-37e95a12786a"`,
			} {
				if !strings.Contains(logs, want) {
					t.Errorf("%s 缺少 %q：%s", tc.path, want, logs)
				}
			}
			if strings.Contains(logs, "private") {
				t.Fatal("结构日志泄漏内容、工具名或入站 ID")
			}
			for _, match := range regexp.MustCompile(`\bts=([^\s]+)`).FindAllStringSubmatch(logs, -1) {
				if _, err := time.Parse(time.RFC3339Nano, match[1]); err != nil || !strings.HasSuffix(match[1], "Z") {
					t.Fatalf("UTC 时间戳错误：%q", match[1])
				}
			}
		})
	}
}

func TestDiagnosticReadFailureBothHTTPRoutes(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/async/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			p, srv, buf := newDiagnosticHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Content-Length", "1000")
				w.WriteHeader(405)
				_, _ = io.WriteString(w, `{"code":3012,`)
				w.(http.Flusher).Flush()
			}))
			_, _, response := postDiagnosticHTTP(t, srv, path, `{"model":"GLM-5.3","stream":true,"messages":[{"role":"user","content":"private-body"}]}`)
			p.Close()
			if !strings.Contains(response, "error") {
				t.Fatal("读断不得当作成功交付")
			}
			logs := buf.String()
			if !strings.Contains(logs, "event=response_body") || !strings.Contains(logs, "status=405") || !strings.Contains(logs, "read_err_kind=unexpected_eof") {
				t.Fatalf("同步/异步读断原因缺失：%s", logs)
			}
		})
	}
}

func TestDiagnosticAsyncReplayHasSummary(t *testing.T) {
	p, srv, buf := newDiagnosticHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { t.Error("重放不能出站"); w.WriteHeader(500) }))
	raw := `{"model":"GLM-5.3","messages":[{"role":"user","content":"private-body"}]}`
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatal(err)
	}
	gateway.NormalizeBody(body, false)
	p.replay.Record(gateway.ContentKey("GLM-5.3", body))
	header, status, response := postDiagnosticHTTP(t, srv, "/async/v1/messages", raw)
	p.Close()
	if status != 200 || !strings.Contains(response, "risk_control_cooldown") {
		t.Fatal("异步重放应继续用原 error 事件交付")
	}
	logs := buf.String()
	for _, want := range []string{"[#]", "upstream_attempts=0", "replay_blocked=1", "input_ref=", "replay_wait_ms=", `gateway_request_id="` + header.Get(gateway.GatewayRequestIDHeader) + `"`} {
		if !strings.Contains(logs, want) {
			t.Errorf("重放缺少 %q：%s", want, logs)
		}
	}
	if strings.Contains(logs, "[attempt]") || strings.Contains(logs, "private-body") {
		t.Fatal("重放不得出站或抄录正文")
	}
}
