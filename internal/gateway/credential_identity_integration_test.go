package gateway_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zcode2api/internal/asyncpool"
	"zcode2api/internal/auth"
	"zcode2api/internal/captcha"
	"zcode2api/internal/config"
	"zcode2api/internal/gateway"
	"zcode2api/internal/model"
	"zcode2api/internal/quota"
	"zcode2api/internal/store"
)

// 只经真实 HTTP/额度公开入口触发旧结果回写，不直接调用内部状态标记函数。
// channel 精确固定“旧请求在途 → 同 ID 重新登录 → 旧响应到达”，不靠 sleep 撞时序。
func TestCredentialIdentityLateAuthentication(t *testing.T) {
	for _, entry := range []string{"sync", "async", "quota"} {
		for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
			for _, relogin := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%d/relogin=%t", entry, status, relogin), func(t *testing.T) {
					f := newCredentialFixture(t, entry, status)
					done := f.start(t)
					f.waitStarted(t)
					if relogin {
						f.relogin(t)
					}
					f.release()
					f.waitDone(t, done)
					if relogin {
						f.assertNewIdentity(t)
						// 旧结果被隔离后，新凭据必须能真正发起下一次请求。
						f.waitDone(t, f.run(t, true))
					} else {
						got := f.st.FindAny(f.account.ID)
						if got.Status != model.StatusInvalid || got.LastErrorKind == nil || *got.LastErrorKind != model.ErrorKindAuthFailed {
							t.Fatalf("同一凭据的真实认证失败仍须标记失效：status=%s kind=%v", got.Status, got.LastErrorKind)
						}
					}
				})
			}
		}
	}
}

func TestCredentialIdentityLateFailures(t *testing.T) {
	for _, entry := range []string{"sync", "async"} {
		for _, status := range []int{402, 429, 503, 405} {
			t.Run(fmt.Sprintf("%s/%d", entry, status), func(t *testing.T) {
				f := newCredentialFixture(t, entry, status)
				done := f.start(t)
				f.waitStarted(t)
				f.relogin(t)
				f.release()
				f.waitDone(t, done)
				f.assertNewIdentity(t)
				if got := f.st.FindAny(f.account.ID); len(got.ExhaustedModels) != 0 || got.RiskControlStreak != 0 || got.RateLimitStreak != 0 || got.Upstream503Streak != 0 {
					t.Fatal("旧身份错误污染了新身份的模型标记或失败阶梯")
				}
			})
		}
	}
}

func TestCredentialIdentityLateSuccess(t *testing.T) {
	for _, entry := range []string{"sync", "async", "quota"} {
		t.Run(entry, func(t *testing.T) {
			f := newCredentialFixture(t, entry, http.StatusOK)
			done := f.run(t, true)
			f.waitStarted(t)
			f.relogin(t)
			// 模拟新身份已独立取得的额度/健康状态；旧成功不能充当新身份恢复证明。
			_, err := f.st.Update(f.account.Provider, f.account.ID, func(a *model.Account) {
				a.Status = model.StatusExhausted
				a.RateLimitStreak, a.RiskControlStreak, a.Upstream503Streak = 2, 3, 4
				a.Quota = map[string]map[string]any{"new-identity": {"remaining": float64(17)}}
			})
			if err != nil {
				t.Fatal(err)
			}
			f.release()
			f.waitDone(t, done)
			got := f.st.FindAny(f.account.ID)
			if got.Status != model.StatusExhausted || got.RateLimitStreak != 2 || got.RiskControlStreak != 3 || got.Upstream503Streak != 4 || got.Quota["new-identity"]["remaining"] != float64(17) {
				t.Fatalf("旧成功覆盖了新身份状态：status=%s quota=%v", got.Status, got.Quota)
			}
			if entry != "quota" && (got.UseCount != 1 || got.TotalInputTokens != 5 || got.TotalOutputTokens != 7) {
				t.Fatalf("旧身份已经消费的用量仍须按同账号累计：use=%d input=%d output=%d", got.UseCount, got.TotalInputTokens, got.TotalOutputTokens)
			}
		})
	}
}

