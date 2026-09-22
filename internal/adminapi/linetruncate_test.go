package adminapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/store"
)

// TestLineTruncateSettingsAPI：线路断流熔断设定的 GET 默认 / PUT 往返 / 非法 400。
func TestLineTruncateSettingsAPI(t *testing.T) {
	mux, st, _ := setup(t)

	status, data := do(t, mux, st, http.MethodGet, "/admin/api/settings", nil)
	if status != http.StatusOK {
		t.Fatalf("GET settings: %d", status)
	}
	if got := data["line_truncate_strikes"]; got != float64(3) {
		t.Fatalf("熔断阈值默认应为 3，实际 %v", got)
	}
	if got := data["line_truncate_avoid_seconds"]; got != float64(60) {
		t.Fatalf("回避时长默认应为 60，实际 %v", got)
	}

	status, _ = do(t, mux, st, http.MethodPut, "/admin/api/settings", map[string]any{
		"line_truncate_strikes":       5,
		"line_truncate_avoid_seconds": 90,
	})
	if status != http.StatusOK {
		t.Fatalf("PUT 合法值: %d", status)
	}
	_, data = do(t, mux, st, http.MethodGet, "/admin/api/settings", nil)
	if data["line_truncate_strikes"] != float64(5) || data["line_truncate_avoid_seconds"] != float64(90) {
		t.Fatalf("设定未生效: %v / %v", data["line_truncate_strikes"], data["line_truncate_avoid_seconds"])
	}

	for _, bad := range []map[string]any{
		{"line_truncate_strikes": -1},
		{"line_truncate_strikes": 101},
		{"line_truncate_strikes": "abc"},
		{"line_truncate_avoid_seconds": -5},
		{"line_truncate_avoid_seconds": 99999},
	} {
		if status, _ = do(t, mux, st, http.MethodPut, "/admin/api/settings", bad); status != http.StatusBadRequest {
			t.Fatalf("非法值 %v 应 400，实际 %d", bad, status)
		}
	}
	// 非法值不得落库。
	_, data = do(t, mux, st, http.MethodGet, "/admin/api/settings", nil)
	if data["line_truncate_strikes"] != float64(5) {
		t.Fatalf("非法 PUT 后设定应保持 5，实际 %v", data["line_truncate_strikes"])
	}
	// 0 = 关闭是合法值。
	if status, _ = do(t, mux, st, http.MethodPut, "/admin/api/settings", map[string]any{"line_truncate_strikes": 0}); status != http.StatusOK {
		t.Fatalf("0（关闭）应合法: %d", status)
	}
}

// TestProxyListIncludesTruncateStats：线路列表合并内存态断流计数（不进持久化 blob）。
func TestProxyListIncludesTruncateStats(t *testing.T) {
	mux, st, _ := setup(t)
	p, err := st.AddProxyProfile("counted-line", "http://127.0.0.1:9", true)
	if err != nil {
		t.Fatal(err)
	}
	st.BumpLineTruncate(p.ID)
	st.BumpLineTruncate(p.ID)
	st.ResetLineTruncate(p.ID)
	st.BumpLineTruncate(p.ID)

	status, data := do(t, mux, st, http.MethodGet, "/admin/api/proxies", nil)
	if status != http.StatusOK {
		t.Fatalf("GET proxies: %d", status)
	}
	profiles, _ := data["profiles"].([]any)
	if len(profiles) != 1 {
		t.Fatalf("应有一条线路: %v", profiles)
	}
	entry, _ := profiles[0].(map[string]any)
	if entry["truncate_streak"] != float64(1) || entry["truncate_total"] != float64(3) {
		t.Fatalf("计数应 streak=1 total=3（复位只清连续）: %v", entry)
	}
}

// streamProbeFixture：隔离 config.UpstreamZai，fake 上游同时充当线路的「代理」
// （Go 的 http 代理发绝对 URI 请求，普通 handler 直接应答——与 probeStub 同一手法）。
type streamProbeFixture struct {
	mux *http.ServeMux
	st  *store.Store
}

