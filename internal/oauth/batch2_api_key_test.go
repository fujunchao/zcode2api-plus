package oauth

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// 所有 api.z.ai 请求只允许连接本地 TLS 服务；不改生产端点，不发送真实账号材料。
func routeAPIKeyExchangeToLocalTLS(t *testing.T, handler http.Handler) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.Proxy = nil
	host, _, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	transport.TLSClientConfig.ServerName = host
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr != "api.z.ai:443" {
			return nil, fmt.Errorf("回归拒绝非预期的网络目标：%s", addr)
		}
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	old := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = old; transport.CloseIdleConnections() })
}

func TestBatch2APIKeyBusinessUnauthorizedIsNotMissingOrganization(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	routeAPIKeyExchangeToLocalTLS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/auth/z/login":
			_, _ = io.WriteString(w, `{"code":0,"data":{"access_token":"synthetic-business-token"}}`)
		case "/api/biz/customer/getCustomerInfo":
			_, _ = io.WriteString(w, `{"code":401,"msg":"synthetic-business-token must not leak"}`)
		default:
			t.Errorf("业务失败后不应继续请求：%s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	key, err := ExchangeAPIKey("synthetic-access-token", "")
	if key != "" || err == nil || !strings.Contains(err.Error(), "401") ||
		strings.Contains(err.Error(), "找不到可用的机构") || strings.Contains(err.Error(), "synthetic-") {
		t.Fatalf("HTTP 200 业务未授权必须准确归因而不泄漏材料：key=%q err=%v", key, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("业务未授权后必须终止兑换，不创建或复制 Key：%v", calls)
	}
}
