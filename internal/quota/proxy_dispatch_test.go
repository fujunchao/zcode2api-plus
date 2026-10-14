package quota

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"zcode2api/internal/model"
	"zcode2api/internal/store"
)

func TestNativeQuotaClientCannotDispatchDeletedProxySnapshot(t *testing.T) {
	svc, st, _ := setup(t)
	defer svc.Close()
	svc.Client = nil
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
	client, err := svc.clientFor(st.FindAny(a.ID))
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://model.example.test/billing/balance", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if _, _, err := st.DeleteProxyProfile(p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Do(req); !errors.Is(err, store.ErrProxyRetired) {
		t.Fatalf("额度原生客户端必须重新校验旧快照: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatal("已删除线路仍发生额度出站")
	}
}
