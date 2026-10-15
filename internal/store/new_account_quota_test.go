package store

import (
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zcode2api/internal/model"
)

func newAccountQuotaEvidence(t *testing.T, s *Store, id string) (*model.Account, model.StartPlanObservation, model.StartPlanObservation) {
	t.Helper()
	checkedAt := float64(time.Now().Unix())
	_, err := s.Update(model.ProviderZai, id, func(a *model.Account) {
		a.StartPlanObservation = model.NewStartPlanObservation(a, checkedAt, true)
	})
	if err != nil {
		t.Fatal(err)
	}
	snap := s.FindAny(id)
	final := model.NewStartPlanObservation(snap, checkedAt+1, true)
	if _, err := s.Update(snap.Provider, id, func(a *model.Account) { a.StartPlanObservation = final }); err != nil {
		t.Fatal(err)
	}
	return snap, snap.StartPlanObservation, final
}

func TestNewAccountQuotaProxyProofRejectsCredentialAndRouteABA(t *testing.T) {
	for _, kind := range []string{"unchanged", "credential", "proxy-url"} {
		t.Run(kind, func(t *testing.T) {
			s := newTestStore(t)
			p := addSelectionProxy(t, s, "初始线路", 18080, true)
			a := addSelectionAccount(t, s, "新账号", p.ID)
			initial := model.NewStartPlanObservation(a, float64(time.Now().Unix()), true)
			if _, err := s.Update(a.Provider, a.ID, func(live *model.Account) { live.StartPlanObservation = initial }); err != nil {
				t.Fatal(err)
			}
			a = s.FindAny(a.ID)
			if !s.RecordStartPlanProxyObservation(a, s.ProxyEgress(a), initial) {
				t.Fatal("真实初始快照应建立代次证明")
			}
			switch kind {
			case "credential":
				for _, secret := range []string{jwtFor("new-generation"), a.Secret()} {
					if _, err := s.RenewJWT(a.Provider, a.ID, secret); err != nil {
						t.Fatal(err)
					}
				}
			case "proxy-url":
				for _, raw := range []string{"http://127.0.0.1:18888", p.URL} {
					if _, err := s.UpdateProxyProfile(p.ID, p.Name, raw, true); err != nil {
						t.Fatal(err)
					}
				}
			}
			live := s.FindAny(a.ID)
			route := s.ProxyEgress(live)
			final := model.NewStartPlanObservation(live, initial.CheckedAt+1, true)
			if _, err := s.Update(a.Provider, a.ID, func(current *model.Account) { current.StartPlanObservation = final }); err != nil {
				t.Fatal(err)
			}
			if !s.RecordStartPlanProxyObservation(live, route, final) {
				t.Fatal("真实末次快照应建立代次证明")
			}
			removed, _, err := s.PurgeEmptyNewAccountProxy(a, initial, final, route)
			if err != nil || removed {
				t.Fatalf("任何代次的空额度都不能授权共享删除：%s removed=%v err=%v", kind, removed, err)
			}
		})
	}
}

func TestNewAccountQuotaPurgeKeepsAllBindings(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprintf("共享=%v", shared), func(t *testing.T) {
			s := newTestStore(t)
			bad := addSelectionProxy(t, s, "bad", 18080, true)
			busy := addSelectionProxy(t, s, "busy", 18081, true)
			best := addSelectionProxy(t, s, "best", 18082, true)
			addSelectionProxy(t, s, "disabled", 18083, false)
			a := addSelectionAccount(t, s, "new", bad.ID)
			peer := addSelectionAccount(t, s, "peer", bad.ID)
			for i := 0; i < 4; i++ {
				addSelectionAccount(t, s, fmt.Sprintf("busy-%d", i), busy.ID)
			}
			if shared {
				addSelectionAccount(t, s, "shared", best.ID)
			}
			snap, initial, final := newAccountQuotaEvidence(t, s, a.ID)
			removed, result, err := s.PurgeEmptyNewAccountProxy(snap, initial, final)
			if err != nil || removed || len(result.Assigned) != 0 || len(result.Direct) != 0 {
				t.Fatalf("空额度不得改变任何关联账号: %v %v %v", removed, result, err)
			}
			for _, id := range []string{a.ID, peer.ID} {
				got := s.FindAny(id)
				if derefStr(got.ProxyID) != bad.ID || derefStr(got.ProxyURL) != bad.URL {
					t.Fatal("应保留既有绑定")
				}
			}
			reopened, err := New()
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if len(reopened.ListProxyProfiles()) != 4 || derefStr(reopened.FindAny(a.ID).ProxyID) != bad.ID {
				t.Fatal("重启仍保留代理与账号绑定")
			}
		})
	}
}

