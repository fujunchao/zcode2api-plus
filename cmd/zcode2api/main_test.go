package main

import (
	"net/http"
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
	if srv.Handler == nil {
		t.Fatal("Handler 不应为空")
	}
}

// 停机预算必须有上限：否则收到信号后可能一直等在途请求，容器只能被强杀。
func TestGracefulShutdownBudgetIsBounded(t *testing.T) {
	if gracefulShutdownTimeout <= 0 || gracefulShutdownTimeout > time.Minute {
		t.Fatalf("停机预算应落在 (0, 1m] 区间: %v", gracefulShutdownTimeout)
	}
}
