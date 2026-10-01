package adminapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"zcode2api/internal/captcha"
	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/quota"
	"zcode2api/internal/store"
)

type claimGuardFixture struct {
	h                    *Handler
	a                    *model.Account
	bad, good            store.ProxyProfile
	oldClaims, newClaims atomic.Int32
}

func newClaimGuardFixture(t *testing.T, balanceStatus int, balanceBody string) *claimGuardFixture {
	t.Helper()
	f := &claimGuardFixture{}
	st := newClaimStore(t)
	handler := func(calls *atomic.Int32) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if strings.HasSuffix(r.URL.Path, "/billing/balance") {
				w.WriteHeader(balanceStatus)
				_, _ = w.Write([]byte(balanceBody))
				return
			}
			if strings.HasSuffix(r.URL.Path, "/billing/preview") {
				_, _ = w.Write([]byte(`{"code":0,"data":{"plans":[{"plan_id":"start-plan","priority":2},{"plan_id":"other","priority":1}]}}`))
				return
			}
			calls.Add(1)
			w.WriteHeader(405)
			_, _ = w.Write([]byte(`{"code":3012,"msg":"request has been blocked due to unusual activity."}`))
		}
	}
	up := httptest.NewServer(handler(&f.oldClaims))
	t.Cleanup(up.Close)
	other := httptest.NewServer(handler(&f.newClaims))
	t.Cleanup(other.Close)
	oldBase := config.ZcodeBillingBase
	config.ZcodeBillingBase = up.URL
	t.Cleanup(func() { config.ZcodeBillingBase = oldBase })
	f.bad, _ = st.AddProxyProfile("bad", up.URL, true)
	f.good, _ = st.AddProxyProfile("good", other.URL, true)
	a, _ := st.AddAccount(model.ProviderZai, "new-account", "header.payload.signature")
	_, _ = st.AssignProxyProfile(a.ID, f.bad.ID)
	qs := quota.NewService(st)
	t.Cleanup(qs.Close)
	cm := captcha.NewManager()
	if err := cm.SetManualParam("test-captcha", "cn"); err != nil {
		t.Fatal(err)
	}
	f.h = &Handler{Store: st, Quota: qs, Captcha: cm}
	f.a = st.FindAny(a.ID)
	return f
}

func (f *claimGuardFixture) manual() *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	f.h.handleClaim(w, httptest.NewRequest(http.MethodPost, "/admin/api/claim", strings.NewReader(`{"plan_id":"start-plan"}`)))
	return w
}

// 重现日志中的组合信号，覆盖手动和入池/定时共用的自动领取入口。
func TestMissingStartPlanClaimRiskPurgesProxy(t *testing.T) {
	for _, mode := range []string{"manual", "auto"} {
		t.Run(mode, func(t *testing.T) {
			f := newClaimGuardFixture(t, 200, `{"code":0,"data":{"plans":[],"balances":[]}}`)
			f.h.Quota.FetchQuota(f.a)
			if mode == "manual" {
				w := f.manual()
				if w.Code != 200 || !strings.Contains(w.Body.String(), "unusual activity") || !strings.Contains(w.Body.String(), "proxy_removed") {
					t.Fatalf("应返回风控与代理移除信息: %d %s", w.Code, w.Body.String())
				}
			} else {
				if !claimSlot.acquire() {
					t.Fatal("测试闸门不应被占用")
				}
				if f.h.claimUnderGateContext(context.Background(), f.a) {
					t.Fatal("风控不得报告领取成功")
				}
			}
			for _, p := range f.h.Store.ListProxyProfiles() {
				if p.ID == f.bad.ID {
					t.Fatal("无 Start plan 且领取风控后，问题代理仍留在代理池")
				}
			}
			got := f.h.Store.FindAny(f.a.ID)
			if got.ProxyID == nil || *got.ProxyID != f.good.ID || got.ProxyURL == nil || *got.ProxyURL != f.good.URL {
				t.Fatal("问题代理移除后，应自动分配可用代理")
			}
			if got.Claim == nil || got.Claim.NextAt == nil || *got.Claim.NextAt <= float64(time.Now().Unix()) {
				t.Fatal("换线应保留领取冷却")
			}
			if f.oldClaims.Load() != 1 || f.newClaims.Load() != 0 {
				t.Fatal("本次不得在新线路立即重领")
			}
		})
	}
}

func TestClaimRiskWithoutInitialQuotaEvidenceKeepsProxy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		query  bool
	}{
		{"尚未查询", 200, `{"code":0,"data":{"plans":[],"balances":[]}}`, false},
		{"查询失败", 500, `{}`, true},
		{"缺字段", 200, `{"code":0,"data":{}}`, true},
		{"已有套餐且用尽", 200, `{"code":0,"data":{"plans":[{"plan_id":"start-plan"}],"balances":[{"total_units":100,"used_units":100,"remaining_units":0}]}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newClaimGuardFixture(t, tc.status, tc.body)
			if tc.query {
				f.h.Quota.FetchQuota(f.a)
			}
			w := f.manual()
			if w.Code != 200 || strings.Contains(w.Body.String(), "proxy_removed") || len(f.h.Store.ListProxyProfiles()) != 2 {
				t.Fatalf("无确切证据不能淘汰代理: %s", w.Body.String())
			}
		})
	}
}

func TestBatchClaimUsesReassignedProxy(t *testing.T) {
	f := newClaimGuardFixture(t, 200, `{"code":0,"data":{"plans":[],"balances":[]}}`)
	peer, _ := f.h.Store.AddAccount(model.ProviderZai, "peer", "header.payload.other")
	_, _ = f.h.Store.AssignProxyProfile(peer.ID, f.bad.ID)
	for _, a := range f.h.Store.ListAccounts("") {
		f.h.Quota.FetchQuota(a)
	}
	w := f.manual()
	if w.Code != 200 || f.oldClaims.Load() != 1 || f.newClaims.Load() != 1 {
		t.Fatalf("批量后续账号应立即使用已改派的出口: old=%d new=%d response=%s", f.oldClaims.Load(), f.newClaims.Load(), w.Body.String())
	}
	if len(f.h.Store.ListProxyProfiles()) != 1 {
		t.Fatal("不能用原线路的空额度证据继续删除新代理")
	}
}
