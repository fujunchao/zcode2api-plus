package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"zcode2api/internal/model"
	"zcode2api/internal/proxysub"
)

func TestStrictProxyCanonicalIdentityIgnoresTransportUnusedURLParts(t *testing.T) {
	for _, raw := range []string{
		"HTTP://user:password@EXAMPLE.COM:00080/",
		"http://user:password@example.com/path-is-not-a-route?ignored=yes#fragment",
		"http://user:password@example.com.:80",
	} {
		if strictProxyIdentity(raw) != strictProxyIdentity("http://user:password@example.com") {
			t.Fatalf("传输未使用的 URL 部分不能绕过同一身份：%q", raw)
		}
	}
	if strictProxyIdentity("http://user@example.com") != strictProxyIdentity("http://user:@example.com:80") {
		t.Fatal("相同 Basic Auth 不能因缺省空密码分成两条身份")
	}
}

func TestStrictProxyOldEvidenceCannotDeleteReconfiguredRoute(t *testing.T) {
	for _, change := range []string{"proxy-aba", "credential-aba", "renew-jwt-aba"} {
		t.Run(change, func(t *testing.T) {
			s, bad, good, a := strictComparisonSetup(t)
			old := s.ProxyEgress(a)
			for i := 0; i < 2; i++ {
				strictObserve(t, s, a, strictObservation(fmt.Sprintf("before-%d", i), 405))
			}
			switch change {
			case "proxy-aba":
				if _, err := s.UpdateProxyProfile(bad.ID, bad.Name, "http://127.0.0.1:18888", true); err != nil {
					t.Fatal(err)
				}
				if _, err := s.UpdateProxyProfile(bad.ID, bad.Name, bad.URL, true); err != nil {
					t.Fatal(err)
				}
			case "credential-aba":
				for _, secret := range []string{"replacement-synthetic", a.Secret()} {
					if _, err := s.EditAccount(a.Provider, a.ID, AccountEdit{SetSecret: true, SecretMode: a.Mode, Secret: secret}); err != nil {
						t.Fatal(err)
					}
				}
			case "renew-jwt-aba":
				for _, secret := range []string{jwtFor("replacement"), a.Secret()} {
					if _, err := s.RenewJWT(a.Provider, a.ID, secret); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := s.AcquireProxyDispatch(a, old); err == nil {
				t.Fatal("A→B→A 不得复活旧派发快照")
			}
			d := strictFinishComparison(t, s, a, good)
			if len(d.Removed) != 0 || len(s.ListProxyProfiles()) != 2 {
				t.Fatalf("旧代次失败不能被新成功补成删除决定：%+v", d)
			}
		})
	}
}

func TestStrictProxyDirectSuccessIsControlButDirectFailureIsNotEvidence(t *testing.T) {
	s, bad, _, a := strictComparisonSetup(t)
	for i := 0; i < 2; i++ {
		strictObserve(t, s, a, strictObservation(fmt.Sprintf("failure-%d", i), 405))
	}
	if _, err := s.AssignProxyProfile(a.ID, ""); err != nil {
		t.Fatal(err)
	}
	a = s.FindAny(a.ID)
	egress := s.DirectEgress(a, "direct")
	if d, err := s.ObserveProxyOutcome(a, egress, strictObservation("direct-failure", 405)); err != nil || len(d.Removed) != 0 {
		t.Fatalf("直连失败不得处罚原线路：%+v %v", d, err)
	}
	d, err := s.ObserveProxyOutcome(a, egress, strictObservation("direct-success", 200))
	if err != nil || len(d.Removed) != 1 || d.Removed[0] != bad.ID {
		t.Fatalf("真实直连完整成功应能作为替代线路对照：%+v %v", d, err)
	}
}

func TestStrictProxyEndpointAndEntryValidation(t *testing.T) {
	for _, kind := range []string{"missing-entry", "missing-path", "wrong-method", "different-path"} {
		t.Run(kind, func(t *testing.T) {
			s, _, good, a := strictComparisonSetup(t)
			for i := 0; i < 2; i++ {
				o := strictObservation(fmt.Sprintf("bad-%d", i), 405)
				switch kind {
				case "missing-entry":
					o.EntryRef = ""
				case "missing-path":
					o.TargetPath = ""
				case "wrong-method":
					o.Method = "GET"
				case "different-path":
					o.TargetPath = "/other-model-service"
				}
				strictObserve(t, s, a, o)
			}
			if d := strictFinishComparison(t, s, a, good); len(d.Removed) != 0 {
				t.Fatalf("接口或入口归因不完整不能拼接证据：%s %+v", kind, d)
			}
		})
	}
}

func TestStrictProxyProfileSnapshotsRejectABADeletionAndLoginDispatch(t *testing.T) {
	s, old, _, _ := strictComparisonSetup(t)
	if _, err := s.UpdateProxyProfile(old.ID, old.Name, "http://127.0.0.1:18888", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateProxyProfile(old.ID, old.Name, old.URL, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireProxyProfileDispatch(old); !errors.Is(err, ErrProxyRetired) {
		t.Fatalf("旧登录快照不能再次发出：%v", err)
	}
	if removed, _, err := s.PurgeProxyProfilesIfCurrent([]ProxyProfile{old}); err != nil || len(removed) != 0 {
		t.Fatalf("迟到测活失败不得删除新配置：%v %v", removed, err)
	}
	if result, err := s.ReplaceFailedLoginProxy(old.ID, old.URL, nil, old); err != nil || result.Removed {
		t.Fatalf("迟到登录失败不得删除新配置：%+v %v", result, err)
	}
}

func TestStrictProxyInvalidManualFallbackIsRejected(t *testing.T) {
	s := newTestStore(t)
	a := addSelectionAccount(t, s, "手工", "")
	invalid := "not-a-proxy"
	if _, err := s.SetProxyURL(a.Provider, a.ID, &invalid); err != nil {
		t.Fatal(err)
	}
	a = s.FindAny(a.ID)
	if _, err := s.AcquireProxyDispatch(a, s.DirectEgress(a, "direct_fallback")); !errors.Is(err, ErrInvalidProxyRoute) {
		t.Fatalf("无效手工路由不得静默降为直连：%v", err)
	}
	release, err := s.AcquireProxyDispatch(a, s.DirectEgress(a, "forced_direct"))
	if err != nil {
		t.Fatalf("显式强制直连仍应可用：%v", err)
	}
	release()
}

func TestStrictProxyCapacityPausesImportWithoutBlockingHealthyDispatch(t *testing.T) {
	s, _, _, a := strictComparisonSetup(t)
	s.mu.Lock()
	for i := 0; i < proxysub.MaxBlocks; i++ {
		key := strictDigest(fmt.Sprint(i))
		s.strictProxy.Blocks[key] = strictProxyBlock{Identity: key, State: "removed", RuleVersion: StrictProxyRuleVersion}
	}
	s.strictProxy.ImportPaused = true
	next := s.cloneStrictProxyLocked()
	err := s.commitProxyBundleStrictLocked(context.Background(), nil, nil, nil, nil, &next)
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	release, err := s.AcquireProxyDispatch(a, s.ProxyEgress(a))
	if err != nil {
		t.Fatalf("墓碑容量满只停止新增导入，不能停用健康绑定：%v", err)
	}
	release()
	if _, err := s.AddProxyProfile("容量已满", "http://127.0.0.1:18888", true); err == nil {
		t.Fatal("达到容量预算后必须停止新导入")
	}
}

func TestStrictProxyDisabledDecisionNeverRevivesPersistedBlock(t *testing.T) {
	s, bad, good, a := strictComparisonSetup(t)
	for i := 0; i < 2; i++ {
		strictObserve(t, s, a, strictObservation(fmt.Sprintf("old-%d", i), 405))
	}
	strictFinishComparison(t, s, a, good)
	if err := s.SetSetting(StrictProxyEnabledKey, "false"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireRawProxyDispatch(bad.URL); !errors.Is(err, ErrProxyRetired) {
		t.Fatalf("关闭新判定也不能复活裸 URL：%v", err)
	}
	if _, err := s.AddProxyProfile("改名", bad.URL, true); !errors.Is(err, ErrProxyRetired) {
		t.Fatalf("关闭新判定也不能重新导入：%v", err)
	}
}

func TestStrictProxyPendingAuditAndOriginalAccountStateArePreserved(t *testing.T) {
	s, bad, good, a := strictComparisonSetup(t)
	peer := addSelectionAccount(t, s, "冷却账号", bad.ID)
	until := float64(time.Now().Add(time.Hour).Unix())
	if _, err := s.Update(peer.Provider, peer.ID, func(live *model.Account) {
		live.Status = model.StatusCooling
		live.CoolingUntil = &until
		live.Enabled = false
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		strictObserve(t, s, a, strictObservation(fmt.Sprintf("failure-%d", i), 405))
	}
	d := strictFinishComparison(t, s, a, good)
	if len(d.Removed) != 1 || d.Assigned != 1 {
		t.Fatalf("应只报告实际事务改派：%+v", d)
	}
	got := s.FindAny(peer.ID)
	if got.Status != model.StatusCooling || got.CoolingUntil == nil || *got.CoolingUntil != until || got.Enabled {
		t.Fatal("严格删除只能变路由，不能清账号保护")
	}
}

func TestStrictProxyDeletedAccountsLeaveNoAutomaticPending(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(fmt.Sprint(batch), func(t *testing.T) {
			s := newTestStore(t)
			a := addSelectionAccount(t, s, "自动等待", "")
			s.AutoAssignProxies([]string{a.ID})
			var err error
			if batch {
				_, err = s.BatchRemoveAccounts([]string{a.ID}, false)
			} else {
				_, err = s.RemoveAccount(a.Provider, a.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			status, _ := s.StrictProxySummary()
			if status.Pending != 0 {
				t.Fatalf("账号删除必须同时移除等待意图：%+v", status)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := New()
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if status, _ := reopened.StrictProxySummary(); status.Pending != 0 {
				t.Fatal("重启后不能恢复已删除账号的等待项")
			}
		})
	}
}

func TestStrictProxyBatchCredentialGenerationIsInitializedAtomically(t *testing.T) {
	s := newTestStore(t)
	result, err := s.BatchAddAccounts(model.ProviderZai, []BatchAddItem{{Name: "批量", Secret: "synthetic-batch-key"}})
	if err != nil {
		t.Fatal(err)
	}
	a := s.FindAny(result.IDs[0])
	egress := s.ProxyEgress(a)
	if egress.CredentialGeneration() == 0 {
		t.Fatal("批量入池不能遗漏初始凭据代次")
	}
	if _, err := s.Update(a.Provider, a.ID, func(live *model.Account) { live.UseCount++ }); err != nil {
		t.Fatal(err)
	}
	release, err := s.AcquireProxyDispatch(a, egress)
	if err != nil {
		t.Fatalf("普通额度/统计写入不能使同凭据快照失效：%v", err)
	}
	release()
}

func TestStrictProxyRecoveryRejectsEmptyAndNullJournal(t *testing.T) {
	for _, raw := range []string{"", "null", "{broken"} {
		t.Run(fmt.Sprintf("length-%d", len(raw)), func(t *testing.T) {
			s := newTestStore(t)
			if err := os.WriteFile(s.strictJournalPath, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if reopened, err := New(); err == nil {
				_ = reopened.Close()
				t.Fatal("损坏恢复记录不能让旧代理默认健康")
			}
		})
	}
}

func TestStrictProxyOldPlatformSuccessDoesNotControlNewAllRouteFailures(t *testing.T) {
	s, bad, good, a := strictComparisonSetup(t)
	if _, err := s.AssignProxyProfile(a.ID, good.ID); err != nil {
		t.Fatal(err)
	}
	strictObserve(t, s, s.FindAny(a.ID), strictObservation("earlier-success", 200))
	strictObserve(t, s, s.FindAny(a.ID), strictObservation("control-now-failing", 405))
	if _, err := s.AssignProxyProfile(a.ID, bad.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		d := strictObserve(t, s, s.FindAny(a.ID), strictObservation(fmt.Sprintf("platform-bad-%d", i), 405))
		if len(d.Removed) != 0 {
			t.Fatalf("全线路转为失败后不能用此前旧成功自动删除：%+v", d)
		}
	}
}

func TestStrictProxySelectionForceDirectKeepsAccountProtection(t *testing.T) {
	s, bad, good, a := strictComparisonSetup(t)
	manual := addSelectionAccount(t, s, "手工退役线路", "")
	if _, err := s.SetProxyURL(manual.Provider, manual.ID, &bad.URL); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		strictObserve(t, s, a, strictObservation(fmt.Sprintf("bad-%d", i), 405))
	}
	strictFinishComparison(t, s, a, good)
	skip := map[string]bool{a.ID: true}
	if s.Select(a.Provider, skip, "") != nil {
		t.Fatal("普通选号不得使用被退役的人工裸 URL")
	}
	if selected := s.SelectForDirect(a.Provider, skip, ""); selected == nil || selected.ID != manual.ID {
		t.Fatal("显式强制直连无需使用旧代理，仍应能选择健康账号")
	}
	if _, err := s.Update(manual.Provider, manual.ID, func(live *model.Account) { live.Enabled = false }); err != nil {
		t.Fatal(err)
	}
	if s.SelectForDirect(a.Provider, skip, "") != nil {
		t.Fatal("强制直连不能绕过账号停用")
	}
}
