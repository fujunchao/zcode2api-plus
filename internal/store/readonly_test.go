package store

import (
	"path/filepath"
	"testing"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

// status 这类纯查询命令必须在「服务已持有独占锁」时也能打开数据库：
// 线上真实报错是 "数据库正在被其他服务或 CLI 使用"，而用户只是想知道账号数。
func TestNewReadOnlyReadsWhileExclusiveHeld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.db")
	oldDB, oldAdmin, oldGW := config.DBPath, config.AdminKeyEnv, config.GatewayKeyEnv
	config.DBPath, config.AdminKeyEnv, config.GatewayKeyEnv = path, "", ""
	t.Cleanup(func() {
		config.DBPath, config.AdminKeyEnv, config.GatewayKeyEnv = oldDB, oldAdmin, oldGW
	})

	writer, err := NewExclusive()
	if err != nil {
		t.Fatalf("独占打开失败: %v", err)
	}
	defer func() { _ = writer.Close() }()
	if _, err := writer.AddAccount(model.ProviderZai, "ro-account", jwtFor("u-ro")); err != nil {
		t.Fatalf("入池失败: %v", err)
	}

	ro, err := NewReadOnly()
	if err != nil {
		t.Fatalf("独占锁被持有时只读打开不应失败: %v", err)
	}
	defer func() { _ = ro.Close() }()

	if got := len(ro.ListAccounts(model.ProviderZai)); got != 1 {
		t.Fatalf("只读快照账号数 = %d, 期望 1", got)
	}
	if ro.AdminKey() == "" {
		t.Fatal("只读快照应能看到后台密钥")
	}
}

// 库不存在时必须返回错误（调用方据此回退到可写入口完成首次建库），不能静默建空库。
func TestNewReadOnlyMissingDatabaseFails(t *testing.T) {
	old := config.DBPath
	config.DBPath = filepath.Join(t.TempDir(), "missing", "accounts.db")
	t.Cleanup(func() { config.DBPath = old })

	if _, err := NewReadOnly(); err == nil {
		t.Fatal("数据库不存在时应返回错误")
	}
}
