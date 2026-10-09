package adminapi

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zcode2api/internal/auth"
	"zcode2api/internal/captcha"
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
		status, body := f.status, f.body
		f.mu.Unlock()
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
