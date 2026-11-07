package store

import (
	"fmt"
	"testing"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

// uplinkEgress 取账号当前的代理 egress；未绑定代理时直接失败。
func uplinkEgress(t *testing.T, s *Store, a *model.Account) ProxyEgress {
	t.Helper()
	e := s.ProxyEgress(a)
	if e.Mode != "proxy" || e.identity == "" {
		t.Fatalf("测试前置失败：账号未绑定代理 egress=%+v", e)
	}
	return e
}

// recordUplink 连续记录两次相同采样，把样本数凑到参与选路门槛。
// 单次样本不足以定性（可能撞上 TLS 冷启动），门槛判定要求 >= proxyUplinkMinSample。
func recordUplink(t *testing.T, s *Store, a *model.Account, kbps float64, now time.Time) {
	t.Helper()
	e := uplinkEgress(t, s, a)
	s.RecordProxyUplink(e, kbps, now)
	s.RecordProxyUplink(e, kbps, now)
}

func TestRecordProxyUplinkEWMA(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "线A", 18301, true)
	a := addSelectionAccount(t, s, "号A", p.ID)
	e := uplinkEgress(t, s, a)
	now := time.Now()

	s.RecordProxyUplink(e, 100, now)
	ent := s.proxyUplink.Entries[e.identity]
	if ent.Sample != 1 || ent.KBps != 100 {
		t.Fatalf("首个样本应直接赋值，实际 sample=%d kbps=%v", ent.Sample, ent.KBps)
	}

	s.RecordProxyUplink(e, 300, now)
	ent = s.proxyUplink.Entries[e.identity]
	want := 100*(1-proxyUplinkAlpha) + 300*proxyUplinkAlpha
	if ent.Sample != 2 {
		t.Fatalf("样本数应为 2，实际 %d", ent.Sample)
	}
	if diff := ent.KBps - want; diff > 0.001 || diff < -0.001 {
		t.Fatalf("EWMA 应为 %.3f，实际 %.3f", want, ent.KBps)
	}
}

func TestRecordProxyUplinkIgnoresDirectAndZero(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "线B", 18302, true)
	a := addSelectionAccount(t, s, "号B", p.ID)
	e := uplinkEgress(t, s, a)
	now := time.Now()

	s.RecordProxyUplink(e, 0, now)
	s.RecordProxyUplink(e, -5, now)
	if len(s.proxyUplink.Entries) != 0 {
		t.Fatalf("kbps<=0 不应建档，实际 %d 条", len(s.proxyUplink.Entries))
	}

	direct := ProxyEgress{ID: "", Mode: "direct"}
	s.RecordProxyUplink(direct, 500, now)
	if len(s.proxyUplink.Entries) != 0 {
		t.Fatalf("直连不应建档，实际 %d 条", len(s.proxyUplink.Entries))
	}
}

func TestProxyUplinkNeedsTwoSamples(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "线C", 18303, true)
	a := addSelectionAccount(t, s, "号C", p.ID)
	e := uplinkEgress(t, s, a)
	now := time.Now()

	s.RecordProxyUplink(e, 900, now)
	if _, ok := s.ProxyUplinkKBps(e.identity); ok {
		t.Fatal("单样本不足以参与选路，应返回 false")
	}
	s.RecordProxyUplink(e, 900, now)
	if kbps, ok := s.ProxyUplinkKBps(e.identity); !ok || kbps < 899 || kbps > 901 {
		t.Fatalf("两个样本后应可用，实际 kbps=%v ok=%v", kbps, ok)
	}
}

func TestProxyUplinkTTLPrune(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "线D", 18304, true)
	a := addSelectionAccount(t, s, "号D", p.ID)
	e := uplinkEgress(t, s, a)
	now := time.Now()

	s.RecordProxyUplink(e, 800, now)
	if len(s.proxyUplink.Entries) != 1 {
		t.Fatal("应有 1 条样本")
	}
	s.pruneProxyUplinkLocked(now.Add(proxyUplinkTTL + time.Minute))
	if len(s.proxyUplink.Entries) != 0 {
		t.Fatalf("超过 TTL 应被淘汰，实际 %d 条", len(s.proxyUplink.Entries))
	}
}

