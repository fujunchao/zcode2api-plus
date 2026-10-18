package store

import (
	"path/filepath"
	"testing"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

func softEgress(t *testing.T, s *Store, a *model.Account) ProxyEgress {
	t.Helper()
	e := s.ProxyEgress(a)
	if e.Mode != "proxy" || e.identity == "" {
		t.Fatalf("测试前置失败：账号未绑定代理 egress=%+v", e)
	}
	return e
}

// 连续 2 次软失败即隔离：与窗口内尝试数无关，稀疏流量下也能生效。
func TestProxySoftQuarantineOnConsecutiveFailures(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "软失败线路", 18100, true)
	a := addSelectionAccount(t, s, "软失败账号", p.ID)
	e := softEgress(t, s, a)
	now := time.Now()
	if d := s.RecordProxySoftOutcome(e, "status_408", 408, false, now); d.Action != "" {
		t.Fatalf("首次失败不应隔离：%+v", d)
	}
	d := s.RecordProxySoftOutcome(e, "status_408", 408, false, now.Add(10*time.Second))
	if d.Action != "quarantine" || d.Strikes != 1 {
		t.Fatalf("连续 2 次失败应触发隔离：%+v", d)
	}
	if got := d.Until.Sub(now.Add(10 * time.Second)); got != 30*time.Minute {
		t.Fatalf("首档隔离时长应为 30 分钟，实际 %v", got)
	}
	if q := s.ProxyQuarantine(now.Add(10 * time.Second)); len(q) != 1 {
		t.Fatalf("隔离应立即生效：%v", q)
	}
	if q := s.ProxyQuarantine(now.Add(40 * time.Minute)); len(q) != 0 {
		t.Fatalf("隔离到期后应自动回池：%v", q)
	}
}

// 成功一次即打断连续计数：抖一下的线路不能被连续判据冤枉。
func TestProxySoftConsecutiveResetOnSuccess(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "抖动线路", 18106, true)
	a := addSelectionAccount(t, s, "抖动账号", p.ID)
	e := softEgress(t, s, a)
	now := time.Now()
	s.RecordProxySoftOutcome(e, "status_408", 408, false, now)
	s.RecordProxySoftOutcome(e, "", 200, true, now.Add(time.Second))
	if d := s.RecordProxySoftOutcome(e, "status_408", 408, false, now.Add(2*time.Second)); d.Action != "" {
		t.Fatalf("中间有成功就不算连续：%+v", d)
	}
}

// 两次失败隔得太久（超过连续计数有效期）→ 重新计数，不隔离。
func TestProxySoftConsecutiveWindowExpires(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "久隔线路", 18107, true)
	a := addSelectionAccount(t, s, "久隔账号", p.ID)
	e := softEgress(t, s, a)
	now := time.Now()
	s.RecordProxySoftOutcome(e, "status_408", 408, false, now)
	if d := s.RecordProxySoftOutcome(e, "status_408", 408, false, now.Add(3*time.Hour)); d.Action != "" {
		t.Fatalf("跨半天的两次孤立失败不应算连续：%+v", d)
	}
}

// 关闭连续判据后，失败率兜底仍然生效。
func TestProxySoftRateFallbackWhenConsecutiveDisabled(t *testing.T) {
	old := config.ProxySoftConsecutive
	config.ProxySoftConsecutive = 0
	t.Cleanup(func() { config.ProxySoftConsecutive = old })

	s := newTestStore(t)
	p := addSelectionProxy(t, s, "兜底线路", 18108, true)
	a := addSelectionAccount(t, s, "兜底账号", p.ID)
	e := softEgress(t, s, a)
	now := time.Now()
	s.RecordProxySoftOutcome(e, "status_408", 408, false, now)
	s.RecordProxySoftOutcome(e, "", 200, true, now.Add(time.Second))
	s.RecordProxySoftOutcome(e, "status_408", 408, false, now.Add(2*time.Second))
	if d := s.RecordProxySoftOutcome(e, "status_408", 408, false, now.Add(3*time.Second)); d.Action != "quarantine" {
		t.Fatalf("3 次失败/4 次尝试应由失败率兜底触发：%+v", d)
	}
}

func TestProxySoftQuarantineSkipsLowRate(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "低失败率", 18101, true)
	a := addSelectionAccount(t, s, "低失败率账号", p.ID)
	e := softEgress(t, s, a)
	now := time.Now()
	s.RecordProxySoftOutcome(e, "", 200, true, now)
	s.RecordProxySoftOutcome(e, "", 200, true, now.Add(time.Second))
	if d := s.RecordProxySoftOutcome(e, "status_408", 408, false, now.Add(2*time.Second)); d.Action != "" {
		t.Fatalf("失败率 33%% 不应隔离：%+v", d)
	}
}

