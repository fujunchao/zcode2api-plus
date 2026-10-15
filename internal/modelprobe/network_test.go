package modelprobe_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/modelprobe"
	"zcode2api/internal/store"
)

func TestUnifiedNetworkModelConnectionFailureDoesNotReplayModel(t *testing.T) {
	f := newFixture(t)
	var modelCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(200)
			return
		}
		modelCalls.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer server.Close()
	if _, err := f.st.UpdateProxyProfile(f.target.ID, "连接复核", server.URL, true); err != nil {
		t.Fatal(err)
	}
	p := f.st.ModelProbeSettings()
	p.NetworkEnabled = true
	if _, err := f.st.SaveModelProbeSettings(p, f.now); err != nil {
		t.Fatal(err)
	}
	m := modelprobe.NewWithNetworkTargets(context.Background(), f.st, func() []string { return []string{"http://model.invalid/"} }, func() time.Time { return f.now })
	defer m.Close()
	if _, err := f.st.QueueModelProbe(f.target.ID, "manual", f.now); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if modelCalls.Load() != 1 {
		t.Fatal("应有一次真实模型连接失败")
	}
	if _, err := m.RunNetworkDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if worked, err := m.RunDue(context.Background()); err != nil || worked {
		t.Fatalf("网络复核不能自动重放模型请求：worked=%t err=%v", worked, err)
	}
	if len(f.st.ListProxyProfiles()) != 2 {
		t.Fatal("浅请求正常时不能因模型连接失败删代理")
	}
}

func TestUnifiedNetworkPreflightFailureDoesNotConsumeModelBudget(t *testing.T) {
	f := newFixture(t)
	var modelCalls atomic.Int32
	f.respond = func(route, secret string, n int) (int, string) {
		if secret != "" {
			modelCalls.Add(1)
		}
		return 403, "Just a moment"
	}
	p := f.st.ModelProbeSettings()
	p.NetworkEnabled = true
	if _, err := f.st.SaveModelProbeSettings(p, f.now); err != nil {
		t.Fatal(err)
	}
	m := modelprobe.NewWithNetworkTargets(context.Background(), f.st, func() []string { return []string{"http://model.invalid/"} }, func() time.Time { return f.now })
	defer m.Close()
	if _, err := f.st.QueueModelProbe(f.target.ID, "manual", f.now); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	summary, err := f.st.ModelProbeSummary(f.now)
	if err != nil {
		t.Fatal(err)
	}
	if summary.UsedRequests != 0 || modelCalls.Load() != 0 {
		t.Fatal("网络未通过不得发送模型或扣模型请求预算")
	}
}

type networkFixture struct {
	st        *store.Store
	m         *modelprobe.Manager
	now       time.Time
	bad, good store.ProxyProfile
	calls     atomic.Int32
	blocked   atomic.Bool
}

