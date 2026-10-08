package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"testing"
	"time"
)

func TestProxyEndpointFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name        string
		err         error
		unreachable bool
	}{
		{"连接失败", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("refused")}, true},
		{"DNS失败", &net.DNSError{Err: "lookup failed", Name: "test.invalid"}, true},
		{"客户端超时", context.DeadlineExceeded, true},
		{"CONNECT失败", &net.OpError{Op: "proxyconnect", Net: "tcp", Err: errors.New("Bad Gateway")}, true},
		{"SOCKS失败", fmt.Errorf("%w: handshake rejected", ErrEndpointUnreachable), true},
		{"TLS协议", tls.RecordHeaderError{Msg: "invalid TLS"}, true},
		{"断连", io.EOF, true},
		{"响应中断", io.ErrUnexpectedEOF, true},
		{"取消", context.Canceled, false},
		{"普通错误", errors.New("ordinary business failure"), false},
		{"非法目标协议", errors.New("unsupported protocol scheme"), false},
		{"重定向循环", errors.New("stopped after 10 redirects"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := &url.Error{Op: "Get", URL: "https://test.invalid", Err: tc.err}
			if got := IsEndpointUnreachable(context.Background(), wrapped); got != tc.unreachable {
				t.Fatalf("不可达=%v，期望 %v：%v", got, tc.unreachable, tc.err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if IsEndpointUnreachable(ctx, io.EOF) {
		t.Fatal("父上下文取消不能被当作代理故障")
	}
	ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if IsEndpointUnreachable(ctx, context.DeadlineExceeded) {
		t.Fatal("调用方截止时间到期不同于代理客户端自身超时")
	}
}
