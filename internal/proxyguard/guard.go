// Package proxyguard 统一计费请求的故障代理处置及结果回报，不负责判断余额或账号资格。
package proxyguard

import (
	"context"
	"fmt"
	"maps"

	"zcode2api/internal/model"
	"zcode2api/internal/proxy"
	"zcode2api/internal/store"
	"zcode2api/internal/web"
)

// Purge 只接受出站层确认的两类故障；取消、直连或无命名绑定不执行删除。
func Purge(ctx context.Context, st *store.Store, acc *model.Account, reason string) map[string]any {
	out := map[string]any{}
	if ctx.Err() != nil || acc == nil || acc.ProxyID == nil || *acc.ProxyID == "" || acc.ProxyURL == nil || *acc.ProxyURL == "" ||
		(reason != proxy.FailureEndpointUnreachable && reason != proxy.FailureClaimSuspicious) {
		return out
	}
	out["proxy_failure"] = reason
	removed, reassign, err := st.PurgeFailedAccountProxy(acc)
	if err != nil {
		out["proxy_remove_error"] = err.Error()
		web.Warn("proxy-failure", fmt.Sprintf("账号 %s 的故障代理 %s 删除失败，保留原绑定: %v", acc.ID, *acc.ProxyID, err))
		return out
	}
	if !removed {
		web.Ok("proxy-failure", fmt.Sprintf("账号 %s 的失败请求绑定已变化或代理已删除，跳过迟到结果", acc.ID))
		return out
	}
	out["proxy_removed"] = *acc.ProxyID
	out["proxy_reassigned"] = len(reassign.Assigned)
	out["proxy_direct_fallback"] = len(reassign.Direct)
	label := "代理无法访问计费端点"
	if reason == proxy.FailureClaimSuspicious {
		label = "领取返回可疑请求"
	}
	web.Warn("proxy-failure", fmt.Sprintf("账号 %s %s，已移除线路 %s；改派 %d 个账号、%d 个退直连，保留账号状态与领取冷却",
		acc.ID, label, *acc.ProxyID, len(reassign.Assigned), len(reassign.Direct)))
	return out
}

// ApplyClaimFailures 只处理本轮第一个已确认的代理故障；即使此前有套餐领取成功也不忽略。
func ApplyClaimFailures(ctx context.Context, st *store.Store, acc *model.Account, outcomes []map[string]any) {
	for _, outcome := range outcomes {
		reason, _ := outcome["proxy_failure"].(string)
		if reason == proxy.FailureEndpointUnreachable || reason == proxy.FailureClaimSuspicious {
			maps.Copy(outcome, Purge(ctx, st, acc, reason))
			return
		}
	}
}
