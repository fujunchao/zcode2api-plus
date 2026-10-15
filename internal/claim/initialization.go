package claim

import (
	"errors"
	"time"

	"zcode2api/internal/model"
)

// InitializationOutcome 只返回决策字段，不把上游原文、验证码或代理认证存入任务。
type InitializationOutcome struct {
	Reason    string
	Succeeded bool
	NextAt    int64
	Claimed   bool
}

func initializationFailure(err error) InitializationOutcome {
	out := InitializationOutcome{Reason: "request_failed"}
	var ce *ClaimError
	if !errors.As(err, &ce) {
		return out
	}
	if ce.nextAt != nil {
		out.NextAt = int64(*ce.nextAt * 1000)
	}
	switch {
	case ce.IsRiskControl():
		out.Reason = "risk_control"
	case ce.IsEndpointUnreachable():
		out.Reason = "proxy_unreachable"
	case ce.IsCaptchaFailure() || ce.Code() == 3007:
		out.Reason = "captcha_required"
	case ce.Code() == 1003:
		out.Reason = "already_claimed"
	case ce.Code() == 1004 || ce.Code() == 1001 || ce.Code() == 1002:
		out.Reason = "ineligible"
	case ce.Code() == 1005 || ce.httpStatus == 429:
		out.Reason = "rate_limited"
	}
	return out
}

func (s *Service) RunInitialization(acc *model.Account) (out InitializationOutcome) {
	defer func() { out.NextAt = max(out.NextAt, s.retryAfter) }()
	if message := s.ReportActivationEvents(acc); message != "" {
		return InitializationOutcome{Reason: "activation_failed"}
	}
	plans, err := s.PreviewPlans(acc)
	if err != nil {
		return initializationFailure(err)
	}
	if len(plans) == 0 {
		return InitializationOutcome{Reason: "no_plans"}
	}
	out = InitializationOutcome{Reason: "claimed"}
	for _, plan := range plans {
		if s.context().Err() != nil {
			return out
		}
		id, _ := plan["plan_id"].(string)
		result, err := s.Claim(acc, id)
		if err != nil {
			failure := initializationFailure(err)
			failure.Succeeded = out.Succeeded
			failure.Claimed = true
			return failure
		}
		out.Succeeded, out.Claimed = true, true
		if n, ok := result["next_at"].(float64); ok && (out.NextAt == 0 || int64(n*1000) < out.NextAt) {
			out.NextAt = int64(n * 1000)
		}
	}
	if out.NextAt == 0 {
		out.NextAt = time.Now().Add(10 * time.Minute).UnixMilli()
	}
	return out
}
