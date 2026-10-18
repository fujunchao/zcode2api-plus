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
	"time"

	"zcode2api/internal/model"
	"zcode2api/internal/store"
	"zcode2api/internal/web"
)

// BeginUpstreamAttempt 紧贴 client.Do 调用。返回回调仅记录响应头/连接结果，不把 HTTP
// 200 当作完整调用成功，也不记录可能包含代理凭据的原始网络错误。
// request 是实际发出的请求；只提取安全摘要，不保留请求或读取正文。nil 表示未提供摘要。
func (d *ReqDiag) BeginUpstreamAttempt(acc *model.Account, egress store.ProxyEgress, request *http.Request, current ...*model.Account) func(*http.Response, error) {
	if d == nil {
		return func(*http.Response, error) {}
	}
	d.upstreamAttempts++
	base := fmt.Sprintf("req=%q ticket=%q attempt=%d selection=%d acc_id=%q route_id=%q route=%q endpoint=%q mode=%s",
		d.ReqID, d.Ticket, d.upstreamAttempts, d.Attempts, acc.ID, egress.ID, egress.Label, egress.Endpoint, egress.Mode)
	base += fmt.Sprintf(" proxy_generation=%d binding_generation=%d credential_generation=%d route_reason=%q", egress.ConfigurationGeneration(), egress.BindingGeneration(), egress.CredentialGeneration(), egress.Reason)
	acquired := false
	if request != nil && egress.Mode != "custom_client" {
		_, acquired = ProxyAttemptEgress(request.Context())
	}
	base += fmt.Sprintf(" dispatch_acquired=%t", acquired)
	trace := fmt.Sprintf(" gateway_request_id=%q protocol=%s", d.GatewayRequestID, d.Protocol)
	d.lastAttemptBase = base + trace
	d.shape.mcsDeclared = request != nil && hasMCSDeclaration(request.Header.Get("Anthropic-Beta"))
	summary := attemptRequestSummary(acc, request)
	extra := trace + d.shape.format(d.preserveContext)
	var observed *model.Account
	if len(current) > 0 {
		observed = current[0]
	}
	// 当前状态在发送前取样并冻结；响应到达期间额度/冷却变化不能改写这份证据。
	extra += d.outboundAttemptSummary(acc, observed, request, d.Now())
	web.Attempt(d.ReqID, "event=start "+base+summary+extra+" ts="+diagnosticTime(d.Now()))
	started := d.Now()
	return func(resp *http.Response, err error) {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
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
		// elapsed_ms 让「超时到底生效没有」可直接验证：死线路此前每次 60–80 秒，
		// 收紧拨号/握手后应落到 25–35 秒。
		web.Attempt(d.ReqID, fmt.Sprintf("event=%s %s status=%d err_kind=%s elapsed_ms=%d%s%s%s ts=%s", event, base, status, kind, d.Now().Sub(started).Milliseconds(), summary, extra, responseHeaderSummary(resp), diagnosticTime(d.Now())))
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

// 会话/追踪/轮次值可能来自下游，只用本进程限时 HMAC 关联，不输出原值。
func attemptIDRef(value string) string {
	if value == "" {
		return "-"
	}
	return privateReference(value, time.Now())[:12]
}

func diagnosticBytesRef(value []byte) string {
	if len(value) == 0 {
		return "-"
	}
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:6])
}

// RiskAttempt 将本次实际出站方式关联到风控处置，防止把强制直连失败归因给未使用的代理。
type RiskAttempt struct {
	RequestID string
	Ticket    string
	Egress    store.ProxyEgress
}
