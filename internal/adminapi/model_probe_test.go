package adminapi

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"zcode2api/internal/auth"
	"zcode2api/internal/captcha"
	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/quota"
	"zcode2api/internal/store"
)

// 从管理端真实入队，再经本地 HTTP 代理接收 405，不能把“收到了响应”显示为模型成功。
func TestModelProbeHTTP405DoesNotReportModelSuccess(t *testing.T) {
	oldDB, oldData, oldTarget := config.DBPath, config.DataDir, config.UpstreamZai
	config.DBPath, config.DataDir = filepath.Join(t.TempDir(), "accounts.db"), t.TempDir()
	config.UpstreamZai = "http://model.invalid/v1/messages"
	t.Cleanup(func() { config.DBPath, config.DataDir, config.UpstreamZai = oldDB, oldData, oldTarget })
	st, err := store.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "model.invalid" || r.Method != http.MethodPost {
			t.Errorf("模型探测目标错误：%s %s", r.Method, r.URL.Host)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = w.Write([]byte(`{"code":"3012","message":"Request has been blocked due to unusual activity."}`))
	}))
	t.Cleanup(proxyServer.Close)
	p, err := st.AddProxyProfile("隔离探测代理", proxyServer.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.AddAccount(model.ProviderZai, "合成探测账号", jwtTokenFor("model-probe-http"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.Update(a.Provider, a.ID, func(live *model.Account) {
		live.Quota = map[string]map[string]any{"glm-5.3-flash": {"remaining": 100000, "available": 100000}}
	})
	if err != nil {
		t.Fatal(err)
	}
	h := New(st, auth.New(st), captcha.NewManager(), quota.NewService(st))
	t.Cleanup(h.Close)
	mux := http.NewServeMux()
	h.Register(mux)
	status, settings := do(t, mux, st, http.MethodGet, "/admin/api/proxies/model-probe/settings", nil)
	if status != http.StatusOK || settings["enabled"] != false {
		t.Fatalf("模型巡检应默认关闭且有独立设置入口：status=%d body=%v", status, settings)
	}
	settings["enabled"], settings["interval_minutes"] = true, 17
	settings["mode"], settings["network_enabled"] = "observe", false // 模型 HTTP 专项；网络层由独立真实 HTTP 回归覆盖。
	settings["account_ids"], settings["target_proxy_ids"] = []string{a.ID}, []string{p.ID}
	status, saved := do(t, mux, st, http.MethodPut, "/admin/api/proxies/model-probe/settings", settings)
	if status != http.StatusOK || saved["interval_minutes"] != float64(17) {
		t.Fatalf("自定义巡检间隔未保存：%d %v", status, saved)
	}
	status, result := do(t, mux, st, http.MethodPost, "/admin/api/proxies/"+p.ID+"/model-probe", nil)
	if status != http.StatusAccepted {
		t.Fatalf("应异步接收探测：%d %v", status, result)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, history := do(t, mux, st, http.MethodGet, "/admin/api/proxies/"+p.ID+"/model-probe/history", nil)
		if status == http.StatusOK {
			if runs, ok := history["runs"].([]any); ok && len(runs) > 0 {
				run := runs[0].(map[string]any)
				if samples, ok := run["samples"].([]any); ok && len(samples) > 0 {
					sample := samples[0].(map[string]any)
					if sample["kind"] != "risk_control" || sample["status"] != float64(405) || sample["complete"] != false {
						t.Fatalf("405 不得冒充模型成功：%v", sample)
					}
					if len(st.ListProxyProfiles()) != 1 {
						t.Fatal("单次 405 或观察模式不得删除代理")
					}
					if got := st.FindAny(a.ID); got.ProxyID != nil || got.ProxyURL != nil || got.UseCount != 0 {
						t.Fatal("探测不得改绑或虚增业务调用次数")
					}
					return
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("未观察到真实模型 HTTP 的分类结果")
}
