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

func markMissingStartPlan(t *testing.T, s *Store, id string) *model.Account {
	t.Helper()
	_, err := s.Update(model.ProviderZai, id, func(a *model.Account) {
		a.StartPlanObservation = model.NewStartPlanObservation(a, float64(time.Now().Unix()), true)
	})
	if err != nil {
		t.Fatal(err)
	}
	return s.FindAny(id)
}

func TestClaimProxyPurgeReassignsAllBindings(t *testing.T) {
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
			snap := markMissingStartPlan(t, s, a.ID)
			removed, result, err := s.PurgeUnprovisionedClaimProxy(snap)
			if err != nil || !removed || len(result.Assigned) != 2 || len(result.Direct) != 0 {
				t.Fatalf("应原子移除并改派全部关联账号: %v %v %v", removed, result, err)
			}
			for _, id := range []string{a.ID, peer.ID} {
				got := s.FindAny(id)
				if derefStr(got.ProxyID) != best.ID || derefStr(got.ProxyURL) != best.URL {
					t.Fatal("应按最少绑定均衡改派")
				}
			}
			reopened, err := New()
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if len(reopened.ListProxyProfiles()) != 3 || derefStr(reopened.FindAny(a.ID).ProxyID) != best.ID {
				t.Fatal("代理移除与账号改派必须一起落库")
			}
		})
	}
}

func TestClaimProxyPurgeProtectsChangedAccount(t *testing.T) {
	for _, change := range []string{"binding", "url", "credential", "allocated", "used", "claimed", "unknown", "deleted"} {
		t.Run(change, func(t *testing.T) {
			s := newTestStore(t)
			bad := addSelectionProxy(t, s, "bad", 18080, true)
			other := addSelectionProxy(t, s, "other", 18081, true)
			a := addSelectionAccount(t, s, "new", bad.ID)
			snap := markMissingStartPlan(t, s, a.ID)
			switch change {
			case "binding":
				_, _ = s.AssignProxyProfile(a.ID, other.ID)
			case "url":
				_, _ = s.UpdateProxyProfile(bad.ID, bad.Name, "http://127.0.0.1:18082", true)
			case "deleted":
				_, _ = s.RemoveAccount(a.Provider, a.ID)
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
					case "claimed":
						ts := float64(1)
						live.SetClaimState(&model.ClaimState{ClaimedAt: &ts})
					case "unknown":
						live.StartPlanObservation = model.StartPlanObservation{}
					}
				})
			}
			before := s.ListProxyProfiles()
			if removed, _, err := s.PurgeUnprovisionedClaimProxy(snap); err != nil || removed {
				t.Fatalf("旧证据不得触发淘汰: %v %v", removed, err)
			}
			if !reflect.DeepEqual(before, s.ListProxyProfiles()) {
				t.Fatal("保护条件下不应修改代理池")
			}
		})
	}
}

func TestClaimProxyPurgeFailureIsAtomic(t *testing.T) {
	s := newTestStore(t)
	bad := addSelectionProxy(t, s, "bad", 18080, true)
	addSelectionProxy(t, s, "good", 18081, true)
	a := addSelectionAccount(t, s, "new", bad.ID)
	peer := addSelectionAccount(t, s, "peer", bad.ID)
	snap := markMissingStartPlan(t, s, a.ID)
	beforeProfiles, beforeAccounts := s.ListProxyProfiles(), s.ListAccounts("")
	_, err := s.db.Exec(fmt.Sprintf("CREATE TRIGGER reject_claim_purge BEFORE INSERT ON accounts WHEN NEW.id='%s' BEGIN SELECT RAISE(ABORT,'test failure'); END", peer.ID))
	if err != nil {
		t.Fatal(err)
	}
	if removed, _, err := s.PurgeUnprovisionedClaimProxy(snap); err == nil || removed {
		t.Fatal("事务失败不得报告移除成功")
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

func TestConcurrentClaimProxyPurgeOnlyOnce(t *testing.T) {
	s := newTestStore(t)
	bad := addSelectionProxy(t, s, "bad", 18080, true)
	addSelectionProxy(t, s, "good", 18081, true)
	a := addSelectionAccount(t, s, "new", bad.ID)
	snap := markMissingStartPlan(t, s, a.ID)
	var count atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			removed, _, err := s.PurgeUnprovisionedClaimProxy(snap)
			if err != nil {
				t.Error(err)
			}
			if removed {
				count.Add(1)
			}
		}()
	}
	wg.Wait()
	if count.Load() != 1 || len(s.ListProxyProfiles()) != 1 {
		t.Fatal("并发/重复风控只应淘汰原线路一次")
	}
}

func TestClaimProxyPurgeWithoutAlternatives(t *testing.T) {
	s := newTestStore(t)
	bad := addSelectionProxy(t, s, "bad", 18080, true)
	a := addSelectionAccount(t, s, "new", bad.ID)
	removed, result, err := s.PurgeUnprovisionedClaimProxy(markMissingStartPlan(t, s, a.ID))
	if err != nil || !removed || len(result.Direct) != 1 {
		t.Fatalf("无替代线路应明确回报直连: %v %v %v", removed, result, err)
	}
	got := s.FindAny(a.ID)
	if got.ProxyID != nil || got.ProxyURL != nil {
		t.Fatal("必须清除已删除代理的地址")
	}
}

func TestClaimProxyPurgeDoesNotReassignSameURL(t *testing.T) {
	s := newTestStore(t)
	bad := addSelectionProxy(t, s, "bad", 18080, true)
	alias := addSelectionProxy(t, s, "same-url", 18080, true)
	good := addSelectionProxy(t, s, "good", 18081, true)
	a := addSelectionAccount(t, s, "new", bad.ID)
	removed, result, err := s.PurgeUnprovisionedClaimProxy(markMissingStartPlan(t, s, a.ID))
	if err != nil || !removed || result.Assigned[a.ID] != good.ID {
		t.Fatalf("补位不得重新选择原代理的同 URL 配置: %v %v %v", removed, result, err)
	}
	if len(s.ListProxyProfiles()) != 2 || alias.ID == good.ID {
		t.Fatal("仅淘汰命中线路，不扩大删除范围")
	}
}
