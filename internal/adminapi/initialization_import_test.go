package adminapi

import (
	"net/http"
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
	if f.h.Store.Select(model.ProviderZai, nil, "test-model") != nil || f.h.Store.SelectForDirect(model.ProviderZai, nil, "test-model") != nil {
		t.Fatal("新导入账号在初始化前被提前调度")
	}
}
