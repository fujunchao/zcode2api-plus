package store

import (
	"fmt"
	"reflect"
	"sync"
	"testing"

	"zcode2api/internal/model"
)

func TestDeleteProxyLeastLoadedReassignment(t *testing.T) {
	for _, tc := range []struct {
		name            string
		loads           []int
		want            []int // 目标代理的列表下标；-1 表示无候选时直连。
		removedDisabled bool
		noDisabled      bool
	}{
		{name: "优先空闲代理", loads: []int{2, 0}, want: []int{1}},
		{name: "无空闲时选择最少绑定", loads: []int{3, 1}, want: []int{1}},
		{name: "相同绑定数按列表顺序", loads: []int{1, 1}, want: []int{0}},
		{name: "空闲耗尽后继续均衡共享", loads: []int{0, 0}, want: []int{0, 1, 0, 1, 0}},
		{name: "全部占用时逐个更新负载", loads: []int{2, 1}, want: []int{1, 0, 1, 0, 1}},
		{name: "删除停用代理也重绑账号", loads: []int{1}, want: []int{0}, removedDisabled: true},
		{name: "仅剩停用代理才回退直连", want: []int{-1, -1}},
		{name: "删除唯一代理才回退直连", want: []int{-1, -1}, noDisabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			removed := addSelectionProxy(t, s, "removed", 18080, !tc.removedDisabled)
			if !tc.noDisabled {
				// 停用代理即使排在列表最前且零绑定，也不能参与补位。
				addSelectionProxy(t, s, "disabled", 18081, false)
			}
			var candidates []ProxyProfile
			for i, load := range tc.loads {
				p := addSelectionProxy(t, s, fmt.Sprintf("remaining-%d", i), 18082+i, true)
				candidates = append(candidates, p)
				for j := 0; j < load; j++ {
					addSelectionAccount(t, s, fmt.Sprintf("peer-%d-%d", i, j), p.ID)
				}
			}
			// 已有直连和手工 URL 不参与自动改派，也不占用命名代理。
			addSelectionAccount(t, s, "direct", "")
			manual := addSelectionAccount(t, s, "manual", "")
			manualURL := removed.URL
			if len(candidates) > 0 {
				manualURL = candidates[len(candidates)-1].URL
			}
			if _, err := s.SetProxyURL(manual.Provider, manual.ID, &manualURL); err != nil {
				t.Fatal(err)
			}
			wantAssigned := map[string]string{}
			var wantDirect []string
			wantTargets := map[string]int{}
			for i, target := range tc.want {
				a := addSelectionAccount(t, s, fmt.Sprintf("moved-%d", i), removed.ID)
				wantTargets[a.ID] = target
				if target < 0 {
					wantDirect = append(wantDirect, a.ID)
				} else {
					wantAssigned[a.ID] = candidates[target].ID
				}
			}
			wantProfiles := s.ListProxyProfiles()[1:]
			wantAccounts := s.ListAccounts("")
			for _, a := range wantAccounts {
				if target, moved := wantTargets[a.ID]; moved {
					a.ProxyID, a.ProxyURL = nil, nil
					if target >= 0 {
						a.ProxyID, a.ProxyURL = &candidates[target].ID, &candidates[target].URL
					}
				}
			}

			ok, result, err := s.DeleteProxyProfile(removed.ID)
			if err != nil || !ok {
				t.Fatalf("删除失败: %v %v", ok, err)
			}
			if !reflect.DeepEqual(result.Assigned, wantAssigned) || !reflect.DeepEqual(result.Direct, wantDirect) {
				t.Fatalf("改派结果不符: got=%+v wantAssigned=%v wantDirect=%v", result, wantAssigned, wantDirect)
			}
			if !reflect.DeepEqual(s.ListProxyProfiles(), wantProfiles) || !reflect.DeepEqual(s.ListAccounts(""), wantAccounts) {
				t.Fatal("仅应删除目标代理，并同步改派关联账号的 ID 与 URL")
			}
			reopened, err := New()
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if !reflect.DeepEqual(reopened.ListProxyProfiles(), wantProfiles) || !reflect.DeepEqual(reopened.ListAccounts(""), wantAccounts) {
				t.Fatal("重新打开数据库后必须保留全部改派结果和未受影响的账号")
			}
		})
	}
}

