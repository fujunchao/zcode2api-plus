package claim

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/store"
)

func TestNativeClaimRequestCannotDispatchDeletedProxySnapshot(t *testing.T) {
	_ = newTestAccount(t)
	st, err := store.New()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	oldBase := config.ZcodeBillingBase
	config.ZcodeBillingBase = "http://model.example.test"
	defer func() { config.ZcodeBillingBase = oldBase }()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{"code":0,"data":{}}`)
	}))
	defer server.Close()
	p, err := st.AddProxyProfile("本地代理", server.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.AddAccount(model.ProviderZai, "合成账号", "synthetic-token")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AssignProxyProfile(a.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	snapshot := st.FindAny(a.ID)
	svc := NewService(nil)
	svc.Store = st
	if _, err := svc.billingRequest(snapshot, http.MethodGet, "/billing/preview", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.DeleteProxyProfile(p.ID); err != nil {
		t.Fatal(err)
	}
	_, err = svc.billingRequest(snapshot, http.MethodGet, "/billing/preview", nil, nil)
	if err == nil {
		t.Fatal("领取请求必须拒绝已删除线路旧快照")
	}
	if failure, ok := err.(*ClaimError); !ok || failure.endpointUnreachable || failure.proxyFailure != "" {
		t.Fatalf("本地准入拒绝不能冒充代理端点新失败: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatal("已删除线路仍发生领取出站")
	}
}
