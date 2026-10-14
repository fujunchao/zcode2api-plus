package adminapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"zcode2api/internal/model"
	"zcode2api/internal/store"
)

func strictAdminDecision(t *testing.T, st *store.Store) (store.ProxyProfile, store.StrictProxyBlockSummary) {
	t.Helper()
	bad, err := st.AddProxyProfile("strict-bad", "http://127.0.0.1:16591", true)
	if err != nil {
		t.Fatal(err)
	}
	good, err := st.AddProxyProfile("strict-good", "http://127.0.0.1:16592", true)
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.AddAccount(model.ProviderZai, "strict-admin", "sk-synthetic-admin")
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{bad.ID, bad.ID, good.ID} {
		if _, err := st.AssignProxyProfile(a.ID, id); err != nil {
			t.Fatal(err)
		}
		acc := st.FindAny(a.ID)
		o := store.ProxyObservation{RequestID: fmt.Sprintf("request-%d", i), EntryRef: fmt.Sprintf("entry-%d", i), TargetOrigin: "https://model.invalid", TargetPath: "/v1/messages", Method: http.MethodPost, Model: "glm-5.3", ContentRef: "content", ProfileRef: "profile", SessionRef: "session", DeviceRef: "device", At: time.Now(), Status: 405, BusinessCode: "3012"}
		if i == 2 {
			o.Status, o.BusinessCode, o.Complete = 200, "", true
		}
		if _, err := st.ObserveProxyOutcome(acc, st.ProxyEgress(acc), o); err != nil {
			t.Fatal(err)
		}
	}
	status, err := st.StrictProxySummary()
	if err != nil || len(status.Blocks) != 1 {
		t.Fatalf("应完成隔离的严格删除: %+v %v", status, err)
	}
	return bad, status.Blocks[0]
}

func TestStrictProxyAdminRequiresAuthAndExplicitCurrentDecision(t *testing.T) {
	mux, st, _ := setup(t)
	bad, block := strictAdminDecision(t, st)
	path := "/admin/api/proxies/strict/blocks/" + block.Identity + "/unblock"
	for _, tc := range []struct{ method, path string }{{"GET", "/admin/api/proxies/strict"}, {"POST", path}, {"POST", "/admin/api/proxies/strict/recover"}} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("严格状态和恢复必须管理员鉴权: %s %d", tc.path, rec.Code)
		}
	}
	code, _ := do(t, mux, st, "POST", path, map[string]any{"decision_id": block.DecisionID})
	if code != http.StatusBadRequest {
		t.Fatalf("缺少复核确认应拒绝: %d", code)
	}
	code, _ = do(t, mux, st, "POST", path, map[string]any{"decision_id": "stale-decision", "revalidated": true})
	if code != http.StatusConflict {
		t.Fatalf("陈旧决定不能解除当前禁止: %d", code)
	}
	if _, err := st.AddProxyProfile("still-blocked", bad.URL, true); err == nil {
		t.Fatal("失败确认不得移除禁止记录")
	}
	code, result := do(t, mux, st, "POST", path, map[string]any{"decision_id": block.DecisionID, "revalidated": true})
	if code != http.StatusOK || result["restored"] != false || result["requires_import"] != true {
		t.Fatalf("解除不得自动恢复已删除代理: %d %+v", code, result)
	}
	if len(st.ListProxyProfiles()) != 1 {
		t.Fatal("解除不能重建配置")
	}
	code, summary := do(t, mux, st, "GET", "/admin/api/proxies/strict", nil)
	if code != 200 || len(summary["blocks"].([]any)) != 0 || !strings.Contains(fmt.Sprint(summary["audit"]), block.DecisionID) {
		t.Fatalf("解除必须留审计: %d %+v", code, summary)
	}
}

func TestStrictProxyAdminDisableOnlyStopsNewDecisions(t *testing.T) {
	mux, st, _ := setup(t)
	bad, _ := strictAdminDecision(t, st)
	code, _ := do(t, mux, st, "PUT", "/admin/api/settings", map[string]any{store.StrictProxyEnabledKey: false})
	if code != 200 || st.StrictProxyEnabled() {
		t.Fatal("新判定开关未生效")
	}
	if _, err := st.AddProxyProfile("cannot-return", bad.URL, true); err == nil {
		t.Fatal("关闭新判定不能复活已淘汰线路")
	}
	code, _ = do(t, mux, st, "PUT", "/admin/api/settings", map[string]any{store.StrictProxyEnabledKey: "not-bool", "gateway_key": "should-not-commit"})
	if code != 400 || st.GatewayKey() == "should-not-commit" {
		t.Fatal("非法开关必须整批原子拒绝")
	}
}
