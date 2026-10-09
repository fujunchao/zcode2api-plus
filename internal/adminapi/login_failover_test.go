package adminapi

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zcode2api/internal/auth"
	"zcode2api/internal/captcha"
	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/proxy"
	"zcode2api/internal/quota"
	"zcode2api/internal/store"
)

type loginFailoverFixture struct {
	h         *Handler
	mux       *http.ServeMux
	bad, good store.ProxyProfile
	billing   *fakeBilling
	badCalls  atomic.Int32
	mu        sync.Mutex
	payloads  []map[string]string
	status    int
	body      string
	onToken   func(*http.Request)
}

// 本地 CONNECT 隧道只允许转发到指定的本地 TLS 服务，绝不解析/连接外部目标。
func loginTestProxy(t *testing.T, backend *httptest.Server) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	connections := map[net.Conn]bool{}
	closing := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != "zcode.z.ai:443" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		remote, err := net.DialTimeout("tcp", backend.Listener.Addr().String(), time.Second)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		local, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			remote.Close()
			t.Error(err)
			return
		}
		mu.Lock()
		if closing {
			mu.Unlock()
			local.Close()
			remote.Close()
			return
		}
		connections[local], connections[remote] = true, true
		mu.Unlock()
		defer func() {
			local.Close()
			remote.Close()
			mu.Lock()
			delete(connections, local)
			delete(connections, remote)
			mu.Unlock()
		}()
		_, _ = fmt.Fprint(local, "HTTP/1.1 200 Connection Established\r\n\r\n")
		go func() { _, _ = io.Copy(remote, buffered); remote.Close() }()
		_, _ = io.Copy(local, remote)
	}))
	transport, err := proxy.TransportFor(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(backend.Certificate())
	name := "example.com"
	if names := backend.Certificate().DNSNames; len(names) > 0 {
		name = names[0]
	}
	transport.TLSClientConfig = &tls.Config{RootCAs: roots, ServerName: name, MinVersion: tls.VersionTLS12}
	t.Cleanup(func() {
		transport.CloseIdleConnections()
		mu.Lock()
		closing = true
		for conn := range connections {
			conn.Close()
		}
		mu.Unlock()
		server.Close()
	})
	return server
}

func newLoginFailoverFixture(t *testing.T) *loginFailoverFixture {
	t.Helper()
	st := newClaimStore(t)
	if err := st.SetSetting("claim_auto_enabled", "false"); err != nil {
		t.Fatal(err)
	}
	f := &loginFailoverFixture{status: http.StatusOK,
		body: `{"code":0,"data":{"token":"` + jwtTokenFor("login-failover-new") + `","user":{"email":"new@example.invalid"}}}`}
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/oauth/token" {
			t.Error("非预期的 OAuth 请求")
			w.WriteHeader(404)
			return
		}
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		f.mu.Lock()
		f.payloads = append(f.payloads, payload)
		status, body, onToken := f.status, f.body, f.onToken
		f.mu.Unlock()
		if onToken != nil {
			onToken(r)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(backend.Close)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.badCalls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(bad.Close)
	good := loginTestProxy(t, backend)
	var err error
	if f.bad, err = st.AddProxyProfile("bad", bad.URL, true); err != nil {
		t.Fatal(err)
	}
	if f.good, err = st.AddProxyProfile("good", good.URL, true); err != nil {
		t.Fatal(err)
	}
	qs := quota.NewService(st)
	f.billing = &fakeBilling{status: http.StatusOK, body: newAccountAllocatedBalance}
	qs.Client = f.billing
	t.Cleanup(qs.Close)
	f.h = New(st, auth.New(st), captcha.NewManager(), qs)
	t.Cleanup(f.h.Close)
	f.mux = http.NewServeMux()
	f.h.Register(f.mux)
	return f
}

type loginRoundTripFunc func(*http.Request) (*http.Response, error)

func (f loginRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func (f *loginFailoverFixture) fakeDirect(t *testing.T, succeeds bool) *atomic.Int32 {
	t.Helper()
	old := http.DefaultTransport
	var calls atomic.Int32
	http.DefaultTransport = loginRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "zcode.z.ai" || r.URL.Path != "/api/v1/oauth/token" {
			t.Error("非预期的直连请求")
			return nil, errors.New("unexpected request")
		}
		calls.Add(1)
		if !succeeds {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("synthetic direct failure")}
		}
		f.mu.Lock()
		body := f.body
		f.mu.Unlock()
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = old })
	return &calls
}

