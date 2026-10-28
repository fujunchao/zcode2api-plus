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
	"time"
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
	CaptchaCacheTTL = int64(envInt("CAPTCHA_CACHE_TTL", 45_000)) // ms，Node/人工令牌
	// 上游配置缓存：默认 1 小时（与官方客户端一致），env 可覆盖。
	// 只有成功响应才写缓存；失败保留 last-good，不写负缓存。
	CaptchaConfigCacheTTL = int64(envInt("CAPTCHA_CONFIG_CACHE_TTL", 3_600_000)) // ms，上游配置
	CaptchaManualCacheTTL = int64(envInt("CAPTCHA_MANUAL_CACHE_TTL", 45_000))    // ms，人工回填

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

	// 预求解 token 池（P0-B）。池按 sceneId|region|prefix 分桶，token 一次性、
	// TTL 到期即丢弃；池空时最多 1–2 路并发求解竞速。关闭后退回逐请求求解。
	CaptchaPoolEnabled       = envBool("ZCODE_CAPTCHA_POOL_ENABLED", true)
	CaptchaPoolTTL           = int64(max(10_000, envInt("ZCODE_CAPTCHA_POOL_TTL_MS", 95_000))) // ms
	CaptchaPoolMin           = max(1, envInt("ZCODE_CAPTCHA_POOL_MIN", 2))
	CaptchaPoolMax           = max(CaptchaPoolMin, envInt("ZCODE_CAPTCHA_POOL_MAX", 6))
	CaptchaPoolRefillMS      = max(200, envInt("ZCODE_CAPTCHA_POOL_REFILL_MS", 1_000))
	CaptchaPoolEmptySolve    = min(2, max(1, envInt("ZCODE_CAPTCHA_POOL_EMPTY_SOLVE", 2)))
	CaptchaPoolIdleShrinkSec = max(10, envInt("ZCODE_CAPTCHA_POOL_IDLE_SHRINK_SECONDS", 120))
	CaptchaPoolIdleStopSec   = max(CaptchaPoolIdleShrinkSec+1, envInt("ZCODE_CAPTCHA_POOL_IDLE_STOP_SECONDS", 900))
	// CPU governor：按 /proc/stat（Linux）或 runtime/metrics 进程 CPU 折算占用，
	// 高负载时降低 refill 并发与池目标；本地不引入新依赖。
	CaptchaCPUGovernorEnabled = envBool("ZCODE_CAPTCHA_CPU_GOVERNOR", true)

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

