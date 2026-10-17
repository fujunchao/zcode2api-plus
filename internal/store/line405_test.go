package store

import (
	"path/filepath"
	"testing"
	"time"

	"zcode2api/internal/model"
)

func line405Egress(t *testing.T, s *Store, a *model.Account) ProxyEgress {
	t.Helper()
	e := s.ProxyEgress(a)
	if e.Mode != "proxy" || e.identity == "" {
		t.Fatalf("测试前置失败：账号未绑定代理 egress=%+v", e)
	}
	return e
}

func TestLine405QuarantineAfterTwoConsecutive(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "405 线路", 18200, true)
	a := addSelectionAccount(t, s, "405 账号", p.ID)
	e := line405Egress(t, s, a)
	now := time.Now()

	if d := s.RecordLine405(e, now); d.Action != "recorded" {
		t.Fatalf("首次 405 只应记账，实际 %+v", d)
	}
	if s.Line405Blocked(e, now) {
		t.Fatal("首次 405 不应隔离")
	}
	d := s.RecordLine405(e, now.Add(time.Second))
	if d.Action != "quarantine" || d.Strikes != 1 {
		t.Fatalf("连续 2 次 405 应隔离，实际 %+v", d)
	}
	if got := d.Until.Sub(now.Add(time.Second)); got != 30*time.Minute {
		t.Fatalf("首档隔离应为 30 分钟，实际 %v", got)
	}
	if !s.Line405Blocked(e, now.Add(time.Second)) {
		t.Fatal("隔离期内应被判脏")
	}
	if q := s.Line405Quarantine(now.Add(time.Second)); len(q) != 1 {
		t.Fatalf("隔离集合应有 1 条，实际 %d", len(q))
	}
}

func TestLine405SuccessResetsStreak(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "抖动线路", 18201, true)
	a := addSelectionAccount(t, s, "抖动账号", p.ID)
	e := line405Egress(t, s, a)
	now := time.Now()

	if d := s.RecordLine405(e, now); d.Action != "recorded" {
		t.Fatalf("首次应记账：%+v", d)
	}
	s.RecordLineOK(e, now.Add(time.Second))
	if d := s.RecordLine405(e, now.Add(2*time.Second)); d.Action != "recorded" {
		t.Fatalf("中间成功过就不该按连续计数隔离，实际 %+v", d)
	}
	if s.Line405Blocked(e, now.Add(2*time.Second)) {
		t.Fatal("未达连续阈值不应隔离")
	}
}

func TestLine405EscalatesAfterExpiry(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "顽固线路", 18202, true)
	a := addSelectionAccount(t, s, "顽固账号", p.ID)
	e := line405Egress(t, s, a)
	now := time.Now()

	s.RecordLine405(e, now)
	first := s.RecordLine405(e, now.Add(time.Second))
	if first.Strikes != 1 {
		t.Fatalf("首档 strikes 应为 1：%+v", first)
	}
	later := now.Add(31 * time.Minute)
	d := s.RecordLine405(e, later)
	if d.Action != "quarantine" || d.Strikes != 2 {
		t.Fatalf("隔离到期后再 405 应立刻二次隔离并升档：%+v", d)
	}
	if got := d.Until.Sub(later); got != 60*time.Minute {
		t.Fatalf("第二档应为 60 分钟，实际 %v", got)
	}
}

func TestLine405SuccessLiftsQuarantine(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "可恢复线路", 18203, true)
	a := addSelectionAccount(t, s, "可恢复账号", p.ID)
	e := line405Egress(t, s, a)
	now := time.Now()

	s.RecordLine405(e, now)
	s.RecordLine405(e, now.Add(time.Second))
	if !s.Line405Blocked(e, now.Add(time.Second)) {
		t.Fatal("前置失败：应先被隔离")
	}
	s.RecordLineOK(e, now.Add(2*time.Second))
	if s.Line405Blocked(e, now.Add(2*time.Second)) {
		t.Fatal("成功一次应立即解封")
	}
	list := s.Line405Failures()
	if len(list) != 1 {
		t.Fatalf("应只有 1 条线路画像，实际 %d", len(list))
	}
	if ent := list[0]; ent.Consecutive405 != 0 || ent.Strikes != 0 || ent.OK != 1 {
		t.Fatalf("成功应清零连续计数与档位：%+v", ent)
	}
}

