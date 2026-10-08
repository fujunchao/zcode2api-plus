package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"zcode2api/internal/model"
	"zcode2api/internal/proxy"
)

// 使用已有真实 HTTP 代理夹具，不把测试凭据或运行数据交给外部服务。
func existingProxyFailureAccounts(t *testing.T, f *newAccountFixture) []*model.Account {
	t.Helper()
	var accounts []*model.Account
	for _, name := range []string{"existing-fault", "existing-peer"} {
		a, err := f.h.Store.AddAccount(model.ProviderZai, name, jwtTokenFor(name))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.h.Store.AssignProxyProfile(a.ID, f.bad.ID); err != nil {
			t.Fatal(err)
		}
		_, err = f.h.Store.Update(a.Provider, a.ID, func(live *model.Account) {
			live.UseCount = 8
			live.Plans = []map[string]any{{"plan_id": "existing-plan"}}
		})
		if err != nil {
			t.Fatal(err)
		}
		accounts = append(accounts, f.h.Store.FindAny(a.ID))
	}
	return accounts
}

func callProxyEndpoint(t *testing.T, f *newAccountFixture, account *model.Account, endpoint string) map[string]any {
	t.Helper()
	if endpoint == "quota" {
		return f.h.Quota.FetchQuota(account)
	}
	mux := http.NewServeMux()
	f.h.Register(mux)
	path, method := "/admin/api/claim", http.MethodPost
	var payload any = map[string]any{"account_ids": []string{account.ID}, "plan_id": "promo"}
	if endpoint == "preview" {
		path, method, payload = "/admin/api/claim/preview?account_id="+account.ID, http.MethodGet, nil
	}
	code, body := do(t, mux, f.h.Store, method, path, payload)
	if code != http.StatusOK {
		t.Fatalf("接口应返回逐账号处理结果：%d %v", code, body)
	}
	return body
}

func TestProxyEndpointBrokenResponses(t *testing.T) {
	for _, endpoint := range []string{"quota", "claim", "preview"} {
		for _, failure := range []string{"truncated", "proxy-auth", "timeout"} {
			t.Run(endpoint+"/"+failure, func(t *testing.T) {
				f := newAccountQuotaFixture(t, newAccountAllocatedBalance, newAccountAllocatedBalance)
				broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch failure {
					case "truncated":
						w.Header().Set("Content-Length", "128")
						w.WriteHeader(http.StatusOK)
						_, _ = w.Write([]byte(`{`))
					case "proxy-auth":
						w.WriteHeader(http.StatusProxyAuthRequired)
					case "timeout":
						select {
						case <-r.Context().Done():
						case <-time.After(time.Second):
						}
					}
				}))
				t.Cleanup(broken.Close)
				var err error
				f.bad, err = f.h.Store.UpdateProxyProfile(f.bad.ID, f.bad.Name, broken.URL, true)
				if err != nil {
					t.Fatal(err)
				}
				if failure == "timeout" {
					// 仅调整该测试独有代理的 Transport；仍使用真实代理客户端，而非注入绕行客户端。
					transport, err := proxy.TransportFor(f.bad.URL)
					if err != nil {
						t.Fatal(err)
					}
					previous := transport.ResponseHeaderTimeout
					transport.ResponseHeaderTimeout = 100 * time.Millisecond
					t.Cleanup(func() {
						f.h.Close()
						f.h.Quota.Close()
						transport.CloseIdleConnections()
						transport.ResponseHeaderTimeout = previous
					})
				}
				accounts := existingProxyFailureAccounts(t, f)
				callProxyEndpoint(t, f, accounts[0], endpoint)
				assertFaultProxyReassigned(t, f, accounts)
			})
		}
	}
}

