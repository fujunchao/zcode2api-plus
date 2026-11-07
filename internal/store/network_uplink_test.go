package store

import (
	"testing"
	"time"

	"zcode2api/internal/model"
	"zcode2api/internal/networkprobe"
)

func tunnelRecord(profileID string, gen uint64, steady, overall int64) NetworkHealthRecord {
	return NetworkHealthRecord{
		ProfileID:  profileID,
		Generation: gen,
		State:      "healthy",
		First: networkprobe.Report{
			OK: true,
			Tunnel: &networkprobe.TunnelReport{
				OK:         true,
				SteadyKBps: steady,
				Kbps:       overall,
			},
		},
	}
}

// TestSyncUplinkFromNetworkRecords 验证巡检的隧道测速会写进选路带宽状态。
//
// 这条链路的意义：订阅每 15 分钟导入一批新线，若等真实请求采样，用户会先踩一次
// 慢线；把首次巡检的 steady_kbps 作为初值，新线在分配前就有画像。
func TestSyncUplinkFromNetworkRecords(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "待测线路", 18371, true)
	now := time.Now()

	s.mu.Lock()
	s.syncUplinkFromRecordsLocked([]NetworkHealthRecord{tunnelRecord(p.ID, p.Generation, 900, 5000)}, now)
	s.mu.Unlock()

	identity := strictProxyIdentity(p.URL)
	ent, ok := s.proxyUplink.Entries[identity]
	if !ok {
		t.Fatalf("巡检结果应写入带宽状态，实际未建档（identity=%s）", identity)
	}
	if ent.KBps != 900 {
		t.Fatalf("应采用稳态速率 900 而非全程平均 5000，实际 %v", ent.KBps)
	}
	if ent.ProfileID != p.ID {
		t.Fatalf("应记录 ProfileID，实际 %q", ent.ProfileID)
	}
}

// TestSyncUplinkIgnoresMissingSteady 验证没有稳态值时不采纳。
// 此时 Kbps 是全程平均（被内核缓冲抬高），用它做选路会把坏线判成快线。
func TestSyncUplinkIgnoresMissingSteady(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "无稳态线路", 18372, true)
	now := time.Now()

	s.mu.Lock()
	s.syncUplinkFromRecordsLocked([]NetworkHealthRecord{tunnelRecord(p.ID, p.Generation, 0, 8000)}, now)
	s.mu.Unlock()

	if len(s.proxyUplink.Entries) != 0 {
		t.Fatalf("没有 steady_kbps 时不应建档，实际 %d 条", len(s.proxyUplink.Entries))
	}
}

func TestSyncUplinkSkipsUnknownProfile(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	s.mu.Lock()
	s.syncUplinkFromRecordsLocked([]NetworkHealthRecord{tunnelRecord("proxy-not-exist", 1, 900, 900)}, now)
	s.mu.Unlock()
	if len(s.proxyUplink.Entries) != 0 {
		t.Fatalf("未知线路不应建档，实际 %d 条", len(s.proxyUplink.Entries))
	}
}

// TestSyncUplinkFeedsSelection 端到端：巡检写入的带宽要真的影响选路。
func TestSyncUplinkFeedsSelection(t *testing.T) {
	s := newTestStore(t)
	fastLine := addSelectionProxy(t, s, "快线", 18373, true)
	slowLine := addSelectionProxy(t, s, "慢线", 18374, true)
	fastAcc := addSelectionAccount(t, s, "快号", fastLine.ID)
	slowAcc := addSelectionAccount(t, s, "慢号", slowLine.ID)
	now := time.Now()

	s.mu.Lock()
	s.syncUplinkFromRecordsLocked([]NetworkHealthRecord{
		tunnelRecord(fastLine.ID, fastLine.Generation, 900, 900),
		tunnelRecord(slowLine.ID, slowLine.Generation, 900, 900),
	}, now)
	s.mu.Unlock()
	// 两条线先都建成快档，再单独把慢线压下去，确认选路确实按巡检值收窄。
	s.mu.Lock()
	slowIdentity := strictProxyIdentity(slowLine.URL)
	ent := s.proxyUplink.Entries[slowIdentity]
	ent.KBps = 20
	ent.Sample = 5
	s.proxyUplink.Entries[slowIdentity] = ent
	fastIdentity := strictProxyIdentity(fastLine.URL)
	fe := s.proxyUplink.Entries[fastIdentity]
	fe.KBps = 900
	fe.Sample = 5
	s.proxyUplink.Entries[fastIdentity] = fe
	s.mu.Unlock()

	for i := 0; i < 4; i++ {
		got := s.Select(model.ProviderZai, nil, "")
		if got == nil || got.ID != fastAcc.ID {
			t.Fatalf("应选中巡检判定为快的线路，实际 %+v（慢号=%s）", got, slowAcc.ID)
		}
	}
}
