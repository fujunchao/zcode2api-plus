// 版本号卫生：伪装版本只能有一个来源（本包的 ZcodeClientVersion）。
// 升级版本号时若漏改某处硬编码，本文件的两个用例会直接变红。
package config

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestUserAgentDerivesFromClientVersion 默认 UA 必须由版本号拼接，不能另写一份。
func TestUserAgentDerivesFromClientVersion(t *testing.T) {
	if want := "ZCode/" + ZcodeClientVersion; UserAgent != want {
		t.Fatalf("UserAgent=%q，应等于 %q", UserAgent, want)
	}
}

// TestClientVersionShape 版本号须为三段纯版本串（不带 v 前缀、不带后缀），
// 与客户端 X-ZCode-App-Version / app_version 参数的形态一致。
func TestClientVersionShape(t *testing.T) {
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(ZcodeClientVersion) {
		t.Fatalf("ZcodeClientVersion=%q 不是三段版本号", ZcodeClientVersion)
	}
}

// TestNoHardcodedClientVersion 三段式版本字面量只允许出现在本包 config.go（定义处）；
// 生产代码其它位置必须引用常量——这是「升版本必全量生效」的静态保证。
// 刻意跳过 _test.go：测试夹具允许出现字面量（如故意伪装的透传头、OS 版本哨兵值）。
func TestNoHardcodedClientVersion(t *testing.T) {
	pat := regexp.MustCompile(`"\d+\.\d+\.\d+"`)
	skipDirs := map[string]bool{
		".git": true, "node_modules": true, "frontend": true, "dist": true,
		"zcode-switch": true, ".workbuddy": true, "data": true,
	}
	var bad []string
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") ||
			filepath.Base(p) == "config.go" {
			return nil
		}
		raw, readErr := os.ReadFile(p)
		if readErr != nil {
			return nil
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if pat.MatchString(line) {
				bad = append(bad, filepath.ToSlash(p)+":"+itoa(i+1)+" "+strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历源码失败: %v", err)
	}
	if len(bad) > 0 {
		t.Fatalf("发现硬编码版本字面量（应改用 ZcodeClientVersion 常量）：\n%s", strings.Join(bad, "\n"))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
