package claim

import (
	"context"
	"io"
	"net/http"
	"testing"
)

type unreachableBilling struct{}

func (unreachableBilling) Do(*http.Request) (*http.Response, error) { return nil, io.EOF }

func TestProxyEndpointInjectedClientCannotBlameAccountProxy(t *testing.T) {
	a := newTestAccount(t)
	id, raw := "named-line", "http://127.0.0.1:18080"
	a.ProxyID, a.ProxyURL = &id, &raw
	svc := NewService(newSolvedManager(t))
	svc.Client = unreachableBilling{}
	_, err := svc.Claim(a, "promo")
	outcome := FailureOutcome(a, "promo", err)
	if outcome["proxy_failure"] != nil || outcome["endpoint_unreachable"] != true {
		t.Fatal("绕过账号线路的注入客户端不能指认命名代理故障")
	}
}

func TestProxyEndpointDirectRequestCannotBlameNamedProxy(t *testing.T) {
	a := newTestAccount(t)
	svc := NewServiceContext(context.Background(), newSolvedManager(t))
	for _, shape := range []string{"direct", "custom"} {
		if shape == "custom" {
			raw := "http://127.0.0.1:18080"
			a.ProxyURL = &raw
		}
		failure := svc.proxyFailureError(a, &ClaimError{riskControl: true}, "claim_suspicious")
		if FailureOutcome(a, "promo", failure)["proxy_failure"] != nil {
			t.Fatal("直连或手工地址不能指认代理池中的命名线路")
		}
	}
}
