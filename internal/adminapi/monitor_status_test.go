// 监控端点的浏览器状态必须反映真实运行态。
//
// 背景：旧实现只看 config.CaptchaBrowserEnabled，服务器实测出现过
// 「浏览器进程根本不存在，控制台仍显示 online」的假信号
// （见 docs/plan-captcha-browser-lifecycle-20261106.md 与
// findings/server-forensics-browser-20261106.md）。
package adminapi

import (
	"testing"

	"zcode2api/internal/captcha"
	"zcode2api/internal/config"
)

func TestBrowserServiceStateReflectsRealStatus(t *testing.T) {
	old := config.CaptchaBrowserEnabled
	t.Cleanup(func() { config.CaptchaBrowserEnabled = old })

	// 开关关闭：无论管理器状态如何都报 standby。
	config.CaptchaBrowserEnabled = false
	if status, _ := browserServiceState(captcha.NewManager()); status != "standby" {
		t.Fatalf("开关关闭应为 standby，实际 %q", status)
	}

	// 开关打开但池尚未懒启动：必须不是 online。
	config.CaptchaBrowserEnabled = true
	status, detail := browserServiceState(captcha.NewManager())
	if status == "online" {
		t.Fatalf("浏览器尚未拉起时不应报 online（detail=%q）", detail)
	}

	// 无管理器：给 unknown，不能 panic。
	if status, _ := browserServiceState(nil); status != "unknown" {
		t.Fatalf("无管理器应为 unknown，实际 %q", status)
	}
}
