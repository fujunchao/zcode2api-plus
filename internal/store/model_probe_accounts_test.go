package store

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

func automaticAccountStore(t *testing.T) (*Store, *model.Account, time.Time) {
	t.Helper()
	oldTarget, oldData := config.UpstreamZai, config.DataDir
	config.UpstreamZai, config.DataDir = "http://model.invalid/v1/messages", t.TempDir()
	t.Cleanup(func() { config.UpstreamZai, config.DataDir = oldTarget, oldData })
	s := newTestStore(t)
	a, err := s.AddAccount(model.ProviderZai, "合成择优账号", jwtFor("auto-window"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(a.Provider, a.ID, func(live *model.Account) {
		live.Quota = map[string]map[string]any{ModelProbeModel: {"available": 10000, "remaining": 10000}}
	}); err != nil {
		t.Fatal(err)
	}
	return s, s.FindAny(a.ID), time.Now()
}

func accountOutcome(id string, status int, at time.Time) ProxyObservation {
	return ProxyObservation{RequestID: id, EntryRef: id, TargetOrigin: "http://model.invalid", TargetPath: "/v1/messages", Method: "POST", Model: ModelProbeModel, ContentRef: "synthetic", ProfileRef: "synthetic", Status: status, Complete: status == 200, At: at}
}

func TestModelProbeAccountStatisticsSeparateTransportAndProbeFailures(t *testing.T) {
	s, a, now := automaticAccountStore(t)
	for i, tc := range []struct {
		status int
		kind   string
	}{{200, "success"}, {429, "rate_limited"}, {503, "upstream_error"}, {0, "timeout"}, {0, "canceled"}} {
		o := accountOutcome(fmt.Sprint(i), tc.status, now)
		s.ObserveModelAccountOutcome(a, o, tc.kind)
		s.ObserveModelAccountOutcome(a, o, tc.kind) // 同次尝试重复回调也只能计一次。
	}
	before := s.modelProbe.AccountWindows[a.ID].score(now)
	if before.Total != 4 || before.Success != 1 || before.Canceled != 1 || before.Neutral != 2 {
		t.Fatalf("完整业务失败分母或分类不正确：%+v", before)
	}
	risk := accountOutcome("probe-risk", 405, now)
	risk.BusinessCode = "3012"
	s.mu.Lock()
	s.recordProbeAccountOutcomeLocked(a.ID, risk, "probe", "risk_control")
	s.mu.Unlock()
	after := s.modelProbe.AccountWindows[a.ID].score(now)
	if after.Total != before.Total || after.Rate != before.Rate || after.ProbeTotal != 1 {
		t.Fatalf("探测可疑节点不能污染业务账号评分：before=%+v after=%+v", before, after)
	}
	if len(s.strictEvidence) != 0 {
		t.Fatal("统计接口不能把其他失败注入严格退役证据")
	}
}

func TestModelProbeAccountScoreUsesConfidenceAndExpiry(t *testing.T) {
	now := time.Now()
	one := &probeAccountWindow{Entries: []probeAccountOutcome{{At: now.UnixMilli(), Source: "business", Kind: "success", Success: true}}}
	stable := &probeAccountWindow{}
	for i := 0; i < 20; i++ {
		stable.Entries = append(stable.Entries, probeAccountOutcome{At: now.UnixMilli(), Source: "business", Kind: "rate_limited", Success: i != 0})
	}
	if one.score(now).Rate >= stable.score(now).Rate {
		t.Fatal("1/1不能无条件压过19/20的充分样本")
	}
	if got := stable.score(now.Add(24*time.Hour + time.Millisecond)); got.Total != 0 || got.Tier != 2 {
		t.Fatalf("过期样本应回到未知而不是保持高分：%+v", got)
	}
}

func TestModelProbeAccountColdPickAndBusinessOccupancy(t *testing.T) {
	s, a, now := automaticAccountStore(t)
	s.mu.Lock()
	s.touchProbeAccountPickLocked(a.ID, now)
	s.pruneProbeAccountWindowsLocked(now)
	w := s.modelProbe.AccountWindows[a.ID]
	s.mu.Unlock()
	if w == nil || w.LastPickAt != now.UnixMilli() {
		t.Fatal("无结果的新账号刚被选中，轮转记录不能立即被清掉")
	}
	release := s.TrackModelAccountUse(a.ID)
	view, err := s.ModelProbeSummary(now)
	if err != nil || len(view.Accounts) != 1 || view.Accounts[0].Reason != "business_busy" {
		t.Fatalf("巡检应避让正在处理业务的账号：%+v %v", view, err)
	}
	release()
	release()
	view, err = s.ModelProbeSummary(now)
	if err != nil || !view.Accounts[0].Eligible {
		t.Fatalf("业务结束后自动恢复资格且重复释放无副作用：%+v %v", view, err)
	}
}

func TestModelProbeAccountWindowRejectsOldCredentialAndPersists(t *testing.T) {
	s, a, now := automaticAccountStore(t)
	s.ObserveModelAccountOutcome(a, accountOutcome("old", 200, now), "success")
	fresh, err := s.AddAccount(a.Provider, a.Name, jwtFor("auto-window")+"-refreshed")
	if err != nil {
		t.Fatal(err)
	}
	s.ObserveModelAccountOutcome(a, accountOutcome("late-old", 200, now), "success")
	view, err := s.ModelProbeSummary(now)
	if err != nil || view.Accounts[0].RecentTotal != 0 {
		t.Fatalf("旧凭据成功不能抬高新凭据排名：%+v %v", view, err)
	}
	s.ObserveModelAccountOutcome(fresh, accountOutcome("fresh", 200, now), "success")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	view, err = again.ModelProbeSummary(now)
	if err != nil || view.Accounts[0].RecentTotal != 1 || view.Accounts[0].RecentSuccess != 1 {
		t.Fatalf("新凭据窗口应在正常关闭与重启后保留：%+v %v", view, err)
	}
}

func TestModelProbeAutomaticMigrationPreservesConsentAndBudgets(t *testing.T) {
	s, a, now := automaticAccountStore(t)
	p := s.ModelProbeSettings()
	p.HealthVersion, p.Enabled, p.Mode = 2, false, "observe"
	p.TargetMode, p.AccountMode = "manual", "dedicated"
	p.AccountIDs, p.TargetProxyIDs, p.ReferenceProxyIDs = []string{a.ID}, []string{"old-target"}, []string{"old-reference"}
	p.AllowDirectControl, p.GlobalDailyLimit, p.AccountDailyLimit = false, 91, 17
	s.modelProbe.Policy = p
	raw, _ := json.Marshal(s.modelProbe.modelProbePersistent)
	if _, err := s.db.Exec("UPDATE meta SET value=? WHERE key=?", string(raw), modelProbeStateKey); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO model_probe_attempts(id,account_id,started_at) VALUES(?,?,?)", "synthetic-attempt", a.ID, now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	got := again.ModelProbeSettings()
	if got.HealthVersion != 3 || got.TargetMode != "auto" || got.AccountMode != "auto" || len(got.AccountIDs) != 0 || len(got.TargetProxyIDs) != 0 || len(got.ReferenceProxyIDs) != 0 || got.Enabled || got.Mode != "observe" || got.AllowDirectControl || got.GlobalDailyLimit != 91 || got.AccountDailyLimit != 17 {
		t.Fatalf("迁移应只解除手工选择，不扩大处置权限或清预算：%+v", got)
	}
	view, err := again.ModelProbeSummary(now)
	if err != nil || view.UsedRequests != 1 || again.FindAny(a.ID).ModelProbeOnly {
		t.Fatalf("迁移应保留已用预算并释放旧专用角色：%+v %v", view, err)
	}
}
