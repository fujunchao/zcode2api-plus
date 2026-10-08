package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

const (
	FailureEndpointUnreachable = "endpoint_unreachable"
	FailureClaimSuspicious     = "claim_suspicious"
)

// ErrEndpointUnreachable 标记本包 SOCKS 拨号/握手失败，避免通过错误文案猜测代理故障。
var ErrEndpointUnreachable = errors.New("代理无法连接目标端点")

// IsEndpointUnreachable 只识别传输层失败，不把普通 HTTP/业务错误归因给代理。
// 调用方的取消/截止时间不算故障；http.Client 自身超时而父上下文仍有效时则算不可达。
func IsEndpointUnreachable(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return false
	}
	var uerr *url.Error
	if errors.As(err, &uerr) {
		err = uerr.Err // url.Error 本身实现 net.Error，不能把所有 URL 错误都当作网络故障。
	}
	if err == nil {
		return false
	}
	if errors.Is(err, ErrEndpointUnreachable) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var networkError net.Error
	var tlsHeaderError tls.RecordHeaderError
	var tlsCertificateError *tls.CertificateVerificationError
	var protocolError *http.ProtocolError
	if errors.As(err, &networkError) || errors.As(err, &tlsHeaderError) ||
		errors.As(err, &tlsCertificateError) || errors.As(err, &protocolError) {
		return true
	}
	// net/http 的部分报文解析错误不是导出类型，仅匹配传输层固定前缀。
	return strings.HasPrefix(err.Error(), "malformed HTTP response") ||
		strings.HasPrefix(err.Error(), "net/http: HTTP/1.x transport connection broken:")
}
