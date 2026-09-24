// 伪装身份卫生：平台 / 架构 / 内核串 / 语言 / 时区 / 发布通道只允许有一个来源
// （本包 profile.go 与 config.go 的定义处）。
//
// 动机：官方客户端把请求头与 body 的平台信息都派生自 (platform, arch, os.release())，
// 所以它**结构上不可能**自相矛盾；而我方曾把同一批语义散落在 upstream / quota /
// claim / telemetry 四处各写一份，长出了「body 说 linux-x64、请求头说 win32-x64」
// 这种上游可直接交叉比对的破绽（见 docs/plan-client-format-parity.md）。
//
// 本用例的作用与 version_test.go 的 TestNoHardcodedClientVersion 相同：
// 升级伪装身份时若漏改某处硬编码，直接变红。
package config

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestNoHardcodedSpoofLiterals 生产代码不得出现伪装身份字面量。
//
// 刻意只扫 .go：前端 UI 与实际下发内容不在此列。也刻意**不**禁 "windows" /
// "linux" / "linux-x64" 这些裸词——captcha 的浏览器下载与指纹逻辑合法使用它们
// （那是运行环境判定，不是伪装身份）；被禁的是伪装身份特有的取值形态。
func TestNoHardcodedSpoofLiterals(t *testing.T) {
	// 每一项都对应 ZcodeProfile 的一个派生值或字段。
	//
	// 刻意不含 "windows-x64" / "linux-x64" / "darwin-arm64"：captcha 的
	// platformTag() 用它们表示**构建主机**的浏览器下载标签（来自 runtime.GOOS），
	// 与伪装身份无关。被禁的只有伪装身份特有的取值形态。
	banned := []*regexp.Regexp{
		regexp.MustCompile(`"win32-x64"`),      // Profile.Platform() 的默认结果
		regexp.MustCompile(`"win32-arm64"`),    // 同上，arm64 变体
		regexp.MustCompile(`"win64-x64"`),      // 历史上的错误变体
		regexp.MustCompile(`"windows-x86_64"`), // Profile.QueryPlatform() 的默认结果
		regexp.MustCompile(`"10\.0\.26100"`),   // Profile.OSRelease
		regexp.MustCompile(`"zh-CN"`),          // Profile.Language
		regexp.MustCompile(`"Asia/Shanghai"`),  // Profile.Timezone
		regexp.MustCompile(`"cmd\.exe"`),       // Profile.Shell
		regexp.MustCompile(`"production"`),     // Profile.ReleaseChannel
		regexp.MustCompile(`"Z Code@"`),        // Profile.Title() 前缀
	}
	// 定义处豁免：这些取值只能在这里写一次。
	skipFiles := map[string]bool{
		"config.go":  true,
		"profile.go": true,
	}
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
		// 测试夹具允许出现字面量（如故意伪装的透传头、哨兵值）。
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") || skipFiles[filepath.Base(p)] {
			return nil
		}
		raw, readErr := os.ReadFile(p)
		if readErr != nil {
			return nil
		}
		for i, line := range strings.Split(string(raw), "\n") {
			// 注释行豁免：文档与解释性注释里提到取值是必要的。
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			for _, re := range banned {
				if re.MatchString(line) {
					bad = append(bad, filepath.ToSlash(p)+":"+itoa(i+1)+" "+trimmed)
					break
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历源码失败: %v", err)
	}
	if len(bad) > 0 {
		t.Fatalf("发现硬编码的伪装身份字面量（应改用 config.ZcodeClientProfile() 派生）：\n%s",
			strings.Join(bad, "\n"))
	}
}
