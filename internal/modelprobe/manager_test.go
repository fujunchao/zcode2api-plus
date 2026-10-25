package modelprobe_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/modelprobe"
	"zcode2api/internal/store"
)

const completeMessage = `{"id":"msg_probe","type":"message","role":"assistant","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`
const riskMessage = `{"code":"3012","message":"Request has been blocked due to unusual activity."}`

type fixture struct {
	st                *store.Store
	m                 *modelprobe.Manager
	now               time.Time
	target, reference store.ProxyProfile
	accounts          []*model.Account
	mu                sync.Mutex
	requests          []string
	respond           func(string, string, int) (int, string)
	contentType       map[string]string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	oldDB, oldData, oldTarget, oldSilent := config.DBPath, config.DataDir, config.UpstreamZai, config.RiskSilentHours
	config.DBPath, config.DataDir, config.UpstreamZai, config.RiskSilentHours = filepath.Join(t.TempDir(), "accounts.db"), t.TempDir(), "http://model.invalid/v1/messages", 0
	t.Cleanup(func() {
		config.DBPath, config.DataDir, config.UpstreamZai, config.RiskSilentHours = oldDB, oldData, oldTarget, oldSilent
	})
	st, err := store.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := &fixture{st: st, now: time.Now()}
	for i := 0; i < 2; i++ {
		token := "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"user_id":"probe-%d","exp":4102444800}`, i))) + ".synthetic"
		a, err := st.AddAccount(model.ProviderZai, fmt.Sprintf("探测%d", i), token)
		if err != nil {
			t.Fatal(err)
		}
		_, err = st.Update(a.Provider, a.ID, func(live *model.Account) {
			live.Quota = map[string]map[string]any{store.ModelProbeModel: {"available": 100000, "remaining": 100000}}
		})
		if err != nil {
			t.Fatal(err)
		}
		f.accounts = append(f.accounts, st.FindAny(a.ID))
	}
	for _, route := range []string{"target", "reference"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			defer f.mu.Unlock()
			account := r.Header.Get("X-Api-Key")
			f.requests = append(f.requests, route+":"+r.Header.Get("X-Request-Id"))
			status, body := http.StatusOK, completeMessage
			if f.respond != nil {
				status, body = f.respond(route, account, len(f.requests))
			}
			w.Header().Set("Content-Type", "application/json")
			if value := f.contentType[route]; value != "" {
				w.Header().Set("Content-Type", value)
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(server.Close)
		p, err := st.AddProxyProfile(route, server.URL, true)
		if err != nil {
			t.Fatal(err)
		}
		if route == "target" {
			f.target = p
		} else {
			f.reference = p
		}
	}
	p := store.DefaultModelProbeSettings()
	p.Enabled = true
	p.Mode = "auto_retire"
	p.TargetMode, p.AccountMode = "manual", "dedicated"
	p.AccountIDs = []string{f.accounts[0].ID, f.accounts[1].ID}
	p.TargetProxyIDs = []string{f.target.ID}
	p.ReferenceProxyIDs = []string{f.reference.ID}
	p.EgressGroups = map[string]string{f.target.ID: "住宅出口", f.reference.ID: "独立参考出口"}
	if _, err := st.SaveModelProbeSettings(p, f.now); err != nil {
		t.Fatal(err)
	}
	f.m = modelprobe.New(context.Background(), st, func() time.Time { return f.now })
	t.Cleanup(f.m.Close)
	return f
}

func (f *fixture) drive(t *testing.T) store.ModelProbeRun {
	t.Helper()
	r, err := f.st.QueueModelProbe(f.target.ID, "manual", f.now)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		_, err := f.m.RunDue(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		history := f.st.ModelProbeHistory(f.target.ID, 0, 100)
		for _, item := range history {
			if item.ID == r.ID {
				r = item
				break
			}
		}
		if r.State == "completed" || r.State == "skipped" || r.State == "interrupted" {
			return r
		}
		if r.NextAt > f.now.UnixMilli() {
			f.now = time.UnixMilli(r.NextAt).Add(time.Millisecond)
		}
	}
	t.Fatal("有界诊断未在阶段预算内结束")
	return r
}

func TestModelProbeHTTPComparableRouteControlRetires(t *testing.T) {
	f := newFixture(t)
	f.respond = func(route, account string, n int) (int, string) {
		if route == "target" {
			return 405, riskMessage
		}
		return 200, completeMessage
	}
	r := f.drive(t)
	if r.Verdict != "route_correlated" || r.ConfirmedRounds != 2 || r.DecisionID == "" {
		t.Fatalf("应通过两轮新鲜对照确认目标线路：%+v", r)
	}
	if r.Attempts != 5 {
		t.Fatalf("一次筛查加两组成对请求应为5次真实出站：%d", r.Attempts)
	}
	if profiles := f.st.ListProxyProfiles(); len(profiles) != 1 || profiles[0].ID != f.reference.ID {
		t.Fatalf("只能淘汰目标线路：%+v", profiles)
	}
	status, err := f.st.StrictProxySummary()
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Blocks) != 1 || status.Blocks[0].RuleVersion != store.ModelProbeRuleVersion {
		t.Fatalf("严格记录必须保留主动规则来源：%+v", status)
	}
	for _, a := range f.accounts {
		live := f.st.FindAny(a.ID)
		if live.ProxyID != nil || live.ProxyURL != nil || live.UseCount != 0 {
			t.Fatal("跨线路探测不能改持久绑定或业务调用次数")
		}
		// 线路相关的 405 只淘汰线路；执行巡检的账号不应被写风控/冷却。
		requireNoAccountRisk(t, live)
	}
}

func TestModelProbeHTTPAccountControlKeepsRoutes(t *testing.T) {
	f := newFixture(t)
	f.respond = func(route, account string, n int) (int, string) {
		if account == f.accounts[0].Secret() {
			return 405, riskMessage
		}
		return 200, completeMessage
	}
	r := f.drive(t)
	if r.Verdict != "account_correlated" {
		t.Fatalf("同一个账号在两路失败，另一个两路成功应偏账号相关：%+v", r)
	}
	if len(f.st.ListProxyProfiles()) != 2 {
		t.Fatal("账号相关不能删除代理")
	}
}

func TestModelProbeHTTPNoHealthyReferenceStopsExpansion(t *testing.T) {
	f := newFixture(t)
	f.respond = func(string, string, int) (int, string) { return 405, riskMessage }
	r := f.drive(t)
	if r.Verdict != "inconclusive" || r.Reason != "reference_unavailable" || r.Attempts > 3 {
		t.Fatalf("没有健康锚点应停止，不能扫描更多账号：%+v", r)
	}
	if len(f.st.ListProxyProfiles()) != 2 {
		t.Fatal("参考组失败不能清空代理池")
	}
	summary, err := f.st.ModelProbeSummary(f.now)
	if err != nil {
		t.Fatal(err)
	}
	if summary.PausedUntil <= f.now.UnixMilli() {
		t.Fatal("缺少参考失败后的全局退避")
	}
}
