package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSubscriptionTransportLifecycle(t *testing.T) {
	raw := "http://127.0.0.1:19821"
	a, _ := TransportForTimeout(raw, time.Second)
	b, _ := NewTransport(raw, time.Second)
	c, _ := NewTransport(raw, time.Second)
	if a == b || b == c {
		t.Fatal("候选不能共享生产缓存")
	}
	b.CloseIdleConnections()
	c.CloseIdleConnections()
	cached, _ := TransportForTimeout(raw, time.Second)
	if cached != a {
		t.Fatal("候选释放不能改动生产缓存")
	}
	ForgetTransport(raw)
	next, _ := TransportForTimeout(raw, time.Second)
	if next == a {
		t.Fatal("退役端点未清理")
	}
	for i := range 600 {
		tr, err := TransportFor(fmt.Sprintf("http://127.0.0.1:%d", 20000+i))
		if err != nil {
			t.Fatal(err)
		}
		_ = tr
	}
	transportMu.Lock()
	n := len(transportPrec)
	transportMu.Unlock()
	if n > 512 {
		t.Fatalf("生产缓存无界: %d", n)
	}
}

func TestRetiringTransportKeepsInflightBody(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("start"))
		w.(http.Flusher).Flush()
		<-release
		_, _ = w.Write([]byte("end"))
	}))
	defer server.Close()
	tr, _ := TransportFor(server.URL)
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "http://fixture.invalid/", nil)
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	ForgetTransport(server.URL)
	close(release)
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || string(body) != "startend" {
		t.Fatalf("清理中断了在途流: %s %v", body, err)
	}
}