// ── 代理软失败隔离 ──────────────────────────────────────────────────────────
// 网络巡检只认传输层失败（连不上）；408/524「能连但跑不动」不构成删除证据，
// 也没有任何地方统计。这里单独按业务可用率计数：窗口内尝试数、失败数、失败率达到
// 阈值即把线路临时移出自动选线。隔离不删除配置、不改启停、到期自动回池；
// 删除仍然只由「两次新鲜网络复核 + 独立对照」决定。
//
// 同一约定：环境变量只是默认值，后台写入后以落库值为准（本组暂无后台项）。
var (
	// ProxySoftQuarantine 软失败隔离总开关；false 时只记录不隔离。
	ProxySoftQuarantine = envBool("ZCODE_PROXY_SOFT_QUARANTINE", true)
	// ProxySoftTruncate 上游中途断流是否计入线路软失败；false 时只走 line-guard 熔断。
	//
	// 断流对客户端可见且无法重试：连接失败可以换线重试，断流不能。默认计入，
	// 让它与连接失败共用「连续 2 次即隔离」，而不是等到第 3 次才不可逆删除。
	ProxySoftTruncate = envBool("ZCODE_PROXY_SOFT_TRUNCATE", true)
	// ProxySoftProbation 隔离到期是否进入观察期（默认开）。
	//
	// 关掉 = 旧行为：到期直接回选线池，并在静默 30 分钟后把档位清零。2026-10-18
	// 日志证明这条旧路径等于「免费复活」：18:28 到期回池，18:48 又断一次。
	ProxySoftProbation = envBool("ZCODE_PROXY_SOFT_PROBATION", true)
	// ProxySoftWindow 统计窗口。
	ProxySoftWindow = time.Duration(max(60, envInt("ZCODE_PROXY_SOFT_WINDOW_SECONDS", 1800))) * time.Second
	// ProxySoftMinAttempts 窗口内最少尝试数（分母）。
	ProxySoftMinAttempts = max(2, envInt("ZCODE_PROXY_SOFT_MIN_ATTEMPTS", 3))
	// ProxySoftMinFailures 窗口内最少软失败数。
	ProxySoftMinFailures = max(2, envInt("ZCODE_PROXY_SOFT_MIN_FAILURES", 3))
	// ProxySoftMinRatePercent 失败率下限（百分比）。
	ProxySoftMinRatePercent = min(100, max(1, envInt("ZCODE_PROXY_SOFT_MIN_RATE_PERCENT", 50)))
	// ProxySoftQuarantineMinutes 首档隔离时长，续期按 2 的幂递增并封顶 8 倍。
	ProxySoftQuarantineMinutes = max(1, envInt("ZCODE_PROXY_SOFT_QUARANTINE_MINUTES", 30))
	// ProxySoftConsecutive 连续软失败多少次即隔离；0 关闭连续判据，只保留失败率兜底。
	//
	// 为什么需要它：池子大、流量散时（实测单线 2.5 次/3 小时），「30 分钟内 ≥3 次失败」
	// 永远达不到——判据形状对、数量级错。连续失败与流量密度无关，一两次就能止损。
	ProxySoftConsecutive = max(0, envInt("ZCODE_PROXY_SOFT_CONSECUTIVE", 2))
	// ProxySoftConsecutiveWindow 连续计数有效期：超过则重新从 1 计。
	// 防止「早上失败一次、傍晚失败一次」这种跨越半天的孤立失败被当成连续。
	ProxySoftConsecutiveWindow = time.Duration(max(60, envInt("ZCODE_PROXY_SOFT_CONSECUTIVE_WINDOW_SECONDS", 7200))) * time.Second
	// ProxyFreshGateSeconds 新导入线路的冷启动门禁时长；超时未出结果即放行。
	ProxyFreshGateSeconds = max(0, envInt("ZCODE_PROXY_FRESH_GATE_SECONDS", 120))
	// ProxyProtectRecentHours 巡检自动删除前的「最近成功」保护窗口（小时）；0 关闭。
	// 只看账号 LastUsedAt（上游成功交付时写），且**只作用于巡检删除**：
	// 严格退役、断流熔断、登录清理与手动删除都不受它影响。
	//
	// 默认 6 小时（v2.10.27 引入时为 24）。依据 2026-10-25 的 7.5 小时日志：94 次免删
	// 覆盖 32 条线路，其中 69% 的依据是 ≥3 小时前的成功——多半已是死线；真正在保护期
	// 内仍交付成功的 9 条线路，首次被保护时成功都在 4.8 小时内。24 小时还会让 4 条持续
	// 死亡线路反复触发「计费请求未完成」观察（占全部观察 64%）。环境变量可覆盖。
	ProxyProtectRecentHours = max(0, envInt("ZCODE_PROXY_PROTECT_RECENT_HOURS", 6))
)

// ── 出站连接超时 ────────────────────────────────────────────────────────────
// 死线路的代价此前是「连代理 30s + 握手 30s」，一次尝试 60–80 秒；三次尝试串起来
// 就是撞 240 秒请求上限（2026-10-18 复盘：8.7MB 请求 3 次 connection_failed 各约 70s）。
// 拆成两个值：连代理通常 <1s，真正需要余量的是「代理由此连到目标」。
var (
	// ProxyDialTimeout 连接到代理服务器本身；≤0 时不生效（回落默认）。
	ProxyDialTimeout = time.Duration(max(1, envInt("ZCODE_PROXY_DIAL_TIMEOUT_SECONDS", 12))) * time.Second
	// ProxyHandshakeTimeout SOCKS 握手 / CONNECT 到目标；跨境线路 5–10s 常见。
	ProxyHandshakeTimeout = time.Duration(max(1, envInt("ZCODE_PROXY_HANDSHAKE_TIMEOUT_SECONDS", 20))) * time.Second
)

