package model

import "time"

// LocalSchedulingBarrier 描述本地调度限制，不代表供应商封禁状态或额度可用性。
// 返回的时间只表示最早重新评估时刻；停用/归档/失效/整号耗尽不承诺自动恢复。
func (a *Account) LocalSchedulingBarrier(now time.Time, extraRiskWindow time.Duration) (string, *float64) {
	if a == nil {
		return "missing", nil
	}
	if a.ArchivedAt != nil {
		return "archived", nil
	}
	if !a.Enabled || a.Status == StatusDisabled {
		return "disabled", nil
	}
	if a.Status == StatusInvalid || a.Status == StatusExhausted {
		return a.Status, nil
	}
	if a.Status != StatusActive && a.Status != StatusCooling {
		return "unknown", nil
	}
	var until *float64
	reason := "ready"
	if a.Status == StatusCooling {
		if a.CoolingUntil == nil {
			return "cooling", nil
		}
		if now.Before(unixTime(*a.CoolingUntil)) {
			v := *a.CoolingUntil
			until, reason = &v, "cooling"
		}
	}
	if a.InRiskControlSilentWindow(now, extraRiskWindow) {
		riskUntil := a.RiskControlSilentUntil(extraRiskWindow)
		if until == nil || *riskUntil >= *until {
			until, reason = riskUntil, "risk_isolation"
		}
	}
	return reason, until
}
