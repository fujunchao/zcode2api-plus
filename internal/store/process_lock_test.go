package store

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"zcode2api/internal/config"
)

// 子进程只访问父测试明确提供的临时库。
func TestProcessStoreHelper(t *testing.T) {
	path := os.Getenv("ZCODE_TEST_LOCK_PATH")
	if path == "" {
		return
	}
	config.DBPath = path
	s, err := NewExclusive()
	if errors.Is(err, ErrDatabaseInUse) {
		fmt.Println("locked")
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fmt.Println("opened")
	if os.Getenv("ZCODE_TEST_LOCK_HOLD") == "1" {
		_, _ = io.Copy(io.Discard, os.Stdin)
	}
}

func TestExclusiveStoreAcrossProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.db")
	old := config.DBPath
	config.DBPath = path
	t.Cleanup(func() { config.DBPath = old })
	s, err := NewExclusive()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	child := exec.Command(os.Args[0], "-test.run=^TestProcessStoreHelper$")
	child.Env = append(os.Environ(), "ZCODE_TEST_LOCK_PATH="+path)
	out, err := child.Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "locked\nPASS\n" {
		t.Fatalf("子进程应明确拒绝并发打开：%q", out)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewExclusive()
	if err != nil {
		t.Fatal(err)
	}
	_ = reopened.Close()
}

func TestExclusiveStoreReleasedAfterProcessDeath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.db")
	child := exec.Command(os.Args[0], "-test.run=^TestProcessStoreHelper$")
	child.Env = append(os.Environ(), "ZCODE_TEST_LOCK_PATH="+path, "ZCODE_TEST_LOCK_HOLD=1")
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	input, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "opened\n" {
		t.Fatalf("子进程启动失败：%q %v", line, err)
	}
	_ = child.Process.Kill()
	_ = child.Wait()
	old := config.DBPath
	config.DBPath = path
	defer func() { config.DBPath = old }()
	s, err := NewExclusive()
	if err != nil {
		t.Fatalf("异常退出后应自动释放锁：%v", err)
	}
	_ = s.Close()
}