// ── 隧道吞吐探针 ────────────────────────────────────────────────────────────
// 浅探针只证明「能连」：探针对目标根路径发一次 6 秒 GET，能连但大包传不动的线路
// 每次判健康（2026-10-17：5.5MB 请求首字节 283 秒，探针全程绿灯）。
// 这里走裸隧道直接写字节测上传能力；默认 shadow 只记录，不参与任何判定。
var (
	// DeepProbe off | shadow | enforce；非法值按 shadow 处理（不改判定，最安全）。
	DeepProbe = env("ZCODE_PROXY_DEEP_PROBE", "shadow")
	// DeepTarget 只接受 host:port，不接受 URL——避免把凭据写进配置。
	DeepTarget = env("ZCODE_PROXY_DEEP_TARGET", "zcode.z.ai:443")
	// DeepBytes 单次写入量。
	DeepBytes = int64(max(65536, envInt("ZCODE_PROXY_DEEP_BYTES", 1048576)))
	// DeepBudget 单次预算。
	DeepBudget = time.Duration(max(1000, envInt("ZCODE_PROXY_DEEP_BUDGET_MS", 12000))) * time.Millisecond
	// DeepMinBytes 早停判定下限：写满这个量且够快，就认为链路没问题。
	//
	// 256KB → 128KB 的依据不是「曲线平缓」，而是 96KB 与 128KB 对真实成功线路的
	// 覆盖率完全相同（77/81）——128KB 是达到满覆盖的最小值，再往下没有收益。
	// 早停是「对端主动关闭」（写错误），与「缓冲区吃满后阻塞」不同：后者会撞满预算，
	// 被 `ms < budget*2/3` 挡掉，所以放宽下限不会把传不动的线放进来。
	//
	// ⚠️ 不要单独下调 DeepBytes 来省流量：满写分支不受 kbps/ms 约束，1MiB/12s 就是
	// 85KB/s 的隐式速率底线（正好等于 DeepMinKbps）。降 size 会让「突发后卡死」的
	// 线在卡死前停止写入、被判 OK——2026-10-20 日志里有 78 条这样的样本。
	DeepMinBytes = int64(max(1024, envInt("ZCODE_PROXY_DEEP_MIN_BYTES", 131072)))
	// DeepMinKbps 速率下限（KB/s）：实测「慢但能用」约 68，故障线路 20 量级。
	DeepMinKbps = int64(max(1, envInt("ZCODE_PROXY_DEEP_MIN_KBPS", 85)))
)

// DeepProbeMode 归一化开关：只有明确 off/enforce 才改变行为，其余一律 shadow。
func DeepProbeMode() string {
	switch strings.ToLower(strings.TrimSpace(DeepProbe)) {
	case "off":
		return "off"
	case "enforce":
		return "enforce"
	default:
		return "shadow"
	}
}

// ── 单次尝试的首字节预算 ────────────────────────────────────────────────────
// Go 的 ResponseHeaderTimeout 是「**写完 body 之后**」才开始计时，慢线路上 5MB
// 上传要几百秒，上传阶段等于没有超时（2026-10-17 复盘：5.5MB 请求首字节 283s）。
// 这里给「建连 + 上传 + 收到响应头」整段设预算：收到响应头即停表，绝不限制长流。
var (
	// UpstreamFirstByteTimeout 首字节预算；0 关闭。
	UpstreamFirstByteTimeout = time.Duration(max(0, envInt("ZCODE_UPSTREAM_FIRST_BYTE_TIMEOUT", 90))) * time.Second
	// BusySwitchAfter 3009 并发准入的换号实验开关：原地重试 N 次后改用另一个账号。
	//
	// 0（默认）= 保持既有假设「模型级准入、换号无用」：重试预算用尽后原样透传 429。
	// >0 = 实验组：N 次原地重试后换号（不冷却账号，靠外层 tried 集合避免重复选中）。
	// 2026-10-20 日志里 5 条 3009 分布在 5 个不同账号/线路，且同样内容之后都成功；
	// 但另有 2 条是同账号延迟后成功——证据不足以推翻假设，只能靠对照数据判断。
	BusySwitchAfter = max(0, envInt("ZCODE_BUSY_SWITCH_AFTER", 0))
	// UpstreamTimeoutRetries 上游 408/524 的换号重试次数；0 关闭。
	UpstreamTimeoutRetries = max(0, envInt("ZCODE_UPSTREAM_TIMEOUT_RETRIES", 1))
	// RequestDeadline 单次请求的墙钟总预算；0 关闭。
	//
	// 首字节预算只约束「一次尝试」，换号重试仍会各拿一份预算：MaxAccountAttempts=5
	// 时最坏是 5×90s=450s（2026-10-17：5.5MB 请求在 5 条慢线上仍可能转圈 7 分钟）。
	// 本预算从进入 RunMessages 起算，只在「准备发起下一次尝试」前判定——**不派生
	// ctx**，已经开始的流式交付不受影响（掐断长回答比慢更糟）。
	//
	// 240s 的依据：实测正常大包首字节 30–65s，两次尝试（首次 + 一次换线）≈130s，
	// 留一倍余量；同时把最坏 450s 压到 240s。
	RequestDeadline = time.Duration(max(0, envInt("ZCODE_REQUEST_DEADLINE_SECONDS", 240))) * time.Second
)

