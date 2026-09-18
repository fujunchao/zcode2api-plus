// 请求整形与上游错误分类。对应 Python 版 routes/gateway.py 的
// 常量、_normalize_body、_is_captcha_error、_upstream_business_code、
// _is_model_concurrency_limit 与模型白名单部分。
package gateway

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/store"
)

const (
	// MaxCaptchaRetries 单账号内验证码失效重试上限（含首次尝试的总次数）。
	MaxCaptchaRetries = 3
	// MaxAccountAttempts 单次请求最多尝试的账号数。
	MaxAccountAttempts = 5
	// MaxRateLimitRetries 瞬时限流（非额度码族的 429）在同一账号上的原地重试次数。
	// 池子里通常还有别的账号，换号成本是百毫秒级；原地等待只用来对冲「限流窗口
	// 秒级复位」这一种可能，因此只给一次机会，不跟随 3010 的多轮等待。
	MaxRateLimitRetries = 1
	// RateLimitRetryDelay 瞬时限流原地重试的基准等待时长（实际施加 ±20% 抖动）。
	RateLimitRetryDelay = time.Second
)

// defaultBusyRetryDelays 原版客户端对 Start Plan 的 3010 等待 1s、2s 重试；
// 账号仍然可用，不能把模型准入限制写成账号 cooling。
var defaultBusyRetryDelays = []time.Duration{time.Second, 2 * time.Second}

// rateLimitCoolingSteps 瞬时限流的递进冷却阶梯（秒）：同一账号连续被限流则逐级
// 加重，任意一次成功调用后归零。第 4 次起落到 config.CoolingSeconds（默认 300s），
// 与连接失败/503 同值——阶梯最重也只与硬故障惩罚持平，不会更重。
var rateLimitCoolingSteps = []int{30, 60, 120}

// transientCoolingSeconds 取「连续第 streak 次被瞬时限流」对应的冷却时长（秒）。
func transientCoolingSeconds(streak int) int {
	if streak < 1 {
		streak = 1
	}
	if streak > len(rateLimitCoolingSteps) {
		return config.CoolingSeconds
	}
	if secs := rateLimitCoolingSteps[streak-1]; secs < config.CoolingSeconds {
		return secs
	}
	return config.CoolingSeconds
}

// MarkRateLimited 记录一次瞬时限流（非额度码族的 429）并按连续次数递进冷却。
// 返回本次实际写入的冷却秒数与累加后的连续次数，供日志展示。
// 与 MarkAccount(cooling) 的区别：那条用于连接失败/503 等硬故障，时长固定为
// config.CoolingSeconds；瞬时 429 的窗口可能只有数秒，用固定 300s 惩罚过重。
//
// 按 ID 定位并在 store 锁内改「当前」对象：调用方持有的是账号副本（Select 返回
// 深拷贝），直接改它既不会落库、也会与并发读写相撞。连续次数的自增也必须在锁内，
// 否则两路并发限流会各自基于旧值算出同一个档位。
func MarkRateLimited(st *store.Store, provider, idOrName, errMsg string, now time.Time) (secs, streak int) {
	_, _ = st.Update(provider, idOrName, func(acc *model.Account) {
		acc.RateLimitStreak++
		streak = acc.RateLimitStreak
		secs = transientCoolingSeconds(streak)
		until := float64(now.Add(time.Duration(secs)*time.Second).UnixNano()) / 1e9
		acc.Status = model.StatusCooling
		acc.CoolingUntil = &until
		acc.LastError = &errMsg
	})
	if streak == 0 {
		// 账号已被删除（并发删除）：不落库，但仍给日志一个可用的秒数。
		secs = transientCoolingSeconds(1)
	}
	return secs, streak
}

// ResetRateLimitStreak 成功调用后清零「连续被限流」计数。
// sync 与 async 两条请求路径都必须调用，否则同一账号会出现「一边归零、一边继续
// 递增」的分歧——这正是两条路径必须共用状态机的老问题。
//
// ⚠️ 它对入参**不做落库**，只改字段，因此必须传 store.Update 回调里的 live 对象
// （或 store 持锁路径内的对象）。传账号副本只是白改一次，不会生效。
func ResetRateLimitStreak(acc *model.Account) {
	acc.RateLimitStreak = 0
}

