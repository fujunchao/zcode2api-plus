// 激活事件体的设备字段测试：核心契约是「跟随伪装配置，而不是跟随运行环境」。
// 回归背景：网关多跑在 Linux 容器里，若 device_os_category 取 runtime.GOOS、
// os_version 恒空、language 回落 en-US，就会与请求头的 X-Platform: win32-x64
// 形成可交叉比对的矛盾，上游据此判定非官方客户端而不投放活动套餐。
package claim

import (
	"runtime"
	"testing"

	"zcode2api/internal/config"
)

// hostOSCategory 复述「无法识别平台标识时退回运行环境」的期望值。
func hostOSCategory() string {
	switch runtime.GOOS {
	case "windows":
		return "windows"
	case "darwin":
		return "macos"
	}
	return "linux"
}

func TestDeviceOSCategoryFollowsPlatform(t *testing.T) {
	cases := []struct {
		platform string
		want     string
	}{
		{"win32-x64", "windows"},
		{"win32-arm64", "windows"},
		{"win64-x64", "windows"},
		{"windows-x64", "windows"},
		{"WIN32-X64", "windows"},
		{" win32-x64 ", "windows"},
		{"darwin-arm64", "macos"},
		{"darwin-x64", "macos"},
		{"macos-arm64", "macos"},
		{"osx-x64", "macos"},
		{"linux-x64", "linux"},
		{"linux-arm64", "linux"},
		// 无法识别时退回运行环境，保证至少有自洽的真值。
		{"", hostOSCategory()},
		{"   ", hostOSCategory()},
		{"freebsd-x64", hostOSCategory()},
	}
	for _, tc := range cases {
		if got := DeviceOSCategory(tc.platform); got != tc.want {
			t.Errorf("DeviceOSCategory(%q) = %q, 期望 %q", tc.platform, got, tc.want)
		}
	}
}

func TestBuildActivationEventBodyUsesDisguiseConfig(t *testing.T) {
	oldPlatform, oldVersion := config.ZcodeClientPlatform, config.ZcodeClientOSVersion
	oldLanguage, oldTimezone := config.ZcodeClientLanguage, config.ZcodeClientTimezone
	t.Cleanup(func() {
		config.ZcodeClientPlatform, config.ZcodeClientOSVersion = oldPlatform, oldVersion
		config.ZcodeClientLanguage, config.ZcodeClientTimezone = oldLanguage, oldTimezone
	})
	config.ZcodeClientPlatform = "win32-x64"
	config.ZcodeClientOSVersion = "10.0.26100"
	config.ZcodeClientLanguage = "zh-CN"
	config.ZcodeClientTimezone = "Asia/Shanghai"

	body := BuildActivationEventBody("app_launch", "u-1", "mid-1")

	want := map[string]any{
		"device_os_category": "windows",
		"device_os_version":  "10.0.26100",
		"client_language":    "zh-CN",
		"client_timezone":    "Asia/Shanghai",
		"app_version":        config.ZcodeClientVersion,
		"device_mid":         "mid-1",
		"user_id":            "u-1",
		"element_name":       "app_launch",
		"screen_resolution":  activationScreen,
	}
	for key, expected := range want {
		if got := body[key]; got != expected {
			t.Errorf("%s = %v, 期望 %v", key, got, expected)
		}
	}

	// 最关键的一条：跑在 Linux（CI/容器）上时也绝不能上报 linux——那正是回归点。
	if runtime.GOOS != "windows" && body["device_os_category"] == "linux" {
		t.Fatalf("设备分类不能跟随运行环境（当前 GOOS=%s）: %v", runtime.GOOS, body["device_os_category"])
	}
	// 字段集固定 16 个，不能因本次改造增减。
	if len(body) != 16 {
		t.Fatalf("激活事件体应固定 16 个字段，实际 %d: %v", len(body), body)
	}
}
