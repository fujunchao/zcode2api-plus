package store

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"zcode2api/internal/model"
	"zcode2api/internal/proxysub"
)

func strictObservation(id string, status int) ProxyObservation {
	o := ProxyObservation{RequestID: id, EntryRef: id, TargetOrigin: "https://model.example.test", TargetPath: "/api/anthropic/v1/messages", Method: "POST", Model: "glm-test", ContentRef: "content-a", ProfileRef: "desktop-v1", SessionRef: "session-a", DeviceRef: "device-a", Status: status, At: time.Now()}
	if status == 405 {
		o.BusinessCode = "3012"
	} else if status == 200 {
		o.Complete = true
	}
	return o
}

func strictObserve(t *testing.T, s *Store, a *model.Account, o ProxyObservation) ProxyDecision {
	t.Helper()
	d, err := s.ObserveProxyOutcome(a, s.ProxyEgress(a), o)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func strictComparisonSetup(t *testing.T) (*Store, ProxyProfile, ProxyProfile, *model.Account) {
	t.Helper()
	s := newTestStore(t)
	bad := addSelectionProxy(t, s, "异常", 18101, true)
	good := addSelectionProxy(t, s, "对照", 18102, true)
	a := addSelectionAccount(t, s, "合成账号", bad.ID)
	return s, bad, good, a
}

func strictFinishComparison(t *testing.T, s *Store, a *model.Account, good ProxyProfile) ProxyDecision {
	t.Helper()
	if _, err := s.AssignProxyProfile(a.ID, good.ID); err != nil {
		t.Fatal(err)
	}
	return strictObserve(t, s, s.FindAny(a.ID), strictObservation("success", 200))
}

func TestStrictProxyRuleCounterexamples(t *testing.T) {
	for _, kind := range []string{"single", "duplicate", "same-entry", "rate-limit", "invalid-request", "headers-only", "different-account", "different-content", "different-profile", "different-session", "different-device", "expired", "contradiction", "platform-failure", "direct"} {
		t.Run(kind, func(t *testing.T) {
			s, bad, good, a := strictComparisonSetup(t)
			if kind == "contradiction" {
				strictObserve(t, s, a, strictObservation("mixed-success", 200))
			}
			for i := 0; i < 2; i++ {
				o := strictObservation(fmt.Sprintf("failure-%d", i), 405)
				switch kind {
				case "single":
					if i == 1 {
						continue
					}
				case "duplicate":
					o.RequestID = "same-http"
				case "same-entry":
					o.EntryRef = "same-entry"
				case "rate-limit":
					o.Status = 429
					o.BusinessCode = "3009"
				case "invalid-request":
					o.InvalidRequest = true
				case "expired":
					o.At = time.Now().Add(-11 * time.Minute)
				}
				if kind == "direct" {
					d, err := s.ObserveProxyOutcome(a, ProxyEgress{Mode: "direct"}, o)
					if err != nil || len(d.Removed) != 0 {
						t.Fatalf("直连不能记代理失败: %+v %v", d, err)
					}
				} else {
					strictObserve(t, s, a, o)
				}
			}
			if _, err := s.AssignProxyProfile(a.ID, good.ID); err != nil {
				t.Fatal(err)
			}
			control := s.FindAny(a.ID)
			if kind == "different-account" {
				control = addSelectionAccount(t, s, "其他账号", good.ID)
			}
			o := strictObservation("control", 200)
			switch kind {
			case "headers-only":
				o.Complete = false
			case "different-content":
				o.ContentRef = "other-content"
			case "different-profile":
				o.ProfileRef = "cli-v1"
			case "different-session":
				o.SessionRef = "other-session"
			case "different-device":
				o.DeviceRef = "other-device"
			case "platform-failure":
				o.Status = 405
				o.BusinessCode = "3012"
				o.Complete = false
			}
			d := strictObserve(t, s, control, o)
			if len(d.Removed) != 0 || len(s.ListProxyProfiles()) != 2 {
				t.Fatalf("%s 不满足严格对照，不得删除 %s: %+v", kind, bad.ID, d)
			}
		})
	}
}

func TestStrictProxySharedEvidenceRequiresTwoCredentialControls(t *testing.T) {
	s, bad, good, a := strictComparisonSetup(t)
	accounts := []*model.Account{a, addSelectionAccount(t, s, "账号二", bad.ID), addSelectionAccount(t, s, "账号三", bad.ID)}
	for i, a := range accounts {
		o := strictObservation(fmt.Sprintf("bad-%d", i), 405)
		o.ContentRef = fmt.Sprintf("content-%d", i%2)
		strictObserve(t, s, a, o)
	}
	for i := 0; i < 2; i++ {
		if _, err := s.AssignProxyProfile(accounts[i].ID, good.ID); err != nil {
			t.Fatal(err)
		}
		o := strictObservation(fmt.Sprintf("good-%d", i), 200)
		o.ContentRef = fmt.Sprintf("content-%d", i)
		d := strictObserve(t, s, s.FindAny(accounts[i].ID), o)
		if i == 0 && len(d.Removed) != 0 {
			t.Fatal("路径 B 不能只有一个账号成功对照")
		}
		if i == 1 && (len(d.Removed) != 1 || d.Removed[0] != bad.ID || d.Reason != "shared_route_control") {
			t.Fatalf("路径 B 应成立: %+v", d)
		}
	}
}

func TestStrictProxyStateSurvivesRestartAndManualUnblock(t *testing.T) {
	s, bad, good, a := strictComparisonSetup(t)
	alias := addSelectionProxy(t, s, "重复别名", 18101, true)
	for i := 0; i < 2; i++ {
		strictObserve(t, s, a, strictObservation(fmt.Sprintf("fail-%d", i), 405))
	}
	d := strictFinishComparison(t, s, a, good)
	if len(d.Removed) != 2 {
		t.Fatalf("同身份重复配置均须删除: %+v，别名 %s", d, alias.ID)
	}
	if _, ok := s.GetSetting(strictProxyStateKey); ok {
		t.Fatal("严格内部状态不能暴露为公开设置")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.AddProxyProfile("换名", bad.URL, true); err == nil {
		t.Fatal("重启后不允许重新导入")
	}
	status, err := reopened.StrictProxySummary()
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Blocks) != 1 || status.Blocks[0].State != "removed" {
		t.Fatalf("缺少持久墓碑: %+v", status)
	}
	snap, err := reopened.SubscriptionSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	key := proxysub.Fingerprint(bad.URL)
	if block, ok := snap.State.Blocks[key]; !ok || block.Until != 0 || !strings.HasPrefix(block.Reason, "strict_3012:") {
		t.Fatal("未配置订阅也必须保存永久严格禁止记录")
	}
	if err := reopened.UnblockSubscription(key); err == nil {
		t.Fatal("普通订阅解除不能绕过严格墓碑")
	}
	if err := reopened.UnblockStrictProxy(status.Blocks[0].Identity); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.AddProxyProfile("人工复检后重导", bad.URL, true); err != nil {
		t.Fatal(err)
	}
}

func TestStrictProxyRetirementFailureKeepsBindingsAndRecovers(t *testing.T) {
	s, bad, good, a := strictComparisonSetup(t)
	peer := addSelectionAccount(t, s, "受影响绑定", bad.ID)
	for i := 0; i < 2; i++ {
		strictObserve(t, s, a, strictObservation(fmt.Sprintf("fail-%d", i), 405))
	}
	if _, err := s.AssignProxyProfile(a.ID, good.ID); err != nil {
		t.Fatal(err)
	}
	profiles := s.ListProxyProfiles()
	before := s.FindAny(peer.ID)
	egress := s.ProxyEgress(before)
	if _, err := s.db.Exec("CREATE TRIGGER reject_strict_delete BEFORE INSERT ON accounts BEGIN SELECT RAISE(ABORT,'strict-test'); END"); err != nil {
		t.Fatal(err)
	}
	snapshot := s.FindAny(a.ID)
	d, err := s.ObserveProxyOutcome(snapshot, s.ProxyEgress(snapshot), strictObservation("success", 200))
	if err == nil || len(d.Removed) != 0 || d.Reason != "persist_failed" {
		t.Fatalf("事务失败不能报告删除: %+v %v", d, err)
	}
	if !reflect.DeepEqual(profiles, s.ListProxyProfiles()) || !reflect.DeepEqual(before, s.FindAny(peer.ID)) {
		t.Fatal("事务失败发布了部分配置/绑定")
	}
	if _, err := s.AcquireProxyDispatch(before, egress); err == nil {
		t.Fatal("retiring 后必须阻断新许可")
	}
	if _, err := os.Stat(s.strictJournalPath); err != nil {
		t.Fatalf("缺少持久恢复意图: %v", err)
	}
	if _, err := s.db.Exec("DROP TRIGGER reject_strict_delete"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.FindAny(peer.ID); derefStr(got.ProxyID) != good.ID {
		t.Fatal("启动必须完成待提交的严格删除/改派")
	}
	if err := reopened.RecoverStrictProxyRetirements(); err != nil {
		t.Fatal(err)
	}
	if len(reopened.ListProxyProfiles()) != 1 {
		t.Fatal("恢复应幂等")
	}
}

func TestStrictProxyDispatchVersionsAndIdempotentRelease(t *testing.T) {
	s, bad, good, a := strictComparisonSetup(t)
	egress := s.ProxyEgress(a)
	release, err := s.AcquireProxyDispatch(a, egress)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		strictObserve(t, s, a, strictObservation(fmt.Sprintf("failure-%d", i), 405))
	}
	strictFinishComparison(t, s, a, good)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); release() }()
	}
	wg.Wait()
	if _, err := s.AcquireProxyDispatch(a, egress); err == nil {
		t.Fatal("淘汰后旧许可快照不能再次派发")
	}
	if len(s.strictDispatch) != 0 {
		t.Fatal("重复释放不能使许可计数变为负值")
	}
	if _, err := s.AddProxyProfile("old", bad.URL, true); err == nil {
		t.Fatal("换 ID 不能绕过淘汰")
	}
}