// ── 上游风控冷却 ────────────────────────────────────────────────────────────
// 405 + unusual activity 按账号施加本地阶梯冷却。该处置范围是网关策略，
// 不能据通用错误文案断言供应商对所有模型的封禁范围。
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

	// ZcodeEndpointOrigin 伪装身份的端点 origin。官方 HTTP-Referer 即此值，
	// 且**不带尾斜杠**（`p1()` 返回 origin）——多一个 `/` 就与官方客户端不同。
	// 所有面向 zcode.z.ai 的请求头都应引用本常量，不要各写一份字面量。
	ZcodeEndpointOrigin = env("ZCODE_ENDPOINT_ORIGIN", "https://zcode.z.ai")

	// UpstreamSendDeviceMid 是否在**推理请求**上发送 X-Device-Mid。
	//
	// 官方客户端的模型请求**不带**这个头（只有额度 / 领取 / 遥测等非模型接口带）——
	// golden 抓包实测确认（见 docs/analysis-client-golden-diff-20260924.md）。官方把
	// 设备指纹放在 **body 的 metadata.user_id.device_id** 里，本网关同样如此
	//（upstream.InjectDeviceMetadata，每账号一份）⇒ 账号隔离不依赖这个头，默认 false。
	//
	// 保留开关只为应急回滚（设 true 可恢复历史行为），不要长期打开：多发一个官方
	// 不发的头，与少发一样是可交叉比对的差异。见 docs/plan-client-format-parity.md GAP-7。
	UpstreamSendDeviceMid = envBool("ZCODE_UPSTREAM_SEND_DEVICE_MID", false)

	// UpstreamInjectEnvironment 是否让 ZcodeSystemBlocks 的静态返回追加 # Environment。
	// 该入口只用于测试、诊断与旧调用方；推理回退路径由 upstream.BuildFallbackSystem
	// 固定生成三块 dynamic 段，不受本开关影响。
	UpstreamInjectEnvironment = envBool("ZCODE_UPSTREAM_INJECT_ENVIRONMENT", false)

	// 调用者自带官方前缀（或显式空值 / 消息内 system）时原样保留其指令上下文，
	// 非官方前缀回退前置合并；关闭时恢复旧版「一律前置」的注入/指令提升行为。
	PreserveClientContext = envBool("ZCODE_PRESERVE_CLIENT_CONTEXT", true)

	// ReplayGuardTTLSeconds 重放防护窗口（秒，P1-3）：同一份请求内容被判定「请求级
	// 风控」后，该窗口内同内容+同模型的请求直接快速失败，不再消耗任何账号 —— 阻止
	// 上游账号标记随换号重试扩散（2026-09-24 整池雪崩的直接机制）。0 = 禁用（应急回退）。
	ReplayGuardTTLSeconds = envInt("ZCODE_REPLAY_GUARD_TTL_SECONDS", 60)

	// ReplayAllowOnce 判定请求级风控后，是否允许同内容再试一次（默认开）。
	//
	// 2026-10-18 日志：三条「各自首次命中」的脏线连击触发判定后，客户端 57 秒内重试
	// 11 次全被挡；而 207 秒后同内容换线即成功——内容没问题，是脏线运气。放行一次
	// 让这种情况自愈；每 60 秒窗口只放行一次，代价有界（最多多试 3 条线）。
	ReplayAllowOnce = envBool("ZCODE_RISK_REPLAY_ALLOW_ONCE", true)

	// RiskSilentHours 可选的额外本地隔离窗口（小时），默认 0，仅按阶梯冷却。
	// 405 可能与请求、账号或代理出口有关，不据本地历史推断供应商封禁期限。
	// 显式大于 0 时仍可额外隔离，并在人工恢复时施加保护冷却；升级不覆盖手动配置。
	// 选号与恢复共用本值，RiskSilentWindow 是唯一换算点；不自动复活失效账号。
	RiskSilentHours = envInt("ZCODE_RISK_SILENT_HOURS", 0)

	// Line405MinStreak 连续多少次 405 判定一条线路不可派发（0 = 关闭线路隔离）。
	//
	// 用「连续」而不是累计：免费代理池里时好时坏的线很多，累计计数会把还能用的线
	// 永久压死；连续两次 405 之间只要成功一次就清零。
	Line405MinStreak = max(0, envInt("ZCODE_LINE405_MIN_STREAK", 2))

	// Line405QuarantineMinutes 线路 405 隔离首档时长（分钟），续期按 2 的幂递增。
	Line405QuarantineMinutes = max(1, envInt("ZCODE_LINE405_QUARANTINE_MINUTES", 30))

	// Line405MaxQuarantineMinutes 隔离续期封顶（分钟）。与软失败一致，最长 4 小时；
	// 超过这个量级仍 405 的线路应当被巡检删除，而不是无限期隔离。
	Line405MaxQuarantineMinutes = max(1, envInt("ZCODE_LINE405_MAX_QUARANTINE_MINUTES", 240))

	// Line405ProbeCooldownMinutes 同一条线路触发定向模型巡检的最小间隔（分钟）。
	// 隔离续期不会重复入队：脏线往往在几次 405 之内连续升档，重复巡检只是烧额度。
	Line405ProbeCooldownMinutes = max(0, envInt("ZCODE_LINE405_PROBE_COOLDOWN_MINUTES", 30))

	// Line405ProbePerHour 定向模型巡检的全局每小时上限（0 = 只隔离、不巡检）。
	//
	// 巡检要消耗账号额度，而免费代理池里「被隔离的线」可能一下几十条；不设预算等于
	// 拿账号池替脏线买单。超限只记日志，隔离本身不受影响。
	Line405ProbePerHour = max(0, envInt("ZCODE_LINE405_PROBE_PER_HOUR", 12))

	// Line405ProbeRetire 巡检判定失败且线路已被反复隔离时，是否走退休链删除。
	//
	// 为什么不用既有 route_correlated：它要求「两轮 target 405 + 参考线成功」，实测
	// 22 条结论全是 suspect、0 次删除——门槛在真实节奏下不可达。改为两条独立证据：
	// 真实流量 405 两次（Strikes≥2，即隔离放出去又犯）+ 巡检 405。
	Line405ProbeRetire = envBool("ZCODE_LINE405_PROBE_RETIRE", true)
	// Line405RetireMinStrikes 触发退休所需的隔离次数（=「放出去又犯」次数）。
	Line405RetireMinStrikes = max(1, envInt("ZCODE_LINE405_RETIRE_MIN_STRIKES", 2))

	// ProbeRiskPenalty 模型巡检 405 时对执行账号的惩罚范围。
	//
	// 巡检的 screen/target 阶段只证明「目标线路被拦」，账号责任要等 reference 对照才成立；
	// 在归因之前就冷却账号，会让健康账号替脏线买单，还会让 reference 对照等冷却白白拖慢。
	//   reference（默认）：只有 reference 阶段失败才写账号冷却/错误；
	//   all：旧行为，screen/target/reference 全部惩罚（回滚用）；
	//   off：巡检完全不写账号状态，只保留 run/proofs 与线路证据。
	// 非法值按 reference 处理：宁可少罚，不可误罚。
	ProbeRiskPenalty = env("ZCODE_MODEL_PROBE_RISK_PENALTY", "reference")

	// ProxyMaxBindingsPerLine 单条线路的绑定软上限（0 = 关闭判定）。
	//
	// ⚠️ 它不是「分散器」：leastLoadedProxy 永远选绑定最少的线路，只要存在未超限的
	// 候选就一定先被选中，因此本值不改变选线结果。它的真实用途是容量口径——当选中的
	// 最空闲线路都已超限时，说明「线路数不足」，日志会记 capacity_short，提示扩池；
	// 此时仍然分配（不产生 auto_pending_proxy），只是让容量缺口可见。
	ProxyMaxBindingsPerLine = max(0, envInt("ZCODE_PROXY_MAX_BINDINGS_PER_LINE", 8))

	// ── 上行速率闸门 ────────────────────────────────────────────────────────
	// 首字节看门狗只给整段预算，上传阶段没有独立判据：8.3 MiB 在 19 KB/s 的线上要
	// 450 秒，必然超预算，但代码要等满 90 秒才知道。闸门按实测速率外推剩余上传时间，
	// 明显超出本次尝试预算就立即中断，把「等 90 秒」变成「2 秒换线」。
	UplinkGate = envBool("ZCODE_UPLINK_GATE", true)
	// UplinkGateMinBytes 低于该体量完全不判定：小请求的慢启动不能代表线路带宽。
	UplinkGateMinBytes = max(0, envInt("ZCODE_UPLINK_GATE_MIN_BYTES", 262144))
	// UplinkGateMinMS 最短采样时长：TLS 握手与首包可能吃掉一两秒，先让速率稳定。
	UplinkGateMinMS = max(0, envInt("ZCODE_UPLINK_GATE_MIN_MS", 1000))
	// UplinkGateMarginSeconds 判定余量：ETA 要超过「预算 - 余量」才放弃，避免边界抖动。
	UplinkGateMarginSeconds = max(0, envInt("ZCODE_UPLINK_GATE_MARGIN_SECONDS", 3))

	// CoolOnConnectionFailure 传输层连接失败是否按旧行为冷却账号 300 秒。
	//
	// 默认 false：dial / TLS / 写入失败按定义是线路问题，账号本身没问题。旧行为在
	// 2.10.24 把拨号超时从 60–80s 压到 ≤32s 之后更容易触发——死线会更快把冷却记到
	// 账号头上，反而缩小可用池。1 = 回退旧行为。
	CoolOnConnectionFailure = envBool("ZCODE_COOL_ON_CONNECTION_FAILURE", false)

	// RiskRequestLevelLines 判定「请求级风控」所需的不同干净线路数。
	//
	// 旧版数的是账号：2 个账号各 405 一次就判内容级。2026-10-17 日志证明这在脏线
	// 池里几乎全是误判（9 次判定 8 次错，同内容 41–149 秒后换线即成功），故改为按
	// 线路去重、默认 3；2 = 恢复旧行为（应急回退）。
	RiskRequestLevelLines = max(1, envInt("ZCODE_RISK_REQUEST_LEVEL_LINES", 3))

	// RiskCoolOnFirst 首次 405 是否直接按阶梯冷却账号。
	// 0（默认）= 只换线不冷却：本次日志里 405 过的账号换条线即可成功，冷却账号
	// 等于拿池子容量替脏线买单。1 = 旧行为（首次即冷却），用于回退。
	RiskCoolOnFirst = envInt("ZCODE_RISK_COOL_ON_FIRST", 0)

	// SelectExpireFirstDays 选号「到期门槛」窗口（天）：持有 N 天内到期、且不会自动
	// 刷新的额度列（one_time / 周月套餐）的账号优先。0 = 关闭。
	//
	// 解决的是「快过期额度还没用就作废」：原第 4 层只按剩余多少排序，与到期方向相反。
	// 门槛只收窄候选，不改变并列组内的轮询——同规格账号整体并列时行为与旧版一致。
	// 日额度（跨度 ≈24h）不算，避免把「每天刷新」误当损耗。
	SelectExpireFirstDays = max(0, envInt("ZCODE_SELECT_EXPIRE_FIRST_DAYS", 3))

	// 官方客户端版本号：随请求头 / URL 参数上行，用于伪装成官方客户端。
	// 2026-09-17 由 3.7.7 升至 3.11.2，与 zcode-switch 的 CLIENT_APP_VERSION 对齐；
	// 2026-09-23 由 3.11.2 升至 3.14.3（本机实装客户端版本，激活失效排查项之一，
	// 见 docs/analysis-zcode-client-activation-diff.md）。
	// 2026-09-30 同步 3.14.4；模型验证码按 skip_model_request 策略独立跳过。
	// 2026-10-09 同步本机确认的 3.14.5；此次客户端修复不改变模型/重置协议。
	// 2026-10-14 同步 3.15.3；普通路由与核心提示词保持，未支持的账号上下文显式拒绝。
	// 2026-10-20 同步本机确认的 3.16.2；逆向核验模型请求形态与提示词未变，仅版本值更新。
	// 2026-10-24 同步本机确认的 3.16.3；来源头/归因头/metadata.user_id/端点/提示词均未变，仅版本值更新。
	ZcodeClientVersion = env("ZCODE_CLIENT_VERSION", "3.16.3")
	// ZcodeClientPlatform 复合平台串（`${裸平台}-${架构}`），供请求头 X-Platform 与
	// 后台展示使用。默认值由 profile.go 的裸平台 / 架构派生，避免两处各写一份而漂移；
	// ZCODE_CLIENT_PLATFORM 仍可单独覆盖（旧部署兼容）。
	//
	// ⚠️ 查询参数 `platform=` 用的是**另一种格式**（win32→windows、x64→x86_64，
	// 即 windows-x86_64），必须走 ZcodeProfile.QueryPlatform()，不要复用本变量。
	ZcodeClientPlatform = env("ZCODE_CLIENT_PLATFORM", ZcodeClientBarePlatform+"-"+ZcodeClientArch)

	// ZcodeQueryPlatformOverride 查询参数 `platform=` 的应急覆盖位。
	//
	// 留空（默认）走 profile.go 的 ZcodeQueryPlatform() 按官方形态派生；一旦上游
	// 对格式变脸，把它设成旧形态即可**免重编**回退，例如：
	//
	//	ZCODE_QUERY_PLATFORM=win32-x64
	//
	// 只为应急保留，正常部署不要设置。它只影响查询参数，不碰请求头 X-Platform——
	// 两者本来就是两种格式，一个开关同时改两处只会制造新的头体矛盾。
	ZcodeQueryPlatformOverride = env("ZCODE_QUERY_PLATFORM", "")

	UserAgent = env("UPSTREAM_USER_AGENT", "ZCode/"+ZcodeClientVersion)

	// AppVersion 供 /meta 与后台展示；-go 后缀标识运行时版本。
	AppVersion = "2.11.2-go"
)