func TestProxySoftQuarantineExtendsAndCaps(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "持续失败", 18102, true)
	a := addSelectionAccount(t, s, "持续失败账号", p.ID)
	e := softEgress(t, s, a)
	now := time.Now()
	s.RecordProxySoftOutcome(e, "status_408", 408, false, now)
	if d := s.RecordProxySoftOutcome(e, "status_408", 408, false, now.Add(time.Second)); d.Action != "quarantine" {
		t.Fatalf("连续 2 次失败应触发首档隔离：%+v", d)
	}
	want := []time.Duration{60 * time.Minute, 120 * time.Minute, 240 * time.Minute, 240 * time.Minute}
	at := now.Add(time.Second)
	for i, expect := range want {
		at = at.Add(5 * time.Minute)
		d := s.RecordProxySoftOutcome(e, "status_408", 408, false, at)
		if d.Action != "extended" {
			t.Fatalf("第 %d 次续期未触发：%+v", i+1, d)
		}
		if got := d.Until.Sub(at); got != expect {
			t.Fatalf("第 %d 次续期应为 %v，实际 %v", i+1, expect, got)
		}
	}
}

// 隔离到期不等于恢复：到期后第一次失败必须直接升档，而不是从 1 重新数。
// 2026-10-18 日志：f7eff3 线 17:58 隔离、18:28 到期回池、18:48 又断一次。
func TestProxySoftProbationEscalatesOnFirstFailure(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "观察期升档", 18103, true)
	a := addSelectionAccount(t, s, "观察期升档账号", p.ID)
	e := softEgress(t, s, a)
	now := time.Now()

	s.RecordProxySoftOutcome(e, "status_408", 408, false, now)
	first := s.RecordProxySoftOutcome(e, "status_408", 408, false, now.Add(time.Second))
	if first.Action != "quarantine" || first.Strikes != 1 {
		t.Fatalf("前置失败：应首档隔离：%+v", first)
	}
	// 到期后的第一次失败：观察期命中，直接第二档（60 分钟）。
	at := first.Until.Add(time.Second)
	d := s.RecordProxySoftOutcome(e, "status_408", 408, false, at)
	if d.Action != "quarantine" || d.Trigger != "probation" || d.Strikes != 2 {
		t.Fatalf("到期后首败应按观察期升档：%+v", d)
	}
	if got := d.Until.Sub(at); got != 60*time.Minute {
		t.Fatalf("第二档应为 60 分钟，实际 %v", got)
	}
}

// 观察期唯一的正常出口：一次完整成功即清空档位。
func TestProxySoftProbationSuccessClearsStrikes(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "观察期成功", 18104, true)
	a := addSelectionAccount(t, s, "观察期成功账号", p.ID)
	e := softEgress(t, s, a)
	now := time.Now()

	s.RecordProxySoftOutcome(e, "status_408", 408, false, now)
	first := s.RecordProxySoftOutcome(e, "status_408", 408, false, now.Add(time.Second))
	s.RecordProxySoftOutcome(e, "", 200, true, first.Until.Add(time.Second))

	got := s.ProxySoftFailures()
	if len(got) != 1 || got[0].Strikes != 0 || got[0].Probation {
		t.Fatalf("观察期内成功应清空档位与观察期：%+v", got)
	}
	// 清零后恢复普通判据：单次失败不再立即隔离。
	if d := s.RecordProxySoftOutcome(e, "status_408", 408, false, first.Until.Add(2*time.Second)); d.Action != "" {
		t.Fatalf("已净身的线路单次失败不应隔离：%+v", d)
	}
}

// 关掉观察期开关 → 回到旧行为：到期回池，静默 30 分钟后档位清零。
func TestProxySoftProbationSwitchOffRestoresQuietReset(t *testing.T) {
	old := config.ProxySoftProbation
	config.ProxySoftProbation = false
	t.Cleanup(func() { config.ProxySoftProbation = old })

	s := newTestStore(t)
	p := addSelectionProxy(t, s, "关闭观察期", 18105, true)
	a := addSelectionAccount(t, s, "关闭观察期账号", p.ID)
	e := softEgress(t, s, a)
	now := time.Now()

	s.RecordProxySoftOutcome(e, "status_408", 408, false, now)
	s.RecordProxySoftOutcome(e, "status_408", 408, false, now.Add(time.Second))
	s.RecordProxySoftOutcome(e, "status_408", 408, false, now.Add(2*time.Second))
	later := now.Add(2*time.Second + 31*time.Minute + 30*time.Minute)
	s.RecordProxySoftOutcome(e, "status_408", 408, false, later)
	got := s.ProxySoftFailures()
	if len(got) != 1 || got[0].Strikes != 0 {
		t.Fatalf("开关关闭时静默后应归零：%+v", got)
	}
}

