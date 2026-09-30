package quota

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
	"zcode2api/internal/model"
)

type auditBlockingBilling struct {
	entered chan struct{}
	release chan struct{}
}

func (b *auditBlockingBilling) Do(r *http.Request) (*http.Response, error) {
	close(b.entered)
	select {
	case <-b.release:
	case <-r.Context().Done():
		return nil, r.Context().Err()
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(balancePayload(100)))}, nil
}
func TestMonitorStopCancelsInflightWork(t *testing.T) {
	svc, st, _ := setup(t)
	st.AddAccount(model.ProviderZai, "audit", "audit.e30.sig")
	st.SetSetting("quota_refresh_interval", "60")
	client := &auditBlockingBilling{entered: make(chan struct{}), release: make(chan struct{})}
	svc.Client = client
	m := svc.NewMonitor()
	m.Start()
	select {
	case <-client.entered:
	case <-time.After(8 * time.Second):
		close(client.release)
		m.Stop()
		t.Fatal("刷新没有启动")
	}
	stopped := make(chan struct{})
	go func() { m.Stop(); close(stopped) }()
	select {
	case <-stopped:
		close(client.release)
	case <-time.After(150 * time.Millisecond):
		close(client.release)
		<-stopped
		t.Fatal("Stop 没有取消正在运行的额度请求，只能等待上游自行完成")
	}
}
