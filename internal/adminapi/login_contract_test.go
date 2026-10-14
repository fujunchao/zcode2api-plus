package adminapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zcode2api/internal/auth"
	"zcode2api/internal/captcha"
	"zcode2api/internal/model"
	"zcode2api/internal/oauth"
	"zcode2api/internal/quota"
)

func TestOAuthManualDeletedProxyDoesNotSilentlySwitch(t *testing.T) {
	f := newLoginFailoverFixture(t)
	id, callback := f.start(t, f.good.ID)
	if _, _, err := f.h.Store.DeleteProxyProfile(f.good.ID); err != nil {
		t.Fatal(err)
	}
	status, _ := f.complete(t, id, callback)
	f.mu.Lock()
	requests := len(f.payloads)
	f.mu.Unlock()
	if status != http.StatusBadRequest || f.badCalls.Load() != 0 || requests != 0 || len(f.h.Store.ListAccounts("")) != 0 {
		t.Fatal("人工已删除线路应零出站明确拒绝，不改派到另一代理或创建账号")
	}
}

func TestOAuthLateAPIKeyResponseCannotOverwriteNewCredential(t *testing.T) {
	st := newClaimStore(t)
	if err := st.SetSetting("claim_auto_enabled", "false"); err != nil {
		t.Fatal(err)
	}
	token := jwtTokenFor("synthetic-late-key")
	acc, err := st.AddAccount(model.ProviderZai, "合成旧身份", token)
	if err != nil {
		t.Fatal(err)
	}
	newToken, newKey := jwtTokenFor("synthetic-relogin"), "synthetic-new-credential-key"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body string
		switch {
		case r.URL.Path == "/api/auth/z/login":
			body = `{"code":0,"data":{"access_token":"synthetic-business-token"}}`
		case r.URL.Path == "/api/biz/customer/getCustomerInfo":
			body = `{"code":0,"data":{"organizations":[{"organizationName":"默认机构","organizationId":"synthetic-org","projects":[{"projectName":"默认项目","projectId":"synthetic-project"}]}]}}`
		case strings.HasSuffix(r.URL.Path, "/api_keys") && r.Method == http.MethodGet:
			body = `{"code":0,"data":[{"name":"zcode-api-key","apiKey":"synthetic-key-id"}]}`
		case strings.HasSuffix(r.URL.Path, "/copy/synthetic-key-id"):
			// 最后一个在途响应之前发生重新登录，旧 Key 不能污染新凭据。
			_, updateErr := st.Update(acc.Provider, acc.ID, func(live *model.Account) {
				live.JWTToken, live.APIKey = &newToken, &newKey
			})
			if updateErr != nil {
				t.Error(updateErr)
			}
			body = `{"code":0,"data":{"secretKey":"synthetic-old-secret"}}`
		default:
			t.Errorf("非预期业务目标：%s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	line := loginTestProxyForHost(t, server, "api.z.ai:443")
	profile, err := st.AddProxyProfile("合成本地代理", line.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AssignProxyProfile(acc.ID, profile.ID); err != nil {
		t.Fatal(err)
	}
	qs := quota.NewService(st)
	qs.Client = &fakeBilling{status: http.StatusOK, body: newAccountAllocatedBalance}
	t.Cleanup(qs.Close)
	h := New(st, auth.New(st), captcha.NewManager(), qs)
	t.Cleanup(h.Close)
	_, apiErr := h.saveOAuthAccount(&oauth.ExchangeResult{Token: token, AccessToken: "synthetic-access-token"}, &loginSession{auto: true, proxyID: profile.ID, proxyURL: profile.URL})
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	current := st.FindAny(acc.ID)
	if current.APIKey == nil || *current.APIKey != newKey || current.JWTToken == nil || *current.JWTToken != newToken {
		t.Fatal("OAuth 旧响应覆盖了新登录的凭据")
	}
}
