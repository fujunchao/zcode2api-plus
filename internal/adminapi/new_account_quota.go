package adminapi

import (
	"context"
	"fmt"

	"zcode2api/internal/model"
	"zcode2api/internal/web"
)

// finishNewAccountQuotaCheck 保留旧模式的领取后真实补查。
// 没有初始额度不代表业务线路故障，不能因此删除共享代理或改派其他账号。
func (h *Handler) finishNewAccountQuotaCheck(ctx context.Context, acc *model.Account, initial model.StartPlanObservation) {
	live := h.Store.SnapshotAccount(acc.Provider, acc.ID)
	if ctx.Err() != nil || live == nil || !live.IsQuotaRefreshTarget() {
		return
	}
	h.Quota.FetchQuotaFresh(ctx, live)
	if ctx.Err() != nil || !h.Store.ClaimAutoEnabled() {
		return
	}
	web.Ok("new-account-quota", fmt.Sprintf("新账号 %s 已完成领取后额度补查；空额度不再触发共享代理删除", acc.ID))
}
