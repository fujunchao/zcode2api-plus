package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zcode2api/internal/model"
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
