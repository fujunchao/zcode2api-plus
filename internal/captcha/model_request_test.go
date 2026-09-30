package captcha

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"zcode2api/internal/config"
)

type modelRequestSolver struct{ calls int }

func (s *modelRequestSolver) Solve(context.Context, Config) (string, error) {
	s.calls++
	return "fresh-model-token", nil
}

func (s *modelRequestSolver) Close() error { return nil }

func TestModelCaptchaPolicyFromHTTP(t *testing.T) {
	for _, tt := range []struct {
		name     string
		status   int
		body     string
		wantSkip bool
	}{
		{"模型跳过", 200, `{"data":{"configs":{"captcha":{"enabled":true,"skip_model_request":true}}}}`, true},
		{"全局禁用", 200, `{"data":{"configs":{"captcha":{"enabled":false,"skip_model_request":false}}}}`, true},
		{"仅模型标记", 200, `{"data":{"configs":{"captcha":{"skip_model_request":true}}}}`, true},
		{"显式不跳过", 200, `{"data":{"configs":{"captcha":{"enabled":true,"skip_model_request":false}}}}`, false},
		{"旧版无字段", 200, `{"data":{"configs":{"captcha":{"enabled":true}}}}`, false},
		{"空值不跳过", 200, `{"data":{"configs":{"captcha":{"enabled":true,"skip_model_request":null}}}}`, false},
		{"字符串不能开启跳过", 200, `{"data":{"configs":{"captcha":{"enabled":true,"skip_model_request":"true"}}}}`, false},
		{"缺少配置对象", 200, `{"data":{"configs":{}}}`, false},
		{"配置接口失败", 500, `{}`, false},
		{"响应不是JSON", 200, `not-json`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			setBrowserEnabled(t, true)
			var fetches atomic.Int32
			setCaptchaServer(t, func(w http.ResponseWriter, r *http.Request) {
				fetches.Add(1)
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			})
			m := NewManager()
			solver := &modelRequestSolver{}
			m.SetSolver(solver)
			token, err := m.GetModelVerifyParam(nil)
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantSkip {
				if token != nil || solver.calls != 0 {
					t.Fatalf("策略跳过时不能取码: token=%v calls=%d", token, solver.calls)
				}
			} else if token == nil || token.VerifyParam != "fresh-model-token" || solver.calls != 1 {
				t.Fatalf("未开启跳过时必须保留取码: token=%v calls=%d", token, solver.calls)
			}
			if fetches.Load() != 1 {
				t.Fatalf("一次模型取码最多获取一次配置（包括失败回退）: %d", fetches.Load())
			}
		})
	}
}

func TestModelCaptchaSkipPreservesClaimManualToken(t *testing.T) {
	setBrowserEnabled(t, false)
	m := NewManager()
	m.SetConfigProvider(func(context.Context) (Config, error) {
		return Config{Enabled: true, SkipModelRequest: true}, nil
	})
	if err := m.SetManualParam("claim-manual-token", "cn"); err != nil {
		t.Fatal(err)
	}
	if token, err := m.GetModelVerifyParam(nil); err != nil || token != nil {
		t.Fatalf("模型应忽略人工缓存并跳过: token=%v err=%v", token, err)
	}
	if token, err := m.GetVerifyParam(nil); err != nil || token == nil || token.VerifyParam != "claim-manual-token" {
		t.Fatalf("模型跳过不能清掉共享的领取人工缓存: token=%v err=%v", token, err)
	}
}

func TestModelCaptchaPolicyRefresh(t *testing.T) {
	setBrowserEnabled(t, true)
	m := NewManager()
	now := time.Unix(1700000000, 0)
	m.SetNow(func() time.Time { return now })
	skip, fetches := true, 0
	m.SetConfigProvider(func(context.Context) (Config, error) {
		fetches++
		return Config{Enabled: true, SkipModelRequest: skip}, nil
	})
	solver := &modelRequestSolver{}
	m.SetSolver(solver)
	check := func(wantSkip bool, wantFetches, wantSolves int) {
		t.Helper()
		token, err := m.GetModelVerifyParam(nil)
		if err != nil || (token == nil) != wantSkip || fetches != wantFetches || solver.calls != wantSolves {
			t.Fatalf("策略刷新不符: token=%v err=%v fetches=%d solves=%d", token, err, fetches, solver.calls)
		}
	}
	check(true, 1, 0)
	skip = false
	check(true, 1, 0) // 有效缓存内不额外访问配置接口。
	now = now.Add(time.Duration(config.CaptchaConfigCacheTTL+1) * time.Millisecond)
	check(false, 2, 1) // 上游恢复验证，缓存过期后恢复取码。
	skip = true
	now = now.Add(time.Duration(config.CaptchaConfigCacheTTL+1) * time.Millisecond)
	check(true, 3, 1)
}

func TestModelCaptchaConfigFailureDoesNotCacheSkip(t *testing.T) {
	setBrowserEnabled(t, false)
	var status, fetches atomic.Int32
	status.Store(http.StatusInternalServerError)
	setCaptchaServer(t, func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write([]byte(`{"data":{"configs":{"captcha":{"enabled":true,"skip_model_request":true}}}}`))
	})
	m := NewManager()
	if token, err := m.GetModelVerifyParam(nil); token != nil || err != ErrUnavailable {
		t.Fatalf("配置请求失败必须保持原有验证要求: token=%v err=%v", token, err)
	}
	status.Store(http.StatusOK)
	if token, err := m.GetModelVerifyParam(nil); token != nil || err != nil {
		t.Fatalf("失败不得缓存，配置恢复后应识别模型跳过: token=%v err=%v", token, err)
	}
	if fetches.Load() != 2 {
		t.Fatalf("两次调用应各获取一次配置: %d", fetches.Load())
	}
}

func TestModelCaptchaRequiredStillUsesManualToken(t *testing.T) {
	setBrowserEnabled(t, false)
	m := NewManager()
	m.SetConfigProvider(func(context.Context) (Config, error) {
		return Config{Enabled: true, SkipModelRequest: false}, nil
	})
	if err := m.SetManualParam("manual-required", "cn"); err != nil {
		t.Fatal(err)
	}
	if token, err := m.GetModelVerifyParam(nil); err != nil || token == nil || token.VerifyParam != "manual-required" {
		t.Fatalf("上游仍要求模型验证码时应保留人工回填: token=%v err=%v", token, err)
	}
}
