package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type shutdownTestListener struct {
	net.Listener
	closed chan struct{}
	once   sync.Once
}

func (l *shutdownTestListener) Close() error {
	err := l.Listener.Close()
	l.once.Do(func() { close(l.closed) })
	return err
}

func awaitShutdownSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("等待%s超时", what)
	}
}

func TestServeHTTPWaitsForInflightBeforeCleanup(t *testing.T) {
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	var cleanedUp, cleanupTooEarly atomic.Bool
	srv := newServer("", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "begin/")
		w.(http.Flusher).Flush()
		close(started)
		<-release
		cleanupTooEarly.Store(cleanedUp.Load())
		_, _ = io.WriteString(w, "end")
		close(finished)
	}))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := &shutdownTestListener{Listener: ln, closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer srv.Close()
	done := make(chan error, 1)
	go func() {
		err := serveHTTP(ctx, srv, listener, time.Second)
		cleanedUp.Store(true) // 对应 serve 返回后执行的数据库/浏览器池清理。
		done <- err
	}()
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	awaitShutdownSignal(t, started, "在途请求开始")
	cancel()
	awaitShutdownSignal(t, listener.closed, "监听关闭")
	select {
	case err := <-done:
		t.Fatalf("请求尚未结束，生命周期函数却提前返回：%v", err)
	case <-time.After(100 * time.Millisecond):
	}
	unblock()
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "begin/end" {
		t.Fatalf("停机应等待响应完整交付：body=%q err=%v", body, err)
	}
	awaitShutdownSignal(t, finished, "请求完成")
	select {
	case err := <-done:
		if err != nil || cleanupTooEarly.Load() {
			t.Fatalf("请求收尾前不应清理依赖：err=%v early=%v", err, cleanupTooEarly.Load())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("请求已结束，但停机未返回")
	}
}

func TestServeHTTPClosesConnectionsAfterBudget(t *testing.T) {
	started, canceled := make(chan struct{}), make(chan struct{})
	srv := newServer("", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done() // 超预算时必须强制关连接，才能取消卡住的上游请求。
		close(canceled)
	}))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer srv.Close()
	done := make(chan error, 1)
	go func() { done <- serveHTTP(ctx, srv, ln, 50*time.Millisecond) }()
	client := &http.Client{Timeout: 10 * time.Second} // 必须长于断言期限，不能靠客户端超时替服务端取消。
	resp, err := client.Get("http://" + ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	awaitShutdownSignal(t, started, "在途请求开始")
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("受控停机不应变成监听错误：%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("停机没有遵守时间预算")
	}
	awaitShutdownSignal(t, canceled, "超预算请求取消")
}

func TestServeHTTPReturnsListenErrorWithoutWaitingForSignal(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serveHTTP(ctx, newServer("", http.NewServeMux()), ln, time.Second) }()
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
			t.Fatalf("监听失败应直接返回原错误，不等待退出信号：%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("监听失败却仍在等待退出信号")
	}
}
