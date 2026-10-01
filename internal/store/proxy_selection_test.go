package store

import (
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"zcode2api/internal/model"
)

func addSelectionProxy(t *testing.T, s *Store, name string, port int, enabled bool) ProxyProfile {
	t.Helper()
	p, err := s.AddProxyProfile(name, fmt.Sprintf("http://127.0.0.1:%d", port), enabled)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func addSelectionAccount(t *testing.T, s *Store, name string, proxyID string) *model.Account {
	t.Helper()
	a, err := s.AddAccount(model.ProviderZai, name, jwtFor(name))
	if err != nil {
		t.Fatal(err)
	}
	if proxyID != "" {
		if _, err = s.AssignProxyProfile(a.ID, proxyID); err != nil {
			t.Fatal(err)
		}
	}
	return s.FindAny(a.ID)
}

func TestProxySelectionCountsAllBindings(t *testing.T) {
	s := newTestStore(t)
	busy := addSelectionProxy(t, s, "busy", 18081, true)
	light := addSelectionProxy(t, s, "light", 18082, true)
	addSelectionProxy(t, s, "disabled", 18083, false)
	for i, status := range []string{model.StatusActive, model.StatusCooling, model.StatusInvalid} {
		a := addSelectionAccount(t, s, fmt.Sprintf("busy-%d", i), busy.ID)
		_, err := s.Update(a.Provider, a.ID, func(live *model.Account) {
			live.Status = status
			live.Enabled = false
			ts := float64(1)
			live.ArchivedAt = &ts
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	addSelectionAccount(t, s, "light", light.ID)
	if p, ok := s.PickAvailableProxyProfile(); !ok || p.ID != light.ID {
		t.Fatalf("无空闲时应统计所有绑定并选最少者: %v %v", p, ok)
	}
	a := addSelectionAccount(t, s, "new", "")
	assigned, fallback := s.AutoAssignProxies([]string{a.ID, a.ID, "missing"})
	if len(assigned) != 1 || assigned[a.ID] != light.ID || len(fallback) != 0 {
		t.Fatalf("新增应选最少绑定并去重: %v %v", assigned, fallback)
	}
}

func TestAutoAssignPreservesExistingBindings(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "original", 18081, true)
	addSelectionProxy(t, s, "free", 18082, true)
	bound := addSelectionAccount(t, s, "bound", p.ID)
	manual := addSelectionAccount(t, s, "manual", "")
	u := "http://127.0.0.1:19000"
	if _, err := s.SetProxyURL(manual.Provider, manual.ID, &u); err != nil {
		t.Fatal(err)
	}
	before := s.ListAccounts("")
	assigned, fallback := s.AutoAssignProxies([]string{bound.ID, manual.ID})
	if len(assigned) != 0 || len(fallback) != 0 || !reflect.DeepEqual(before, s.ListAccounts("")) {
		t.Fatal("自动分配不得覆盖现有线路或手工代理")
	}
}

func TestAutoAssignSharedBatchFailureIsAtomic(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "shared", 18080, true)
	addSelectionAccount(t, s, "old", p.ID)
	a := addSelectionAccount(t, s, "new-a", "")
	b := addSelectionAccount(t, s, "new-b", "")
	before := s.ListAccounts("")
	// 第二个账号写入失败：前一个账号的共享绑定也必须回滚。
	_, err := s.db.Exec(fmt.Sprintf("CREATE TRIGGER reject_second_proxy BEFORE INSERT ON accounts WHEN NEW.id='%s' BEGIN SELECT RAISE(ABORT,'test failure'); END", b.ID))
	if err != nil {
		t.Fatal(err)
	}
	assigned, fallback := s.AutoAssignProxies([]string{a.ID, b.ID})
	if len(assigned) != 0 || len(fallback) != 2 || !reflect.DeepEqual(before, s.ListAccounts("")) {
		t.Fatal("共享分配失败后不得发布部分内存状态或报告成功")
	}
	reopened, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for _, id := range []string{a.ID, b.ID} {
		got := reopened.FindAny(id)
		if got.ProxyID != nil || got.ProxyURL != nil {
			t.Fatal("事务失败后数据库不得保留部分绑定")
		}
	}
}

func TestConcurrentAutoAssignBalancesProxies(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 3; i++ {
		addSelectionProxy(t, s, fmt.Sprintf("line-%d", i), 18080+i, true)
	}
	var ids []string
	for i := 0; i < 30; i++ {
		ids = append(ids, addSelectionAccount(t, s, fmt.Sprintf("account-%d", i), "").ID)
	}
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			assigned, fallback := s.AutoAssignProxies([]string{id})
			if len(assigned) != 1 || len(fallback) != 0 {
				t.Errorf("并发分配失败: %v %v", assigned, fallback)
			}
		}()
	}
	wg.Wait()
	counts := map[string]int{}
	for _, a := range s.ListAccounts("") {
		counts[derefStr(a.ProxyID)]++
	}
	for _, p := range s.ListProxyProfiles() {
		if counts[p.ID] != 10 {
			t.Fatalf("并发新增应均衡分配: %v", counts)
		}
	}
}

