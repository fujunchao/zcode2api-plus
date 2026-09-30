package adminapi

import (
	"net/http"
	"testing"
	"zcode2api/internal/auth"
	"zcode2api/internal/captcha"
	"zcode2api/internal/model"
	"zcode2api/internal/oauth"
	"zcode2api/internal/quota"
)

func TestOAuthExplicitDirectSurvivesSave(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "新账号", true: "重新登录"}[existing], func(t *testing.T) {
			mux, st, billing := setup(t)
			_ = st.SetSetting("claim_auto_enabled", "false")
			line, _ := st.AddProxyProfile("unused", "http://127.0.0.1:11080", true)
			token := jwtTokenFor("direct-user")
			if existing {
				a, _ := st.AddAccount(model.ProviderZai, "existing", token)
				_, _ = st.AssignProxyProfile(a.ID, line.ID)
			}
			code, body := do(t, mux, st, http.MethodPost, "/admin/api/login/start", map[string]any{"proxy_id": proxyIDDirect})
			if code != http.StatusOK {
				t.Fatal(body)
			}
			session := getLoginFlow(body["flow_id"].(string))
			qs := quota.NewService(st)
			qs.Client = billing
			h := New(st, auth.New(st), captcha.NewManager(), qs)
			a, e := h.saveOAuthAccount(&oauth.ExchangeResult{Token: token}, session)
			if e != nil {
				t.Fatal(e)
			}
			if got := st.FindAny(a.ID); got.ProxyURL != nil || got.ProxyID != nil {
				t.Fatal("明确选择直连后仍绑定代理")
			}
		})
	}
}
