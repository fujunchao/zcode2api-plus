package store

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

// 隔离到期的一次成功只清连续计数，不能抹掉最近 60 分钟内仍然有效的断流。
func TestProxySoftTruncateRollingWindowAfterRecovery(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "滚动窗口线路", 18501, true)
	a := addSelectionAccount(t, s, "滚动窗口账号", p.ID)
	e := softEgress(t, s, a)
	now := time.Now()
	s.RecordProxySoftOutcome(e, ProxySignalUpstreamTruncate, 0, false, now)
	s.RecordProxySoftOutcome(e, "", 200, true, now.Add(time.Minute))
	first := s.RecordProxySoftOutcome(e, ProxySignalUpstreamTruncate, 0, false, now.Add(45*time.Minute))
	if first.Action != "quarantine" || first.Trigger != "truncate_strikes" {
		t.Fatalf("前置失败：第 45 分钟应隔离: %+v", first)
	}
	s.RecordProxySoftOutcome(e, "", 200, true, now.Add(76*time.Minute))
	if state := s.ProxySoftFailures(); len(state) != 1 || state[0].Probation || state[0].Consecutive != 0 {
		t.Fatalf("成功后应退出观察期并清连续计数: %+v", state)
	}
	d := s.RecordProxySoftOutcome(e, ProxySignalUpstreamTruncate, 0, false, now.Add(80*time.Minute))
	if d.Action != "quarantine" || d.Trigger != "truncate_strikes" {
		t.Fatalf("第 45、80 分钟只隔 35 分钟，必须再次触发断流闸门: %+v", d)
	}
	if got := s.ProxySoftFailures()[0].Truncates; got != 2 {
		t.Fatalf("只保留窗口内两次断流，实际 %d", got)
	}
}

// 从真实 SQLite 重开验证：未触发隔离的首个样本也必须已经落盘。
func TestProxySoftTruncateFirstSampleSurvivesRestart(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "窗口内累计", true: "过期不累计"}[expired], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "accounts.db")
			s := openAt(t, path)
			p := addSelectionProxy(t, s, "重启线路", 18502, true)
			a := addSelectionAccount(t, s, "重启账号", p.ID)
			e := softEgress(t, s, a)
			now := time.Now()
			if d := s.RecordProxySoftOutcome(e, ProxySignalUpstreamTruncate, 0, false, now); d.Action != "" {
				t.Fatalf("单次断流不应隔离: %+v", d)
			}
			var raw string
			if err := s.db.QueryRow("SELECT value FROM meta WHERE key=?", proxySoftFailureKey).Scan(&raw); err != nil {
				t.Errorf("首次断流返回前必须落盘: %v", err)
			} else {
				var saved proxySoftState
				if err := json.Unmarshal([]byte(raw), &saved); err != nil || saved.Entries[e.identity].Truncates != 1 {
					t.Errorf("首次样本持久化错误: %s err=%v", raw, err)
				}
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s = openAt(t, path)
			// 避免已有连续失败判据替新判据兜底，必须由断流专用历史触发。
			s.RecordProxySoftOutcome(e, "", 200, true, now.Add(time.Minute))
			delay := 45 * time.Minute
			if expired {
				delay = 61 * time.Minute
			}
			d := s.RecordProxySoftOutcome(e, ProxySignalUpstreamTruncate, 0, false, now.Add(delay))
			if !expired && (d.Action != "quarantine" || d.Trigger != "truncate_strikes") {
				t.Fatalf("重启后必须保留窗口内的首次断流: %+v", d)
			}
			if expired && d.Action != "" {
				t.Fatalf("重启恢复的过期样本不能触发隔离: %+v", d)
			}
		})
	}
}
