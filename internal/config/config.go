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
	// 加下界：0 或负数会让每次求解的 deadline 立刻到期（本项与同组其它超时项一致）。
	CaptchaSolveTimeout = max(1, envInt("ZCODE_CAPTCHA_TIMEOUT", 40)) // 每次求解超时（秒）

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
	// AsyncForceDirect 强制 async 池忽略账号代理、恒直连上游。
	// 只是默认值：落库后以后台「系統設定」的开关为准（store.AsyncForceDirect）。
	AsyncForceDirect = envBool("ZCODE_ASYNC_FORCE_DIRECT", false)
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

// ── 代理线路自动巡检 ────────────────────────────────────────────────────────
// 开启后周期性对全部「启用」线路做 z.ai 可达性检测：不通过的线路自动移除，
// 其绑定账号按「空閒优先、其次绑定账号数最少」改派（全部线路不可用且直连
// 也不可达时视为本机网络故障，跳过该轮移除，防止把整个线路池清空）。
var (
	ProxyHealthEnabled = envBool("ZCODE_PROXY_HEALTH_ENABLED", true)
	// 巡检间隔（分钟）。
	ProxyHealthIntervalMinutes = max(1, envInt("ZCODE_PROXY_HEALTH_INTERVAL", 30))
)

// ── 上游风控冷却 ────────────────────────────────────────────────────────────
// 上游用 HTTP 405 + "unusual activity" 表达风控拦截。它看的是身份维度（账号、
// 设备指纹、出口 IP、请求头），与请求的模型无关，所以处置是停整个账号。
//
// 冷却时长按连续命中次数递进：第 N 次命中取第 N 档；连续次数**超过档位数**则把
// 账号置为 invalid（需人工介入）。也就是说档位数同时就是升级点——想给账号多一次
// 自证机会就多加一档。默认 3 档 = 5 / 15 / 60 分钟。
//
// 与领取冷却同一约定：环境变量只是默认值，后台写入后以落库值为准。
var (
	RiskCoolingSteps = env("ZCODE_RISK_COOLING_STEPS", "300,900,3600")
)

// ── 上游 503 冷却阶梯 ────────────────────────────────────────────────────────
// 上游 503（服务不可用）是上游健康信号而非账号问题。此前一律固定冷却
// CoolingSeconds（300s），上游抖动时个位数账号池会在几十秒内被整池冷却清空
// （2026-09-20 线上事故）。改为按连续命中次数递进：第 N 次取第 N 档，
// 超过档位数封顶 CoolingSeconds（与硬故障惩罚持平，不升 invalid）。
//
// 与风控阶梯同一约定：环境变量只是默认值，后台写入后以落库值为准。
var (
	Upstream503CoolingSteps = env("ZCODE_UPSTREAM_503_COOLING_STEPS", "30,60,120")
)

// ── 线路断流熔断与账号短回避 ────────────────────────────────────────────────
// 2026-09-22 事故定论（docs/analysis-flash-5min-stream-cut-20260921.md 09-22 附录）：
// 某线路带 ~300s 连接时长上限，而断流不标账号 + 额度优先调度黏住「最富」账号，
// 客户端的 TRANSPORT 盲重试三次全部撞同一条线路。两层处置：
//
//   - 线路级熔断：同一线路**连续** N 次（默认 3，完整成功交付后归零）上游侧断流
//     → 移除该线路并改派绑定账号（复用 proxy-health 的 PurgeProxyProfiles 原子路径）。
//     责任在线路而非账号：多账号可共享一条线路，按线路聚合计数才能命中真凶，
//     且移除线路不会把上游的锅变成对账号的惩罚。
//   - 账号短回避：断流后的 ~60s 内该账号暂不被选号（仅选号层软过滤，不是冷却、
//     不标状态），让紧接着的客户端重试自然落到别的线路——比 N 连击熔断更早止血。
//
// 同一约定：环境变量只是默认值，后台「系統設定」写入后以落库值为准。
var (
	// LineTruncateStrikes 触发线路熔断的连续断流次数；0 = 关闭熔断。
	LineTruncateStrikes = envInt("ZCODE_LINE_TRUNCATE_STRIKES", 3)
	// LineTruncateAvoidSeconds 断流后账号选号回避时长（秒）；0 = 关闭回避。
	LineTruncateAvoidSeconds = envInt("ZCODE_LINE_TRUNCATE_AVOID_SECONDS", 60)
)

// ── 上游端点 ────────────────────────────────────────────────────────────────
var (
	UpstreamZai         = env("ZAI_UPSTREAM_URL", "https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages")
	UpstreamZaiFallback = env("ZAI_FALLBACK_URL", "https://api.z.ai/api/anthropic/v1/messages")

	// ZCode 计费 / 额度查询端点（不可配置，与官方客户端一致）。
	ZcodeBillingBase = "https://zcode.z.ai/api/v1/zcode-plan"

	// 官方客户端版本号：随请求头 / URL 参数上行，用于伪装成官方客户端。
	// 2026-09-17 由 3.7.7 升至 3.11.2，与 zcode-switch 的 CLIENT_APP_VERSION 对齐；
	// 2026-09-23 由 3.11.2 升至 3.14.3（本机实装客户端版本，激活失效排查项之一，
	// 见 docs/analysis-zcode-client-activation-diff.md）。
	ZcodeClientVersion = env("ZCODE_CLIENT_VERSION", "3.14.3")
	// 与 Python 版保持一致的客户端平台标识；旧的 win32 参数已失效。
	ZcodeClientPlatform = env("ZCODE_CLIENT_PLATFORM", "win32-x64")

	UserAgent = env("UPSTREAM_USER_AGENT", "ZCode/"+ZcodeClientVersion)

	// AppVersion 供 /meta 与后台展示；-go 后缀标识运行时版本。
	AppVersion = "2.5.2-go"
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
		// 写盘失败必须留下痕迹：该值随每个上游请求送出并用于激活上报，落不了盘时
		// 每次重启都会换一个（上游视为新装置），管理员只会看到「账号莫名被风控」，
		// 没有任何线索指向 DataDir 不可写。用 stderr 而非 internal/web：web 包依赖
		// 本包，反向引用会成环。
		if err := os.MkdirAll(DataDir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "[!] 设备指纹目录不可建（%s），本次使用临时指纹，重启后会变化: %v\n", DataDir, err)
		} else if err := os.WriteFile(path, []byte(mid), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "[!] 设备指纹写入失败（%s），本次使用临时指纹，重启后会变化: %v\n", path, err)
		}
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
