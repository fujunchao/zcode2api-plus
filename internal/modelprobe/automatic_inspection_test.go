package modelprobe_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"zcode2api/internal/model"
	"zcode2api/internal/store"
)

// 使用生产调度、发送和落账路径；额外代理只连接本地 httptest。
func (f *fixture) drainImmediate(t *testing.T) {
	t.Helper()
	for i := 0; i < 40; i++ {
		worked, err := f.m.RunDue(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !worked {
			return
		}
	}
	t.Fatal("当前时刻的阶段未在有界次数内排空")
}

func (f *fixture) observeAccountSuccess(t *testing.T, account *model.Account, n int) {
	t.Helper()
	if _, err := f.st.AssignProxyProfile(account.ID, f.target.ID); err != nil {
		t.Fatal(err)
	}
	a := f.st.FindAny(account.ID)
	for i := 0; i < n; i++ {
		_, err := f.st.ObserveProxyOutcome(a, f.st.ProxyEgress(a), store.ProxyObservation{RequestID: fmt.Sprintf("auto-success-%s-%d", a.ID, i), EntryRef: fmt.Sprintf("entry-%d", i), TargetOrigin: "http://model.invalid", TargetPath: "/v1/messages", Method: "POST", Model: store.ModelProbeModel, ContentRef: "synthetic", ProfileRef: "synthetic", SessionRef: "synthetic", DeviceRef: "synthetic", Status: 200, Complete: true, At: f.now})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestModelProbeAutomaticInspectionAdvancesEachBaseInterval(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 4; i++ {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(completeMessage))
		}))
		t.Cleanup(server.Close)
		if _, err := f.st.AddProxyProfile(fmt.Sprintf("全量节点-%d", i), server.URL, true); err != nil {
			t.Fatal(err)
		}
	}
	f.now = f.now.Truncate(time.Hour)
	f.enableAuto(t, "observe")
	checked := func() int {
		ids := map[string]bool{}
		for _, r := range f.st.ModelProbeHistory("", 0, 100) {
			if r.State == "completed" && r.Verdict == "healthy_recent" && r.Attempts > 0 {
				ids[r.TargetID] = true
			}
		}
		return len(ids)
	}
	if err := f.st.ScheduleModelProbes(f.now); err != nil {
		t.Fatal(err)
	}
	f.drainImmediate(t)
	if got := checked(); got != 4 {
		t.Fatalf("第一批应真实检测4条而不是仅入队：%d", got)
	}
	f.now = f.now.Add(30 * time.Minute)
	if err := f.st.ScheduleModelProbes(f.now); err != nil {
		t.Fatal(err)
	}
	f.drainImmediate(t)
	if got := checked(); got != 6 {
		t.Fatalf("基础间隔30分钟后应补齐全部6条，不能再等放大后的周期：%d", got)
	}
}

func TestModelProbeAutomaticInspectionSkipsSpentAccountNotTarget(t *testing.T) {
	f := newFixture(t)
	f.observeAccountSuccess(t, f.accounts[0], 3)
	f.enableAuto(t, "observe")
	p := f.st.ModelProbeSettings()
	p.AccountDailyLimit = 1
	if _, err := f.st.SaveModelProbeSettings(p, f.now); err != nil {
		t.Fatal(err)
	}
	used := map[string]int{}
	f.respond = func(route, account string, n int) (int, string) {
		used[account]++
		return 200, completeMessage
	}
	for i := 0; i < 2; i++ {
		r := f.drive(t)
		if r.Attempts != 1 || r.Verdict != "healthy_recent" {
			t.Fatalf("最高分账号预算耗尽应换候选而非跳过目标：%+v", r)
		}
		f.now = f.now.Add(time.Millisecond)
	}
	if len(used) != 2 {
		t.Fatal("应使用两个不同的合格账号")
	}
	for _, n := range used {
		if n != 1 {
			t.Fatalf("不得突破单账号一天一次的预算：%v", used)
		}
	}
}

func TestModelProbeAutomaticInspectionSelectsAtDispatch(t *testing.T) {
	f := newFixture(t)
	f.observeAccountSuccess(t, f.accounts[0], 3)
	f.enableAuto(t, "observe")
	r, err := f.st.QueueModelProbe(f.target.ID, "manual", f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.SetEnabled(model.ProviderZai, f.accounts[0].ID, false); err != nil {
		t.Fatal(err)
	}
	f.drainImmediate(t)
	for _, got := range f.st.ModelProbeHistory(f.target.ID, 0, 100) {
		if got.ID == r.ID && got.Attempts == 1 && got.Verdict == "healthy_recent" {
			return
		}
	}
	t.Fatal("排队后首选账号停用，尚未发送的任务应自动选择当前合格账号")
}

func TestModelProbeAutomaticInspectionColdAccountsDoNotShareLease(t *testing.T) {
	f := newFixture(t)
	f.enableAuto(t, "observe")
	p := f.st.ModelProbeSettings()
	p.Concurrency = 2
	if _, err := f.st.SaveModelProbeSettings(p, f.now); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{f.target.ID, f.reference.ID} {
		if _, err := f.st.QueueModelProbe(id, "manual", f.now); err != nil {
			t.Fatal(err)
		}
	}
	first, err := f.st.TakeModelProbe(f.now)
	if err != nil || first == nil {
		t.Fatalf("未取得第一个执行槽：%v", err)
	}
	defer f.st.CompleteModelProbe(first, store.ProxyObservation{}, "canceled", model.Usage{}, f.now)
	second, err := f.st.TakeModelProbe(f.now)
	if err != nil || second == nil {
		t.Fatalf("两个无样本账号都空闲，第二任务不应固定等待第一账号：%v", err)
	}
	defer f.st.CompleteModelProbe(second, store.ProxyObservation{}, "canceled", model.Usage{}, f.now)
	if first.Account().ID == second.Account().ID {
		t.Fatal("两个并发阶段不得占用同一探测账号")
	}
}

func TestModelProbeAutomaticInspectionBusinessSuccessIsNotActiveCoverage(t *testing.T) {
	f := newFixture(t)
	f.enableAuto(t, "observe")
	f.observeAccountSuccess(t, f.accounts[0], 1)
	r, err := f.st.QueueModelProbe(f.target.ID, "scheduled", f.now)
	if err != nil {
		t.Fatal(err)
	}
	if r.State == "skipped" {
		t.Fatal("全节点主动巡检不能用业务旁证免测")
	}
	f.drainImmediate(t)
	if len(f.requests) != 1 {
		t.Fatalf("应实际完成一次目标模型检查：%d", len(f.requests))
	}
}

func TestModelProbeAutomaticInspectionResumesAfterAccountReturns(t *testing.T) {
	f := newFixture(t)
	f.enableAuto(t, "observe")
	for _, a := range f.accounts {
		if _, err := f.st.SetEnabled(a.Provider, a.ID, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.st.ScheduleModelProbes(f.now); err != nil {
		t.Fatal(err)
	}
	f.drainImmediate(t)
	if len(f.requests) != 0 {
		t.Fatal("无合格账号时不得发送请求")
	}
	if _, err := f.st.SetEnabled(f.accounts[0].Provider, f.accounts[0].ID, true); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Minute)
	if err := f.st.ScheduleModelProbes(f.now); err != nil {
		t.Fatal(err)
	}
	f.drainImmediate(t)
	if len(f.requests) != 2 {
		t.Fatalf("账号恢复后应接续两个待检节点，不等下一完整周期：%d", len(f.requests))
	}
}
