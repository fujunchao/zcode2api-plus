// 伪装档案的派生公式测试。
//
// 官方客户端的请求头与 body 全部派生自 (platform, arch, os.release())，
// 因此**同一个三元组在任何消费点都必须得到同一个值**——本文件的表格就是
// 那条派生关系的人工核对版；profile_literals_test.go 负责防止有人绕开它。
package config

import (
	"os"
	"strings"
	"testing"
)

// TestZcodeProfileDerivations 钉住派生公式，含两种平台串形态的差异。
func TestZcodeProfileDerivations(t *testing.T) {
	cases := []struct {
		name              string
		bare, arch        string
		wantPlatform      string // 请求头 X-Platform
		wantOSCategory    string // 请求头 X-Os-Category
		wantQueryPlatform string // 查询参数 platform=
		wantEnvOSVersion  string // # Environment 段的 OS Version
	}{
		{"windows-x64", "win32", "x64", "win32-x64", "windows", "windows-x86_64", "win32 10.0.26100 x64"},
		{"windows-arm64", "win32", "arm64", "win32-arm64", "windows", "windows-aarch64", "win32 10.0.26100 arm64"},
		{"macos-arm64", "darwin", "arm64", "darwin-arm64", "macos", "darwin-aarch64", "darwin 10.0.26100 arm64"},
		{"linux-x64", "linux", "x64", "linux-x64", "linux", "linux-x86_64", "linux 10.0.26100 x64"},
		{"arch-empty", "win32", "", "win32", "windows", "windows", "win32 10.0.26100 "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := ZcodeProfile{BarePlatform: tc.bare, Arch: tc.arch, OSRelease: "10.0.26100"}
			if got := p.Platform(); got != tc.wantPlatform {
				t.Errorf("Platform()=%q，期望 %q", got, tc.wantPlatform)
			}
			if got := p.OSCategory(); got != tc.wantOSCategory {
				t.Errorf("OSCategory()=%q，期望 %q", got, tc.wantOSCategory)
			}
			if got := p.QueryPlatform(); got != tc.wantQueryPlatform {
				t.Errorf("QueryPlatform()=%q，期望 %q", got, tc.wantQueryPlatform)
			}
			if got := p.EnvOSVersion(); got != tc.wantEnvOSVersion {
				t.Errorf("EnvOSVersion()=%q，期望 %q", got, tc.wantEnvOSVersion)
			}
			// EnvPlatform 必须是裸平台名：填成复合形态（win32-x64）与官方不符。
			if got := p.EnvPlatform(); got != tc.bare {
				t.Errorf("EnvPlatform()=%q，期望裸平台名 %q", got, tc.bare)
			}
		})
	}
}

// TestQueryPlatformIsNotHeaderPlatform 两套平台串格式**刻意不同**，
// 历史上这两处共用一个常量。若有人把它们「简化」成同一个，本用例变红。
func TestQueryPlatformIsNotHeaderPlatform(t *testing.T) {
	p := ZcodeProfile{BarePlatform: "win32", Arch: "x64"}
	if p.Platform() == p.QueryPlatform() {
		t.Fatalf("X-Platform 与查询 platform= 不应同值：两者都是 %q", p.Platform())
	}
}

// TestDefaultProfileIsSelfConsistent 默认配置下，档案派生出的 X-Platform 必须等于
// 既有的复合变量——否则「头用一份、body 用另一份」的旧矛盾会重新长出来。
// 允许用 ZCODE_CLIENT_PLATFORM 单独覆盖（旧部署兼容），故仅在未覆盖时强校验。
func TestDefaultProfileIsSelfConsistent(t *testing.T) {
	if os.Getenv("ZCODE_CLIENT_PLATFORM") != "" {
		t.Skip("部署显式覆盖了 ZCODE_CLIENT_PLATFORM，跳过默认一致性校验")
	}
	got := ZcodeClientProfile().Platform()
	if got != ZcodeClientPlatform {
		t.Fatalf("档案 Platform()=%q 与 ZcodeClientPlatform=%q 不一致："+
			"请求头与 body 会声称不同平台", got, ZcodeClientPlatform)
	}
}

