package adminapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zcode2api/internal/model"
	"zcode2api/internal/store"
)

// 导入创建必须与初始化登记同事务，不能等待后台循环后才阻止模型调度。
func TestNewAccountInitializationImportRegistersBeforeDispatch(t *testing.T) {
	f := newAccountQuotaFixture(t, newAccountEmptyBalance, newAccountEmptyBalance)
	if err := f.h.Store.SetSetting("claim_auto_enabled", "false"); err != nil {
		t.Fatal(err)
	}
	policy := store.DefaultNewAccountProxyPolicy()
	policy.Enabled, policy.DefaultProxyID = true, f.good.ID
	if _, err := f.h.Store.SaveNewAccountProxyPolicy(policy); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	f.h.Register(mux)
	status, body := do(t, mux, f.h.Store, http.MethodPost, "/admin/api/import", map[string]any{
		"providers": map[string]any{"zai": []map[string]any{{"name": "导入新号", "secret": jwtTokenFor("new-import"), "disabled_models": []string{"other-model"}}}},
	})
	if status != http.StatusOK {
		t.Fatalf("导入失败：%d %v", status, body)
	}
	accounts := f.h.Store.ListAccounts(model.ProviderZai)
	if len(accounts) != 1 || accounts[0].Mode != "jwt" {
		t.Fatalf("应新建一个 JWT：%+v", accounts)
	}
	a := accounts[0]
	if task := f.h.Store.Initialization(a.ID); task == nil || !task.Managed || task.State != "queued" {
		t.Fatalf("新导入 JWT 没有登记初始化任务：%+v", task)
	}
	if len(a.DisabledModels) != 1 || a.DisabledModels[0] != "other-model" {
		t.Fatal("导入的停用模型设置丢失")
	}
	if f.h.Store.Select(model.ProviderZai, nil, "test-model") != nil || f.h.Store.SelectForDirect(model.ProviderZai, nil, "test-model") != nil {
		t.Fatal("新导入账号在初始化前被提前调度")
	}
}

func TestNewAccountInitializationManualEnrollIsExplicitAndScoped(t *testing.T) {
	f := newAccountQuotaFixture(t, newAccountEmptyBalance, newAccountEmptyBalance)
	if err := f.h.Store.SetSetting("claim_auto_enabled", "false"); err != nil {
		t.Fatal(err)
	}
	a, _, err := f.h.Store.AddAccountWithIdentity(model.ProviderZai, "旧版漏处理", jwtTokenFor("missed-import"), "")
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := f.h.Store.AddAccountWithIdentity(model.ProviderZai, "未选择", jwtTokenFor("not-selected"), "")
	if err != nil {
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
	status, body := do(t, mux, f.h.Store, http.MethodPost, "/admin/api/accounts/batch/initialize", map[string]any{"ids": []string{a.ID}, "confirm_proxy_takeover": true})
	if status != http.StatusOK || body["succeeded"] != float64(1) {
		t.Fatalf("应支持明确选择后的补入初始化：%d %v", status, body)
	}
	task := f.h.Store.Initialization(a.ID)
	if task == nil || !f.h.Store.FindAny(a.ID).InitializationBlocked() {
		t.Fatal("补入必须原子建立任务与调度保护")
	}
	if f.h.Store.Initialization(other.ID) != nil {
		t.Fatal("手动补入扫描了未选择账号")
	}
	revision := task.Revision
	status, body = do(t, mux, f.h.Store, http.MethodPost, "/admin/api/accounts/batch/initialize", map[string]any{"ids": []string{a.ID}, "confirm_proxy_takeover": true})
	if status != http.StatusOK || body["duplicated"] != float64(1) || f.h.Store.Initialization(a.ID).Revision != revision {
		t.Fatalf("重复补入不应重建任务：%d %v", status, body)
	}
}

func TestNewAccountInitializationManualEnrollRequiresAuthAndConfirmation(t *testing.T) {
	f := newAccountQuotaFixture(t, newAccountEmptyBalance, newAccountEmptyBalance)
	a, _, err := f.h.Store.AddAccountWithIdentity(model.ProviderZai, "确认", jwtTokenFor("confirmation"), "")
	if err != nil {
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
	unauthorized := httptest.NewRecorder()
	mux.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, "/admin/api/accounts/batch/initialize", strings.NewReader(`{"ids":["unused"],"confirm_proxy_takeover":true}`)))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("补入入口未鉴权：%d", unauthorized.Code)
	}
	for _, confirmation := range []any{false, nil, "true"} {
		status, body := do(t, mux, f.h.Store, http.MethodPost, "/admin/api/accounts/batch/initialize", map[string]any{"ids": []string{a.ID}, "confirm_proxy_takeover": confirmation})
		if status != http.StatusBadRequest || f.h.Store.Initialization(a.ID) != nil {
			t.Fatalf("未明确确认仍创建任务：%d %v", status, body)
		}
	}
}
