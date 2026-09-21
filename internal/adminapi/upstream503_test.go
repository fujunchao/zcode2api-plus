package adminapi

import (
	"net/http"
	"testing"
)

// 上游 503 冷却阶梯的读写与校验：与风控阶梯同一约定（非法 400 而非钳制、
// 带空白合法值规范化后落库）。默认 30,60,120——503 是上游健康信号，
// 首档秒级即可对冲瞬时抖动（2026-09-20 整池冷却事故）。
func TestUpstream503CoolingStepsAPI(t *testing.T) {
	mux, st, _ := setup(t)

	code, body := do(t, mux, st, http.MethodGet, "/admin/api/settings", nil)
	if code != http.StatusOK {
		t.Fatalf("读取设置应 200: %d", code)
	}
	if got := str(t, body["upstream_503_cooling_steps"]); got != "30,60,120" {
		t.Fatalf("默认阶梯应 30,60,120: %q", got)
	}

	// 带空白的合法值规范化后落库。
	code, _ = do(t, mux, st, http.MethodPut, "/admin/api/settings", map[string]any{
		"upstream_503_cooling_steps": " 15, 30 ,60 ",
	})
	if code != http.StatusOK {
		t.Fatalf("合法阶梯应 200: %d", code)
	}
	_, body = do(t, mux, st, http.MethodGet, "/admin/api/settings", nil)
	if got := str(t, body["upstream_503_cooling_steps"]); got != "15,30,60" {
		t.Fatalf("写入后应回读规范化后的阶梯: %q", got)
	}

	// 非法值一律 400，且不得改动已存值。
	for _, bad := range []string{"", "   ", "0", "-5", "abc", "30,,60", "30,abc,60"} {
		code, _ = do(t, mux, st, http.MethodPut, "/admin/api/settings",
			map[string]any{"upstream_503_cooling_steps": bad})
		if code != http.StatusBadRequest {
			t.Fatalf("非法阶梯 %q 应 400: %d", bad, code)
		}
	}
	_, body = do(t, mux, st, http.MethodGet, "/admin/api/settings", nil)
	if got := str(t, body["upstream_503_cooling_steps"]); got != "15,30,60" {
		t.Fatalf("非法值不该改动已存值: %q", got)
	}
}
