package claim

import (
	"errors"
	"net/http"
	"testing"
)

func TestClaimRiskClassification(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		risk   bool
	}{
		{"日志风控", 405, `{"code":3012,"msg":"request has been blocked due to unusual activity."}`, true},
		{"HTTP200业务风控", 200, `{"code":"3012","msg":"denied"}`, true},
		{"嵌套错误文案", 405, `{"error":{"message":"unusual activity"}}`, true},
		{"纯文本风控", 403, `request blocked due to unusual activity`, true},
		{"英文可疑请求", 403, `Suspicious request detected`, true},
		{"中文可疑请求", 200, `{"code":9001,"msg":"检测到可疑的请求"}`, true},
		{"繁体可疑请求", 403, `檢測到可疑請求`, true},
		{"普通405", 405, `method not allowed`, false},
		{"鉴权失败", 401, `{"code":401,"msg":"unauthorized"}`, false},
		{"验证码失败", 400, `{"code":3007,"msg":"captcha blocked"}`, false},
		{"已领取", 200, `{"code":1003,"msg":"already claimed"}`, false},
		{"额度耗尽", 200, `{"code":1005,"msg":"request blocked"}`, false},
		{"已领取的可疑字样", 200, `{"code":1003,"msg":"suspicious request"}`, false},
		{"成功文案不是风控", 200, `{"code":0,"data":{"note":"risk checked"}}`, false},
		{"元数据键不是风控", 500, `{"code":0,"msg":"busy","risk_control":false}`, false},
		{"伪造故障元数据", 200, `{"code":1003,"proxy_failure":"claim_suspicious"}`, false},
		{"上游冷却时间", 405, `{"code":3012,"msg":"unusual activity","data":{"plan":{"ends_at":1700007200}}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestAccount(t)
			billing := &fakeBilling{responses: map[string]func(upstreamCall) (int, string){
				"/billing/claim": func(upstreamCall) (int, string) { return tc.status, tc.body },
			}}
			svc := NewService(newSolvedManager(t))
			svc.Client = billing
			_, err := svc.Claim(a, "start-plan")
			var ce *ClaimError
			got := errors.As(err, &ce) && ce.IsRiskControl()
			if got != tc.risk {
				t.Fatalf("风控分类=%v，预期=%v，error=%v", got, tc.risk, err)
			}
			if tc.risk {
				out := FailureOutcome(a, "start-plan", err)
				if tc.name == "上游冷却时间" && out["next_at"] != float64(1700007200) {
					t.Fatalf("风控分类不得丢失上游冷却时间: %v", out)
				}
				if out["risk_control"] != true || out["http_status"] != tc.status || len(billing.requests) != 1 {
					t.Fatalf("风控信号应透传且不在原线路重试: %v calls=%d", out, len(billing.requests))
				}
			}
		})
	}
}

func TestAutoClaimStopsAfterRiskControl(t *testing.T) {
	a := newTestAccount(t)
	billing := &fakeBilling{responses: map[string]func(upstreamCall) (int, string){
		"/billing/preview": func(upstreamCall) (int, string) {
			return 200, `{"code":0,"data":{"plans":[{"plan_id":"first","priority":2},{"plan_id":"second","priority":1}]}}`
		},
		"/billing/claim": func(upstreamCall) (int, string) { return 405, `{"code":3012,"msg":"unusual activity"}` },
	}}
	svc := NewService(newSolvedManager(t))
	svc.Client = billing
	out := svc.AutoClaimAllPlans(a)
	claims := 0
	for _, call := range billing.requests {
		if call.Method == http.MethodPost && lastSegment(call.Path) == "claim" {
			claims++
		}
	}
	if claims != 1 || len(out) != 1 || out[0]["risk_control"] != true {
		t.Fatalf("首次风控后不应再领取其他套餐: calls=%d outcomes=%v", claims, out)
	}
}
