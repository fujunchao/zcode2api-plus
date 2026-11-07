package gateway

import (
	"slices"
	"testing"
	"time"
)

func TestTruncateRetryGuardPerLineTTL(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	g := NewTruncateRetryGuard(120*time.Second, func() time.Time { return now })
	g.Note("same-content", "line-a", now)
	// 第一次重试本身可能持续接近两分钟：第二次断流应获得自己的完整有效期。
	g.Note("same-content", "line-b", now.Add(119*time.Second))
	if got := g.Avoid("same-content", now.Add(121*time.Second)); len(got) != 1 || got[0] != "line-b" {
		t.Fatalf("A 应过期，刚断流的 B 应继续避让: %v", got)
	}
	if got := g.Avoid("same-content", now.Add(238*time.Second)); !slices.Contains(got, "line-b") {
		t.Fatalf("B 应从自己的断流时间起保留 120 秒: %v", got)
	}
	if got := g.Avoid("same-content", now.Add(240*time.Second)); len(got) != 0 {
		t.Fatalf("B 过期后应恢复可选: %v", got)
	}
}

func TestTruncateRetryGuardRepeatedLineRenewsTTL(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	g := NewTruncateRetryGuard(120*time.Second, time.Now)
	g.Note("same-content", "line-a", now)
	g.Note("same-content", "line-a", now.Add(119*time.Second))
	// 并发收尾可能以旧时间稍后入锁，不能缩短最近一次断流的保护期。
	g.Note("same-content", "line-a", now.Add(10*time.Second))
	if got := g.Avoid("same-content", now.Add(121*time.Second)); len(got) != 1 || got[0] != "line-a" {
		t.Fatalf("同线再次断流应续期，旧事件不能覆盖新时间: %v", got)
	}
}

func TestTruncateRetryGuardCleanupKeepsFreshLine(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	g := NewTruncateRetryGuard(120*time.Second, time.Now)
	g.Note("same-content", "line-a", now)
	g.Note("same-content", "line-b", now.Add(119*time.Second))
	if n := g.Cleanup(now.Add(121 * time.Second)); n != 0 {
		t.Errorf("内容还有有效线路时不能删除整条记录，实际删除 %d 条", n)
	}
	if got := g.Avoid("same-content", now.Add(121*time.Second)); len(got) != 1 || got[0] != "line-b" {
		t.Fatalf("清理过期 A 不得删除仍有效的 B: %v", got)
	}
	if n := g.Cleanup(now.Add(240 * time.Second)); n != 1 {
		t.Fatalf("全部线路过期后才应删除整条内容记录，实际 %d", n)
	}
}