func TestStrictProxyIgnoresChangedCredentialAndRouteVersion(t *testing.T) {
	for _, change := range []string{"credential", "url", "aba-url", "deleted-account"} {
		t.Run(change, func(t *testing.T) {
			s, bad, _, a := strictComparisonSetup(t)
			egress := s.ProxyEgress(a)
			switch change {
			case "credential":
				if _, err := s.EditAccount(a.Provider, a.ID, AccountEdit{SetSecret: true, Secret: "replaced-synthetic-token", SecretMode: "apiKey"}); err != nil {
					t.Fatal(err)
				}
			case "url", "aba-url":
				if _, err := s.UpdateProxyProfile(bad.ID, bad.Name, "http://127.0.0.1:18103", true); err != nil {
					t.Fatal(err)
				}
				if change == "aba-url" {
					if _, err := s.UpdateProxyProfile(bad.ID, bad.Name, bad.URL, true); err != nil {
						t.Fatal(err)
					}
				}
			case "deleted-account":
				if _, err := s.RemoveAccount(a.Provider, a.ID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.AcquireProxyDispatch(a, egress); err == nil {
				t.Fatal("旧凭据/路由快照不得获得派发许可")
			}
			d, err := s.ObserveProxyOutcome(a, egress, strictObservation("stale", 405))
			if err != nil || d.Reason != "stale_snapshot" {
				t.Fatalf("旧快照不得写失败证据: %+v %v", d, err)
			}
		})
	}
}

func TestStrictProxyAutoPendingDoesNotOverrideExplicitDirectOrManualURL(t *testing.T) {
	s := newTestStore(t)
	auto := addSelectionAccount(t, s, "自动", "")
	explicit := addSelectionAccount(t, s, "显式直连", "")
	legacy := addSelectionAccount(t, s, "旧空绑定", "")
	if _, err := s.AssignProxyProfile(explicit.ID, ""); err != nil {
		t.Fatal(err)
	}
	if assigned, fallback := s.AutoAssignProxies([]string{auto.ID, explicit.ID}); len(assigned) != 0 || len(fallback) != 1 || fallback[0] != auto.ID {
		t.Fatalf("只有自动账号进入等待: %v %v", assigned, fallback)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	p := addSelectionProxy(t, reopened, "新补充", 18104, true)
	if derefStr(reopened.FindAny(auto.ID).ProxyID) != p.ID {
		t.Fatal("重启后自动等待账号应随新增线路补绑")
	}
	if reopened.FindAny(explicit.ID).ProxyURL != nil || reopened.FindAny(legacy.ID).ProxyURL != nil {
		t.Fatal("补绑不能猜测旧空绑定或覆盖显式直连")
	}
}

func TestStrictProxyManualURLIsRejectedWithoutSilentReassignment(t *testing.T) {
	s, bad, good, a := strictComparisonSetup(t)
	manual := addSelectionAccount(t, s, "人工裸URL", "")
	if _, err := s.SetProxyURL(manual.Provider, manual.ID, &bad.URL); err != nil {
		t.Fatal(err)
	}
	before := s.FindAny(manual.ID)
	egress := s.ProxyEgress(before)
	for i := 0; i < 2; i++ {
		strictObserve(t, s, a, strictObservation(fmt.Sprintf("failure-%d", i), 405))
	}
	strictFinishComparison(t, s, a, good)
	if !reflect.DeepEqual(before, s.FindAny(manual.ID)) {
		t.Fatal("淘汰不能悄悄重写人工裸 URL")
	}
	if _, err := s.AcquireProxyDispatch(before, egress); err == nil {
		t.Fatal("人工 URL 仍须受统一永久禁止控制")
	}
}

func TestStrictProxySubscriptionLateCommitCannotResurrect(t *testing.T) {
	s := newTestStore(t)
	bad, err := s.AddProxyProfile("待淘汰公网端点", "http://8.8.8.8:18080", true)
	if err != nil {
		t.Fatal(err)
	}
	good := addSelectionProxy(t, s, "对照", 18102, true)
	a := addSelectionAccount(t, s, "合成账号", bad.ID)
	for i := 0; i < 2; i++ {
		strictObserve(t, s, a, strictObservation(fmt.Sprintf("failure-%d", i), 405))
	}
	strictFinishComparison(t, s, a, good)
	snap, err := s.SubscriptionSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	c := snap.State.Config
	c.EnableImported = true
	c, err = s.SaveSubscriptionConfig(c)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.FinishSubscriptionRun(context.Background(), proxysub.Run{ID: "late-probe", Revision: c.Revision, Mode: "import", Status: "completed"}, []proxysub.Candidate{{URL: bad.URL, Key: proxysub.Fingerprint(bad.URL), ProbedAt: time.Now().UnixMilli()}})
	if err != nil {
		t.Fatal(err)
	}
	if r.Added != 0 || r.CommitSkipped != 1 {
		t.Fatalf("迟到探测成功不能复活墓碑: %+v", r)
	}
}

func TestStrictProxyIdentityPreservesAuthenticationPartition(t *testing.T) {
	if strictProxyIdentity("HTTP://User:one@EXAMPLE.COM:80/") != strictProxyIdentity("http://User:one@example.com") {
		t.Fatal("同路由别名应归一")
	}
	if strictProxyIdentity("http://User:one@example.com") == strictProxyIdentity("http://User:two@example.com") {
		t.Fatal("认证身份不同不得粗暴合并")
	}
	if strictProxyIdentity("http://8.8.8.8:80/") != proxysub.Fingerprint("http://8.8.8.8:80") {
		t.Fatal("无认证订阅身份必须一致")
	}
}

func TestProductionProxySelectionUsesSharedAdmission(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		tree, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range tree.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name == "chooseProxyLocked" {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				id, ok := call.Fun.(*ast.Ident)
				if ok && id.Name == "leastLoadedProxy" {
					t.Errorf("%s/%s 绕过统一代理准入", file, fn.Name.Name)
				}
				return true
			})
		}
	}
}

