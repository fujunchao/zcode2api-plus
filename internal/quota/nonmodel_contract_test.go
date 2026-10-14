package quota

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

func TestQuotaNativeHTTPContract(t *testing.T) {
	for _, mode := range []string{"jwt", "api_key"} {
		t.Run(mode, func(t *testing.T) {
			svc, st, _ := setup(t)
			t.Cleanup(svc.Close)
			svc.Client = nil
			acc, err := st.AddAccount(model.ProviderZai, "合成额度账号", "synthetic-key")
			if err != nil {
				t.Fatal(err)
			}
			key, token, device := "synthetic-api-key", "synthetic-jwt", "synthetic-device"
			_, _ = st.Update(acc.Provider, acc.ID, func(live *model.Account) {
				live.Mode, live.APIKey, live.JWTToken, live.VirtualDeviceMid = mode, &key, &token, &device
			})
			seen := make(chan *http.Request, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen <- r
				_, _ = io.WriteString(w, balancePayload(1000))
			}))
			t.Cleanup(server.Close)
			line, err := st.AddProxyProfile("合成代理", server.URL, true)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = st.AssignProxyProfile(acc.ID, line.ID)
			old := config.ZcodeBillingBase
			config.ZcodeBillingBase = "http://billing.example.test/api/v1/zcode-plan"
			t.Cleanup(func() { config.ZcodeBillingBase = old })
			if result := svc.FetchQuota(st.FindAny(acc.ID)); result["error"] != nil {
				t.Fatal(result)
			}
			r := <-seen
			if r.Host != "billing.example.test" || r.URL.Path != "/api/v1/zcode-plan/billing/balance" || r.Method != http.MethodGet || !r.URL.IsAbs() {
				t.Fatal("额度请求必须经选定代理访问原计费目标")
			}
			if r.URL.Query().Get("app_version") != config.ZcodeClientVersion || r.URL.Query().Get("platform") != config.ZcodeQueryPlatform() {
				t.Fatal("额度参数没有使用统一档案")
			}
			for name, value := range config.ZcodeSourceHeaders() {
				if r.Header.Get(name) != value {
					t.Errorf("额度来源头 %s 不匹配", name)
				}
			}
			if r.Header.Get("X-Device-Mid") != device || r.Header.Get("X-ZCode-Agent") != "" || r.Header.Get("Cookie") != "" {
				t.Fatal("额度设备/非模型头契约错误")
			}
			if mode == "jwt" {
				if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("x-api-key") != "" {
					t.Fatal("额度 JWT 认证不得混用 API Key")
				}
			} else if r.Header.Get("Authorization") != "" || r.Header.Get("x-api-key") != key {
				t.Fatal("额度 API Key 认证不得附带旧 JWT")
			}
		})
	}
}

func TestQuotaEmptyProxyUsesExplicitDirect(t *testing.T) {
	svc, st, _ := setup(t)
	t.Cleanup(svc.Close)
	svc.Client = nil
	acc, err := st.AddAccount(model.ProviderZai, "合成直连账号", "synthetic-api-key")
	if err != nil {
		t.Fatal(err)
	}
	var directCalls, proxyCalls atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		directCalls.Add(1)
		_, _ = io.WriteString(w, balancePayload(100))
	}))
	t.Cleanup(origin.Close)
	trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(trap.Close)
	trapURL, err := url.Parse(trap.URL)
	if err != nil {
		t.Fatal(err)
	}
	// 默认传输明确配置为代理，避免 http.ProxyFromEnvironment 对环回地址的特殊绕过。
	// 验证真实路由，不依赖 Store 许可包装器内部的具体 Transport 类型。
	oldDefault, oldBase := http.DefaultTransport, config.ZcodeBillingBase
	defaultProxyTransport := &http.Transport{Proxy: http.ProxyURL(trapURL)}
	http.DefaultTransport, config.ZcodeBillingBase = defaultProxyTransport, origin.URL
	t.Cleanup(func() {
		http.DefaultTransport, config.ZcodeBillingBase = oldDefault, oldBase
		defaultProxyTransport.CloseIdleConnections()
	})
	result := svc.FetchQuota(acc)
	if result["error"] != nil || directCalls.Load() != 1 || proxyCalls.Load() != 0 {
		t.Fatalf("原生额度入口必须明确直连，不消费默认代理：result=%v direct=%d proxy=%d", result, directCalls.Load(), proxyCalls.Load())
	}
}
