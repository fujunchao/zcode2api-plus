package store

import (
	"path/filepath"
	"testing"
	"time"

	"zcode2api/internal/config"
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

func TestProxyEgressLineKey(t *testing.T) {
	proxy := ProxyEgress{Mode: "proxy", ID: "p1", identity: "http://a.example:8080"}
	if got := proxy.LineKey(); got != "proxy:http://a.example:8080" {
		t.Fatalf("代理线路键不符: %q", got)
	}
	for _, mode := range []string{"direct", "direct_fallback", "forced_direct"} {
		if got := (ProxyEgress{Mode: mode}).LineKey(); got != "direct" {
			t.Fatalf("%s 应归并为 direct，实际 %q", mode, got)
		}
	}
	if got := (ProxyEgress{Mode: "invalid_route"}).LineKey(); got != "" {
		t.Fatalf("无效路由不应参与聚合: %q", got)
	}
}

func TestNoteAccount405DistinguishesLines(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	if !s.NoteAccount405("acc-1", "line-a", now) {
		t.Fatal("首次命中应归因线路")
	}
	if !s.NoteAccount405("acc-1", "line-a", now.Add(time.Minute)) {
		t.Fatal("同一条线重复命中仍归因线路")
	}
	if s.NoteAccount405("acc-1", "line-b", now.Add(2*time.Minute)) {
		t.Fatal("换线后仍 405 才应升级为账号级")
	}
	if !s.NoteAccount405("acc-1", "line-c", now.Add(3*time.Hour)) {
		t.Fatal("TTL 过期后应重新视为首次")
	}
	if !s.NoteAccount405("acc-2", "line-a", now) {
		t.Fatal("不同账号互不影响")
	}
}

// enableLine405Probe 打开自动巡检：目标覆盖全池、账号自动择优，队列即可入队。
func enableLine405Probe(t *testing.T, s *Store, now time.Time) {
	t.Helper()
	p := DefaultModelProbeSettings()
	p.Enabled = true
	if _, err := s.SaveModelProbeSettings(p, now); err != nil {
		t.Fatalf("启用巡检失败: %v", err)
	}
}

func TestLine405QuarantineQueuesProbe(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "巡检线路", 18300, true)
	a := addSelectionAccount(t, s, "巡检账号", p.ID)
	e := line405Egress(t, s, a)
	now := time.Now()
	enableLine405Probe(t, s, now)

	s.RecordLine405(e, now)
	if runs := s.ModelProbeHistory(p.ID, 0, 10); len(runs) != 0 {
		t.Fatalf("首次 405 只记账，不该入队巡检: %d", len(runs))
	}
	d := s.RecordLine405(e, now.Add(time.Second))
	if d.Action != "quarantine" {
		t.Fatalf("前置失败：第二次 405 应隔离: %+v", d)
	}
	runs := s.ModelProbeHistory(p.ID, 0, 10)
	if len(runs) != 1 || runs[0].Source != "line405" {
		t.Fatalf("隔离后应入队一次 source=line405 的巡检: %+v", runs)
	}
	list := s.Line405Failures()
	if len(list) != 1 || list[0].ProbeState != "queued" {
		t.Fatalf("画像应记录 probe_state=queued: %+v", list)
	}
}

func TestLine405ProbeCooldownSkipsRepeat(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "限频线路", 18301, true)
	a := addSelectionAccount(t, s, "限频账号", p.ID)
	e := line405Egress(t, s, a)
	now := time.Now()
	enableLine405Probe(t, s, now)

	s.RecordLine405(e, now)
	s.RecordLine405(e, now.Add(time.Second))
	// 隔离期内再次命中（续期升档）：30 分钟内不应重复入队。
	s.RecordLine405(e, now.Add(2*time.Second))
	if runs := s.ModelProbeHistory(p.ID, 0, 10); len(runs) != 1 {
		t.Fatalf("同线路 30 分钟内只应入队一次，实际 %d", len(runs))
	}
}

