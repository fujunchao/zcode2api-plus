// 「手动恢复」端点的 handler 测试：判定边界（404/400/200）、错误映射、批量
// 逐条明细与严格模式、路由不被 {account_id} 通配吞、鉴权。
// store 层语义测试见 internal/store/recover_test.go。
package adminapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"zcode2api/internal/model"
	"zcode2api/internal/store"
)

// seedRiskControlInvalid 造一个「风控失效」终态账号（连击超档后的形态）。
func seedRiskControlInvalid(t *testing.T, st *store.Store, name, secret string) *model.Account {
	t.Helper()
	acc, err := st.AddAccount(model.ProviderZai, name, secret)
	if err != nil {
		t.Fatal(err)
	}
	kind := model.ErrorKindRiskControl
	msg := "request has been blocked due to unusual activity."
	now := float64(time.Now().UnixNano()) / 1e9
	if _, err := st.Update(model.ProviderZai, acc.ID, func(a *model.Account) {
		a.Status = model.StatusInvalid
		a.LastError = &msg
		a.LastErrorKind = &kind
		a.LastErrorAt = &now
		a.RiskControlStreak = 4
	}); err != nil {
		t.Fatal(err)
	}
	return acc
}

func TestRecoverAccountEndpoint(t *testing.T) {
	mux, st, _ := setup(t)
	acc := seedRiskControlInvalid(t, st, "risk", "risk-secret")

	code, body := do(t, mux, st, http.MethodPost, "/admin/api/accounts/"+acc.ID+"/recover", nil)
	if code != http.StatusOK {
		t.Fatalf("恢复应 200: %d %v", code, body)
	}
	if ok, _ := body["ok"].(bool); !ok {
		t.Fatalf("响应应 ok=true: %v", body)
	}
	got := st.FindAny(acc.ID)
	if got.Status != model.StatusActive || got.RiskControlStreak != 0 || got.LastErrorKind != nil {
		t.Fatalf("账号应已恢复为正常: %+v", got)
	}
}

func TestRecoverAccountEndpointNotFound(t *testing.T) {
	mux, st, _ := setup(t)
	code, body := do(t, mux, st, http.MethodPost, "/admin/api/accounts/no-such-id/recover", nil)
	if code != http.StatusNotFound {
		t.Fatalf("不存在应 404: %d %v", code, body)
	}
	if detail := str(t, body["detail"]); detail != "账号不存在" {
		t.Fatalf("detail 不符: %q", detail)
	}
}

func TestRecoverAccountEndpointRejectsNonRecoverable(t *testing.T) {
	mux, st, _ := setup(t)
	normal, err := st.AddAccount(model.ProviderZai, "normal", "normal-secret")
	if err != nil {
		t.Fatal(err)
	}

	code, body := do(t, mux, st, http.MethodPost, "/admin/api/accounts/"+normal.ID+"/recover", nil)
	if code != http.StatusBadRequest {
		t.Fatalf("非风控失效应 400: %d %v", code, body)
	}
	if detail := str(t, body["detail"]); detail != "账号非风控失效状态" {
		t.Fatalf("detail 不符: %q", detail)
	}
	if got := st.FindAny(normal.ID); got.Status != model.StatusActive {
		t.Fatalf("被拒绝的账号状态不得变化: %s", got.Status)
	}
}

func TestRecoverAccountEndpointRejectsArchived(t *testing.T) {
	mux, st, _ := setup(t)
	acc := seedRiskControlInvalid(t, st, "archived", "archived-secret")
	if _, err := st.SetArchived(model.ProviderZai, acc.ID, true); err != nil {
		t.Fatal(err)
	}

	code, body := do(t, mux, st, http.MethodPost, "/admin/api/accounts/"+acc.ID+"/recover", nil)
	if code != http.StatusBadRequest {
		t.Fatalf("归档账号应 400: %d %v", code, body)
	}
	if detail := str(t, body["detail"]); detail != "账号已归档，请先恢复到账号池再恢复" {
		t.Fatalf("detail 不符: %q", detail)
	}
}

func TestBatchRecoverEndpoint(t *testing.T) {
	mux, st, _ := setup(t)
	risk := seedRiskControlInvalid(t, st, "risk", "risk-secret")
	normal, err := st.AddAccount(model.ProviderZai, "normal", "normal-secret")
	if err != nil {
		t.Fatal(err)
	}

	// 路由证据：若 /batch/recover 被 {account_id}/recover 吞掉，这里会是 404
	// 「账号不存在」；返回批量汇总体即证明命中批量 handler。
	code, body := do(t, mux, st, http.MethodPost, "/admin/api/accounts/batch/recover", map[string]any{
		"ids":        []any{risk.ID, normal.ID, "missing-id"},
		"missing_ok": true,
	})
	if code != http.StatusOK {
		t.Fatalf("批量恢复应 200: %d %v", code, body)
	}
	if got := num(t, body["total"]); got != 3 {
		t.Fatalf("total 应为 3: %v", body["total"])
	}
	if got := num(t, body["succeeded"]); got != 1 {
		t.Fatalf("succeeded 应为 1: %v", body["succeeded"])
	}
	if got := num(t, body["not_found"]); got != 1 {
		t.Fatalf("not_found 应为 1: %v", body["not_found"])
	}
	if got := num(t, body["failed"]); got != 1 {
		t.Fatalf("failed 应为 1: %v", body["failed"])
	}
	items := body["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("明细应有 3 条: %d", len(items))
	}
	if first := items[0].(map[string]any); first["status"] != "ok" {
		t.Fatalf("第 1 条应 ok: %v", first)
	}
	if second := items[1].(map[string]any); second["status"] != "error" || second["message"] != "账号非风控失效状态" {
		t.Fatalf("第 2 条应 error 且带原因: %v", second)
	}
	if third := items[2].(map[string]any); third["status"] != "not_found" {
		t.Fatalf("第 3 条应 not_found: %v", third)
	}
	if got := st.FindAny(risk.ID); got.Status != model.StatusActive {
		t.Fatalf("成功项应已恢复: %+v", got)
	}
	if got := st.FindAny(normal.ID); got.Status != model.StatusActive {
		t.Fatalf("非可恢复项不得被改动: %+v", got)
	}
}

func TestBatchRecoverEndpointStrictMissing(t *testing.T) {
	mux, st, _ := setup(t)
	risk := seedRiskControlInvalid(t, st, "risk", "risk-secret")

	// 缺省 missing_ok=false：含缺失 ID 整体拒绝、零改动。
	code, body := do(t, mux, st, http.MethodPost, "/admin/api/accounts/batch/recover", map[string]any{
		"ids": []any{risk.ID, "missing-id"},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("严格模式应 400: %d %v", code, body)
	}
	if got := st.FindAny(risk.ID); got.Status != model.StatusInvalid {
		t.Fatalf("严格模式拒绝时不得有改动: %s", got.Status)
	}
}

func TestRecoverEndpointRequiresAdminKey(t *testing.T) {
	mux, st, _ := setup(t)
	acc := seedRiskControlInvalid(t, st, "risk", "risk-secret")

	for _, path := range []string{
		"/admin/api/accounts/" + acc.ID + "/recover",
		"/admin/api/accounts/batch/recover",
	} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if res := rec.Result(); res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s 缺后台密钥应 401: %d", path, res.StatusCode)
		}
	}
}
