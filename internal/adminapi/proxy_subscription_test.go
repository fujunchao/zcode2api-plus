package adminapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSubscriptionAdminConfigContract(t *testing.T) {
	mux, st, _ := setup(t)
	code, body := do(t, mux, st, "GET", "/admin/api/proxies/subscription", nil)
	if code != 200 {
		t.Fatal(code, body)
	}
	c := body["config"].(map[string]any)
	if c["auto"] != false || c["enable_imported"] != false || c["configured"] != false {
		t.Fatal("试运行默认必须关闭")
	}
	for _, p := range []map[string]any{
		{"revision": 0, "url": "http://user:secret@127.0.0.1:1"},
		{"revision": 0, "concurrency": 9}, {"revision": 0, "auto": "true"}, {"revision": 0, "unknown": 1},
		{"revision": 0, "url": ""}, {"revision": 0, "auto": true},
		{"revision": nil}, {"revision": 0, "concurrency": nil}, {"revision": 0, "auto": nil},
	} {
		code, _ = do(t, mux, st, "PUT", "/admin/api/proxies/subscription", p)
		if code != 400 {
			t.Fatalf("应拒绝 %v: %d", p, code)
		}
	}
	code, body = do(t, mux, st, "PUT", "/admin/api/proxies/subscription", map[string]any{"revision": 0, "url": "http://127.0.0.1:1/api/repo/private-token.json?key=other-secret"})
	if code != 200 {
		t.Fatal(code, body)
	}
	for _, path := range []string{"/admin/api/proxies/subscription", "/admin/api/proxies/subscription/runs", "/admin/api/proxies/subscription/blocks"} {
		code, body = do(t, mux, st, "GET", path, nil)
		raw, _ := json.Marshal(body)
		if code != 200 || strings.Contains(string(raw), "private-token") || strings.Contains(string(raw), "other-secret") {
			t.Fatalf("泄露订阅地址 %s", raw)
		}
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("接口未鉴权 %s: %d", path, w.Code)
		}
	}
	code, _ = do(t, mux, st, "PUT", "/admin/api/proxies/subscription", map[string]any{"revision": 0, "auto": false})
	if code != 409 {
		t.Fatal("旧版本写入应冲突")
	}
	code, _ = do(t, mux, st, "PUT", "/admin/api/proxies/subscription", map[string]any{"revision": 1, "interval_minutes": 15})
	if code != 200 {
		t.Fatal("未提供 URL 应保留旧地址")
	}
	snap, _ := st.SubscriptionSnapshot()
	if !strings.Contains(snap.State.Config.URL, "private-token") {
		t.Fatal("地址被清空")
	}
	code, _ = do(t, mux, st, "PUT", "/admin/api/proxies/subscription", map[string]any{"revision": 2, "clear_url": true})
	if code != 200 {
		t.Fatal("显式清空失败")
	}
}
