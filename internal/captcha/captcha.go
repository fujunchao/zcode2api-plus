// Package captcha 阿里云无痕验证码的配置获取、令牌缓存与求解器编排。
// 对应 Python 版 app/captcha.py。
//
// 范围：配置缓存（默认 1 小时 + last-good）、人工回填令牌缓存（45 秒）、
// Solver 接口与失效语义、可选预求解 token 池（token_pool.go，一次性 + TTL）。
// 浏览器求解器（rod 驱动 cloakbrowser 的 Chromium）在 M4 提供。令牌只在内存，
// 不落盘、不进日志；token 池同样不打印 token 原文。
package captcha

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/web"
)

// Token 一次无痕验证的结果。
type Token struct {
	VerifyParam string // base64(JSON{certifyId, sceneId, isSign, securityToken})
	Region      string // 请求头 X-Aliyun-Captcha-Verify-Region 所需
	// TakeStats 本次获取的诊断信息（等待/池大小/是否空池/补解失败数），
	// 只用于日志与指标，不包含 token 原文。
	TakeStats TakeStats
}

// Config 上游下发的验证码配置。
type Config struct {
	Enabled bool
	Prefix  string
	Region  string
	SceneID string
	// SkipModelRequest 只跳过模型请求验证；套餐领取仍使用 Enabled 控制。
	SkipModelRequest bool
}

// DefaultConfig 上游配置接口不可用时的兜底值（region 与线上实测一致）。
var DefaultConfig = Config{Enabled: true, Prefix: "no8xfe", Region: "cn", SceneID: "11xygtvd"}

// ErrUnavailable 无可用求解器（M4 前的常态；依赖人工回填兜底）。
var ErrUnavailable = errors.New("验证码求解器不可用（等待浏览器池或后台人工回填）")

// Solver 真实验证码求解器接口；M4 由 rod 浏览器池实现。
type Solver interface {
	Solve(ctx context.Context, cfg Config) (string, error)
	Close() error
}

// ConfigProvider 配置获取函数（默认请求上游接口；测试可覆写以避免触网）。
type ConfigProvider func(ctx context.Context) (Config, error)

// configURL 验证码配置端点（测试可覆写）。
var configURL = "https://zcode.z.ai/api/v1/client/configs"

// Manager 令牌缓存 + 配置缓存 + 求解器编排 + 可选 token 池。
type Manager struct {
	solver         Solver
	configProvider ConfigProvider

	mu        sync.Mutex
	cached    *Token
	cachedAt  time.Time
	cachedTTL time.Duration
	// lastGoodConfig 最后一次成功拉取的上游配置；配置接口故障时继续使用。
	lastGoodConfig *Config
	lastGoodAt     time.Time
	// configFailUntil 配置刷新失败后的短暂退避截止：last-good 存在时避免每个
	// JWT 请求都去打故障中的配置接口。到点前直接返回 last-good。
	configFailUntil time.Time
	pool            *tokenPool

	now func() time.Time
}

// captchaConfigFailureBackoff 配置接口失败后、last-good 仍可用时的重试间隔。
const captchaConfigFailureBackoff = 30 * time.Second

// NewManager 创建管理器。
func NewManager() *Manager {
	return &Manager{now: time.Now, configProvider: fetchConfigHTTP}
}

// SetSolver 注入浏览器求解器（M4）；nil 表示暂无。
func (m *Manager) SetSolver(s Solver) {
	m.mu.Lock()
	m.solver = s
	pool := m.pool
	m.mu.Unlock()
	if pool != nil {
		pool.SetSolver(s)
	}
}

// EnableTokenPool 开启/关闭预求解 token 池。关闭时同步停止 refill 与在途 mint；
// 池开启后仍需 SetConfig（由 FetchConfig 成功路径完成）才进入可服务状态。
func (m *Manager) EnableTokenPool(enabled bool) {
	m.mu.Lock()
	if !enabled {
		pool := m.pool
		m.pool = nil
		m.mu.Unlock()
		if pool != nil {
			_ = pool.Close()
		}
		return
	}
	if m.pool != nil {
		m.mu.Unlock()
		return
	}
	pool := newTokenPool(m.solver, defaultTokenPoolOptions(), m.now)
	cfg := m.lastGoodConfig
	m.pool = pool
	m.mu.Unlock()
	if cfg != nil {
		pool.SetConfig(*cfg, true)
	}
}