func TestLoginProxyFailoverExhaustedResumesOriginalCallback(t *testing.T) {
	f := newLoginFailoverFixture(t)
	_, err := f.h.Store.UpdateProxyProfile(f.good.ID, f.good.Name, f.good.URL, false)
	if err != nil {
		t.Fatal(err)
	}
	direct := f.fakeDirect(t, false)
	id, callback := f.start(t, proxyIDAuto)
	code, _ := f.complete(t, id, callback)
	if code == http.StatusOK || getLoginFlow(id) == nil || direct.Load() != 1 || len(f.h.Store.ListProxyProfiles()) != 1 {
		t.Fatal("代理耗尽后只直连一次并保留原会话")
	}
	if _, err := f.h.Store.UpdateProxyProfile(f.good.ID, f.good.Name, f.good.URL, true); err != nil {
		t.Fatal(err)
	}
	code, body := f.complete(t, id, callback)
	if code != http.StatusOK || body["status"] != "ready" || body["account"].(map[string]any)["proxy_id"] != f.good.ID {
		t.Fatalf("恢复线路后应直接续用原回调：%d %v", code, body)
	}
	if f.badCalls.Load() != 1 || direct.Load() != 1 {
		t.Fatal("续用会话不能重试已经失败的出口")
	}
}

func TestLoginProxyFailoverFallsBackDirectOnce(t *testing.T) {
	f := newLoginFailoverFixture(t)
	_, _ = f.h.Store.UpdateProxyProfile(f.good.ID, f.good.Name, f.good.URL, false)
	direct := f.fakeDirect(t, true)
	id, callback := f.start(t, proxyIDAuto)
	code, body := f.complete(t, id, callback)
	if code != http.StatusOK || direct.Load() != 1 || body["account"].(map[string]any)["proxy_id"] != nil {
		t.Fatalf("无可用线路应沿用直连回退：%d %v", code, body)
	}
}

func TestLoginProxyFailoverConcurrentCompletion(t *testing.T) {
	f := newLoginFailoverFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	f.mu.Lock()
	f.onToken = func(r *http.Request) {
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}
	f.mu.Unlock()
	id, callback := f.start(t, f.good.ID)
	done := make(chan int, 1)
	go func() { code, _ := f.complete(t, id, callback); done <- code }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("兑换请求没有开始")
	}
	code, _ := f.complete(t, id, callback)
	if code != http.StatusConflict {
		t.Fatal("并发提交应提示处理中，不能再次兑换授权码")
	}
	once.Do(func() { close(release) })
	select {
	case code = <-done:
		if code != http.StatusOK {
			t.Fatal("首次请求应正常完成")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("首次请求未完成")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.payloads) != 1 || f.billing.callCount() != 1 {
		t.Fatal("并发提交只能兑换并初始化一次")
	}
}

func TestLoginProxyFailoverCancellationKeepsProxy(t *testing.T) {
	f := newLoginFailoverFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	f.mu.Lock()
	f.onToken = func(r *http.Request) {
		close(entered)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}
	f.mu.Unlock()
	id, callback := f.start(t, f.good.ID)
	raw, _ := json.Marshal(map[string]any{"callback_url": callback})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/login/complete/"+id, strings.NewReader(string(raw))).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+f.h.Store.AdminKey())
	done := make(chan struct{})
	go func() { defer close(done); f.mux.ServeHTTP(httptest.NewRecorder(), req) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("兑换请求没有开始")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("取消未终止登录请求")
	}
	if len(f.h.Store.ListProxyProfiles()) != 2 || getLoginFlow(id) == nil || len(f.h.Store.ListAccounts("")) != 0 {
		t.Fatal("取消不能删除代理、丢弃会话或创建账号")
	}
}

