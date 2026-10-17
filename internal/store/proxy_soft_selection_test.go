package store

import (
	"testing"
	"time"

	"zcode2api/internal/config"
)

func quarantineProxy(t *testing.T, s *Store, p ProxyProfile, now time.Time) string {
	t.Helper()
	e := ProxyEgress{ID: p.ID, Mode: "proxy", Generation: p.Generation, identity: strictProxyIdentity(p.URL)}
	for i := 0; i < 3; i++ {
		s.RecordProxySoftOutcome(e, "status_408", 408, false, now.Add(time.Duration(i)*time.Second))
	}
	if q := s.ProxyQuarantine(now.Add(3 * time.Second)); q[e.identity].IsZero() {
		t.Fatalf("隔离未生效：%v", q)
	}
	return e.identity
}

func TestChooseProxySkipsQuarantined(t *testing.T) {
	s := newTestStore(t)
	bad := addSelectionProxy(t, s, "隔离线路", 18200, true)
	good := addSelectionProxy(t, s, "正常线路", 18201, true)
	identity := quarantineProxy(t, s, bad, time.Now())
	if blocked := s.ProxyQuarantine(time.Now()); blocked[identity].IsZero() {
		t.Fatal("隔离状态未写入")
	}
	got, ok := s.chooseProxyLocked(s.ListProxyProfiles(), s.proxyOccupancyLocked(), "", "")
	if !ok || got.ID != good.ID {
		t.Fatalf("自动选线应跳过隔离线路，实际 %+v ok=%v", got, ok)
	}
}

func TestChooseProxyRelaxesWhenAllQuarantined(t *testing.T) {
	s := newTestStore(t)
	only := addSelectionProxy(t, s, "唯一线路", 18202, true)
	quarantineProxy(t, s, only, time.Now())
	got, ok := s.chooseProxyLocked(s.ListProxyProfiles(), s.proxyOccupancyLocked(), "", "")
	if !ok || got.ID != only.ID {
		t.Fatalf("全池被隔离时应放宽而不是返回 false，实际 %+v ok=%v", got, ok)
	}
}

func TestQuarantineReassignsBoundAccount(t *testing.T) {
	s := newTestStore(t)
	bad := addSelectionProxy(t, s, "坏线路", 18203, true)
	good := addSelectionProxy(t, s, "好线路", 18204, true)
	a := addSelectionAccount(t, s, "被改派账号", bad.ID)
	e := softEgress(t, s, a)
	now := time.Now()
	for i := 0; i < 3; i++ {
		s.RecordProxySoftOutcome(e, "status_408", 408, false, now.Add(time.Duration(i)*time.Second))
	}
	live := s.FindAny(a.ID)
	if live.ProxyID == nil || *live.ProxyID != good.ID {
		t.Fatalf("隔离后应把绑定账号改派到可用线路，实际 %v", derefStr(live.ProxyID))
	}
	if len(s.ListProxyProfiles()) != 2 {
		t.Fatal("隔离不得删除任何线路")
	}
}

func TestFreshProxyGateBlocksUntilResultSaved(t *testing.T) {
	s := newTestStore(t)
	fresh := addSelectionProxy(t, s, "新导入", 18205, true)
	other := addSelectionProxy(t, s, "既有", 18206, true)
	now := time.Now()
	p := s.ModelProbeSettings()

	s.mu.Lock()
	s.queueFreshProxyChecksLocked([]ProxyProfile{fresh}, now)
	blocked := s.proxySelectionBlockedLocked(now)
	s.mu.Unlock()
	if !blocked[strictProxyIdentity(fresh.URL)] {
		t.Fatal("新导入线路在首检完成前必须被门禁挡住")
	}
	if blocked[strictProxyIdentity(other.URL)] {
		t.Fatal("既有线路不应被冷启动门禁影响")
	}
	if got := s.TakeProxyNetworkRechecks(p, now); len(got) != 1 || got[0].ID != fresh.ID {
		t.Fatalf("新导入线路应能在没有绑定账号时被巡检取走：%+v", got)
	}
	s.mu.Lock()
	blocked = s.proxySelectionBlockedLocked(now.Add(time.Second))
	s.mu.Unlock()
	if !blocked[strictProxyIdentity(fresh.URL)] {
		t.Fatal("取走后结果未落库期间必须继续挡住（不能留空窗）")
	}
	if err := s.SaveNetworkHealth(p, []NetworkHealthRecord{{ProfileID: fresh.ID, Generation: fresh.Generation, PolicyRevision: p.Revision, State: "healthy", At: now.UnixMilli()}}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	blocked = s.proxySelectionBlockedLocked(now.Add(time.Second))
	s.mu.Unlock()
	if blocked[strictProxyIdentity(fresh.URL)] {
		t.Fatal("结果落库后门禁必须解除")
	}
}

func TestFreshProxyGateExpiresWithoutResult(t *testing.T) {
	s := newTestStore(t)
	fresh := addSelectionProxy(t, s, "超时放行", 18207, true)
	now := time.Now()
	s.mu.Lock()
	s.queueFreshProxyChecksLocked([]ProxyProfile{fresh}, now)
	s.mu.Unlock()

	old := config.ProxyFreshGateSeconds
	config.ProxyFreshGateSeconds = 120
	t.Cleanup(func() { config.ProxyFreshGateSeconds = old })

	s.mu.Lock()
	blocked := s.proxySelectionBlockedLocked(now.Add(121 * time.Second))
	s.mu.Unlock()
	if blocked[strictProxyIdentity(fresh.URL)] {
		t.Fatal("超过门禁有效期必须放行，否则巡检忙时会把池子挡死")
	}
}
