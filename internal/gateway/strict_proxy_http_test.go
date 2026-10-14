package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"zcode2api/internal/model"
)

// 真实发送和完整交付必须接入严格判定，不能只在 Store 直接调用时有效。
func TestStrictProxyHTTPRecordsControlAndRetires(t *testing.T) {
	f := newFixture(t)
	badServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(405)
		_, _ = w.Write([]byte(`{"code":"3012","message":"Request has been blocked due to unusual activity."}`))
	}))
	defer badServer.Close()
	goodServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okUpstreamJSON))
	}))
	defer goodServer.Close()
	bad, err := f.st.AddProxyProfile("bad-control", badServer.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	good, err := f.st.AddProxyProfile("good-control", goodServer.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	a, err := f.st.AddAccount(model.ProviderZai, "synthetic", "sk-synthetic")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{bad.ID, bad.ID, good.ID} {
		_, _ = f.st.Update(a.Provider, a.ID, func(live *model.Account) { live.Status = model.StatusActive; live.CoolingUntil = nil })
		if _, err := f.st.AssignProxyProfile(a.ID, p); err != nil {
			t.Fatal(err)
		}
		f.postWithHeaders(t, msgBody(), map[string]string{"X-Session-Id": "sess-synthetic-control", "X-Zcode-Trace-Id": "trace-synthetic"})
	}
	if _, err := f.st.AddProxyProfile("bad-reimport", bad.URL, true); err == nil {
		t.Fatal("真实HTTP对照成立后，异常代理仍能重导，发送/成功观察没有闭合")
	}
}
