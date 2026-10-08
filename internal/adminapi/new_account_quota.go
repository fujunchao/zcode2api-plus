package adminapi

import (
	"context"
	"fmt"

	"zcode2api/internal/model"
	"zcode2api/internal/web"
)

// finishNewAccountQuotaCheck 只执行一次：先刷新，再用两次独立证据判断原代理。
// 领取失败或没有可领套餐也要补查；但错误响应、取消和变更后的绑定不构成删除依据。
func (h *Handler) finishNewAccountQuotaCheck(ctx context.Context, acc *model.Account, initial model.StartPlanObservation) {
	live := h.Store.SnapshotAccount(acc.Provider, acc.ID)
	if ctx.Err() != nil || live == nil || !live.IsQuotaRefreshTarget() {
		return
	}
	_, final := h.Quota.FetchQuotaFresh(ctx, live)
	if ctx.Err() != nil || !h.Store.ClaimAutoEnabled() {
		return
	}
	removed, reassign, err := h.Store.PurgeEmptyNewAccountProxy(acc, initial, final)
	if err != nil {
		web.Warn("new-account-quota", fmt.Sprintf("新账号 %s 双次空额度检测移除代理失败，保留原绑定: %v", acc.ID, err))
	} else if removed {
		web.Warn("new-account-quota", fmt.Sprintf("新账号 %s 领取前后两次确认未获初始额度，已移除原线路 %s；改派 %d 个账号、%d 个退直连，保留领取冷却，不继续换线重领",
			acc.ID, initial.ProxyID, len(reassign.Assigned), len(reassign.Direct)))
	} else {
		web.Ok("new-account-quota", fmt.Sprintf("新账号 %s 已完成领取后额度补查，未满足同一代理双次空额度条件，保留代理", acc.ID))
	}
}