func TestStrictProxyPendingRetainsFailureExclusionsAfterReplenishment(t *testing.T) {
	s := newTestStore(t)
	bad := addSelectionProxy(t, s, "故障", 18101, true)
	alias := addSelectionProxy(t, s, "故障别名", 18101, true)
	a := addSelectionAccount(t, s, "自动改派账号", bad.ID)
	if _, err := s.ReplaceFailedLoginProxy(bad.ID, bad.URL, nil); err != nil {
		t.Fatal(err)
	}
	if got := s.FindAny(a.ID); got.ProxyURL != nil {
		t.Fatal("仅故障别名时应回退直连")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	good := addSelectionProxy(t, reopened, "新补充", 18102, true)
	if got := reopened.FindAny(a.ID); derefStr(got.ProxyID) != good.ID {
		t.Fatalf("补线不能回到失败别名 %s，得到 %+v", alias.ID, got.ProxyID)
	}
}

func TestStrictProxyEditExplicitDirectCancelsPendingAtomically(t *testing.T) {
	s := newTestStore(t)
	a := addSelectionAccount(t, s, "自动等待", "")
	s.AutoAssignProxies([]string{a.ID})
	if _, err := s.db.Exec("CREATE TRIGGER reject_strict_edit BEFORE INSERT ON accounts BEGIN SELECT RAISE(ABORT,'strict-edit'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EditAccount(a.Provider, a.ID, AccountEdit{SetProxyURL: true}); err == nil {
		t.Fatal("人工路由编辑必须报告事务失败")
	}
	if status, _ := s.StrictProxySummary(); status.Pending != 1 {
		t.Fatal("失败事务不能先清除自动等待意图")
	}
	if _, err := s.db.Exec("DROP TRIGGER reject_strict_edit"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EditAccount(a.Provider, a.ID, AccountEdit{SetProxyURL: true}); err != nil {
		t.Fatal(err)
	}
	addSelectionProxy(t, s, "新补充", 18102, true)
	if s.FindAny(a.ID).ProxyURL != nil {
		t.Fatal("显式直连不能被补线覆盖")
	}
	if _, ok := s.GetSetting(strictProxyStateKey); ok {
		t.Fatal("编辑后内部意图不得进入设置")
	}
}

func TestStrictProxyNewBindingAvoidsSharedSuspect(t *testing.T) {
	s, bad, good, a := strictComparisonSetup(t)
	strictObserve(t, s, a, strictObservation("one-failure", 405))
	if p, ok := s.PickAvailableProxyProfile(); !ok || p.ID != good.ID {
		t.Fatalf("登录预选不能新增到跨账号 suspect: %+v", p)
	}
	next := addSelectionAccount(t, s, "新账号", "")
	assigned, _ := s.AutoAssignProxies([]string{next.ID})
	if assigned[next.ID] != good.ID {
		t.Fatal("自动新增绑定须避开共享 suspect")
	}
	if len(s.ListProxyProfiles()) != 2 {
		t.Fatalf("单次失败仍不能删除 suspect %s", bad.ID)
	}
}
