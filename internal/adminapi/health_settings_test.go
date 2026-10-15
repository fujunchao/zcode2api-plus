package adminapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUnifiedHealthSettingsAliasesAndAuthorization(t *testing.T) {
	mux, st, _ := setup(t)
	for _, path := range []string{"/admin/api/proxies/health", "/admin/api/proxies/health/settings"} {
		r := httptest.NewRecorder()
		mux.ServeHTTP(r, httptest.NewRequest("GET", path, nil))
		if r.Code != 401 {
			t.Fatalf("未授权健康端点返回%d", r.Code)
		}
	}
	code, p := do(t, mux, st, "GET", "/admin/api/proxies/health/settings", nil)
	if code != 200 || p["network_enabled"] != true || p["enabled"] != false {
		t.Fatal("未沿用默认网络巡检且禁止默认模型请求")
	}
	p["mode"], p["network_interval_minutes"], p["network_confirm_seconds"] = "observe", 7, 3
	if code, _ := do(t, mux, st, "PUT", "/admin/api/proxies/health/settings", p); code != http.StatusOK {
		t.Fatal("统一配置保存失败")
	}
	_, legacy := do(t, mux, st, "GET", "/admin/api/settings", nil)
	if legacy["proxy_health_interval"] != float64(7) {
		t.Fatal("新设置未同步兼容别名")
	}
	if code, _ := do(t, mux, st, "PUT", "/admin/api/settings", map[string]any{"proxy_health_enabled": false}); code != 200 {
		t.Fatal("旧兼容设置写入失败")
	}
	_, updated := do(t, mux, st, "GET", "/admin/api/proxies/health/settings", nil)
	if updated["network_enabled"] != false || updated["mode"] != "observe" || updated["revision"].(float64) <= p["revision"].(float64) {
		t.Fatal("旧入口未修改同一策略或扩大权限")
	}
}
