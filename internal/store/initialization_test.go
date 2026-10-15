package store

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"zcode2api/internal/model"
)

func TestNewAccountInitializationLateResultsCannotOverrideChanges(t *testing.T) {
	for _, change := range []string{"pause", "disable", "batch-disable", "archive", "manual", "credential-aba", "proxy-aba"} {
		t.Run(change, func(t *testing.T) {
			s, a, trial, fixed := initializationStoreFixture(t)
			attempt, err := s.BeginInitialization(a.ID, time.Now())
			if err != nil || attempt == nil {
				t.Fatal(err)
			}
			defer s.ReleaseInitialization(attempt)
			switch change {
			case "pause":
				err = s.ChangeInitialization(a.ID, s.Initialization(a.ID).Revision, "pause")
			case "disable":
				_, err = s.SetEnabled(a.Provider, a.ID, false)
			case "batch-disable":
				_, err = s.BatchSetEnabled([]string{a.ID}, false, false)
			case "archive":
				_, err = s.SetArchived(a.Provider, a.ID, true)
			case "manual":
				_, err = s.AssignProxyProfile(a.ID, fixed.ID)
			case "credential-aba":
				_, err = s.RenewJWT(a.Provider, a.ID, jwtFor("other"))
				if err == nil {
					_, err = s.RenewJWT(a.Provider, a.ID, a.Secret())
				}
			case "proxy-aba":
				_, err = s.UpdateProxyProfile(trial.ID, trial.Name, "http://127.0.0.1:18082", true)
				if err == nil {
					_, err = s.UpdateProxyProfile(trial.ID, trial.Name, trial.URL, true)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			before := s.FindAny(a.ID)
			task := s.Initialization(a.ID)
			if err := s.ObserveInitialization(attempt, model.InitialQuotaObservation{Kind: "available", Snapshot: attempt.Account, CheckedAt: float64(time.Now().Unix())}); !errors.Is(err, ErrInitializationConflict) {
				t.Fatalf("迟到响应未被拒绝：%v", err)
			}
			if !reflect.DeepEqual(before, s.FindAny(a.ID)) || !reflect.DeepEqual(task, s.Initialization(a.ID)) {
				t.Fatal("迟到结果修改了人工操作后的状态")
			}
		})
	}
}

func TestNewAccountInitializationCreateAndBindAreAtomic(t *testing.T) {
	for _, stage := range []string{"create", "bind"} {
		t.Run(stage, func(t *testing.T) {
			s, a, _, _ := initializationStoreFixture(t)
			if stage == "create" {
				if _, err := s.db.Exec(`CREATE TRIGGER reject_initialization BEFORE INSERT ON account_initializations BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
					t.Fatal(err)
				}
				before := s.ListAccounts("")
				if _, _, err := s.AddAccountWithIdentity(model.ProviderZai, "第二个", jwtFor("second"), "", AccountCreationOptions{InitializeProxy: true}); err == nil {
					t.Fatal("登记任务失败时不能创建可调度账号")
				}
				if !reflect.DeepEqual(before, s.ListAccounts("")) {
					t.Fatal("创建失败提前发布了账号")
				}
			} else {
				attempt, err := s.BeginInitialization(a.ID, time.Now())
				if err != nil || attempt == nil {
					t.Fatal(err)
				}
				defer s.ReleaseInitialization(attempt)
				if err := s.ObserveInitialization(attempt, model.InitialQuotaObservation{Kind: "available", Snapshot: attempt.Account, CheckedAt: float64(time.Now().Unix())}); err != nil {
					t.Fatal(err)
				}
				before, task := s.FindAny(a.ID), s.Initialization(a.ID)
				if _, err := s.db.Exec(fmt.Sprintf("CREATE TRIGGER reject_bind BEFORE INSERT ON accounts WHEN NEW.id='%s' BEGIN SELECT RAISE(ABORT,'injected'); END", a.ID)); err != nil {
					t.Fatal(err)
				}
				if ok, err := s.BindInitialization(a.ID, task.Revision, false); err == nil || ok {
					t.Fatal("绑定落盘失败不得报告完成")
				}
				if !reflect.DeepEqual(before, s.FindAny(a.ID)) || !reflect.DeepEqual(task, s.Initialization(a.ID)) {
					t.Fatal("绑定失败发布了部分状态")
				}
			}
			reopened, err := New()
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if len(reopened.ListAccounts("")) != 1 || reopened.FindAny(a.ID).IsSelectable(time.Now()) {
				t.Fatal("重启后不能放行未完成账号")
			}
		})
	}
}

func TestNewAccountInitializationRestartPreservesUncertainClaim(t *testing.T) {
	s, a, _, _ := initializationStoreFixture(t)
	attempt, err := s.BeginInitialization(a.ID, time.Now())
	if err != nil || attempt == nil {
		t.Fatal(err)
	}
	if err := s.RecordInitializationClaim(attempt, "trial"); err != nil {
		t.Fatal(err)
	}
	s.ReleaseInitialization(attempt)
	reopened, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	resumed, err := reopened.BeginInitialization(a.ID, time.Now())
	if err != nil || resumed == nil {
		t.Fatal(err)
	}
	defer reopened.ReleaseInitialization(resumed)
	if resumed.Task.State != "verifying" || !resumed.Task.Uncertain || resumed.Task.Attempts != 1 || len(resumed.Task.PlanIDs) != 1 {
		t.Fatalf("恢复必须核验原请求，不能重开取额：%+v", resumed.Task)
	}
	if resumed.Task.Deadline != attempt.Task.Deadline {
		t.Fatal("重启清零了轮次预算")
	}
}

func TestNewAccountInitializationDispatchAndReplenishment(t *testing.T) {
	s, a, _, fixed := initializationStoreFixture(t)
	for _, mode := range []string{"direct", "forced_direct", "custom_client"} {
		egress := s.DirectEgress(a, mode)
		if mode == "custom_client" {
			egress.Mode = mode
		}
		if release, err := s.AcquireModelDispatch(a, egress); err == nil {
			release()
			t.Fatalf("初始化账号通过 %s 提前出站", mode)
		}
	}
	completeInitializationFixture(t, s, a)
	if _, _, err := s.DeleteProxyProfile(fixed.ID); err != nil {
		t.Fatal(err)
	}
	addSelectionProxy(t, s, "新增补线", 18089, true)
	if got := s.FindAny(a.ID); got.ProxyID != nil || got.IsSelectable(time.Now()) {
		t.Fatal("自动补线覆盖固定出口等待")
	}
	if err := s.ChangeInitialization(a.ID, s.Initialization(a.ID).Revision, "retry"); err == nil {
		t.Fatal("已到账账号不能被重新安排领取")
	}
}

func TestNewAccountInitializationPolicyRevisionAndUnavailableDisable(t *testing.T) {
	s, _, _, fixed := initializationStoreFixture(t)
	policy, err := s.NewAccountProxyPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveNewAccountProxyPolicy(DefaultNewAccountProxyPolicy()); !errors.Is(err, ErrInitializationConflict) {
		t.Fatalf("旧配置覆盖了当前版本：%v", err)
	}
	if _, _, err := s.DeleteProxyProfile(fixed.ID); err != nil {
		t.Fatal(err)
	}
	policy.Enabled = false
	if _, err := s.SaveNewAccountProxyPolicy(policy); err != nil {
		t.Fatalf("节点失效后仍必须能关闭策略：%v", err)
	}
}

func TestNewAccountInitializationExcludesUpdatedDefaultAliases(t *testing.T) {
	s, a, trial, fixed := initializationStoreFixture(t)
	if _, err := s.UpdateProxyProfile(fixed.ID, fixed.Name, trial.URL, true); err != nil {
		t.Fatal(err)
	}
	attempt, err := s.BeginInitialization(a.ID, time.Now())
	if attempt != nil {
		s.ReleaseInitialization(attempt)
	}
	if err != nil || attempt != nil {
		t.Fatalf("默认节点地址更新后，也不能通过别名拿它取额：%v", err)
	}
	if s.Initialization(a.ID).Reason != "no_candidates" {
		t.Fatal("应等待不同出口的取额节点")
	}
}

func TestNewAccountInitializationPauseAfterQuotaPreventsAutomaticBind(t *testing.T) {
	s, a, _, _ := initializationStoreFixture(t)
	attempt, err := s.BeginInitialization(a.ID, time.Now())
	if err != nil || attempt == nil {
		t.Fatal(err)
	}
	defer s.ReleaseInitialization(attempt)
	if err := s.ObserveInitialization(attempt, model.InitialQuotaObservation{Kind: "available", Snapshot: attempt.Account, CheckedAt: float64(time.Now().Unix())}); err != nil {
		t.Fatal(err)
	}
	if err := s.ChangeInitialization(a.ID, s.Initialization(a.ID).Revision, "pause"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.BindInitialization(a.ID, s.Initialization(a.ID).Revision, false); err == nil || ok {
		t.Fatal("到账后被人工暂停的任务仍被后台完成了绑定")
	}
	if !s.FindAny(a.ID).InitializationBlocked() {
		t.Fatal("暂停被当成了业务就绪")
	}
	if ok, err := s.BindInitialization(a.ID, s.Initialization(a.ID).Revision, false, true); err != nil || !ok {
		t.Fatalf("管理员明确重试绑定应可以恢复：%v", err)
	}
}

func initializationStoreFixture(t *testing.T) (*Store, *model.Account, ProxyProfile, ProxyProfile) {
	t.Helper()
	s := newTestStore(t)
	trial := addSelectionProxy(t, s, "取额", 18080, true)
	fixed := addSelectionProxy(t, s, "固定", 18081, true)
	policy := DefaultNewAccountProxyPolicy()
	policy.Enabled = true
	policy.DefaultProxyID = fixed.ID
	if _, err := s.SaveNewAccountProxyPolicy(policy); err != nil {
		t.Fatal(err)
	}
	a, _, err := s.AddAccountWithIdentity(model.ProviderZai, "新号", jwtFor("initialization"), "", AccountCreationOptions{InitializeProxy: true, Source: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return s, s.FindAny(a.ID), trial, fixed
}

func completeInitializationFixture(t *testing.T, s *Store, a *model.Account) {
	t.Helper()
	attempt, err := s.BeginInitialization(a.ID, time.Now())
	if err != nil || attempt == nil {
		t.Fatalf("无法开始：%v", err)
	}
	defer s.ReleaseInitialization(attempt)
	value := attempt.Account.Clone()
	value.Quota = map[string]map[string]any{"test-model": {"model": "test-model", "total": float64(150), "used": float64(0), "remaining": float64(150), "available": float64(150)}}
	if err := s.ObserveInitialization(attempt, model.InitialQuotaObservation{Kind: "available", Snapshot: value, CheckedAt: float64(time.Now().Unix())}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.BindInitialization(a.ID, s.Initialization(a.ID).Revision, false); err != nil || !ok {
		t.Fatalf("无法绑定：%v", err)
	}
}

func TestNewAccountInitializationFixedProxyRoutingIntent(t *testing.T) {
	for _, action := range []string{"risk-rotate", "delete", "manual-direct"} {
		t.Run(action, func(t *testing.T) {
			s, a, _, fixed := initializationStoreFixture(t)
			completeInitializationFixture(t, s, a)
			switch action {
			case "risk-rotate":
				if result, err := s.RotateAccountProxyAt(s.FindAny(a.ID), time.Now()); err != nil || result.Reason != "fixed_proxy" {
					t.Fatalf("固定出口不能自动轮换：%+v %v", result, err)
				}
				if *s.FindAny(a.ID).ProxyID != fixed.ID {
					t.Fatal("固定绑定被覆盖")
				}
			case "delete":
				if _, _, err := s.DeleteProxyProfile(fixed.ID); err != nil {
					t.Fatal(err)
				}
				got := s.FindAny(a.ID)
				if got.ProxyID != nil || got.IsSelectable(time.Now()) || got.Initialization == nil || got.Initialization.GrantedAt == 0 {
					t.Fatalf("已到账账号只能等待固定出口，不得改派或重领：%+v", got.Initialization)
				}
			case "manual-direct":
				if _, err := s.AssignProxyProfile(a.ID, ""); err != nil {
					t.Fatal(err)
				}
				got := s.FindAny(a.ID)
				if got.Initialization.Managed || got.InitializationBlocked() || got.ProxyID != nil {
					t.Fatalf("人工直连应原子解除托管：%+v", got.Initialization)
				}
			}
		})
	}
}
