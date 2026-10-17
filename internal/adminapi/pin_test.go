package adminapi

import (
	"net/http"
	"testing"

	"zcode2api/internal/model"
)

// 钉住接口的完整回路：POST 后列表可见、GET 可查、DELETE 幂等清除、越界时长 400。
func TestPinAPIRoundTrip(t *testing.T) {
	mux, st, _ := setup(t)
	acc, err := st.AddAccount(model.ProviderZai, "pin-me", jwtTokenFor("pin-user"))
	if err != nil {
		t.Fatal(err)
	}

	code, body := do(t, mux, st, http.MethodPost, "/admin/api/accounts/"+acc.ID+"/pin", map[string]any{"minutes": 30})
	if code != http.StatusOK {
		t.Fatalf("钉住应成功: %d %v", code, body)
	}
	pinned, _ := body["pinned"].(map[string]any)
	if pinned == nil || pinned["id"] != acc.ID {
		t.Fatalf("响应应回显被钉账号: %v", body)
	}

	// 列表响应带 pinned，供账号行渲染徽标。
	code, body = do(t, mux, st, http.MethodGet, "/admin/api/accounts", nil)
	if code != http.StatusOK {
		t.Fatalf("列表应成功: %d", code)
	}
	listed, _ := body["pinned"].(map[string]any)
	if listed == nil || listed["id"] != acc.ID {
		t.Fatalf("列表应包含钉住状态: %v", body["pinned"])
	}

	// GET 状态
	code, body = do(t, mux, st, http.MethodGet, "/admin/api/pin", nil)
	if code != http.StatusOK || body["pinned"] == nil {
		t.Fatalf("GET /pin 应返回状态: %d %v", code, body)
	}

	// DELETE 幂等
	for range 2 {
		code, body = do(t, mux, st, http.MethodDelete, "/admin/api/pin", nil)
		if code != http.StatusOK || body["pinned"] != nil {
			t.Fatalf("解除应幂等成功: %d %v", code, body)
		}
	}

	// 越界时长拒绝
	code, _ = do(t, mux, st, http.MethodPost, "/admin/api/accounts/"+acc.ID+"/pin", map[string]any{"minutes": 1})
	if code != http.StatusBadRequest {
		t.Fatalf("1 分钟应被拒绝，实际 %d", code)
	}
}
