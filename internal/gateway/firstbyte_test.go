package gateway

import (
	"context"
	"testing"
	"time"
)

func TestFirstByteWatchdogFires(t *testing.T) {
	w := newFirstByteWatchdog(context.Background(), 50*time.Millisecond, "line-a")
	defer w.Stop()
	if w.Fired() {
		t.Fatal("初始不应触发")
	}
	time.Sleep(120 * time.Millisecond)
	if !w.Fired() {
		t.Fatal("超过预算必须触发")
	}
	if w.Context().Err() == nil {
		t.Fatal("触发后 ctx 应已取消，client.Do 才会返回")
	}
	if w.Identity() != "line-a" {
		t.Fatalf("线路身份应原样带出：%s", w.Identity())
	}
}

func TestFirstByteWatchdogHeadersStopsTimer(t *testing.T) {
	w := newFirstByteWatchdog(context.Background(), 50*time.Millisecond, "")
	defer w.Stop()
	w.Headers(nil)
	time.Sleep(120 * time.Millisecond)
	if w.Fired() {
		t.Fatal("收到响应头后不得再触发（长流不能被预算打断）")
	}
	if w.Context().Err() != nil {
		t.Fatal("收到响应头后 ctx 必须仍然可用")
	}
}

func TestFirstByteWatchdogDisabled(t *testing.T) {
	w := newFirstByteWatchdog(context.Background(), 0, "")
	defer w.Stop()
	time.Sleep(60 * time.Millisecond)
	if w.Fired() || w.Context().Err() != nil {
		t.Fatal("预算为 0 时应完全关闭看门狗")
	}
}

func TestTimeoutStatus(t *testing.T) {
	for status, want := range map[int]bool{408: true, 524: true, 504: false, 500: false, 405: false, 200: false} {
		if got := timeoutStatus(status); got != want {
			t.Fatalf("timeoutStatus(%d)=%v，期望 %v", status, got, want)
		}
	}
}
