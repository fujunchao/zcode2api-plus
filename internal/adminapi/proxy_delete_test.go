package adminapi

import (
	"net/http"
	"testing"

	"zcode2api/internal/model"
)

// 删除代理后即使剩余代理已被占用，也应共享该代理而不是回退直连。
func TestDeleteProxySharesOccupiedProxy(t *testing.T) {
	mux, st, _ := setup(t)
	removed, err := st.AddProxyProfile("removed", "http://127.0.0.1:18080", true)
	if err != nil {
		t.Fatal(err)
	}
	remaining, err := st.AddProxyProfile("remaining", "http://127.0.0.1:18081", true)
	if err != nil {
		t.Fatal(err)
	}
	var movedID string
	for _, line := range st.ListProxyProfiles() {
		acc, err := st.AddAccount(model.ProviderZai, line.Name, jwtTokenFor(line.Name))
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := st.AssignProxyProfile(acc.ID, line.ID); !ok || err != nil {
			t.Fatalf("绑定代理失败: %v %v", ok, err)
		}
		if line.ID == removed.ID {
			movedID = acc.ID
		}
	}

	code, body := do(t, mux, st, http.MethodDelete, "/admin/api/proxies/"+removed.ID, nil)
	if code != http.StatusOK {
		t.Fatalf("应 200: %d %v", code, body)
	}
	acc := st.FindAny(movedID)
	if acc == nil || acc.ProxyID == nil || *acc.ProxyID != remaining.ID || acc.ProxyURL == nil || *acc.ProxyURL != remaining.URL {
		t.Fatalf("无空闲代理时应共享剩余代理，不应回退直连: account=%+v response=%v", acc, body)
	}
	if num(t, body["reassigned"]) != 1 || num(t, body["direct_fallback"]) != 0 {
		t.Fatalf("应改派 1 个账号且无直连回退: %v", body)
	}
}
