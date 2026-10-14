package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"syscall"

	"zcode2api/internal/web"
)

var (
	diagnosticHexID       = regexp.MustCompile(`^[0-9a-fA-F]{16,64}$`)
	diagnosticTraceparent = regexp.MustCompile(`^[0-9a-fA-F]{2}-[0-9a-fA-F]{32}-[0-9a-fA-F]{16}-[0-9a-fA-F]{2}$`)
	diagnosticCode        = regexp.MustCompile(`^(?:[0-9]{1,10}|F[0-9]{3})$`)
)

// 只允许固定追踪头。未知形态只留引用，绝不输出任意 token、URL 或整组响应头。
var responseTraceHeaders = [...]struct{ header, field string }{
	{"X-Request-Id", "response_request"},
	{"Request-Id", "response_alt_request"},
	{"X-Log-Id", "response_log"},
	{"X-Zhipu-Request-Id", "response_zhipu_request"},
	{"Traceparent", "response_traceparent"},
}

func responseHeaderSummary(resp *http.Response) string {
	if resp == nil {
		return ""
	}
	var b strings.Builder
	for _, item := range responseTraceHeaders {
		id, ref := "-", "-"
		if values := resp.Header.Values(item.header); len(values) == 1 {
			id, ref = diagnosticResponseID(values[0])
		}
		fmt.Fprintf(&b, " %s_id=%q %s_ref=%q", item.field, id, item.field, ref)
	}
	return b.String()
}

func diagnosticResponseID(value string) (id, ref string) {
	if value == "" {
		return "-", "-"
	}
	// 未知/超长值也不截取原文；哈希用于本地关联，不是原始追踪 ID。
	ref = attemptIDRef(value)
	if len(value) <= 128 && (diagnosticUUID.MatchString(value) || diagnosticHexID.MatchString(value) || diagnosticTraceparent.MatchString(value)) {
		return value, ref
	}
	return "-", ref
}

// ReadErrorResponse 保留原错误分类的 64 KiB 前缀输入，多读一个字节仅用于确认超限。
// 关闭响应体仍由原调用方负责，诊断不改变错误传播或重试策略。
func ReadErrorResponse(resp *http.Response, diag *ReqDiag, phase string) ([]byte, error) {
	raw, _, err := ReadErrorResponseWithCompletion(resp, diag, phase)
	return raw, err
}

// ReadErrorResponseWithCompletion 另行报告错误体是否完整；截断前缀仍供旧分类器使用，
// 但不能作为严格线路淘汰证据。即使前缀恰好是合法 JSON，也不能推断正文已读完。
func ReadErrorResponseWithCompletion(resp *http.Response, diag *ReqDiag, phase string) ([]byte, bool, error) {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes+1))
	truncated := len(raw) > maxErrorBodyBytes
	diag.ObserveResponseBody(resp, raw, err, truncated, phase)
	if truncated {
		raw = raw[:maxErrorBodyBytes]
	}
	return raw, !truncated && err == nil, err
}

