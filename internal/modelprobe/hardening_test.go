package modelprobe_test

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"zcode2api/internal/model"
	"zcode2api/internal/store"
)

func TestModelProbeObserveNeverChangesBusinessRoutes(t *testing.T) {
	f := newFixture(t)
	p := f.st.ModelProbeSettings()
	p.Mode = "observe"
	if _, err := f.st.SaveModelProbeSettings(p, f.now); err != nil {
		t.Fatal(err)
	}
	f.respond = func(route, account string, n int) (int, string) {
		if route == "target" {
			return 405, riskMessage
		}
		return 200, completeMessage
	}
	r := f.drive(t)
	if r.Verdict != "route_correlated" || r.Reason != "qualified_observe" || r.DecisionID != "" || len(f.st.ListProxyProfiles()) != 2 {
		t.Fatalf("观察模式不能处置业务线路：%+v", r)
	}
}

func TestModelProbeHTTPRejectsIncompleteEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"仅usage", 200, `{"usage":{"output_tokens":1}}`},
		{"无消息ID", 200, `{"type":"message","role":"assistant","content":[],"stop_reason":"end_turn","usage":{"output_tokens":1}}`},
		{"200业务错误", 200, riskMessage},
		{"重定向", 302, completeMessage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.respond = func(route, account string, n int) (int, string) {
				if route == "target" {
					return 405, riskMessage
				}
				return tc.status, tc.body
			}
			r := f.drive(t)
			if r.DecisionID != "" || len(f.st.ListProxyProfiles()) != 2 {
				t.Fatalf("不完整参考不能删除代理：%+v", r)
			}
		})
	}
}

func TestModelProbeHTTPNonRiskDoesNotAccumulate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"普通405", 405, `{"code":"9999","message":"method not allowed"}`},
		{"只有文案", 405, `{"message":"blocked"}`},
		{"3009", 429, `{"code":"3009","message":"busy"}`},
		{"401", 401, `{"message":"unauthorized"}`},
		{"超长错误", 405, riskMessage + strings.Repeat(" ", 64<<10)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.respond = func(string, string, int) (int, string) { return tc.status, tc.body }
			r := f.drive(t)
			if r.Attempts != 1 || r.ConfirmedRounds != 0 || len(f.st.ListProxyProfiles()) != 2 {
				t.Fatalf("非完整3012不得进入反复对照：%+v", r)
			}
		})
	}
}

func TestModelProbeHTTPLateResponseAfterVersionChange(t *testing.T) {
	for _, change := range []string{"credential_aba", "route_aba", "policy_disabled"} {
		t.Run(change, func(t *testing.T) {
			f := newFixture(t)
			f.respond = func(route, account string, n int) (int, string) {
				switch change {
				case "credential_aba":
					a := f.accounts[0]
					if _, err := f.st.RenewJWT(a.Provider, a.ID, strings.TrimSuffix(a.Secret(), ".synthetic")+".changed"); err != nil {
						t.Error(err)
					}
					if _, err := f.st.RenewJWT(a.Provider, a.ID, a.Secret()); err != nil {
						t.Error(err)
					}
				case "route_aba":
					p := f.target
					if _, err := f.st.UpdateProxyProfile(p.ID, p.Name, f.reference.URL, true); err != nil {
						t.Error(err)
					}
					if _, err := f.st.UpdateProxyProfile(p.ID, p.Name, p.URL, true); err != nil {
						t.Error(err)
					}
				case "policy_disabled":
					p := f.st.ModelProbeSettings()
					p.Enabled = false
					if _, err := f.st.SaveModelProbeSettings(p, f.now); err != nil {
						t.Error(err)
					}
				}
				return 405, riskMessage
			}
			r := f.drive(t)
			if len(r.Samples) != 1 || !r.Samples[0].Stale || r.DecisionID != "" {
				t.Fatalf("旧执行权的响应不得触发处置：%+v", r)
			}
			if a := f.st.FindAny(f.accounts[0].ID); a.RiskControlStreak != 0 {
				t.Fatal("迟到失败污染了新状态")
			}
		})
	}
}

func TestModelProbeQueueDeduplicatesAndBudgetSurvivesRestart(t *testing.T) {
	f := newFixture(t)
	p := f.st.ModelProbeSettings()
	p.GlobalDailyLimit = 1
	if _, err := f.st.SaveModelProbeSettings(p, f.now); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	ids := make(chan string, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := f.st.QueueModelProbe(f.target.ID, "manual", f.now)
			if err != nil {
				t.Error(err)
				return
			}
			ids <- r.ID
		}()
	}
	wg.Wait()
	close(ids)
	unique := map[string]bool{}
	for id := range ids {
		unique[id] = true
	}
	if len(unique) != 1 {
		t.Fatalf("同目标重复入队：%v", unique)
	}
	f.respond = func(string, string, int) (int, string) { return 200, completeMessage }
	if _, err := f.m.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := f.drive(t)
	if r.Attempts != 0 || r.Reason != "budget_exhausted" {
		t.Fatalf("超过请求预算仍出站：%+v", r)
	}
	reopened, err := store.New()
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	summary, err := reopened.ModelProbeSummary(f.now)
	if err != nil {
		t.Fatal(err)
	}
	if summary.UsedRequests != 1 || summary.RemainingRequests != 0 {
		t.Fatalf("重启重置了滚动请求预算：%+v", summary)
	}
	if reopened.Select(model.ProviderZai, nil, store.ModelProbeModel) != nil {
		t.Fatal("重启后专用角色丢失")
	}
}

