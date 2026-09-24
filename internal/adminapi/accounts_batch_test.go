// 批量管理端点的 handler 测试：请求校验、逐条明细回传、错误映射（400/500）、
// 旧端点回归不变、新路由鉴权。store 层语义测试见 internal/store/batch_test.go。
package adminapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"zcode2api/internal/model"
)

func TestBatchAddEndpoint(t *testing.T) {
	mux, st, _ := setup(t)

	// 预置一个既有账号，供重复判定。
	if code, body := do(t, mux, st, http.MethodPost, "/admin/api/accounts", map[string]any{
		"tokens": "dup-secret-token",
	}); code != http.StatusOK {
		t.Fatalf("预置账号失败: %d %v", code, body)
	}

	code, body := do(t, mux, st, http.MethodPost, "/admin/api/accounts/batch/add", map[string]any{
		"items": []any{
			map[string]any{"name": "b1", "secret": "fresh-1"},
			map[string]any{"secret": "dup-secret-token"}, // 重复
			map[string]any{"name": "b2", "secret": "fresh-2", "email": "b2@example.com"},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("批量新增应 200: %d %v", code, body)
	}
	if got := num(t, body["total"]); got != 3 {
		t.Fatalf("total 应为 3: %v", body["total"])
	}
	if got := num(t, body["succeeded"]); got != 2 {
		t.Fatalf("succeeded 应为 2: %v", body["succeeded"])
	}
	if got := num(t, body["duplicated"]); got != 1 {
		t.Fatalf("duplicated 应为 1: %v", body["duplicated"])
	}
	items := body["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("明细应有 3 条: %d", len(items))
	}
	first := items[0].(map[string]any)
	if first["status"] != "ok" {
		t.Fatalf("第 1 条应 ok: %v", first)
	}
	second := items[1].(map[string]any)
	if second["status"] != "duplicate" {
		t.Fatalf("第 2 条应 duplicate: %v", second)
	}
	ids := body["ids"].([]any)
	if len(ids) != 2 {
		t.Fatalf("ids 应只含 2 个新建账号: %v", ids)
	}
	// 明细里的空数组序列化为 [] 而非 null。
	if body["not_found"] == nil || body["failed"] == nil {
		t.Fatalf("计数键不得缺失: %v", body)
	}
	if got := len(st.ListAccounts(model.ProviderZai)); got != 3 {
		t.Fatalf("池内应有 3 个账号，实际 %d", got)
	}
}

func TestBatchAddEndpointValidation(t *testing.T) {
	mux, st, _ := setup(t)

	// items 缺失 / 非数组 / 空数组 → 400。
	for name, payload := range map[string]map[string]any{
		"缺items":   {},
		"items非数组": {"items": "nope"},
		"items为空":  {"items": []any{}},
	} {
		if code, _ := do(t, mux, st, http.MethodPost, "/admin/api/accounts/batch/add", payload); code != http.StatusBadRequest {
			t.Fatalf("%s 应 400: %d", name, code)
		}
	}
	// 缺 secret → 400 且带定位。
	code, body := do(t, mux, st, http.MethodPost, "/admin/api/accounts/batch/add", map[string]any{
		"items": []any{map[string]any{"name": "no-secret"}},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("缺 secret 应 400: %d %v", code, body)
	}
	// 超限 → 400。
	oversize := make([]any, 501)
	for i := range oversize {
		oversize[i] = map[string]any{"secret": fmt.Sprintf("k%d", i)}
	}
	if code, _ := do(t, mux, st, http.MethodPost, "/admin/api/accounts/batch/add", map[string]any{
		"items": oversize,
	}); code != http.StatusBadRequest {
		t.Fatalf("超过 500 上限应 400: %d", code)
	}
	// 未知 provider → 400。
	if code, _ := do(t, mux, st, http.MethodPost, "/admin/api/accounts/batch/add", map[string]any{
		"provider": "nope", "items": []any{map[string]any{"secret": "k"}},
	}); code != http.StatusBadRequest {
		t.Fatalf("未知 provider 应 400: %d", code)
	}
	// 校验失败零副作用。
	if got := len(st.ListAccounts(model.ProviderZai)); got != 0 {
		t.Fatalf("全部校验失败后池应仍为空，实际 %d", got)
	}
}

func TestBatchDeleteEndpointStrictVsMissingOK(t *testing.T) {
	mux, st, _ := setup(t)
	var ids []any
	for i := 0; i < 3; i++ {
		code, body := do(t, mux, st, http.MethodPost, "/admin/api/accounts", map[string]any{
			"tokens": fmt.Sprintf("del-token-%d", i),
		})
		if code != http.StatusOK {
			t.Fatalf("预置失败: %d %v", code, body)
		}
		ids = append(ids, body["ids"].([]any)[0])
	}

	// 严格模式：ghost 混入 → 400 + not_found 明细，一个都不删。
	code, body := do(t, mux, st, http.MethodPost, "/admin/api/accounts/batch/delete", map[string]any{
		"ids": []any{ids[0], "ghost", ids[1]},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("严格模式缺 ID 应 400: %d %v", code, body)
	}
	if got := num(t, body["not_found"]); got != 1 {
		t.Fatalf("not_found 应为 1: %v", body["not_found"])
	}
	if got := len(st.ListAccounts(model.ProviderZai)); got != 3 {
		t.Fatalf("严格模式一个都不删，实际剩 %d", got)
	}

	// 宽松模式：删除存在项。
	code, body = do(t, mux, st, http.MethodPost, "/admin/api/accounts/batch/delete", map[string]any{
		"ids": []any{ids[0], "ghost", ids[1]}, "missing_ok": true,
	})
	if code != http.StatusOK {
		t.Fatalf("宽松模式应 200: %d %v", code, body)
	}
	if got := num(t, body["succeeded"]); got != 2 {
		t.Fatalf("succeeded 应为 2: %v", body["succeeded"])
	}
	if got := len(st.ListAccounts(model.ProviderZai)); got != 1 {
		t.Fatalf("应剩 1 个账号，实际 %d", got)
	}

	// 旧 DELETE 端点（数组体）回归：行为不变（跳过不存在项）。
	code, body = do(t, mux, st, http.MethodDelete, "/admin/api/accounts", []any{ids[2], "ghost"})
	if code != http.StatusOK {
		t.Fatalf("旧 DELETE 端点应 200: %d %v", code, body)
	}
	if got := num(t, body["deleted"]); got != 1 {
		t.Fatalf("旧端点应删除 1 个: %v", body["deleted"])
	}
	if got := len(st.ListAccounts(model.ProviderZai)); got != 0 {
		t.Fatalf("旧端点删除后池应为空，实际 %d", got)
	}
}

func TestBatchEnableDisableEndpoint(t *testing.T) {
	mux, st, _ := setup(t)
	var ids []any
	for i := 0; i < 2; i++ {
		_, body := do(t, mux, st, http.MethodPost, "/admin/api/accounts", map[string]any{
			"tokens": fmt.Sprintf("en-token-%d", i),
		})
		ids = append(ids, body["ids"].([]any)[0])
	}

	// 批量禁用。
	code, body := do(t, mux, st, http.MethodPost, "/admin/api/accounts/batch/disable", map[string]any{"ids": ids})
	if code != http.StatusOK || num(t, body["succeeded"]) != 2 {
		t.Fatalf("批量禁用应 200 且成功 2: %d %v", code, body)
	}
	for _, id := range ids {
		if acc := st.FindAny(id.(string)); acc.Enabled || acc.Status != model.StatusDisabled {
			t.Fatalf("账号 %s 应为 disabled: enabled=%v status=%s", id, acc.Enabled, acc.Status)
		}
	}

	// 批量启用（只启用第一个）。
	code, body = do(t, mux, st, http.MethodPost, "/admin/api/accounts/batch/enable", map[string]any{
		"ids": []any{ids[0]},
	})
	if code != http.StatusOK || num(t, body["succeeded"]) != 1 {
		t.Fatalf("批量启用应 200 且成功 1: %d %v", code, body)
	}
	if acc := st.FindAny(ids[0].(string)); !acc.Enabled || acc.Status != model.StatusActive {
		t.Fatalf("启用后应为 active: enabled=%v status=%s", acc.Enabled, acc.Status)
	}
	if acc := st.FindAny(ids[1].(string)); acc.Enabled {
		t.Fatal("未包含在批量里的账号不得被改动")
	}

	// 严格模式：ghost → 400 零改动。
	code, _ = do(t, mux, st, http.MethodPost, "/admin/api/accounts/batch/enable", map[string]any{
		"ids": []any{ids[1], "ghost"},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("严格模式应 400: %d", code)
	}
	if acc := st.FindAny(ids[1].(string)); acc.Enabled {
		t.Fatal("严格拒绝后不得改动任何账号")
	}

	// 单账号 enabled 端点回归不变。
	if code, _ := do(t, mux, st, http.MethodPost, "/admin/api/accounts/"+ids[0].(string)+"/enabled",
		map[string]any{"enabled": false}); code != http.StatusOK {
		t.Fatalf("单账号端点应 200: %d", code)
	}
	if acc := st.FindAny(ids[0].(string)); acc.Status != model.StatusDisabled {
		t.Fatalf("单账号端点应禁用成功: %s", acc.Status)
	}
}

func TestBatchEndpointsRequireAuth(t *testing.T) {
	mux, _, _ := setup(t)
	for _, path := range []string{
		"/admin/api/accounts/batch/add",
		"/admin/api/accounts/batch/delete",
		"/admin/api/accounts/batch/enable",
		"/admin/api/accounts/batch/disable",
	} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if res := rec.Result(); res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s 缺凭证应 401: %d", path, res.StatusCode)
		}
	}
}

func TestListAccountsConditionalQuery(t *testing.T) {
	mux, st, _ := setup(t)
	var idDisabled string
	for i := 0; i < 3; i++ {
		_, body := do(t, mux, st, http.MethodPost, "/admin/api/accounts", map[string]any{
			"tokens": fmt.Sprintf("q-token-%d", i),
		})
		if i == 1 {
			idDisabled = body["ids"].([]any)[0].(string)
		}
	}
	if code, _ := do(t, mux, st, http.MethodPost, "/admin/api/accounts/"+idDisabled+"/enabled",
		map[string]any{"enabled": false}); code != http.StatusOK {
		t.Fatalf("禁用失败: %d", code)
	}

	// 无参数：行为与旧版一致（全量），响应追加 total/offset/limit。
	code, body := do(t, mux, st, http.MethodGet, "/admin/api/accounts", nil)
	if code != http.StatusOK {
		t.Fatalf("列表应 200: %d", code)
	}
	if got := len(body["accounts"].([]any)); got != 3 {
		t.Fatalf("无参数应返回全部 3 个: %d", got)
	}
	if got := num(t, body["total"]); got != 3 {
		t.Fatalf("total 应为 3: %v", body["total"])
	}

	// status 过滤。
	code, body = do(t, mux, st, http.MethodGet, "/admin/api/accounts?status=disabled", nil)
	if code != http.StatusOK {
		t.Fatalf("过滤查询应 200: %d %v", code, body)
	}
	accounts := body["accounts"].([]any)
	if len(accounts) != 1 {
		t.Fatalf("status=disabled 应只命中 1 个: %d", len(accounts))
	}
	if got := accounts[0].(map[string]any)["id"]; got != idDisabled {
		t.Fatalf("应命中被禁用账号: %v", got)
	}
	if got := num(t, body["total"]); got != 1 {
		t.Fatalf("过滤后 total 应为 1: %v", body["total"])
	}

	// keyword 过滤（按 id 子串）。
	needle := idDisabled[:10]
	code, body = do(t, mux, st, http.MethodGet, "/admin/api/accounts?keyword="+needle, nil)
	if code != http.StatusOK || len(body["accounts"].([]any)) != 1 {
		t.Fatalf("keyword 应命中 1 个: %d %v", code, body)
	}

	// 分页：limit + offset。
	code, body = do(t, mux, st, http.MethodGet, "/admin/api/accounts?limit=2&offset=1", nil)
	if code != http.StatusOK {
		t.Fatalf("分页应 200: %d", code)
	}
	if got := len(body["accounts"].([]any)); got != 2 {
		t.Fatalf("limit=2 应返回 2 条: %d", got)
	}
	if got := num(t, body["total"]); got != 3 {
		t.Fatalf("分页时 total 仍应为 3: %v", body["total"])
	}

	// 非法参数一律 400。
	for _, qs := range []string{
		"status=nope", "enabled=maybe", "mode=oauth", "model_status=ok",
		"archived=x", "sort=weird", "limit=501", "limit=0", "offset=-1",
		"created_after=abc",
	} {
		if code, _ := do(t, mux, st, http.MethodGet, "/admin/api/accounts?"+qs, nil); code != http.StatusBadRequest {
			t.Fatalf("%s 应 400: %d", qs, code)
		}
	}

	// stats 保持全量口径（不受过滤影响）。
	code, body = do(t, mux, st, http.MethodGet, "/admin/api/accounts?status=disabled", nil)
	if code != http.StatusOK {
		t.Fatalf("过滤查询应 200: %d", code)
	}
	stats := body["stats"].(map[string]any)
	if got := num(t, stats["total"]); got != 3 {
		t.Fatalf("stats 应保持全量口径 total=3: %v", stats["total"])
	}
}