func newNetworkFixture(t *testing.T) *networkFixture {
	t.Helper()
	oldDB, oldData := config.DBPath, config.DataDir
	config.DBPath, config.DataDir = filepath.Join(t.TempDir(), "accounts.db"), t.TempDir()
	t.Cleanup(func() { config.DBPath, config.DataDir = oldDB, oldData })
	st, err := store.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := &networkFixture{st: st, now: time.Now()}
	f.blocked.Store(true)
	check := func(r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Api-Key") != "" {
			t.Error("网络探测必须为无凭据 GET")
		}
	}
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { check(r); w.WriteHeader(401) }))
	t.Cleanup(direct.Close)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		check(r)
		f.calls.Add(1)
		if f.blocked.Load() {
			w.Header().Set("Cf-Mitigated", "challenge")
			w.WriteHeader(403)
		} else {
			w.WriteHeader(200)
		}
	}))
	t.Cleanup(bad.Close)
	f.bad, err = st.AddProxyProfile("待复核", bad.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	f.good, err = st.AddProxyProfile("参考", direct.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	f.m = modelprobe.NewWithNetworkTargets(context.Background(), st, func() []string { return []string{direct.URL} }, func() time.Time { return f.now })
	t.Cleanup(f.m.Close)
	return f
}

func (f *networkFixture) first(t *testing.T) {
	t.Helper()
	if err := f.m.QueueNetwork(""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.RunNetworkDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.st.ListProxyProfiles()) != 2 {
		t.Fatal("首轮失败不得删除")
	}
}
func (f *networkFixture) second(t *testing.T) {
	t.Helper()
	f.now = f.now.Add(11 * time.Second)
	if _, err := f.m.RunNetworkDue(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestUnifiedNetworkWithoutAccountsConfirmsBeforeRemoval(t *testing.T) {
	f := newNetworkFixture(t)
	if len(f.st.ListAccounts("")) != 0 {
		t.Fatal("夹具不应使用账号")
	}
	f.first(t)
	if _, err := f.m.RunNetworkDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.calls.Load() != 1 {
		t.Fatal("没有等待复核间隔就再次请求")
	}
	f.second(t)
	if p := f.st.ListProxyProfiles(); len(p) != 1 || p[0].ID != f.good.ID {
		t.Fatal("复核失败且参考正常才应移除目标")
	}
	r, err := f.st.NetworkHealth()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, v := range r {
		if v.ProfileID == f.bad.ID {
			found = v.Removed && v.Second != nil && v.Control != nil && v.Control.OK
		}
	}
	if !found {
		t.Fatal("缺少首轮、复核及参考证据")
	}
}

func TestUnifiedNetworkRecoveryAndObserveKeepProfiles(t *testing.T) {
	for _, mode := range []string{"recovered", "observe"} {
		t.Run(mode, func(t *testing.T) {
			f := newNetworkFixture(t)
			if mode == "observe" {
				p := f.st.ModelProbeSettings()
				p.Mode = "observe"
				if _, err := f.st.SaveModelProbeSettings(p, f.now); err != nil {
					t.Fatal(err)
				}
			}
			f.first(t)
			if mode == "recovered" {
				f.blocked.Store(false)
			}
			f.second(t)
			if len(f.st.ListProxyProfiles()) != 2 {
				t.Fatal("恢复或观察模式不能删除")
			}
		})
	}
}

func TestUnifiedNetworkPolicyAndRouteChangesInvalidateRound(t *testing.T) {
	for _, change := range []string{"disable", "observe", "route"} {
		t.Run(change, func(t *testing.T) {
			f := newNetworkFixture(t)
			f.first(t)
			switch change {
			case "disable":
				if err := f.st.SetSetting("proxy_health_enabled", "false"); err != nil {
					t.Fatal(err)
				}
			case "observe":
				p := f.st.ModelProbeSettings()
				p.Mode = "observe"
				if _, err := f.st.SaveModelProbeSettings(p, f.now); err != nil {
					t.Fatal(err)
				}
			case "route":
				if _, err := f.st.UpdateProxyProfile(f.bad.ID, f.bad.Name, f.good.URL, true); err != nil {
					t.Fatal(err)
				}
			}
			f.second(t)
			if len(f.st.ListProxyProfiles()) != 2 {
				t.Fatal("旧轮次不能删除新状态")
			}
		})
	}
}

func TestUnifiedNetworkAutomaticallyIncludesNewProxy(t *testing.T) {
	f := newNetworkFixture(t)
	f.blocked.Store(false)
	f.first(t)
	added, err := f.st.AddProxyProfile("后续新增", f.good.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(31 * time.Minute)
	if _, err := f.m.RunNetworkDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	records, err := f.st.NetworkHealth()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range records {
		if r.ProfileID == added.ID && r.State == "healthy" {
			return
		}
	}
	t.Fatal("后续新增线路未自动纳入网络层")
}

func TestUnifiedNetworkRestartDoesNotResumeHalfRound(t *testing.T) {
	f := newNetworkFixture(t)
	f.first(t)
	other, err := store.New()
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	r, err := other.NetworkHealth()
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range r {
		if v.State == "waiting" {
			t.Fatal("重启不能沿用待复核删除证据")
		}
	}
	if len(other.ListProxyProfiles()) != 2 {
		t.Fatal("启动恢复不得删除")
	}
}