// Warm 预热一条验证码 token 入桶（不消费、不阻塞）。
//
// 用于「已知即将需要验证码」的场景：定时领取前提前 2 分钟调用，领取真正
// 开始时 token 已就绪，第一个账号不必吃约 7.6s 的浏览器冷启动。
//
// 幂等：池里已有库存或在途求解时什么都不做。token 池未启用时是空操作
// （逐请求求解路径本身就会拉起浏览器，没有「预热」可做）。
func (m *Manager) Warm(ctx context.Context) error {
	if m == nil || !config.CaptchaBrowserEnabled {
		return nil
	}
	cfg := m.FetchConfig(ctx)
	if !cfg.Enabled {
		return nil
	}
	m.mu.Lock()
	pool := m.pool
	m.mu.Unlock()
	if pool != nil {
		pool.Warm()
	}
	return nil
}

// BrowserStatus 转发浏览器求解器的运行态。
//
// 求解器是接口（测试注入假实现），只有真实 *BrowserSolver 才有状态可报；
// 其余情况返回 disabled，监控页据此显示「按需启用」。
func (m *Manager) BrowserStatus() BrowserStatus {
	if m == nil {
		return BrowserStatus{State: "disabled"}
	}
	m.mu.Lock()
	solver := m.solver
	m.mu.Unlock()
	if bs, ok := solver.(*BrowserSolver); ok {
		return bs.Status()
	}
	return BrowserStatus{State: "disabled"}
}

// TokenPoolStats 返回 token 池指标；未启用时返回零值。
func (m *Manager) TokenPoolStats() TokenPoolStats {
	if m == nil {
		return TokenPoolStats{}
	}
	m.mu.Lock()
	pool := m.pool
	m.mu.Unlock()
	if pool == nil {
		return TokenPoolStats{}
	}
	return pool.Stats()
}

// SetConfigProvider 注入配置获取函数（测试用）。
func (m *Manager) SetConfigProvider(p ConfigProvider) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.configProvider = p
}

// SetNow 注入时钟（测试用）。
func (m *Manager) SetNow(fn func() time.Time) {
	m.mu.Lock()
	m.now = fn
	pool := m.pool
	m.mu.Unlock()
	if pool != nil {
		pool.SetNow(fn)
	}
}

// FetchConfig 拉取上游验证码配置。
//
// 语义（P0-B）：
//   - 成功才写缓存，TTL 默认 1 小时（CaptchaConfigCacheTTL，env 可覆盖）；
//   - 失败不覆盖、不清空 last-good；有 last-good 时继续返回它，
//     并在 30 秒退避窗口内不再重复打故障接口；
//   - 无 last-good 时保持既有回退：返回 DefaultConfig（Enabled=true），
//     由调用方按原有严格获取语义决定是否求解。
func (m *Manager) FetchConfig(ctx context.Context) Config {
	m.mu.Lock()
	now := m.now()
	if m.lastGoodConfig != nil &&
		now.Sub(m.lastGoodAt) < time.Duration(config.CaptchaConfigCacheTTL)*time.Millisecond {
		cfg := *m.lastGoodConfig
		m.mu.Unlock()
		return cfg
	}
	if m.lastGoodConfig != nil && now.Before(m.configFailUntil) {
		cfg := *m.lastGoodConfig
		m.mu.Unlock()
		return cfg
	}
	m.mu.Unlock()

	cfg, err := m.configProvider(ctx)
	now = m.now()
	m.mu.Lock()
	if err != nil {
		if m.lastGoodConfig != nil {
			m.configFailUntil = now.Add(captchaConfigFailureBackoff)
			lastGood := *m.lastGoodConfig
			m.mu.Unlock()
			web.Warn("captcha", fmt.Sprintf(
				"验证码配置刷新失败，继续使用 last-good（%s 内不重试）: %s",
				captchaConfigFailureBackoff, redact(err.Error())))
			return lastGood
		}
		m.mu.Unlock()
		return DefaultConfig
	}
	oldKey := ""
	if m.lastGoodConfig != nil {
		oldKey = captchaConfigKey(*m.lastGoodConfig)
	}
	m.lastGoodConfig = &cfg
	m.lastGoodAt = now
	m.configFailUntil = time.Time{}
	pool := m.pool
	m.mu.Unlock()
	// 成功配置只在锁外通知池：SetConfig 会清旧桶，不能与 Manager 锁交叉。
	if pool != nil {
		pool.SetConfig(cfg, true)
		if oldKey != "" && oldKey != captchaConfigKey(cfg) {
			web.Ok("captcha", "验证码配置键变化，token 池已切换到新配置")
		}
	}
	return cfg
}

