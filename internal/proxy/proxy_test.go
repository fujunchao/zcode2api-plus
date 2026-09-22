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

// TestTransportForTimeoutSeparatesCacheByTimeout 缓存键必须包含超时值。
//
// 网关用 120s、async 池用 180s，两者都走 TransportForTimeout。若缓存键只有代理
// URL，先到的那次调用会把自己的 ResponseHeaderTimeout 固化进共享 Transport，
// 另一个用途静默拿到错误的超时（SSE 被提前掐断，且极难归因）。
// 本用例是缺口 0d370e5 的回归守卫：把 key 改回只含 URL，两条断言即红。
func TestTransportForTimeoutSeparatesCacheByTimeout(t *testing.T) {
	const gatewayTimeout, asyncTimeout = 120 * time.Second, 180 * time.Second

	g, err := TransportForTimeout("http://127.0.0.1:7890", gatewayTimeout)
	if err != nil {
		t.Fatal(err)
	}
	if g.ResponseHeaderTimeout != gatewayTimeout {
		t.Fatalf("网关用途应拿到 %v，实际 %v", gatewayTimeout, g.ResponseHeaderTimeout)
	}

	a, err := TransportForTimeout("http://127.0.0.1:7890", asyncTimeout)
	if err != nil {
		t.Fatal(err)
	}
	if a == g {
		t.Fatal("同 URL 不同超时必须各自持有一份 Transport，否则超时语义互相覆盖")
	}
	if a.ResponseHeaderTimeout != asyncTimeout {
		t.Fatalf("async 用途应拿到 %v，实际 %v", asyncTimeout, a.ResponseHeaderTimeout)
	}

	// 同 URL 同超时仍要命中缓存（缓存复用不能被这次改动破坏）
	g2, err := TransportForTimeout("http://127.0.0.1:7890", gatewayTimeout)
	if err != nil {
		t.Fatal(err)
	}
	if g2 != g {
		t.Fatal("同 URL 同超时应命中缓存返回同一 Transport")
	}

	// TransportFor 等价于「默认超时」那一档
	d, err := TransportFor("http://127.0.0.1:7890")
	if err != nil {
		t.Fatal(err)
	}
	if d != g {
		t.Fatal("TransportFor 应等价于 TransportForTimeout(默认 120s)")
	}

	// 直连（空 URL）同样按超时区分
	d180, err := TransportForTimeout("", asyncTimeout)
	if err != nil {
		t.Fatal(err)
	}
	d120, err := TransportForTimeout("", gatewayTimeout)
	if err != nil {
		t.Fatal(err)
	}
	if d180 == d120 {
		t.Fatal("直连的两种超时也不应共用 Transport")
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

// 本地解析得到 IPv6（或 IPv6 在首位）时，必须以 ATYP=0x04 发送，
// 不得送出 addr 长度为 0 的畸形 CONNECT（IPv4 优先，找不到才用 IPv6）。
func TestSocks5LocalResolveFallsBackToIPv6(t *testing.T) {
	var seenATYP byte
	var seenAddr []byte
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		head := make([]byte, 2)
		if _, e := readFull(conn, head); e != nil {
			return
		}
		methods := make([]byte, head[1])
		if _, e := readFull(conn, methods); e != nil {
			return
		}
		_, _ = conn.Write([]byte{0x05, 0x00})
		req := make([]byte, 4)
		if _, e := readFull(conn, req); e != nil {
			return
		}
		seenATYP = req[3]
		switch req[3] {
		case 0x01:
			seenAddr = make([]byte, 4)
		case 0x04:
			seenAddr = make([]byte, 16)
		case 0x03:
			n := make([]byte, 1)
			_, _ = readFull(conn, n)
			seenAddr = make([]byte, int(n[0]))
		}
		if _, e := readFull(conn, seenAddr); e != nil {
			return
		}
		port := make([]byte, 2)
		if _, e := readFull(conn, port); e != nil {
			return
		}
		_, _ = conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	}()

	u, _ := url.Parse("socks5://" + ln.Addr().String())
	conn, err := net.DialTimeout("tcp", ln.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// 用只解析出 IPv6 的主机名触发回退路径
	if err := socks5Handshake(ctx, conn, u, "[::1]:80", false); err != nil {
		t.Fatalf("IPv6 目标握手失败: %v", err)
	}
	if seenATYP != 0x04 {
		t.Fatalf("IPv6 目标应以 ATYP=0x04 发送，得到 %d", seenATYP)
	}
	if len(seenAddr) != 16 {
		t.Fatalf("ATYP=0x04 应带 16 字节地址，得到 %d", len(seenAddr))
	}
}

// startSocks5Replier 起一个假 socks5：完成协商与 CONNECT 读取后，用 reply 回应。
func startSocks5Replier(t *testing.T, reply []byte) string {
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
				head := make([]byte, 2)
				if _, e := readFull(c, head); e != nil {
					return
				}
				methods := make([]byte, head[1])
				if _, e := readFull(c, methods); e != nil {
					return
				}
				if _, e := c.Write([]byte{0x05, 0x00}); e != nil {
					return
				}
				req := make([]byte, 4)
				if _, e := readFull(c, req); e != nil {
					return
				}
				n := make([]byte, 1)
				if _, e := readFull(c, n); e != nil {
					return
				}
				rest := make([]byte, int(n[0])+2)
				if _, e := readFull(c, rest); e != nil {
					return
				}
				_, _ = c.Write(reply)
			}(conn)
		}
	}()
	return ln.Addr().String()
}

