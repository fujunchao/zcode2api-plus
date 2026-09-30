package claim

import (
	"context"
	"testing"

	"zcode2api/internal/captcha"
)

func TestModelCaptchaSkipDoesNotDisableClaim(t *testing.T) {
	for _, manual := range []bool{false, true} {
		name := "求解并在3007后换码"
		if manual {
			name = "保留领取人工缓存"
		}
		t.Run(name, func(t *testing.T) {
			acc := newTestAccount(t)
			cm := newSolvedManager(t, "claim-first", "claim-second")
			cm.SetConfigProvider(func(context.Context) (captcha.Config, error) {
				return captcha.Config{Enabled: true, SkipModelRequest: true, Prefix: "px", Region: "cn", SceneID: "sc"}, nil
			})
			if manual {
				if err := cm.SetManualParam("claim-manual", "cn"); err != nil {
					t.Fatal(err)
				}
			}
			if token, err := cm.GetModelVerifyParam(nil); token != nil || err != nil {
				t.Fatalf("同一个 Manager 的模型路径必须跳过: token=%v err=%v", token, err)
			}
			up := &fakeBilling{responses: map[string]func(upstreamCall) (int, string){
				"/billing/claim": func(call upstreamCall) (int, string) {
					if call.Headers.Get(captchaHeader) == "claim-first" {
						return 200, `{"code":3007}`
					}
					return 200, `{"code":0,"data":{}}`
				},
			}}
			svc := &Service{Captcha: cm, Client: up}
			if _, err := svc.Claim(acc, "p1"); err != nil {
				t.Fatalf("模型跳过不能影响领取: %v", err)
			}
			wantTokens := []string{"claim-first", "claim-second"}
			if manual {
				wantTokens = []string{"claim-manual"}
			}
			if len(up.requests) != len(wantTokens) {
				t.Fatalf("领取请求数不符: %d", len(up.requests))
			}
			for i, call := range up.requests {
				if call.Headers.Get(captchaHeader) != wantTokens[i] || call.Headers.Get(captchaRegionHeader) != "cn" {
					t.Fatalf("领取必须保留验证码和区域: request=%d headers=%v", i, call.Headers)
				}
			}
		})
	}
}