// fetchConfigHTTP 拉取并校验上游验证码配置。
//
// 校验语义对齐 ZCode 3.18.0 客户端 readValidatedRequestVerificationConfig：
// 先验证业务 envelope 的 code/success，再验证 captcha 对象、显式布尔字段，
// 以及主动验证码场景（enabled=true）必需的 prefix/region/sceneId。
// 任一处不合规都返回错误，由 FetchConfig 继续使用 last-good（或回退
// DefaultConfig），避免残缺配置被当作成功值写入缓存、覆盖此前有效配置。
func fetchConfigHTTP(ctx context.Context) (Config, error) {
	endpoint, err := url.Parse(configURL)
	if err != nil {
		return Config{}, err
	}
	query := endpoint.Query()
	query.Set("app_version", config.ZcodeClientVersion)
	query.Set("platform", config.ZcodeQueryPlatform())
	endpoint.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return Config{}, err
	}
	// 公开配置与账号无关。来源档案可以发送，JWT/API Key、Cookie、模型归因不可发送。
	for key, value := range config.ZcodeSourceHeaders() {
		req.Header.Set(key, value)
	}
	// 官方公共配置拒绝重定向，避免来源信息跟随到不同目标。
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		return Config{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Config{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Code    json.RawMessage `json:"code"`
		Success json.RawMessage `json:"success"`
		Data    struct {
			Configs struct {
				// RawMessage 才能区分「缺失」「显式 null」「类型错误」；
				// 强类型指针把 null 与缺失都变成零值，无法对齐 3.18.0 的严格语义。
				Captcha json.RawMessage `json:"captcha"`
			} `json:"configs"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return Config{}, err
	}
	if err := validateEnvelope(payload.Code, payload.Success); err != nil {
		return Config{}, err
	}
	raw := bytes.TrimSpace(payload.Data.Configs.Captcha)
	switch {
	case len(raw) == 0:
		return Config{}, errors.New("上游配置缺少 captcha 对象")
	case bytes.Equal(raw, []byte("null")):
		return Config{}, errors.New("上游配置 captcha 为 null，必须为对象")
	case raw[0] != '{':
		return Config{}, fmt.Errorf("上游配置 captcha 不是对象: %s", payloadSnippet(raw))
	}
	var c struct {
		Enabled          json.RawMessage `json:"enabled"`
		Prefix           json.RawMessage `json:"prefix"`
		Region           json.RawMessage `json:"region"`
		SceneID          json.RawMessage `json:"sceneId"`
		SkipModelRequest json.RawMessage `json:"skip_model_request"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return Config{}, fmt.Errorf("上游配置 captcha 解析失败: %w", err)
	}
	// 客户端把 enabled 当作可选布尔：缺失时按启用处理，显式 null/非布尔才算无效。
	enabled, err := optionalBoolDefaultTrue(c.Enabled, "enabled")
	if err != nil {
		return Config{}, err
	}
	skip, err := optionalBool(c.SkipModelRequest, "skip_model_request")
	if err != nil {
		return Config{}, err
	}
	cfg := Config{Enabled: enabled, SkipModelRequest: skip}
	// 客户端仅在「未禁用且未跳过模型请求」时要求主动验证码三字段齐全；
	// skip_model_request=true 时允许只下发跳过标记。
	if enabled && !skip {
		if cfg.Prefix, err = requireNonEmptyString(c.Prefix, "prefix"); err != nil {
			return Config{}, err
		}
		if cfg.Region, err = requireNonEmptyString(c.Region, "region"); err != nil {
			return Config{}, err
		}
		if cfg.SceneID, err = requireNonEmptyString(c.SceneID, "sceneId"); err != nil {
			return Config{}, err
		}
		return cfg, nil
	}
	// enabled=false 是合法最小配置：未启用的字段不做要求，仅尽力保留字符串值。
	cfg.Prefix = optionalString(c.Prefix)
	cfg.Region = optionalString(c.Region)
	cfg.SceneID = optionalString(c.SceneID)
	return cfg, nil
}

// validateEnvelope 校验响应包装的业务状态（对齐 3.18.0 客户端）：
// code 出现时必须为 0，显式 null 或非数字同样视为失败；
// success 显式为 false 视为业务失败，缺失或其它取值不额外解释。
func validateEnvelope(code, success json.RawMessage) error {
	if raw := bytes.TrimSpace(code); len(raw) > 0 {
		if bytes.Equal(raw, []byte("null")) {
			return errors.New("上游配置业务状态异常: code 为 null")
		}
		// json.Number 会把 JSON 字符串 "0" 也当合法值，先按首字节排除非数字字面量。
		if raw[0] != '-' && (raw[0] < '0' || raw[0] > '9') {
			return fmt.Errorf("上游配置业务状态异常: code 不是数字（%s）", payloadSnippet(raw))
		}
		var num json.Number
		if err := json.Unmarshal(raw, &num); err != nil {
			return fmt.Errorf("上游配置业务状态异常: code 不是数字（%s）", payloadSnippet(raw))
		}
		if f, err := num.Float64(); err != nil || f != 0 {
			return fmt.Errorf("上游配置业务状态异常: code=%s", num.String())
		}
	}
	if raw := bytes.TrimSpace(success); len(raw) > 0 && bytes.Equal(raw, []byte("false")) {
		return errors.New("上游配置业务状态异常: success=false")
	}
	return nil
}

// requireBool 读取必须是显式 JSON 布尔的字段；缺失、null 或其它类型都视为配置不完整。
func requireBool(raw json.RawMessage, field string) (bool, error) {
	switch string(bytes.TrimSpace(raw)) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "":
		return false, fmt.Errorf("上游配置 captcha.%s 缺失，必须为显式布尔值", field)
	default:
		return false, fmt.Errorf("上游配置 captcha.%s 必须为显式布尔值（实际 %s）", field, payloadSnippet(raw))
	}
}

