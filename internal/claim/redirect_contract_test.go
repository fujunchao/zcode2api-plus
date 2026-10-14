package claim

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"zcode2api/internal/config"
)

func TestClaimRedirectDoesNotForwardAccountCredentials(t *testing.T) {
	acc := newTestAccount(t)
	var forwarded atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded.Add(1) }))
	t.Cleanup(other.Close)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", other.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
		_, _ = io.WriteString(w, `{"code":0,"data":{"plans":[]}}`)
	}))
	t.Cleanup(origin.Close)
	old := config.ZcodeBillingBase
	config.ZcodeBillingBase = origin.URL
	t.Cleanup(func() { config.ZcodeBillingBase = old })
	_, err := NewService(nil).PreviewPlans(acc)
	if err == nil || forwarded.Load() != 0 {
		t.Fatal("3xx 不能冒充成功或把账号认证发送到重定向目标")
	}
	failure := FailureOutcome(acc, "", err)
	if failure["proxy_failure"] != nil || failure["risk_control"] != nil {
		t.Fatal("拒绝重定向不得被当成代理网络故障或模型风控")
	}
}
