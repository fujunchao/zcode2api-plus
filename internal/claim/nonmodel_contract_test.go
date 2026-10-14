package claim

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zcode2api/internal/captcha"
	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

// 真实 HTTP 入口同时校验目标、账号认证、来源档案和直连/代理传输，不只测造头函数。
func TestClaimNonmodelHTTPContract(t *testing.T) {
	for _, proxied := range []bool{false, true} {
		for _, mode := range []string{"jwt", "api_key"} {
			t.Run(mode+map[bool]string{false: "/direct", true: "/proxy"}[proxied], func(t *testing.T) {
				acc := newTestAccount(t)
				acc.Mode = mode
				apiKey := "synthetic-api-key"
				device := "synthetic-device"
				acc.APIKey, acc.VirtualDeviceMid = &apiKey, &device
				seen := make(chan *http.Request, 2)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					seen <- r.Clone(context.Background())
					_, _ = io.WriteString(w, `{"code":0,"data":{"plans":[]}}`)
				}))
				t.Cleanup(server.Close)
				oldBase := config.ZcodeBillingBase
				config.ZcodeBillingBase = server.URL
				if proxied {
					config.ZcodeBillingBase = "http://billing.example.test/api/v1/zcode-plan"
					acc.ProxyURL = &server.URL
				}
				t.Cleanup(func() { config.ZcodeBillingBase = oldBase })
				svc := NewService(nil)
				if _, err := svc.PreviewPlans(acc); err != nil {
					t.Fatal(err)
				}
				r := <-seen
				if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/billing/preview") {
					t.Fatalf("领取预览目标错误: %s %s", r.Method, r.URL.Path)
				}
				if proxied && (r.Host != "billing.example.test" || !r.URL.IsAbs()) {
					t.Fatal("代理必须观察到原目标的绝对 URL，不得绕过代理直接连接")
				}
				if r.URL.Query().Get("app_version") != config.ZcodeClientVersion || r.URL.Query().Get("platform") != config.ZcodeQueryPlatform() {
					t.Fatal("预览查询参数未使用统一档案")
				}
				assertNonmodelClaimHeaders(t, r.Header, acc)
				if mode == "jwt" {
					cm := captcha.NewManager()
					cm.SetConfigProvider(func(context.Context) (captcha.Config, error) { return captcha.Config{Enabled: true}, nil })
					if err := cm.SetManualParam("synthetic-captcha", "synthetic-region"); err != nil {
						t.Fatal(err)
					}
					svc.Captcha = cm
					if _, err := svc.Claim(acc, "synthetic-plan"); err != nil {
						t.Fatal(err)
					}
					r = <-seen
					assertNonmodelClaimHeaders(t, r.Header, acc)
					if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/billing/claim") || r.Header.Get(captchaHeader) != "synthetic-captcha" || r.Header.Get(captchaRegionHeader) != "synthetic-region" {
						t.Fatal("领取不能丢失方法、目标或验证码契约")
					}
				}
			})
		}
	}
}

func assertNonmodelClaimHeaders(t *testing.T, headers http.Header, acc *model.Account) {
	t.Helper()
	for key, value := range config.ZcodeSourceHeaders() {
		if headers.Get(key) != value {
			t.Errorf("非模型来源头 %s 不匹配档案", key)
		}
	}
	if headers.Get("X-Device-Mid") != *acc.VirtualDeviceMid || headers.Get("Content-Type") != "application/json" {
		t.Error("计费请求缺少账号设备或 JSON 头")
	}
	if acc.Mode == "jwt" {
		if headers.Get("Authorization") != "Bearer "+*acc.JWTToken || headers.Get("x-api-key") != "" {
			t.Error("计费 JWT 仅发 Bearer，不能套用模型双鉴权")
		}
	} else if headers.Get("Authorization") != "" || headers.Get("x-api-key") != *acc.APIKey {
		t.Error("计费 API Key 只发 x-api-key，不能混用旧 JWT")
	}
	for _, key := range []string{"X-ZCode-Agent", "X-Zcode-Trace-Id", "X-Session-Id", "Cookie"} {
		if headers.Get(key) != "" {
			t.Errorf("非模型请求不得携带 %s", key)
		}
	}
}

func TestActivationHTTPContractIsUnauthenticated(t *testing.T) {
	acc := newTestAccount(t)
	seen := make(chan *http.Request, len(ActivationElements))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 16 || body["user_id"] != "u123" {
			t.Error("激活事件体不是已知的合成用户与固定字段集")
		}
		seen <- r.Clone(context.Background())
		_, _ = io.WriteString(w, `{"code":0}`)
	}))
	t.Cleanup(server.Close)
	old := EventReportURL
	EventReportURL = "http://telemetry.example.test/api/v1/event/report"
	t.Cleanup(func() { EventReportURL = old })
	acc.ProxyURL = &server.URL
	if err := NewService(nil).ReportActivationEvents(acc); err != "" {
		t.Fatal(err)
	}
	for range ActivationElements {
		r := <-seen
		if r.Method != http.MethodPost || r.Host != "telemetry.example.test" || r.URL.Path != "/api/v1/event/report" {
			t.Fatal("遥测必须使用同一账号代理，并只发送到配置的遥测目标")
		}
		for _, key := range []string{"Authorization", "x-api-key", "X-Coding-Plan-Api-Key", "Cookie", "X-ZCode-Agent"} {
			if r.Header.Get(key) != "" {
				t.Errorf("公开遥测不应携带账号认证或模型头 %s", key)
			}
		}
	}
}

func TestClaimEmptyProxyUsesExplicitDirect(t *testing.T) {
	client, err := NewService(nil).clientFor(newTestAccount(t))
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := client.(*http.Client).Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatal("直连/自动回退不得继承环境代理")
	}
}