func TestLine405ProbeDisabledKeepsQuarantine(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "关闭巡检线路", 18302, true)
	a := addSelectionAccount(t, s, "关闭巡检账号", p.ID)
	e := line405Egress(t, s, a)
	now := time.Now()

	s.RecordLine405(e, now)
	s.RecordLine405(e, now.Add(time.Second))
	if !s.Line405Blocked(e, now.Add(time.Second)) {
		t.Fatal("巡检关闭时隔离仍必须生效")
	}
	if runs := s.ModelProbeHistory(p.ID, 0, 10); len(runs) != 0 {
		t.Fatalf("巡检关闭时不应入队: %d", len(runs))
	}
	if list := s.Line405Failures(); len(list) != 1 || list[0].ProbeState != "" {
		t.Fatalf("未入队时不应有 probe_state: %+v", list)
	}
}

func TestLine405ProbeHourlyBudget(t *testing.T) {
	old := config.Line405ProbePerHour
	config.Line405ProbePerHour = 1
	t.Cleanup(func() { config.Line405ProbePerHour = old })

	s := newTestStore(t)
	first := addSelectionProxy(t, s, "预算线路一", 18303, true)
	second := addSelectionProxy(t, s, "预算线路二", 18304, true)
	a1 := addSelectionAccount(t, s, "预算账号一", first.ID)
	a2 := addSelectionAccount(t, s, "预算账号二", second.ID)
	now := time.Now()
	enableLine405Probe(t, s, now)

	s.RecordLine405(line405Egress(t, s, a1), now)
	s.RecordLine405(line405Egress(t, s, a1), now.Add(time.Second))
	s.RecordLine405(line405Egress(t, s, a2), now.Add(2*time.Second))
	s.RecordLine405(line405Egress(t, s, a2), now.Add(3*time.Second))

	if runs := s.ModelProbeHistory(first.ID, 0, 10); len(runs) != 1 {
		t.Fatalf("第一条应入队: %d", len(runs))
	}
	if runs := s.ModelProbeHistory(second.ID, 0, 10); len(runs) != 0 {
		t.Fatalf("超出每小时预算不应入队: %d", len(runs))
	}
	if !s.Line405Blocked(line405Egress(t, s, a2), now.Add(3*time.Second)) {
		t.Fatal("超预算只跳过巡检，隔离必须仍然生效")
	}
}

func TestApplyLine405ProbeVerdict(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "结论回写线路", 18305, true)
	a := addSelectionAccount(t, s, "结论回写账号", p.ID)
	e := line405Egress(t, s, a)
	now := time.Now()

	s.RecordLine405(e, now)
	s.RecordLine405(e, now.Add(time.Second))
	if !s.Line405Blocked(e, now.Add(time.Second)) {
		t.Fatal("前置失败：应先隔离")
	}
	// 未知结论不动状态。
	s.ApplyLine405ProbeVerdict(p.ID, "inconclusive", now.Add(2*time.Second))
	if !s.Line405Blocked(e, now.Add(2*time.Second)) {
		t.Fatal("未知结论不应解封")
	}
	// 失败结论保留隔离。
	s.ApplyLine405ProbeVerdict(p.ID, "route_correlated", now.Add(3*time.Second))
	if !s.Line405Blocked(e, now.Add(3*time.Second)) {
		t.Fatal("失败结论不应解封")
	}
	if list := s.Line405Failures(); list[0].ProbeState != "failed" {
		t.Fatalf("应记录 probe_state=failed: %+v", list[0])
	}
	// 健康结论提前解封并清零档位。
	s.ApplyLine405ProbeVerdict(p.ID, "healthy_recent", now.Add(4*time.Second))
	if s.Line405Blocked(e, now.Add(4*time.Second)) {
		t.Fatal("健康结论应提前解封")
	}
	if list := s.Line405Failures(); list[0].ProbeState != "healthy" || list[0].Strikes != 0 || list[0].Consecutive405 != 0 {
		t.Fatalf("健康结论应清零: %+v", list[0])
	}
	// 不存在的 profile 幂等。
	s.ApplyLine405ProbeVerdict("missing", "healthy_recent", now.Add(5*time.Second))
}