func TestProxyEndpointOrdinaryResponsesKeepProxy(t *testing.T) {
	for _, endpoint := range []string{"quota", "claim", "preview"} {
		for _, tc := range []struct {
			name   string
			status int
			body   string
		}{
			{"unauthorized", 401, `{"code":401,"msg":"unauthorized"}`},
			{"forbidden", 403, `{"code":401,"msg":"unauthorized"}`},
			{"rate-limit", 429, `{"code":429,"msg":"too many requests"}`},
			{"upstream-503", 503, `{"code":503,"msg":"service unavailable"}`},
			{"ordinary-405", 405, `already queried`},
			{"invalid-json", 200, `invalid JSON`},
			{"already-claimed", 200, `{"code":1003,"msg":"suspicious request"}`},
			{"quota-exhausted", 200, `{"code":1005,"msg":"request blocked"}`},
			{"captcha", 400, `{"code":3007,"msg":"captcha blocked"}`},
			{"forged-metadata", 200, `{"code":1003,"proxy_failure":"claim_suspicious","risk_control":true}`},
		} {
			t.Run(endpoint+"/"+tc.name, func(t *testing.T) {
				f := newAccountQuotaFixture(t, newAccountAllocatedBalance, newAccountAllocatedBalance)
				accounts := existingProxyFailureAccounts(t, f)
				f.mu.Lock()
				f.before, f.beforeStatus = tc.body, tc.status
				f.claimBody, f.claimStatus = tc.body, tc.status
				f.preview = tc.body
				f.mu.Unlock()
				callProxyEndpoint(t, f, accounts[0], endpoint)
				if len(f.h.Store.ListProxyProfiles()) != 2 {
					t.Fatal("普通 HTTP/业务失败不能作为删除代理的证据")
				}
			})
		}
	}
}

func TestProxyEndpointOldEmptyQuotaKeepsProxy(t *testing.T) {
	f := newAccountQuotaFixture(t, newAccountEmptyBalance, newAccountEmptyBalance)
	accounts := existingProxyFailureAccounts(t, f)
	f.h.Quota.FetchQuota(accounts[0])
	f.h.Quota.FetchQuotaFresh(context.Background(), accounts[0])
	if len(f.h.Store.ListProxyProfiles()) != 2 {
		t.Fatal("老账号两次正常返回空额度仍不能删除代理")
	}
}

func TestProxyEndpointSuspiciousAfterPartialSuccessStillPurges(t *testing.T) {
	f := newAccountQuotaFixture(t, newAccountAllocatedBalance, newAccountAllocatedBalance)
	accounts := existingProxyFailureAccounts(t, f)
	f.mu.Lock()
	f.onRequest = func(_ *http.Request, request newAccountRequest) {
		if request.plan == "promo" {
			f.mu.Lock()
			f.claimStatus, f.claimBody = http.StatusForbidden, `{"code":3012,"msg":"suspicious request"}`
			f.mu.Unlock()
		}
	}
	f.mu.Unlock()
	if !claimSlot.acquire() {
		t.Fatal("测试领取槽位被占用")
	}
	if !f.h.claimUnderGateContext(context.Background(), accounts[0]) {
		t.Fatal("本轮第一个套餐应已成功领取")
	}
	assertFaultProxyReassigned(t, f, accounts)
	if state := f.h.Store.FindAny(accounts[0].ID).ClaimView(); state == nil || state.ClaimedAt == nil || state.NextAt == nil {
		t.Fatal("淘汰不能丢失已成功领取的记录")
	}
}