func TestLine405GenerationReset(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "改配置线路", 18204, true)
	a := addSelectionAccount(t, s, "改配置账号", p.ID)
	e := line405Egress(t, s, a)
	now := time.Now()

	s.RecordLine405(e, now)
	s.RecordLine405(e, now.Add(time.Second))
	edited := e
	edited.Generation++
	if d := s.RecordLine405(edited, now.Add(2*time.Second)); d.Action != "recorded" {
		t.Fatalf("线路配置变更后旧账应作废，实际 %+v", d)
	}
}

func TestLine405DirectIgnored(t *testing.T) {
	s := newTestStore(t)
	direct := ProxyEgress{Mode: "direct"}
	if d := s.RecordLine405(direct, time.Now()); d.Action != "ignored" {
		t.Fatalf("直连不应建档，实际 %+v", d)
	}
	if got := s.Line405Failures(); len(got) != 0 {
		t.Fatalf("直连不应产生条目，实际 %d", len(got))
	}
}

func TestLine405StatePersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.db")
	s := openAt(t, path)
	p := addSelectionProxy(t, s, "持久化线路", 18205, true)
	a := addSelectionAccount(t, s, "持久化账号", p.ID)
	e := line405Egress(t, s, a)
	now := time.Now()
	s.RecordLine405(e, now)
	s.RecordLine405(e, now.Add(time.Second))
	if err := s.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}

	reopened := openAt(t, path)
	if !reopened.Line405Blocked(e, now.Add(2*time.Second)) {
		t.Fatal("隔离状态应随 meta 持久化，重开后仍然生效")
	}
}

func TestLine405SelectionSkipsQuarantinedLine(t *testing.T) {
	s := newTestStore(t)
	dirty := addSelectionProxy(t, s, "脏线", 18206, true)
	clean := addSelectionProxy(t, s, "干净线", 18207, true)
	dirtyAccount := addSelectionAccount(t, s, "脏线账号", dirty.ID)
	cleanAccount := addSelectionAccount(t, s, "干净线账号", clean.ID)
	e := line405Egress(t, s, dirtyAccount)
	now := time.Now()
	s.RecordLine405(e, now)
	s.RecordLine405(e, now.Add(time.Second))

	got := s.Select(model.ProviderZai, nil, "")
	if got == nil || got.ID != cleanAccount.ID {
		t.Fatalf("应跳过隔离线路上的账号，实际 %+v", got)
	}
}

func TestLine405SelectionFallsBackWhenAllBlocked(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "唯一线路", 18208, true)
	a := addSelectionAccount(t, s, "唯一账号", p.ID)
	e := line405Egress(t, s, a)
	now := time.Now()
	s.RecordLine405(e, now)
	s.RecordLine405(e, now.Add(time.Second))

	got := s.Select(model.ProviderZai, nil, "")
	if got == nil || got.ID != a.ID {
		t.Fatalf("全池隔离时必须回落，不能返回无可用账号，实际 %+v", got)
	}
}

func TestLine405QuarantineExpires(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "到期线路", 18209, true)
	a := addSelectionAccount(t, s, "到期账号", p.ID)
	e := line405Egress(t, s, a)
	now := time.Now()
	s.RecordLine405(e, now)
	d := s.RecordLine405(e, now.Add(time.Second))
	if !s.Line405Blocked(e, now.Add(time.Second)) {
		t.Fatal("前置失败：应先被隔离")
	}
	if s.Line405Blocked(e, d.Until.Add(time.Second)) {
		t.Fatal("隔离到期后应自动解除")
	}
}
