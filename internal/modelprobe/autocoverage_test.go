// 自动全覆盖与账号自动择优的端到端回归：不手工选择任何目标或账号，
// 全部启用线路进入轮转覆盖，探测账号按近期成功率从合格池自动挑选。
package modelprobe_test

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"zcode2api/internal/model"
	"zcode2api/internal/store"
)

// enableAuto 把 fixture 切换为自动覆盖 + 自动择优：清空手工选择，专用角色随之释放。
// 必须从当前设置派生（保留 revision），否则会撞上乐观锁而不是走到目标行为。
func (f *fixture) enableAuto(t *testing.T, mode string) {
	t.Helper()
	p := f.st.ModelProbeSettings()
	p.Enabled, p.Mode = true, mode
	p.TargetMode, p.AccountMode = "auto", "auto"
	p.AccountIDs, p.TargetProxyIDs, p.ReferenceProxyIDs = nil, nil, nil
	p.EgressGroups = map[string]string{}
	p.ExcludeProxyIDs = nil
	if _, err := f.st.SaveModelProbeSettings(p, f.now); err != nil {
		t.Fatal(err)
	}
}

// 自动模式不需要任何手工选择：未出现在任何列表中的启用线路也能入队，排除名单立即生效。
func TestModelProbeAutoTargetsQueueWithoutSelection(t *testing.T) {
	f := newFixture(t)
	extra, err := f.st.AddProxyProfile("自动纳入线路", "http://127.0.0.1:18099", true)
	if err != nil {
		t.Fatal(err)
	}
	f.enableAuto(t, "observe")
	if _, err := f.st.QueueModelProbe(extra.ID, "manual", f.now); err != nil {
		t.Fatalf("自动模式应允许未手工选择的线路入队：%v", err)
	}
	p := f.st.ModelProbeSettings()
	p.ExcludeProxyIDs = []string{extra.ID}
	if _, err := f.st.SaveModelProbeSettings(p, f.now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.QueueModelProbe(extra.ID, "manual", f.now); err == nil {
		t.Fatal("排除名单中的线路不应入队")
	}
}

// 轮转覆盖：默认预算 200 次/24h、间隔 30 分钟时每轮 4 条，6 条线路分两个窗口覆盖。
func TestModelProbeAutoRotationDrainsBacklogByQuota(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 4; i++ {
		if _, err := f.st.AddProxyProfile(fmt.Sprintf("自动线路-%d", i), fmt.Sprintf("http://127.0.0.1:%d", 18100+i), true); err != nil {
			t.Fatal(err)
		}
	}
	// 对齐到 60 分钟轮转窗口边界：+1 分钟仍在同一窗口，+60 分钟进入下一窗口。
	window := int64((60 * time.Minute).Milliseconds())
	f.now = time.UnixMilli(f.now.UnixMilli() / window * window)
	f.enableAuto(t, "observe")
	openTargets := func() map[string]bool {
		out := map[string]bool{}
		for _, r := range f.st.ModelProbeHistory("", 0, 100) {
			if r.State == "queued" || r.State == "running" || r.State == "waiting" {
				out[r.TargetID] = true
			}
		}
		return out
	}
	if err := f.st.ScheduleModelProbes(f.now); err != nil {
		t.Fatal(err)
	}
	if got := len(openTargets()); got != 4 {
		t.Fatalf("默认预算下每轮应只入队 4 条（共 6 条线路）：%d", got)
	}
	if err := f.st.ScheduleModelProbes(f.now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := len(openTargets()); got != 4 {
		t.Fatalf("同一轮转窗口内不得超出配额：%d", got)
	}
	if err := f.st.ScheduleModelProbes(f.now.Add(60 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := len(openTargets()); got != 6 {
		t.Fatalf("下一个轮转窗口应补齐剩余线路：%d", got)
	}
}

// 账号自动择优：近期有完整业务成功的账号优先于列表靠前但无近期样本的账号。
func TestModelProbeAutoAccountsPickRecentSuccess(t *testing.T) {
	f := newFixture(t)
	third, err := f.st.AddAccount(model.ProviderZai, "第三账号", "eyJhbGciOiJub25lIn0."+base64.RawURLEncoding.EncodeToString([]byte(`{"user_id":"probe-extra","exp":4102444800}`))+".synthetic")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Update(third.Provider, third.ID, func(live *model.Account) {
		live.Quota = map[string]map[string]any{store.ModelProbeModel: {"available": 100000, "remaining": 100000}}
	}); err != nil {
		t.Fatal(err)
	}
	good := f.st.FindAny(f.accounts[1].ID)
	if _, err := f.st.AssignProxyProfile(good.ID, f.target.ID); err != nil {
		t.Fatal(err)
	}
	good = f.st.FindAny(good.ID)
	if _, err := f.st.ObserveProxyOutcome(good, f.st.ProxyEgress(good), store.ProxyObservation{RequestID: "business-success-auto", EntryRef: "entry-auto", TargetOrigin: "http://model.invalid", TargetPath: "/v1/messages", Method: "POST", Model: store.ModelProbeModel, ContentRef: "content", ProfileRef: "profile", SessionRef: "session", DeviceRef: "device", Status: 200, Complete: true, At: f.now}); err != nil {
		t.Fatal(err)
	}
	f.enableAuto(t, "observe")
	var used []string
	f.respond = func(route, account string, n int) (int, string) {
		used = append(used, account)
		return 200, completeMessage
	}
	r := f.drive(t)
	if r.Verdict != "healthy_recent" {
		t.Fatalf("自动择优账号的完整成功应记为健康：%+v", r)
	}
	if len(used) == 0 || used[0] != f.accounts[1].Secret() {
		t.Fatalf("应按近期成功率选择探测账号（列表首位的账号无近期样本）：%v", used)
	}
	for _, a := range f.st.ListAccounts("") {
		if a.ModelProbeOnly {
			t.Fatalf("自动择优不得把账号标记为专用角色：%s", a.Name)
		}
	}
	if live := f.st.FindAny(good.ID); live.ProxyID == nil || *live.ProxyID != f.target.ID {
		t.Fatal("探测不得改账号的持久绑定")
	}
}

// 参考线路自动挑选：同出口组不互证；自动退役缺独立参考时只观察不删除。
func TestModelProbeAutoReferenceRequiresIndependentEgress(t *testing.T) {
	setup := func(t *testing.T, groups map[string]string) (*fixture, store.ProxyProfile) {
		t.Helper()
		f := newFixture(t)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			status, body := f.respond("extra", r.Header.Get("X-Api-Key"), 1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(server.Close)
		line3, err := f.st.AddProxyProfile("独立线路", server.URL, true)
		if err != nil {
			t.Fatal(err)
		}
		f.enableAuto(t, "auto_retire")
		p := f.st.ModelProbeSettings()
		p.EgressGroups = groups
		if _, err := f.st.SaveModelProbeSettings(p, f.now); err != nil {
			t.Fatal(err)
		}
		f.respond = func(route, account string, n int) (int, string) {
			if route == "target" {
				return 405, riskMessage
			}
			return 200, completeMessage
		}
		return f, line3
	}

	t.Run("独立参考成立并淘汰目标", func(t *testing.T) {
		f, line3 := setup(t, nil)
		p := f.st.ModelProbeSettings()
		p.EgressGroups = map[string]string{f.target.ID: "住宅出口", f.reference.ID: "住宅出口", line3.ID: "独立出口"}
		if _, err := f.st.SaveModelProbeSettings(p, f.now); err != nil {
			t.Fatal(err)
		}
		r := f.drive(t)
		if r.Verdict != "route_correlated" || r.DecisionID == "" {
			t.Fatalf("自动挑选的独立参考应支持严格淘汰：%+v", r)
		}
		usedReference := false
		for _, s := range r.Samples {
			if s.Role == "reference" && s.RouteID == line3.ID {
				usedReference = true
			}
		}
		if !usedReference {
			t.Fatalf("参考必须是自动挑选的独立线路：%+v", r.Samples)
		}
		for _, profile := range f.st.ListProxyProfiles() {
			if profile.ID == f.target.ID {
				t.Fatal("证据达标后应淘汰目标线路")
			}
		}
	})

	t.Run("无独立参考时只观察", func(t *testing.T) {
		f, _ := setup(t, map[string]string{})
		r := f.drive(t)
		if r.DecisionID != "" || r.Reason != "no_reference" {
			t.Fatalf("缺少独立出口组声明不得自动删除：%+v", r)
		}
		if len(f.st.ListProxyProfiles()) != 3 {
			t.Fatal("观察结论不得移除线路")
		}
	})
}

// 自动退役至少需要两个合格探测账号：只有一个时保留线路相关结论但不执行删除。
func TestModelProbeAutoRetireRequiresTwoEligibleAccounts(t *testing.T) {
	f := newFixture(t)
	if ok, err := f.st.SetEnabled(model.ProviderZai, f.accounts[1].ID, false); !ok || err != nil {
		t.Fatalf("停用账号失败：%v", err)
	}
	f.enableAuto(t, "auto_retire")
	p := f.st.ModelProbeSettings()
	p.EgressGroups = map[string]string{f.target.ID: "住宅出口", f.reference.ID: "独立参考出口"}
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
	if r.Verdict != "route_correlated" || r.DecisionID != "" || r.Reason != "insufficient_probe_accounts" {
		t.Fatalf("账号不足两个时不得自动删除：%+v", r)
	}
	if len(f.st.ListProxyProfiles()) != 2 {
		t.Fatal("证据不足的结论不得移除线路")
	}
}