// ObserveResponseBody 在读完/读断之后记录独立事件，不能用 headers 的 err_kind 推断它。
// 新事件只含结构、业务码、固定错误类型、内容引用；不复述错误消息或任意扩展字段。
func (d *ReqDiag) ObserveResponseBody(resp *http.Response, raw []byte, readErr error, truncated bool, phase string) {
	if d == nil {
		return
	}
	status, declaredLength := 0, int64(-1)
	contentType := "other"
	if resp != nil {
		status, declaredLength = resp.StatusCode, resp.ContentLength
		switch ct := strings.ToLower(resp.Header.Get("Content-Type")); {
		case strings.Contains(ct, "application/json"):
			contentType = "json"
		case strings.Contains(ct, "text/event-stream"):
			contentType = "sse"
		case strings.Contains(ct, "text/html"):
			contentType = "html"
		case strings.Contains(ct, "text/plain"):
			contentType = "text"
		}
	}
	base := d.lastAttemptBase
	if base == "" {
		base = fmt.Sprintf("req=%q ticket=%q attempt=%d gateway_request_id=%q protocol=%s", d.ReqID, d.Ticket, d.upstreamAttempts, d.GatewayRequestID, d.Protocol)
	}
	meta, valid := diagnosticErrorMetadata(raw, truncated)
	metadata, _ := json.Marshal(meta)
	web.Attempt(d.ReqID, fmt.Sprintf("event=response_body %s ts=%s phase=%s status=%d content_type=%s declared_bytes=%d body_read_bytes=%d body_truncated=%t read_err_kind=%s error_json=%t response_body_ref=%q error_meta=%s",
		base, diagnosticTime(d.Now()), phase, status, contentType, declaredLength, len(raw), truncated,
		diagnosticReadError(readErr), valid, diagnosticBytesRef(raw), metadata))
}

func diagnosticReadError(err error) string {
	if err == nil {
		return "-"
	}
	var ne net.Error
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "unexpected_eof"
	case errors.Is(err, io.EOF):
		return "eof"
	case errors.Is(err, syscall.ECONNRESET):
		return "connection_reset"
	case errors.Is(err, net.ErrClosed), errors.Is(err, syscall.EPIPE):
		return "connection_closed"
	default:
		return "read_failed"
	}
}

var diagnosticErrorTypes = map[string]bool{
	"error": true, "api_error": true, "authentication_error": true,
	"permission_error": true, "not_found_error": true, "invalid_request_error": true,
	"rate_limit_error": true, "overloaded_error": true, "server_error": true,
	"request_too_large": true, "insufficient_quota": true, "risk_control": true,
}

func diagnosticErrorMetadata(raw []byte, truncated bool) (map[string]any, bool) {
	meta := map[string]any{}
	// 超限/部分 JSON 不根据某个子串猜测业务码；观测不改变原分类器的判断。
	if truncated || len(raw) > maxErrorBodyBytes || !json.Valid(raw) {
		return meta, false
	}
	var root map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&root) != nil || root == nil {
		return meta, false
	}
	errorObject, _ := root["error"].(map[string]any)
	details, _ := errorObject["details"].(map[string]any)
	rootDetails, _ := root["details"].(map[string]any)
	for _, object := range []struct {
		prefix string
		value  map[string]any
	}{{"", root}, {"error.", errorObject}, {"error.details.", details}, {"details.", rootDetails}} {
		for _, name := range []string{"code", "type", "reason_code"} {
			if value, exists := object.value[name]; exists {
				key := object.prefix + name
				var text string
				switch value := value.(type) {
				case string:
					text = value
				case json.Number:
					text = value.String()
				default:
					meta[key+"_non_scalar"] = true
					continue
				}
				if diagnosticCode.MatchString(text) || (name == "type" && diagnosticErrorTypes[text]) {
					meta[key] = text
				} else {
					meta[key+"_ref"] = attemptIDRef(text)
				}
			}
		}
		// 每层最多一个消息摘要，既能比较相同错误，又不会抄录可能回显的用户内容。
		for _, name := range []string{"msg", "message", "reason"} {
			if value, ok := object.value[name].(string); ok && value != "" {
				meta[object.prefix+"message_ref"] = attemptIDRef(value)
				meta[object.prefix+"message_bytes"] = len(value)
				break
			}
		}
		if retryable, ok := object.value["retryable"].(bool); ok {
			meta[object.prefix+"retryable"] = retryable
		}
	}
	// 同时保留少量正文内的服务端追踪字段；未知格式仍只记摘要。
	for _, key := range []string{"request_id", "requestId", "log_id", "logId"} {
		if value, ok := root[key].(string); ok && value != "" {
			id, ref := diagnosticResponseID(value)
			meta[key], meta[key+"_ref"] = id, ref
		}
	}
	return meta, true
}