func TestProxyUplinkLimitTruncate(t *testing.T) {
	s := newTestStore(t)
	base := time.Now()
	s.proxyUplink.Entries = map[string]ProxyUplink{}
	for i := 0; i < proxyUplinkLimit+5; i++ {
		id := fmt.Sprintf("identity-%05d", i)
		s.proxyUplink.Entries[id] = ProxyUplink{Identity: id, KBps: 100, Sample: 2, LastAt: base.UnixMilli() + int64(i)}
	}
	s.pruneProxyUplinkLocked(base.Add(time.Minute))
	if len(s.proxyUplink.Entries) != proxyUplinkLimit {
		t.Fatalf("应按上限截断到 %d，实际 %d", proxyUplinkLimit, len(s.proxyUplink.Entries))
	}
	// 最旧的 5 条应被淘汰，最新的必须还在。
	if _, ok := s.proxyUplink.Entries["identity-00000"]; ok {
		t.Fatal("最旧条目应被淘汰")
	}
	if _, ok := s.proxyUplink.Entries[fmt.Sprintf("identity-%05d", proxyUplinkLimit+4)]; !ok {
		t.Fatal("最新条目不应被淘汰")
	}
}

// TestSelectPrefersFastestUplinkBand 是本次改造的核心断言：
// 三条线路带宽差异明显（800 / 200 / 30），选路必须落在最快的那个档位。
//
// 刻意用 800 vs 30 这种真实差异而不是 620 vs 600 的边界值：
// 边界值测不出分档是否真的生效，只会让测试「碰巧通过」。
func TestSelectPrefersFastestUplinkBand(t *testing.T) {
	s := newTestStore(t)
	fastLine := addSelectionProxy(t, s, "快线", 18311, true)
	midLine := addSelectionProxy(t, s, "中线", 18312, true)
	slowLine := addSelectionProxy(t, s, "慢线", 18313, true)
	fastAcc := addSelectionAccount(t, s, "快号", fastLine.ID)
	midAcc := addSelectionAccount(t, s, "中号", midLine.ID)
	slowAcc := addSelectionAccount(t, s, "慢号", slowLine.ID)

	now := time.Now()
	recordUplink(t, s, fastAcc, 800, now)
	recordUplink(t, s, midAcc, 200, now)
	recordUplink(t, s, slowAcc, 30, now)

	// 连续选 6 次都必须落在快线上——收窄发生在轮询之前，
	// 所以即便轮询游标在转，候选池里也只有快线一个账号。
	for i := 0; i < 6; i++ {
		got := s.Select(model.ProviderZai, nil, "")
		if got == nil || got.ID != fastAcc.ID {
			t.Fatalf("第 %d 次选择应落在快线上，实际 %+v", i, got)
		}
	}
}

func TestSelectUplinkBandKeepsRotationWithinBand(t *testing.T) {
	s := newTestStore(t)
	p1 := addSelectionProxy(t, s, "快线1", 18321, true)
	p2 := addSelectionProxy(t, s, "快线2", 18322, true)
	p3 := addSelectionProxy(t, s, "快线3", 18323, true)
	a1 := addSelectionAccount(t, s, "号1", p1.ID)
	a2 := addSelectionAccount(t, s, "号2", p2.ID)
	a3 := addSelectionAccount(t, s, "号3", p3.ID)

	now := time.Now()
	recordUplink(t, s, a1, 900, now)
	recordUplink(t, s, a2, 850, now)
	recordUplink(t, s, a3, 800, now)

	// 同档内必须继续轮询，不能把流量压到一个账号上。
	// 3 个账号各选 2 次 = 6 次，每个账号应恰好被选 2 次。
	counts := map[string]int{}
	for i := 0; i < 6; i++ {
		got := s.Select(model.ProviderZai, nil, "")
		if got == nil {
			t.Fatal("不应返回空")
		}
		counts[got.ID]++
	}
	if len(counts) != 3 {
		t.Fatalf("同档内应轮询到全部 3 个账号，实际 %v", counts)
	}
	for id, n := range counts {
		if n != 2 {
			t.Fatalf("账号 %s 应被选 2 次，实际 %d（分布 %v）", id, n, counts)
		}
	}
}

