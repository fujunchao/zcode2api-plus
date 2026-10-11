package gateway

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/web"
)

// 以 rc.41 的响应头读取契约模拟中继，不把这项测试冒充生产 New API 部署验收。
func TestDiagnosticNewAPIRelayAssociation(t *testing.T) {
	oldData := config.DataDir
	config.DataDir = t.TempDir()
	t.Cleanup(func() { config.DataDir = oldData })
	f := newFixture(t)
	if _, err := f.st.AddAccount(model.ProviderZai, "relay-account", "sk-synthetic-account"); err != nil {
		t.Fatal(err)
	}
	f.respond = jsonResp(http.StatusOK, okUpstreamJSON)
	buf := &syncBuffer{}
	web.SetOut(buf)
	t.Cleanup(func() { web.SetOut(nil) })
	type relayRecord struct{ requestID, upstreamID, caller string }
	records := make(chan relayRecord, 2)
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		localID := config.NewUUIDv4()
		w.Header().Set(NewAPIRequestIDHeader, localID)
		up, err := http.NewRequestWithContext(r.Context(), http.MethodPost, f.srv.URL+"/v1/messages", r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		up.Header.Set("X-Api-Key", "sk-test")
		for _, name := range []string{GatewayRequestIDHeader, NewAPIRequestIDHeader, "X-Upstream-Request-Id", "X-New-Api-Request-Id"} {
			up.Header.Set(name, "private-forged-id")
		}
		resp, err := f.srv.Client().Do(up)
		if err != nil {
			t.Error(err)
			w.WriteHeader(502)
			return
		}
		defer resp.Body.Close()
		// 对应 rc.41 doRequest → UpstreamRequestIdKey → model.Log.UpstreamRequestId。
		records <- relayRecord{localID, resp.Header.Get(NewAPIRequestIDHeader), r.Header.Get("X-Test-Caller")}
		if resp.Header.Get(GatewayRequestIDHeader) != resp.Header.Get(NewAPIRequestIDHeader) {
			t.Error("网关规范头与 rc.41 兼容别名不一致")
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(relay.Close)
	seen := map[string]bool{}
	for _, caller := range []string{"private-caller-a", "private-caller-b"} {
		req, _ := http.NewRequest(http.MethodPost, relay.URL, strings.NewReader(`{"model":"GLM-5.3","messages":[{"role":"user","content":"private-prompt"}]}`))
		req.Header.Set("X-Test-Caller", caller)
		resp, err := relay.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil || resp.StatusCode != 200 || string(raw) != okUpstreamJSON {
			t.Fatal("新增关联不能改变交付")
		}
		record := <-records
		if record.requestID != resp.Header.Get(NewAPIRequestIDHeader) || record.requestID == record.upstreamID || record.caller != caller {
			t.Fatalf("两层 ID 或用户映射被混淆：%+v", record)
		}
		if !diagnosticUUID.MatchString(record.upstreamID) || seen[record.upstreamID] {
			t.Fatal("网关 ID 不唯一")
		}
		seen[record.upstreamID] = true
		if !strings.Contains(buf.String(), `gateway_request_id="`+record.upstreamID+`"`) {
			t.Fatal("New API 记录无法关联网关日志")
		}
		for _, name := range []string{GatewayRequestIDHeader, NewAPIRequestIDHeader, "X-Upstream-Request-Id", "X-New-Api-Request-Id", "X-Test-Caller"} {
			if f.lastCall().Header.Get(name) != "" {
				t.Fatalf("内部头 %s 被送给模型上游", name)
			}
		}
		if f.lastCall().Header.Get("X-Request-Id") == record.upstreamID {
			t.Fatal("模型 HTTP 尝试 ID 必须与网关入站 ID 独立")
		}
	}
	if strings.Contains(buf.String(), "private-") {
		t.Fatal("网关诊断不应复制调用者/提示词/伪造 ID")
	}
}

func TestDiagnosticIngressDoesNotBufferSSE(t *testing.T) {
	buf := &syncBuffer{}
	web.SetOut(buf)
	t.Cleanup(func() { web.SetOut(nil) })
	release := make(chan struct{})
	srv := httptest.NewServer(ObserveRequests("messages", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "data: last\n\n")
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	if err != nil || line != "data: first\n" {
		t.Fatalf("必须在处理器结束前交付首帧：%q %v", line, err)
	}
	close(release)
	tail, err := io.ReadAll(reader)
	if err != nil || string(tail) != "\ndata: last\n\n" {
		t.Fatalf("流正文改变：%q %v", tail, err)
	}
	if resp.Header.Get(GatewayRequestIDHeader) == "" || !strings.Contains(buf.String(), "http_status=200 response_bytes=25") {
		t.Fatalf("响应关联/真实写出字节数缺失：%s", buf.String())
	}
}

type diagnosticNonFlusher struct{ header http.Header }

func (w *diagnosticNonFlusher) Header() http.Header         { return w.header }
func (w *diagnosticNonFlusher) WriteHeader(int)             {}
func (w *diagnosticNonFlusher) Write(p []byte) (int, error) { return len(p), nil }

func TestDiagnosticIngressPreservesWriterCapabilities(t *testing.T) {
	buf := &syncBuffer{}
	web.SetOut(buf)
	t.Cleanup(func() { web.SetOut(nil) })
	w := &diagnosticNonFlusher{header: http.Header{}}
	ObserveRequests("messages", func(w http.ResponseWriter, _ *http.Request) {
		if _, ok := w.(http.Flusher); ok {
			t.Error("不支持 Flush 的底层不能被伪装成支持")
		}
		if _, ok := w.(interface{ Unwrap() http.ResponseWriter }); !ok {
			t.Error("ResponseController 必须能穿过包装")
		}
		w.WriteHeader(http.StatusBadRequest)
	})(w, httptest.NewRequest(http.MethodPost, "/v1/messages?private=query", nil))
	if strings.Contains(buf.String(), "private") || !strings.Contains(buf.String(), "http_status=400") {
		t.Fatal("不得记录查询参数或丢失 HTTP 结果")
	}
}
