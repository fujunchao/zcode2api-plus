//go:build windows

package store

import (
	"os"
	"path/filepath"
	"testing"
)

// 只在 GitHub Actions Windows runner 验证同目录覆盖；不依赖运行账号或网络。
func TestStrictProxyWindowsJournalReplacesExistingWithoutDeleteWindow(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "strict.json")
	if err := os.WriteFile(target, []byte(`{"old":"removed"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{`{"next":"retiring"}`, `{"next":"removed"}`} {
		file, err := os.CreateTemp(dir, ".strict-proxy-")
		if err != nil {
			t.Fatal(err)
		}
		_, writeErr := file.WriteString(value)
		syncErr := file.Sync()
		closeErr := file.Close()
		if writeErr != nil || syncErr != nil || closeErr != nil {
			t.Fatalf("临时意图未完整持久化: %v %v %v", writeErr, syncErr, closeErr)
		}
		if err := replaceStrictJournal(file.Name(), target); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(target)
		if err != nil || string(got) != value {
			t.Fatalf("已有日志必须被完整替换: %q %v", got, err)
		}
	}
	if err := replaceStrictJournal(filepath.Join(dir, "missing"), target); err == nil {
		t.Fatal("源文件缺失必须报告失败")
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != `{"next":"removed"}` {
		t.Fatal("替换失败必须保留旧恢复意图")
	}
}
