package adminapi

import (
	"context"
	"encoding/json"
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
	balanceNumber             int
}

// 两个本地服务器同时充当代理和上游，所有凭据、验证码和余额均为合成数据。
type newAccountFixture struct {
	h                         *Handler
	bad, good                 store.ProxyProfile
	mu                        sync.Mutex
	requests                  []newAccountRequest
	balances                  map[string]int
	before                    string
	after                     string
	beforeStatus, afterStatus int
	preview, claimBody        string
	claimStatus               int
	onRequest                 func(*http.Request, newAccountRequest)
}

func newAccountQuotaFixture(t *testing.T, before, after string) *newAccountFixture {
	t.Helper()
	st := newClaimStore(t)
	f := &newAccountFixture{before: before, after: after, balances: map[string]int{},
		beforeStatus: http.StatusOK, afterStatus: http.StatusOK, claimStatus: http.StatusOK,
		preview:   `{"code":0,"data":{"plans":[{"plan_id":"trial","priority":1},{"plan_id":"promo","priority":2}]}}`,
		claimBody: `{"code":0,"data":{}}`}
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
			request := newAccountRequest{line: line, account: account, path: r.URL.Path, plan: plan}
			body := `{"code":0,"data":{}}`
			status := http.StatusOK
			switch {
			case strings.HasSuffix(r.URL.Path, "/billing/balance"):
				f.balances[account]++
				request.balanceNumber = f.balances[account]
				body, status = f.before, f.beforeStatus
				if f.balances[account] > 1 {
					body, status = f.after, f.afterStatus
				}
			case strings.HasSuffix(r.URL.Path, "/billing/preview"):
				body = f.preview
			case strings.HasSuffix(r.URL.Path, "/billing/claim"):
				body, status = f.claimBody, f.claimStatus
			}
			f.requests = append(f.requests, request)
			onRequest := f.onRequest
			f.mu.Unlock()
			if onRequest != nil {
				onRequest(r, request)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
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
			t.Fatalf("每个新号都必须完成领取及两次查询：%s %v", a.Name, counts)
		}
	}
}

func TestNewAccountUnknownOrExhaustedQuotaKeepsProxy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"查询失败", http.StatusServiceUnavailable, `{}`},
		{"鉴权失败", http.StatusUnauthorized, `{}`},
		{"非JSON", http.StatusOK, `invalid`},
		{"业务失败", http.StatusOK, `{"code":3012,"data":{"plans":[],"balances":[]}}`},
		{"缺成功码", http.StatusOK, `{"data":{"plans":[],"balances":[]}}`},
		{"缺余额字段", http.StatusOK, `{"code":0,"data":{"plans":[]}}`},
		{"未知余额", http.StatusOK, `{"code":0,"data":{"plans":[],"balances":[{}]}}`},
		{"已有套餐耗尽", http.StatusOK, `{"code":0,"data":{"plans":[{"plan_id":"trial"}],"balances":[{"total_units":100,"used_units":100,"remaining_units":0}]}}`},
		{"已有配额耗尽", http.StatusOK, `{"code":0,"data":{"plans":[],"balances":[{"total_units":100,"used_units":100,"remaining_units":0}]}}`},
		{"重复查询405", http.StatusMethodNotAllowed, `already queried`},
	} {
		for _, stage := range []string{"首次", "领取后"} {
			t.Run(stage+tc.name, func(t *testing.T) {
				f := newAccountQuotaFixture(t, newAccountEmptyBalance, newAccountEmptyBalance)
				f.mu.Lock()
				if stage == "首次" {
					f.beforeStatus, f.before = tc.status, tc.body
				} else {
					f.afterStatus, f.after = tc.status, tc.body
				}
				f.mu.Unlock()
				a := f.login(t, "unknown-quota")
				f.wait(t)
				got := f.h.Store.FindAny(a.ID)
				if len(f.h.Store.ListProxyProfiles()) != 2 || got.ProxyID == nil || *got.ProxyID != f.bad.ID {
					t.Fatal("任一次未知、失败或已有配额，都不能删除新账号代理")
				}
			})
		}
	}
}