func TestModelProbePolicyChangeRevokesUnsentLease(t *testing.T) {
	f := newFixture(t)
	if _, err := f.st.QueueModelProbe(f.target.ID, "manual", f.now); err != nil {
		t.Fatal(err)
	}
	l, err := f.st.TakeModelProbe(f.now)
	if err != nil || l == nil {
		t.Fatal(err)
	}
	p := f.st.ModelProbeSettings()
	p.Enabled = false
	if _, err := f.st.SaveModelProbeSettings(p, f.now); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, l.TargetURL(), nil)
	req.Header.Set("X-Request-Id", "synthetic-unissued")
	req.Header.Set("X-Api-Key", l.Account().Secret())
	req.Header.Set("Authorization", "Bearer "+l.Account().Secret())
	client := f.st.GuardModelProbeClient(&http.Client{}, l, func() time.Time { return f.now })
	if _, err := client.Do(req); err == nil {
		t.Fatal("已撤销许可仍然派发")
	}
	if err := f.st.CompleteModelProbe(l, store.ProxyObservation{}, "dispatch_changed", model.Usage{}, f.now); err != nil {
		t.Fatal(err)
	}
	summary, err := f.st.ModelProbeSummary(f.now)
	if err != nil {
		t.Fatal(err)
	}
	if summary.UsedRequests != 0 || summary.Active != 0 {
		t.Fatal("未发送请求不得扣预算或泄漏许可")
	}
}

func TestModelProbeClosedManagerFinishesWaitingRuns(t *testing.T) {
	f := newFixture(t)
	f.respond = func(route, account string, n int) (int, string) { return 405, riskMessage }
	if _, err := f.st.QueueModelProbe(f.target.ID, "manual", f.now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := f.st.ModelProbeHistory(f.target.ID, 0, 1)[0]; r.State != "waiting" {
		t.Fatal("应进入冷却等待")
	}
	f.m.Close()
	if r := f.st.ModelProbeHistory(f.target.ID, 0, 1)[0]; r.State != "interrupted" {
		t.Fatalf("关停后不能留活动任务：%+v", r)
	}
}

func TestModelProbeInvalidAccountsProduceNoHTTP(t *testing.T) {
	for _, reason := range []string{"cooling", "unknown_quota", "disabled_model"} {
		t.Run(reason, func(t *testing.T) {
			f := newFixture(t)
			for _, a := range f.accounts {
				_, err := f.st.Update(a.Provider, a.ID, func(live *model.Account) {
					switch reason {
					case "cooling":
						until := float64(f.now.Add(time.Hour).Unix())
						live.Status, live.CoolingUntil = model.StatusCooling, &until
					case "unknown_quota":
						live.Quota = map[string]map[string]any{}
					case "disabled_model":
						live.DisabledModels = []string{store.ModelProbeModel}
					}
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			r := f.drive(t)
			if r.State != "skipped" || r.Attempts != 0 {
				t.Fatalf("不合格账号不能出站：%+v", r)
			}
		})
	}
}

func TestModelProbeFixedAccountsWaitAfterRetirement(t *testing.T) {
	f := newFixture(t)
	p := store.DefaultNewAccountProxyPolicy()
	p.Enabled = true
	p.DefaultProxyID = f.target.ID
	if _, err := f.st.SaveNewAccountProxyPolicy(p); err != nil {
		t.Fatal(err)
	}
	a, _, err := f.st.AddAccountWithIdentity(model.ProviderZai, "托管业务账号", "eyJhbGciOiJub25lIn0.eyJ1c2VyX2lkIjoiZml4ZWQifQ.synthetic", "", store.AccountCreationOptions{InitializeProxy: true, Source: "test"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := f.st.BeginInitialization(a.ID, f.now)
	if err != nil || attempt == nil {
		t.Fatalf("初始化未开始：%v", err)
	}
	if err := f.st.ObserveInitialization(attempt, model.InitialQuotaObservation{Kind: "available", Snapshot: attempt.Account, CheckedAt: float64(f.now.Unix())}); err != nil {
		t.Fatal(err)
	}
	f.st.ReleaseInitialization(attempt)
	if ok, err := f.st.BindInitialization(a.ID, f.st.Initialization(a.ID).Revision, false); err != nil || !ok {
		t.Fatalf("未绑定固定节点：%v", err)
	}
	f.respond = func(route, account string, n int) (int, string) {
		if route == "target" {
			return 405, riskMessage
		}
		return 200, completeMessage
	}
	r := f.drive(t)
	if r.DecisionID == "" {
		t.Fatalf("未形成严格淘汰：%+v", r)
	}
	live := f.st.FindAny(a.ID)
	if !live.InitializationBlocked() || f.st.Initialization(a.ID).State != "awaiting_default" {
		t.Fatal("固定节点失效必须等待")
	}
	if release, err := f.st.AcquireModelDispatch(live, f.st.DirectEgress(live, "forced_direct")); err == nil {
		release()
		t.Fatal("固定账号不得被强制直连绕过")
	}
}
