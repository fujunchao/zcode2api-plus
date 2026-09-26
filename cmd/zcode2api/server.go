package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"zcode2api/internal/web"
)

// serveHTTP 管理 HTTP 监听与停机；listener 由调用方创建，便于用真实本地连接
// 验证在途请求的收尾时序，而不需要启动完整账号池或发送进程级信号。
// Shutdown 会先关监听，使 Serve 立即返回 ErrServerClosed；因此必须由当前
// 协程等待 Shutdown，而不能把 Serve 返回误认为在途请求已全部结束。
func serveHTTP(ctx context.Context, srv *http.Server, listener net.Listener, budget time.Duration) error {
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(listener) }()
	var err error
	select {
	case err = <-serveDone:
		// 监听失败等启动错误不需要等待信号，也不遗留等待退出信号的协程。
	case <-ctx.Done():
		web.Ok("main", "收到退出信号，开始优雅停机…")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), budget)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			web.Warn("main", "优雅停机未在限时内完成: "+err.Error())
			// Shutdown 超时不会取消活跃请求。强制关连接，让请求上下文取消，
			// 避免在途上游调用继续占用即将关闭的数据库、验证码池等依赖。
			if err := srv.Close(); err != nil {
				web.Warn("main", "强制关闭连接失败: "+err.Error())
			}
		}
		err = <-serveDone
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
