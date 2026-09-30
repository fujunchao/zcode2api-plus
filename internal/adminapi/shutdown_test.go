package adminapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	"zcode2api/internal/auth"
	"zcode2api/internal/captcha"
	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/quota"
)

func TestClaimSchedulerStopCancelsRequest(t *testing.T) {
	st := newClaimStore(t)
	entered, release := make(chan struct{}), make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer up.Close()
	defer close(release)
	old := config.ZcodeBillingBase
	config.ZcodeBillingBase = up.URL
	defer func() { config.ZcodeBillingBase = old }()
	a, _ := st.AddAccount(model.ProviderZai, "test", "test.e30.sig")
	_ = st.SetSetting("claim_schedule_enabled", "true")
	_ = st.SetSetting("claim_schedule_time", time.Now().Format("15:04"))
	h := New(st, auth.New(st), captcha.NewManager(), quota.NewService(st))
	defer h.Close()
	scheduler := NewClaimScheduler(h)
	frozen := time.Now()
	_ = st.SetSetting("claim_schedule_time", frozen.Format("15:04"))
	scheduler.now = func() time.Time { return frozen }
	scheduler.Start()
	select {
	case <-entered:
	case <-time.After(7 * time.Second):
		scheduler.Stop()
		t.Fatal("定时领取没有开始")
	}
	done := make(chan struct{})
	go func() { scheduler.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("停止定时器没有取消上游请求")
	}
	if st.FindAny(a.ID).ClaimView() != nil {
		t.Fatal("取消不能记成领取失败或延长领取冷却")
	}
}

func TestScheduledClaimCancelsWhileWaitingForSlot(t *testing.T) {
	st := newClaimStore(t)
	_, _ = st.AddAccount(model.ProviderZai, "test", "test.e30.sig")
	h := &Handler{Store: st}
	if !claimSlot.acquire() {
		t.Fatal("槽位被占用")
	}
	defer claimSlot.release()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.runScheduledClaimsContext(ctx, time.Now()); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("槽位等待没有响应取消")
	}
}

func TestProxyHealthCancellationDoesNotPurgeLine(t *testing.T) {
	st := newClaimStore(t)
	entered, release := make(chan struct{}), make(chan struct{})
	up := probeStub(t, func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	defer close(release)
	_, _ = st.AddProxyProfile("line", up.URL, true)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	scheduler := NewProxyHealthScheduler(st, parent)
	done := make(chan struct{})
	go func() { scheduler.runOnce(scheduler.stop); close(done) }()
	<-entered
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("巡检网络请求未取消")
	}
	if len(st.ListProxyProfiles()) != 1 {
		t.Fatal("停机不能被判作线路故障")
	}
}
