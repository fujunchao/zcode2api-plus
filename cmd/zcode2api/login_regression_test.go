package main

import (
	"encoding/base64"
	"path/filepath"
	"testing"
	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/oauth"
	"zcode2api/internal/store"
)

func TestCLIReloginRefreshesJWTWithoutReplacingAccount(t *testing.T) {
	old := config.DBPath
	config.DBPath = filepath.Join(t.TempDir(), "accounts.db")
	defer func() { config.DBPath = old }()
	st, err := store.New()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	token := "header." + base64.RawURLEncoding.EncodeToString([]byte("{\"sub\":\"same-user\"}")) + ".old"
	a, _ := st.AddAccount(model.ProviderZai, "custom-name", token)
	proxyURL := "http://127.0.0.1:1234"
	_, _ = st.SetProxyURL(a.Provider, a.ID, &proxyURL)
	_, _ = st.Update(a.Provider, a.ID, func(a *model.Account) { a.Status = model.StatusInvalid; a.RiskControlStreak = 4; a.UseCount = 42 })
	before := st.FindAny(a.ID)
	fresh := token + "-new"
	got, isNew, err := saveCLILogin(st, &oauth.ExchangeResult{Token: fresh})
	if err != nil {
		t.Fatal(err)
	}
	if isNew || got.ID != a.ID || len(st.ListAccounts("")) != 1 {
		t.Fatal("重登必须命中原账号")
	}
	current := st.FindAny(a.ID)
	if current.Secret() != fresh || current.Status != model.StatusActive || current.RiskControlStreak != 0 {
		t.Fatal("重登未替换旧 JWT 或清除失效状态")
	}
	if current.Name != "custom-name" || current.UseCount != 42 || *current.VirtualDeviceMid != *before.VirtualDeviceMid || *current.ProxyURL != proxyURL {
		t.Fatal("重登改动了非凭据数据")
	}
	reopened, err := store.New()
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.FindAny(a.ID).Secret() != fresh {
		t.Fatal("新 JWT 未持久化")
	}
}