func TestCredentialIdentityKeepsAdministrativeDisable(t *testing.T) {
	for _, entry := range []string{"sync", "async", "quota"} {
		t.Run(entry, func(t *testing.T) {
			f := newCredentialFixture(t, entry, http.StatusUnauthorized)
			done := f.start(t)
			f.waitStarted(t)
			if _, err := f.st.SetEnabled(f.account.Provider, f.account.ID, false); err != nil {
				t.Fatal(err)
			}
			f.release()
			f.waitDone(t, done)
			if got := f.st.FindAny(f.account.ID); got.Enabled || got.Status != model.StatusDisabled {
				t.Fatal("在途结果覆盖了管理员停用状态")
			}
		})
	}
}

type credentialFixture struct {
	st      *store.Store
	account *model.Account
	entry   string
	api     *httptest.Server
	qs      *quota.Service
	ctx     context.Context
	oldJWT  string
	newJWT  string
	started chan struct{}
	release func()
}

func newCredentialFixture(t *testing.T, entry string, oldStatus int) *credentialFixture {
	t.Helper()
	oldDB, oldData := config.DBPath, config.DataDir
	oldUp, oldFallback, oldBilling := config.UpstreamZai, config.UpstreamZaiFallback, config.ZcodeBillingBase
	oldAsync, oldRetries := config.AsyncEnabled, config.AsyncMaxRetries
	config.DBPath, config.DataDir = filepath.Join(t.TempDir(), "accounts.db"), t.TempDir()
	config.AsyncEnabled, config.AsyncMaxRetries = true, 0
	t.Cleanup(func() {
		config.DBPath, config.DataDir = oldDB, oldData
		config.UpstreamZai, config.UpstreamZaiFallback, config.ZcodeBillingBase = oldUp, oldFallback, oldBilling
		config.AsyncEnabled, config.AsyncMaxRetries = oldAsync, oldRetries
	})
	st, err := store.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.SetSetting("gateway_key", "synthetic-gateway-key"); err != nil {
		t.Fatal(err)
	}
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"user_id":"synthetic-relogin-user"}`))
	f := &credentialFixture{st: st, entry: entry, oldJWT: "header." + payload + ".old", newJWT: "header." + payload + ".new", started: make(chan struct{})}
	f.account, _, err = st.SaveOAuthAccount("synthetic-user", f.oldJWT, "synthetic@example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	var releaseOnce sync.Once
	f.release = func() { releaseOnce.Do(func() { close(gate) }) }
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.Header.Get("Authorization") == "Bearer "+f.newJWT {
			credentialSuccess(w, entry)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+f.oldJWT {
			t.Error("本回归的在途请求必须携带旧合成 JWT")
		}
		if calls.Add(1) == 1 {
			close(f.started)
		}
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
		if oldStatus == http.StatusOK {
			credentialSuccess(w, entry)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(oldStatus)
		if oldStatus == 405 {
			_, _ = io.WriteString(w, `{"error":{"message":"request has been blocked due to unusual activity"}}`)
		} else {
			_, _ = io.WriteString(w, `{"error":{"type":"authentication_error","message":"synthetic expired token"}}`)
		}
	}))
	t.Cleanup(up.Close)
	config.UpstreamZai, config.UpstreamZaiFallback, config.ZcodeBillingBase = up.URL, up.URL, up.URL
	cm := captcha.NewManager()
	cm.SetConfigProvider(func(context.Context) (captcha.Config, error) {
		return captcha.Config{Enabled: true, SkipModelRequest: true}, nil
	})
	engine := gateway.NewEngine(st, cm, up.Client())
	engine.RateLimitRetryDelay = 0
	engine.BusyRetryDelays, engine.OverloadRetryDelays = nil, nil
	pool := asyncpool.NewPool(st, auth.New(st), cm)
	pool.Client = up.Client()
	pool.RateLimitRetryDelay = 0
	pool.BusyRetryDelays, pool.OverloadRetryDelays = nil, nil
	t.Cleanup(pool.Close)
	f.qs = quota.NewService(st)
	f.qs.Client = up.Client()
	t.Cleanup(f.qs.Close)
	mux := http.NewServeMux()
	(&gateway.Handler{Engine: engine, Auth: auth.New(st)}).Register(mux)
	pool.Register(mux)
	f.api = httptest.NewServer(mux)
	t.Cleanup(f.api.Close)
	var cancel context.CancelFunc
	f.ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	// 先解除模拟服务阻塞并取消请求，再关服务器、后台任务和隔离数据库。
	t.Cleanup(func() { f.release(); cancel() })
	return f
}

func (f *credentialFixture) start(t *testing.T) <-chan error {
	return f.run(t, false)
}

func credentialSuccess(w http.ResponseWriter, entry string) {
	w.Header().Set("Content-Type", "application/json")
	switch entry {
	case "quota":
		_, _ = io.WriteString(w, `{"code":0,"data":{"plans":[],"balances":[{"model":"GLM-5.3","remaining_units":100,"available_units":100,"total_units":100,"used_units":0}]}}`)
	case "async":
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5}}}\n\n"+
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":7}}\n\n"+
			"data: {\"type\":\"message_stop\"}\n\n")
	default:
		_, _ = io.WriteString(w, `{"id":"synthetic","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":7}}`)
	}
}

func (f *credentialFixture) run(t *testing.T, success bool) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		if f.entry == "quota" {
			result, _ := f.qs.FetchQuotaFresh(f.ctx, f.account)
			if (result["error"] == nil) != success {
				done <- fmt.Errorf("额度结果不符合预期：success=%t result=%v", success, result)
				return
			}
			done <- nil
			return
		}
		path := "/v1/messages"
		if f.entry == "async" {
			path = "/async/v1/messages"
		}
		req, err := http.NewRequestWithContext(f.ctx, http.MethodPost, f.api.URL+path,
			strings.NewReader(`{"model":"GLM-5.3","max_tokens":8,"messages":[{"role":"user","content":"synthetic identity regression"}]}`))
		if err != nil {
			done <- err
			return
		}
		req.Header.Set("x-api-key", "synthetic-gateway-key")
		req.Header.Set("Content-Type", "application/json")
		resp, err := f.api.Client().Do(req)
		if err != nil {
			done <- err
			return
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err == nil && strings.Contains(string(raw), "error") == success {
			err = fmt.Errorf("模型结果不符合预期：success=%t status=%d body=%s", success, resp.StatusCode, raw)
		}
		if success && err == nil && (resp.StatusCode != http.StatusOK || (f.entry == "async" && !strings.Contains(string(raw), "event: done"))) {
			err = fmt.Errorf("成功请求必须完整交付：status=%d body=%s", resp.StatusCode, raw)
		}
		done <- err
	}()
	return done
}

func (f *credentialFixture) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-f.started:
	case <-f.ctx.Done():
		t.Fatal("旧请求未到达隔离上游")
	}
}

func (f *credentialFixture) waitDone(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-f.ctx.Done():
		t.Fatal("在途请求未完成")
	}
}

func (f *credentialFixture) relogin(t *testing.T) {
	t.Helper()
	next, isNew, err := f.st.SaveOAuthAccount("synthetic-user", f.newJWT, "synthetic@example.invalid")
	if err != nil || isNew || next.ID != f.account.ID || next.Status != model.StatusActive {
		t.Fatalf("重新登录必须更新同 ID 身份：isNew=%t err=%v", isNew, err)
	}
}

func (f *credentialFixture) assertNewIdentity(t *testing.T) {
	t.Helper()
	got := f.st.FindAny(f.account.ID)
	if got.Secret() != f.newJWT || got.Status != model.StatusActive || got.LastError != nil || got.LastErrorKind != nil || got.CoolingUntil != nil {
		t.Fatalf("旧请求污染了重新登录的新身份：status=%s error=%v kind=%v cooling=%v", got.Status, got.LastError, got.LastErrorKind, got.CoolingUntil)
	}
}
