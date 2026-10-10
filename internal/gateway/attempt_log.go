package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"

	"zcode2api/internal/model"
	"zcode2api/internal/store"
	"zcode2api/internal/web"
)

// BeginUpstreamAttempt 紧贴 client.Do 调用。返回回调仅记录响应头/连接结果，不把 HTTP
// 200 当作完整调用成功，也不记录可能包含代理凭据的原始网络错误。
// request 是实际发出的请求；只提取安全摘要，不保留请求或读取正文。nil 表示未提供摘要。
func (d *ReqDiag) BeginUpstreamAttempt(acc *model.Account, egress store.ProxyEgress, request *http.Request) func(int, error) {
	if d == nil {
		return func(int, error) {}
	}
	d.upstreamAttempts++
	base := fmt.Sprintf("req=%q ticket=%q attempt=%d selection=%d acc_id=%q route_id=%q route=%q endpoint=%q mode=%s",
		d.ReqID, d.Ticket, d.upstreamAttempts, d.Attempts, acc.ID, egress.ID, egress.Label, egress.Endpoint, egress.Mode)
	summary := attemptRequestSummary(acc, request)
	web.Attempt(d.ReqID, "event=start "+base+summary)
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
		// 新字段始终追加，不挪动原有日志字段；开始与结果使用同一份请求快照。
		web.Attempt(d.ReqID, fmt.Sprintf("event=%s %s status=%d err_kind=%s%s", event, base, status, kind, summary))
	}
}

var diagnosticUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func attemptRequestSummary(acc *model.Account, request *http.Request) string {
	mode := acc.Mode
	if mode != "jwt" && mode != "apiKey" {
		mode = "unknown"
	}
	origin, requestID := "-", "-"
	var session, trace, query string
	if request != nil {
		if u := request.URL; u != nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https") {
			// 不调用 URL.String：userinfo、路径、query、fragment 都可能含凭据。
			origin = u.Scheme + "://" + u.Host
		}
		if value := request.Header.Get("X-Request-Id"); diagnosticUUID.MatchString(value) {
			// 此头由网关生成；异常值不作为日志文本输出。
			requestID = value
		}
		session = request.Header.Get("X-Session-Id")
		trace = request.Header.Get("X-Zcode-Trace-Id")
		query = request.Header.Get("X-Query-Id")
	}
	return fmt.Sprintf(" account_mode=%s target_origin=%q upstream_request_id=%q session_ref=%q trace_ref=%q query_ref=%q",
		mode, origin, requestID, attemptIDRef(session), attemptIDRef(trace), attemptIDRef(query))
}

// 会话/追踪/轮次值可能来自下游，不把原值写日志；短哈希只用于同轮诊断关联。
func attemptIDRef(value string) string {
	if value == "" {
		return "-"
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:6])
}

// RiskAttempt 将本次实际出站方式关联到风控处置，防止把强制直连失败归因给未使用的代理。
type RiskAttempt struct {
	RequestID string
	Ticket    string
	Egress    store.ProxyEgress
}