// optionalBoolDefaultTrue 读取可选布尔字段：缺失按 true 处理（客户端语义），
// 出现时必须是显式布尔；显式 null 与非布尔都视为配置不完整。
func optionalBoolDefaultTrue(raw json.RawMessage, field string) (bool, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return true, nil
	}
	return requireBool(raw, field)
}

// optionalBool 读取可选布尔字段：缺失视为 false，出现时必须是显式布尔。
func optionalBool(raw json.RawMessage, field string) (bool, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return false, nil
	}
	return requireBool(raw, field)
}

// requireNonEmptyString 校验启用验证码时的必需字符串字段（去空白后不能为空）。
func requireNonEmptyString(raw json.RawMessage, field string) (string, error) {
	value, ok := jsonString(raw)
	if !ok {
		return "", fmt.Errorf("上游配置 captcha.%s 必须为非空字符串（实际 %s）", field, payloadSnippet(raw))
	}
	if value == "" {
		return "", fmt.Errorf("上游配置 captcha.%s 不能为空或纯空白", field)
	}
	return value, nil
}

// optionalString 读取可选字符串字段：缺失或非字符串按空串处理（未启用时不校验）。
func optionalString(raw json.RawMessage) string {
	value, _ := jsonString(raw)
	return value
}

// jsonString 解析 JSON 字符串并去掉首尾空白；缺失、null 或非字符串返回 false。
func jsonString(raw json.RawMessage) (string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '"' {
		return "", false
	}
	var value string
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return "", false
	}
	return strings.TrimSpace(value), true
}

