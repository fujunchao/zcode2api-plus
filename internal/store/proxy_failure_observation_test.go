package store

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"zcode2api/internal/model"
	"zcode2api/internal/proxy"
)

func TestProxyEndpointObservationCoalescesAndRateLimits(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "共享出口", 18080, true)
	a := addSelectionAccount(t, s, "账号", p.ID)
	route, now := s.ProxyEgress(a), time.Now()
	beforeAccounts, beforeProfiles := s.ListAccounts(""), s.ListProxyProfiles()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := s.ObserveProxyEndpointFailure(context.Background(), a, proxy.FailureEndpointUnreachable, now, route); got != "recheck_pending" {
				t.Errorf("有效异常应合并为待复核：%s", got)
			}
		}()
	}
	wg.Wait()
	if !reflect.DeepEqual(beforeAccounts, s.ListAccounts("")) || !reflect.DeepEqual(beforeProfiles, s.ListProxyProfiles()) {
		t.Fatal("异常通知不得修改账号或代理配置")
	}
	policy := s.ModelProbeSettings()
	if pending := s.TakeProxyNetworkRechecks(policy, now); len(pending) != 1 || pending[0].ID != p.ID {
		t.Fatalf("共享异常应只消费一个独立网络复核：%v", pending)
	}
	if pending := s.TakeProxyNetworkRechecks(policy, now); len(pending) != 0 {
		t.Fatal("同一通知不能被重复消费")
	}
	if got := s.ObserveProxyEndpointFailure(context.Background(), a, proxy.FailureClaimSuspicious, now.Add(time.Minute), route); got != "rate_limited" {
		t.Fatalf("不同计费入口应共享最短复核间隔：%s", got)
	}
	later := now.Add(5 * time.Minute)
	if got := s.ObserveProxyEndpointFailure(context.Background(), a, proxy.FailureEndpointUnreachable, later, route); got != "recheck_pending" {
		t.Fatalf("持续故障在退避后可重新复核：%s", got)
	}
	if len(s.TakeProxyNetworkRechecks(policy, later)) != 1 {
		t.Fatal("退避结束后的新异常不能被旧通知吞掉")
	}
}

func TestProxyEndpointObservationRejectsStaleIdentity(t *testing.T) {
	for _, when := range []string{"before_report", "before_consume"} {
		for _, change := range []string{"binding", "url_aba", "credential_aba", "enabled_aba", "protection_aba", "deleted", "policy"} {
			t.Run(when+"/"+change, func(t *testing.T) {
				s := newTestStore(t)
				p := addSelectionProxy(t, s, "原出口", 18080, true)
				other := addSelectionProxy(t, s, "另一出口", 18081, true)
				a := addSelectionAccount(t, s, "账号", p.ID)
				route, now := s.ProxyEgress(a), time.Now()
				if when == "before_consume" {
					s.ObserveProxyEndpointFailure(context.Background(), a, proxy.FailureEndpointUnreachable, now, route)
				}
				switch change {
				case "binding":
					_, _ = s.AssignProxyProfile(a.ID, other.ID)
				case "url_aba":
					_, _ = s.UpdateProxyProfile(p.ID, p.Name, other.URL, true)
					_, _ = s.UpdateProxyProfile(p.ID, p.Name, p.URL, true)
				case "credential_aba":
					_, _ = s.Update(a.Provider, a.ID, func(live *model.Account) { value := "replacement"; live.JWTToken = &value })
					_, _ = s.Update(a.Provider, a.ID, func(live *model.Account) { live.JWTToken = a.JWTToken })
				case "enabled_aba":
					_, _ = s.UpdateProxyProfile(p.ID, p.Name, p.URL, false)
					_, _ = s.UpdateProxyProfile(p.ID, p.Name, p.URL, true)
				case "protection_aba":
					_, _ = s.UpdateProxyProfile(p.ID, p.Name, p.URL, true, true)
					_, _ = s.UpdateProxyProfile(p.ID, p.Name, p.URL, true, false)
				case "deleted":
					_, _ = s.RemoveAccount(a.Provider, a.ID)
				case "policy":
					policy := s.ModelProbeSettings()
					policy.NetworkEnabled = false
					if _, err := s.SaveModelProbeSettings(policy, now); err != nil {
						t.Fatal(err)
					}
				}
				if when == "before_report" {
					if got := s.ObserveProxyEndpointFailure(context.Background(), a, proxy.FailureEndpointUnreachable, now, route); got == "recheck_pending" {
						t.Fatal("迟到身份或关闭的巡检不应产生新通知")
					}
				}
				if len(s.TakeProxyNetworkRechecks(s.ModelProbeSettings(), now)) != 0 {
					t.Fatal("消费前已过期的请求身份不能触发新检查")
				}
				if len(s.ListProxyProfiles()) != 2 {
					t.Fatal("任何异常通知都不得删除代理")
				}
			})
		}
	}
}

func TestProxyEndpointObservationCancellationAndExpiry(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "出口", 18080, true)
	a := addSelectionAccount(t, s, "账号", p.ID)
	route, now := s.ProxyEgress(a), time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := s.ObserveProxyEndpointFailure(ctx, a, proxy.FailureEndpointUnreachable, now, route); got != "ignored" {
		t.Fatal("取消不是代理故障")
	}
	if got := s.ObserveProxyEndpointFailure(context.Background(), a, "ordinary_http_error", now, route); got != "ignored" {
		t.Fatal("普通 HTTP 错误不能提交代理故障通知")
	}
	if len(s.TakeProxyNetworkRechecks(s.ModelProbeSettings(), now)) != 0 {
		t.Fatal("取消或普通业务错误不得触发复核")
	}
	s.ObserveProxyEndpointFailure(context.Background(), a, proxy.FailureEndpointUnreachable, now, route)
	if len(s.TakeProxyNetworkRechecks(s.ModelProbeSettings(), now.Add(10*time.Minute))) != 0 {
		t.Fatal("超过有效期的通知不能触发检查")
	}
	s.ObserveProxyEndpointFailure(context.Background(), a, proxy.FailureEndpointUnreachable, now, route)
	reopened, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if len(reopened.TakeProxyNetworkRechecks(reopened.ModelProbeSettings(), now)) != 0 {
		t.Fatal("重启不得重建旧计费失败通知")
	}
}
