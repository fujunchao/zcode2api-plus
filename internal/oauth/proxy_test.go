// oauth 代理链路单测：证明登录请求确实经选定代理出站，且失败文案不泄露代理凭据。
// 背景：登录、API Key 兑换、额度刷新、活动领取是同一条出站链路，代理必须在
// 建立会话时就生效，否则会出现"登录走代理、用的时候直连"。
package oauth

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestExchangeCodeRunsThroughProxy(t *testing.T) {
	// https 目标会先向代理发 CONNECT 隧道请求——这正是能在代理侧观察到的证据。
	var mu sync.Mutex
	var seen []string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.Host)
		mu.Unlock()
		// 拒绝建隧道：本用例只验证"请求确实到了代理"。
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer fake.Close()

	flow := NewFlow()
	if _, _, err := flow.Init(); err != nil {
		t.Fatalf("Init 失败: %v", err)
	}
	if _, err := flow.ExchangeCode("code", flow.State, fake.URL); err == nil {
		t.Fatal("代理拒绝后应报错")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("代理侧未观察到任何请求：token 交换没有走代理")
	}
	if !strings.HasPrefix(seen[0], http.MethodConnect) {
		t.Fatalf("https 目标应经 CONNECT 隧道: %v", seen)
	}
}

func TestExchangeCodeRejectsInvalidProxy(t *testing.T) {
	flow := NewFlow()
	if _, _, err := flow.Init(); err != nil {
		t.Fatalf("Init 失败: %v", err)
	}
	_, err := flow.ExchangeCode("code", flow.State, "ftp://1.2.3.4:21")
	if err == nil || !strings.Contains(err.Error(), "代理") {
		t.Fatalf("非法代理应在发起请求前报错: %v", err)
	}
}

func TestExchangeCodeFailsFastOnDeadProxy(t *testing.T) {
	// 指向一个刚关闭的本地端口：失败文案必须标明"经代理"，否则使用者
	// 无法区分"上游挂了"和"代理不通"。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	dead := "http://" + ln.Addr().String()
	_ = ln.Close()

	flow := NewFlow()
	if _, _, err := flow.Init(); err != nil {
		t.Fatalf("Init 失败: %v", err)
	}
	_, err = flow.ExchangeCode("code", flow.State, dead)
	if err == nil {
		t.Fatal("代理不可达应报错")
	}
	if !strings.Contains(err.Error(), "经代理") {
		t.Fatalf("错误应标明经代理: %v", err)
	}
}

func TestClientForKeepsEnvProxyWhenUnset(t *testing.T) {
	// 不指定代理时必须用零值客户端（nil Transport → DefaultTransport），
	// 否则会静默关掉环境变量 HTTP_PROXY 的既有行为。
	direct, err := clientFor("", exchangeTimeout)
	if err != nil {
		t.Fatalf("构造直连客户端失败: %v", err)
	}
	if direct.Transport != nil {
		t.Fatal("未指定代理时 Transport 应为 nil，以沿用 DefaultTransport")
	}
	viaProxy, err := clientFor("http://1.2.3.4:8080", exchangeTimeout)
	if err != nil {
		t.Fatalf("构造代理客户端失败: %v", err)
	}
	if viaProxy.Transport == nil {
		t.Fatal("指定代理时应带自定义 Transport")
	}
	if viaProxy.Timeout != exchangeTimeout {
		t.Fatalf("超时应保持一致: %v", viaProxy.Timeout)
	}
}

func TestRequestErrorMasksProxyPassword(t *testing.T) {
	err := requestError("http://user:s3cret@1.2.3.4:8080", errors.New("dial tcp: connection refused"))
	if strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("错误文案泄露了代理密码: %v", err)
	}
	if !strings.Contains(err.Error(), "***") || !strings.Contains(err.Error(), "经代理") {
		t.Fatalf("应同时给出脱敏代理地址与经代理说明: %v", err)
	}
	plain := requestError("", errors.New("boom"))
	if strings.Contains(plain.Error(), "经代理") {
		t.Fatalf("直连失败不应提代理: %v", plain)
	}
}
