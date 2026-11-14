package proxy

import (
	"bytes"
	"context"
	"net"
	"net/url"
	"testing"
	"time"
)

// captureSocks4Request 起一个只服务一次连接的假 SOCKS4 代理：读完整条请求后回 0x5a。
// 返回监听地址与「取请求字节」的取值函数（调用会阻塞到请求读完）。
//
// 请求形态：VER | CMD | PORT(2) | DSTIP(4) | USERID | 0x00
// 4a 时 DSTIP 固定为 0.0.0.x，随后再接「域名 | 0x00」。
func captureSocks4Request(t *testing.T) (string, func() []byte) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	got := make(chan []byte, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		head := make([]byte, 8)
		if _, err := readFull(conn, head); err != nil {
			return
		}
		buf := append([]byte{}, head...)
		if _, err := readUntilNUL(conn, &buf); err != nil { // userid
			return
		}
		if head[4] == 0 && head[5] == 0 && head[6] == 0 && head[7] != 0 { // 4a 标志
			if _, err := readUntilNUL(conn, &buf); err != nil { // 域名
				return
			}
		}
		got <- buf
		_, _ = conn.Write([]byte{0x00, 0x5a, 0, 0, 0, 0, 0, 0})
	}()
	return ln.Addr().String(), func() []byte { return <-got }
}

func readUntilNUL(conn net.Conn, buf *[]byte) (int, error) {
	for {
		one := make([]byte, 1)
		if _, err := readFull(conn, one); err != nil {
			return 0, err
		}
		*buf = append(*buf, one...)
		if one[0] == 0x00 {
			return len(*buf), nil
		}
	}
}

// 域名目标必须先本地解析成 IPv4 再发标准 SOCKS4。免费 socks4 代理绝大多数不实现
// 4a（2026-11-14 实测 33 条里 22 条只认本地解析、2 条认 4a），一律发 4a 等于把
// 这批线路全部判死——订阅导入的复检探针走的也是这条拨号路径。
func TestSocks4HandshakeResolvesLocally(t *testing.T) {
	old := lookupIPv4
	lookupIPv4 = func(context.Context, string) net.IP { return net.IPv4(203, 0, 113, 7).To4() }
	t.Cleanup(func() { lookupIPv4 = old })

	addr, request := captureSocks4Request(t)
	u, _ := url.Parse("socks4://" + addr)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := socks4Handshake(ctx, conn, u, "zcode.z.ai:443"); err != nil {
		t.Fatalf("socks4 握手失败: %v", err)
	}
	req := request()
	if len(req) < 9 {
		t.Fatalf("请求过短: %v", req)
	}
	if req[0] != 0x04 || req[1] != 0x01 {
		t.Fatalf("VER/CMD 不对: %v", req[:2])
	}
	if port := int(req[2])<<8 | int(req[3]); port != 443 {
		t.Fatalf("目标端口应为 443，得到 %d", port)
	}
	if got := net.IP(req[4:8]).String(); got != "203.0.113.7" {
		t.Fatalf("应以本地解析出的 IPv4 作为目标，得到 %q（0.0.0.1 是 4a 标志）", got)
	}
	if req[8] != 0x00 {
		t.Fatalf("userid 之后必须紧跟 NUL 结束符: %v", req[8:])
	}
}

// 解析不到 IPv4 时才退回 SOCKS4a：此时 DSTIP 固定 0.0.0.1，域名跟在 userid 之后。
func TestSocks4HandshakeFallsBackToRemoteResolve(t *testing.T) {
	old := lookupIPv4
	lookupIPv4 = func(context.Context, string) net.IP { return nil }
	t.Cleanup(func() { lookupIPv4 = old })

	addr, request := captureSocks4Request(t)
	u, _ := url.Parse("socks4://" + addr)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := socks4Handshake(ctx, conn, u, "example.com:443"); err != nil {
		t.Fatalf("socks4a 回落握手失败: %v", err)
	}
	req := request()
	if got := net.IP(req[4:8]).String(); got != "0.0.0.1" {
		t.Fatalf("4a 的 DSTIP 应为 0.0.0.1，得到 %q", got)
	}
	want := append([]byte{0x00}, []byte("example.com")...)
	want = append(want, 0x00)
	if !bytes.Equal(req[8:], want) {
		t.Fatalf("4a 尾部应为 userid NUL + 域名 NUL，得到 %v", req[8:])
	}
}

// IPv6 字面量目标在 SOCKS4 报文里没有位置：必须早失败，而不是发一个装不下的地址。
func TestSocks4HandshakeRejectsIPv6Target(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if conn, err := ln.Accept(); err == nil {
			_ = conn.Close()
		}
	}()

	u, _ := url.Parse("socks4://" + ln.Addr().String())
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := socks4Handshake(ctx, conn, u, "[2001:db8::1]:443"); err == nil {
		t.Fatal("IPv6 目标应直接报错")
	}
}
