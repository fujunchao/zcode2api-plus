package adminapi

import (
	"fmt"

	"zcode2api/internal/model"
	"zcode2api/internal/web"
)

func (h *Handler) handleClaimProxyRisk(acc *model.Account, outcomes []map[string]any) {
	for _, outcome := range outcomes {
		if ok, _ := outcome["ok"].(bool); ok {
			return // 本轮已成功领取，不再按“未获配额”淘汰出口。
		}
	}
	for _, outcome := range outcomes {
		if risk, _ := outcome["risk_control"].(bool); !risk {
			continue
		}
		removed, reassign, err := h.Store.PurgeUnprovisionedClaimProxy(acc)
		if err != nil {
			web.Warn("claim-proxy", fmt.Sprintf("账号 %s 领取风控，问题代理移除失败，保留原绑定: %v", acc.ID, err))
		} else if removed {
			outcome["proxy_removed"] = *acc.ProxyID
			outcome["proxy_reassigned"] = len(reassign.Assigned)
			outcome["proxy_direct_fallback"] = len(reassign.Direct)
			web.Warn("claim-proxy", fmt.Sprintf("账号 %s 未获 Start plan 初始额度且领取被风控，已移除线路 %s；改派 %d 个账号、%d 个退直连，保留领取冷却",
				acc.ID, *acc.ProxyID, len(reassign.Assigned), len(reassign.Direct)))
		} else {
			web.Warn("claim-proxy", fmt.Sprintf("账号 %s 领取被风控，但无当前出口的未获初始额度证据或绑定已变化，未移除代理", acc.ID))
		}
		return // 每次领取最多淘汰一条，不循环换代理重领。
	}
}
