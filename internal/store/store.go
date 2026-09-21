// Package store 账号与设置的持久化存储（SQLite）。
// 对应 Python 版 app/store.py；schema 与 Python 版完全一致，
// 两个版本可互读同一个 data/accounts.db（Account JSON 字段契约见 internal/model）。
//
// 运行期账号对象常驻内存（保证轮询游标与状态实时性），
// 每次变更同步落库；进程启动时从 SQLite 读取快照。
package store

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/proxy"
	"zcode2api/internal/web"
)

const (
	legacyAdminKey = "zcode" // 2.0.1 之前发布的固定默认后台密码，升级时强制轮换

	metaTable     = "meta"
	accountsTable = "accounts"
)

// Providers 支持的提供商（与 Python 版 PROVIDERS 一致）。
var Providers = []string{model.ProviderZai}

// ErrNotFound 代理配置等条目不存在。
var ErrNotFound = errors.New("条目不存在")

// ErrProxyNotFound 指派账号时引用的代理线路不存在（对齐 Python 版 ValueError）。
var ErrProxyNotFound = errors.New("代理配置不存在")

// Store 线程安全的账号 / 设置存储，含轮询游标。
type Store struct {
	mu       sync.Mutex
	db       *sql.DB
	accounts map[string][]*model.Account
	settings map[string]string
	rotation map[string]int

	// settingsSnapshot 是 settings 的不可变快照，供无锁读取。
	//
	// 读设置的路径包含每次 API 请求的鉴权（VerifyGatewayKey → GetSetting）。若与
	// 写路径共用 s.mu，一次慢写（磁盘满、外部进程持写锁时最多 busy_timeout 5s）
	// 会让所有请求的鉴权一起排队——DB 慢即服务不可用。快照让读取完全不碰锁；
	// 写入仍是「改 map 后发布新快照」。
	settingsSnapshot atomic.Pointer[map[string]string]

	// GeneratedAdminKey / GeneratedGatewayKey：本次启动随机生成/轮换的密钥，
	// 供启动横幅提示管理者（环境变量配置时不记录）。
	GeneratedAdminKey   string
	GeneratedGatewayKey string
}

