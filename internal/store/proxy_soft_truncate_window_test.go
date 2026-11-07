package store

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"zcode2api/internal/config"
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
			// 连续计数复位必须一起持久化，不能重启后把两个孤立失败误当连续失败。
			s.RecordProxySoftOutcome(e, "", 200, true, now.Add(time.Minute))
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s = openAt(t, path)
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

// 非默认阈值也必须是滚动窗口，不能只比较最近两次时间。
func TestProxySoftTruncateRollingWindowThreeSamples(t *testing.T) {
	oldStrikes, oldConsecutive, oldMinFailures := config.ProxySoftTruncateStrikes, config.ProxySoftConsecutive, config.ProxySoftMinFailures
	config.ProxySoftTruncateStrikes, config.ProxySoftConsecutive, config.ProxySoftMinFailures = 3, 0, 100
	t.Cleanup(func() {
		config.ProxySoftTruncateStrikes, config.ProxySoftConsecutive, config.ProxySoftMinFailures = oldStrikes, oldConsecutive, oldMinFailures
	})
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "三样本线路", 18503, true)
	a := addSelectionAccount(t, s, "三样本账号", p.ID)
	e := softEgress(t, s, a)
	now := time.Now()
	for _, minute := range []int{0, 50, 61} {
		if d := s.RecordProxySoftOutcome(e, ProxySignalUpstreamTruncate, 0, false, now.Add(time.Duration(minute)*time.Minute)); d.Action != "" {
			t.Fatalf("第 %d 分钟有效断流不足三次，不应隔离: %+v", minute, d)
		}
	}
	d := s.RecordProxySoftOutcome(e, ProxySignalUpstreamTruncate, 0, false, now.Add(62*time.Minute))
	if d.Action != "quarantine" || d.Trigger != "truncate_strikes" || s.ProxySoftFailures()[0].Truncates != 3 {
		t.Fatalf("第 50、61、62 分钟应累计为三次: decision=%+v state=%+v", d, s.ProxySoftFailures())
	}
}

func TestProxySoftTruncateLegacyStateMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.db")
	s := openAt(t, path)
	p := addSelectionProxy(t, s, "旧版记录线路", 18504, true)
	a := addSelectionAccount(t, s, "旧版记录账号", p.ID)
	e := softEgress(t, s, a)
	now := time.Now()
	legacy := proxySoftState{Entries: map[string]ProxySoftFailure{
		e.identity: {Identity: e.identity, ProfileID: e.ID, Generation: e.Generation,
			LastAt: now.UnixMilli(), Truncates: 1, TruncateWindowStart: now.UnixMilli()},
	}}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT OR REPLACE INTO meta (key,value) VALUES (?,?)", proxySoftFailureKey, string(raw)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openAt(t, path)
	d := s.RecordProxySoftOutcome(e, ProxySignalUpstreamTruncate, 0, false, now.Add(45*time.Minute))
	if d.Action != "quarantine" || d.Trigger != "truncate_strikes" {
		t.Fatalf("旧版仍有效的首次计数应参与新窗口: %+v", d)
	}
	got := s.ProxySoftFailures()[0]
	if len(got.TruncateTimes) != 2 {
		t.Fatalf("迁移后应保存两次时间戳: %+v", got)
	}
	got.TruncateTimes[0] = 0
	if s.ProxySoftFailures()[0].TruncateTimes[0] == 0 {
		t.Fatal("后台快照不得共享内部时间戳切片")
	}
}

func TestProxySoftTruncatePersistenceRetry(t *testing.T) {
	for _, closeRetry := range []bool{false, true} {
		t.Run(map[bool]string{false: "下一次记录重试", true: "关闭时重试"}[closeRetry], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "accounts.db")
			s := openAt(t, path)
			p := addSelectionProxy(t, s, "保存失败线路", 18505, true)
			a := addSelectionAccount(t, s, "保存失败账号", p.ID)
			e := softEgress(t, s, a)
			if _, err := s.db.Exec("CREATE TRIGGER reject_truncate_save BEFORE INSERT ON meta WHEN NEW.key='proxy_soft_failure_state' BEGIN SELECT RAISE(ABORT, 'synthetic save failure'); END"); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			s.RecordProxySoftOutcome(e, ProxySignalUpstreamTruncate, 0, false, now)
			if !s.proxySoftDirty || len(s.ProxySoftFailures()) != 1 {
				t.Fatal("保存失败时必须保留内存样本及重试标记")
			}
			if _, err := s.db.Exec("DROP TRIGGER reject_truncate_save"); err != nil {
				t.Fatal(err)
			}
			if !closeRetry {
				s.RecordProxySoftOutcome(e, "", 200, true, now.Add(time.Second))
				if s.proxySoftDirty {
					t.Fatal("后续记录应重新保存并清除脏标记")
				}
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s = openAt(t, path)
			if got := s.ProxySoftFailures(); len(got) != 1 || got[0].Truncates != 1 {
				t.Fatalf("重试写入后首次断流必须跨重启保留: %+v", got)
			}
		})
	}
}
