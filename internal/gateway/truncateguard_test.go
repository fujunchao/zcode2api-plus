package gateway

import (
	"testing"
	"time"
)

// 重试感知选线：同一内容在 TTL 内记住断流线路；不同内容/过期/关闭互不影响。
func TestTruncateRetryGuard(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { return now }
	g := NewTruncateRetryGuard(2*time.Minute, clock)

	g.Note("key-1", "line-a", now)
	g.Note("key-1", "line-b", now.Add(10*time.Second))
	if got := g.Avoid("key-1", now.Add(20*time.Second)); len(got) != 2 {
		t.Fatalf("同内容的断流线路应全部避让，实际 %v", got)
	}
	if got := g.Avoid("key-2", now); len(got) != 0 {
		t.Fatalf("不同内容不应互相影响：%v", got)
	}
	// TTL 过期后不再避让。
	if got := g.Avoid("key-1", now.Add(3*time.Minute)); len(got) != 0 {
		t.Fatalf("TTL 过期后不应避让：%v", got)
	}
	if got := g.Avoid("key-1", now.Add(4*time.Minute)); len(got) != 0 {
		t.Fatalf("过期条目应被清理：%v", got)
	}
}

// 关闭开关（ttl=0）返回 nil 表；nil 接收者调用必须安全。
func TestTruncateRetryGuardDisabled(t *testing.T) {
	if g := NewTruncateRetryGuard(0, time.Now); g != nil {
		t.Fatalf("ttl=0 应返回 nil 表")
	}
	var g *TruncateRetryGuard
	g.Note("k", "line-a", time.Now())
	if got := g.Avoid("k", time.Now()); got != nil {
		t.Fatalf("nil 表不应避让：%v", got)
	}
	if n := g.Cleanup(time.Now()); n != 0 {
		t.Fatalf("nil 表清理应返回 0：%d", n)
	}
}

// 触顶时整体清空而不是无限增长（与 ReplayGuard 同侧安全选择）。
func TestTruncateRetryGuardCap(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	g := NewTruncateRetryGuard(time.Hour, func() time.Time { return now })
	for i := 0; i < truncateRetryMaxEntries+10; i++ {
		g.Note(string(rune('a'+i%26))+string(rune('0'+i/26)), "line-a", now)
	}
	g.mu.Lock()
	size := len(g.entries)
	g.mu.Unlock()
	if size > truncateRetryMaxEntries {
		t.Fatalf("条目数应封顶 %d，实际 %d", truncateRetryMaxEntries, size)
	}
}