func TestProxyEndpointBatchPreviewUsesReassignedProxy(t *testing.T) {
	f := newAccountQuotaFixture(t, newAccountAllocatedBalance, newAccountAllocatedBalance)
	accounts := existingProxyFailureAccounts(t, f)
	f.mu.Lock()
	f.preview = `{"code":3012,"msg":"suspicious request"}`
	f.onRequest = func(_ *http.Request, request newAccountRequest) {
		if request.path == "/billing/preview" && request.account == accounts[0].Secret() {
			f.mu.Lock()
			f.preview = `{"code":0,"data":{"plans":[]}}`
			f.mu.Unlock()
		}
	}
	f.mu.Unlock()
	mux := http.NewServeMux()
	f.h.Register(mux)
	code, _ := do(t, mux, f.h.Store, http.MethodGet, "/admin/api/claim/preview", nil)
	if code != http.StatusOK {
		t.Fatal("预览失败")
	}
	assertFaultProxyReassigned(t, f, accounts)
	for _, request := range f.accountRequests(accounts[1].Secret()) {
		if request.line != "good" {
			t.Fatal("后续账号必须立即使用改派后的代理，不能沿用批量旧快照")
		}
	}
}

func TestProxyEndpointCancellationKeepsProxy(t *testing.T) {
	for _, endpoint := range []string{"quota", "claim", "preview"} {
		t.Run(endpoint, func(t *testing.T) {
			f := newAccountQuotaFixture(t, newAccountAllocatedBalance, newAccountAllocatedBalance)
			entered, release := make(chan struct{}), make(chan struct{})
			defer close(release)
			blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.Contains(r.URL.Path, "/billing/") {
					_, _ = w.Write([]byte(`{"code":0}`))
					return
				}
				w.Header().Set("Content-Length", "128")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{`))
				w.(http.Flusher).Flush()
				close(entered)
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			t.Cleanup(blocked.Close)
			var err error
			f.bad, err = f.h.Store.UpdateProxyProfile(f.bad.ID, f.bad.Name, blocked.URL, true)
			if err != nil {
				t.Fatal(err)
			}
			accounts := existingProxyFailureAccounts(t, f)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				if endpoint == "quota" {
					_, _ = f.h.Quota.FetchQuotaFresh(ctx, accounts[0])
					return
				}
				callProxyEndpoint(t, f, accounts[0], endpoint)
			}()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("请求未开始读取响应")
			}
			if endpoint == "quota" {
				cancel()
			} else {
				f.h.Close()
			}
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("取消后请求未退出")
			}
			if len(f.h.Store.ListProxyProfiles()) != 2 {
				t.Fatal("取消引起的响应中断不能删除代理")
			}
		})
	}
}

func TestProxyEndpointLateFailureKeepsChangedProxy(t *testing.T) {
	for _, endpoint := range []string{"quota", "claim", "preview"} {
		t.Run(endpoint, func(t *testing.T) {
			f := newAccountQuotaFixture(t, newAccountAllocatedBalance, newAccountAllocatedBalance)
			entered, release := make(chan struct{}), make(chan struct{})
			var enteredOnce, releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.Contains(r.URL.Path, "/billing/") {
					_, _ = w.Write([]byte(`{"code":0}`))
					return
				}
				enteredOnce.Do(func() { close(entered) })
				<-release
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					_ = conn.Close()
				}
			}))
			t.Cleanup(blocked.Close)
			var err error
			f.bad, err = f.h.Store.UpdateProxyProfile(f.bad.ID, f.bad.Name, blocked.URL, true)
			if err != nil {
				t.Fatal(err)
			}
			accounts := existingProxyFailureAccounts(t, f)
			done := make(chan struct{})
			go func() { defer close(done); callProxyEndpoint(t, f, accounts[0], endpoint) }()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("请求未开始")
			}
			if _, err := f.h.Store.AssignProxyProfile(accounts[0].ID, f.good.ID); err != nil {
				t.Fatal(err)
			}
			releaseOnce.Do(func() { close(release) })
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("请求未结束")
			}
			if len(f.h.Store.ListProxyProfiles()) != 2 || *f.h.Store.FindAny(accounts[0].ID).ProxyID != f.good.ID {
				t.Fatal("迟到失败不得删除修改后的代理")
			}
		})
	}
}

func assertFaultProxyReassigned(t *testing.T, f *newAccountFixture, accounts []*model.Account) {
	t.Helper()
	if len(f.h.Store.ListProxyProfiles()) != 1 {
		t.Fatal("已确认无法访问端点或领取可疑请求的代理，应立即从池中删除")
	}
	for _, a := range accounts {
		got := f.h.Store.FindAny(a.ID)
		if got.ProxyID == nil || *got.ProxyID != f.good.ID || got.ProxyURL == nil || *got.ProxyURL != f.good.URL || got.UseCount != 8 {
			t.Fatal("故障代理的全部关联账号应按现有策略改派，保留使用记录")
		}
	}
}

func disconnectProxy(t *testing.T, f *newAccountFixture) {
	t.Helper()
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	t.Cleanup(dead.Close)
	p, err := f.h.Store.UpdateProxyProfile(f.bad.ID, f.bad.Name, dead.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	f.bad = p
}

func TestProxyEndpointQuotaFailureReassignsAllBindings(t *testing.T) {
	f := newAccountQuotaFixture(t, newAccountAllocatedBalance, newAccountAllocatedBalance)
	disconnectProxy(t, f)
	accounts := existingProxyFailureAccounts(t, f)
	result := f.h.Quota.FetchQuota(accounts[0])
	if result["error"] == nil {
		t.Fatal("断连必须返回查询失败")
	}
	assertFaultProxyReassigned(t, f, accounts)
	if result["proxy_removed"] != f.bad.ID {
		t.Fatal("额度结果应明确回报原代理已被删除")
	}
}

func TestProxyEndpointClaimFailureReassignsAllBindings(t *testing.T) {
	f := newAccountQuotaFixture(t, newAccountAllocatedBalance, newAccountAllocatedBalance)
	disconnectProxy(t, f)
	accounts := existingProxyFailureAccounts(t, f)
	mux := http.NewServeMux()
	f.h.Register(mux)
	code, body := do(t, mux, f.h.Store, http.MethodPost, "/admin/api/claim", map[string]any{
		"account_ids": []string{accounts[0].ID}, "plan_id": "promo",
	})
	if code != http.StatusOK {
		t.Fatalf("领取请求应保留逐账号结果：%d %v", code, body)
	}
	assertFaultProxyReassigned(t, f, accounts)
	data, _ := json.Marshal(body)
	if !strings.Contains(string(data), "proxy_removed") {
		t.Fatal("领取结果应带上代理删除信息")
	}
}

func TestProxyEndpointSuspiciousClaimReassignsExistingAccounts(t *testing.T) {
	for _, mode := range []string{"manual", "auto", "preview"} {
		t.Run(mode, func(t *testing.T) {
			f := newAccountQuotaFixture(t, newAccountAllocatedBalance, newAccountAllocatedBalance)
			accounts := existingProxyFailureAccounts(t, f)
			f.mu.Lock()
			f.claimStatus, f.claimBody = http.StatusForbidden, `{"code":3012,"msg":"Suspicious request detected"}`
			if mode == "preview" {
				f.preview = `{"code":3012,"msg":"检测到可疑的请求"}`
			}
			f.mu.Unlock()
			mux := http.NewServeMux()
			f.h.Register(mux)
			switch mode {
			case "auto":
				if !claimSlot.acquire() {
					t.Fatal("测试领取槽位被占用")
				}
				f.h.claimUnderGateContext(context.Background(), accounts[0])
			case "preview":
				code, _ := do(t, mux, f.h.Store, http.MethodGet, "/admin/api/claim/preview?account_id="+accounts[0].ID, nil)
				if code != http.StatusOK {
					t.Fatal("预览接口应保留逐账号结果")
				}
			default:
				code, _ := do(t, mux, f.h.Store, http.MethodPost, "/admin/api/claim", map[string]any{
					"account_ids": []string{accounts[0].ID}, "plan_id": "promo",
				})
				if code != http.StatusOK {
					t.Fatal("领取接口应保留逐账号结果")
				}
			}
			assertFaultProxyReassigned(t, f, accounts)
		})
	}
}
