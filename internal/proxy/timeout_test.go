package proxy

import (
	"context"
	"net"
	"net/url"
	"testing"
	"time"

	"zcode2api/internal/config"
)

func TestProxyTimeoutsComeFromConfig(t *testing.T) {
	oldDial, oldHandshake := config.ProxyDialTimeout, config.ProxyHandshakeTimeout
	t.Cleanup(func() { config.ProxyDialTimeout, config.ProxyHandshakeTimeout = oldDial, oldHandshake })

	config.ProxyDialTimeout, config.ProxyHandshakeTimeout = 3*time.Second, 4*time.Second
	if got := dialTimeout(); got != 3*time.Second {
		t.Fatalf("拨号超时应取配置值，实际 %v", got)
	}
	if got := handshakeTimeout(); got != 4*time.Second {
		t.Fatalf("握手超时应取配置值，实际 %v", got)
	}
	// 非法配置（0/负）必须回落到默认，绝不能变成「无限等待」。
	config.ProxyDialTimeout, config.ProxyHandshakeTimeout = 0, 0
	if got := dialTimeout(); got != fallbackDialTimeout {
		t.Fatalf("非法拨号配置应回落默认，实际 %v", got)
	}
	if got := handshakeTimeout(); got != fallbackHandshakeTimeout {
		t.Fatalf("非法握手配置应回落默认，实际 %v", got)
	}
}

// silentProxy 接受连接但永不回包，用来验证握手不会无限期挂住。
func silentProxy(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 512)
				for {
					if _, err := c.Read(buf); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	return ln.Addr().String()
}

func TestSocks5HandshakeHonorsBudget(t *testing.T) {
	old := config.ProxyHandshakeTimeout
	config.ProxyHandshakeTimeout = 80 * time.Millisecond
	t.Cleanup(func() { config.ProxyHandshakeTimeout = old })

	addr := silentProxy(t)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	u, _ := url.Parse("socks5://" + addr)
	start := time.Now()
	if err := socks5Handshake(context.Background(), conn, u, "127.0.0.1:9", false); err == nil {
		t.Fatal("无响应代理的握手必须失败")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("握手没有按预算超时：%v", elapsed)
	}
}

// socks4 此前完全没有 deadline，半死不活的代理能把握手无限期挂住。
func TestSocks4HandshakeHonorsBudget(t *testing.T) {
	old := config.ProxyHandshakeTimeout
	config.ProxyHandshakeTimeout = 80 * time.Millisecond
	t.Cleanup(func() { config.ProxyHandshakeTimeout = old })

	addr := silentProxy(t)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	u, _ := url.Parse("socks4://" + addr)
	start := time.Now()
	if err := socks4Handshake(conn, u, "127.0.0.1:9", false); err == nil {
		t.Fatal("无响应代理的 socks4 握手必须失败")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("socks4 握手没有按预算超时：%v", elapsed)
	}
}
