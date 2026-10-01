package gateway

import (
	"context"
	"errors"
	"fmt"
	"net"

	"zcode2api/internal/model"
	"zcode2api/internal/store"
	"zcode2api/internal/web"
)

// BeginUpstreamAttempt 紧贴 client.Do 调用。返回回调仅记录响应头/连接结果，不把 HTTP
// 200 当作完整调用成功，也不记录可能包含代理凭据的原始网络错误。
func (d *ReqDiag) BeginUpstreamAttempt(acc *model.Account, egress store.ProxyEgress) func(int, error) {
	if d == nil {
		return func(int, error) {}
	}
	d.upstreamAttempts++
	base := fmt.Sprintf("req=%q ticket=%q attempt=%d selection=%d acc_id=%q route_id=%q route=%q endpoint=%q mode=%s",
		d.ReqID, d.Ticket, d.upstreamAttempts, d.Attempts, acc.ID, egress.ID, egress.Label, egress.Endpoint, egress.Mode)
	web.Attempt(d.ReqID, "event=start "+base)
	return func(status int, err error) {
		kind := "-"
		event := "headers"
		if err != nil {
			event, kind = "error", "connection_failed"
			var ne net.Error
			if errors.Is(err, context.Canceled) {
				kind = "canceled"
			} else if errors.As(err, &ne) && ne.Timeout() {
				kind = "timeout"
			}
		}
		web.Attempt(d.ReqID, fmt.Sprintf("event=%s %s status=%d err_kind=%s", event, base, status, kind))
	}
}

// RiskAttempt 将本次实际出站方式关联到风控处置，防止把强制直连失败归因给未使用的代理。
type RiskAttempt struct {
	RequestID string
	Ticket    string
	Egress    store.ProxyEgress
}
