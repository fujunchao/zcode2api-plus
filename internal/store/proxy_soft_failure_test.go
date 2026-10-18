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

func TestProxySoftStrikesResetAfterQuiet(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "静默归零", 18103, true)
	a := addSelectionAccount(t, s, "静默归零账号", p.ID)
	e := softEgress(t, s, a)
	now := time.Now()
	s.RecordProxySoftOutcome(e, "status_408", 408, false, now)
	s.RecordProxySoftOutcome(e, "status_408", 408, false, now.Add(time.Second))
	s.RecordProxySoftOutcome(e, "status_408", 408, false, now.Add(2*time.Second))
	// 隔离结束后静默 30 分钟以上，连续次数归零。
	later := now.Add(2*time.Second + 31*time.Minute + 30*time.Minute)
	s.RecordProxySoftOutcome(e, "status_408", 408, false, later)
	got := s.ProxySoftFailures()
	if len(got) != 1 || got[0].Strikes != 0 {
		t.Fatalf("静默后连续次数应归零：%+v", got)
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
