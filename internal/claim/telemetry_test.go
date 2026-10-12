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

// TestActivationElementsOrder 三个激活事件的顺序必须是官方口径：
// app_launch → app_daily_active → app_login_success。
// 顺序即上报顺序——首个失败会中止整轮上报（见 ReportActivationEvents）。
func TestActivationElementsOrder(t *testing.T) {
	want := []string{"app_launch", "app_daily_active", "app_login_success"}
	if len(ActivationElements) != len(want) {
		t.Fatalf("激活事件应为 %d 个，实际 %d: %v", len(want), len(ActivationElements), ActivationElements)
	}
	for i, w := range want {
		if ActivationElements[i] != w {
			t.Errorf("第 %d 个激活事件应为 %q，实际 %q", i, w, ActivationElements[i])
		}
	}
}

// TestBuildActivationEventBodyLoginProfile 登录事件的 event_region 与
// event_extra_detail 与其余事件不同：前者 app_profile + login_provider，
// 后者 app + 空对象；两者字段数都必须仍是 16（键集不随 element 变化）。
func TestBuildActivationEventBodyLoginProfile(t *testing.T) {
	login := BuildActivationEventBody(activationLoginElement, "u-1", "mid-1")
	if got := login["event_region"]; got != "app_profile" {
		t.Errorf("登录事件 event_region = %v，期望 app_profile", got)
	}
	detail, ok := login["event_extra_detail"].(map[string]any)
	if !ok || detail["login_provider"] != "zai" {
		t.Errorf("登录事件 event_extra_detail = %v，期望 login_provider=zai", login["event_extra_detail"])
	}
	if len(login) != 16 {
		t.Fatalf("登录事件体应固定 16 个字段，实际 %d: %v", len(login), login)
	}

	launch := BuildActivationEventBody("app_launch", "u-1", "mid-1")
	if got := launch["event_region"]; got != "app" {
		t.Errorf("非登录事件 event_region = %v，期望 app", got)
	}
	if detail, ok := launch["event_extra_detail"].(map[string]any); !ok || len(detail) != 0 {
		t.Errorf("非登录事件 event_extra_detail = %v，期望空对象", launch["event_extra_detail"])
	}
	if len(launch) != 16 {
		t.Fatalf("非登录事件体应固定 16 个字段，实际 %d: %v", len(launch), launch)
	}
}
