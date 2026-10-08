package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"zcode2api/internal/auth"
	"zcode2api/internal/captcha"
	"zcode2api/internal/claim"
	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/oauth"
	"zcode2api/internal/quota"
	"zcode2api/internal/store"
)

const newAccountEmptyBalance = `{"code":0,"data":{"plans":[],"balances":[{"model":"test-model","total_units":0,"used_units":0,"remaining_units":0,"available_units":0}]}}`
const newAccountAllocatedBalance = `{"code":0,"data":{"plans":[{"plan_id":"trial","entitlements":[]}],"balances":[{"model":"test-model","total_units":150,"used_units":0,"remaining_units":150,"available_units":150}]}}`

type newAccountRequest struct {
	line, account, path, plan string
}

// 两个本地服务器同时充当代理和上游，所有凭据、验证码和余额均为合成数据。
type newAccountFixture struct {
	h         *Handler
	bad, good store.ProxyProfile
	mu        sync.Mutex
	requests  []newAccountRequest
	balances  map[string]int
	before    string
	after     string
}

func newAccountQuotaFixture(t *testing.T, before, after string) *newAccountFixture {
	t.Helper()
	st := newClaimStore(t)
	f := &newAccountFixture{before: before, after: after, balances: map[string]int{}}
	handler := func(line string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			account := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			plan := ""
			if strings.HasSuffix(r.URL.Path, "/billing/claim") {
				var body map[string]string
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				plan = body["plan_id"]
			}
			f.mu.Lock()
			f.requests = append(f.requests, newAccountRequest{line, account, r.URL.Path, plan})
			body := `{"code":0,"data":{}}`
			switch {
			case strings.HasSuffix(r.URL.Path, "/billing/balance"):
				f.balances[account]++
				body = f.before
				if f.balances[account] > 1 {
					body = f.after
				}
			case strings.HasSuffix(r.URL.Path, "/billing/preview"):
				body = `{"code":0,"data":{"plans":[{"plan_id":"trial","priority":1},{"plan_id":"promo","priority":2}]}}`
			}
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}
	}
	bad := httptest.NewServer(handler("bad"))
	t.Cleanup(bad.Close)
	good := httptest.NewServer(handler("good"))
	t.Cleanup(good.Close)
	oldBase, oldReport := config.ZcodeBillingBase, claim.EventReportURL
	config.ZcodeBillingBase, claim.EventReportURL = bad.URL, bad.URL+"/event/report"
	t.Cleanup(func() { config.ZcodeBillingBase, claim.EventReportURL = oldBase, oldReport })
	var err error
	if f.bad, err = st.AddProxyProfile("初始代理", bad.URL, true); err != nil {
		t.Fatal(err)
	}
	if f.good, err = st.AddProxyProfile("备用代理", good.URL, true); err != nil {
		t.Fatal(err)
	}
	cm := captcha.NewManager()
	cm.SetConfigProvider(func(context.Context) (captcha.Config, error) { return captcha.DefaultConfig, nil })
	if err := cm.SetManualParam("synthetic-captcha", "cn"); err != nil {
		t.Fatal(err)
	}
	qs := quota.NewService(st)
	t.Cleanup(qs.Close)
	f.h = New(st, auth.New(st), cm, qs)
	t.Cleanup(f.h.Close)
	return f
}

func (f *newAccountFixture) login(t *testing.T, name string) *model.Account {
	t.Helper()
	a, err := f.h.saveOAuthAccount(&oauth.ExchangeResult{Token: jwtTokenFor(name)},
		&loginSession{proxyID: f.bad.ID, proxyURL: f.bad.URL, auto: true})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (f *newAccountFixture) wait(t *testing.T) {
	t.Helper()
	done := make(chan struct{})
	go func() { f.h.autoTasks.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("新账号领取收尾未完成")
	}
}

func (f *newAccountFixture) accountRequests(account string) []newAccountRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []newAccountRequest
	for _, r := range f.requests {
		if r.account == account {
			out = append(out, r)
		}
	}
	return out
}