func TestNewAccountQuotaPurgeProtectsChangedAccount(t *testing.T) {
	for _, change := range []string{"binding", "url", "credential", "allocated", "used", "claimed-before", "unknown", "newer", "disabled", "archived", "deleted"} {
		t.Run(change, func(t *testing.T) {
			s := newTestStore(t)
			bad := addSelectionProxy(t, s, "bad", 18080, true)
			other := addSelectionProxy(t, s, "other", 18081, true)
			a := addSelectionAccount(t, s, "new", bad.ID)
			snap, initial, final := newAccountQuotaEvidence(t, s, a.ID)
			switch change {
			case "binding":
				_, _ = s.AssignProxyProfile(a.ID, other.ID)
			case "url":
				_, _ = s.UpdateProxyProfile(bad.ID, bad.Name, "http://127.0.0.1:18082", true)
			case "deleted":
				_, _ = s.RemoveAccount(a.Provider, a.ID)
			case "claimed-before":
				ts := float64(1)
				snap.SetClaimState(&model.ClaimState{ClaimedAt: &ts})
			default:
				_, _ = s.Update(a.Provider, a.ID, func(live *model.Account) {
					switch change {
					case "credential":
						token := "changed.token.sig"
						live.JWTToken = &token
					case "allocated":
						live.Plans = []map[string]any{{"plan_id": "start-plan"}}
						live.StartPlanObservation.Missing = false
					case "used":
						live.UseCount = 1
					case "unknown":
						live.StartPlanObservation = model.StartPlanObservation{}
					case "newer":
						live.StartPlanObservation.CheckedAt++
					case "disabled":
						live.Enabled = false
					case "archived":
						live.ArchivedAt = &initial.CheckedAt
					}
				})
			}
			before := s.ListProxyProfiles()
			if removed, _, err := s.PurgeEmptyNewAccountProxy(snap, initial, final); err != nil || removed {
				t.Fatalf("旧证据不得触发淘汰: %v %v", removed, err)
			}
			if !reflect.DeepEqual(before, s.ListProxyProfiles()) {
				t.Fatal("保护条件下不应修改代理池")
			}
		})
	}
}

func TestNewAccountQuotaPurgeFailureIsAtomic(t *testing.T) {
	s := newTestStore(t)
	bad := addSelectionProxy(t, s, "bad", 18080, true)
	addSelectionProxy(t, s, "good", 18081, true)
	a := addSelectionAccount(t, s, "new", bad.ID)
	peer := addSelectionAccount(t, s, "peer", bad.ID)
	snap, initial, final := newAccountQuotaEvidence(t, s, a.ID)
	beforeProfiles, beforeAccounts := s.ListProxyProfiles(), s.ListAccounts("")
	_, err := s.db.Exec(fmt.Sprintf("CREATE TRIGGER reject_claim_purge BEFORE INSERT ON accounts WHEN NEW.id='%s' BEGIN SELECT RAISE(ABORT,'test failure'); END", peer.ID))
	if err != nil {
		t.Fatal(err)
	}
	if removed, _, err := s.PurgeEmptyNewAccountProxy(snap, initial, final); err != nil || removed {
		t.Fatal("空额度检测不应启动任何删除事务")
	}
	if !reflect.DeepEqual(beforeProfiles, s.ListProxyProfiles()) || !reflect.DeepEqual(beforeAccounts, s.ListAccounts("")) {
		t.Fatal("事务失败不得发布部分内存状态")
	}
	reopened, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if len(reopened.ListProxyProfiles()) != 2 || derefStr(reopened.FindAny(a.ID).ProxyID) != bad.ID {
		t.Fatal("数据库应完整回滚")
	}
	if reopened.FindAny(a.ID).MissingStartPlanEvidence() {
		t.Fatal("重启后不得从历史空快照推断当前线路异常")
	}
}

func TestNewAccountQuotaPurgeConcurrentOnlyOnce(t *testing.T) {
	s := newTestStore(t)
	bad := addSelectionProxy(t, s, "bad", 18080, true)
	addSelectionProxy(t, s, "good", 18081, true)
	a := addSelectionAccount(t, s, "new", bad.ID)
	snap, initial, final := newAccountQuotaEvidence(t, s, a.ID)
	var count atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			removed, _, err := s.PurgeEmptyNewAccountProxy(snap, initial, final)
			if err != nil {
				t.Error(err)
			}
			if removed {
				count.Add(1)
			}
		}()
	}
	wg.Wait()
	if count.Load() != 0 || len(s.ListProxyProfiles()) != 2 {
		t.Fatal("并发/重复空额度检测不应淘汰任何线路")
	}
}

