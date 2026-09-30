package asyncpool

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"zcode2api/internal/captcha"
	"zcode2api/internal/config"
	"zcode2api/internal/gateway"
	"zcode2api/internal/model"
)

func TestAsyncModelCaptchaSkip(t *testing.T) {
	for _, cached := range []bool{false, true} {
		name := "无缓存"
		if cached {
			name = "有人工缓存"
		}
		t.Run(name, func(t *testing.T) {
			p, st, cm, solver := newTestPool(t)
			addJWTAccount(t, st, "async-model-skip")
			cm.SetConfigProvider(func(context.Context) (captcha.Config, error) {
				return captcha.Config{Enabled: true, SkipModelRequest: true}, nil
			})
			solver.err = errors.New("模型跳过时不能调用求解器")
			if cached {
				if err := cm.SetManualParam("claim-only-token", "cn"); err != nil {
					t.Fatal(err)
				}
			}
			up := &scriptedUpstream{specs: []upstreamSpec{{status: http.StatusOK, lines: []string{
				`data: {"id":"ok"}`, `data: {"type":"message_stop"}`,
			}}}}
			config.UpstreamZai = up.start(t).URL
			tk := insertTicket(p, "skip-model", map[string]any{
				"model": "GLM-5.3", "max_tokens": 8,
				"messages": []any{map[string]any{"role": "user", "content": "ping"}},
			})
			p.processTicket(context.Background(), "skip-model")
			var types []string
			for _, event := range drainEvents(tk) {
				types = append(types, event.Type)
			}
			if strings.Join(types, ",") != "ready,chunk,chunk,done" {
				t.Fatalf("模型跳过验证码后应完成异步流: %v", types)
			}
			if up.callCount() != 1 || solver.callCount() != 0 {
				t.Fatalf("应仅请求模型，不得调用 solver: model=%d solver=%d", up.callCount(), solver.callCount())
			}
			for _, key := range []string{"X-Aliyun-Captcha-Verify-Param", "X-Aliyun-Captcha-Verify-Region"} {
				if got := up.header(0).Get(key); got != "" {
					t.Fatalf("模型跳过不得发送 %s: %q", key, got)
				}
			}
		})
	}
}

func TestAsyncModelCaptchaSkipChallengeIsBounded(t *testing.T) {
	p, st, cm, solver := newTestPool(t)
	acc := addJWTAccount(t, st, "async-skip-challenge")
	cm.SetConfigProvider(func(context.Context) (captcha.Config, error) {
		return captcha.Config{Enabled: true, SkipModelRequest: true}, nil
	})
	solver.err = errors.New("模型跳过时不能强制开启求解")
	up := &scriptedUpstream{}
	for range gateway.MaxCaptchaRetries {
		up.specs = append(up.specs, upstreamSpec{status: http.StatusForbidden, body: `{"code":3007}`})
	}
	config.UpstreamZai = up.start(t).URL
	tk := insertTicket(p, "skip-challenge", map[string]any{"model": "GLM-5.3", "messages": []any{}})
	p.processTicket(context.Background(), "skip-challenge")
	events := drainEvents(tk)
	if len(events) == 0 || events[len(events)-1].Type != "error" {
		t.Fatalf("挑战重试耗尽应结束票务: %v", events)
	}
	errObj := events[len(events)-1].Data.(map[string]any)["error"].(map[string]any)
	if errObj["type"] != "captcha_required" || up.callCount() != gateway.MaxCaptchaRetries || solver.callCount() != 0 {
		t.Fatalf("挑战应有界且遵守模型跳过: error=%v model=%d solver=%d", errObj, up.callCount(), solver.callCount())
	}
	if got := st.FindAny(acc.ID); got.Status != model.StatusActive {
		t.Fatalf("验证码挑战不能误标为账号鉴权失败: %s", got.Status)
	}
}