func TestNewAccountRefreshesQuotaAfterAllClaims(t *testing.T) {
	f := newAccountQuotaFixture(t, newAccountEmptyBalance, newAccountAllocatedBalance)
	a := f.login(t, "fresh-with-reward")
	f.wait(t)
	var steps []string
	for _, r := range f.accountRequests(a.Secret()) {
		if r.line != "bad" {
			t.Fatal("领取与两次查询应使用最初绑定的代理")
		}
		step := r.path
		if r.plan != "" {
			step += ":" + r.plan
		}
		steps = append(steps, step)
	}
	want := []string{"/billing/balance", "/billing/preview", "/billing/claim:promo", "/billing/claim:trial", "/billing/balance"}
	if !reflect.DeepEqual(steps, want) {
		t.Fatalf("新号必须在全部套餐领取后重新出站查询：got=%v want=%v", steps, want)
	}
	got := f.h.Store.FindAny(a.ID)
	if got.Quota["test-model"]["remaining"] != float64(150) || len(f.h.Store.ListProxyProfiles()) != 2 {
		t.Fatal("领取后额度应及时落库，不能删除已获额度账号的代理")
	}
}

func TestNewAccountDoubleEmptyQuotaReassignsProxy(t *testing.T) {
	f := newAccountQuotaFixture(t, newAccountEmptyBalance, newAccountEmptyBalance)
	a := f.login(t, "fresh-without-quota")
	f.wait(t)
	got := f.h.Store.FindAny(a.ID)
	if len(f.h.Store.ListProxyProfiles()) != 1 || got.ProxyID == nil || *got.ProxyID != f.good.ID {
		t.Fatal("新号两次确认未获初始额度后，应删除原代理并按现有策略改派")
	}
	if got.ClaimView() == nil || got.ClaimView().ClaimedAt == nil || got.ClaimView().NextAt == nil {
		t.Fatal("换线不能清除本次领取结果和冷却")
	}
	requests := f.accountRequests(a.Secret())
	for _, r := range requests {
		if r.line != "bad" {
			t.Fatal("本轮只能处理原代理一次，改派后不得继续重领或循环淘汰")
		}
	}
}

func TestNewAccountFirstQuotaProtectsProxy(t *testing.T) {
	f := newAccountQuotaFixture(t, newAccountAllocatedBalance, newAccountEmptyBalance)
	a := f.login(t, "first-had-quota")
	f.wait(t)
	requests := f.accountRequests(a.Secret())
	balances := 0
	for _, r := range requests {
		if strings.HasSuffix(r.path, "/billing/balance") {
			balances++
		}
	}
	if balances != 2 {
		t.Fatalf("第一次已有额度也必须在领取后刷新，不能复用缓存：%d", balances)
	}
	if len(f.h.Store.ListProxyProfiles()) != 2 {
		t.Fatal("第一次有额度时不能因第二次为空就删除代理")
	}
}

func TestExistingAccountEmptyQuotaClaimRiskKeepsProxy(t *testing.T) {
	f := newClaimGuardFixture(t, http.StatusOK, `{"code":0,"data":{"plans":[],"balances":[]}}`)
	f.h.Quota.FetchQuota(f.a)
	w := f.manual()
	if w.Code != http.StatusOK || len(f.h.Store.ListProxyProfiles()) != 2 || strings.Contains(w.Body.String(), "proxy_removed") {
		t.Fatalf("老账号空额度和领取风控都不得触发新号代理淘汰：%s", w.Body.String())
	}
}

func TestNewAccountsWaitForClaimSlot(t *testing.T) {
	f := newAccountQuotaFixture(t, newAccountEmptyBalance, newAccountAllocatedBalance)
	if !claimSlot.acquire() {
		t.Fatal("测试开始时领取槽位不应被占用")
	}
	released := false
	defer func() {
		if !released {
			claimSlot.release()
		}
	}()
	accounts := []*model.Account{f.login(t, "queued-first"), f.login(t, "queued-second")}
	done := make(chan struct{})
	go func() { f.h.autoTasks.Wait(); close(done) }()
	select {
	case <-done:
		t.Fatal("领取槽位繁忙时新账号应等待，不能直接跳过")
	case <-time.After(100 * time.Millisecond):
	}
	claimSlot.release()
	released = true
	f.wait(t)
	for _, a := range accounts {
		counts := map[string]int{}
		for _, r := range f.accountRequests(a.Secret()) {
			counts[r.path]++
		}
		if counts["/billing/balance"] != 2 || counts["/billing/claim"] != 2 {
			t.Fatal(fmt.Sprintf("每个新号都必须完成领取及两次查询：%s %v", a.Name, counts))
		}
	}
}
