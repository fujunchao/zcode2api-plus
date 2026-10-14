package quota

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/store"
)

func replaceQuotaCredentialABA(t *testing.T, st *store.Store, snapshot *model.Account) {
	t.Helper()
	for _, token := range []string{"synthetic-new.token.sig", snapshot.Secret()} {
		if _, err := st.Update(snapshot.Provider, snapshot.ID, func(live *model.Account) { live.JWTToken = &token }); err != nil {
			t.Fatal(err)
		}
	}
}

// 真实 HTTP 已获许可后执行 A→B→A；旧成功和旧401都不能写回新代次，也不能缓存供新请求使用。
func TestQuotaLateCredentialABAResponseCannotMutateNewIdentity(t *testing.T) {
	for _, oldStatus := range []int{http.StatusOK, http.StatusUnauthorized} {
		t.Run(http.StatusText(oldStatus), func(t *testing.T) {
			svc, st, _ := setup(t)
			t.Cleanup(svc.Close)
			svc.Client = nil
			acc, err := st.AddAccount(model.ProviderZai, "合成额度ABA", "synthetic-old.token.sig")
			if err != nil {
				t.Fatal(err)
			}
			snapshot := st.FindAny(acc.ID)
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					close(entered)
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
					w.WriteHeader(oldStatus)
					_, _ = io.WriteString(w, balancePayload(0))
					return
				}
				_, _ = io.WriteString(w, balancePayload(200))
			}))
			t.Cleanup(server.Close)
			oldBase := config.ZcodeBillingBase
			config.ZcodeBillingBase = server.URL
			t.Cleanup(func() { config.ZcodeBillingBase = oldBase })
			done := make(chan struct{})
			go func() { svc.FetchQuota(snapshot); close(done) }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("旧额度请求未进入本地HTTP服务")
			}
			replaceQuotaCredentialABA(t, st, snapshot)
			marker := "synthetic-new-generation"
			_, _ = st.Update(acc.Provider, acc.ID, func(live *model.Account) {
				live.Status = model.StatusActive
				live.LastError = &marker
				live.Quota = map[string]map[string]any{"GLM-5.3-Flash": {"remaining": float64(777)}}
			})
			releaseOnce.Do(func() { close(release) })
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("旧额度请求未完成")
			}
			current := st.FindAny(acc.ID)
			if current.Status != model.StatusActive || current.LastError == nil || *current.LastError != marker || current.Quota["GLM-5.3-Flash"]["remaining"] != float64(777) {
				t.Fatal("旧凭据代次的成功/鉴权失败覆盖了重新登录后的健康状态")
			}
			result := svc.FetchQuota(snapshot)
			if result["cached"] == true || result["error"] != nil || calls.Load() != 2 || st.FindAny(acc.ID).Quota["GLM-5.3-Flash"]["remaining"] != float64(200) {
				t.Fatalf("新身份查询不能复用ABA之前的缓存：result=%v calls=%d", result, calls.Load())
			}
		})
	}
}

func TestQuotaCredentialABAInflightMustIssueFreshRequest(t *testing.T) {
	svc, st, _ := setup(t)
	t.Cleanup(svc.Close)
	svc.Client = nil
	acc, err := st.AddAccount(model.ProviderZai, "合成并发ABA", "synthetic-old.token.sig")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := st.FindAny(acc.ID)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		_, _ = io.WriteString(w, balancePayload(int(n)*100))
	}))
	t.Cleanup(server.Close)
	oldBase := config.ZcodeBillingBase
	config.ZcodeBillingBase = server.URL
	t.Cleanup(func() { config.ZcodeBillingBase = oldBase })
	firstDone := make(chan struct{})
	go func() { svc.FetchQuota(snapshot); close(firstDone) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("首个查询未开始")
	}
	replaceQuotaCredentialABA(t, st, snapshot)
	secondDone := make(chan map[string]any, 1)
	go func() { secondDone <- svc.FetchQuota(snapshot) }()
	releaseOnce.Do(func() { close(release) })
	select {
	case result := <-secondDone:
		if result["error"] != nil || result["cached"] == true || calls.Load() != 2 {
			t.Fatalf("ABA后等待旧在途查询也须重新出站：result=%v calls=%d", result, calls.Load())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("新代次查询未完成")
	}
	<-firstDone
	if st.FindAny(acc.ID).Quota["GLM-5.3-Flash"]["remaining"] != float64(200) {
		t.Fatal("新代次应使用第二次独立额度结果")
	}
}
