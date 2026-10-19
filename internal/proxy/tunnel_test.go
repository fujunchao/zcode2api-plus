package proxy

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// drainServer 接受连接并持续读取，模拟「对端愿意收」。
func drainServer(t *testing.T) net.Listener {
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
				_, _ = io.Copy(io.Discard, c)
			}(conn)
		}
	}()
	return ln
}

// fakeConnectProxy 是最小的 HTTP CONNECT 代理，只够验证 DialTunnel 的隧道建立。
func fakeConnectProxy(t *testing.T) net.Listener {
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
				buf := make([]byte, 4096)
				var head strings.Builder
				for !strings.Contains(head.String(), "\r\n\r\n") {
					n, err := c.Read(buf)
					if err != nil {
						return
					}
					head.Write(buf[:n])
				}
				lines := strings.Split(head.String(), "\r\n")
				fields := strings.Fields(lines[0])
				if len(fields) < 2 {
					return
				}
				backend, err := net.Dial("tcp", fields[1])
				if err != nil {
					_, _ = c.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
					return
				}
				defer backend.Close()
				if _, err := c.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
					return
				}
				go func() { _, _ = io.Copy(backend, c) }()
				_, _ = io.Copy(c, backend)
			}(conn)
		}
	}()
	return ln
}

func TestDialTunnelSocks5(t *testing.T) {
	backend := drainServer(t)
	socks := fakeSocks5(t, nil)
	conn, err := DialTunnel(context.Background(), "socks5://"+socks, backend.Addr().String())
	if err != nil {
		t.Fatalf("socks5 隧道建立失败: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatalf("隧道写入失败: %v", err)
	}
}

func TestDialTunnelHTTPConnect(t *testing.T) {
	backend := drainServer(t)
	proxyLn := fakeConnectProxy(t)
	conn, err := DialTunnel(context.Background(), "http://"+proxyLn.Addr().String(), backend.Addr().String())
	if err != nil {
		t.Fatalf("http CONNECT 隧道建立失败: %v", err)
	}
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatalf("隧道写入失败: %v", err)
	}
}

func TestDialTunnelRejectsBadInput(t *testing.T) {
	if _, err := DialTunnel(context.Background(), "", "127.0.0.1:443"); err == nil {
		t.Fatal("空代理 URL 必须报错")
	}
	if _, err := DialTunnel(context.Background(), "http://127.0.0.1:1", "no-port"); err == nil {
		t.Fatal("目标缺端口必须报错")
	}
}
