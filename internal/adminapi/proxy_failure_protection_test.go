package adminapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"zcode2api/internal/model"
	"zcode2api/internal/proxy"
)

// 通过实际额度入口和真实本地代理复现：一次响应头超时不能删除共享业务出口。
func TestProxyEndpointQuotaTimeoutDoesNotDeleteSharedProxy(t *testing.T) {
	f := newAccountQuotaFixture(t, newAccountAllocatedBalance, newAccountAllocatedBalance)
	var timeout atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if timeout.Load() {
			select {
			case <-r.Context().Done():
			case <-time.After(time.Second):
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(newAccountAllocatedBalance))
	}))
	t.Cleanup(server.Close)
	var err error
	f.bad, err = f.h.Store.UpdateProxyProfile(f.bad.ID, "家庭出口", server.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	transport, err := proxy.TransportFor(f.bad.URL)
	if err != nil {
		t.Fatal(err)
	}
	previous := transport.ResponseHeaderTimeout
	transport.ResponseHeaderTimeout = 80 * time.Millisecond
	t.Cleanup(func() {
		f.h.Close()
		f.h.Quota.Close()
		transport.CloseIdleConnections()
		transport.ResponseHeaderTimeout = previous
	})
	accounts := existingProxyFailureAccounts(t, f)
	if result := f.h.Quota.FetchQuota(accounts[0]); result["error"] != nil {
		t.Fatalf("初始额度查询应成功：%v", result)
	}
	before := f.h.Store.FindAny(accounts[0].ID)
	timeout.Store(true)
	result, _ := f.h.Quota.FetchQuotaFresh(context.Background(), before)
	message, _ := result["error"].(string)
	if !strings.Contains(message, "额度查询网络错误") || !strings.Contains(message, "timeout") {
		t.Fatalf("必须真实触发等待响应头超时：%v", result)
	}
	if len(f.h.Store.ListProxyProfiles()) != 2 || result["proxy_removed"] != nil {
		t.Fatalf("单次额度超时不得删除共享代理：%v", result)
	}
	for _, account := range accounts {
		live := f.h.Store.FindAny(account.ID)
		if live.ProxyID == nil || *live.ProxyID != f.bad.ID || live.ProxyURL == nil || *live.ProxyURL != f.bad.URL {
			t.Fatal("额度观测失败不得改派任何共享账号")
		}
	}
	live := f.h.Store.FindAny(before.ID)
	if live.Status != before.Status || !reflect.DeepEqual(live.Quota, before.Quota) || live.LastErrorKind == nil || *live.LastErrorKind != model.ErrorKindQuotaQueryFailed {
		t.Fatal("超时只能记录额度查询错误，必须保留成功额度和业务状态")
	}
	timeout.Store(false)
	if recovered, _ := f.h.Quota.FetchQuotaFresh(context.Background(), live); recovered["error"] != nil {
		t.Fatalf("原出口恢复后应可正常刷新，无需重建或改派：%v", recovered)
	}
	if f.h.Store.FindAny(before.ID).LastError != nil {
		t.Fatal("成功额度查询应清除已经恢复的观测错误")
	}
}
