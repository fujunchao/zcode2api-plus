// Package config 承载全部运行期配置：环境变量 + 默认值。
// 对应 Python 版 app/settings.py；账号与凭证不在此处，
// 而是持久化到数据目录（见 internal/store）。
package config

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if v == "" {
		return def
	}
	// 与 Python 版一致：非空且不在假值集合内的一律按真处理。
	switch v {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

// ── 目录 ────────────────────────────────────────────────────────────────────
// 数据目录：相对路径按进程工作目录解析并转为绝对路径。
// （与 Python 版的差异：Python 相对仓库根解析；Go 二进制无固定根。）
var DataDir = resolveDir("ZCODE_DATA_DIR", "data")

// DBPath 账号与设置数据库；测试可直接覆写。
var DBPath = filepath.Join(DataDir, "accounts.db")

func resolveDir(key, def string) string {
	raw := env(key, def)
	if filepath.IsAbs(raw) {
		return raw
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return raw
	}
	return abs
}

// ── 服务 ────────────────────────────────────────────────────────────────────
var (
	Port = envInt("ZCODE_PORT", 3000)
	Host = env("ZCODE_HOST", "0.0.0.0")
)

// ── 鉴权 ────────────────────────────────────────────────────────────────────
// 密钥初始值：仅在 meta 表缺失（或后台密码仍为历史默认值）时生效，写入后以数据库为准。
// 未设置环境变量时由 store 首次启动随机生成（见 internal/store 的引导逻辑）。
var (
	AdminKeyEnv   = env("ZCODE_ADMIN_KEY", "")
	GatewayKeyEnv = env("ZCODE_GATEWAY_KEY", "")
)

// ── 验证码 ──────────────────────────────────────────────────────────────────
var (
	CaptchaCacheTTL       = int64(envInt("CAPTCHA_CACHE_TTL", 45_000))         // ms，Node/人工令牌
	CaptchaConfigCacheTTL = int64(envInt("CAPTCHA_CONFIG_CACHE_TTL", 600_000)) // ms，上游配置
	CaptchaManualCacheTTL = int64(envInt("CAPTCHA_MANUAL_CACHE_TTL", 45_000))  // ms，人工回填

	CaptchaSolveRetries = envInt("ZCODE_CAPTCHA_RETRIES", 4)
	CaptchaSolveTimeout = envInt("ZCODE_CAPTCHA_TIMEOUT", 40) // 每次求解超时（秒）

	// 真实 Chromium（rod 驱动 cloakbrowser 下载的浏览器二进制）。
	CaptchaBrowserEnabled         = envBool("ZCODE_CAPTCHA_BROWSER", false)
	CaptchaBrowserWorkers         = max(1, envInt("ZCODE_CAPTCHA_BROWSER_WORKERS", 1))
	CaptchaBrowserStartupTimeout  = max(5, envInt("ZCODE_CAPTCHA_BROWSER_STARTUP_TIMEOUT", 90))
	CaptchaBrowserRequestTimeout  = max(5, envInt("ZCODE_CAPTCHA_BROWSER_REQUEST_TIMEOUT", 45))
	CaptchaBrowserQueueTimeout    = max(1, envInt("ZCODE_CAPTCHA_BROWSER_QUEUE_TIMEOUT", 60))
	CaptchaBrowserShutdownTimeout = max(1, envInt("ZCODE_CAPTCHA_BROWSER_SHUTDOWN_TIMEOUT", 10))
	CaptchaBrowserFailureCooldown = max(1, envInt("ZCODE_CAPTCHA_BROWSER_FAILURE_COOLDOWN", 60))
	// 显式指定浏览器可执行文件；缺省时按 cloakbrowser 缓存目录自动发现
	//（CLOAKBROWSER_BINARY_PATH / CLOAKBROWSER_CACHE_DIR，见 internal/captcha/solve.go）。
	CaptchaBrowserBin = env("ZCODE_CAPTCHA_BROWSER_BIN", "")
)

// ── Async 空闲池 ────────────────────────────────────────────────────────────
var (
	AsyncEnabled       = envBool("ZCODE_ASYNC_ENABLED", true)
	AsyncTicketTimeout = max(30, envInt("ZCODE_ASYNC_TICKET_TIMEOUT", 300))
	AsyncMaxRetries    = max(0, envInt("ZCODE_ASYNC_MAX_RETRIES", 3))
)

// ── 用量监控 ────────────────────────────────────────────────────────────────
var (
	QuotaRefreshInterval = envInt("ZCODE_QUOTA_REFRESH_INTERVAL", 60) // 秒，0=关闭
	CoolingSeconds       = envInt("ZCODE_COOLING_SECONDS", 300)       // 限流冷却（秒）
)

// ── 套餐领取 ────────────────────────────────────────────────────────────────
// 冷却只拦自动路径（账号入池触发的领取），手动点按钮始终可强制触发：
// 用户点了没反应是最糟的交互，冷却的目的是避免自动路径白打上游。
var (
	// 自动领取失败后的重试冷却（秒）：验证码不可用或已领过但上游没给 ends_at 时用。
	ClaimCaptchaCooldownSeconds = max(60, envInt("ZCODE_CLAIM_CAPTCHA_COOLDOWN", 3600))
	// 其他失败（网络/参数）后的冷却（秒）。
	ClaimRetryCooldownSeconds = max(30, envInt("ZCODE_CLAIM_RETRY_COOLDOWN", 600))
	// 「刷新资格」这个只读探测的冷却（秒），避免前端每次加载都打上游。
	ClaimPreviewCooldownSeconds = max(0, envInt("ZCODE_CLAIM_PREVIEW_COOLDOWN", 60))
)

// ── 上游端点 ────────────────────────────────────────────────────────────────
var (
	UpstreamZai         = env("ZAI_UPSTREAM_URL", "https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages")
	UpstreamZaiFallback = env("ZAI_FALLBACK_URL", "https://api.z.ai/api/anthropic/v1/messages")

	// ZCode 计费 / 额度查询端点（不可配置，与官方客户端一致）。
	ZcodeBillingBase = "https://zcode.z.ai/api/v1/zcode-plan"

	// 官方客户端版本号：随请求头 / URL 参数上行，用于伪装成官方客户端。
	// 2026-09-17 由 3.7.7 升至 3.11.2，与 zcode-switch 的 CLIENT_APP_VERSION 对齐。
	ZcodeClientVersion = env("ZCODE_CLIENT_VERSION", "3.11.2")
	// 与 Python 版保持一致的客户端平台标识；旧的 win32 参数已失效。
	ZcodeClientPlatform = env("ZCODE_CLIENT_PLATFORM", "win32-x64")

	UserAgent = env("UPSTREAM_USER_AGENT", "ZCode/"+ZcodeClientVersion)

	// AppVersion 供 /meta 与后台展示；-go 后缀标识运行时版本。
	AppVersion = "2.0.10-go"
)

// ── 遥测设备伪装 ────────────────────────────────────────────────────────────
// 激活事件体里的设备字段。刻意**不跟随运行环境**：网关多跑在 Linux 容器里，
// 而 X-Platform / User-Agent 都声称桌面客户端，跟随环境会让 body 与请求头
// 自相矛盾（上游可交叉比对，据此判定「非官方客户端」而不投放活动套餐）。
// 三项都应与 ZcodeClientPlatform 相符：Windows 客户端报内核串，如 10.0.26100。
var (
	ZcodeClientOSVersion = env("ZCODE_CLIENT_OS_VERSION", "10.0.26100")
	ZcodeClientLanguage  = env("ZCODE_CLIENT_LANGUAGE", "zh-CN")
	ZcodeClientTimezone  = env("ZCODE_CLIENT_TIMEZONE", "Asia/Shanghai")
)

// ── 设备身份 ────────────────────────────────────────────────────────────────
var (
	deviceMidOnce sync.Once
	deviceMid     string
)

// DeviceMid 返回固定设备 UUID（首次调用时生成并持久化到 <DataDir>/device_mid.txt）。
// 对应 Python 版 settings._load_or_generate_device_mid：固定标识避免频繁更换被风控。
func DeviceMid() string {
	deviceMidOnce.Do(func() {
		path := filepath.Join(DataDir, "device_mid.txt")
		if b, err := os.ReadFile(path); err == nil {
			if mid := strings.TrimSpace(string(b)); mid != "" {
				deviceMid = mid
				return
			}
		}
		mid := newUUID()
		_ = os.MkdirAll(DataDir, 0o755)
		_ = os.WriteFile(path, []byte(mid), 0o644)
		deviceMid = mid
	})
	return deviceMid
}

// NewDeviceMid 生成新的设备指纹，供每账号分配（见 model.Account.VirtualDeviceMid）。
// 与全局 DeviceMid() 不同：它不写 device_mid.txt，而是随账号存进 accounts.data。
func NewDeviceMid() string { return newUUID() }

// newUUID 生成 UUIDv4（不引入第三方依赖）。
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
