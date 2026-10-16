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
	"zcode2api/internal/modelprobe"
	"zcode2api/internal/proxy"
	"zcode2api/internal/store"
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
	manager := modelprobe.NewWithNetworkTargets(context.Background(), f.h.Store, func() []string { return []string{f.good.URL} })
	t.Cleanup(manager.Close)
	if worked, err := manager.RunNetworkDue(context.Background()); err != nil || !worked {
		t.Fatalf("真实额度失败必须接入统一网络复核：worked=%v err=%v", worked, err)
	}
	records, err := f.h.Store.NetworkHealth()
	if err != nil || len(records) != 1 || records[0].ProfileID != f.bad.ID || records[0].State != "healthy" || records[0].Removed {
		t.Fatalf("计费异常不能冒充网络首轮失败；已恢复的出口应正常保留：%+v %v", records, err)
	}
	if recovered, _ := f.h.Quota.FetchQuotaFresh(context.Background(), live); recovered["error"] != nil {
		t.Fatalf("原出口恢复后应可正常刷新，无需重建或改派：%v", recovered)
	}
	if f.h.Store.FindAny(before.ID).LastError != nil {
		t.Fatal("成功额度查询应清除已经恢复的观测错误")
	}
}

func TestProxyEndpointProtectionManagementRoundTrip(t *testing.T) {
	f := newAccountQuotaFixture(t, newAccountAllocatedBalance, newAccountAllocatedBalance)
	mux := http.NewServeMux()
	f.h.Register(mux)
	code, created := do(t, mux, f.h.Store, http.MethodPost, "/admin/api/proxies", map[string]any{
		"name": "人工维护出口", "url": "http://127.0.0.1:18089", "enabled": false, "auto_delete_protected": true,
	})
	if code != http.StatusOK || created["auto_delete_protected"] != true {
		t.Fatalf("新增代理应原子保存保护标记：%d %v", code, created)
	}
	id := created["id"].(string)
	code, updated := do(t, mux, f.h.Store, http.MethodPut, "/admin/api/proxies/"+id, map[string]any{
		"name": "旧客户端改名", "url": "http://127.0.0.1:18089", "enabled": false,
	})
	if code != http.StatusOK || updated["auto_delete_protected"] != true || updated["enabled"] != false {
		t.Fatalf("旧客户端编辑不得丢失保护：%d %v", code, updated)
	}
	code, listed := do(t, mux, f.h.Store, http.MethodGet, "/admin/api/proxies", nil)
	if code != http.StatusOK {
		t.Fatal("读取代理失败")
	}
	found := false
	for _, item := range listed["profiles"].([]any) {
		profile := item.(map[string]any)
		if profile["id"] == id {
			found = profile["auto_delete_protected"] == true && profile["auto_delete_protection"] == "manual"
		}
	}
	if !found {
		t.Fatal("列表必须展示有效保护原因")
	}
	code, _ = do(t, mux, f.h.Store, http.MethodPut, "/admin/api/proxies/"+id, map[string]any{
		"name": "非法保护字段", "url": "http://127.0.0.1:18089", "auto_delete_protected": "false",
	})
	if code != http.StatusBadRequest || f.h.Store.ProxyAutoDeleteProtections()[id] != "manual" {
		t.Fatal("保护字段类型错误不能静默解除保护")
	}
	code, _ = do(t, mux, f.h.Store, http.MethodDelete, "/admin/api/proxies/"+id, nil)
	if code != http.StatusOK || len(f.h.Store.ListProxyProfiles()) != 2 {
		t.Fatal("受保护的节点仍允许显式手动删除")
	}
}

func TestProxyEndpointDefaultProtectionIsVisibleWithoutManualFlag(t *testing.T) {
	f := newAccountQuotaFixture(t, newAccountAllocatedBalance, newAccountAllocatedBalance)
	policy := store.DefaultNewAccountProxyPolicy()
	policy.Enabled, policy.DefaultProxyID = true, f.bad.ID
	if _, err := f.h.Store.SaveNewAccountProxyPolicy(policy); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	f.h.Register(mux)
	code, listed := do(t, mux, f.h.Store, http.MethodGet, "/admin/api/proxies", nil)
	if code != http.StatusOK {
		t.Fatal("读取代理失败")
	}
	for _, item := range listed["profiles"].([]any) {
		profile := item.(map[string]any)
		if profile["id"] == f.bad.ID {
			if profile["auto_delete_protected"] != false || profile["auto_delete_protection"] != "default_proxy" {
				t.Fatalf("已有默认节点应自动保护，无需管理员重新勾选：%v", profile)
			}
			return
		}
	}
	t.Fatal("默认节点丢失")
}