// CONNECT 回复用 ATYP=0x03（域名型 BND.ADDR）时必须把「长度+域名+端口」读干净。
// 曾按 (len-1)+2 计算剩余字节，少读 1 字节——那一字节留在 socket 里被后续应用层
// 当成数据首字节（表现为 TLS 握手失败、请求行被吃掉）。ATYP 由代理决定，所以任何
// socks5 线路只要回域名型地址就会中招，不只是 socks5h。
func TestSocks5HandshakeConsumesDomainReply(t *testing.T) {
	const bndDomain = "proxy.local"
	marker := []byte("HELLO")
	reply := append([]byte{0x05, 0x00, 0x00, 0x03, byte(len(bndDomain))}, []byte(bndDomain)...)
	reply = append(reply, 0x1f, 0x90) // 端口 8080
	// 回复后面紧跟应用层数据：握手若少读，首字节就会被这条回复的尾巴污染。
	proxyAddr := startSocks5Replier(t, append(reply, marker...))

	u, _ := url.Parse("socks5://" + proxyAddr)
	conn, err := net.DialTimeout("tcp", proxyAddr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := socks5Handshake(ctx, conn, u, "example.com:80", true); err != nil {
		t.Fatalf("域名型回复握手失败: %v", err)
	}
	got := make([]byte, len(marker))
	if _, err := readFull(conn, got); err != nil {
		t.Fatalf("读应用层数据失败: %v", err)
	}
	if string(got) != string(marker) {
		t.Fatalf("握手读多了或读少了回复字节，应用层首字节被污染: got %q want %q", got, marker)
	}
}

func TestSocks5HandshakeRejectsBadVersion(t *testing.T) {
	// 非 0x05 的回复版本说明对端不是 socks5，不能当成成功握手继续使用隧道。
	proxyAddr := startSocks5Replier(t, []byte{0x04, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	u, _ := url.Parse("socks5://" + proxyAddr)
	conn, err := net.DialTimeout("tcp", proxyAddr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := socks5Handshake(ctx, conn, u, "example.com:80", true); err == nil {
		t.Fatal("回复版本非 0x05 应报错")
	}
}

func TestMaskURL(t *testing.T) {
	// 代理凭据不该出现在面向使用者的报错里。
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"带密码", "http://user:s3cret@1.2.3.4:8080", "http://user:***@1.2.3.4:8080"},
		{"仅用户名", "http://user@1.2.3.4:8080", "http://user@1.2.3.4:8080"},
		{"无凭据", "socks5://1.2.3.4:1080", "socks5://1.2.3.4:1080"},
		{"空串", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := MaskURL(c.in); got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
	// 解析失败时原样返回（此时也没有密码可泄）。
	if got := MaskURL("://bad"); got != "://bad" {
		t.Fatalf("解析失败应原样返回: %q", got)
	}
}
