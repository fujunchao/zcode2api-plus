package store

import (
	"fmt"
	"testing"
	"time"
)

// 从真实日志缩小得到：第一次 A → B 后，B 风控不能再因为 A 空闲而选回 A。
func TestRiskProxyHistoryAvoidsRecentFailedRoutes(t *testing.T) {
	s := newTestStore(t)
	a := addSelectionProxy(t, s, "A", 18080, true)
	b := addSelectionProxy(t, s, "B", 18081, true)
	c := addSelectionProxy(t, s, "C", 18082, true)
	acc := addSelectionAccount(t, s, "risk", a.ID)
	for _, want := range []string{b.ID, c.ID} {
		next, changed, err := s.RotateAccountProxy(s.FindAny(acc.ID))
		if err != nil || !changed || next.ID != want {
			t.Fatalf("应避让近期失败线路：want=%s got=%s changed=%v err=%v", want, next.ID, changed, err)
		}
	}
	if _, changed, err := s.RotateAccountProxy(s.FindAny(acc.ID)); err != nil || changed {
		t.Fatalf("候选耗尽时应保留当前绑定，不得循环旧线路：changed=%v err=%v", changed, err)
	}
}

func TestRiskProxyHistorySurvivesRestartAndExpires(t *testing.T) {
	s := newTestStore(t)
	a := addSelectionProxy(t, s, "A", 18080, true)
	b := addSelectionProxy(t, s, "B", 18081, true)
	c := addSelectionProxy(t, s, "C", 18082, true)
	acc := addSelectionAccount(t, s, "risk", a.ID)
	now := time.Now().Truncate(time.Second)
	if r, err := s.RotateAccountProxyAt(s.FindAny(acc.ID), now); err != nil || r.Next.ID != b.ID {
		t.Fatalf("首次改派失败：%+v %v", r, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, exposed := reopened.GetSetting(riskProxyHistoryKey); exposed {
		t.Fatal("内部线路历史不应暴露为设置")
	}
	if r, err := reopened.RotateAccountProxyAt(reopened.FindAny(acc.ID), now.Add(time.Minute)); err != nil || r.Next.ID != c.ID {
		t.Fatalf("重启后仍应避让 A：%+v %v", r, err)
	}
	if r, err := reopened.RotateAccountProxyAt(reopened.FindAny(acc.ID), now.Add(24*time.Hour)); err != nil || r.Next.ID != a.ID {
		t.Fatalf("24 小时到期后 A 可重新选择，B 尚未到期：%+v %v", r, err)
	}
}

func TestRiskProxyHistoryIsAccountScopedAndExcludesURLAliases(t *testing.T) {
	s := newTestStore(t)
	a := addSelectionProxy(t, s, "A", 18080, true)
	b := addSelectionProxy(t, s, "B", 18081, true)
	addSelectionProxy(t, s, "A-alias", 18080, true)
	c := addSelectionProxy(t, s, "C", 18082, true)
	acc := addSelectionAccount(t, s, "risk", a.ID)
	for _, want := range []string{b.ID, c.ID} {
		r, err := s.RotateAccountProxyAt(s.FindAny(acc.ID), time.Now())
		if err != nil || r.Next.ID != want {
			t.Fatalf("同 URL 别名不得绕过历史：%+v %v", r, err)
		}
	}
	peer := addSelectionAccount(t, s, "peer", c.ID)
	if r, err := s.RotateAccountProxyAt(peer, time.Now()); err != nil || r.Next.ID != a.ID {
		t.Fatalf("不应影响其他账号选择 A：%+v %v", r, err)
	}
}

func TestRiskProxyHistoryRecordsNoCandidateAndProtectsStaleSnapshot(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(fmt.Sprint(stale), func(t *testing.T) {
			s := newTestStore(t)
			a := addSelectionProxy(t, s, "A", 18080, true)
			acc := addSelectionAccount(t, s, "risk", a.ID)
			if !stale {
				if r, err := s.RotateAccountProxyAt(acc, time.Now()); err != nil || r.Reason != "no_alternative" {
					t.Fatalf("应明确无候选：%+v %v", r, err)
				}
			}
			b := addSelectionProxy(t, s, "B", 18081, true)
			_, _ = s.AssignProxyProfile(acc.ID, b.ID)
			if stale {
				if r, err := s.RotateAccountProxyAt(acc, time.Now()); err != nil || r.Reason != "stale_snapshot" {
					t.Fatalf("旧快照不能写历史或改派：%+v %v", r, err)
				}
			}
			r, err := s.RotateAccountProxyAt(s.FindAny(acc.ID), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if stale && r.Next.ID != a.ID {
				t.Fatal("旧响应不应把 A 加入避让历史")
			}
			if !stale && r.Reason != "recent_routes_exhausted" {
				t.Fatalf("无候选时记录的 A 仍须被避让：%+v", r)
			}
		})
	}
}

func TestRiskProxyHistoryTransactionFailureDoesNotPublish(t *testing.T) {
	for _, reopen := range []bool{false, true} {
		t.Run(fmt.Sprint(reopen), func(t *testing.T) {
			s := newTestStore(t)
			a := addSelectionProxy(t, s, "A", 18080, true)
			b := addSelectionProxy(t, s, "B", 18081, true)
			acc := addSelectionAccount(t, s, "risk", a.ID)
			if _, err := s.db.Exec("CREATE TRIGGER reject_risk_account BEFORE INSERT ON accounts BEGIN SELECT RAISE(ABORT,'test failure'); END"); err != nil {
				t.Fatal(err)
			}
			if r, err := s.RotateAccountProxyAt(acc, time.Now()); err == nil || r.Reason != "persist_failed" {
				t.Fatalf("必须报告事务失败：%+v %v", r, err)
			}
			if *s.FindAny(acc.ID).ProxyID != a.ID {
				t.Fatal("失败后绑定被改写")
			}
			if _, err := s.db.Exec("DROP TRIGGER reject_risk_account"); err != nil {
				t.Fatal(err)
			}
			if reopen {
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				var err error
				s, err = New()
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
			}
			_, _ = s.AssignProxyProfile(acc.ID, b.ID)
			if r, err := s.RotateAccountProxyAt(s.FindAny(acc.ID), time.Now()); err != nil || r.Next.ID != a.ID {
				t.Fatalf("失败事务不能遗留 A 的历史：%+v %v", r, err)
			}
		})
	}
}
