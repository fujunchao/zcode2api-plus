package adminapi

import (
	"net/http"
	"reflect"
	"testing"

	"zcode2api/internal/gateway"
	"zcode2api/internal/model"
)

func TestAccountsModelQuotasMatchStatusAndIgnoreFilters(t *testing.T) {
	mux, st, billing := setup(t)
	for _, path := range []string{"/admin/api/accounts", "/admin/api/status"} {
		_, body := do(t, mux, st, http.MethodGet, path, nil)
		quotas, ok := body["model_quotas"].([]any)
		if !ok || len(quotas) != len(gateway.AvailableModels) {
			t.Fatalf("空池也应返回两个模型: %s %v", path, body)
		}
	}
	var activeID string
	for _, name := range []string{"active", "disabled", "archived"} {
		acc, err := st.AddAccount(model.ProviderZai, name, "quota-"+name)
		if err != nil {
			t.Fatal(err)
		}
		if name == "active" {
			activeID = acc.ID
		}
		if _, err := st.Update(model.ProviderZai, acc.ID, func(a *model.Account) {
			a.Quota = map[string]map[string]any{
				"GLM-5.3":       {"total": 100, "used": 30, "remaining": 70},
				"GLM-5.3-Flash": {"total": 1000, "used": 200, "remaining": 800},
			}
			if name == "disabled" {
				a.Enabled = false
				a.Status = model.StatusDisabled
			}
			if name == "archived" {
				ts := 1.0
				a.ArchivedAt = &ts
			}
		}); err != nil {
			t.Fatal(err)
		}
	}
	_, status := do(t, mux, st, http.MethodGet, "/admin/api/status", nil)
	for _, query := range []string{"", "?status=disabled", "?archived=true", "?limit=1&offset=1", "?limit=1&offset=99"} {
		code, body := do(t, mux, st, http.MethodGet, "/admin/api/accounts"+query, nil)
		if code != http.StatusOK || !reflect.DeepEqual(body["model_quotas"], status["model_quotas"]) {
			t.Fatalf("账号池汇总不应随筛选分页改变: %s %d %v", query, code, body)
		}
		quotas := body["model_quotas"].([]any)
		if num(t, quotas[0].(map[string]any)["remaining"]) != 1600 || num(t, quotas[1].(map[string]any)["total"]) != 200 {
			t.Fatalf("应包含停用账号但排除归档账号: %v", quotas)
		}
	}
	if _, err := st.Update(model.ProviderZai, activeID, func(a *model.Account) {
		a.Quota["GLM-5.3"]["remaining"] = 0
	}); err != nil {
		t.Fatal(err)
	}
	_, body := do(t, mux, st, http.MethodGet, "/admin/api/accounts", nil)
	quotas := body["model_quotas"].([]any)
	if num(t, quotas[1].(map[string]any)["remaining"]) != 70 || num(t, quotas[0].(map[string]any)["remaining"]) != 1600 {
		t.Fatalf("额度快照更新后应独立更新模型汇总: %v", quotas)
	}
	if billing.callCount() != 0 {
		t.Fatal("列表汇总不得额外查询上游")
	}
}

func TestStatusModelQuotas(t *testing.T) {
	mux, st, billing := setup(t)
	read := func() []any {
		t.Helper()
		code, body := do(t, mux, st, http.MethodGet, "/admin/api/status", nil)
		if code != http.StatusOK || body["quota_pool"] == nil || body["providers"] == nil || body["quota_refresh_interval"] == nil {
			t.Fatalf("旧状态字段必须保留: %d %v", code, body)
		}
		quotas, ok := body["model_quotas"].([]any)
		if !ok || len(quotas) != len(gateway.AvailableModels) {
			t.Fatalf("空池也必须返回所有支持模型: %v", body)
		}
		return quotas
	}
	for i, raw := range read() {
		q := raw.(map[string]any)
		if q["model"] != gateway.AvailableModels[i] || q["total"] != nil || q["remaining"] != nil || q["used"] != nil || num(t, q["items"]) != 0 {
			t.Fatalf("空池不能显示为已耗尽: %v", q)
		}
	}
	acc, err := st.AddAccount(model.ProviderZai, "额度测试", "quota-test-secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Update(model.ProviderZai, acc.ID, func(a *model.Account) {
		a.Quota = map[string]map[string]any{
			"GLM-5.3":       {"total": 100, "used": 100, "remaining": 0},
			"GLM-5.3-Flash": {"total": 1000, "used": 200, "remaining": 800},
		}
		a.ExhaustedModels = []string{"glm-5.3"}
	}); err != nil {
		t.Fatal(err)
	}
	quotas := read()
	flash, base := quotas[0].(map[string]any), quotas[1].(map[string]any)
	if num(t, flash["remaining"]) != 800 || num(t, flash["total"]) != 1000 || num(t, base["remaining"]) != 0 || num(t, base["total"]) != 100 {
		t.Fatalf("单模型耗尽不得影响另一模型: %v", quotas)
	}
	// 汇总实时读取存储快照，不缓存旧总数，不发起额外计费查询。
	if _, err := st.Update(model.ProviderZai, acc.ID, func(a *model.Account) {
		a.Quota["GLM-5.3-Flash"]["remaining"] = 600
	}); err != nil {
		t.Fatal(err)
	}
	if q := read()[0].(map[string]any); num(t, q["remaining"]) != 600 {
		t.Fatalf("刷新后的额度未反映到汇总: %v", q)
	}
	if billing.callCount() != 0 {
		t.Fatal("读取状态不得触发上游额度查询")
	}
}
