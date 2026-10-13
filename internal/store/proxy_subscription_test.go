package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"zcode2api/internal/model"
	"zcode2api/internal/proxy"
	"zcode2api/internal/proxysub"
)

func subscriptionFixture(t *testing.T, s *Store) proxysub.Config {
	t.Helper()
	c := proxysub.DefaultConfig()
	c.URL = "http://127.0.0.1:1/api/repo/synthetic-token.json"
	c, err := s.SaveSubscriptionConfig(c)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func subscriptionCandidate(n int) proxysub.Candidate {
	raw := fmt.Sprintf("http://8.8.8.8:%d", 8000+n)
	return proxysub.Candidate{URL: raw, Key: proxysub.Fingerprint(raw), LastOK: time.Now().UnixMilli(), Streak: 2, ProbedAt: time.Now().UnixMilli()}
}

func subscriptionRun(t *testing.T, s *Store, c proxysub.Config, id string) proxysub.Run {
	t.Helper()
	r := proxysub.Run{ID: id, Revision: c.Revision, Mode: "import", Status: "probing", StartedAt: time.Now().UnixMilli(), Details: []proxysub.Detail{}}
	if err := s.BeginSubscriptionRun(r); err != nil {
		t.Fatal(err)
	}
	r.Status = "completed"
	r.FinishedAt = time.Now().UnixMilli()
	return r
}

func TestSubscriptionImportIdempotentAndIsolated(t *testing.T) {
	s := newTestStore(t)
	c := subscriptionFixture(t, s)
	manual, _ := s.AddProxyProfile("手工", subscriptionCandidate(0).URL, false)
	account, _ := s.AddAccount(model.ProviderZai, "synthetic", "synthetic-token")
	_, _ = s.AssignProxyProfile(account.ID, manual.ID)
	before, _ := json.Marshal(s.ListAccounts(""))
	candidates := []proxysub.Candidate{subscriptionCandidate(0), subscriptionCandidate(1), subscriptionCandidate(1)}
	r := subscriptionRun(t, s, c, "first")
	result, err := s.FinishSubscriptionRun(context.Background(), r, candidates)
	if err != nil || result.Added != 1 {
		t.Fatalf("导入结果 %+v %v", result, err)
	}
	snap, _ := s.SubscriptionSnapshot()
	if snap.OwnedCount() != 1 || len(snap.Profiles) != 2 {
		t.Fatal("来源归属错误")
	}
	for _, p := range s.ListProxyProfiles() {
		if p.Enabled {
			t.Fatal("试运行默认应停用")
		}
	}
	after, _ := json.Marshal(s.ListAccounts(""))
	if string(after) != string(before) {
		t.Fatal("导入改变账号")
	}
	again, err := s.FinishSubscriptionRun(context.Background(), r, candidates)
	if err != nil || again.Added != 1 || len(s.ListProxyProfiles()) != 2 {
		t.Fatal("终态重放不幂等")
	}
	r = subscriptionRun(t, s, c, "second")
	result, err = s.FinishSubscriptionRun(context.Background(), r, candidates)
	if err != nil || result.Added != 0 {
		t.Fatal("重复订阅不应新增")
	}
}

func TestSubscriptionCommitRechecksChanges(t *testing.T) {
	for _, change := range []string{"revision", "manual", "capacity", "cancel", "stale-local-probe", "preview"} {
		t.Run(change, func(t *testing.T) {
			s := newTestStore(t)
			c := subscriptionFixture(t, s)
			if change == "capacity" {
				c.Capacity = 1
				c.ImportLimit = 1
				c, _ = s.SaveSubscriptionConfig(c)
			}
			r := subscriptionRun(t, s, c, "race")
			a := subscriptionCandidate(1)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch change {
			case "revision":
				c.IntervalMinutes++
				_, _ = s.SaveSubscriptionConfig(c)
			case "manual":
				_, _ = s.AddProxyProfile("人工同址", a.URL, false)
			case "capacity":
				other := subscriptionRun(t, s, c, "winner")
				_, _ = s.FinishSubscriptionRun(ctx, other, []proxysub.Candidate{subscriptionCandidate(2)})
			case "cancel":
				cancel()
			case "stale-local-probe":
				a.ProbedAt = time.Now().Add(-3 * time.Hour).UnixMilli()
			case "preview":
				r.Mode = "preview"
			}
			result, err := s.FinishSubscriptionRun(ctx, r, []proxysub.Candidate{a})
			if change == "revision" || change == "cancel" {
				if !errors.Is(err, proxysub.ErrConflict) {
					t.Fatalf("应拒绝迟到提交: %v", err)
				}
			} else if err != nil || result.Added != 0 {
				t.Fatalf("不应导入: %+v %v", result, err)
			}
		})
	}
}

func TestSubscriptionMutationBlocksAndAtomicity(t *testing.T) {
	for _, action := range []string{"manual-delete", "purge", "disable", "url", "rename"} {
		t.Run(action, func(t *testing.T) {
			s := newTestStore(t)
			c := subscriptionFixture(t, s)
			c.EnableImported = true
			c, _ = s.SaveSubscriptionConfig(c)
			a := subscriptionCandidate(1)
			_, err := s.FinishSubscriptionRun(context.Background(), subscriptionRun(t, s, c, "first"), []proxysub.Candidate{a})
			if err != nil {
				t.Fatal(err)
			}
			p := s.ListProxyProfiles()[0]
			switch action {
			case "manual-delete":
				_, _, err = s.DeleteProxyProfile(p.ID)
			case "purge":
				_, _, err = s.PurgeProxyProfiles([]string{p.ID})
			case "disable":
				_, err = s.UpdateProxyProfile(p.ID, p.Name, p.URL, false)
			case "url":
				_, err = s.UpdateProxyProfile(p.ID, p.Name, subscriptionCandidate(2).URL, true)
			case "rename":
				_, err = s.UpdateProxyProfile(p.ID, "人工名称", p.URL, true)
			}
			if err != nil {
				t.Fatal(err)
			}
			snap, _ := s.SubscriptionSnapshot()
			b, ok := snap.State.Blocks[a.Key]
			if action == "rename" {
				if ok || snap.OwnedCount() != 1 {
					t.Fatal("改名不应脱离归属或禁止")
				}
				return
			}
			if !ok || !b.Active(time.Now()) {
				t.Fatal("缺少禁止重入记录")
			}
			if (action == "purge") != (b.Until > 0) {
				t.Fatal("人工和自动 TTL 混淆")
			}
			_, err = s.FinishSubscriptionRun(context.Background(), subscriptionRun(t, s, c, "again"), []proxysub.Candidate{a})
			if err != nil {
				t.Fatal(err)
			}
			if action == "manual-delete" || action == "purge" {
				if len(s.ListProxyProfiles()) != 0 {
					t.Fatal("被删除线路又被导入")
				}
			}
		})
	}
	t.Run("删除失败全回滚", func(t *testing.T) {
		s := newTestStore(t)
		c := subscriptionFixture(t, s)
		_, _ = s.FinishSubscriptionRun(context.Background(), subscriptionRun(t, s, c, "first"), []proxysub.Candidate{subscriptionCandidate(1)})
		p := s.ListProxyProfiles()[0]
		acc, _ := s.AddAccount(model.ProviderZai, "a", "synthetic")
		_, _ = s.AssignProxyProfile(acc.ID, p.ID)
		before, _ := s.SubscriptionSnapshot()
		_, err := s.db.Exec("CREATE TRIGGER reject_subscription_delete BEFORE INSERT ON accounts BEGIN SELECT RAISE(ABORT,'synthetic failure'); END")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = s.DeleteProxyProfile(p.ID); err == nil {
			t.Fatal("应报告写库失败")
		}
		after, _ := s.SubscriptionSnapshot()
		if !reflect.DeepEqual(before, after) || s.FindAny(acc.ID).ProxyID == nil {
			t.Fatal("失败删除发布了部分状态")
		}
	})
}

func TestSubscriptionFinishTransactionAndRecovery(t *testing.T) {
	s := newTestStore(t)
	c := subscriptionFixture(t, s)
	r := subscriptionRun(t, s, c, "atomic")
	before, _ := s.SubscriptionSnapshot()
	_, err := s.db.Exec("CREATE TRIGGER reject_subscription_meta BEFORE INSERT ON meta WHEN NEW.key='proxy_subscription_v1' BEGIN SELECT RAISE(ABORT,'synthetic'); END")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.FinishSubscriptionRun(context.Background(), r, []proxysub.Candidate{subscriptionCandidate(1)}); err == nil {
		t.Fatal("应失败")
	}
	after, _ := s.SubscriptionSnapshot()
	if !reflect.DeepEqual(before, after) {
		t.Fatal("事务失败仍导入")
	}
	_, _ = s.db.Exec("DROP TRIGGER reject_subscription_meta")
	if err = s.RecoverSubscriptionRuns(); err != nil {
		t.Fatal(err)
	}
	snap, _ := s.SubscriptionSnapshot()
	if snap.State.Runs[0].Status != "interrupted" || len(snap.Profiles) != 0 {
		t.Fatal("重启不应恢复导入")
	}
	for i := range 65 {
		_ = s.BeginSubscriptionRun(proxysub.Run{ID: fmt.Sprint(i), Revision: c.Revision, Status: "failed"})
	}
	snap, _ = s.SubscriptionSnapshot()
	if len(snap.State.Runs) != proxysub.MaxHistory {
		t.Fatal("历史无界")
	}
}

func TestSubscriptionPreviewDoesNotCleanBlocks(t *testing.T) {
	s := newTestStore(t)
	c := subscriptionFixture(t, s)
	snap, _ := s.SubscriptionSnapshot()
	a := subscriptionCandidate(1)
	snap.State.Blocks[a.Key] = proxysub.Block{Key: a.Key, Reason: "合成已过期暂缓", Until: time.Now().Add(-time.Hour).UnixMilli()}
	raw, _ := json.Marshal(snap.State)
	if err := s.SetSetting(proxysub.StateKey, string(raw)); err != nil {
		t.Fatal(err)
	}
	r := subscriptionRun(t, s, c, "preview")
	r.Mode = "preview"
	if _, err := s.FinishSubscriptionRun(context.Background(), r, nil); err != nil {
		t.Fatal(err)
	}
	after, _ := s.SubscriptionSnapshot()
	if !reflect.DeepEqual(snap.State.Blocks, after.State.Blocks) {
		t.Fatal("预览不应清理或改变禁止记录")
	}
}

func TestSubscriptionManualBlockPersistsAndSharedTransportSurvives(t *testing.T) {
	s := newTestStore(t)
	c := subscriptionFixture(t, s)
	a := subscriptionCandidate(3)
	if _, err := s.FinishSubscriptionRun(context.Background(), subscriptionRun(t, s, c, "seed"), []proxysub.Candidate{a}); err != nil {
		t.Fatal(err)
	}
	imported := s.ListProxyProfiles()[0]
	manual, err := s.AddProxyProfile("手工同址", a.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := proxy.TransportFor(a.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { proxy.ForgetTransport(a.URL) })
	if _, _, err = s.DeleteProxyProfile(imported.ID); err != nil {
		t.Fatal(err)
	}
	still, _ := proxy.TransportFor(a.URL)
	if tr != still {
		t.Fatal("仍被手工线路引用的传输不应清理")
	}
	if _, _, err = s.DeleteProxyProfile(manual.ID); err != nil {
		t.Fatal(err)
	}
	next, _ := proxy.TransportFor(a.URL)
	if next == tr {
		t.Fatal("最后引用删除后应清理退役缓存")
	}
	reopened, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	snap, err := reopened.SubscriptionSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if block, ok := snap.State.Blocks[a.Key]; !ok || block.Until != 0 {
		t.Fatal("人工重入保护未持久化")
	}
	result, err := reopened.FinishSubscriptionRun(context.Background(), subscriptionRun(t, reopened, c, "reopen"), []proxysub.Candidate{a})
	if err != nil || result.Added != 0 {
		t.Fatal("重启绕过了人工重入保护")
	}
}

func TestSubscriptionExpiredBlockStillRequiresFreshProbe(t *testing.T) {
	s := newTestStore(t)
	c := subscriptionFixture(t, s)
	a := subscriptionCandidate(1)
	snap, _ := s.SubscriptionSnapshot()
	snap.State.Blocks[a.Key] = proxysub.Block{Key: a.Key, Reason: "合成自动淘汰", Until: time.Now().Add(-time.Minute).UnixMilli()}
	raw, _ := json.Marshal(snap.State)
	if err := s.SetSetting(proxysub.StateKey, string(raw)); err != nil {
		t.Fatal(err)
	}
	a.ProbedAt = time.Now().Add(-time.Hour).UnixMilli()
	r, err := s.FinishSubscriptionRun(context.Background(), subscriptionRun(t, s, c, "stale-probe"), []proxysub.Candidate{a})
	if err != nil || r.Added != 0 {
		t.Fatal("暂缓到期不能复用过期本地探测")
	}
	a.ProbedAt = time.Now().UnixMilli()
	r, err = s.FinishSubscriptionRun(context.Background(), subscriptionRun(t, s, c, "fresh-probe"), []proxysub.Candidate{a})
	if err != nil || r.Added != 1 {
		t.Fatalf("新的有效复检应允许入库: %+v %v", r, err)
	}
}

func TestSubscriptionOldHistoryCanCommitWithCurrentProbe(t *testing.T) {
	s := newTestStore(t)
	c := subscriptionFixture(t, s) // 保留 v2.10.2 的 120 分钟配置。
	for i := range 2 {
		a := subscriptionCandidate(10 + i)
		a.LastOK = time.Now().Add(-48 * time.Hour).UnixMilli()
		a.Streak = 1
		if i == 1 {
			a.LastOK = 0
			a.Streak = 0
		}
		r, err := s.FinishSubscriptionRun(context.Background(), subscriptionRun(t, s, c, fmt.Sprint("historical-", i)), []proxysub.Candidate{a})
		if err != nil || r.Added != 1 {
			t.Fatalf("当前本机复检通过后不应再次按外部历史拒绝：%+v %v", r, err)
		}
	}
}
