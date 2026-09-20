package adminapi

import (
	"net/http"
	"testing"
)

// 风控冷却阶梯的读写与校验。
//
// 非法值必须 400 而不是被钳制/忽略：档位数同时是升级点（连续命中超过档位数就把账号
// 置为失效），静默少一档会让管理员拿到一个自己都不知道有几档的阶梯。
func TestRiskCoolingStepsAPI(t *testing.T) {
	mux, st, _ := setup(t)

	code, body := do(t, mux, st, http.MethodGet, "/admin/api/settings", nil)
	if code != http.StatusOK {
		t.Fatalf("读取设置应 200: %d", code)
	}
	if got := str(t, body["risk_cooling_steps"]); got != "300,900,3600" {
		t.Fatalf("默认阶梯应 300,900,3600: %q", got)
	}

	// 带空白的合法值应被规范化后落库（否则回读会带一堆空格，看起来像没保存对）。
	code, _ = do(t, mux, st, http.MethodPut, "/admin/api/settings", map[string]any{
		"risk_cooling_steps": " 60, 120 ,180 ",
	})
	if code != http.StatusOK {
		t.Fatalf("合法阶梯应 200: %d", code)
	}
	_, body = do(t, mux, st, http.MethodGet, "/admin/api/settings", nil)
	if got := str(t, body["risk_cooling_steps"]); got != "60,120,180" {
		t.Fatalf("写入后应回读规范化后的阶梯: %q", got)
	}

	// 档位数可少可多（档位数就是升级点）。
	code, _ = do(t, mux, st, http.MethodPut, "/admin/api/settings", map[string]any{
		"risk_cooling_steps": "600",
	})
	if code != http.StatusOK {
		t.Fatalf("单档也应合法: %d", code)
	}
	_, body = do(t, mux, st, http.MethodGet, "/admin/api/settings", nil)
	if got := str(t, body["risk_cooling_steps"]); got != "600" {
		t.Fatalf("单档应回读 600: %q", got)
	}

	// 非法值一律 400，且不得改动已存值。
	for _, bad := range []string{"", "   ", "0", "-5", "abc", "300,,900", "300,abc,900"} {
		code, _ = do(t, mux, st, http.MethodPut, "/admin/api/settings",
			map[string]any{"risk_cooling_steps": bad})
		if code != http.StatusBadRequest {
			t.Fatalf("非法阶梯 %q 应 400: %d", bad, code)
		}
	}
	_, body = do(t, mux, st, http.MethodGet, "/admin/api/settings", nil)
	if got := str(t, body["risk_cooling_steps"]); got != "600" {
		t.Fatalf("非法值不该改动已存值: %q", got)
	}
}
