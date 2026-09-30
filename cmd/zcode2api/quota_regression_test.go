package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/store"
)

func TestCLIQuotaPrintsFreshSnapshot(t *testing.T) {
	oldDB, oldData, oldBase := config.DBPath, config.DataDir, config.ZcodeBillingBase
	config.DataDir = t.TempDir()
	config.DBPath = filepath.Join(config.DataDir, "accounts.db")
	defer func() { config.DBPath, config.DataDir, config.ZcodeBillingBase = oldDB, oldData, oldBase }()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{\"code\":0,\"data\":{\"plans\":[],\"balances\":[{\"show_name\":\"glm-5.3-flash\",\"total_units\":1000,\"remaining_units\":750,\"available_units\":750}]}}"))
	}))
	defer up.Close()
	config.ZcodeBillingBase = up.URL
	st, err := store.New()
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.AddAccount(model.ProviderZai, "audit", "audit.e30.sig")
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	output := filepath.Join(t.TempDir(), "stdout.txt")
	f, err := os.Create(output)
	if err != nil {
		t.Fatal(err)
	}
	oldOut := os.Stdout
	os.Stdout = f
	cmdQuota()
	os.Stdout = oldOut
	f.Close()
	raw, _ := os.ReadFile(output)
	reloaded, err := store.New()
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	if len(reloaded.FindAny(a.ID).Quota) == 0 {
		t.Fatal("模拟上游没有成功更新额度")
	}
	if !strings.Contains(string(raw), "750") {
		t.Fatalf("库内已是 750，CLI 却仍打印刷新前快照：%s", raw)
	}
}
