package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"zcode2api/internal/config"
	"zcode2api/internal/proxy"
	"zcode2api/internal/store"
)

func TestOAuthTokenNativeHTTPContract(t *testing.T) {
	flow := NewFlow()
	if _, _, err := flow.Init(); err != nil {
		t.Fatal(err)
	}
	routeOAuthToLocalTLS(t, "zcode.z.ai", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Host != "zcode.z.ai" || r.URL.Path != "/api/v1/oauth/token" {
			t.Error("OAuth token 不得偏离固定目标与方法")
		}
		var payload map[string]string
		if json.NewDecoder(r.Body).Decode(&payload) != nil || len(payload) != 4 || payload["provider"] != "zai" || payload["code"] != "synthetic-code" || payload["state"] != flow.State || payload["redirect_uri"] != registeredRedirectURI {
			t.Error("OAuth token 必须保留 provider/code/state/redirect_uri 契约")
		}
		for key, value := range config.ZcodeSourceHeaders() {
			if r.Header.Get(key) != value {
				t.Errorf("同源 OAuth token 缺少来源头 %s", key)
			}
		}
		for _, key := range []string{"Authorization", "x-api-key", "X-Coding-Plan-Api-Key", "Cookie", "X-ZCode-Agent"} {
			if r.Header.Get(key) != "" {
				t.Errorf("OAuth 授权码兑换不可混入已有账号认证 %s", key)
			}
		}
		_, _ = io.WriteString(w, `{"code":0,"data":{"token":"synthetic-jwt"}}`)
	}))
	result, err := flow.ExchangeCode("synthetic-code", flow.State, "")
	if err != nil || result.Token != "synthetic-jwt" {
		t.Fatalf("合成 OAuth 交换失败: %v", err)
	}
}

func TestOAuthBusinessTargetDoesNotReceiveZcodeSourceHeaders(t *testing.T) {
	steps := batch2KeySteps(false)
	var calls atomic.Int32
	routeAPIKeyExchangeToLocalTLS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		step := int(calls.Add(1)) - 1
		if step >= len(steps) {
			t.Error("业务链发出多余请求")
			w.WriteHeader(500)
			return
		}
		for key := range config.ZcodeSourceHeaders() {
			if key != "User-Agent" && r.Header.Get(key) != "" {
				t.Errorf("api.z.ai 不是 ZCode 同源，不应套用 %s", key)
			}
		}
		for _, key := range []string{"X-Device-Mid", "x-api-key", "Cookie", "X-ZCode-Agent"} {
			if r.Header.Get(key) != "" {
				t.Errorf("业务认证链不应带 %s", key)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": steps[step].data})
	}))
	key, err := ExchangeAPIKey("synthetic-access-token", "")
	if err != nil || key == "" || int(calls.Load()) != len(steps) {
		t.Fatalf("独立业务认证链失败: %v", err)
	}
}

func TestOAuthAPIKeyEveryStepRechecksDispatchAndReleases(t *testing.T) {
	for blockedAt := range len(batch2KeySteps(false)) {
		t.Run(string(rune('0'+blockedAt)), func(t *testing.T) {
			steps := batch2KeySteps(false)
			var calls, permits, releases atomic.Int32
			routeAPIKeyExchangeToLocalTLS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				index := int(calls.Add(1)) - 1
				if index >= len(steps) {
					t.Error("出现未获许可的业务请求")
					w.WriteHeader(500)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": steps[index].data})
			}))
			ctx := WithDispatchGuard(context.Background(), func() (func(), error) {
				if int(permits.Add(1))-1 == blockedAt {
					return nil, store.ErrProxyRetired
				}
				return func() { releases.Add(1) }, nil
			})
			key, err := ExchangeAPIKeyContext(ctx, "synthetic-access-token", "")
			if key != "" || !errors.Is(err, store.ErrProxyRetired) || int(calls.Load()) != blockedAt || int(releases.Load()) != blockedAt {
				t.Fatalf("每一步都须准入且许可只释放一次：calls=%d permits=%d releases=%d err=%v", calls.Load(), permits.Load(), releases.Load(), err)
			}
			if proxy.IsEndpointUnreachable(ctx, err) {
				t.Fatal("本地退役拒绝不能误归为代理网络失败")
			}
		})
	}
}