// 隔离/观察状态 24 小时无流量会被 TTL 清理（规格里明确的隐式出口，不是洗白漏洞）。
// 注意观察期是「到期后下一次判定」的瞬时状态：正常情况在同一次调用内就解析
// （成功清零、失败升档），能长期停留的只有「到期后一直没有流量」的条目。
func TestProxySoftProbationEntryPrunedAfterTTL(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "观察期TTL", 18106, true)
	a := addSelectionAccount(t, s, "观察期TTL账号", p.ID)
	e := softEgress(t, s, a)
	now := time.Now()

	s.RecordProxySoftOutcome(e, "status_408", 408, false, now)
	first := s.RecordProxySoftOutcome(e, "status_408", 408, false, now.Add(time.Second))
	// 到期后不再有流量：条目停在观察期。
	s.ProxySoftFailures()
	later := first.Until.Add(25 * time.Hour)
	s.mu.Lock()
	s.pruneProxySoftLocked(later)
	s.mu.Unlock()
	if got := s.ProxySoftFailures(); len(got) != 0 {
		t.Fatalf("24h 无流量的观察期条目应被清理：%+v", got)
	}
}

func TestProxySoftSkipsNonProxyEgress(t *testing.T) {
	s := newTestStore(t)
	a := addSelectionAccount(t, s, "直连账号", "")
	e := s.ProxyEgress(a)
	if e.Mode == "proxy" {
		t.Fatalf("测试前置失败：账号意外绑定了代理")
	}
	now := time.Now()
	for i := 0; i < 5; i++ {
		if d := s.RecordProxySoftOutcome(e, "status_408", 408, false, now.Add(time.Duration(i)*time.Second)); d.Action != "" {
			t.Fatalf("直连不应进入软失败统计：%+v", d)
		}
	}
	if len(s.ProxySoftFailures()) != 0 {
		t.Fatal("直连不应产生软失败记录")
	}
}

func TestProxySoftQuarantineSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.db")
	s := openAt(t, path)
	p := addSelectionProxy(t, s, "持久化线路", 18104, true)
	a := addSelectionAccount(t, s, "持久化账号", p.ID)
	e := softEgress(t, s, a)
	now := time.Now()
	s.RecordProxySoftOutcome(e, "status_408", 408, false, now)
	if d := s.RecordProxySoftOutcome(e, "status_408", 408, false, now.Add(time.Second)); d.Action != "quarantine" {
		t.Fatalf("应触发隔离：%+v", d)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	reopened := openAt(t, path)
	if q := reopened.ProxyQuarantine(time.Now()); len(q) != 1 {
		t.Fatalf("隔离状态应跨重启保留：%v", q)
	}
}

func TestProxySoftIgnoresDisabledSwitch(t *testing.T) {
	old := config.ProxySoftQuarantine
	config.ProxySoftQuarantine = false
	t.Cleanup(func() { config.ProxySoftQuarantine = old })

	s := newTestStore(t)
	p := addSelectionProxy(t, s, "关闭开关", 18105, true)
	a := addSelectionAccount(t, s, "关闭开关账号", p.ID)
	e := softEgress(t, s, a)
	now := time.Now()
	for i := 0; i < 5; i++ {
		if d := s.RecordProxySoftOutcome(e, "status_408", 408, false, now.Add(time.Duration(i)*time.Second)); d.Action != "" {
			t.Fatalf("总开关关闭时不应隔离：%+v", d)
		}
	}
	// 关闭只影响动作，不影响观测：后台仍应看到这条线路的可用率。
	if got := s.ProxySoftFailures(); len(got) != 1 || got[0].Failures != 5 {
		t.Fatalf("关闭隔离后仍应记账：%+v", got)
	}
	if q := s.ProxyQuarantine(now); len(q) != 0 {
		t.Fatalf("关闭隔离后不得产生隔离状态：%v", q)
	}
}

// 到期后静默数小时再来失败，判据与立即失败一致（观察期是惰性判定，不靠计时器）。
func TestProxySoftProbationAfterLongSilence(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "观察期长静默", 18107, true)
	a := addSelectionAccount(t, s, "观察期长静默账号", p.ID)
	e := softEgress(t, s, a)
	now := time.Now()

	s.RecordProxySoftOutcome(e, "status_408", 408, false, now)
	first := s.RecordProxySoftOutcome(e, "status_408", 408, false, now.Add(time.Second))
	at := first.Until.Add(6 * time.Hour)
	d := s.RecordProxySoftOutcome(e, "status_408", 408, false, at)
	if d.Action != "quarantine" || d.Trigger != "probation" || d.Strikes != 2 {
		t.Fatalf("静默 6 小时后首败仍应按观察期升档：%+v", d)
	}
	if got := d.Until.Sub(at); got != 60*time.Minute {
		t.Fatalf("第二档应为 60 分钟，实际 %v", got)
	}
}