func TestNewAccountClaimFailureOrNoPlanStillRefreshes(t *testing.T) {
	for _, mode := range []string{"没有可领套餐", "领取风控", "领取网络响应失败"} {
		t.Run(mode, func(t *testing.T) {
			f := newAccountQuotaFixture(t, newAccountEmptyBalance, newAccountEmptyBalance)
			f.mu.Lock()
			switch mode {
			case "没有可领套餐":
				f.preview = `{"code":0,"data":{"plans":[]}}`
			case "领取风控":
				f.claimStatus, f.claimBody = http.StatusMethodNotAllowed, `{"code":3012,"msg":"request has been blocked due to unusual activity."}`
			default:
				f.claimStatus, f.claimBody = http.StatusServiceUnavailable, `{"code":500,"msg":"temporarily unavailable"}`
			}
			f.onRequest = func(_ *http.Request, request newAccountRequest) {
				if request.balanceNumber == 2 && len(f.h.Store.ListProxyProfiles()) != 2 {
					t.Error("第二次额度响应前不能提前删除代理")
				}
			}
			f.mu.Unlock()
			a := f.login(t, "claim-not-successful")
			f.wait(t)
			got := f.h.Store.FindAny(a.ID)
			if got.ProxyID == nil || *got.ProxyID != f.good.ID || len(f.h.Store.ListProxyProfiles()) != 1 {
				t.Fatal("领取失败或无套餐也必须补查；双次确证空额度才在收尾淘汰")
			}
		})
	}
}

func TestNewAccountChangeDuringSecondQueryKeepsProxy(t *testing.T) {
	for _, change := range []string{"binding", "url", "credential", "used", "disabled", "archived", "deleted"} {
		t.Run(change, func(t *testing.T) {
			f := newAccountQuotaFixture(t, newAccountEmptyBalance, newAccountEmptyBalance)
			f.mu.Lock()
			f.onRequest = func(_ *http.Request, request newAccountRequest) {
				if request.balanceNumber != 2 {
					return
				}
				a := f.h.Store.ListAccounts(model.ProviderZai)[0]
				var err error
				switch change {
				case "binding":
					_, err = f.h.Store.AssignProxyProfile(a.ID, f.good.ID)
				case "url":
					_, err = f.h.Store.UpdateProxyProfile(f.bad.ID, f.bad.Name, f.good.URL, true)
				case "deleted":
					_, err = f.h.Store.RemoveAccount(a.Provider, a.ID)
				default:
					_, err = f.h.Store.Update(a.Provider, a.ID, func(live *model.Account) {
						switch change {
						case "credential":
							token := jwtTokenFor("changed-user")
							live.JWTToken = &token
						case "used":
							live.UseCount++
						case "disabled":
							live.Enabled = false
						case "archived":
							now := float64(time.Now().Unix())
							live.ArchivedAt = &now
						}
					})
				}
				if err != nil {
					t.Error(err)
				}
			}
			f.mu.Unlock()
			f.login(t, "changed-during-refresh")
			f.wait(t)
			if len(f.h.Store.ListProxyProfiles()) != 2 {
				t.Fatal("账号或线路已变化时，不得用迟到的第二次响应删除代理")
			}
		})
	}
}

