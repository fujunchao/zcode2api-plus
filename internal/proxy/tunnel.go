package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DialTunnel 建立到 target（host:port）的裸隧道连接，用于吞吐测量。
//
// 为什么不复用 http.Transport：我们要的是「建连后直接往连接里写字节」，
// 而 transport 会先发一个 HTTP 请求、并在对端提前回 4xx 时中止写入——
// 那样测到的是对方的拒绝速度，不是链路带宽（2026-10-18 复盘）。
//
// 出站路径与超时预算和 TransportFor 保持一致：socks 复用同一份 dialer 与握手函数，
// http/https 走同一套 CONNECT 语义。
func DialTunnel(ctx context.Context, proxyURL, target string) (net.Conn, error) {
	raw := strings.TrimSpace(proxyURL)
	if raw == "" {
		return nil, errors.New("代理 URL 为空")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("代理 URL 无效: %v", err)
	}
	if _, _, err := net.SplitHostPort(target); err != nil {
		return nil, fmt.Errorf("探测目标无效: %v", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "socks4":
		return socksDialer(u, true)(ctx, "tcp", target)
	case "socks5", "socks5h":
		return socksDialer(u, false)(ctx, "tcp", target)
	case "http", "https":
		return dialConnectTunnel(ctx, u, target)
	default:
		return nil, fmt.Errorf("不支持的代理协议: %s", u.Scheme)
	}
}

// dialConnectTunnel 手写 CONNECT：不用 http.ReadResponse，避免它的 Body 语义
// 把隧道里的字节当成响应体读走或阻塞在 Close 上。
func dialConnectTunnel(ctx context.Context, u *url.URL, target string) (net.Conn, error) {
	addr := hostPort(u)
	if addr == "" {
		return nil, errors.New("代理缺少主机")
	}
	d := &net.Dialer{Timeout: dialTimeout(), KeepAlive: 30 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("%w: 连接代理失败: %w", ErrEndpointUnreachable, err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = conn.Close()
		}
	}()

	if strings.EqualFold(u.Scheme, "https") {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: u.Hostname()})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return nil, fmt.Errorf("%w: 代理 TLS 握手失败: %w", ErrEndpointUnreachable, err)
		}
		conn = tlsConn
	}

	deadline := time.Now().Add(handshakeTimeout())
	if ctxDeadline, has := ctx.Deadline(); has && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	_ = conn.SetDeadline(deadline)

	var head strings.Builder
	fmt.Fprintf(&head, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n", target, target)
	if u.User != nil {
		user := u.User.Username()
		pass, _ := u.User.Password()
		token := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
		fmt.Fprintf(&head, "Proxy-Authorization: Basic %s\r\n", token)
	}
	head.WriteString("\r\n")
	if _, err := conn.Write([]byte(head.String())); err != nil {
		return nil, fmt.Errorf("%w: CONNECT 写入失败: %w", ErrEndpointUnreachable, err)
	}

	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("%w: CONNECT 响应失败: %w", ErrEndpointUnreachable, err)
	}
	fields := strings.Fields(strings.TrimSpace(statusLine))
	code := 0
	if len(fields) >= 2 {
		code, _ = strconv.Atoi(fields[1])
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("%w: CONNECT 响应头失败: %w", ErrEndpointUnreachable, err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("%w: CONNECT HTTP %d", ErrEndpointUnreachable, code)
	}
	_ = conn.SetDeadline(time.Time{})
	ok = true
	return conn, nil
}
