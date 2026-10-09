package store

import (
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
)

func TestLoginProxyReplacementReassignsAndSelectsLeastLoaded(t *testing.T) {
	s := newTestStore(t)
	bad := addSelectionProxy(t, s, "bad", 18080, true)
	good := addSelectionProxy(t, s, "good", 18081, true)
	busy := addSelectionProxy(t, s, "busy", 18082, true)
	addSelectionProxy(t, s, "disabled", 18083, false)
	a := addSelectionAccount(t, s, "first", bad.ID)
	b := addSelectionAccount(t, s, "second", bad.ID)
	for i := 0; i < 4; i++ {
		addSelectionAccount(t, s, fmt.Sprintf("busy-%d", i), busy.ID)
	}
	r, err := s.ReplaceFailedLoginProxy(bad.ID, bad.URL, nil)
	if err != nil || !r.Removed || r.Next == nil || r.Next.ID != good.ID || len(r.Reassign.Assigned) != 2 {
		t.Fatalf("删除后应按更新后的绑定数选择出口：%+v %v", r, err)
	}
	if derefStr(s.FindAny(a.ID).ProxyID) != good.ID || derefStr(s.FindAny(b.ID).ProxyID) != good.ID {
		t.Fatal("原代理绑定账号也必须改派")
	}
}

func TestLoginProxyReplacementAvoidsAllPreviouslyFailedURLs(t *testing.T) {
	s := newTestStore(t)
	oldAlias := addSelectionProxy(t, s, "old-alias", 18080, true)
	bad := addSelectionProxy(t, s, "current-bad", 18081, true)
	good := addSelectionProxy(t, s, "good", 18082, true)
	a := addSelectionAccount(t, s, "bound", bad.ID)
	r, err := s.ReplaceFailedLoginProxy(bad.ID, bad.URL, map[string]bool{oldAlias.URL: true})
	if err != nil || !r.Removed || r.Next == nil || r.Next.ID != good.ID || derefStr(s.FindAny(a.ID).ProxyID) != good.ID {
		t.Fatal("登录及关联账号改派均不能回到此前失败地址的别名")
	}
	if len(s.ListProxyProfiles()) != 2 {
		t.Fatal("不能扩大删除到未命中的别名配置")
	}
}

func TestLoginProxyReplacementProtectsUpdatedProfile(t *testing.T) {
	s := newTestStore(t)
	old := addSelectionProxy(t, s, "line", 18080, true)
	_, _ = s.UpdateProxyProfile(old.ID, old.Name, "http://127.0.0.1:18081", true)
	r, err := s.ReplaceFailedLoginProxy(old.ID, old.URL, nil)
	if err != nil || r.Removed || r.Next == nil || r.Next.ID != old.ID || r.Next.URL == old.URL || len(s.ListProxyProfiles()) != 1 {
		t.Fatal("旧连接的迟到失败不能删除已更新的代理")
	}
}

func TestLoginProxyReplacementFailureIsAtomic(t *testing.T) {
	s := newTestStore(t)
	bad := addSelectionProxy(t, s, "bad", 18080, true)
	addSelectionProxy(t, s, "good", 18081, true)
	a := addSelectionAccount(t, s, "bound", bad.ID)
	beforeProfiles, beforeAccounts := s.ListProxyProfiles(), s.ListAccounts("")
	if _, err := s.db.Exec(fmt.Sprintf("CREATE TRIGGER reject_login_proxy BEFORE INSERT ON accounts WHEN NEW.id='%s' BEGIN SELECT RAISE(ABORT,'test failure'); END", a.ID)); err != nil {
		t.Fatal(err)
	}
	r, err := s.ReplaceFailedLoginProxy(bad.ID, bad.URL, nil)
	if err == nil || r.Removed || r.Next != nil || !reflect.DeepEqual(beforeProfiles, s.ListProxyProfiles()) || !reflect.DeepEqual(beforeAccounts, s.ListAccounts("")) {
		t.Fatal("事务失败必须保留原代理和全部绑定")
	}
}

func TestLoginProxyReplacementConcurrentOnlyDeletesOnce(t *testing.T) {
	s := newTestStore(t)
	bad := addSelectionProxy(t, s, "bad", 18080, true)
	good := addSelectionProxy(t, s, "good", 18081, true)
	var wg sync.WaitGroup
	var removed atomic.Int32
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := s.ReplaceFailedLoginProxy(bad.ID, bad.URL, nil)
			if err != nil {
				t.Error(err)
			}
			if r.Removed {
				removed.Add(1)
			}
			if r.Next == nil || r.Next.ID != good.ID {
				t.Error("应返回可用的后续线路")
			}
		}()
	}
	wg.Wait()
	if removed.Load() != 1 || len(s.ListProxyProfiles()) != 1 {
		t.Fatal("多个会话同时失败只能删除原配置一次")
	}
}

func TestLoginProxyReplacementFallsBackDirect(t *testing.T) {
	s := newTestStore(t)
	bad := addSelectionProxy(t, s, "bad", 18080, true)
	addSelectionProxy(t, s, "alias", 18080, true)
	a := addSelectionAccount(t, s, "bound", bad.ID)
	r, err := s.ReplaceFailedLoginProxy(bad.ID, bad.URL, nil)
	if err != nil || !r.Removed || r.Next != nil || len(r.Reassign.Direct) != 1 || s.FindAny(a.ID).ProxyID != nil {
		t.Fatal("只剩失败地址别名时应明确回退直连")
	}
}