func TestNewAccountQuotaPurgeWithoutAlternatives(t *testing.T) {
	s := newTestStore(t)
	bad := addSelectionProxy(t, s, "bad", 18080, true)
	a := addSelectionAccount(t, s, "new", bad.ID)
	snap, initial, final := newAccountQuotaEvidence(t, s, a.ID)
	removed, result, err := s.PurgeEmptyNewAccountProxy(snap, initial, final)
	if err != nil || removed || len(result.Direct) != 0 {
		t.Fatalf("无替代线路也不能因空额度退直连: %v %v %v", removed, result, err)
	}
	got := s.FindAny(a.ID)
	if derefStr(got.ProxyID) != bad.ID || derefStr(got.ProxyURL) != bad.URL {
		t.Fatal("必须保留代理地址")
	}
}

func TestNewAccountQuotaPurgeDoesNotReassignSameURL(t *testing.T) {
	s := newTestStore(t)
	bad := addSelectionProxy(t, s, "bad", 18080, true)
	alias := addSelectionProxy(t, s, "same-url", 18080, true)
	good := addSelectionProxy(t, s, "good", 18081, true)
	a := addSelectionAccount(t, s, "new", bad.ID)
	snap, initial, final := newAccountQuotaEvidence(t, s, a.ID)
	removed, result, err := s.PurgeEmptyNewAccountProxy(snap, initial, final)
	if err != nil || removed || len(result.Assigned) != 0 {
		t.Fatalf("空额度不得改派到别名或其他代理: %v %v %v", removed, result, err)
	}
	if len(s.ListProxyProfiles()) != 3 || alias.ID == good.ID {
		t.Fatal("不得删除任何线路")
	}
}

func TestNewAccountQuotaPurgeRequiresTwoIndependentObservations(t *testing.T) {
	for _, change := range []string{"first-positive", "second-positive", "first-unknown", "second-unknown", "same-query", "other-account", "other-proxy", "direct"} {
		t.Run(change, func(t *testing.T) {
			s := newTestStore(t)
			bad := addSelectionProxy(t, s, "bad", 18080, true)
			a := addSelectionAccount(t, s, "new", bad.ID)
			snap, initial, final := newAccountQuotaEvidence(t, s, a.ID)
			switch change {
			case "first-positive":
				initial.Missing = false
			case "second-positive":
				final.Missing = false
			case "first-unknown":
				initial = model.StartPlanObservation{}
			case "second-unknown":
				final = model.StartPlanObservation{}
			case "same-query":
				final = initial
			case "other-account":
				initial.AccountID = "other"
			case "other-proxy":
				initial.ProxyURL = "http://127.0.0.1:18082"
			case "direct":
				initial.ProxyID = ""
			}
			if removed, _, err := s.PurgeEmptyNewAccountProxy(snap, initial, final); err != nil || removed {
				t.Fatalf("必须同时持有同账号同出口的两次独立空额度证据：%v %v", removed, err)
			}
			if len(s.ListProxyProfiles()) != 1 {
				t.Fatal("不完整证据不得改变代理池")
			}
		})
	}
}

func TestNewAccountQuotaPurgePreservesSuccessfulClaim(t *testing.T) {
	s := newTestStore(t)
	bad := addSelectionProxy(t, s, "bad", 18080, true)
	addSelectionProxy(t, s, "good", 18081, true)
	a := addSelectionAccount(t, s, "new", bad.ID)
	snap, initial, final := newAccountQuotaEvidence(t, s, a.ID)
	claimedAt, nextAt := initial.CheckedAt+0.5, final.CheckedAt+3600
	_, err := s.UpdateClaimState(a.Provider, a.ID, func(*model.ClaimState) *model.ClaimState {
		return &model.ClaimState{ClaimedAt: &claimedAt, NextAt: &nextAt}
	})
	if err != nil {
		t.Fatal(err)
	}
	removed, _, err := s.PurgeEmptyNewAccountProxy(snap, initial, final)
	if err != nil || removed {
		t.Fatalf("领取结果不确定时不能删除共享代理：%v %v", removed, err)
	}
	got := s.FindAny(a.ID)
	state := got.ClaimView()
	if derefStr(got.ProxyID) != bad.ID || state == nil || state.ClaimedAt == nil || *state.ClaimedAt != claimedAt || state.NextAt == nil || *state.NextAt != nextAt {
		t.Fatal("必须完整保留绑定、领取结果与冷却")
	}
}
