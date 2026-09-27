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

// 重复添加不是编辑：任何线路选择都不能改变旧账号，也不能再次刷新额度/自动领取。
func TestLegacyDuplicateAddHasNoAccountSideEffects(t *testing.T) {
	for _, selection := range []string{"auto", "profile", "manual", "direct"} {
		t.Run(selection, func(t *testing.T) {
			mux, st, billing := setup(t)
			_ = st.SetSetting("claim_auto_enabled", "false")
			line, _ := st.AddProxyProfile("original", "http://127.0.0.1:18081", true)
			other, _ := st.AddProxyProfile("other", "http://127.0.0.1:18082", true)
			acc, err := st.AddAccount(model.ProviderZai, "existing", jwtTokenFor("duplicate-user"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.AssignProxyProfile(acc.ID, line.ID); err != nil {
				t.Fatal(err)
			}
			payload := map[string]any{"tokens": acc.Secret(), "proxy_id": proxyIDAuto}
			switch selection {
			case "profile":
				payload["proxy_id"] = other.ID
			case "manual":
				payload["proxy_id"], payload["proxy_url"] = proxyIDDirect, "http://127.0.0.1:18083"
			case "direct":
				payload["proxy_id"] = proxyIDDirect
			}
			code, body := do(t, mux, st, http.MethodPost, "/admin/api/accounts", payload)
			if code != http.StatusOK {
				t.Fatalf("重复添加应幂等成功：%d %v", code, body)
			}
			got := st.FindAny(acc.ID)
			if got.ProxyID == nil || *got.ProxyID != line.ID || got.ProxyURL == nil || *got.ProxyURL != line.URL {
				t.Error("重复添加改变了已有账号的线路")
			}
			if billing.callCount() != 0 || got.LastCheckedAt != nil {
				t.Error("重复添加不应重新触发额度刷新")
			}
			if body["count"] != float64(1) || body["created"] != float64(0) || body["duplicated"] != float64(1) || body["assigned"] != float64(0) {
				t.Fatalf("保留旧 count 契约，同时区分真正新建与重复：%v", body)
			}
		})
	}
}

func TestLegacyMixedAddAssignsOnlyNewAccounts(t *testing.T) {
	mux, st, billing := setup(t)
	_ = st.SetSetting("claim_auto_enabled", "false")
	line, _ := st.AddProxyProfile("occupied", "http://127.0.0.1:18081", true)
	free, _ := st.AddProxyProfile("free", "http://127.0.0.1:18082", true)
	old, err := st.AddAccount(model.ProviderZai, "existing", jwtTokenFor("existing-user"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AssignProxyProfile(old.ID, line.ID); err != nil {
		t.Fatal(err)
	}
	code, body := do(t, mux, st, http.MethodPost, "/admin/api/accounts", map[string]any{
		"tokens": []any{old.Secret(), jwtTokenFor("fresh-user")}, "proxy_id": proxyIDAuto,
	})
	if code != http.StatusOK || body["created"] != float64(1) || body["duplicated"] != float64(1) || body["assigned"] != float64(1) {
		t.Fatalf("混合添加的统计应只计新账号的指派：%d %v", code, body)
	}
	for _, a := range st.ListAccounts(model.ProviderZai) {
		want := free.ID
		if a.ID == old.ID {
			want = line.ID
		}
		if a.ProxyID == nil || *a.ProxyID != want {
			t.Fatalf("只应给新账号分配空闲线路：id=%s", a.ID)
		}
	}
	if billing.callCount() != 1 {
		t.Fatalf("只刷新本次真正新增的账号：calls=%d", billing.callCount())
	}
}

// 重新登录命中已有账号：必须换上新令牌并清掉旧失效状态，否则过期账号重登也救不回来。
func TestReloginRenewsJWTOfExistingAccount(t *testing.T) {
	_, st, _ := setup(t)
	_ = st.SetSetting("claim_auto_enabled", "false")
	oldToken := jwtTokenFor("relogin-user")
	acc, err := st.AddAccount(model.ProviderZai, "existing", oldToken)
	if err != nil {
		t.Fatal(err)
	}
	reason, kind := "token expired", "auth"
	_, _ = st.Update(acc.Provider, acc.ID, func(a *model.Account) {
		a.Status = model.StatusInvalid
		a.LastError, a.LastErrorKind = &reason, &kind
		a.RiskControlStreak = 3
	})
	qs := quota.NewService(st)
	qs.Client = &fakeBilling{status: http.StatusOK, body: `{"code":0,"data":{"plans":[],"balances":[]}}`}
	h := New(st, auth.New(st), captcha.NewManager(), qs)
	newToken := jwtTokenFor("relogin-user") + "x"
	got, apiErr := h.saveOAuthAccount(&oauth.ExchangeResult{Token: newToken}, nil)
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if got.ID != acc.ID || len(st.ListAccounts(model.ProviderZai)) != 1 {
		t.Fatal("重新登录应命中已有账号而不是新建")
	}
	cur := st.FindAny(acc.ID)
	if cur.Secret() != newToken {
		t.Fatal("重新登录未保存新 JWT")
	}
	if cur.LastErrorKind != nil && *cur.LastErrorKind == kind || cur.RiskControlStreak != 0 {
		t.Fatalf("重新登录应清掉旧失效状态：%+v", cur)
	}
}
