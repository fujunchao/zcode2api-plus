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
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(oldStatus)
		_, _ = io.WriteString(w, `{"error":{"type":"authentication_error","message":"synthetic expired token"}}`)
	}))
	t.Cleanup(up.Close)
	config.UpstreamZai, config.UpstreamZaiFallback, config.ZcodeBillingBase = up.URL, up.URL, up.URL
	cm := captcha.NewManager()
	cm.SetConfigProvider(func(context.Context) (captcha.Config, error) {
		return captcha.Config{Enabled: true, SkipModelRequest: true}, nil
	})
	engine := gateway.NewEngine(st, cm, up.Client())
	pool := asyncpool.NewPool(st, auth.New(st), cm)
	pool.Client = up.Client()
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
	t.Helper()
	done := make(chan error, 1)
	go func() {
		if f.entry == "quota" {
			result, _ := f.qs.FetchQuotaFresh(f.ctx, f.account)
			if result["error"] == nil {
				done <- fmt.Errorf("旧认证失败必须返回查询错误")
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
		if err == nil && !strings.Contains(string(raw), "error") {
			err = fmt.Errorf("旧认证请求应以错误结束：status=%d body=%s", resp.StatusCode, raw)
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
