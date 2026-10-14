package oauth

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestOAuthRedirectDoesNotForwardCodeOrBusinessToken(t *testing.T) {
	for _, endpoint := range []string{"token", "business"} {
		t.Run(endpoint, func(t *testing.T) {
			var forwarded atomic.Int32
			other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded.Add(1) }))
			t.Cleanup(other.Close)
			host := "api.z.ai"
			if endpoint == "token" {
				host = "zcode.z.ai"
			}
			routeOAuthToLocalTLS(t, host, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", other.URL)
				w.WriteHeader(http.StatusTemporaryRedirect)
				_, _ = io.WriteString(w, `{"code":0,"data":{"token":"synthetic-jwt"}}`)
			}))
			var err error
			if endpoint == "token" {
				flow := NewFlow()
				_, _, _ = flow.Init()
				_, err = flow.ExchangeCode("synthetic-code", flow.State, "")
			} else {
				_, err = ExchangeAPIKey("synthetic-access-token", "")
			}
			if err == nil || forwarded.Load() != 0 {
				t.Fatal("OAuth 307 不得转发授权码/业务 token 或伪装兑换成功")
			}
		})
	}
}
