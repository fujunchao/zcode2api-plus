package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"zcode2api/internal/captcha"
	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/store"
)

func TestCLIProxyEndpointClaimFailureReassignsAccount(t *testing.T) {
	oldDB, oldData, oldBase := config.DBPath, config.DataDir, config.ZcodeBillingBase
	config.DataDir = t.TempDir()
	config.DBPath = filepath.Join(config.DataDir, "accounts.db")
	t.Cleanup(func() { config.DBPath, config.DataDir, config.ZcodeBillingBase = oldDB, oldData, oldBase })
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/preview") {
			_, _ = w.Write([]byte(`{"code":0,"data":{"plans":[{"plan_id":"promo","priority":1}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":3012,"msg":"suspicious request"}`))
	}))
	t.Cleanup(up.Close)
	config.ZcodeBillingBase = up.URL
	st, err := store.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	bad, _ := st.AddProxyProfile("bad", up.URL, true)
	good, _ := st.AddProxyProfile("good", "http://127.0.0.1:18081", true)
	a, _ := st.AddAccount(model.ProviderZai, "cli", "header.payload.sig") // 无 user_id，不触发外部遥测。
	_, _ = st.AssignProxyProfile(a.ID, bad.ID)
	cm := captcha.NewManager()
	cm.SetConfigProvider(func(context.Context) (captcha.Config, error) { return captcha.DefaultConfig, nil })
	if err := cm.SetManualParam("synthetic-captcha", "cn"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cm.Close() })
	outcomes := claimCLIAccount(st, st.FindAny(a.ID), cm)
	if len(outcomes) != 1 || outcomes[0]["proxy_removed"] != bad.ID || len(st.ListProxyProfiles()) != 1 {
		t.Fatalf("CLI 真实领取边界必须执行共同的代理故障收尾：%v", outcomes)
	}
	got := st.FindAny(a.ID)
	if got.ProxyID == nil || *got.ProxyID != good.ID {
		t.Fatal("CLI 失败后应改派到可用线路")
	}
}
