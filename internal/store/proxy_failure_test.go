package store

import (
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"zcode2api/internal/model"
)

func TestProxyEndpointPurgePreservesExistingAccounts(t *testing.T) {
	s := newTestStore(t)
	bad := addSelectionProxy(t, s, "bad", 18080, true)
	good := addSelectionProxy(t, s, "good", 18081, true)
	busy := addSelectionProxy(t, s, "busy", 18082, true)
	addSelectionProxy(t, s, "disabled", 18083, false)
	a := addSelectionAccount(t, s, "existing", bad.ID)
	peer := addSelectionAccount(t, s, "peer", bad.ID)
	for i := 0; i < 3; i++ {
		addSelectionAccount(t, s, fmt.Sprintf("busy-%d", i), busy.ID)
	}
	_, _ = s.Update(a.Provider, a.ID, func(live *model.Account) {
		live.UseCount = 42
		live.Plans = []map[string]any{{"plan_id": "existing"}}
		claimedAt, nextAt := float64(1), float64(9999999999)
		live.SetClaimState(&model.ClaimState{ClaimedAt: &claimedAt, NextAt: &nextAt})
	})
	_, _ = s.SetEnabled(peer.Provider, peer.ID, false)
	before := s.ListAccounts("")
	removed, result, err := s.PurgeFailedAccountProxy(s.FindAny(a.ID))
	if err != nil || !removed || len(result.Assigned) != 2 || len(result.Direct) != 0 {
		t.Fatalf("确认故障应原子移除并改派所有绑定：%v %v %v", removed, result, err)
	}
	for _, old := range before {
		if old.ID == a.ID || old.ID == peer.ID {
			old.ProxyID, old.ProxyURL = &good.ID, &good.URL
		}
		if !reflect.DeepEqual(old, s.FindAny(old.ID)) {
			t.Fatal("除代理绑定外不得改动已有账号的任何状态")
		}
	}
}

func TestProxyEndpointPurgeProtectsChangedRequest(t *testing.T) {
	for _, change := range []string{"binding", "url", "credential", "deleted"} {
		t.Run(change, func(t *testing.T) {
			s := newTestStore(t)
			bad := addSelectionProxy(t, s, "bad", 18080, true)
			good := addSelectionProxy(t, s, "good", 18081, true)
			a := addSelectionAccount(t, s, "existing", bad.ID)
			switch change {
			case "binding":
				_, _ = s.AssignProxyProfile(a.ID, good.ID)
			case "url":
				_, _ = s.UpdateProxyProfile(bad.ID, bad.Name, good.URL, true)
			case "credential":
				_, _ = s.Update(a.Provider, a.ID, func(live *model.Account) { token := "new.token.sig"; live.JWTToken = &token })
			case "deleted":
				_, _ = s.RemoveAccount(a.Provider, a.ID)
			}
			before := s.ListProxyProfiles()
			if removed, _, err := s.PurgeFailedAccountProxy(a); err != nil || removed || !reflect.DeepEqual(before, s.ListProxyProfiles()) {
				t.Fatal("迟到故障不允许删除变更后的代理")
			}
		})
	}
}

func TestProxyEndpointPurgeFailureIsAtomic(t *testing.T) {
	s := newTestStore(t)
	bad := addSelectionProxy(t, s, "bad", 18080, true)
	addSelectionProxy(t, s, "good", 18081, true)
	a := addSelectionAccount(t, s, "existing", bad.ID)
	peer := addSelectionAccount(t, s, "peer", bad.ID)
	beforeAccounts, beforeProxies := s.ListAccounts(""), s.ListProxyProfiles()
	if _, err := s.db.Exec(fmt.Sprintf("CREATE TRIGGER reject_endpoint_purge BEFORE INSERT ON accounts WHEN NEW.id='%s' BEGIN SELECT RAISE(ABORT,'test failure'); END", peer.ID)); err != nil {
		t.Fatal(err)
	}
	if removed, _, err := s.PurgeFailedAccountProxy(a); removed || err == nil {
		t.Fatal("事务失败不得报告成功")
	}
	if !reflect.DeepEqual(beforeAccounts, s.ListAccounts("")) || !reflect.DeepEqual(beforeProxies, s.ListProxyProfiles()) {
		t.Fatal("事务失败不得发布部分内存状态")
	}
	reopened, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if len(reopened.ListProxyProfiles()) != 2 || derefStr(reopened.FindAny(a.ID).ProxyID) != bad.ID {
		t.Fatal("失败事务必须完整回滚")
	}
}

func TestProxyEndpointPurgeConcurrentOnlyOnce(t *testing.T) {
	s := newTestStore(t)
	bad := addSelectionProxy(t, s, "bad", 18080, true)
	addSelectionProxy(t, s, "good", 18081, true)
	a := addSelectionAccount(t, s, "existing", bad.ID)
	var wg sync.WaitGroup
	var count atomic.Int32
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			removed, _, err := s.PurgeFailedAccountProxy(a)
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
		t.Fatal("并发失败只能删除原代理一次")
	}
}

func TestProxyEndpointPurgeExcludesSameAddressAndFallsBack(t *testing.T) {
	s := newTestStore(t)
	bad := addSelectionProxy(t, s, "bad", 18080, true)
	alias := addSelectionProxy(t, s, "alias", 18080, true)
	addSelectionProxy(t, s, "disabled", 18081, false)
	a := addSelectionAccount(t, s, "existing", bad.ID)
	removed, result, err := s.PurgeFailedAccountProxy(a)
	if err != nil || !removed || len(result.Direct) != 1 || s.FindAny(a.ID).ProxyID != nil || s.FindAny(a.ID).ProxyURL != nil {
		t.Fatal("只有同地址别名时应清除绑定并回退直连")
	}
	if len(s.ListProxyProfiles()) != 2 || alias.ID == bad.ID {
		t.Fatal("只能删除命中的原代理配置")
	}
}
