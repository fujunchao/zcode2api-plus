package adminapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"zcode2api/internal/model"
	"zcode2api/internal/store"
)

func TestNewAccountInitializationQuotaEvidence(t *testing.T) {
	for _, tc := range []struct{ name, body, kind string }{
		{"空额度", newAccountEmptyBalance, "missing"},
		{"可用额度", newAccountAllocatedBalance, "available"},
		{"缺业务码", `{"data":{"plans":[],"balances":[]}}`, "unknown"},
		{"缺余额", `{"code":0,"data":{"plans":[]}}`, "unknown"},
		{"空对象", `{"code":0,"data":{}}`, "unknown"},
		{"只有套餐", `{"code":0,"data":{"plans":[{"plan_id":"pending"}],"balances":[]}}`, "unknown"},
		{"未知剩余额度", `{"code":0,"data":{"plans":[],"balances":[{"model":"m","total_units":150}]}}`, "unknown"},
		{"可用量优先", `{"code":0,"data":{"plans":[{"plan_id":"old"}],"balances":[{"model":"m","total_units":150,"available_units":0,"remaining_units":150}]}}`, "allocated"},
		{"无模型名", `{"code":0,"data":{"plans":[],"balances":[{"available_units":150}]}}`, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAccountQuotaFixture(t, tc.body, tc.body)
			policy := store.DefaultNewAccountProxyPolicy()
			policy.Enabled = true
			policy.DefaultProxyID = f.good.ID
			if _, err := f.h.Store.SaveNewAccountProxyPolicy(policy); err != nil {
				t.Fatal(err)
			}
			a, _, err := f.h.Store.AddAccountWithIdentity(model.ProviderZai, "证据", jwtTokenFor("evidence"), "", store.AccountCreationOptions{InitializeProxy: true})
			if err != nil {
				t.Fatal(err)
			}
			attempt, err := f.h.Store.BeginInitialization(a.ID, time.Now())
			if err != nil || attempt == nil {
				t.Fatal(err)
			}
			defer f.h.Store.ReleaseInitialization(attempt)
			before := f.h.Store.FindAny(a.ID)
			got := f.h.Quota.ObserveInitialization(context.Background(), attempt)
			if got.Kind != tc.kind {
				t.Fatalf("证据分类错误：got=%s want=%s", got.Kind, tc.kind)
			}
			if !reflect.DeepEqual(before, f.h.Store.FindAny(a.ID)) || len(f.h.Store.ListProxyProfiles()) != 2 {
				t.Fatal("观测提前写入账号或删除代理")
			}
		})
	}
}

func TestNewAccountInitializationPolicyDoesNotAdoptExistingAccounts(t *testing.T) {
	f := newAccountQuotaFixture(t, newAccountEmptyBalance, newAccountEmptyBalance)
	old, _, err := f.h.Store.SaveOAuthAccount("旧号", jwtTokenFor("old-policy"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.Store.AssignProxyProfile(old.ID, f.bad.ID); err != nil {
		t.Fatal(err)
	}
	policy := store.DefaultNewAccountProxyPolicy()
	policy.Enabled = true
	policy.DefaultProxyID = f.good.ID
	if _, err := f.h.Store.SaveNewAccountProxyPolicy(policy); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	f.h.Register(mux)
	if status, _ := do(t, mux, f.h.Store, http.MethodPost, "/admin/api/accounts", map[string]any{"tokens": []string{old.Secret()}, "proxy_id": "__auto__"}); status != http.StatusOK {
		t.Fatal(status)
	}
	if f.h.Store.Initialization(old.ID) != nil || *f.h.Store.FindAny(old.ID).ProxyID != f.bad.ID {
		t.Fatal("重复添加接管了旧号")
	}
	if status, _ := do(t, mux, f.h.Store, http.MethodPost, "/admin/api/import", map[string]any{"providers": map[string]any{"zai": []map[string]string{{"name": "备份", "secret": jwtTokenFor("backup-policy")}}}}); status != http.StatusOK {
		t.Fatal(status)
	}
	for _, a := range f.h.Store.ListAccounts("") {
		if f.h.Store.Initialization(a.ID) != nil {
			t.Fatal("备份导入登记了新号任务")
		}
	}
}

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

func TestNewAccountInitializationRotatesThenPinsDefault(t *testing.T) {
	f := newAccountQuotaFixture(t, newAccountEmptyBalance, newAccountEmptyBalance)
	fixed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("固定业务代理不应参与取额请求")
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer fixed.Close()
	target, err := f.h.Store.AddProxyProfile("固定业务出口", fixed.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	granted := false
	f.mu.Lock()
	f.response = func(r newAccountRequest) (int, string) {
		if r.line == "good" && strings.HasSuffix(r.path, "/billing/claim") {
			granted = true
		}
		if strings.HasSuffix(r.path, "/billing/balance") {
			if granted {
				return http.StatusOK, newAccountAllocatedBalance
			}
			return http.StatusOK, newAccountEmptyBalance
		}
		if strings.HasSuffix(r.path, "/billing/preview") {
			if r.line == "good" {
				return http.StatusOK, `{"code":0,"data":{"plans":[{"plan_id":"trial","priority":1}]}}`
			}
			return http.StatusOK, `{"code":0,"data":{"plans":[]}}`
		}
		return http.StatusOK, `{"code":0,"data":{}}`
	}
	f.mu.Unlock()
	mux := http.NewServeMux()
	f.h.Register(mux)
	_, policy := do(t, mux, f.h.Store, http.MethodGet, "/admin/api/proxies/new-account-policy", nil)
	policy["enabled"], policy["default_proxy_id"] = true, target.ID
	policy["pool_mode"], policy["proxy_ids"] = "selected", []string{f.bad.ID, f.good.ID}
	if status, body := do(t, mux, f.h.Store, http.MethodPut, "/admin/api/proxies/new-account-policy", policy); status != http.StatusOK {
		t.Fatalf("配置失败：%d %v", status, body)
	}
	if status, body := do(t, mux, f.h.Store, http.MethodPost, "/admin/api/accounts", map[string]any{"tokens": []string{jwtTokenFor("rotate-new")}, "proxy_id": "__auto__"}); status != http.StatusOK {
		t.Fatalf("新增失败：%d %v", status, body)
	}
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		a := f.h.Store.ListAccounts(model.ProviderZai)[0]
		if a.ProxyID != nil && *a.ProxyID == target.ID && a.IsSelectable(time.Now()) {
			if len(f.h.Store.ListProxyProfiles()) != 3 || a.Quota["test-model"]["available"] != float64(150) {
				t.Fatal("取额后须保留全部代理和真实额度")
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("应从 A 切到 B 取额后固定 C，实际 %+v", a.Initialization)
		case <-ticker.C:
		}
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
