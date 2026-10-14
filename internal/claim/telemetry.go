// Package claim 激活遥测上报：官方 event/report 端点的单一事实源（对应
// Python 版 app/telemetry.py）。活动套餐投放疑似以「官方客户端当日活跃」
// 为资格信号，preview 前模拟 app_launch / app_daily_active / app_login_success
// 三个事件。端点不校验登录态，请求不带 Authorization。
//
// 出站口径与 billing 一致：事件经 Service.clientFor(acc) 走**账号绑定的代理
// 线路**。真实桌面客户端的 event/report 与 billing/preview 永远来自同一个 IP；
// 此前事件用独立的裸直连客户端，账号绑了线路时同一 device_mid 会从两个不同
// IP 出现（2026-09-23 事故：激活事件全部成功、preview 恒返回空套餐，
// docs/releases/v2.5.1-go.md）。
package claim

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"

	"zcode2api/internal/config"
)

// EventReportURL 激活事件上报端点（测试可覆写指向 mock）。
var EventReportURL = "https://zcode.z.ai/api/v1/event/report"

// ActivationElements preview 前依次上报的激活事件。
//
// 第三个 app_login_success 是 2026-10-12 由第三方实现（zcode-pool）对照补齐的：
// 官方客户端这一轮共上报这三个元素，此前只发前两个。顺序即上报顺序——上游按序
// 消费，且首个失败即中止整轮（见 ReportActivationEvents）。
var ActivationElements = []string{"app_launch", "app_daily_active", "app_login_success"}

// activationScreen 桌面端常见分辨率；上游仅做形态校验，固定值即可。
const activationScreen = "2560x1440"

// activationLoginElement 需要走「登录」形态的事件名：它的 event_region 与
// event_extra_detail 与其余事件不同。
const activationLoginElement = "app_login_success"

// activationRegion 事件体的 event_region：登录事件归入 app_profile，其余归入 app。
func activationRegion(element string) string {
	if element == activationLoginElement {
		return "app_profile"
	}
	return "app"
}

// activationExtraDetail 事件体的 event_extra_detail：登录事件需声明登录入口，
// 其余为空对象。
//
// 登录入口写死 "zai"：本网关的账号只有 zai / bigmodel 两种 provider，而活动
// 套餐投放只对 zai 侧生效，与官方客户端上报的口径一致。刻意**不为此在
// model.Account 上加字段**——那会牵连账号结构的五处同步（json tag / PublicView /
// accounts.data / 导出格式 / Clone），为一个只用于遥测的常量不值当。
func activationExtraDetail(element string) map[string]any {
	if element == activationLoginElement {
		return map[string]any{"login_provider": "zai"}
	}
	return map[string]any{}
}

// DeviceOSCategory 把客户端平台标识（win32-x64 / darwin-arm64 / linux-x64）映射为
// 事件体的 device_os_category。
//
// 刻意**不跟随 runtime.GOOS**：网关多跑在 Linux 容器里，而请求头与 UA 都声称桌面
// 客户端，跟随运行环境会让 body 与头自相矛盾（上游可交叉比对）。仅当平台标识无法
// 识别时才退回运行环境，至少保证有自洽的真值。
func DeviceOSCategory(platform string) string {
	p := strings.ToLower(strings.TrimSpace(platform))
	switch {
	case strings.HasPrefix(p, "win32"), strings.HasPrefix(p, "win64"),
		strings.HasPrefix(p, "windows"):
		return "windows"
	case strings.HasPrefix(p, "darwin"), strings.HasPrefix(p, "mac"),
		strings.HasPrefix(p, "osx"):
		return "macos"
	case strings.HasPrefix(p, "linux"):
		return "linux"
	}
	switch runtime.GOOS {
	case "windows":
		return "windows"
	case "darwin":
		return "macos"
	}
	return "linux"
}

// BuildActivationEventBody 激活事件体（官方 sendReport 字段集固定这 16 个）。
// 设备字段取自 config 的伪装配置而非运行环境（理由见 config 与 DeviceOSCategory）：
// 时区/语言/系统版本空着或跟随容器，会与请求头的 win32-x64 形成可交叉比对的矛盾。
func BuildActivationEventBody(element, userID, deviceMid string) map[string]any {
	return map[string]any{
		"event_id":           newUUID(),
		"client_timezone":    config.ZcodeClientTimezone,
		"client_language":    config.ZcodeClientLanguage,
		"element_name":       element,
		"event_region":       activationRegion(element),
		"event_type":         "view",
		"event_text":         "",
		"event_extra_detail": activationExtraDetail(element),
		"user_id":            userID,
		"screen_resolution":  activationScreen,
		"app_version":        config.ZcodeClientVersion,
		"device_os_category": DeviceOSCategory(config.ZcodeClientPlatform),
		"device_os_version":  config.ZcodeClientOSVersion,
		"device_mid":         deviceMid,
		"mac_id":             "",
		"marketing_params":   "{}",
	}
}

// PostActivationEvent 单条激活事件上报（无 Authorization）。
// client 必须与 billing 请求同源（Service.clientFor），保证两者同 IP 出站。
// HTTP >= 400 或业务码非 0 返回错误；调用方决定容错策略。
func PostActivationEvent(client HTTPClient, userID, element, deviceMid string) error {
	return PostActivationEventContext(context.Background(), client, userID, element, deviceMid)
}

func PostActivationEventContext(ctx context.Context, client HTTPClient, userID, element, deviceMid string) error {
	body, err := json.Marshal(BuildActivationEventBody(element, userID, deviceMid))
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, EventReportURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	text := string(raw)
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("event/report %s HTTP %d: %s", element, res.StatusCode, truncateStr(text, 120))
	}
	var parsed map[string]any
	_ = json.Unmarshal(raw, &parsed)
	if code := BusinessCode(parsed); code != 0 {
		return fmt.Errorf("event/report %s 业务码异常(%d): %s", element, code, truncateStr(text, 120))
	}
	return nil
}

// BusinessCode 上游业务码；非对象 JSON / 缺 code / 非数字 → -1（视为失败）。
func BusinessCode(body map[string]any) int {
	if body == nil {
		return -1
	}
	return toInt(body["code"])
}

// toInt 宽松取整数（float64/int/string 数字形态）。
func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		var out int
		if _, err := fmt.Sscanf(strings.TrimSpace(n), "%d", &out); err == nil {
			return out
		}
	}
	return -1
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