// payloadSnippet 截断原始 JSON 片段，便于在日志里定位问题又避免刷屏。
func payloadSnippet(raw json.RawMessage) string {
	const maxRunes = 48
	runes := []rune(strings.TrimSpace(string(raw)))
	if len(runes) > maxRunes {
		return string(runes[:maxRunes]) + "…"
	}
	return string(runes)
}

// GetVerifyParam 返回领取等非模型操作的验证码令牌，不受 SkipModelRequest 影响。
// 语义：人工回填缓存命中直接返回；配置禁用返回 (nil, nil)（无需验证码）；
// 浏览器令牌优先走 token 池（一次性 + TTL），池未启用时逐请求求解，不写人工缓存。
func (m *Manager) GetVerifyParam(ctx context.Context) (*Token, error) {
	return m.getVerifyParam(ctx, false)
}

// GetModelVerifyParam 返回模型请求验证码；上游允许跳过时返回 (nil, nil)。
// 必须先查模型策略再读人工缓存，避免跳过后仍携带旧 token。
// 模型跳过不清空共享缓存：其中的人工 token 可能仍用于套餐领取。
func (m *Manager) GetModelVerifyParam(ctx context.Context) (*Token, error) {
	return m.getVerifyParam(ctx, true)
}

func (m *Manager) getVerifyParam(ctx context.Context, modelRequest bool) (*Token, error) {
	if ctx == nil {
		ctx = context.Background() // 允许 nil ctx（claim 等后台调用）
	}
	var cfg Config
	if modelRequest {
		cfg = m.FetchConfig(ctx)
		if !cfg.Enabled || cfg.SkipModelRequest {
			return nil, nil
		}
	}
	m.mu.Lock()
	if m.cached != nil && m.now().Sub(m.cachedAt) < m.cachedTTL {
		t := *m.cached
		m.mu.Unlock()
		return &t, nil
	}
	m.mu.Unlock()

	if !modelRequest {
		// 保留领取的缓存优先语义；模型入口上面已获取配置，不重复请求。
		cfg = m.FetchConfig(ctx)
	}
	if !cfg.Enabled {
		m.Invalidate()
		return nil, nil
	}

	// 浏览器求解路径：优先从 token 池热取；池不可用时退回逐请求求解。
	m.mu.Lock()
	solver := m.solver
	pool := m.pool
	m.mu.Unlock()
	if config.CaptchaBrowserEnabled && solver != nil {
		if pool != nil && pool.Ready(cfg) {
			return pool.Take(ctx, cfg)
		}
		started := time.Now()
		param, err := solver.Solve(ctx, cfg)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(param) == "" {
			return nil, errors.New("真实浏览器验证码求解器返回空结果")
		}
		return &Token{
			VerifyParam: param,
			Region:      cfg.Region,
			TakeStats:   TakeStats{Valid: true, Wait: time.Since(started)},
		}, nil
	}

	return nil, ErrUnavailable
}

// SetManualParam 缓存人工回填的令牌（后台 /admin/captcha 提交），短期复用。
func (m *Manager) SetManualParam(param, region string) error {
	param = strings.TrimSpace(param)
	if param == "" {
		return errors.New("verify_param 不能为空")
	}
	if region == "" {
		region = m.FetchConfig(context.Background()).Region
	}
	if region == "" {
		region = DefaultConfig.Region
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cached = &Token{VerifyParam: param, Region: region}
	m.cachedAt = m.now()
	m.cachedTTL = time.Duration(config.CaptchaManualCacheTTL) * time.Millisecond
	return nil
}

// Invalidate 清空缓存的令牌（上游拒绝验证码时调用）。
func (m *Manager) Invalidate() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cached = nil
	m.cachedAt = time.Time{}
	m.cachedTTL = 0
}

// Close 停止 token 池（refill 与在途 mint），再释放求解器资源（浏览器池）。
// 两者的 Close 都可能阻塞（池关闭需等待 worker 收尾），故在锁外调用，
// 避免阻塞并发的 GetVerifyParam。
func (m *Manager) Close() error {
	m.mu.Lock()
	pool := m.pool
	m.pool = nil
	s := m.solver
	m.mu.Unlock()
	if pool != nil {
		_ = pool.Close()
	}
	if s != nil {
		return s.Close()
	}
	return nil
}
