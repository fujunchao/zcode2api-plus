package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"zcode2api/internal/captcha"
	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

// 模拟官方 3.14.4 配置：验证码总开关仍开，但模型请求已经无需验证。
func TestJWTModelCaptchaSkipWithoutSolver(t *testing.T) {
	oldData := config.DataDir
	config.DataDir = t.TempDir()
	t.Cleanup(func() { config.DataDir = oldData })
	f := newFixture(t)
	f.cm.SetConfigProvider(func(context.Context) (captcha.Config, error) {
		return captcha.Config{Enabled: true, SkipModelRequest: true}, nil
	})
	f.respond = jsonResp(200, okUpstreamJSON)
	acc, err := f.st.AddAccount(model.ProviderZai, "skip-model", "header.payload.sig")
	if err != nil {
		t.Fatal(err)
	}
	status, raw := f.postWithHeaders(t, msgBody(), map[string]string{
		"X-Aliyun-Captcha-Verify-Param":  "downstream-token",
		"X-Aliyun-Captcha-Verify-Region": "downstream-region",
		"X-Session-Id":                   "sess_model-skip",
	})
	if status != 200 || raw != okUpstreamJSON {
		t.Fatalf("模型跳过验证码时应无 solver 成功，实际 %d %s", status, raw)
	}
	if f.callCount() != 1 {
		t.Fatalf("应请求模型 1 次，实际 %d", f.callCount())
	}
	call := f.lastCall()
	for _, key := range []string{"X-Aliyun-Captcha-Verify-Param", "X-Aliyun-Captcha-Verify-Region"} {
		if got := call.Header.Get(key); got != "" {
			t.Fatalf("模型跳过时不得携带或透传 %s: %q", key, got)
		}
	}
	if call.Path != "/zai" || call.Header.Get("Authorization") != "Bearer header.payload.sig" || call.Header.Get("X-Api-Key") != "header.payload.sig" {
		t.Fatalf("JWT 路径及鉴权必须保留: %s %v", call.Path, call.Header)
	}
	system, ok := call.Body["system"].([]any)
	if !ok || len(system) != 3 {
		t.Fatalf("跳过验证码不能丢失 JWT 的三个 system 块: %v", call.Body["system"])
	}
	metadata, ok := call.Body["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("必须保留设备 metadata: %v", call.Body)
	}
	rawID, _ := metadata["user_id"].(string)
	var identity map[string]any
	if err := json.Unmarshal([]byte(rawID), &identity); err != nil {
		t.Fatal(err)
	}
	if acc.VirtualDeviceMid == nil || identity["device_id"] != *acc.VirtualDeviceMid || identity["session_id"] != "model-skip" || call.Header.Get("X-Session-Id") != "model-skip" {
		t.Fatalf("JWT 设备及会话身份必须保留: %v", identity)
	}
}

type modelSkipProbeSolver struct{ calls atomic.Int32 }

func (s *modelSkipProbeSolver) Solve(context.Context, captcha.Config) (string, error) {
	s.calls.Add(1)
	return "", errors.New("模型跳过时不应调用求解器")
}

func (s *modelSkipProbeSolver) Close() error { return nil }

func TestJWTModelCaptchaSkipStreamAndRetries(t *testing.T) {
	for _, tt := range []struct {
		name        string
		firstStatus int
		firstBody   string
		challenge   bool
	}{
		{"流式成功", 0, "", false},
		{"并发限制重试", 429, `{"code":3010}`, false},
		{"瞬时限流重试", 429, `{"error":{"message":"rate limited"}}`, false},
		{"平台过载重试", 529, `{"code":1305}`, false},
		{"HTTP验证码挑战", 403, `{"code":3007}`, true},
		{"业务验证码挑战", 200, `{"code":3007}`, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			old, oldData := config.CaptchaBrowserEnabled, config.DataDir
			config.CaptchaBrowserEnabled = true
			config.DataDir = t.TempDir()
			t.Cleanup(func() { config.CaptchaBrowserEnabled, config.DataDir = old, oldData })
			f.cm.SetConfigProvider(func(context.Context) (captcha.Config, error) {
				return captcha.Config{Enabled: true, SkipModelRequest: true}, nil
			})
			solver := &modelSkipProbeSolver{}
			f.cm.SetSolver(solver)
			if err := f.cm.SetManualParam("claim-only-token", "cn"); err != nil {
				t.Fatal(err)
			}
			f.eng.OverloadRetryDelays = []time.Duration{0}
			acc, err := f.st.AddAccount(model.ProviderZai, "stream-skip", "header.payload.sig")
			if err != nil {
				t.Fatal(err)
			}
			stream := "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5}}}\n\n" +
				"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n" +
				"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
				"data: {\"type\":\"message_stop\"}\n\n"
			f.respond = func(call int, _ *http.Request) (int, http.Header, string) {
				if tt.firstStatus != 0 && (call == 1 || tt.challenge) {
					return tt.firstStatus, http.Header{"Content-Type": {"application/json"}}, tt.firstBody
				}
				return 200, http.Header{"Content-Type": {"text/event-stream"}}, stream
			}
			body := msgBody()
			body["stream"] = true
			status, raw := f.post(t, body, "sk-test")
			wantCalls := 1
			if tt.challenge {
				wantCalls = MaxCaptchaRetries
				if status != 503 || !strings.Contains(raw, "captcha_required") {
					t.Fatalf("真实验证码挑战应按原预算终止，而不是鉴权失败: %d %s", status, raw)
				}
			} else {
				if tt.firstStatus != 0 {
					wantCalls = 2
				}
				if status != 200 || raw != stream {
					t.Fatalf("应保持流式成功与内容透传: %d %s", status, raw)
				}
			}
			if f.callCount() != wantCalls || solver.calls.Load() != 0 {
				t.Fatalf("模型请求数或 solver 调用数错误: model=%d want=%d solver=%d", f.callCount(), wantCalls, solver.calls.Load())
			}
			f.mu.Lock()
			calls := append([]upstreamCall(nil), f.calls...)
			f.mu.Unlock()
			for _, call := range calls {
				for _, key := range []string{"X-Aliyun-Captcha-Verify-Param", "X-Aliyun-Captcha-Verify-Region"} {
					if got := call.Header.Get(key); got != "" {
						t.Fatalf("首次与重试都不得携带缓存验证码 %s: %q", key, got)
					}
				}
			}
			if got := f.st.FindAny(acc.ID); got.Status != model.StatusActive {
				t.Fatalf("模型跳过及验证码挑战不能误伤账号状态: %s", got.Status)
			}
		})
	}
}