// JitteredDelay 给重试等待施加 ±20% 抖动：多个请求在同一瞬间被限流时，
// 避免它们在同一瞬间一起重试，也避免一批账号在同一瞬间集体复活。
func JitteredDelay(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
}

// ModelNameMap Z.AI 上游模型名大小写敏感；仅映射开放清单内的模型，
// 其余写法本就会被白名单拒绝，无需维护映射。
var ModelNameMap = map[string]string{
	"glm-5.3": "GLM-5.3",
}

// AvailableModels 对外公布的可用模型（/v1/models 与请求白名单）。
var AvailableModels = []string{"glm-5.3-flash", "GLM-5.3"}

var allowedModels = func() map[string]bool {
	set := map[string]bool{}
	for _, name := range AvailableModels {
		set[model.NormalizeModelName(name)] = true
	}
	return set
}()

// ModelAllowed 请求模型必须在对外清单内才允许转发上游。
func ModelAllowed(m any) bool {
	return allowedModels[model.NormalizeModelName(m)]
}

// quotaExhaustedCodes Z.AI 官方 429 业务码中的额度/用量上限族（docs.z.ai 错误码表）：
// 1113 欠费；1308 用量上限；1309 套餐过期；1310 周/月上限；1311 套餐不含此模型；
// 1313 公平使用；1316-1321 5小时/7天上限及子账户/企业消费上限。
// 与瞬时限流（1302/1305）区分：上限族按「模型耗尽」换号，限流按冷却换号。
var quotaExhaustedCodes = map[string]bool{
	"1113": true, "1308": true, "1309": true, "1310": true, "1311": true, "1313": true,
	"1316": true, "1317": true, "1318": true, "1319": true, "1320": true, "1321": true,
}

// IsQuotaExhaustedCode 判断上游业务码是否属于额度/用量上限族。
// 429 响应须用本函数区分「该模型耗尽」（换号）与「瞬时限流」（冷却换号）；
// asyncpool 与 engine 共用同一判定，避免两条路径对同一账号标出不同状态。
func IsQuotaExhaustedCode(text string) bool {
	return quotaExhaustedCodes[UpstreamBusinessCode(text)]
}

// captchaHeaders 上游用于携带验证码挑战的响应头。
var captchaHeaders = []string{
	"x-aliyun-captcha-verify-param",
	"x-aliyun-captcha-verify-region",
	"x-aliyun-captcha-challenge",
}

// IsCaptchaError 识别上游验证码挑战：响应头、body code=3007、F001 与旧文本
// （仅对 400/403 生效，避免把 5xx 的偶发字样误判；对齐 _is_captcha_error）。
func IsCaptchaError(text string, statusCode int, header http.Header) bool {
	for _, name := range captchaHeaders {
		if header.Get(name) != "" {
			return true
		}
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err == nil {
		candidates := []any{payload["code"]}
		if errObj, ok := payload["error"].(map[string]any); ok {
			candidates = append(candidates, errObj["code"])
		}
		for _, c := range candidates {
			if fmt.Sprint(c) == "3007" {
				return true
			}
		}
	}
	low := strings.ToLower(text)
	clientError := statusCode == 0 || statusCode == 400 || statusCode == 403
	if strings.Contains(low, "f001") {
		return clientError
	}
	if strings.Contains(low, "captcha") || strings.Contains(low, "verify token") || strings.Contains(low, "verify failed") {
		return clientError
	}
	return false
}

// UpstreamBusinessCode 提取上游错误体中的业务码。
// 兼容两种形态：zcode-plan 包装格式 {"code":..,"msg":..} 与
// api.z.ai 的 Anthropic 风格 {"error":{"message":..,"type":"1000"}}。
// 无业务码时返回空串。
func UpstreamBusinessCode(text string) string {
	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		return ""
	}
	code := payload["code"]
	if code == nil {
		if errObj, ok := payload["error"].(map[string]any); ok {
			code = errObj["type"]
		}
	}
	if code == nil {
		return ""
	}
	return fmt.Sprint(code)
}

// IsModelConcurrencyLimit 3010 是模型并发准入限制，不代表账号被限流或失效。
func IsModelConcurrencyLimit(statusCode int, text string) bool {
	if statusCode != 429 {
		return false
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err == nil && fmt.Sprint(payload["code"]) == "3010" {
		return true
	}
	return strings.Contains(strings.ToLower(text), "model admission concurrency limit")
}
