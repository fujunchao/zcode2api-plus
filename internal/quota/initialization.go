package quota

import (
	"context"
	"math"
	"strings"

	"zcode2api/internal/model"
	"zcode2api/internal/store"
)

// ObserveInitialization 真正出站且不复用普通缓存，不落状态、不删除代理。
// 网络结果和配额补丁由初始化模块通过 Store 的任务事务提交。
func (s *Service) ObserveInitialization(ctx context.Context, a *store.InitializationAttempt) model.InitialQuotaObservation {
	view := a.Account.Clone()
	reader := &Service{Store: s.Store, Client: s.Client, ctx: ctx, now: s.now, observationOnly: view, initialization: a}
	if s.Client != nil {
		release, err := s.Store.AcquireInitializationDispatch(a)
		if err != nil {
			return model.InitialQuotaObservation{Kind: "unknown", Error: "初始化执行权已变化"}
		}
		defer release()
	}
	result, evidence := reader.fetchQuotaOnce(ctx, a.Account)
	out := model.InitialQuotaObservation{Kind: "unknown", Snapshot: view, CheckedAt: evidence.CheckedAt}
	out.NextAt = reader.observationNextAt
	if message, ok := result["error"].(string); ok {
		out.Error = message
	}
	_, out.ProxyFailure = result["proxy_failure"]
	if evidence.CheckedAt <= 0 {
		return out
	}
	if evidence.Missing {
		out.Kind = "missing"
		return out
	}
	payload, _ := result["balance"].(map[string]any)
	data, _ := payload["data"].(map[string]any)
	balances, _ := data["balances"].([]any)
	known, unknown, allocated := 0, false, false
	for _, raw := range balances {
		balance, ok := raw.(map[string]any)
		if !ok {
			unknown = true
			continue
		}
		name := firstNonEmptyString(balance["model"], balance["show_name"])
		if strings.TrimSpace(name) == "" {
			unknown = true
			continue
		}
		value, exists := balance["available_units"]
		if !exists || value == nil {
			value = balance["remaining_units"]
		}
		if n, ok := asNumber(value); ok && n >= 0 && !math.IsInf(n, 0) && !math.IsNaN(n) {
			known++
			if n > 0 {
				out.Kind = "available"
				return out
			}
		} else {
			unknown = true
		}
		for _, key := range []string{"total_units", "used_units"} {
			if n, ok := asNumber(balance[key]); ok && n > 0 && !math.IsInf(n, 0) && !math.IsNaN(n) {
				allocated = true
			}
		}
	}
	// 套餐名称或总量不证明当前已经耗尽；余额缺失时继续隔离，不能释放临时出口。
	if allocated && known > 0 && !unknown {
		out.Kind = "allocated"
	}
	return out
}