// New 打开（必要时创建）数据库并加载快照。
func New() (*Store, error) {
	db, err := openDB(config.DBPath)
	if err != nil {
		return nil, err
	}
	s := &Store{
		db:       db,
		accounts: map[string][]*model.Account{model.ProviderZai: {}},
		settings: map[string]string{},
		rotation: map[string]int{},
	}
	if err := s.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close 关闭数据库连接。
func (s *Store) Close() error { return s.db.Close() }

func openDB(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	// _pragma 参数对每个新连接生效；配合 MaxOpenConns(1) 即单连接 + WAL 语义，
	// 对齐 Python 版「进程内复用单个连接」的形态。
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func (s *Store) init() error {
	if _, err := s.db.Exec(fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS %s (
			id          TEXT PRIMARY KEY,
			provider    TEXT NOT NULL,
			name        TEXT,
			mode        TEXT,
			status      TEXT,
			enabled     INTEGER NOT NULL DEFAULT 1,
			created_at  REAL,
			data        TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_acc_provider ON %s (provider);
		CREATE INDEX IF NOT EXISTS idx_acc_status   ON %s (status);
	`, metaTable, accountsTable, accountsTable, accountsTable)); err != nil {
		return err
	}
	if err := s.bootstrapAuthKeys(); err != nil {
		return err
	}
	return s.load()
}

// bootstrapAuthKeys 初始化后台密码与网关 API Key，保证两者永不为空、永不為固定預設值。
// - 环境变量显式配置时，用于替换缺失值或历史默认值（写入后仍以数据库为准）；
// - 否则随机生成，并记录到 Generated* 字段供启动横幅展示；
// - 历史版本写死的管理密码「zcode」在升级时强制轮换。
func (s *Store) bootstrapAuthKeys() error {
	existing := map[string]string{}
	rows, err := s.db.Query(
		fmt.Sprintf("SELECT key, value FROM %s WHERE key IN ('admin_key', 'gateway_key')", metaTable))
	if err != nil {
		return err
	}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			rows.Close()
			return err
		}
		existing[k] = v
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	adminKey := existing["admin_key"]
	if adminKey == "" || adminKey == legacyAdminKey {
		if config.AdminKeyEnv != "" {
			adminKey = config.AdminKeyEnv
		} else {
			adminKey = randomTokenURLSafe(24)
			s.GeneratedAdminKey = adminKey
		}
		if err := s.setMeta("admin_key", adminKey); err != nil {
			return err
		}
	}

	gatewayKey := existing["gateway_key"]
	if gatewayKey == "" {
		if config.GatewayKeyEnv != "" {
			gatewayKey = config.GatewayKeyEnv
		} else {
			gatewayKey = "sk-" + randomTokenURLSafe(24)
			s.GeneratedGatewayKey = gatewayKey
		}
		if err := s.setMeta("gateway_key", gatewayKey); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) load() error {
	metaRows := map[string]string{}
	rows, err := s.db.Query(fmt.Sprintf("SELECT key, value FROM %s", metaTable))
	if err != nil {
		return err
	}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			rows.Close()
			return err
		}
		metaRows[k] = v
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	settings := metaRows
	// 密钥由 bootstrapAuthKeys 保证存在；此处缺省空值即拒绝鉴权（fail closed）
	if _, ok := settings["admin_key"]; !ok {
		settings["admin_key"] = ""
	}
	if _, ok := settings["gateway_key"]; !ok {
		settings["gateway_key"] = ""
	}
	if _, ok := settings["quota_refresh_interval"]; !ok {
		settings["quota_refresh_interval"] = strconv.Itoa(config.QuotaRefreshInterval)
	}
	s.settings = settings
	s.publishSettings()

	accounts := map[string][]*model.Account{model.ProviderZai: {}}
	rows, err = s.db.Query(fmt.Sprintf(
		"SELECT data FROM %s ORDER BY created_at ASC", accountsTable))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return err
		}
		acc, err := model.FromJSON([]byte(data))
		if err != nil {
			continue // 坏行跳过（对齐 Python 版）
		}
		if _, ok := accounts[acc.Provider]; ok {
			accounts[acc.Provider] = append(accounts[acc.Provider], acc)
		}
	}
	s.accounts = accounts
	if err := rows.Err(); err != nil {
		return err
	}
	return s.migrateAccountIdentity()
}

// migrateAccountIdentity 一次性回填 Go 增量身份字段（幂等，仅有改动时落库）：
//   - virtual_device_mid：此前全局共用一份 device_mid，同一台机器上的多个账号
//     会被上游按设备关联，这里给每个账号分配独立指纹；
//   - user_id：旧数据只存凭据，token 一刷新同一个号就变成"另一条记录"。
//
// 刻意不删除、不合并任何存量账号——查重只作用于新增（见 AddAccountWithIdentity）。
func (s *Store) migrateAccountIdentity() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, list := range s.accounts {
		for _, acc := range list {
			changed := false
			if acc.VirtualDeviceMid == nil || strings.TrimSpace(*acc.VirtualDeviceMid) == "" {
				mid := config.NewDeviceMid()
				acc.VirtualDeviceMid = &mid
				changed = true
			}
			if (acc.UserID == nil || *acc.UserID == "") && acc.Mode == "jwt" && acc.JWTToken != nil {
				if uid := model.JWTUserID(*acc.JWTToken); uid != "" {
					acc.UserID = &uid
					changed = true
				}
			}
			if !changed {
				continue
			}
			if err := s.persistAccountLocked(acc); err != nil {
				return err
			}
		}
	}
	return nil
}

// ── 持久化（调用方须持有 s.mu）──────────────────────────────────────────────

func (s *Store) persistAccountLocked(acc *model.Account) error {
	data, err := marshalJSON(acc)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		fmt.Sprintf(`INSERT OR REPLACE INTO %s
			(id, provider, name, mode, status, enabled, created_at, data)
			VALUES (?,?,?,?,?,?,?,?)`, accountsTable),
		acc.ID, acc.Provider, acc.Name, acc.Mode, acc.Status, boolToInt(acc.Enabled), acc.CreatedAt, string(data))
	return err
}

func (s *Store) deleteAccountLocked(id string) error {
	_, err := s.db.Exec(fmt.Sprintf("DELETE FROM %s WHERE id = ?", accountsTable), id)
	return err
}

func (s *Store) setMeta(key, value string) error {
	_, err := s.db.Exec(
		fmt.Sprintf("INSERT OR REPLACE INTO %s (key, value) VALUES (?, ?)", metaTable), key, value)
	return err
}

// marshalJSON 与 Python json.dumps(ensure_ascii=False) 对齐：不转义 HTML 字符。
func marshalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// ── 设置 ────────────────────────────────────────────────────────────────────

// GetSetting 读取设置（第二返回值表示是否存在）。
//
// 走原子快照而非 s.mu：鉴权路径（VerifyGatewayKey / VerifyAdminKey）每次都调用本
// 函数，若与写路径共用锁，一次慢写就会让所有请求的鉴权排队——DB 慢即服务不可用。
func (s *Store) GetSetting(key string) (string, bool) {
	snap := s.settingsSnapshot.Load()
	if snap == nil {
		return "", false
	}
	v, ok := (*snap)[key]
	return v, ok
}

// publishSettings 发布 settings 的不可变快照（调用方须持有 s.mu）。
//
// 复制一份而非共享原 map：快照必须不可变，否则读到一半被并发写会触发 Go 的并发
// map 读写检测。
func (s *Store) publishSettings() {
	cp := make(map[string]string, len(s.settings))
	for k, v := range s.settings {
		cp[k] = v
	}
	s.settingsSnapshot.Store(&cp)
}

// logPersistFailure 记录一次落库失败。
//
// 统计路径（网关计 token、异步池计状态、额度刷新）刻意忽略 Update 的错误——不该因为
// 统计写不进去就让用户的对话请求失败。但完全静默会让「磁盘满导致状态全部不落库」没有
// 任何线索可查：后台数字与实际持久化状态脱节、重启后回滚，而日志里什么都没有。这里
// 集中记一次，涵盖所有调用方。
//
// 节流到每分钟一条：持续失败时每个请求都会走到这里，不节流会把日志刷爆并掩盖其他信息。
//
// 调用方都持有 s.mu，故本函数必须自行确保「不在锁内做 I/O」——web.Warn 是同步的
// stdout 写，stdout 阻塞（管道满、终端卡住）时会把整个 Store 锁住。做法是只在锁内
// 做判断，把实际输出交给独立 goroutine。
func logPersistFailure(scope, detail string, err error) {
	if err == nil {
		return
	}
	persistLogMu.Lock()
	now := time.Now()
	allow := now.Sub(persistLogLast) >= persistLogInterval
	if allow {
		persistLogLast = now
	}
	persistLogMu.Unlock()
	if !allow {
		return
	}
	msg := fmt.Sprintf("落库失败（%s，%s）: %v；该改动可能只存在于内存，重启后会回滚", scope, detail, err)
	go web.Warn("store", msg)
}

var (
	persistLogMu       sync.Mutex
	persistLogLast     time.Time
	persistLogInterval = time.Minute
)

// SetSetting 更新设置并落库。
func (s *Store) SetSetting(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 先落库再改内存，与 AddAccount / RemoveAccount 同一原则：落库失败时内存不能留下
	// 一个未持久化的值，否则本次进程按新值运行、重启后回滚，而调用方收到错误以为没生效。
	if err := s.setMeta(key, value); err != nil {
		logPersistFailure("setting", key, err)
		return err
	}
	s.settings[key] = value
	s.publishSettings()
	return nil
}

func (s *Store) AdminKey() string {
	v, _ := s.GetSetting("admin_key")
	return v
}

func (s *Store) GatewayKey() string {
	v, _ := s.GetSetting("gateway_key")
	return v
}

// QuotaRefreshInterval 额度刷新间隔（秒，非负；非法值回退默认）。
func (s *Store) QuotaRefreshInterval() int {
	v, ok := s.GetSetting("quota_refresh_interval")
	if !ok {
		return config.QuotaRefreshInterval
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return config.QuotaRefreshInterval
	}
	return max(0, n)
}

// ── 套餐領取設定 ────────────────────────────────────────────────────────────
//
// 环境变量（ZCODE_CLAIM_*）只是**默认值**：这些键缺失或非法时回退 config，
// 落库后以后台设置页为准，改完即生效（领取冷却与调度器每轮都重新读取）。

// claimScheduleTimeRe 合法的每日定时点（本地时区 HH:MM）。
var claimScheduleTimeRe = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)

// claimScheduleTimeDefault 定时点非法时的回退值。
const claimScheduleTimeDefault = "23:00"

// ClaimAutoEnabled 入池自动领取开关（批量添加 / OAuth 登录后是否自动领取一次）。
// 刻意只约束自动路径：手动点按钮始终可用。
func (s *Store) ClaimAutoEnabled() bool { return s.claimBool("claim_auto_enabled", true) }

// ClaimScheduleEnabled 每日定时领取开关。默认关闭：升级/新装不该在用户无感知时
// 自发产生每日上游流量，由管理员在设置页打开。
func (s *Store) ClaimScheduleEnabled() bool { return s.claimBool("claim_schedule_enabled", false) }

// ClaimScheduleTime 每日定时领取的本地时间点（HH:MM）；非法回退 23:00。
func (s *Store) ClaimScheduleTime() string {
	v, ok := s.GetSetting("claim_schedule_time")
	if !ok {
		return claimScheduleTimeDefault
	}
	if t := strings.TrimSpace(v); ValidClaimScheduleTime(t) {
		return t
	}
	return claimScheduleTimeDefault
}

// ValidClaimScheduleTime 校验每日定时点格式（HH:MM，本地时区）。
func ValidClaimScheduleTime(v string) bool {
	return claimScheduleTimeRe.MatchString(strings.TrimSpace(v))
}

// ClaimCooldowns 领取冷却三档（秒）：验证码类 / 其他失败 / 「刷新资格」节流。
func (s *Store) ClaimCooldowns() (captchaSec, retrySec, previewSec int) {
	captchaSec = s.claimInt("claim_captcha_cooldown", config.ClaimCaptchaCooldownSeconds, 60)
	retrySec = s.claimInt("claim_retry_cooldown", config.ClaimRetryCooldownSeconds, 30)
	previewSec = s.claimInt("claim_preview_cooldown", config.ClaimPreviewCooldownSeconds, 0)
	return captchaSec, retrySec, previewSec
}

// claimBool 读取布尔设置：真值集合与 envBool 一致，缺失回退 def。
func (s *Store) claimBool(key string, def bool) bool {
	v, ok := s.GetSetting(key)
	if !ok {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

// claimInt 读取整数设置并钳到下限；缺失或非法回退 def。
func (s *Store) claimInt(key string, def, min int) int {
	v, ok := s.GetSetting(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return max(min, n)
}

// ── 上游风控冷却阶梯 ────────────────────────────────────────────────────────
//
// 键名导出：它同时出现在 store 的访问器与 adminapi 的 GET/PUT 三处，写成字面量
// 迟早会漂移（改一处忘一处会静默失效——GET 少字段前端走兜底、PUT 未知键被忽略）。

// RiskCoolingStepsKey 风控冷却阶梯的设定键（逗号分隔的秒数）。
const RiskCoolingStepsKey = "risk_cooling_steps"

// riskCoolingStepMax 单档上限（7 天）。防写入异常大的值把账号锁死到「看不出问题」。
const riskCoolingStepMax = 7 * 24 * 3600

// RiskCoolingStepsString 返回设定原始串；缺失或为空回退环境变量默认值。
func (s *Store) RiskCoolingStepsString() string {
	if v, ok := s.GetSetting(RiskCoolingStepsKey); ok {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return config.RiskCoolingSteps
}

// NormalizeRiskCoolingSteps 校验并规范化阶梯：去空白、统一逗号、钳超大值。
// 非法（空串 / 含 0 / 负数 / 非数字）返回 ok=false，由调用方决定是否 400。
//
// 落库前先规范化，GET 才能总是回读到同一个规范形态：否则管理员填 " 60, 120 "
// 会原样存进去，回读时前端显示一堆空格，看起来像没保存对。
func NormalizeRiskCoolingSteps(raw string) (string, bool) {
	steps, err := parseRiskCoolingSteps(raw)
	if err != nil {
		return "", false
	}
	parts := make([]string, len(steps))
	for i, n := range steps {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ","), true
}

// RiskCoolingSteps 解析并钳制风控冷却阶梯；解析失败回退默认值。
//
// 回退而不是报错：这是被调度热路径调用的读取（每次命中风控都要选档），不能因为
// 一条脏设定就拒绝服务。PUT 层已经用 ValidRiskCoolingSteps 把非法输入挡在库外。
func (s *Store) RiskCoolingSteps() []int {
	if steps, err := parseRiskCoolingSteps(s.RiskCoolingStepsString()); err == nil {
		return steps
	}
	if steps, err := parseRiskCoolingSteps(config.RiskCoolingSteps); err == nil {
		return steps
	}
	// 编译期常量再坏也不该发生；兜个底避免返回 nil 切片让调用方 len()==0 误判
	// 成「阶梯为空 ⇒ 第一次命中就置 invalid」。
	return []int{300, 900, 3600}
}

// parseRiskCoolingSteps 解析逗号分隔的秒数；任何一项非法即整串作废。
//
// 刻意不做「跳过坏项、保留好项」：管理员填 "300,abc,3600" 时静默丢一项，会得到
// 一个他不知道有几档的阶梯（而档位数决定升级点），比直接报错危险得多。
func parseRiskCoolingSteps(raw string) ([]int, error) {
	parts := strings.Split(raw, ",")
	steps := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n < 1 {
			return nil, fmt.Errorf("冷却档位必须是正整数秒: %q", p)
		}
		steps = append(steps, min(n, riskCoolingStepMax))
	}
	if len(steps) == 0 {
		return nil, fmt.Errorf("冷却阶梯不能为空")
	}
	return steps, nil
}

// ── 上游 503 冷却阶梯 ────────────────────────────────────────────────────────
//
// 与风控阶梯同构但语义不同：503 是上游健康信号，超阶梯长度只封顶
// config.CoolingSeconds、不升 invalid（见 gateway.MarkUpstreamUnavailable）。
// 解析/钳制复用风控的 parseRiskCoolingSteps——两者的合法形态完全一致
// （逗号分隔的正整数秒、单档上限 7 天）。

// Upstream503CoolingStepsKey 上游 503 冷却阶梯的设定键（逗号分隔的秒数）。
const Upstream503CoolingStepsKey = "upstream_503_cooling_steps"

// Upstream503CoolingStepsString 返回设定原始串；缺失或为空回退环境变量默认值。
func (s *Store) Upstream503CoolingStepsString() string {
	if v, ok := s.GetSetting(Upstream503CoolingStepsKey); ok {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return config.Upstream503CoolingSteps
}

// NormalizeUpstream503CoolingSteps 校验并规范化 503 冷却阶梯（语义同风控版本）。
func NormalizeUpstream503CoolingSteps(raw string) (string, bool) {
	steps, err := parseRiskCoolingSteps(raw)
	if err != nil {
		return "", false
	}
	parts := make([]string, len(steps))
	for i, n := range steps {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ","), true
}

// Upstream503CoolingSteps 解析并钳制 503 冷却阶梯；解析失败回退默认值。
// 回退而不是报错：与风控同一理由——这是调度路径上的读取，脏设定不能拒绝服务。
func (s *Store) Upstream503CoolingSteps() []int {
	if steps, err := parseRiskCoolingSteps(s.Upstream503CoolingStepsString()); err == nil {
		return steps
	}
	if steps, err := parseRiskCoolingSteps(config.Upstream503CoolingSteps); err == nil {
		return steps
	}
	return []int{30, 60, 120}
}

// ── 線路自動巡檢設定 ────────────────────────────────────────────────────────
//
// 与领取设置同一约定：环境变量（ZCODE_PROXY_HEALTH_*）只是**默认值**，落库后
// 以后台设置页为准；调度器每轮重新读取，改完下一轮生效。

// ProxyHealthEnabled 自动巡检开关（缺失或非法回退环境变量值）。
func (s *Store) ProxyHealthEnabled() bool {
	return s.claimBool("proxy_health_enabled", config.ProxyHealthEnabled)
}

// ProxyHealthIntervalMinutes 巡检间隔（分钟，≥1；缺失或非法回退环境变量值）。
func (s *Store) ProxyHealthIntervalMinutes() int {
	return s.claimInt("proxy_health_interval", config.ProxyHealthIntervalMinutes, 1)
}

// ── 代理設定 ────────────────────────────────────────────────────────────────

// ProxyProfile 命名代理出口（設定以 JSON 儲存在 meta 表中）。
type ProxyProfile struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	URL     string `json:"url"`
	Enabled bool   `json:"enabled"`
}

// ListProxyProfiles 列出全部代理线路。
func (s *Store) ListProxyProfiles() []ProxyProfile {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listProxyProfilesLocked()
}

func (s *Store) listProxyProfilesLocked() []ProxyProfile {
	out := []ProxyProfile{}
	raw := s.settings["proxy_profiles"]
	if raw == "" {
		return out
	}
	var profiles []ProxyProfile
	if err := json.Unmarshal([]byte(raw), &profiles); err != nil {
		return out
	}
	for _, p := range profiles {
		if p.ID != "" && p.URL != "" {
			out = append(out, p)
		}
	}
	return out
}

func (s *Store) saveProxyProfilesLocked(profiles []ProxyProfile) error {
	data, err := marshalJSON(profiles)
	if err != nil {
		return err
	}
	s.settings["proxy_profiles"] = string(data)
	s.publishSettings()
	return s.setMeta("proxy_profiles", string(data))
}

// AddProxyProfile 新增命名代理出口。
func (s *Store) AddProxyProfile(name, url string, enabled bool) (ProxyProfile, error) {
	normalized, err := proxy.NormalizeProxyURL(url)
	if err != nil {
		return ProxyProfile{}, err
	}
	if normalized == nil {
		return ProxyProfile{}, errors.New("代理 URL 不能為空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	profiles := s.listProxyProfilesLocked()
	name = strings.TrimSpace(name)
	if name == "" {
		name = fmt.Sprintf("代理-%d", len(profiles)+1)
	}
	for _, p := range profiles {
		if p.Name == name {
			return ProxyProfile{}, errors.New("代理名稱已存在")
		}
	}
	profile := ProxyProfile{ID: "proxy-" + randomHex(4), Name: name, URL: *normalized, Enabled: enabled}
	profiles = append(profiles, profile)
	return profile, s.saveProxyProfilesLocked(profiles)
}

// UpdateProxyProfile 更新代理线路；同步指派了该线路的账号。
func (s *Store) UpdateProxyProfile(profileID, name, url string, enabled bool) (ProxyProfile, error) {
	normalized, err := proxy.NormalizeProxyURL(url)
	if err != nil {
		return ProxyProfile{}, err
	}
	if normalized == nil {
		return ProxyProfile{}, errors.New("代理 URL 不能為空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	profiles := s.listProxyProfilesLocked()
	target := -1
	for i, p := range profiles {
		if p.ID == profileID {
			target = i
			break
		}
	}
	if target < 0 {
		return ProxyProfile{}, ErrNotFound
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = profiles[target].Name
	}
	for i, p := range profiles {
		if i != target && p.Name == name {
			return ProxyProfile{}, errors.New("代理名稱已存在")
		}
	}
	profiles[target].Name = name
	profiles[target].URL = *normalized
	profiles[target].Enabled = enabled
	if err := s.saveProxyProfilesLocked(profiles); err != nil {
		return ProxyProfile{}, err
	}
	for _, acc := range s.allAccountsLocked() {
		if acc.ProxyID != nil && *acc.ProxyID == profileID {
			acc.ProxyURL = normalized
			if err := s.persistAccountLocked(acc); err != nil {
				return ProxyProfile{}, err
			}
		}
	}
	return profiles[target], nil
}

// ProxyReassign 删除线路后对「原本绑定它的账号」的处置结果。
type ProxyReassign struct {
	Assigned map[string]string // accountID → 改派到的新线路 ID
	Direct   []string          // 已无空閒线路可补、退回直连的 accountID
}

// DeleteProxyProfile 删除代理线路；不存在返回 false。
//
// 原本绑定该线路的账号不会被打成直连，而是先摘掉失效指派、再用「此刻仍然空閒」
// 的线路补位，尽量让这些账号继续有代理可用；只有当确实没有空閒线路时才退回直连
// ——退回时必须把过期的 ProxyURL 一并清掉，否则账号会继续用一条刚被删掉的线路。
//
// 全程在同一把锁内完成：删除、指派与落库之间不存在「账号既没了线路又留着旧地址」
// 的中间态。
func (s *Store) DeleteProxyProfile(profileID string) (bool, ProxyReassign, error) {
	reassign := ProxyReassign{Assigned: map[string]string{}}
	s.mu.Lock()
	defer s.mu.Unlock()

	profiles := s.listProxyProfilesLocked()
	remaining := make([]ProxyProfile, 0, len(profiles))
	removed := false
	for _, p := range profiles {
		if p.ID == profileID {
			removed = true
			continue
		}
		remaining = append(remaining, p)
	}
	if !removed {
		return false, reassign, nil
	}
	if err := s.saveProxyProfilesLocked(remaining); err != nil {
		return false, reassign, err
	}

	// 第一步：摘掉失效指派。必须先做，补位时看到的才是干净的占用情况。
	affected := []*model.Account{}
	for _, acc := range s.allAccountsLocked() {
		if acc.ProxyID != nil && *acc.ProxyID == profileID {
			acc.ProxyID = nil
			acc.ProxyURL = nil
			affected = append(affected, acc)
		}
	}

	// 第二步：按序补位；补不上才落回直连（此时 ProxyID/ProxyURL 已清空）。
	free := s.freeProxyProfilesLocked()
	for _, acc := range affected {
		if len(free) == 0 {
			reassign.Direct = append(reassign.Direct, acc.ID)
			if err := s.persistAccountLocked(acc); err != nil {
				return true, reassign, err
			}
			continue
		}
		p := free[0]
		free = free[1:]
		newID, newURL := p.ID, p.URL
		acc.ProxyID = &newID
		acc.ProxyURL = &newURL
		if err := s.persistAccountLocked(acc); err != nil {
			// 落库失败就退回直连，别把内存里的指针留成没写进去的状态。
			acc.ProxyID = nil
			acc.ProxyURL = nil
			reassign.Direct = append(reassign.Direct, acc.ID)
			continue
		}
		reassign.Assigned[acc.ID] = newID
	}
	return true, reassign, nil
}

// PurgeProxyProfiles 批量删除代理线路，并把「原本绑定它们的账号」统一改派。
//
// 与单条 DeleteProxyProfile 的两点差别：
//   - 先把整批线路一次性摘除、再统一改派——逐条删除会把 A 线路的账号补位到
//     同样待删的 B 线路上，紧接着又被二次改派，白白产生抖动；
//   - 补位规则多一层：没有空閒线路时改派到「当前绑定账号数最少」的线路
//     （并列取线路表顺序，先创建者优先），让存活线路摊薄负载；只有连候选
//     都没有（无启用线路）才退回直连，退回时同步清掉过期 ProxyURL。
//
// 多个账号同时待改派时按 store 内顺序（即账号入池顺序）逐个分配，每分配一个
// 就更新占用计数，批内也保持摊薄。不存在于线路表的 ID 静默跳过，不报错。
func (s *Store) PurgeProxyProfiles(ids []string) ([]string, ProxyReassign, error) {
	reassign := ProxyReassign{Assigned: map[string]string{}}
	purgeSet := map[string]bool{}
	for _, id := range ids {
		if id != "" {
			purgeSet[id] = true
		}
	}
	if len(purgeSet) == 0 {
		return nil, reassign, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	profiles := s.listProxyProfilesLocked()
	remaining := make([]ProxyProfile, 0, len(profiles))
	purged := []string{}
	for _, p := range profiles {
		if purgeSet[p.ID] {
			purged = append(purged, p.ID)
			continue
		}
		remaining = append(remaining, p)
	}
	if len(purged) == 0 {
		return nil, reassign, nil
	}
	if err := s.saveProxyProfilesLocked(remaining); err != nil {
		return nil, reassign, err
	}

	// 第一步：摘掉全部失效指派。必须先做，占用计数看到的才是干净状态
	//（这一步同时保证了改派绝不会落到同批待删的线路上）。
	accounts := s.allAccountsLocked()
	affected := []*model.Account{}
	for _, acc := range accounts {
		if acc.ProxyID != nil && purgeSet[*acc.ProxyID] {
			acc.ProxyID = nil
			acc.ProxyURL = nil
			affected = append(affected, acc)
		}
	}

	// 第二步：统计存活启用线路的占用数；「占用最少」天然涵盖空閒优先
	//（空閒 = 占用 0 ≤ 任何其它线路），并列时取线路表顺序。
	occupancy := map[string]int{}
	candidates := []ProxyProfile{}
	for _, p := range remaining {
		if !p.Enabled {
			continue
		}
		candidates = append(candidates, p)
		occupancy[p.ID] = 0
	}
	for _, acc := range accounts {
		if id := derefStr(acc.ProxyID); id != "" {
			if _, ok := occupancy[id]; ok {
				occupancy[id]++
			}
		}
	}

	// 第三步：逐个改派；没有候选才退回直连（此时 ProxyID/ProxyURL 已清空）。
	for _, acc := range affected {
		best := -1
		for i, p := range candidates {
			if best < 0 || occupancy[p.ID] < occupancy[candidates[best].ID] {
				best = i
			}
		}
		if best < 0 {
			reassign.Direct = append(reassign.Direct, acc.ID)
			if err := s.persistAccountLocked(acc); err != nil {
				return purged, reassign, err
			}
			continue
		}
		p := candidates[best]
		newID, newURL := p.ID, p.URL
		acc.ProxyID = &newID
		acc.ProxyURL = &newURL
		if err := s.persistAccountLocked(acc); err != nil {
			// 落库失败就退回直连，别把内存里的指针留成没写进去的状态。
			acc.ProxyID = nil
			acc.ProxyURL = nil
			reassign.Direct = append(reassign.Direct, acc.ID)
			continue
		}
		occupancy[p.ID]++
		reassign.Assigned[acc.ID] = newID
	}
	return purged, reassign, nil
}

// AssignProxyProfile 把账号指派到代理线路；profileID 为空表示直连。
func (s *Store) AssignProxyProfile(accountID, profileID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	acc := s.findAnyLocked(accountID)
	if acc == nil {
		return false, nil
	}
	var profileURL *string
	if profileID != "" {
		found := false
		for _, p := range s.listProxyProfilesLocked() {
			if p.ID == profileID {
				u := p.URL
				profileURL = &u
				found = true
				break
			}
		}
		if !found {
			return false, ErrProxyNotFound
		}
		acc.ProxyID = &profileID
		acc.ProxyURL = profileURL
	} else {
		acc.ProxyID = nil
		acc.ProxyURL = nil
	}
	return true, s.persistAccountLocked(acc)
}

// freeProxyProfilesLocked 列出「启用中且未被任何账号占用」的线路。
// 占用判定只看 ProxyID —— 手工填 proxy_url 的账号不占用命名线路。
// 注意 listProxyProfilesLocked 不过滤 Enabled，这里必须自筛。
func (s *Store) freeProxyProfilesLocked() []ProxyProfile {
	taken := map[string]bool{}
	for _, a := range s.allAccountsLocked() {
		if id := derefStr(a.ProxyID); id != "" {
			taken[id] = true
		}
	}
	out := []ProxyProfile{}
	for _, p := range s.listProxyProfilesLocked() {
		if p.Enabled && !taken[p.ID] {
			out = append(out, p)
		}
	}
	return out
}

// PickFreeProxyProfile 挑一条空闲线路但不写入任何账号。
//
// 登录会话需要在账号建立之前就把出口定下来（token 交换、API Key 兑换、额度刷新、
// 活动领取是同一条出站链路），所以不能等到 AutoAssignProxies 那一步。
func (s *Store) PickFreeProxyProfile() (ProxyProfile, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if free := s.freeProxyProfilesLocked(); len(free) > 0 {
		return free[0], true
	}
	return ProxyProfile{}, false
}

// AutoAssignProxies 给一批账号分配「尚未被任何账号占用」的代理线路。
//
// 单锁内原子完成：先按当前占用情况选出可用候选，再按 accountIDs 顺序逐个分配，
// 因此批量导入不会把同一条线路分给两个账号。候选不足时剩余账号保持直连
// （ProxyID/ProxyURL 均为 nil），由第二个返回值回报，供调用方提示用户。
//
// 注意：刻意不复用 AssignProxyProfile——它会自行加锁，在持锁上下文里调用会死锁。
func (s *Store) AutoAssignProxies(accountIDs []string) (assigned map[string]string, directFallback []string) {
	assigned = map[string]string{}
	directFallback = []string{}
	if len(accountIDs) == 0 {
		return assigned, directFallback
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	free := s.freeProxyProfilesLocked()
	for _, id := range accountIDs {
		acc := s.findAnyLocked(id)
		if acc == nil {
			continue
		}
		if len(free) == 0 {
			directFallback = append(directFallback, id)
			continue
		}
		p := free[0]
		free = free[1:]
		profileID, profileURL := p.ID, p.URL
		acc.ProxyID = &profileID
		acc.ProxyURL = &profileURL
		if err := s.persistAccountLocked(acc); err != nil {
			directFallback = append(directFallback, id)
			continue
		}
		assigned[id] = profileID
	}
	return assigned, directFallback
}

// ── 账号读取 ────────────────────────────────────────────────────────────────

// ListAccounts 列出账号；provider 为空表示全部。
//
// 返回的是**副本**：调用方改它不会影响 store（要改账号一律走 Update 或字段级方法）。
// 这样账号对象就不会被多个 goroutine 无锁共享——此前返回内部指针，调用方习惯
// 「锁外改字段 → UpdateAccount 落库」，与后台刷新/领取等路径相撞（CI 的 -race 抓到过）。
func (s *Store) ListAccounts(provider string) []*model.Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	if provider != "" {
		src := s.accounts[provider]
		out := make([]*model.Account, len(src))
		for i, a := range src {
			out[i] = a.Clone()
		}
		return out
	}
	var out []*model.Account
	for _, p := range Providers {
		for _, a := range s.accounts[p] {
			out = append(out, a.Clone())
		}
	}
	return out
}

// Find 按 provider + id/名称 查找账号；找不到返回 nil。返回副本（见 ListAccounts）。
func (s *Store) Find(provider, idOrName string) *model.Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.findLocked(provider, idOrName).Clone()
}

// FindAny 按 id 在全部提供商中查找账号；找不到返回 nil。返回副本（见 ListAccounts）。
func (s *Store) FindAny(idOrName string) *model.Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.findAnyLocked(idOrName).Clone()
}

// SnapshotAccount 返回账号的独立副本（找不到返回 nil）。
//
// 给**长活后台任务**用：领取这类操作可能持续数十秒，期间后台仍在改账号
// （删除线路改派 ProxyURL/ProxyID、额度刷新改 Status/Quota）。直接持有
// Find/ListAccounts 交出的内部指针，就会与这些写入相撞——CI 的 -race
// 实测到过（入池自动领取 vs 删除线路）。取副本在锁内完成，故副本内容是
// 一个内部一致的时点快照。
//
// 副作用：副本的改动不会进 store，回写必须走 Store 的方法（按 ID 落到
// 「当前」对象上），否则会用旧快照整体覆盖并发改动。
func (s *Store) SnapshotAccount(provider, idOrName string) *model.Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	acc := s.findLocked(provider, idOrName)
	if acc == nil {
		return nil
	}
	return acc.Clone()
}

func (s *Store) findLocked(provider, idOrName string) *model.Account {
	for _, a := range s.accounts[provider] {
		if a.ID == idOrName || a.Name == idOrName {
			return a
		}
	}
	return nil
}

func (s *Store) findAnyLocked(idOrName string) *model.Account {
	for _, p := range Providers {
		for _, a := range s.accounts[p] {
			if a.ID == idOrName {
				return a
			}
		}
	}
	return nil
}

func (s *Store) allAccountsLocked() []*model.Account {
	var out []*model.Account
	for _, p := range Providers {
		out = append(out, s.accounts[p]...)
	}
	return out
}

// ── 账号增删改 ──────────────────────────────────────────────────────────────

// AddAccount 添加账号；同一账号（按身份或凭据判定）已存在时直接返回既有记录。
func (s *Store) AddAccount(provider, name, secret string) (*model.Account, error) {
	acc, _, err := s.AddAccountWithIdentity(provider, name, secret, "")
	return acc, err
}

// AddAccountWithIdentity 带身份信息入池。email 只有 OAuth 路径需要传（该类 token
// 形态拿不到邮箱）；user_id 一律从 secret 派生，故手动添加 / 导入 / CLI 路径自动获得身份。
//
// 判重按优先级分三轮遍历，而不是单遍取首个命中：单遍会让列表里靠前的低优先级判据
// （凭据相同）抢先于靠后的高优先级判据（同一个 user_id），把"同一账号换了 token"
// 误判成两个账号。
//
// 第二个返回值 isNew 区分「本次新建」与「命中既有记录」：调用方（OAuth 重登、
// 自动分配代理线路）需要据此决定是否施加只对新号生效的默认值。
//
// ⚠️ 返回的是 **store 内部对象本身**（不是副本），刻意如此：登录链路依赖「后续
// AutoAssignProxies 改的就是同一个对象，读 account.ProxyURL 立刻能看到新线路」。
// 因此调用方**不得把它交给长活后台任务**，也不要长期持有——需要副本请用
// SnapshotAccount。读 API（Find/FindAny/ListAccounts/Select）返回的才是副本。
func (s *Store) AddAccountWithIdentity(provider, name, secret, email string) (*model.Account, bool, error) {
	if _, ok := s.providersSet()[provider]; !ok {
		return nil, false, fmt.Errorf("不支持的 provider: %s", provider)
	}
	acc := model.Create(provider, name, secret)
	if acc.Mode == "jwt" {
		if uid := model.JWTUserID(acc.Secret()); uid != "" {
			acc.UserID = &uid
		}
	}
	if trimmed := strings.TrimSpace(email); trimmed != "" {
		acc.Email = &trimmed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing := s.duplicateLocked(provider, acc); existing != nil {
		return existing, false, nil // 同一账号：返回既有记录，不新建
	}
	// 每账号独立设备指纹：全局共用一份会让同机多账号被上游按设备关联。
	mid := config.NewDeviceMid()
	acc.VirtualDeviceMid = &mid
	// 先落库再改内存：落库失败时内存不能留下一个不存在的账号。反过来会让账号在本次
	// 进程里可用、重启后消失，而调用方收到错误以为没建成。
	if err := s.persistAccountLocked(acc); err != nil {
		logPersistFailure("add", provider+"/"+acc.ID, err)
		return nil, false, err
	}
	s.accounts[provider] = append(s.accounts[provider], acc)
	return acc, true, nil
}

// duplicateLocked 按 user_id → email → 凭据 三轮判定是否已存在同一账号。
// 高优先级判据未命中时才降级：token 会刷新而凭据字节会变，且不同账号可能共用邮箱。
func (s *Store) duplicateLocked(provider string, acc *model.Account) *model.Account {
	list := s.accounts[provider]
	if uid := derefStr(acc.UserID); uid != "" {
		for _, a := range list {
			if derefStr(a.UserID) == uid {
				return a
			}
		}
	}
	if email := derefStr(acc.Email); email != "" {
		for _, a := range list {
			if derefStr(a.Email) == email {
				return a
			}
		}
	}
	if secret := strings.TrimSpace(acc.Secret()); secret != "" {
		for _, a := range list {
			if a.Secret() == secret {
				return a
			}
		}
	}
	return nil
}

// derefStr 取字符串指针值并去空白（nil 返回空串）。
func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(*p)
}

func (s *Store) providersSet() map[string]bool {
	// Providers 是固定小切片，直接现构集合即可。
	set := map[string]bool{}
	for _, p := range Providers {
		set[p] = true
	}
	return set
}

// RemoveAccount 删除账号；未找到返回 false。
func (s *Store) RemoveAccount(provider, idOrName string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := s.accounts[provider]
	target := s.findLocked(provider, idOrName)
	if target == nil {
		return false, nil
	}
	// 先落库再改内存：反过来时落库失败会让内存与 DB 分叉——本次进程里账号已消失，
	// 重启后又从 DB 载入回来。删除常被用来撤销可疑或外泄的凭证，这种「显示已删除、
	// 实际还在」属于安全相关的静默失败。
	if err := s.deleteAccountLocked(target.ID); err != nil {
		logPersistFailure("delete", provider+"/"+target.ID, err)
		return false, err
	}
	remaining := items[:0:0]
	for _, a := range items {
		if a.ID != target.ID {
			remaining = append(remaining, a)
		}
	}
	s.accounts[provider] = remaining
	return true, nil
}

// UpdateClaimState 在锁内把领取状态替换到**当前**账号对象上并落库，
// 返回是否命中账号（已删除返回 false 且不报错——领取任务与删除并发时属正常情况）。
//
// 为什么不能拿账号副本整体回写：领取（可能数十秒）跑在账号副本上，期间
// store 可能已改过该账号（例如删除线路改派了 ProxyURL）。用副本整体回写会把
// 这些改动一并覆盖回旧值，造成丢失更新，所以回写只针对领取状态这一个字段。
//
// fn 收到的是当前状态的副本（ClaimState 视为不可变），返回新状态整体替换；
// 返回 nil 表示不改。锁序 store.mu → claimMu，与 persistAccountLocked 一致。
func (s *Store) UpdateClaimState(
	provider, idOrName string,
	fn func(cur *model.ClaimState) *model.ClaimState,
) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	acc := s.findLocked(provider, idOrName)
	if acc == nil {
		return false, nil
	}
	cur := acc.ClaimView()
	if cur == nil {
		cur = &model.ClaimState{}
	}
	if next := fn(cur); next != nil {
		acc.SetClaimState(next)
	}
	return true, s.persistAccountLocked(acc)
}

// Update 在锁内对**当前**账号对象应用 fn（可读可写），随后落库，返回是否命中。
//
// 这是账号状态变更唯一安全的形态：读到的值与写出的值都由同一把锁保护，因此与
// 其它写入方（删除线路改派、另一路额度刷新、后台领取回写）天然串行。相对的
// 「锁外改字段 → 再 UpdateAccount 落库」会让并发读方（后台领取取快照、后台
// 列表序列化、另一路写入方）撞上——CI 的 -race 实测抓到过（quota 后台刷新
// 写 Plans/Quota/Status vs 领取任务读账号）。
//
// fn 内**不要**再调用 Store 的其它方法（本方法已持锁，会自锁）。
func (s *Store) Update(provider, idOrName string, fn func(*model.Account)) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	acc := s.findLocked(provider, idOrName)
	if acc == nil {
		return false, nil
	}
	fn(acc)
	failed := s.persistAccountLocked(acc)
	logPersistFailure("update", provider+"/"+idOrName, failed)
	return true, failed
}

// ── 字段级写入 ──────────────────────────────────────────────────────────────
//
// 下面这些方法把「按语义改某几个字段」收敛到锁内（都经 Update）。调用方先在
// 锁外完成解析与校验，再把结果交给这里应用——既不把请求体解析塞进闭包，也不
// 再走「锁外改字段 → UpdateAccount 落库」那种会让并发读方撞上的旧形态。

// AccountEdit 编辑账号时要替换的字段。用 Set* 布尔位区分「改成空/零值」与
// 「本次不改这一项」——只靠指针无法表达后者。
type AccountEdit struct {
	Name        *string  // 非 nil 时替换名称
	SetSecret   bool     // true 时按 SecretMode 替换凭据并恢复为 active
	Secret      string   //
	SecretMode  string   // "jwt" | "apiKey"
	SetProxyURL bool     // true 时替换出站地址（必要时解除线路指派）
	ProxyURL    *string  // nil 表示改为直连
	SetDisabled bool     // true 时替换停用模型清单
	Disabled    []string //
}

// EditAccount 按补丁替换账号字段并落库。
func (s *Store) EditAccount(provider, idOrName string, edit AccountEdit) (bool, error) {
	return s.Update(provider, idOrName, func(acc *model.Account) {
		if edit.Name != nil {
			acc.Name = *edit.Name
		}
		if edit.SetSecret {
			secret := edit.Secret
			if edit.SecretMode == "jwt" {
				acc.Mode = "jwt"
				acc.JWTToken = &secret
				acc.APIKey = nil
			} else {
				acc.Mode = "apiKey"
				acc.APIKey = &secret
				acc.JWTToken = nil
			}
			// 换凭据即视为重新可用：清掉旧的失效状态与错误（含错误归类与时间，
			// 否则前端会留下「已恢复健康但仍带着旧错误类型」的脏标记）。
			acc.Status = model.StatusActive
			acc.LastError = nil
			acc.LastErrorKind = nil
			acc.LastErrorAt = nil
			// 风控连续计数也要清零：它是「这个身份被拦了几次」的计数，而换凭据就是
			// 换身份。不清的话，一个被升到 invalid 的账号人工救回来之后，下一次风控
			// 命中就是老 streak + 1，会立刻又判失效——等于人工修复无效。
			acc.RiskControlStreak = 0
		}
		if edit.SetProxyURL {
			// 地址没变则保留原线路指派；变了说明要改成手工代理，解除指派。
			if !sameStringPtr(edit.ProxyURL, acc.ProxyURL) {
				acc.ProxyID = nil
			}
			acc.ProxyURL = edit.ProxyURL
		}
		if edit.SetDisabled {
			acc.SetDisabledModels(edit.Disabled)
		}
	})
}

// SetProxyURL 设置账号的手工出站地址并解除线路指派（url 为 nil 表示直连）。
func (s *Store) SetProxyURL(provider, idOrName string, url *string) (bool, error) {
	return s.Update(provider, idOrName, func(acc *model.Account) {
		acc.ProxyID = nil
		acc.ProxyURL = url
	})
}

// SetIdentity 补写账号身份（OAuth 登录后回填邮箱、或把 oauth-login 正名为邮箱）。
// email / name 传 nil 表示不改该项。
func (s *Store) SetIdentity(provider, idOrName string, email, name *string) (bool, error) {
	return s.Update(provider, idOrName, func(acc *model.Account) {
		if email != nil {
			acc.Email = email
		}
		if name != nil {
			acc.Name = *name
		}
	})
}

// SetAPIKey 写入兑换得到的 API Key（OAuth 登录链路）。
func (s *Store) SetAPIKey(provider, idOrName, key string) (bool, error) {
	return s.Update(provider, idOrName, func(acc *model.Account) {
		acc.APIKey = &key
	})
}

// SetDisabledModels 替换账号的停用模型清单（导入链路）。
func (s *Store) SetDisabledModels(provider, idOrName string, models []string) (bool, error) {
	return s.Update(provider, idOrName, func(acc *model.Account) {
		acc.SetDisabledModels(models)
	})
}

// ResetTokenStats 清零账号的累计 token 统计（后台「重置統計」）。
func (s *Store) ResetTokenStats(provider, idOrName string) (bool, error) {
	return s.Update(provider, idOrName, func(acc *model.Account) {
		acc.ResetTokenStats()
	})
}

// sameStringPtr 判断两个可空字符串内容相同（都为 nil 视为相同）。
// 与 adminapi 的同名助手语义一致，各自留在包内避免为一个纯函数建依赖。
func sameStringPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// SetEnabled 启用/禁用账号（禁用同时置 DISABLED 状态）。
func (s *Store) SetEnabled(provider, idOrName string, enabled bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	acc := s.findLocked(provider, idOrName)
	if acc == nil {
		return false, nil
	}
	acc.Enabled = enabled
	if !enabled {
		acc.Status = model.StatusDisabled
	} else if acc.Status == model.StatusDisabled {
		acc.Status = model.StatusActive
	}
	if err := s.persistAccountLocked(acc); err != nil {
		return false, err
	}
	return true, nil
}

// SetArchived 归档/恢复账号：归档即强制停用（无论原状态），调度、领取、刷新全部跳过；
// 恢复后保持停用状态，需手动启用才会重新参与调度。归档时间取当前时刻。
func (s *Store) SetArchived(provider, idOrName string, archived bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	acc := s.findLocked(provider, idOrName)
	if acc == nil {
		return false, nil
	}
	if archived {
		now := float64(time.Now().UnixNano()) / 1e9
		acc.ArchivedAt = &now
		acc.Enabled = false
		acc.Status = model.StatusDisabled
	} else {
		acc.ArchivedAt = nil
	}
	if err := s.persistAccountLocked(acc); err != nil {
		return false, err
	}
	return true, nil
}

// ── 额度优先选择 ────────────────────────────────────────────────────────────

// Select 按模型额度优先、并列时轮询的方式选择账号。
//
// 选号分五层，前一层筛出的池子决定后一层的作用范围：
//  1. 可选：状态可调度（IsSelectable）且未被本次请求跳过（skipIDs）；
//  2. 模型分档：该模型 available 者优先，其次 unknown；absent/exhausted 一律排除；
//  3. 优惠优先：持有未耗尽的一次性优惠额度（one_time）者优先——这类额度不用就过期，
//     与「谁剩得多」是两回事，故独立成层；
//  4. 额度优先：剩余可用额度（token）最多者优先；
//  5. 轮询：在第 4 层并列的账号之间按 round-robin 游标轮转。
//
// skipIDs 保证同一次请求不会重复尝试已失败的账号。
func (s *Store) Select(provider string, skipIDs map[string]bool, modelName string) *model.Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	var base []*model.Account
	for _, a := range s.accounts[provider] {
		if a.IsSelectable(now) && !skipIDs[a.ID] {
			base = append(base, a)
		}
	}
	pool := base
	if modelName != "" {
		var available, unknown []*model.Account
		for _, a := range base {
			switch a.ModelAvailability(modelName) {
			case "available":
				available = append(available, a)
			case "unknown":
				unknown = append(unknown, a)
			}
		}
		if len(available) > 0 {
			pool = available
		} else {
			pool = unknown
		}
	}
	if len(pool) == 0 {
		return nil
	}
	// 持有未耗尽一次性优惠（period 含 one_time 且 remaining>0）的账号优先：
	// 优惠额度不用会过期，每日体验额度次日刷新；优惠组耗尽后自然回落全池轮询。
	var promo, regular []*model.Account
	for _, a := range pool {
		if hasPromoQuota(a) {
			promo = append(promo, a)
		} else {
			regular = append(regular, a)
		}
	}
	if len(promo) > 0 {
		pool = promo
	} else {
		pool = regular
	}
	// 额度优先：只保留剩余额度最多的那一组，再由下面的游标在组内轮转。
	//
	// 为什么是「并列组 + 组内轮转」而不是「永远取最大的那一个」：额度快照是按账号
	// 缓存的（quota.QuotaCacheTTL，15 秒）且只在账号被使用时才刷新，所以同一份数值会
	// 在一段时间里保持不变。取并列组既保住了「额度多的先用」这个方向，又不会把全部
	// 流量压在一个账号上；同规格账号（同一套餐、剩余量相同）会整体并列，行为与纯轮询
	// 一致。
	//
	// 没有额度数值的账号记 -1：排在任何有数值者之后，但彼此之间照样并列。这与第 2 层
	// 「unknown 作后备」同向，也不会把「还不知道」误当成「已经用完（0）」。
	best := 0.0
	var top []*model.Account
	for i, a := range pool {
		v, ok := a.UsableQuotaForModel(modelName)
		if !ok {
			v = -1
		}
		switch {
		case i == 0 || v > best:
			best, top = v, []*model.Account{a}
		case v == best:
			top = append(top, a)
		}
	}
	pool = top
	key := provider + ":" + orStar(modelName)
	idx := s.rotation[key] % len(pool)
	acc := pool[idx]
	s.rotation[key] = (idx + 1) % len(pool)
	// 返回副本：网关/异步工单会跨整个请求（乃至数十秒的流式转发）持有它，
	// 期间后台仍在改这个账号。改动一律经 Update 按 ID 落到「当前」对象上。
	return acc.Clone()
}

func orStar(modelName string) string {
	if modelName == "" {
		return "*"
	}
	return modelName
}

// ── 池状态聚合（no_available_account 出口用） ────────────────────────────────
//
// 背景：503「無可用帳號」的文案曾是静态字符串，线上一次整池冷却事故里，客户端
// 与运维都以为「账号没绑定模型/额度用完」，实际是全池冷却中（见
// docs/analysis-503-no-available-account-20260920.md）。此聚合让错误体与日志
// 能如实回答「此刻池子里每个账号为什么不可选」。
//
// 只在选号失败的出口路径调用（低频），锁内 O(n) 遍历可接受；不进请求热路径。

// PoolStat 池状态快照：按「为什么不可选」分桶 + 冷却细分 + 指定模型的可用性分布。
//
// 桶口径与 Select 第一层一致（IsSelectable）：归档 / 停用 / 失效 / 整号耗尽 /
// 冷却中（未到期）/ 其余计入 Active。Active 桶再按 ModelAvailability(modelName)
// 细分，直接回答「健康账号里有多少被模型层排除、为什么」。
type PoolStat struct {
	Total     int
	Archived  int
	Disabled  int // Enabled=false 或 Status=disabled
	Invalid   int
	Exhausted int // 整号 exhausted（全部已知模型耗尽）
	Cooling   int // Status=cooling 且冷却未到期
	Active    int // 通过健康门槛（Select 第一层）的账号数
	// CoolingEarliest 冷却账号中最早的到期时间（unix 秒）；无冷却账号为 nil。
	CoolingEarliest *float64
	// CoolingByKind 冷却账号按 last_error_kind 细分（upstream_unavailable /
	// risk_control / rate_limited / …），键为空串表示未记录成因。
	CoolingByKind map[string]int
	// ModelStat Active 账号中该模型的可用性分布
	// （available/exhausted/absent/disabled/unknown 计数）。modelName 为空时为 nil。
	ModelStat map[string]int
}

// PoolStats 聚合指定 provider 池的状态快照。modelName 非空时附带模型维度分布。
func (s *Store) PoolStats(provider, modelName string) PoolStat {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	stat := PoolStat{CoolingByKind: map[string]int{}}
	for _, a := range s.accounts[provider] {
		stat.Total++
		if a.ArchivedAt != nil {
			stat.Archived++
			continue
		}
		if !a.Enabled || a.Status == model.StatusDisabled {
			stat.Disabled++
			continue
		}
		switch a.Status {
		case model.StatusInvalid:
			stat.Invalid++
			continue
		case model.StatusExhausted:
			stat.Exhausted++
			continue
		case model.StatusCooling:
			// 与 IsSelectable 同口径：到期未刷新状态的冷却账号视为已恢复，
			// 归入 Active（Select 第一层就是这么放行的）。
			if a.CoolingUntil == nil || now.Before(time.Unix(0, int64(*a.CoolingUntil*1e9))) {
				stat.Cooling++
				if a.CoolingUntil != nil &&
					(stat.CoolingEarliest == nil || *a.CoolingUntil < *stat.CoolingEarliest) {
					v := *a.CoolingUntil
					stat.CoolingEarliest = &v
				}
				kind := ""
				if a.LastErrorKind != nil {
					kind = *a.LastErrorKind
				}
				stat.CoolingByKind[kind]++
				continue
			}
		}
		stat.Active++
		if modelName != "" {
			if stat.ModelStat == nil {
				stat.ModelStat = map[string]int{}
			}
			stat.ModelStat[a.ModelAvailability(modelName)]++
		}
	}
	return stat
}

// hasPromoQuota 账号额度快照中是否存在未耗尽的一次性优惠额度。
// 快照缺 remaining 视为未知（不参与优先判定），仅明确 remaining>0 才算优惠在握。
func hasPromoQuota(a *model.Account) bool {
	for _, q := range a.Quota {
		period, _ := q["period"].(string)
		if !strings.Contains(period, "one_time") {
			continue
		}
		if v, ok := asNumber(q["remaining"]); ok && v > 0 {
			return true
		}
	}
	return false
}

func asNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	}
	return 0, false
}

// ── 导入 / 导出 ─────────────────────────────────────────────────────────────

type exportAccount struct {
	Name           string   `json:"name"`
	Mode           string   `json:"mode"`
	Secret         string   `json:"secret"`
	DisabledModels []string `json:"disabled_models"`
}

// ExportPayload 导出格式（version 1，与 Python 版一致）。
type ExportPayload struct {
	Version    int                        `json:"version"`
	ExportedAt float64                    `json:"exported_at"`
	Providers  map[string][]exportAccount `json:"providers"`
}

// Export 导出全部账号（含明文凭证，仅用于备份/迁移）。
func (s *Store) Export() ExportPayload {
	s.mu.Lock()
	defer s.mu.Unlock()
	providers := map[string][]exportAccount{}
	for _, p := range Providers {
		list := []exportAccount{}
		for _, a := range s.accounts[p] {
			disabled := a.DisabledModels
			if disabled == nil {
				disabled = []string{}
			}
			list = append(list, exportAccount{
				Name:           a.Name,
				Mode:           a.Mode,
				Secret:         a.Secret(),
				DisabledModels: disabled,
			})
		}
		providers[p] = list
	}
	return ExportPayload{
		Version:    1,
		ExportedAt: float64(time.Now().UnixNano()) / 1e9,
		Providers:  providers,
	}
}

type importItem struct {
	Name           string   `json:"name"`
	Secret         string   `json:"secret"`
	Token          string   `json:"token"`
	JWTToken       string   `json:"jwtToken"`
	APIKey         string   `json:"apiKey"`
	DisabledModels []string `json:"disabled_models"`
}

// ImportPayload 导入格式（与 Python 版兼容；secret 可用多个别名键）。
type ImportPayload struct {
	Providers map[string][]importItem `json:"providers"`
}

// ImportAccounts 导入账号，返回导入数量与本次「新建」的账号 ID。
// 命中既有记录（重复导入）时计数仍递增，但不计入 newIDs —— 调用方据此只给新号
// 分配代理线路，不会去动老号已有的线路指派。
func (s *Store) ImportAccounts(payload ImportPayload) (int, []string, error) {
	count := 0
	newIDs := []string{}
	for provider, items := range payload.Providers {
		if !s.providersSet()[provider] {
			continue
		}
		for _, item := range items {
			secret := firstNonEmpty(item.Secret, item.Token, item.JWTToken, item.APIKey)
			if secret == "" {
				continue
			}
			acc, isNew, err := s.AddAccountWithIdentity(provider, item.Name, secret, "")
			if err != nil {
				return count, newIDs, err
			}
			if item.DisabledModels != nil {
				if _, err := s.SetDisabledModels(provider, acc.ID, item.DisabledModels); err != nil {
					return count, newIDs, err
				}
			}
			if isNew {
				newIDs = append(newIDs, acc.ID)
			}
			count++
		}
	}
	return count, newIDs, nil
}

// ── 内部工具 ────────────────────────────────────────────────────────────────

func randomTokenURLSafe(nBytes int) string {
	b := make([]byte, nBytes)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