func TestExistingAccountReloginNeverRunsNewAccountGuard(t *testing.T) {
	f := newAccountQuotaFixture(t, newAccountEmptyBalance, newAccountEmptyBalance)
	old, _, err := f.h.Store.SaveOAuthAccount("existing", jwtTokenFor("existing-user"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.Store.AssignProxyProfile(old.ID, f.bad.ID); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.claimStatus, f.claimBody = http.StatusMethodNotAllowed, `{"code":3012,"msg":"request has been blocked due to unusual activity."}`
	f.mu.Unlock()
	a := f.login(t, "existing-user")
	f.wait(t)
	if a.ID != old.ID || len(f.h.Store.ListProxyProfiles()) != 2 {
		t.Fatal("已有账号重登即使空额度且风控也不能触发新号检测")
	}
	balances := 0
	for _, r := range f.accountRequests(a.Secret()) {
		if r.balanceNumber > 0 {
			balances++
		}
	}
	if balances != 1 {
		t.Fatal("老账号不得进入新号专属二次检测")
	}
}

func TestNewAccountEntryPointsRefreshOnlyCreatedAccounts(t *testing.T) {
	for _, path := range []string{"/admin/api/accounts", "/admin/api/accounts/batch/add"} {
		t.Run(path, func(t *testing.T) {
			f := newAccountQuotaFixture(t, newAccountEmptyBalance, newAccountAllocatedBalance)
			mux := http.NewServeMux()
			f.h.Register(mux)
			payload := map[string]any{"proxy_id": f.bad.ID, "tokens": []string{jwtTokenFor("entry-one"), jwtTokenFor("entry-two")},
				"items": []map[string]string{{"secret": jwtTokenFor("entry-one")}, {"secret": jwtTokenFor("entry-two")}}}
			code, body := do(t, mux, f.h.Store, http.MethodPost, path, payload)
			if code != http.StatusOK {
				t.Fatalf("新增失败：%d %v", code, body)
			}
			f.wait(t)
			code, body = do(t, mux, f.h.Store, http.MethodPost, path, payload)
			if code != http.StatusOK {
				t.Fatalf("重复新增失败：%d %v", code, body)
			}
			f.wait(t)
			for _, a := range f.h.Store.ListAccounts(model.ProviderZai) {
				balances, claims := 0, 0
				for _, r := range f.accountRequests(a.Secret()) {
					if r.balanceNumber > 0 {
						balances++
					}
					if r.plan != "" {
						claims++
					}
				}
				if balances != 2 || claims != 2 {
					t.Fatalf("只对真正新号完整执行一次领取与双查：balances=%d claims=%d", balances, claims)
				}
			}
		})
	}
}

func TestNewAccountDirectOrCustomProxyIsNotPurged(t *testing.T) {
	for _, custom := range []bool{false, true} {
		t.Run(map[bool]string{false: "直连", true: "手工地址"}[custom], func(t *testing.T) {
			f := newAccountQuotaFixture(t, newAccountEmptyBalance, newAccountEmptyBalance)
			session := &loginSession{direct: true}
			if custom {
				session = &loginSession{proxyURL: f.bad.URL}
			}
			a, err := f.h.saveOAuthAccount(&oauth.ExchangeResult{Token: jwtTokenFor("without-profile")}, session)
			if err != nil {
				t.Fatal(err)
			}
			f.wait(t)
			if got := f.h.Store.FindAny(a.ID); got.ProxyID != nil || len(f.h.Store.ListProxyProfiles()) != 2 {
				t.Fatal("直连或手工地址不能误删代理池中同地址的命名线路")
			}
		})
	}
}

func TestNewAccountDisabledAutoClaimDoesNotPurge(t *testing.T) {
	f := newAccountQuotaFixture(t, newAccountEmptyBalance, newAccountEmptyBalance)
	if err := f.h.Store.SetSetting("claim_auto_enabled", "false"); err != nil {
		t.Fatal(err)
	}
	a := f.login(t, "auto-disabled")
	f.wait(t)
	if requests := f.accountRequests(a.Secret()); len(requests) != 1 || requests[0].balanceNumber != 1 {
		t.Fatal("自动领取关闭时只保留入池首查")
	}
	if len(f.h.Store.ListProxyProfiles()) != 2 {
		t.Fatal("关闭自动领取不得触发代理淘汰")
	}
}

func TestImportedAccountsDoNotRunNewAccountGuard(t *testing.T) {
	f := newAccountQuotaFixture(t, newAccountEmptyBalance, newAccountEmptyBalance)
	mux := http.NewServeMux()
	f.h.Register(mux)
	code, body := do(t, mux, f.h.Store, http.MethodPost, "/admin/api/import", map[string]any{
		"providers": map[string]any{model.ProviderZai: []map[string]string{{"name": "backup", "secret": jwtTokenFor("backup-user")}}},
	})
	if code != http.StatusOK {
		t.Fatalf("导入失败：%d %v", code, body)
	}
	f.wait(t)
	if len(f.accountRequests(jwtTokenFor("backup-user"))) != 0 || len(f.h.Store.ListProxyProfiles()) != 2 {
		t.Fatal("备份导入不能追溯触发新号领取与代理检测")
	}
}

func TestNewAccountWaitingClaimCanBeCanceled(t *testing.T) {
	f := newAccountQuotaFixture(t, newAccountEmptyBalance, newAccountEmptyBalance)
	if !claimSlot.acquire() {
		t.Fatal("测试领取槽位被占用")
	}
	defer claimSlot.release()
	a := f.login(t, "cancel-queued")
	done := make(chan struct{})
	go func() { f.h.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("新号等待领取必须响应停机")
	}
	if len(f.accountRequests(a.Secret())) != 1 || len(f.h.Store.ListProxyProfiles()) != 2 {
		t.Fatal("取消等待不得触发额外查询或代理淘汰")
	}
}

func TestNewAccountSecondQueryCancellationDoesNotPurge(t *testing.T) {
	f := newAccountQuotaFixture(t, newAccountEmptyBalance, newAccountEmptyBalance)
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	f.mu.Lock()
	f.onRequest = func(r *http.Request, request newAccountRequest) {
		if request.balanceNumber == 2 {
			close(entered)
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}
	}
	f.mu.Unlock()
	f.login(t, "cancel-refresh")
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("领取后的真实查询没有启动")
	}
	f.h.Close()
	if len(f.h.Store.ListProxyProfiles()) != 2 {
		t.Fatal("停机取消第二次查询不能被视为两次空额度")
	}
}
