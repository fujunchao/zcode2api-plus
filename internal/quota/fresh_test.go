package quota

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zcode2api/internal/model"
)

const observedEmptyBalance = `{"code":0,"data":{"plans":[],"balances":[{"model":"test-model","total_units":0,"used_units":0,"remaining_units":0,"available_units":0}]}}`

func TestFetchQuotaFreshBypassesCache(t *testing.T) {
	svc, st, billing := setup(t)
	t.Cleanup(svc.Close)
	a, _ := st.AddAccount(model.ProviderZai, "new", "header.payload.sig")
	billing.setStatus(http.StatusOK, balancePayload(100))
	_, initial := svc.RefreshAccountsObserved([]*model.Account{a})
	billing.setStatus(http.StatusOK, observedEmptyBalance)
	result, final := svc.FetchQuotaFresh(context.Background(), a)
	if billing.callCount() != 2 || result["cached"] == true || !final.Missing {
		t.Fatalf("强制查询必须绕过首次成功缓存并取得新证据：calls=%d result=%v final=%+v", billing.callCount(), result, final)
	}
	if initial[a.ID].Missing || initial[a.ID].CheckedAt <= 0 || final.CheckedAt <= initial[a.ID].CheckedAt {
		t.Fatal("第一次有额度与第二次空额度必须保留为两份独立观测")
	}
	if st.FindAny(a.ID).Quota["test-model"]["remaining"] != float64(0) {
		t.Fatal("第二次查询必须真正更新账号余额")
	}
	if cached := svc.FetchQuota(a); cached["cached"] != true || billing.callCount() != 2 {
		t.Fatal("普通刷新仍应复用最新成功缓存")
	}
}

func TestFetchQuotaFreshFailureDiscardsOldCache(t *testing.T) {
	svc, st, billing := setup(t)
	t.Cleanup(svc.Close)
	a, _ := st.AddAccount(model.ProviderZai, "new", "header.payload.sig")
	billing.setStatus(http.StatusOK, balancePayload(100))
	svc.FetchQuota(a)
	billing.setStatus(http.StatusServiceUnavailable, `{}`)
	result, observation := svc.FetchQuotaFresh(context.Background(), a)
	if result["error"] == nil || observation.CheckedAt != 0 || observation.Missing {
		t.Fatal("失败不得复用旧查询的空额度证据")
	}
	billing.setStatus(http.StatusOK, balancePayload(200))
	if result := svc.FetchQuota(a); result["cached"] == true || billing.callCount() != 3 {
		t.Fatal("强制刷新失败后不能继续返回原先的成功缓存")
	}
}

func TestFetchQuotaFreshWaitsForInflightThenQueriesAgain(t *testing.T) {
	svc, st, _ := setup(t)
	t.Cleanup(svc.Close)
	a, _ := st.AddAccount(model.ProviderZai, "new", "header.payload.sig")
	entered, release, second := make(chan struct{}), make(chan struct{}), make(chan struct{})
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
		} else if n == 2 {
			close(second)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(balancePayload(int(n) * 100)))}, nil
	})
	oldDone := make(chan struct{})
	go func() { svc.FetchQuota(a); close(oldDone) }()
	<-entered
	freshDone := make(chan map[string]any, 1)
	go func() { result, _ := svc.FetchQuotaFresh(context.Background(), a); freshDone <- result }()
	select {
	case <-second:
		t.Fatal("强制刷新不能与同账号已有查询并行")
	case <-time.After(100 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case result := <-freshDone:
		if result["cached"] == true || result["error"] != nil || calls.Load() != 2 {
			t.Fatalf("等待旧查询后应再发起一次真实查询：%v calls=%d", result, calls.Load())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("强制刷新等待后没有继续")
	}
	<-oldDone
	if st.FindAny(a.ID).Quota["GLM-5.3-Flash"]["remaining"] != float64(200) {
		t.Fatal("领取后的观测不能由领取前开始的查询替代")
	}
}

func TestFetchQuotaFreshWaitingCanBeCanceled(t *testing.T) {
	svc, st, _ := setup(t)
	t.Cleanup(svc.Close)
	a, _ := st.AddAccount(model.ProviderZai, "new", "header.payload.sig")
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var calls atomic.Int32
	svc.Client = clientFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(balancePayload(100)))}, nil
	})
	oldDone := make(chan struct{})
	go func() { svc.FetchQuota(a); close(oldDone) }()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		result, observation := svc.FetchQuotaFresh(ctx, a)
		if result["error"] == nil || observation.CheckedAt != 0 {
			t.Error("取消不能产生有效额度证据")
		}
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("等待在途查询未响应取消")
	}
	if calls.Load() != 1 {
		t.Fatal("取消的强制刷新不应再出站")
	}
	releaseOnce.Do(func() { close(release) })
	<-oldDone
}

func TestRefreshAccountsObservedUsesActualRequestIdentity(t *testing.T) {
	svc, st, billing := setup(t)
	t.Cleanup(svc.Close)
	a, _ := st.AddAccount(model.ProviderZai, "new", "header.payload.sig")
	// AddAccount 返回内部对象；显式读取独立快照，才能模拟持有旧凭据的调用方。
	a = st.FindAny(a.ID)
	billing.setStatus(http.StatusOK, observedEmptyBalance)
	_, observations := svc.RefreshAccountsObserved([]*model.Account{a})
	initial := observations[a.ID]
	if !initial.Missing || !initial.Matches(st.FindAny(a.ID)) {
		t.Fatal("首查应返回与实际请求身份匹配的独立证据")
	}
	_, _ = st.Update(a.Provider, a.ID, func(live *model.Account) {
		token := "changed.payload.sig"
		live.JWTToken = &token
		live.StartPlanObservation = model.StartPlanObservation{}
	})
	_, final := svc.FetchQuotaFresh(context.Background(), a) // 故意传入旧快照。
	if !final.Matches(st.FindAny(a.ID)) || final.Matches(a) || !initial.Matches(a) {
		t.Fatal("旧调用参数不能污染实际请求的身份，首查证据也不得随账号变更")
	}
}
