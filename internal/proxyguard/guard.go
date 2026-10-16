// Package proxyguard 统一计费请求的异常观测及网络复核通知，不负责删除代理或判定账号资格。
package proxyguard

import (
	"context"
	"fmt"
	"maps"
	"time"

	"zcode2api/internal/model"
	"zcode2api/internal/proxy"
	"zcode2api/internal/store"
	"zcode2api/internal/web"
)

// Observe 保留原错误分类，但单次计费失败不等于共享线路故障，更不能直接删除。
func Observe(ctx context.Context, st *store.Store, acc *model.Account, reason string, route ...store.ProxyEgress) map[string]any {
	out := map[string]any{}
	if ctx.Err() != nil || st == nil || acc == nil || acc.ProxyID == nil || *acc.ProxyID == "" || acc.ProxyURL == nil || *acc.ProxyURL == "" ||
		(reason != proxy.FailureEndpointUnreachable && reason != proxy.FailureClaimSuspicious) {
		return out
	}
	out["proxy_failure"] = reason
	action := st.ObserveProxyEndpointFailure(ctx, acc, reason, time.Now(), route...)
	out["proxy_failure_action"] = action
	label := "计费请求未完成"
	if reason == proxy.FailureClaimSuspicious {
		label = "领取返回可疑请求"
	}
	web.Warn("proxy-observation", fmt.Sprintf("账号 %s %s，保留线路 %s 与全部绑定；网络复核 action=%s",
		acc.ID, label, *acc.ProxyID, action))
	return out
}

// ApplyClaimFailures 合并本轮第一个计费异常，不删除代理、不丢失此前领取结果或冷却。
func ApplyClaimFailures(ctx context.Context, st *store.Store, acc *model.Account, outcomes []map[string]any, route ...store.ProxyEgress) {
	for _, outcome := range outcomes {
		reason, _ := outcome["proxy_failure"].(string)
		if reason == proxy.FailureEndpointUnreachable || reason == proxy.FailureClaimSuspicious {
			maps.Copy(outcome, Observe(ctx, st, acc, reason, route...))
			return
		}
	}
}
