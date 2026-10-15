package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/store"
)

func TestCLIImportRegistersInitializationWithoutClaiming(t *testing.T) {
	oldDB, oldData := config.DBPath, config.DataDir
	config.DataDir = t.TempDir()
	config.DBPath = filepath.Join(config.DataDir, "accounts.db")
	t.Cleanup(func() { config.DBPath, config.DataDir = oldDB, oldData })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("CLI 导入不得隐式领取")
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()
	st, err := store.New()
	if err != nil {
		t.Fatal(err)
	}
	fixed, err := st.AddProxyProfile("固定", server.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	policy := store.DefaultNewAccountProxyPolicy()
	policy.Enabled = true
	policy.DefaultProxyID = fixed.ID
	if _, err := st.SaveNewAccountProxyPolicy(policy); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	token := "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"cli-import"}`)) + ".sig"
	data, err := json.Marshal(map[string]any{"providers": map[string]any{"zai": []map[string]any{{"name": "CLI 导入", "secret": token, "disabled_models": []string{"other-model"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "synthetic-accounts.json")
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	cmdImport([]string{file})
	cmdImport([]string{file})
	reopened, err := store.New()
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	accounts := reopened.ListAccounts("")
	if len(accounts) != 1 {
		t.Fatalf("重复 CLI 导入没有判重：%d", len(accounts))
	}
	a := accounts[0]
	task := reopened.Initialization(a.ID)
	if task == nil || task.State != "queued" || task.Revision != 1 || a.IsSelectable(time.Now()) || len(a.DisabledModels) != 1 {
		t.Fatalf("CLI 未保存可恢复的初始化及导入元数据：%+v", task)
	}
}
