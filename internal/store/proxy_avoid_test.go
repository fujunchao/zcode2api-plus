package store

import (
	"testing"

	"zcode2api/internal/model"
)

// 首字节超时后「换号即换线」：回避集合里的线路不得再被选中。
func TestSelectAvoidingSkipsLine(t *testing.T) {
	s := newTestStore(t)
	bad := addSelectionProxy(t, s, "慢线", 18300, true)
	good := addSelectionProxy(t, s, "快线", 18301, true)
	if _, err := s.AddAccount(model.ProviderZai, "a1", "sk-a1"); err != nil {
		t.Fatal(err)
	}
	fast, err := s.AddAccount(model.ProviderZai, "a2", "sk-a2")
	if err != nil {
		t.Fatal(err)
	}
	accounts := s.ListAccounts(model.ProviderZai)
	if len(accounts) != 2 {
		t.Fatalf("账号准备失败：%d", len(accounts))
	}
	for _, a := range accounts {
		target := bad.ID
		if a.ID == fast.ID {
			target = good.ID
		}
		if _, err := s.AssignProxyProfile(a.ID, target); err != nil {
			t.Fatal(err)
		}
	}
	avoid := map[string]bool{strictProxyIdentity(bad.URL): true}
	for i := 0; i < 4; i++ {
		acc := s.SelectAvoiding(model.ProviderZai, map[string]bool{}, "", avoid)
		if acc == nil || acc.ID != fast.ID {
			t.Fatalf("回避慢线后只能选中快线账号，实际 %+v", acc)
		}
	}
}

// 全部候选都在回避集合里时必须回退：宁可再慢一次，也不能返回 nil 变成「无可用账号」。
func TestSelectAvoidingFallsBackWhenAllAvoided(t *testing.T) {
	s := newTestStore(t)
	p1 := addSelectionProxy(t, s, "线1", 18302, true)
	p2 := addSelectionProxy(t, s, "线2", 18303, true)
	addSelectionAccount(t, s, "a1", p1.ID)
	addSelectionAccount(t, s, "a2", p2.ID)
	avoid := map[string]bool{
		strictProxyIdentity(p1.URL): true,
		strictProxyIdentity(p2.URL): true,
	}
	if acc := s.SelectAvoiding(model.ProviderZai, map[string]bool{}, "", avoid); acc == nil {
		t.Fatal("回避清空候选时必须回退到不过滤")
	}
}

// avoid 为 nil 时与旧 Select 完全等价。
func TestSelectAvoidingNilMatchesSelect(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "线", 18304, true)
	addSelectionAccount(t, s, "a1", p.ID)
	if acc := s.SelectAvoiding(model.ProviderZai, map[string]bool{}, "", nil); acc == nil {
		t.Fatal("nil 回避集合不应影响选号")
	}
}
