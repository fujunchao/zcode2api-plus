package quota

import (
	"io"
	"net/http"
	"net/http/httptest"
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
	svc, _, _ := setup(t)
	t.Cleanup(svc.Close)
	svc.Client = nil
	client, err := svc.clientFor(&model.Account{})
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := client.(*http.Client).Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatal("额度明确直连不能被环境代理改路")
	}
}