// RiskSilentWindow 风控静默窗口的 Duration 形态。
// 单一换算点：选号闸门（store.Select）、池状态统计、恢复护栏与请求级判定共用，
// 避免各调用点在自乘 time.Hour 上漂移。读取的是 RiskSilentHours 的当前值，
// 测试可以覆写该变量生效。
func RiskSilentWindow() time.Duration { return time.Duration(RiskSilentHours) * time.Hour }

// ── 遥测设备伪装 ────────────────────────────────────────────────────────────
// 激活事件体里的设备字段。刻意**不跟随运行环境**：网关多跑在 Linux 容器里，
// 而 X-Platform / User-Agent 都声称桌面客户端，跟随环境会让 body 与请求头
// 自相矛盾（上游可交叉比对，据此判定「非官方客户端」而不投放活动套餐）。
//
// ⚠️ 这四项与 profile.go 的裸平台 / 架构同属**一个身份**，必须成组一致：
// 官方客户端把它们全部派生自 (platform, arch, os.release())。生产代码不要直接
// 引用这些变量做拼接，改用 ZcodeClientProfile()——否则又会散出第二份真相。
var (
	// ZcodeClientOSVersion 内核发行串（官方 os.release()）。它同时是请求头
	// X-Os-Version 的取值，以及 # Environment 段 `OS Version` 的中间那段。
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

// NewUUIDv4 生成 UUIDv4，供需要「每请求一个随机 id」的调用方使用
// （如上游请求头 x-request-id / x-zcode-trace-id —— 官方客户端用 crypto.randomUUID()）。
//
// 放在本包是为了共用同一份实现：newUUID 在 config / claim / asyncpool 已各有一份，
// 上游再造第四份没有意义。
func NewUUIDv4() string { return newUUID() }

// newUUID 生成 UUIDv4（不引入第三方依赖）。
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
