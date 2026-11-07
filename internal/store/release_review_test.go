package store

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

func enableReviewNetworkPolicy(t *testing.T, s *Store) ModelProbeSettings {
	t.Helper()
	p := s.ModelProbeSettings()
	p.NetworkEnabled = true
	p, err := s.SaveModelProbeSettings(p, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// 必须经过真实事务入口：只测 sync 辅助函数发现不了单连接被事务占用的问题。
func TestNetworkUplinkSavePersists(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "事务测试线路", 18401, true)
	policy := enableReviewNetworkPolicy(t, s)
	rec := tunnelRecord(p.ID, p.Generation, 900, 5000)
	rec.RoundID, rec.PolicyRevision = "round-persist", policy.Revision
	if err := s.SaveNetworkHealth(policy, []NetworkHealthRecord{rec}); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := s.db.QueryRow("SELECT value FROM meta WHERE key=?", proxyUplinkKey).Scan(&raw); err != nil {
		t.Fatalf("巡检返回成功时，带宽必须已写入数据库: %v", err)
	}
	var persisted proxyUplinkState
	if err := json.Unmarshal([]byte(raw), &persisted); err != nil {
		t.Fatal(err)
	}
	if got := persisted.Entries[strictProxyIdentity(p.URL)]; got.Sample != 1 || got.KBps != 900 {
		t.Fatalf("持久化样本错误: %+v", got)
	}
}

// 对齐实际巡检的三次保存：首次、复核、处置结果；重启也不能重复计算已有测量。
func TestNetworkUplinkSaveDeduplicatesMeasurements(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.db")
	s := openAt(t, path)
	p := addSelectionProxy(t, s, "复核测试线路", 18402, true)
	policy := enableReviewNetworkPolicy(t, s)
	rec := tunnelRecord(p.ID, p.Generation, 30, 5000)
	rec.RoundID, rec.PolicyRevision = "round-1", policy.Revision
	save := func(st *Store, wantN int, wantRate float64) {
		t.Helper()
		if err := st.SaveNetworkHealth(policy, []NetworkHealthRecord{rec}); err != nil {
			t.Fatal(err)
		}
		got := st.proxyUplink.Entries[strictProxyIdentity(p.URL)]
		if got.Sample != wantN || got.KBps-wantRate > 0.001 || wantRate-got.KBps > 0.001 {
			t.Errorf("测量应只计一次并采用复核值: got=%+v wantN=%d wantRate=%v", got, wantN, wantRate)
		}
	}
	save(s, 1, 30)
	second := tunnelRecord(p.ID, p.Generation, 900, 900).First
	rec.Second, rec.State = &second, "recovered"
	want := 30*(1-proxyUplinkAlpha) + 900*proxyUplinkAlpha
	save(s, 2, want)
	rec.At = time.Now().UnixMilli()
	save(s, 2, want)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openAt(t, path)
	save(s, 2, want)
	rec.RoundID, rec.Second = "round-2", nil
	rec.First = tunnelRecord(p.ID, p.Generation, 800, 800).First
	save(s, 3, want*(1-proxyUplinkAlpha)+800*proxyUplinkAlpha)
}

func TestNetworkUplinkSaveFailureIsAtomic(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "回滚测试线路", 18403, true)
	policy := enableReviewNetworkPolicy(t, s)
	_, err := s.db.Exec("CREATE TRIGGER reject_review_uplink BEFORE INSERT ON meta WHEN NEW.key='proxy_uplink_state' BEGIN SELECT RAISE(ABORT, 'synthetic uplink failure'); END")
	if err != nil {
		t.Fatal(err)
	}
	rec := tunnelRecord(p.ID, p.Generation, 900, 900)
	rec.RoundID, rec.PolicyRevision = "round-rollback", policy.Revision
	if err := s.SaveNetworkHealth(policy, []NetworkHealthRecord{rec}); err == nil {
		t.Error("带宽写入失败必须向调用方报告，不能只记日志后提交巡检结果")
	}
	records, err := s.NetworkHealth()
	if err != nil || len(records) != 0 || len(s.ProxyUplinks()) != 0 {
		t.Fatalf("失败后数据库与内存均须回滚: records=%+v uplinks=%+v err=%v", records, s.ProxyUplinks(), err)
	}
	if _, err := s.db.Exec("DROP TRIGGER reject_review_uplink"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveNetworkHealth(policy, []NetworkHealthRecord{rec}); err != nil {
		t.Fatal(err)
	}
	if got := s.ProxyUplinks(); len(got) != 1 || got[0].Sample != 1 {
		t.Fatalf("失败重试应只计入一个样本: %+v", got)
	}
}

func TestSelectUplinkUnknownBeforeSlow(t *testing.T) {
	old := config.SelectUplinkBand
	config.SelectUplinkBand = true
	t.Cleanup(func() { config.SelectUplinkBand = old })
	for _, samples := range []int{0, 1} {
		s := newTestStore(t)
		slow := addSelectionAccount(t, s, "慢号", addSelectionProxy(t, s, "慢线", 18404, true).ID)
		unknown := addSelectionAccount(t, s, "未知号", addSelectionProxy(t, s, "未知线", 18405, true).ID)
		recordUplink(t, s, slow, 30, time.Now())
		if samples == 1 {
			s.RecordProxyUplink(uplinkEgress(t, s, unknown), 800, time.Now())
		}
		for i := 0; i < 4; i++ {
			if got := s.Select(model.ProviderZai, nil, ""); got == nil || got.ID != unknown.ID {
				t.Fatalf("未知线路应先于已知慢线获得机会，样本数=%d 实际=%+v", samples, got)
			}
		}
	}
}
