package main

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestHTTPAndBackgroundCleanupShareBudget(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	release, entered, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer func() { close(release); <-finished }()
	done := make(chan error, 1)
	go func() {
		done <- serveHTTP(ctx, newServer("", http.NewServeMux()), ln, 80*time.Millisecond, func() {
			close(entered)
			<-release
			close(finished)
		})
	}()
	started := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("后台收尾突破共享停机预算")
	}
	<-entered
	if time.Since(started) > 500*time.Millisecond {
		t.Fatal("停机预算未生效")
	}
}
