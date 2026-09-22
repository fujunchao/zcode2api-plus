package store

import (
	"testing"
	"time"

	"zcode2api/internal/model"
)

// TestLineTruncateCounterSemantics：线路断流计数的「连续 / 累计 / 复位 / 清理」语义。
// 累计只增不清（对齐账号级 StreamTruncateCount）；连续在 Reset 后归零；
// 线路被删除时两类条目一并清理。
func TestLineTruncateCounterSemantics(t *testing.T) {
	s := newTestStore(t)
	p, err := s.AddProxyProfile("line-a", "http://127.0.0.1:9", true)
	if err != nil {
		t.Fatal(err)
	}

	for want := 1; want <= 3; want++ {
		if got := s.BumpLineTruncate(p.ID); got != want {
			t.Fatalf("BumpLineTruncate 第 %d 次返回 %d", want, got)
		}
	}
	s.ResetLineTruncate(p.ID)
	if got := s.BumpLineTruncate(p.ID); got != 1 {
		t.Fatalf("复位后连续计数应从 1 重新开始，实际 %d", got)
	}
	stats := s.LineTruncateStats()
	if got := stats[p.ID]; got.Streak != 1 || got.Total != 4 {
		t.Fatalf("复位只清连续、累计应保留：streak=%d total=%d", got.Streak, got.Total)
	}

	// 删除线路清理计数条目。
	if _, _, err := s.PurgeProxyProfiles([]string{p.ID}); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.LineTruncateStats()[p.ID]; ok {
		t.Fatal("线路删除后计数条目应被清理")
	}
}

// TestSelectAvoidsRecentlyTruncatedAccount：断流短回避是「仅选号层」的软过滤——
// 池内还有别的账号时跳过被回避者；全部被回避时不过滤（软过滤永远不能让 Select
// 选不出号）；回避到期自动失效。
func TestSelectAvoidsRecentlyTruncatedAccount(t *testing.T) {
	s := newTestStore(t)
	a, err := s.AddAccount(model.ProviderZai, "avoid-a", "sk-aaa")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.AddAccount(model.ProviderZai, "avoid-b", "sk-bbb")
	if err != nil {
		t.Fatal(err)
	}

	// 回避 a（未来的截止时刻）；b 不受影响。
	future := float64(time.Now().Add(60*time.Second).UnixNano()) / 1e9
	if _, err := s.Update(model.ProviderZai, a.ID, func(live *model.Account) {
		live.TruncateAvoidUntil = future
	}); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		got := s.Select(model.ProviderZai, nil, "")
		if got == nil {
			t.Fatal("应能选出账号")
		}
		if got.ID == a.ID {
			t.Fatalf("第 %d 次选号命中被回避账号（池内另有可用账号）", i+1)
		}
	}

	// 全池被回避：不过滤，照常选出。
	past := float64(time.Now().Add(-time.Second).UnixNano()) / 1e9
	if _, err := s.Update(model.ProviderZai, b.ID, func(live *model.Account) {
		live.TruncateAvoidUntil = future
	}); err != nil {
		t.Fatal(err)
	}
	if got := s.Select(model.ProviderZai, nil, ""); got == nil {
		t.Fatal("全池被回避时不应选不出号")
	}

	// 回避到期：a 恢复参与（轮询会重新轮到它）。
	if _, err := s.Update(model.ProviderZai, a.ID, func(live *model.Account) {
		live.TruncateAvoidUntil = past
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(model.ProviderZai, b.ID, func(live *model.Account) {
		live.TruncateAvoidUntil = past
	}); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for i := 0; i < 10; i++ {
		seen[s.Select(model.ProviderZai, nil, "").ID] = true
	}
	if !seen[a.ID] || !seen[b.ID] {
		t.Fatalf("回避到期后两账号都应重新参与轮询: %v", seen)
	}
}
