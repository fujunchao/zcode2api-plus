package networkprobe

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// startConnectProxy 起一个最小 HTTP CONNECT 代理，把隧道接到真实目标。
// 测试不跨包复用 internal/proxy 里未导出的假代理，这里自带一份。
func startConnectProxy(t *testing.T) string {
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
				fields := strings.Fields(strings.Split(head.String(), "\r\n")[0])
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
	return "http://" + ln.Addr().String()
}

// targetServer 按 mode 决定怎么对待写入：
//   - drain：全部读掉
//   - early：读掉 limit 字节后关闭
//   - stall：完全不读（让内核缓冲塞满，写阻塞到超时）
func targetServer(t *testing.T, mode string, limit int64) string {
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
				switch mode {
				case "drain":
					_, _ = io.Copy(io.Discard, c)
				case "early":
					_, _ = io.CopyN(io.Discard, c, limit)
				case "stall":
					// 不读、不关：写端会阻塞到自己的 deadline。
					time.Sleep(5 * time.Second)
				}
			}(conn)
		}
	}()
	return ln.Addr().String()
}

func TestCheckTunnelWriteFull(t *testing.T) {
	proxyURL := startConnectProxy(t)
	target := targetServer(t, "drain", 0)
	rep := CheckTunnelWrite(context.Background(), proxyURL, target, 256<<10, 2*time.Second, 64<<10, 85)
	if !rep.OK || rep.Bytes != 256<<10 {
		t.Fatalf("写满应当 OK：%+v", rep)
	}
	if rep.Kbps <= 0 {
		t.Fatalf("速率应可计算：%+v", rep)
	}
}

// 对端读够就关：只要够快、写的量够，就算链路没问题。
func TestCheckTunnelWriteEarlyStopIsOK(t *testing.T) {
	proxyURL := startConnectProxy(t)
	target := targetServer(t, "early", 128<<10)
	rep := CheckTunnelWrite(context.Background(), proxyURL, target, 4<<20, 2*time.Second, 64<<10, 1)
	if !rep.OK || !rep.EarlyStop {
		t.Fatalf("对端早关且够快应判 OK：%+v", rep)
	}
}

// 对端完全不读：内核缓冲塞满后写阻塞到超时，必须判失败。
func TestCheckTunnelWriteStallFails(t *testing.T) {
	proxyURL := startConnectProxy(t)
	target := targetServer(t, "stall", 0)
	start := time.Now()
	// 64MB 远大于两端内核缓冲之和：缓冲被填满后写必然阻塞到 deadline。
	rep := CheckTunnelWrite(context.Background(), proxyURL, target, 64<<20, 300*time.Millisecond, 64<<10, 85)
	if rep.OK {
		t.Fatalf("写不动必须判失败：%+v", rep)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("必须按预算结束：%v", elapsed)
	}
}

func TestCheckTunnelWriteDialFailure(t *testing.T) {
	// 代理端口不通：应立即失败，不写字节。
	rep := CheckTunnelWrite(context.Background(), "http://127.0.0.1:1", "127.0.0.1:443", 1024, time.Second, 512, 85)
	if rep.OK || rep.Bytes != 0 || rep.Error == "" {
		t.Fatalf("建连失败应直接判失败：%+v", rep)
	}
}
