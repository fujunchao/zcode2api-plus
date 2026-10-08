package quota

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zcode2api/internal/model"
)

func TestProxyEndpointQuotaCacheDoesNotCrossIdentity(t *testing.T) {
	for _, change := range []string{"proxy", "credential"} {
		t.Run(change, func(t *testing.T) {
			svc, st, billing := setup(t)
			t.Cleanup(svc.Close)
			bad, _ := st.AddProxyProfile("bad", "http://127.0.0.1:18080", true)
			good, _ := st.AddProxyProfile("good", "http://127.0.0.1:18081", true)
			a, _ := st.AddAccount(model.ProviderZai, "test", "old.token.sig")
			_, _ = st.AssignProxyProfile(a.ID, bad.ID)
			snapshot := st.FindAny(a.ID)
			billing.setStatus(http.StatusOK, balancePayload(100))
			svc.FetchQuota(snapshot)
			if change == "proxy" {
				_, _, _ = st.PurgeFailedAccountProxy(snapshot)
				if *st.FindAny(a.ID).ProxyID != good.ID {
					t.Fatal("代理应已改派")
				}
			} else {
				_, _ = st.Update(a.Provider, a.ID, func(live *model.Account) { token := "new.token.sig"; live.JWTToken = &token })
			}
			billing.setStatus(http.StatusOK, balancePayload(200))
			result := svc.FetchQuota(snapshot)
			if result["cached"] == true || billing.callCount() != 2 || st.FindAny(a.ID).Quota["GLM-5.3-Flash"]["remaining"] != float64(200) {
				t.Fatal("改派或换令牌后不能继续复用旧身份的额度缓存")
			}
		})
	}
}

func TestProxyEndpointQuotaInflightDoesNotCrossProxy(t *testing.T) {
	svc, st, _ := setup(t)
	t.Cleanup(svc.Close)
	bad, _ := st.AddProxyProfile("bad", "http://127.0.0.1:18080", true)
	good, _ := st.AddProxyProfile("good", "http://127.0.0.1:18081", true)
	a, _ := st.AddAccount(model.ProviderZai, "test", "old.token.sig")
	_, _ = st.AssignProxyProfile(a.ID, bad.ID)
	snapshot := st.FindAny(a.ID)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var calls atomic.Int32
	svc.Client = clientFunc(func(r *http.Request) (*http.Response, error) {
		n := calls.Add(1)
		if n == 1 {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(balancePayload(int(n) * 100)))}, nil
	})
	firstDone := make(chan struct{})
	go func() { svc.FetchQuota(snapshot); close(firstDone) }()
	<-entered
	_, _ = st.AssignProxyProfile(a.ID, good.ID)
	secondDone := make(chan map[string]any, 1)
	go func() { secondDone <- svc.FetchQuota(snapshot) }()
	releaseOnce.Do(func() { close(release) })
	select {
	case result := <-secondDone:
		if result["error"] != nil || result["cached"] == true || calls.Load() != 2 {
			t.Fatalf("新代理需要独立请求：%v calls=%d", result, calls.Load())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("新代理查询被旧在途请求阻塞")
	}
	<-firstDone
	if st.FindAny(a.ID).Quota["GLM-5.3-Flash"]["remaining"] != float64(200) {
		t.Fatal("旧代理的迟到余额不能代替新代理查询")
	}
}
