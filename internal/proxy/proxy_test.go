// proxy 包单测：Transport 缓存、URL 校验、socks5 握手（对本地假代理）。
package proxy

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestTransportForValidation(t *testing.T) {
	if _, err := TransportFor("ftp://x"); err == nil {
		t.Fatal("不支持的协议应报错")
	}
	if _, err := TransportFor(""); err != nil {
		t.Fatalf("空代理（直连）不应报错: %v", err)
	}
}

func TestTransportForCache(t *testing.T) {
	a, err := TransportFor("http://127.0.0.1:7890")
	if err != nil {
		t.Fatal(err)
	}
	b, err := TransportFor("http://127.0.0.1:7890")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("同 URL 应命中缓存返回同一 Transport")
	}
	c, err := TransportFor("")
	if err != nil {
		t.Fatal(err)
	}
	if c == a {
		t.Fatal("直连与代理不应共用 Transport")
	}
}

// fakeSocks5 启动一个最小 socks5 服务器：支持无鉴权 CONNECT 并回成功，
// 之后把后续字节转发给 behind（模拟真实目标）。
func fakeSocks5(t *testing.T, behind *httptest.Server) string {
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
			go serveSocks5(conn, behind)
		}
	}()
	return ln.Addr().String()
}

func serveSocks5(conn net.Conn, behind *httptest.Server) {
	defer conn.Close()
	head := make([]byte, 2)
	if _, e := readFull(conn, head); e != nil || head[0] != 0x05 {
		return
	}
	methods := make([]byte, head[1])
	if _, e := readFull(conn, methods); e != nil {
		return
	}
	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
		return
	}
	req := make([]byte, 4)
	if _, e := readFull(conn, req); e != nil || req[1] != 0x01 {
		return
	}
	var host string
	switch req[3] {
	case 0x01:
		ip := make([]byte, 4)
		if _, e := readFull(conn, ip); e != nil {
			return
		}
		host = net.IP(ip).String()
	case 0x03:
		n := make([]byte, 1)
		if _, e := readFull(conn, n); e != nil {
			return
		}
		name := make([]byte, n[0])
		if _, e := readFull(conn, name); e != nil {
			return
		}
		host = string(name)
	default:
		return
	}
	portBytes := make([]byte, 2)
	if _, e := readFull(conn, portBytes); e != nil {
		return
	}
	port := binary.BigEndian.Uint16(portBytes)
	if _, err := conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	backend, err := net.Dial("tcp", net.JoinHostPort(host, fmt.Sprint(port)))
	if err != nil {
		return
	}
	defer backend.Close()
	go copyBoth(backend, conn)
	copyBoth(conn, backend)
}

func copyBoth(dst net.Conn, src net.Conn) {
	buf := make([]byte, 4096)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func TestSocks5HandshakeThroughFakeProxy(t *testing.T) {
	behind := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Seen-Host", r.Host)
		w.WriteHeader(http.StatusOK)
	}))
	defer behind.Close()

	proxyAddr := fakeSocks5(t, behind)
	behindURL, _ := url.Parse(behind.URL)
	transport, err := TransportFor("socks5://" + proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + behindURL.Host + "/hello")
	if err != nil {
		t.Fatalf("经 socks5 代理请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应 200，得到 %d", resp.StatusCode)
	}
	if host := resp.Header.Get("X-Seen-Host"); !strings.Contains(host, behindURL.Host) {
		t.Fatalf("目标 Host 不符: %q", host)
	}
}

func TestSocks5RemoteResolveATYPDomain(t *testing.T) {
	// socks5h：域名由代理解析；假代理收到 ATYP=0x03 才算通过
	var seenATYP byte
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				head := make([]byte, 2)
				if _, e := readFull(c, head); e != nil {
					return
				}
				methods := make([]byte, head[1])
				if _, e := readFull(c, methods); e != nil {
					return
				}
				_, _ = c.Write([]byte{0x05, 0x00})
				req := make([]byte, 4)
				if _, e := readFull(c, req); e != nil {
					return
				}
				seenATYP = req[3]
				n := make([]byte, 1)
				if _, e := readFull(c, n); e != nil {
					return
				}
				rest := make([]byte, int(n[0])+2)
				if _, e := readFull(c, rest); e != nil {
					return
				}
				_, _ = c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
			}(conn)
		}
	}()

	u, _ := url.Parse("socks5h://" + ln.Addr().String())
	conn, err := net.DialTimeout("tcp", ln.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := socks5Handshake(ctx, conn, u, "example.com:80", true); err != nil {
		t.Fatalf("socks5h 握手失败: %v", err)
	}
	if seenATYP != 0x03 {
		t.Fatalf("socks5h 应以域名 ATYP=0x03 发送，得到 %d", seenATYP)
	}
}