func TestSelectWithoutUplinkSamplesDoesNotNarrow(t *testing.T) {
	s := newTestStore(t)
	p1 := addSelectionProxy(t, s, "线1", 18331, true)
	p2 := addSelectionProxy(t, s, "线2", 18332, true)
	a1 := addSelectionAccount(t, s, "号1", p1.ID)
	a2 := addSelectionAccount(t, s, "号2", p2.ID)

	// 没有任何带宽样本：行为必须与改造前一致（正常轮询，两个账号都能被选到）。
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		got := s.Select(model.ProviderZai, nil, "")
		if got == nil {
			t.Fatal("无样本时不应返回空")
		}
		seen[got.ID] = true
	}
	if !seen[a1.ID] || !seen[a2.ID] {
		t.Fatalf("无样本时不应收窄候选，实际只选到 %v", seen)
	}
}

func TestSelectUplinkBandIgnoresUnsampelledWhenFastExists(t *testing.T) {
	s := newTestStore(t)
	fastLine := addSelectionProxy(t, s, "快线", 18341, true)
	unknownLine := addSelectionProxy(t, s, "未知线", 18342, true)
	fastAcc := addSelectionAccount(t, s, "快号", fastLine.ID)
	addSelectionAccount(t, s, "未知号", unknownLine.ID)

	recordUplink(t, s, fastAcc, 1000, time.Now())

	// 未知档（0）的优先级低于已知快档（3），不应被选中。
	for i := 0; i < 4; i++ {
		got := s.Select(model.ProviderZai, nil, "")
		if got == nil || got.ID != fastAcc.ID {
			t.Fatalf("有已知快线时不应选中无样本线路，实际 %+v", got)
		}
	}
}

func TestSelectUplinkBandCanBeDisabled(t *testing.T) {
	old := config.SelectUplinkBand
	config.SelectUplinkBand = false
	t.Cleanup(func() { config.SelectUplinkBand = old })

	s := newTestStore(t)
	fastLine := addSelectionProxy(t, s, "快线", 18351, true)
	slowLine := addSelectionProxy(t, s, "慢线", 18352, true)
	fastAcc := addSelectionAccount(t, s, "快号", fastLine.ID)
	slowAcc := addSelectionAccount(t, s, "慢号", slowLine.ID)

	now := time.Now()
	recordUplink(t, s, fastAcc, 1000, now)
	recordUplink(t, s, slowAcc, 20, now)

	// 关闭开关后必须退回改造前的纯轮询：两个账号都会被选到。
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		got := s.Select(model.ProviderZai, nil, "")
		if got == nil {
			t.Fatal("关闭开关后不应返回空")
		}
		seen[got.ID] = true
	}
	if !seen[fastAcc.ID] || !seen[slowAcc.ID] {
		t.Fatalf("关闭开关后不应按带宽收窄，实际只选到 %v", seen)
	}
}

func TestProxyUplinkSnapshotExcludesLowSample(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "线E", 18361, true)
	a := addSelectionAccount(t, s, "号E", p.ID)
	e := uplinkEgress(t, s, a)
	now := time.Now()

	s.RecordProxyUplink(e, 700, now)
	if snap := s.ProxyUplinkSnapshot(); len(snap) != 0 {
		t.Fatalf("单样本不应出现在快照里，实际 %v", snap)
	}
	s.RecordProxyUplink(e, 700, now)
	if snap := s.ProxyUplinkSnapshot(); len(snap) != 1 {
		t.Fatalf("两个样本后应出现在快照里，实际 %v", snap)
	}
}

func TestUplinkBandThresholds(t *testing.T) {
	cases := []struct {
		kbps   float64
		sample int
		want   int
	}{
		{1000, 2, 3},
		{600, 2, 3},
		{599, 2, 2},
		{150, 2, 2},
		{149, 2, 1},
		{1, 2, 1},
		{1000, 1, 0}, // 样本不足，不可信
		{0, 2, 0},    // 无有效速率
	}
	for _, c := range cases {
		if got := UplinkBand(c.kbps, c.sample); got != c.want {
			t.Fatalf("UplinkBand(%.0f, %d) = %d，期望 %d", c.kbps, c.sample, got, c.want)
		}
	}
}