func TestLoginProxyFailoverCachesCredentialsAcrossSaveFailure(t *testing.T) {
	f := newLoginFailoverFixture(t)
	db, err := sql.Open("sqlite", config.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`CREATE TRIGGER reject_login_binding BEFORE INSERT ON accounts WHEN json_extract(NEW.data, '$.proxy_id') IS NOT NULL BEGIN SELECT RAISE(ABORT,'test login binding failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	id, callback := f.start(t, f.good.ID)
	code, _ := f.complete(t, id, callback)
	if code == http.StatusOK || len(f.h.Store.ListAccounts("")) != 1 || f.billing.callCount() != 0 {
		t.Fatal("测试应停在已建号但代理指派落库失败的边界")
	}
	if _, err := db.Exec("DROP TRIGGER reject_login_binding"); err != nil {
		t.Fatal(err)
	}
	code, body := f.complete(t, id, callback)
	if code != http.StatusOK || body["account"].(map[string]any)["proxy_id"] != f.good.ID || len(f.h.Store.ListAccounts("")) != 1 || f.billing.callCount() != 1 {
		t.Fatalf("保存重试必须续用凭据并补完新号初始化：%d %v", code, body)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.payloads) != 1 {
		t.Fatal("保存失败不能导致一次性授权码再次兑换")
	}
}

func TestLoginProxyFailoverRejectsDifferentCallbackAfterSuccess(t *testing.T) {
	f := newLoginFailoverFixture(t)
	id, callback := f.start(t, f.good.ID)
	code, _ := f.complete(t, id, callback)
	if code != http.StatusOK {
		t.Fatal("首次登录失败")
	}
	code, _ = f.complete(t, id, strings.Replace(callback, "synthetic-code", "other-code", 1))
	if code != http.StatusBadRequest {
		t.Fatal("不能用另一份回调复用缓存凭据")
	}
}

func TestLoginProxyFailoverDeletedOrUpdatedProfileBeforeCallback(t *testing.T) {
	for _, mode := range []string{"deleted", "updated"} {
		t.Run(mode, func(t *testing.T) {
			f := newLoginFailoverFixture(t)
			id, callback := f.start(t, proxyIDAuto)
			want := f.good.ID
			if mode == "deleted" {
				_, _, _ = f.h.Store.DeleteProxyProfile(f.bad.ID)
			} else {
				_, _ = f.h.Store.UpdateProxyProfile(f.bad.ID, f.bad.Name, f.good.URL, true)
				want = f.bad.ID
			}
			code, body := f.complete(t, id, callback)
			if code != http.StatusOK || body["account"].(map[string]any)["proxy_id"] != want || f.badCalls.Load() != 0 {
				t.Fatalf("不能锁定已删除或已更新的旧出口：%d %v", code, body)
			}
		})
	}
}

func TestLoginProxyFailoverRespectsExpiryAndState(t *testing.T) {
	f := newLoginFailoverFixture(t)
	id, callback := f.start(t, proxyIDAuto)
	code, _ := f.complete(t, id, "zcode://oauth/callback?code=synthetic-code&state=wrong")
	if code != http.StatusBadRequest || f.badCalls.Load() != 0 {
		t.Fatal("错误 state 不能发起代理请求")
	}
	getLoginFlow(id).flow.CreatedAt = time.Now().Add(-loginFlowTTL - time.Second)
	code, _ = f.complete(t, id, callback)
	if code != http.StatusNotFound || len(f.h.Store.ListProxyProfiles()) != 2 {
		t.Fatal("过期会话不能继续重试或删除代理")
	}
}

func (f *loginFailoverFixture) start(t *testing.T, proxyID string) (string, string) {
	t.Helper()
	code, body := do(t, f.mux, f.h.Store, http.MethodPost, "/admin/api/login/start", map[string]any{"proxy_id": proxyID})
	if code != http.StatusOK {
		t.Fatal(body)
	}
	id := body["flow_id"].(string)
	session := getLoginFlow(id)
	t.Cleanup(func() { loginFlowsMu.Lock(); delete(loginFlows, id); loginFlowsMu.Unlock() })
	callback := "zcode://oauth/callback?code=synthetic-code&state=" + url.QueryEscape(session.flow.State)
	return id, callback
}

func (f *loginFailoverFixture) complete(t *testing.T, id, callback string) (int, map[string]any) {
	t.Helper()
	return do(t, f.mux, f.h.Store, http.MethodPost, "/admin/api/login/complete/"+id, map[string]any{"callback_url": callback})
}

func TestLoginProxyFailoverCompletesSameCallback(t *testing.T) {
	f := newLoginFailoverFixture(t)
	id, callback := f.start(t, proxyIDAuto)
	peer, err := f.h.Store.AddAccount(model.ProviderZai, "existing-peer", jwtTokenFor("existing-peer"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.Store.AssignProxyProfile(peer.ID, f.bad.ID); err != nil {
		t.Fatal(err)
	}
	code, body := f.complete(t, id, callback)
	if code != http.StatusOK || body["status"] != "ready" {
		t.Fatalf("同一回调应自动避开坏代理并登录成功：%d %v", code, body)
	}
	if len(f.h.Store.ListProxyProfiles()) != 1 || *f.h.Store.FindAny(peer.ID).ProxyID != f.good.ID {
		t.Fatal("应删除故障代理并改派其已有绑定账号")
	}
	account := body["account"].(map[string]any)
	if account["proxy_id"] != f.good.ID {
		t.Fatal("新账号应绑定最终成功的代理")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.payloads) != 1 || f.payloads[0]["code"] != "synthetic-code" || f.badCalls.Load() != 1 {
		t.Fatal("换线必须继续使用原回调，不能重建授权或重复尝试原代理")
	}
}

func TestLoginProxyFailoverDuplicateCallbackRedeemsOnce(t *testing.T) {
	f := newLoginFailoverFixture(t)
	id, callback := f.start(t, f.good.ID)
	code, first := f.complete(t, id, callback)
	if code != http.StatusOK {
		t.Fatal(first)
	}
	code, second := f.complete(t, id, callback)
	if code != http.StatusOK || second["status"] != "ready" {
		t.Fatalf("重复提交成功回调应返回同一账号：%d %v", code, second)
	}
	if first["account"].(map[string]any)["id"] != second["account"].(map[string]any)["id"] || f.billing.callCount() != 1 {
		t.Fatal("重复提交不能重复初始化账号")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.payloads) != 1 {
		t.Fatal("一次性授权码不能重复兑换")
	}
}

func TestLoginProxyFailoverBusinessErrorKeepsProxy(t *testing.T) {
	f := newLoginFailoverFixture(t)
	f.mu.Lock()
	f.status, f.body = http.StatusBadRequest, `{"error":"invalid_grant"}`
	f.mu.Unlock()
	id, callback := f.start(t, f.good.ID)
	code, _ := f.complete(t, id, callback)
	if code == http.StatusOK || len(f.h.Store.ListProxyProfiles()) != 2 || getLoginFlow(id) == nil {
		t.Fatal("授权码业务错误不能删除健康代理或丢弃会话")
	}
}

func TestLoginProxyFailoverTriesSeveralLinesWithoutRepeatingAlias(t *testing.T) {
	f := newLoginFailoverFixture(t)
	id, callback := f.start(t, proxyIDAuto)
	peer, _ := f.h.Store.AddAccount(model.ProviderZai, "busy-good", jwtTokenFor("busy-good"))
	_, _ = f.h.Store.AssignProxyProfile(peer.ID, f.good.ID)
	var secondCalls atomic.Int32
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { secondCalls.Add(1); w.WriteHeader(http.StatusBadGateway) }))
	t.Cleanup(second.Close)
	if _, err := f.h.Store.AddProxyProfile("second-bad", second.URL, true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.Store.AddProxyProfile("old-alias", f.bad.URL, true); err != nil {
		t.Fatal(err)
	}
	code, body := f.complete(t, id, callback)
	if code != http.StatusOK || body["account"].(map[string]any)["proxy_id"] != f.good.ID || body["proxy_retries"] != float64(2) {
		t.Fatalf("应连续换线直至可用线路：%d %v", code, body)
	}
	if f.badCalls.Load() != 1 || secondCalls.Load() != 1 || len(f.h.Store.ListProxyProfiles()) != 2 {
		t.Fatal("已失败地址的别名不能再次尝试，也不能扩大删除范围")
	}
}
