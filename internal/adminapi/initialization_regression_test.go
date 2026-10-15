package adminapi

import (
	"net/http"
	"reflect"
	"testing"

	"zcode2api/internal/model"
)

// 通过新增端点验证共享线路保护；上游和凭据均来自隔离夹具。
func TestNewAccountEmptyQuotaNeverDeletesSharedProxy(t *testing.T) {
	f := newAccountQuotaFixture(t, newAccountEmptyBalance, newAccountEmptyBalance)
	old, _, err := f.h.Store.SaveOAuthAccount("已有账号", jwtTokenFor("existing-shared"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.Store.AssignProxyProfile(old.ID, f.bad.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.Store.Update(old.Provider, old.ID, func(a *model.Account) { a.UseCount = 8 }); err != nil {
		t.Fatal(err)
	}
	before := f.h.Store.FindAny(old.ID)
	mux := http.NewServeMux()
	f.h.Register(mux)
	status, result := do(t, mux, f.h.Store, http.MethodPost, "/admin/api/accounts", map[string]any{
		"tokens": []string{jwtTokenFor("new-empty-shared")}, "proxy_id": f.bad.ID,
	})
	if status != http.StatusOK {
		t.Fatalf("新增失败：%d %v", status, result)
	}
	f.wait(t)
	if len(f.h.Store.ListProxyProfiles()) != 2 || !reflect.DeepEqual(before, f.h.Store.FindAny(old.ID)) {
		t.Fatal("新号空额度不得删除共享代理或改派已有账号")
	}
}

func TestNewAccountInitializationRegistersBeforeQuota(t *testing.T) {
	f := newAccountQuotaFixture(t, newAccountEmptyBalance, newAccountEmptyBalance)
	if err := f.h.Store.SetSetting("claim_auto_enabled", "false"); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	f.h.Register(mux)
	status, policy := do(t, mux, f.h.Store, http.MethodGet, "/admin/api/proxies/new-account-policy", nil)
	if status != http.StatusOK || policy["enabled"] != false {
		t.Fatalf("应提供默认关闭的初始化代理配置：%d %v", status, policy)
	}
	policy["enabled"], policy["default_proxy_id"] = true, f.good.ID
	status, saved := do(t, mux, f.h.Store, http.MethodPut, "/admin/api/proxies/new-account-policy", policy)
	if status != http.StatusOK {
		t.Fatalf("应能选择默认业务代理：%d %v", status, saved)
	}
	status, created := do(t, mux, f.h.Store, http.MethodPost, "/admin/api/accounts", map[string]any{
		"tokens": []string{jwtTokenFor("waiting-initialization")}, "proxy_id": "__auto__",
	})
	if status != http.StatusOK {
		t.Fatalf("新增失败：%d %v", status, created)
	}
	if f.h.Store.Select(model.ProviderZai, nil, "test-model") != nil || f.h.Store.SelectForDirect(model.ProviderZai, nil, "test-model") != nil {
		t.Fatal("初始化必须在创建账号时阻断普通与强制直连选号")
	}
	if got := f.accountRequests(jwtTokenFor("waiting-initialization")); len(got) != 0 {
		t.Fatalf("自动领取关闭时不得先从普通首查路径出站：%v", got)
	}
	status, listed := do(t, mux, f.h.Store, http.MethodGet, "/admin/api/accounts", nil)
	if status != http.StatusOK {
		t.Fatal(listed)
	}
	view := listed["accounts"].([]any)[0].(map[string]any)
	state, ok := view["initialization"].(map[string]any)
	if !ok || state["default_proxy_id"] != f.good.ID || state["state"] != "queued" {
		t.Fatalf("账号视图应公开初始化阶段和目标：%v", view)
	}
}