func TestRotateAccountProxySelection(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprintf("共享兜底=%v", shared), func(t *testing.T) {
			s := newTestStore(t)
			old := addSelectionProxy(t, s, "old", 18080, true)
			busy := addSelectionProxy(t, s, "busy", 18081, true)
			best := addSelectionProxy(t, s, "best", 18082, true)
			addSelectionProxy(t, s, "disabled", 18083, false)
			a := addSelectionAccount(t, s, "risk", old.ID)
			peer := addSelectionAccount(t, s, "peer", old.ID)
			for i := 0; i < 3; i++ {
				addSelectionAccount(t, s, fmt.Sprintf("busy-%d", i), busy.ID)
			}
			if shared {
				addSelectionAccount(t, s, "best", best.ID)
			}
			p, changed, err := s.RotateAccountProxy(a)
			if err != nil || !changed || p.ID != best.ID {
				t.Fatalf("应选择其他线路中的最少绑定者: %v %v %v", p, changed, err)
			}
			reopened, err := New()
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			got := reopened.FindAny(a.ID)
			if derefStr(got.ProxyID) != best.ID || derefStr(got.ProxyURL) != best.URL {
				t.Fatal("新代理 ID 与 URL 必须一起落库")
			}
			if !reflect.DeepEqual(peer, s.FindAny(peer.ID)) || len(s.ListProxyProfiles()) != 4 {
				t.Fatal("不得删除原线路或改变其他绑定账号")
			}
		})
	}
}

func TestRotateAccountProxyNoAlternative(t *testing.T) {
	for _, mode := range []string{"direct", "bound", "manual", "same-url"} {
		t.Run(mode, func(t *testing.T) {
			s := newTestStore(t)
			addSelectionProxy(t, s, "disabled", 18083, false)
			a := addSelectionAccount(t, s, "risk", "")
			if mode == "bound" {
				p := addSelectionProxy(t, s, "only", 18080, true)
				_, _ = s.AssignProxyProfile(a.ID, p.ID)
			} else if mode == "manual" || mode == "same-url" {
				u := "http://127.0.0.1:18080"
				_, _ = s.SetProxyURL(a.Provider, a.ID, &u)
				if mode == "same-url" {
					addSelectionProxy(t, s, "same", 18080, true)
				}
			}
			a = s.FindAny(a.ID)
			if _, changed, err := s.RotateAccountProxy(a); err != nil || changed {
				t.Fatalf("无替代线路不应修改: %v %v", changed, err)
			}
			if !reflect.DeepEqual(a, s.FindAny(a.ID)) {
				t.Fatal("无候选时应保留原出口")
			}
		})
	}
}

func TestRotateAccountProxyFromDirectOrManual(t *testing.T) {
	for _, manual := range []bool{false, true} {
		t.Run(fmt.Sprintf("手工代理=%v", manual), func(t *testing.T) {
			s := newTestStore(t)
			p := addSelectionProxy(t, s, "new", 18081, true)
			a := addSelectionAccount(t, s, "risk", "")
			if manual {
				u := "http://127.0.0.1:19000"
				_, _ = s.SetProxyURL(a.Provider, a.ID, &u)
			}
			if got, changed, err := s.RotateAccountProxy(s.FindAny(a.ID)); err != nil || !changed || got.ID != p.ID {
				t.Fatalf("风控时应分配命名线路: %v %v %v", got, changed, err)
			}
		})
	}
}

func TestRotateAccountProxySkipsStaleSnapshot(t *testing.T) {
	for _, change := range []string{"binding", "url", "delete"} {
		t.Run(change, func(t *testing.T) {
			s := newTestStore(t)
			old := addSelectionProxy(t, s, "old", 18080, true)
			other := addSelectionProxy(t, s, "other", 18081, true)
			a := addSelectionAccount(t, s, "risk", old.ID)
			switch change {
			case "binding":
				_, _ = s.AssignProxyProfile(a.ID, other.ID)
			case "url":
				_, _ = s.UpdateProxyProfile(old.ID, old.Name, "http://127.0.0.1:18082", true)
			case "delete":
				_, _ = s.RemoveAccount(a.Provider, a.ID)
			}
			before := s.FindAny(a.ID)
			if _, changed, err := s.RotateAccountProxy(a); err != nil || changed {
				t.Fatalf("旧请求不得覆盖当前绑定: %v %v", changed, err)
			}
			if !reflect.DeepEqual(before, s.FindAny(a.ID)) {
				t.Fatal("旧请求改变了账号")
			}
		})
	}
}

func TestConcurrentRiskResponsesRotateOnlyOnce(t *testing.T) {
	s := newTestStore(t)
	old := addSelectionProxy(t, s, "old", 18080, true)
	addSelectionProxy(t, s, "next", 18081, true)
	a := addSelectionAccount(t, s, "risk", old.ID)
	var changedCount atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, changed, err := s.RotateAccountProxy(a)
			if err != nil {
				t.Error(err)
			}
			if changed {
				changedCount.Add(1)
			}
		}()
	}
	wg.Wait()
	if changedCount.Load() != 1 {
		t.Fatalf("同一旧出口的并发风控响应只能换线一次: %d", changedCount.Load())
	}
}