// probeRetireFixture 造一条「已隔离两次 + 巡检 failed」的线路，返回 profile 与 identity。
func probeRetireFixture(t *testing.T) (*Store, ProxyProfile, string) {
	t.Helper()
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "双证线路", 18401, true)
	a := addSelectionAccount(t, s, "双证账号", p.ID)
	e := line405Egress(t, s, a)
	now := time.Now()
	// 第一次隔离。
	s.RecordLine405(e, now)
	s.RecordLine405(e, now.Add(time.Second))
	// 隔离到期后再犯 → Strikes=2（「放出去又犯」一次）。
	later := now.Add(31 * time.Minute)
	s.RecordLine405(e, later)
	return s, p, strictProxyIdentity(p.URL)
}

func line405ProbeRun(p ProxyProfile, mode string) *ModelProbeRun {
	r := &ModelProbeRun{TargetID: p.ID, targetURL: "https://zcode.z.ai", policy: ModelProbeSettings{Mode: mode}}
	r.proofs = []modelProbeEvidence{{ProxyObservation: ProxyObservation{RequestID: "req-evidence-1"}}}
	return r
}

func profileExists(s *Store, id string) bool {
	for _, p := range s.ListProxyProfiles() {
		if p.ID == id {
			return true
		}
	}
	return false
}

// 双证退休：真实流量两次 405（Strikes≥2）+ 巡检 failed + auto_retire → 删除。
// 这是 v2.10.22 没能做到的部分：22 条巡检结论全 suspect、0 次删除。
func TestLine405ProbeRetireWithTwoStrikes(t *testing.T) {
	s, p, identity := probeRetireFixture(t)
	ent := s.Line405Failures()[0]
	if ent.Strikes < 2 {
		t.Fatalf("前置失败：应有 2 次隔离，实际 %+v", ent)
	}
	run := line405ProbeRun(p, "auto_retire")
	s.mu.Lock()
	s.modelProbe.Policy.Mode = "auto_retire"
	s.maybeRetireLine405ProbeLocked(run, identity, time.Now())
	s.mu.Unlock()

	if profileExists(s, p.ID) {
		t.Fatal("双证成立时线路应被退休删除")
	}
}

// 只隔离过一次就删太激进：放出去又犯才是「这条线反复脏」的证据。
func TestLine405ProbeRetireNeedsTwoStrikes(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "单次隔离线路", 18402, true)
	a := addSelectionAccount(t, s, "单次隔离账号", p.ID)
	e := line405Egress(t, s, a)
	now := time.Now()
	s.RecordLine405(e, now)
	s.RecordLine405(e, now.Add(time.Second))

	run := line405ProbeRun(p, "auto_retire")
	s.mu.Lock()
	s.modelProbe.Policy.Mode = "auto_retire"
	s.maybeRetireLine405ProbeLocked(run, strictProxyIdentity(p.URL), now.Add(time.Minute))
	s.mu.Unlock()

	if !profileExists(s, p.ID) {
		t.Fatal("Strikes=1 时不应删除")
	}
}

// observe 模式只观察：即便双证成立也不删（自动处置必须先被显式开启）。
func TestLine405ProbeRetireRespectsObserveMode(t *testing.T) {
	s, p, identity := probeRetireFixture(t)
	run := line405ProbeRun(p, "observe")
	s.mu.Lock()
	s.modelProbe.Policy.Mode = "observe"
	s.maybeRetireLine405ProbeLocked(run, identity, time.Now())
	s.mu.Unlock()

	if !profileExists(s, p.ID) {
		t.Fatal("observe 模式不应删除")
	}
}

// 开关关闭时退回 v2.10.24 行为：只隔离不删。
func TestLine405ProbeRetireSwitchOff(t *testing.T) {
	old := config.Line405ProbeRetire
	config.Line405ProbeRetire = false
	t.Cleanup(func() { config.Line405ProbeRetire = old })

	s, p, identity := probeRetireFixture(t)
	run := line405ProbeRun(p, "auto_retire")
	s.mu.Lock()
	s.modelProbe.Policy.Mode = "auto_retire"
	s.maybeRetireLine405ProbeLocked(run, identity, time.Now())
	s.mu.Unlock()

	if !profileExists(s, p.ID) {
		t.Fatal("开关关闭时不应删除")
	}
}
