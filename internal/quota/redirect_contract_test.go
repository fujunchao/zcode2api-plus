package quota

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

func TestQuotaRedirectDoesNotForwardAccountCredentials(t *testing.T) {
	svc, st, _ := setup(t)
	t.Cleanup(svc.Close)
	svc.Client = nil
	acc, err := st.AddAccount(model.ProviderZai, "合成账号", "synthetic-api-key")
	if err != nil {
		t.Fatal(err)
	}
	var forwarded atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded.Add(1) }))
	t.Cleanup(other.Close)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(origin.Close)
	old := config.ZcodeBillingBase
	config.ZcodeBillingBase = origin.URL
	t.Cleanup(func() { config.ZcodeBillingBase = old })
	result := svc.FetchQuota(acc)
	if result["error"] == nil || result["proxy_failure"] != nil || forwarded.Load() != 0 {
		t.Fatal("额度重定向须保持查询错误，不能跟随或误删代理")
	}
}
