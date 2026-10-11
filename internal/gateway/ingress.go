package gateway

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/web"
)

const (
	// GatewayRequestIDHeader 返回网关生成的完整 UUID；不沿用入站同名值。
	GatewayRequestIDHeader = "X-Zcode2api-Request-Id"
	// NewAPIRequestIDHeader 是 rc.41 会保存为 upstream_request_id 的渠道响应头。
	// 它表示本网关请求，不是最终用户身份，也不是模型上游的 request ID。
	NewAPIRequestIDHeader = "X-Oneapi-Request-Id"
)

type requestTraceKey struct{}

// 上下文中只放不可变值；异步票可沿用它，不携带 HTTP 请求或用户头。
type requestTrace struct {
	ID       string
	Protocol string
}

// ObserveRequests 只观测模型 HTTP 入口，不读取正文、不缓存响应，也不改变 SSE 刷新。
// protocol 由路由注册处给出固定值，不从客户端 URL/查询参数生成。
func ObserveRequests(protocol string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		trace := requestTrace{ID: config.NewUUIDv4(), Protocol: protocol}
		ctx := context.WithValue(r.Context(), requestTraceKey{}, trace)
		shortID := trace.ID[:6]
		w.Header().Set(GatewayRequestIDHeader, trace.ID)
		w.Header().Set(NewAPIRequestIDHeader, trace.ID)
		base := fmt.Sprintf("req=%q gateway_request_id=%q protocol=%s", shortID, trace.ID, protocol)
		started := time.Now()
		web.Ingress(shortID, "event=start "+base+" ts="+diagnosticTime(started))

		writer := &diagnosticResponseWriter{ResponseWriter: w}
		returned := false
		defer func() {
			now := time.Now()
			status := writer.status
			if status == 0 && returned {
				status = http.StatusOK
			}
			// HTTP 200 只表示响应头；协议是否完整仍看 [#] 的 complete/err。
			web.Ingress(shortID, fmt.Sprintf("event=end %s ts=%s http_status=%d response_bytes=%d total_ms=%d canceled=%t write_failed=%t handler_returned=%t",
				base, diagnosticTime(now), status, writer.bytes, now.Sub(started).Milliseconds(),
				ctx.Err() != nil, writer.writeFailed, returned))
		}()
		var output http.ResponseWriter = writer
		if flusher, ok := w.(http.Flusher); ok {
			output = &diagnosticFlushingWriter{diagnosticResponseWriter: writer, flusher: flusher}
		}
		next(output, r.WithContext(ctx))
		returned = true
	}
}

// RequestShortID 保留旧日志的六位短 ID；完整关联必须使用 GatewayRequestIDHeader。
// 非 HTTP 调用（如隔离测试）没有入口上下文时仍生成独立短 ID。
func RequestShortID(ctx context.Context) string {
	if trace, ok := ctx.Value(requestTraceKey{}).(requestTrace); ok {
		return trace.ID[:6]
	}
	return randomHex(3)
}

// SetTrace 把 HTTP 入口关联带到同步引擎或异步票。只拷贝值，不反向修改上下文。
func (d *ReqDiag) SetTrace(ctx context.Context) {
	if d == nil {
		return
	}
	if trace, ok := ctx.Value(requestTraceKey{}).(requestTrace); ok {
		d.GatewayRequestID, d.Protocol = trace.ID, trace.Protocol
	}
}

func diagnosticTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

type diagnosticResponseWriter struct {
	http.ResponseWriter
	status      int
	bytes       int64
	writeFailed bool
}

func (w *diagnosticResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *diagnosticResponseWriter) WriteHeader(status int) {
	if status >= 200 && w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *diagnosticResponseWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	if err != nil {
		w.writeFailed = true
	}
	return n, err
}

type diagnosticFlushingWriter struct {
	*diagnosticResponseWriter
	flusher http.Flusher
}

func (w *diagnosticFlushingWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	w.flusher.Flush()
}