func newStreamProbeFixture(t *testing.T, h http.Handler) *streamProbeFixture {
	t.Helper()
	mux, st, _ := setup(t)
	oldZai, oldFB := config.UpstreamZai, config.UpstreamZaiFallback
	t.Cleanup(func() { config.UpstreamZai, config.UpstreamZaiFallback = oldZai, oldFB })

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	config.UpstreamZai = srv.URL + "/zai"
	config.UpstreamZaiFallback = srv.URL + "/fallback"
	return &streamProbeFixture{mux: mux, st: st}
}

// probeProfileAndAccount：建一条指向 fake 上游的线路并绑定一个 JWT 账号。
func probeProfileAndAccount(t *testing.T, st *store.Store, proxyURL, name string) (string, *model.Account) {
	t.Helper()
	p, err := st.AddProxyProfile(name, proxyURL, true)
	if err != nil {
		t.Fatal(err)
	}
	acc, err := st.AddAccount(model.ProviderZai, name+"-acct", "header.payload.signature")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AssignProxyProfile(acc.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	return p.ID, acc
}

func TestStreamProbePassesCompleteSSE(t *testing.T) {
	f := newStreamProbeFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"type\":\"message_start\"}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
		fl.Flush()
	}))
	id, acc := probeProfileAndAccount(t, f.st, config.UpstreamZai, "probe-ok")

	status, data := do(t, f.mux, f.st, http.MethodPost, "/admin/api/proxies/"+id+"/stream-test", nil)
	if status != http.StatusOK {
		t.Fatalf("stream-test: %d (%v)", status, data)
	}
	if data["ok"] != true || !strings.Contains(str(t, data["verdict"]), "通过") {
		t.Fatalf("完整 SSE 应判定通过: %v", data)
	}
	// 探测是只读运维工具：不写账号状态、不记断流。
	live := f.st.Find(model.ProviderZai, acc.ID)
	if live.StreamTruncateCount != 0 || live.Status != model.StatusActive {
		t.Fatalf("探测不应改动账号: count=%d status=%s", live.StreamTruncateCount, live.Status)
	}
	if len(f.st.LineTruncateStats()) != 0 {
		t.Fatal("探测不应产生线路断流计数")
	}
}

func TestStreamProbeDetectsMidStreamCut(t *testing.T) {
	f := newStreamProbeFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"type\":\"message_start\"}\n\n")
		fl.Flush()
		panic(http.ErrAbortHandler)
	}))
	id, _ := probeProfileAndAccount(t, f.st, config.UpstreamZai, "probe-cut")

	status, data := do(t, f.mux, f.st, http.MethodPost, "/admin/api/proxies/"+id+"/stream-test", nil)
	if status != http.StatusOK {
		t.Fatalf("stream-test: %d (%v)", status, data)
	}
	if data["ok"] != false || !strings.Contains(str(t, data["verdict"]), "中途掐断") {
		t.Fatalf("中途断流应判失败并给出结论: %v", data)
	}
}

func TestStreamProbeAcceptsBusinessResponse(t *testing.T) {
	f := newStreamProbeFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"1302"}}`))
	}))
	id, _ := probeProfileAndAccount(t, f.st, config.UpstreamZai, "probe-json")

	status, data := do(t, f.mux, f.st, http.MethodPost, "/admin/api/proxies/"+id+"/stream-test", nil)
	if status != http.StatusOK {
		t.Fatalf("stream-test: %d", status)
	}
	if data["ok"] != true || !strings.Contains(str(t, data["verdict"]), "连通性通过") {
		t.Fatalf("完整业务响应应判连通性通过: %v", data)
	}
	if data["status"] != float64(http.StatusTooManyRequests) {
		t.Fatalf("应携带上游状态码: %v", data["status"])
	}
}

func TestStreamProbeRequiresBoundJWTAccount(t *testing.T) {
	f := newStreamProbeFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	p, err := f.st.AddProxyProfile("probe-empty", "http://127.0.0.1:9", true)
	if err != nil {
		t.Fatal(err)
	}
	status, data := do(t, f.mux, f.st, http.MethodPost, "/admin/api/proxies/"+p.ID+"/stream-test", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("无绑定 JWT 账号应 400: %d (%v)", status, data)
	}
	if !strings.Contains(str(t, data["detail"]), "JWT") {
		t.Fatalf("错误信息应说明缺少 JWT 账号: %v", data)
	}
}
