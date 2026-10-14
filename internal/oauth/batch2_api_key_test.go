package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"zcode2api/internal/proxy"
)

// 所有 api.z.ai 请求只允许连接本地 TLS 服务；不改生产端点，不发送真实账号材料。
func routeAPIKeyExchangeToLocalTLS(t *testing.T, handler http.Handler) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.Proxy = nil
	host, _, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	transport.TLSClientConfig.ServerName = host
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr != "api.z.ai:443" {
			return nil, fmt.Errorf("回归拒绝非预期的网络目标：%s", addr)
		}
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	old := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = old; transport.CloseIdleConnections() })
}

func TestBatch2APIKeyBusinessUnauthorizedIsNotMissingOrganization(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	routeAPIKeyExchangeToLocalTLS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/auth/z/login":
			_, _ = io.WriteString(w, `{"code":0,"data":{"access_token":"synthetic-business-token"}}`)
		case "/api/biz/customer/getCustomerInfo":
			_, _ = io.WriteString(w, `{"code":401,"msg":"synthetic-business-token must not leak"}`)
		default:
			t.Errorf("业务失败后不应继续请求：%s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	key, err := ExchangeAPIKey("synthetic-access-token", "")
	if key != "" || err == nil || !strings.Contains(err.Error(), "401") ||
		strings.Contains(err.Error(), "找不到可用的机构") || strings.Contains(err.Error(), "synthetic-") {
		t.Fatalf("HTTP 200 业务未授权必须准确归因而不泄漏材料：key=%q err=%v", key, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("业务未授权后必须终止兑换，不创建或复制 Key：%v", calls)
	}
}

type batch2KeyStep struct {
	method, path string
	data         any
}

func batch2KeySteps(existing bool) []batch2KeyStep {
	const base = "/api/biz/v1/organization/synthetic-org/projects/synthetic-project/api_keys"
	keys := []any{}
	if existing {
		keys = append(keys, map[string]any{"name": "zcode-api-key", "apiKey": "synthetic-key-id"})
	}
	steps := []batch2KeyStep{
		{http.MethodPost, "/api/auth/z/login", map[string]any{"access_token": "synthetic-business-token"}},
		{http.MethodGet, "/api/biz/customer/getCustomerInfo", map[string]any{
			"organizations": []any{map[string]any{
				"organizationName": "默认机构", "organizationId": "synthetic-org",
				"projects": []any{map[string]any{"projectName": "默认项目", "projectId": "synthetic-project"}},
			}},
		}},
		{http.MethodGet, base, keys},
	}
	if !existing {
		steps = append(steps, batch2KeyStep{http.MethodPost, base, map[string]any{"apiKey": "synthetic-key-id"}})
	}
	return append(steps, batch2KeyStep{http.MethodGet, base + "/copy/synthetic-key-id", map[string]any{"secretKey": "synthetic-secret-value"}})
}

// tweak 只修改模拟服务响应，整个业务过程始终从公开 ExchangeAPIKey 进入。
func runBatch2KeyExchange(t *testing.T, existing bool, tweak func(int, map[string]any) (int, string)) (string, error, int) {
	t.Helper()
	steps := batch2KeySteps(existing)
	var mu sync.Mutex
	calls := 0
	routeAPIKeyExchangeToLocalTLS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			t.Errorf("读取合成请求失败：%v", readErr)
		}
		mu.Lock()
		index := calls
		calls++
		mu.Unlock()
		if index >= len(steps) {
			t.Error("兑换发出了额外请求")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		step := steps[index]
		if r.Host != "api.z.ai" || r.Method != step.method || r.URL.Path != step.path {
			t.Errorf("兑换步骤偏离公开 HTTP 协议：%s %s %s", r.Host, r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		// 3.15.3 按端点区分认证；customerInfo 用裸业务 token，Key 管理仍用 Bearer。
		wantAuthorization := "Bearer synthetic-business-token"
		switch step.path {
		case "/api/auth/z/login":
			wantAuthorization = ""
			var login map[string]string
			if json.Unmarshal(body, &login) != nil || login["token"] != "synthetic-access-token" {
				t.Error("业务登录必须使用原 OAuth access token")
			}
		case "/api/biz/customer/getCustomerInfo":
			wantAuthorization = "synthetic-business-token"
		}
		if r.Header.Get("Authorization") != wantAuthorization {
			t.Errorf("端点 %s 的认证格式错误：got=%q want=%q", step.path, r.Header.Get("Authorization"), wantAuthorization)
		}
		payload := map[string]any{"code": 0, "data": step.data}
		status, raw := http.StatusOK, ""
		if tweak != nil {
			status, raw = tweak(index, payload)
		}
		if raw == "" {
			body, err := json.Marshal(payload)
			if err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			raw = string(body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, raw)
	}))
	key, err := ExchangeAPIKey("synthetic-access-token", "")
	mu.Lock()
	count := calls
	mu.Unlock()
	return key, err, count
}

func TestBatch2APIKeyBusinessFailuresStopEveryStep(t *testing.T) {
	for stage := range 5 {
		for _, tc := range []struct {
			name         string
			code         any
			unauthorized bool
		}{
			{"numeric_401", 401, true},
			{"string_401", "401", true},
			{"other_failure", 3401, false},
			{"opaque_code", "synthetic-business-token\nprivate", false},
		} {
			t.Run(fmt.Sprintf("step=%d/%s", stage, tc.name), func(t *testing.T) {
				key, err, calls := runBatch2KeyExchange(t, false, func(index int, payload map[string]any) (int, string) {
					if index == stage {
						// 故意保留貌似合法的 data，业务失败不能继续使用它。
						payload["code"] = tc.code
						payload["msg"] = "synthetic-access-token synthetic-business-token must not leak"
					}
					return http.StatusOK, ""
				})
				if key != "" || err == nil || calls != stage+1 {
					t.Fatalf("业务失败必须立即终止，不继续创建/复制或返回 Key：stage=%d calls=%d key=%q err=%v", stage, calls, key, err)
				}
				if errors.Is(err, ErrBusinessUnauthorized) != tc.unauthorized {
					t.Fatalf("401 应与其他业务错误明确区分：%v", err)
				}
				if strings.Contains(err.Error(), "synthetic-") || proxy.IsEndpointUnreachable(context.Background(), err) {
					t.Fatalf("业务失败不能泄漏材料或被归为代理不可达：%v", err)
				}
				if !tc.unauthorized && tc.name == "other_failure" && !strings.Contains(err.Error(), "3401") {
					t.Fatalf("安全的数字业务码应保留：%v", err)
				}
			})
		}
	}
}

func TestBatch2APIKeySuccessShapesAndExistingKey(t *testing.T) {
	for _, existing := range []bool{false, true} {
		for _, shape := range []string{"missing", "numeric_zero", "string_zero", "null"} {
			t.Run(fmt.Sprintf("existing=%t/%s", existing, shape), func(t *testing.T) {
				key, err, calls := runBatch2KeyExchange(t, existing, func(_ int, payload map[string]any) (int, string) {
					switch shape {
					case "missing":
						delete(payload, "code")
					case "string_zero":
						payload["code"] = "0"
					case "null":
						payload["code"] = nil
					}
					return http.StatusOK, ""
				})
				wantCalls := 5
				if existing {
					wantCalls = 4
				}
				if err != nil || key != "synthetic-key-id.synthetic-secret-value" || calls != wantCalls {
					t.Fatalf("合法成功或已有 Key 复用不能改变：calls=%d key=%q err=%v", calls, key, err)
				}
			})
		}
	}
}

func TestBatch2APIKeyOtherFailuresRemainDistinct(t *testing.T) {
	for _, kind := range []string{"empty_organizations", "malformed_json", "http_failure"} {
		t.Run(kind, func(t *testing.T) {
			key, err, calls := runBatch2KeyExchange(t, false, func(index int, payload map[string]any) (int, string) {
				if index == 1 {
					switch kind {
					case "empty_organizations":
						payload["data"] = map[string]any{"organizations": []any{}}
					case "malformed_json":
						return http.StatusOK, "{"
					case "http_failure":
						return http.StatusServiceUnavailable, `{"message":"temporarily unavailable"}`
					}
				}
				return http.StatusOK, ""
			})
			if key != "" || err == nil || calls != 2 || errors.Is(err, ErrBusinessUnauthorized) {
				t.Fatalf("其他失败不应伪装业务 401 或继续兑换：calls=%d err=%v", calls, err)
			}
			want := map[string]string{"empty_organizations": "找不到可用的机构", "malformed_json": "无效 JSON", "http_failure": "503"}[kind]
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("原有诊断应保留：want=%q err=%v", want, err)
			}
		})
	}
}