// TestZcodeClientProfileTracksVars 档案必须每次读变量、不做缓存：
// 后台「系統設定」与测试夹具都会改这些包级变量。
func TestZcodeClientProfileTracksVars(t *testing.T) {
	oldBare, oldArch := ZcodeClientBarePlatform, ZcodeClientArch
	oldShell, oldCWD := ZcodeClientShell, ZcodeClientCWD
	t.Cleanup(func() {
		ZcodeClientBarePlatform, ZcodeClientArch = oldBare, oldArch
		ZcodeClientShell, ZcodeClientCWD = oldShell, oldCWD
	})

	ZcodeClientBarePlatform, ZcodeClientArch = "darwin", "arm64"
	ZcodeClientShell, ZcodeClientCWD = "/bin/zsh", "/home/dev/demo"

	p := ZcodeClientProfile()
	if p.Platform() != "darwin-arm64" || p.OSCategory() != "macos" {
		t.Fatalf("档案未跟随变量变化: %+v", p)
	}
	if p.EnvPlatform() != "darwin" || p.Shell != "/bin/zsh" || p.CWD != "/home/dev/demo" {
		t.Fatalf("档案字段未跟随变量变化: %+v", p)
	}
}

// TestZcodeProfileTitle X-Title 形态：`Z Code@${sourceTitle}`。
func TestZcodeProfileTitle(t *testing.T) {
	if got := (ZcodeProfile{SourceTitle: "cli"}).Title(); got != "Z Code@cli" {
		t.Fatalf("Title()=%q", got)
	}
	if got := (ZcodeProfile{SourceTitle: "electron"}).Title(); got != "Z Code@electron" {
		t.Fatalf("Title()=%q", got)
	}
}

// TestZcodeSourceHeadersShape 非模型端点（配置 / 额度 / 领取 / 遥测）的来源头必须
// 恰好是官方那 10 项、取值全部由档案派生，且**不含** X-ZCode-Agent（只属模型端点）。
func TestZcodeSourceHeadersShape(t *testing.T) {
	headers := ZcodeSourceHeaders()

	wantKeys := []string{
		"User-Agent", "HTTP-Referer", "X-Title", "X-Release-Channel",
		"X-Client-Language", "X-Client-Timezone", "X-Platform",
		"X-Os-Category", "X-Os-Version", "X-ZCode-App-Version",
	}
	if len(headers) != len(wantKeys) {
		t.Fatalf("来源头应为 %d 项，实际 %d: %v", len(wantKeys), len(headers), headers)
	}
	for _, key := range wantKeys {
		if strings.TrimSpace(headers[key]) == "" {
			t.Errorf("来源头缺 %s 或取值为空: %v", key, headers)
		}
	}

	p := ZcodeClientProfile()
	for key, expected := range map[string]string{
		"HTTP-Referer":        ZcodeEndpointOrigin,
		"X-Title":             p.Title(),
		"X-Release-Channel":   p.ReleaseChannel,
		"X-Client-Language":   p.Language,
		"X-Client-Timezone":   p.Timezone,
		"X-Platform":          p.Platform(),
		"X-Os-Category":       p.OSCategory(),
		"X-Os-Version":        p.OSRelease,
		"X-ZCode-App-Version": p.AppVersion,
	} {
		if headers[key] != expected {
			t.Errorf("%s = %q，期望 %q（必须由档案派生，不能另写一份）", key, headers[key], expected)
		}
	}
	// 非模型端点的 UA 是裸串：带 ai-sdk 后缀是模型端点的形态，两者不可混用。
	if strings.Contains(headers["User-Agent"], "ai-sdk/") {
		t.Errorf("非模型端点 UA 应为裸串，实际 %q", headers["User-Agent"])
	}

	// 每次调用返回新 map：调用方会往里补 Content-Type / X-Device-Mid / Authorization。
	headers["__polluted__"] = "x"
	if _, ok := ZcodeSourceHeaders()["__polluted__"]; ok {
		t.Error("ZcodeSourceHeaders 返回了共享 map，一个请求会污染后续所有请求")
	}
}
