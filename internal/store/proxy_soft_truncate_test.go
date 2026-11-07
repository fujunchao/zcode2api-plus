package store

import (
	"testing"
	"time"

	"zcode2api/internal/config"
)

// 断流专用闸门：同一线路 60 分钟内**累计** 2 次断流即隔离，即使中间夹着成功。
//
// 这是 2026-11-07 复盘的主要缺口：断流与 transport_timeout 共用失败率判据（要求 3 次失败），
// 而「夹着成功」的坏线永远凑不满——见 docs/plan-line-truncate-rate-gate-20261107.md。
func TestProxySoftTruncateStrikesQuarantine(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "断流线路", 18150, true)
	a := addSelectionAccount(t, s, "断流账号", p.ID)
	e := softEgress(t, s, a)
	now := time.Now()

	if d := s.RecordProxySoftOutcome(e, ProxySignalUpstreamTruncate, 0, false, now); d.Action != "" {
		t.Fatalf("首次断流不应隔离：%+v", d)
	}
	// 中间一次完整成功：连续计数清零（consecutive=0），但断流计数**不能**清零。
	s.RecordProxySoftOutcome(e, "", 200, true, now.Add(time.Minute))
	d := s.RecordProxySoftOutcome(e, ProxySignalUpstreamTruncate, 0, false, now.Add(45*time.Minute))
	if d.Action != "quarantine" || d.Trigger != "truncate_strikes" {
		t.Fatalf("累计 2 次断流应触发 truncate_strikes 隔离：%+v", d)
	}
	if q := s.ProxyQuarantine(now.Add(45 * time.Minute)); len(q) != 1 {
		t.Fatalf("隔离应立即生效：%v", q)
	}
}

// 传输超时不属于断流判据：两次超时（中间夹成功）不得被算成「断流 strike」。
func TestProxySoftTruncateGateIgnoresTransportTimeout(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "超时线路", 18152, true)
	a := addSelectionAccount(t, s, "超时账号", p.ID)
	e := softEgress(t, s, a)
	now := time.Now()

	s.RecordProxySoftOutcome(e, ProxySignalTransportTimeout, 0, false, now)
	s.RecordProxySoftOutcome(e, "", 200, true, now.Add(time.Minute))
	d := s.RecordProxySoftOutcome(e, ProxySignalTransportTimeout, 0, false, now.Add(20*time.Minute))
	if d.Action != "" {
		t.Fatalf("超时不应触发断流判据（连续=1、失败率不足）：%+v", d)
	}
}

// 开关关闭（阈值 0）时只记账不隔离。
func TestProxySoftTruncateGateDisabled(t *testing.T) {
	old := config.ProxySoftTruncateStrikes
	config.ProxySoftTruncateStrikes = 0
	t.Cleanup(func() { config.ProxySoftTruncateStrikes = old })

	s := newTestStore(t)
	p := addSelectionProxy(t, s, "断流关闭", 18154, true)
	a := addSelectionAccount(t, s, "断流关闭账号", p.ID)
	e := softEgress(t, s, a)
	now := time.Now()

	s.RecordProxySoftOutcome(e, ProxySignalUpstreamTruncate, 0, false, now)
	s.RecordProxySoftOutcome(e, "", 200, true, now.Add(time.Minute))
	if d := s.RecordProxySoftOutcome(e, ProxySignalUpstreamTruncate, 0, false, now.Add(2*time.Minute)); d.Action != "" {
		t.Fatalf("阈值 0 时不应隔离：%+v", d)
	}
	if got := s.proxySoft.Entries[e.identity].Truncates; got != 0 {
		t.Fatalf("阈值 0 时不应累计断流计数，实际 %d", got)
	}
}

// 超出窗口（60 分钟）的两条断流不累计。
func TestProxySoftTruncateGateWindowExpires(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "断流窗口", 18156, true)
	a := addSelectionAccount(t, s, "断流窗口账号", p.ID)
	e := softEgress(t, s, a)
	now := time.Now()

	s.RecordProxySoftOutcome(e, ProxySignalUpstreamTruncate, 0, false, now)
	s.RecordProxySoftOutcome(e, "", 200, true, now.Add(time.Minute))
	if d := s.RecordProxySoftOutcome(e, ProxySignalUpstreamTruncate, 0, false, now.Add(61*time.Minute)); d.Action != "" {
		t.Fatalf("超过 60 分钟窗口不应累计：%+v", d)
	}
}
