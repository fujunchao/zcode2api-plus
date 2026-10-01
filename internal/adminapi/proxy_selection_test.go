package adminapi

import (
	"fmt"
	"net/http"
	"testing"

	"zcode2api/internal/model"
)

func TestAccountCreationEntrypointsShareLeastLoadedProxy(t *testing.T) {
	for _, entry := range []string{"add", "batch", "import", "login"} {
		t.Run(entry, func(t *testing.T) {
			mux, st, _ := setup(t)
			_ = st.SetSetting("claim_auto_enabled", "false")
			busy, _ := st.AddProxyProfile("busy", "http://127.0.0.1:18080", true)
			light, _ := st.AddProxyProfile("light", "http://127.0.0.1:18081", true)
			_, _ = st.AddProxyProfile("disabled", "http://127.0.0.1:18082", false)
			for i, pid := range []string{busy.ID, busy.ID, busy.ID, light.ID} {
				a, err := st.AddAccount(model.ProviderZai, fmt.Sprintf("old-%d", i), fmt.Sprintf("sk-old-%d", i))
				if err != nil {
					t.Fatal(err)
				}
				if _, err = st.AssignProxyProfile(a.ID, pid); err != nil {
					t.Fatal(err)
				}
			}
			if entry == "login" {
				h := &Handler{Store: st}
				url, id, auto, apiErr := h.resolveLoginProxy(map[string]any{"proxy_id": proxyIDAuto})
				if apiErr != nil || !auto || id != light.ID || url != light.URL {
					t.Fatalf("自动登录应共享最少绑定线路: %q %q %v %v", url, id, auto, apiErr)
				}
				return
			}
			path := "/admin/api/accounts"
			payload := map[string]any{"tokens": []any{"sk-new-1", "sk-new-2"}}
			items := []any{map[string]any{"secret": "sk-new-1"}, map[string]any{"secret": "sk-new-2"}}
			switch entry {
			case "batch":
				path, payload = "/admin/api/accounts/batch/add", map[string]any{"items": items}
			case "import":
				path, payload = "/admin/api/import", map[string]any{"providers": map[string]any{model.ProviderZai: items}}
			}
			code, body := do(t, mux, st, http.MethodPost, path, payload)
			if code != http.StatusOK || body["assigned"] != float64(2) || body["direct_fallback"] != float64(0) {
				t.Fatalf("无空闲时新号应全部共享分配: %d %v", code, body)
			}
			for _, a := range st.ListAccounts("") {
				if a.Secret() != "sk-new-1" && a.Secret() != "sk-new-2" {
					continue
				}
				if a.ProxyID == nil || *a.ProxyID != light.ID || a.ProxyURL == nil || *a.ProxyURL != light.URL {
					t.Fatalf("两个新号应依次补到负载较低的线路: %s", a.ID)
				}
			}
			// 重复新增/导入不能重新分配已经存在的账号。
			code, body = do(t, mux, st, http.MethodPost, path, payload)
			if code != http.StatusOK || body["assigned"] != float64(0) {
				t.Fatalf("重复入池不得再次分配: %d %v", code, body)
			}
		})
	}
}
