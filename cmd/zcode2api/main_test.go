package main

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 服务端超时是刻意配置的：零值 http.Server 既不防御慢速头攻击，也不回收空闲连接。
// WriteTimeout 必须保持 0——SSE 是长连接，写超时会把正常的长流掐断。
func TestNewServerTimeouts(t *testing.T) {
	srv := newServer("127.0.0.1:0", http.NewServeMux())
	if srv.ReadHeaderTimeout != readHeaderTimeout || srv.ReadHeaderTimeout <= 0 {
		t.Fatalf("ReadHeaderTimeout 未设置: %v", srv.ReadHeaderTimeout)
	}
	if srv.IdleTimeout != idleTimeout || srv.IdleTimeout <= 0 {
		t.Fatalf("IdleTimeout 未设置: %v", srv.IdleTimeout)
	}
	if srv.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout 必须为 0（SSE 长连接），当前 %v", srv.WriteTimeout)
	}
	if srv.MaxHeaderBytes != maxHeaderBytes || srv.MaxHeaderBytes <= 0 {
		t.Fatalf("MaxHeaderBytes 未设置: %v", srv.MaxHeaderBytes)
	}
	if srv.Handler == nil {
		t.Fatal("Handler 不应为空")
	}
}

// 请求体上限是进程级防护：八个入口都把 body 直接解进 map，无上限时一个超大
// JSON 就能撑爆进程、连带杀掉在途 SSE 串流。中间件包在 mux 外层，新增端点
// 自动受保护。
func TestLimitBodyRejectsOversizedBody(t *testing.T) {
	var readErr error
	var readLen int
	h := limitBody(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		buf, err := io.ReadAll(r.Body)
		readErr, readLen = err, len(buf)
	}))

	small := strings.Repeat("a", 1024)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(small)))
	if readErr != nil || readLen != len(small) {
		t.Fatalf("正常大小的请求体不应受影响: err=%v n=%d", readErr, readLen)
	}

	// 超限请求体远大于上限：读取必须报错，且不会整段读进内存。
	big := bytes.Repeat([]byte("a"), maxBodyBytes*2)
	readErr, readLen = nil, 0
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(big)))
	var mbe *http.MaxBytesError
	if !errors.As(readErr, &mbe) {
		t.Fatalf("超限请求体应返回 MaxBytesError: %v", readErr)
	}
	if readLen > maxBodyBytes+1 {
		t.Fatalf("超限时不应把整段读进内存: 读了 %d 字节（上限 %d）", readLen, maxBodyBytes)
	}
}

// 停机预算必须有上限：否则收到信号后可能一直等在途请求，容器只能被强杀。
func TestGracefulShutdownBudgetIsBounded(t *testing.T) {
	if gracefulShutdownTimeout <= 0 || gracefulShutdownTimeout > time.Minute {
		t.Fatalf("停机预算应落在 (0, 1m] 区间: %v", gracefulShutdownTimeout)
	}
}