func TestDeleteProxyCountsAllBindings(t *testing.T) {
	for _, state := range []string{"disabled", "cooling", "invalid", "archived"} {
		t.Run(state, func(t *testing.T) {
			s := newTestStore(t)
			removed := addSelectionProxy(t, s, "removed", 18080, true)
			busy := addSelectionProxy(t, s, "busy", 18081, true)
			light := addSelectionProxy(t, s, "light", 18082, true)
			addSelectionAccount(t, s, "busy-active", busy.ID)
			peer := addSelectionAccount(t, s, "busy-other", busy.ID)
			addSelectionAccount(t, s, "light-active", light.ID)
			moved := addSelectionAccount(t, s, "moved", removed.ID)
			for _, a := range []*model.Account{peer, moved} {
				_, err := s.Update(a.Provider, a.ID, func(live *model.Account) {
					switch state {
					case "disabled":
						live.Enabled = false
					case "cooling":
						live.Status = model.StatusCooling
						until := float64(4102444800)
						live.CoolingUntil = &until
					case "invalid":
						live.Status = model.StatusInvalid
					case "archived":
						ts := float64(1)
						live.ArchivedAt = &ts
					}
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			want := s.FindAny(moved.ID)
			want.ProxyID, want.ProxyURL = &light.ID, &light.URL
			ok, result, err := s.DeleteProxyProfile(removed.ID)
			if err != nil || !ok || result.Assigned[moved.ID] != light.ID || len(result.Direct) != 0 {
				t.Fatalf("应计入所有状态的绑定并选择 light: %v %+v %v", ok, result, err)
			}
			if !reflect.DeepEqual(s.FindAny(moved.ID), want) {
				t.Fatal("重绑不得改变账号的启停、冷却、失效或归档状态")
			}
		})
	}
}

func TestDeleteProxySharedFailureIsAtomic(t *testing.T) {
	s := newTestStore(t)
	removed := addSelectionProxy(t, s, "removed", 18080, true)
	shared := addSelectionProxy(t, s, "shared", 18081, true)
	addSelectionAccount(t, s, "peer", shared.ID)
	addSelectionAccount(t, s, "first", removed.ID)
	second := addSelectionAccount(t, s, "second", removed.ID)
	s.BumpLineTruncate(removed.ID)
	profiles, accounts, stats := s.ListProxyProfiles(), s.ListAccounts(""), s.LineTruncateStats()
	// 第二个改派账号写入失败，连同已写入的第一个账号和线路表一起回滚。
	_, err := s.db.Exec(fmt.Sprintf("CREATE TRIGGER reject_second_delete BEFORE INSERT ON accounts WHEN NEW.id='%s' BEGIN SELECT RAISE(ABORT,'test failure'); END", second.ID))
	if err != nil {
		t.Fatal(err)
	}
	ok, result, err := s.DeleteProxyProfile(removed.ID)
	if err == nil || ok || len(result.Assigned) != 0 || len(result.Direct) != 0 {
		t.Fatalf("事务失败不能报告删除或改派成功: %v %+v %v", ok, result, err)
	}
	if !reflect.DeepEqual(s.ListProxyProfiles(), profiles) || !reflect.DeepEqual(s.ListAccounts(""), accounts) || !reflect.DeepEqual(s.LineTruncateStats(), stats) {
		t.Fatal("事务失败后线路、账号和断流计数都必须保持原样")
	}
	reopened, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if !reflect.DeepEqual(reopened.ListProxyProfiles(), profiles) || !reflect.DeepEqual(reopened.ListAccounts(""), accounts) {
		t.Fatal("事务失败后数据库不得保留部分删除或改派")
	}
}

func TestConcurrentDeleteProxiesBalancesBindings(t *testing.T) {
	s := newTestStore(t)
	var profiles []ProxyProfile
	for i := 0; i < 4; i++ {
		p := addSelectionProxy(t, s, fmt.Sprintf("line-%d", i), 18080+i, true)
		profiles = append(profiles, p)
		for j := 0; j < 2; j++ {
			addSelectionAccount(t, s, fmt.Sprintf("account-%d-%d", i, j), p.ID)
		}
	}
	var wg sync.WaitGroup
	for _, p := range profiles[:2] {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, result, err := s.DeleteProxyProfile(p.ID); err != nil || !ok || len(result.Direct) != 0 {
				t.Errorf("并发删除不应失败或回退直连: %v %+v %v", ok, result, err)
			}
		}()
	}
	wg.Wait()
	if !reflect.DeepEqual(s.ListProxyProfiles(), profiles[2:]) {
		t.Fatal("必须只保留未删除的代理")
	}
	counts := map[string]int{}
	wantURLs := map[string]string{profiles[2].ID: profiles[2].URL, profiles[3].ID: profiles[3].URL}
	for _, a := range s.ListAccounts("") {
		id := derefStr(a.ProxyID)
		counts[id]++
		if url, ok := wantURLs[id]; !ok || derefStr(a.ProxyURL) != url {
			t.Fatal("并发改派后代理 ID 与地址必须一致，且不能指向已删除代理")
		}
	}
	if len(counts) != 2 || counts[profiles[2].ID] != 4 || counts[profiles[3].ID] != 4 {
		t.Fatalf("全部账号应均衡共享剩余代理: %v", counts)
	}
}
