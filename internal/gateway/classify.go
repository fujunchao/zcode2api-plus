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
	"zcode2api/internal/web"
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

// statusOverloaded 上游表达「平台服务过载」的状态码。它不在 net/http 的常量表里：
// Anthropic 兼容面沿用 Anthropic 的 529（overloaded），而官方错误码表把同一业务码
// 1305 标为 429——只认 429 会整类漏掉平台过载（线上实测：529 全部落到「其余错误」
// 分支被原样透传，网关一次都不重试）。
const statusOverloaded = 529

// OverloadRetryDelays 平台过载的原地退避档位（1s、3s，均施加 ±20% 抖动）。
// 官方文档对 1305 的处置建议是「增加重试间隔，避免立即高频重试」——过载是平台侧
// 的拥塞，退避只用来对冲瞬时峰值，给两次机会即可，再多只是替上游挡枪。
//
// 导出是为了让 asyncpool 复用同一份默认值：同一次平台过载若在 sync 侧退避、在
// async 侧立刻失败，两条路径的语义就漂移了。
var OverloadRetryDelays = []time.Duration{time.Second, 3 * time.Second}

// rateLimitCoolingSteps 瞬时限流的递进冷却阶梯（秒）：同一账号连续被限流则逐级
// 加重，任意一次成功调用后归零。第 4 次起落到 config.CoolingSeconds（默认 300s），
// 与连接失败同值——阶梯最重也只与硬故障惩罚持平，不会更重。
// （上游 503 有自己独立的 Upstream503CoolingSteps 阶梯，见 MarkUpstreamUnavailable。）
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
		StampAccountError(acc, model.ErrorKindRateLimited, errMsg, now)
	})
	if streak == 0 {
		// 账号已被删除（并发删除）：不落库，但仍给日志一个可用的秒数。
		secs = transientCoolingSeconds(1)
	}
	return secs, streak
}

// ── 最近错误（last_error_kind / last_error_at） ───────────────────────────────
//
// 一次失败要留下两样东西：给人看的文案（LastError）与给筛选用的归类（LastErrorKind）。
// 归类必须由「产生这次失败的那条分支」决定，因此每个写入点都要求显式给出 kind——
// 漏给会编译失败，分类不可能被静默漏掉。
//
// 各 Mark* / bumpFail 只负责自己那一类错误的 kind（如 MarkRateLimited 恒为
// rate_limited、MarkModelExhausted 恒为 quota_exhausted），不把 kind 开放成参数：
// 函数语义已唯一确定 kind，开放成参数反而多给了写错的机会。

// StampAccountError 在 store 锁内写入错误归类与时间；detail 非空时才覆盖 LastError 文案。
//
// 必须是「锁内」形态（只改字段、不落库）：调用方已经在 store.Update 的回调里，
// 从这里再调一次 store.Update 就是自锁。导出是为了让 asyncpool 在它自己的
// bumpFail 里复用同一套写入语义，两条路径对同一次失败必须写出相同的字段。
func StampAccountError(acc *model.Account, kind, detail string, now time.Time) {
	ts := float64(now.UnixNano()) / 1e9
	acc.LastErrorKind = &kind
	acc.LastErrorAt = &ts
	if detail != "" {
		acc.LastError = &detail
	}
}

// RecordAccountError 记一次「最近错误」：归类 + 时间。
//
// detail 传空串的语义是「只归类、不污染错误文本」——客户端取消走这条路：用户按一下
// ESC 不该把上一次真实故障的文案冲掉，但「这个号最近发生过什么」仍值得留痕。
//
// ⚠️ 只在「该失败出口本来没有任何账号写入」时才用它（3010 并发准入、验证码、客户端
// 取消）。若该出口已经会调 MarkAccount / MarkModelExhausted / bumpFail，请把 kind 并进
// 那一次写入，否则同一分支会两次 store.Update、两次落库。两次是顺序调用而非嵌套，
// 不会自锁，但白白多一倍持久化 IO。
func RecordAccountError(st *store.Store, provider, idOrName, kind, detail string, now time.Time) {
	_, _ = st.Update(provider, idOrName, func(acc *model.Account) {
		StampAccountError(acc, kind, detail, now)
	})
}

// ── 上游风控冷却（405 + unusual activity） ───────────────────────────────────

// riskControlCoolingSeconds 取「连续第 streak 次命中风控」对应的冷却秒数。
//
// 连续次数**超过阶梯长度**时返回 (0, true)：由调用方把账号置为 invalid。也就是说
// 阶梯的档位数同时就是升级点——管理员想给账号多一次自证机会就多加一档，想更早
// 封禁就减一档，不需要第二个参数。
func riskControlCoolingSeconds(st *store.Store, streak int) (secs int, invalid bool) {
	if streak < 1 {
		streak = 1
	}
	steps := st.RiskCoolingSteps()
	if streak > len(steps) {
		return 0, true
	}
	return steps[streak-1], false
}

// MarkRiskControl 记录一次上游风控拦截并按连续命中次数递进冷却，超限则置 invalid。
// 返回本次实际写入的冷却秒数、累加后的连续次数、以及是否已升级为失效，供日志展示。
//
// 形状与 MarkRateLimited 一致（锁内自增 → 选档 → 写状态 → StampAccountError），
// 但刻意不复用 MarkAccount：那条的契约是「cooling 即固定 config.CoolingSeconds」，
// 供 503 / 连接失败等硬故障共用；风控需要「递进阶梯 + 超限置 invalid」两种语义，
// 往 MarkAccount 里塞参数会把 503 路径也拖进这套复杂度。
//
// 为什么不按模型冷却：风控看的是身份维度——JWT 账号、X-Device-Mid 设备指纹、出口
// IP、请求头与 UA——模型只是 body 里的一个字段。同一个身份换个模型照样被拦，按模型
// 冷却只会把失败摊到别的模型上、把暴露时间拖长。这条分支必须排在「其余错误」兜底
// 之前，否则 405 会被当成未识别错误原样透传、账号状态一点不变（线上曾如此）。
//
// ⚠️ 本函数只处理**账号级**风控。「同一请求体在 ≥2 个不同账号上都被拒」属请求级，
// 处置是停止换号 + 保留冷却 + 登记重放防护 —— 见 riskscope.go（MarkRiskControl
// 的实现也住在那里：账号级与请求级的冷却动作相同，差别只在换号与扩散防护）。

// ResetRiskControlStreak 成功调用后清零「连续命中风控」计数。
// 与 ResetRateLimitStreak 同语义、同样必须对 store.Update 回调里的 live 对象调用。
//
// 清零点是「冷却到期后的首次成功调用」——风控封禁期间账号根本不会被选中，所以只有
// 冷却窗口过去、真正打通一次，才说明这次拦截是偶发的；否则连续命中会一路升到失效。
func ResetRiskControlStreak(acc *model.Account) {
	acc.RiskControlStreak = 0
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

// RecordStreamTruncate 记一次「上游中途掐断流」的观测计数，返回累加后的总数。
//
// 这是**纯观测**入口：只动 Account.StreamTruncateCount，不写 Status、不写
// CoolingUntil、不写 last_error/last_error_kind——中途截断发生在响应头已经发给
// 客户端之后，无法换号重试，也没有任何证据表明责任在账号；把它当账号故障处理
// 会把上游的模型级时长墙变成对账号的集体惩罚（与 2026-09-20 的 503 事故同类）。
//
// 计数只增不清（见 Account.StreamTruncateCount 的注释），所以这里没有配对的 Reset。
// 必须经 store.Update 对「当前」对象自增：调用方持有的是 Select 交出的副本。
// 返回值必须是锁内自增后的新值——读副本只会拿到旧值，与 markRateLimited 同一个坑。
func RecordStreamTruncate(st *store.Store, provider, idOrName string) (total int) {
	_, _ = st.Update(provider, idOrName, func(acc *model.Account) {
		acc.StreamTruncateCount++
		total = acc.StreamTruncateCount
	})
	return total
}

// RecordUpstreamTruncate 记一次「上游侧中途掐断流」并驱动两级止血，返回账号累计
// 断流数（与 RecordStreamTruncate 同口径，供诊断行 trunc_total 使用）。
//
// 在账号级纯观测计数之外，补上 2026-09-22 事故复盘定论缺的两层（见
// docs/analysis-flash-5min-stream-cut-20260921.md 09-22 附录）：
//
//  1. 账号短回避：写 TruncateAvoidUntil（仅选号层软过滤，不是冷却、不标状态），
//     让客户端 ~2s 后的 TRANSPORT 重试自然落到别的账号/线路；
//  2. 线路级熔断：账号绑定命名线路时按线路累计「连续」断流，达到在线阈值
//     （默认 3，0=关闭）即移除该线路并改派绑定账号——复用 PurgeProxyProfiles
//     原子路径，与 proxy-health 同一套。责任在线路而非账号：多账号可共享一条
//     线路，按线路聚合计数才命得中真凶，且不把上游的锅变成对账号的惩罚。
//
// sync 与 async 两条请求路径都必须经由本入口，否则会出现「一边熔断、一边只计数」
// 的分歧。裸 ProxyURL / 直连账号无处熔断，只做账号级计数与回避。
func RecordUpstreamTruncate(st *store.Store, acc *model.Account, now time.Time) int {
	total := RecordStreamTruncate(st, acc.Provider, acc.ID)
	if avoid := st.LineTruncateAvoidSeconds(); avoid > 0 {
		until := float64(now.Add(time.Duration(avoid)*time.Second).UnixNano()) / 1e9
		_, _ = st.Update(acc.Provider, acc.ID, func(live *model.Account) {
			live.TruncateAvoidUntil = until
		})
	}
	if acc.ProxyID == nil || *acc.ProxyID == "" {
		return total
	}
	lineID := *acc.ProxyID
	// 先取标签再熔断：移除后 ProxyLabel 只能给出 proxy-id:<id>，日志里就丢了线路名。
	label := st.ProxyLabel(acc)
	streak := st.BumpLineTruncate(lineID)
	if threshold := st.LineTruncateStrikes(); threshold > 0 && streak >= threshold {
		if purged, reassign, err := st.PurgeProxyProfiles([]string{lineID}); err != nil {
			web.Warn("line-guard", fmt.Sprintf("线路 %s 连续 %d 次上游断流，但移除失败: %v", label, streak, err))
		} else if len(purged) > 0 {
			web.Warn("line-guard", fmt.Sprintf("线路 %s 连续 %d 次上游断流，已移除；改派 %d 个账号、%d 个退直连",
				label, streak, len(reassign.Assigned), len(reassign.Direct)))
		}
	}
	return total
}

// ResetLineTruncate 在完整成功交付后清零账号所绑线路的「连续断流」计数。
//
// ⚠️ 必须在「交付完成」处调用（engine.finishDelivery 成功分支 / async forwardSSE
// 成功路径），不能塞进 MarkSuccess：engine 的 success 在流开始读取之前调用，
// 「每次正常开头、~300s 处被切」的链会刚复位又被计入，永远凑不满熔断连击。
// acc 的 ProxyID 是选号时的快照；若线路在流期间被改派，清的是旧线路的计数——
// 该线路已无此账号，多清一次只会让它晚一档熔断，无害。
func ResetLineTruncate(st *store.Store, acc *model.Account) {
	if acc == nil || acc.ProxyID == nil || *acc.ProxyID == "" {
		return
	}
	st.ResetLineTruncate(*acc.ProxyID)
}

// ── 上游 503 冷却阶梯 ─────────────────────────────────────────────────────────
//
// 背景：503 一律固定冷却 config.CoolingSeconds（默认 300s）的年代，一次上游抖动
// 会把个位数账号池在几十秒内整池冷却清空，客户端收到成片的 no_available_account
// （2026-09-20 线上事故，docs/analysis-503-no-available-account-20260920.md）。
// 503 大多是上游侧瞬时不可用，秒级到分钟级的递进冷却足够对冲；只有持续失败的
// 账号才逐步升到 CoolingSeconds 封顶。

// upstream503CoolingSeconds 取「连续第 streak 次收到上游 503」对应的冷却秒数。
// 超过阶梯长度封顶 config.CoolingSeconds——与连接失败等硬故障持平，不再更重。
func upstream503CoolingSeconds(st *store.Store, streak int) int {
	if streak < 1 {
		streak = 1
	}
	steps := st.Upstream503CoolingSteps()
	if streak > len(steps) {
		return config.CoolingSeconds
	}
	secs := steps[streak-1]
	if secs > config.CoolingSeconds {
		return config.CoolingSeconds
	}
	return secs
}

// MarkUpstreamUnavailable 记录一次上游 503 并按连续次数递进冷却。
// 返回本次实际写入的冷却秒数与累加后的连续次数，供日志展示。
//
// 与 MarkRiskControl 的两点刻意差异：
//   - 超阶梯长度**不升 invalid**：503 是上游健康信号而非账号问题，换号救不了
//     「上游真的挂了」，封顶冷却即可，不该判账号死刑；
//   - 换凭据（EditAccount）不清零：它衡量的是「这个账号的上游链路最近有多不
//     健康」，与凭据无关（风控清零是因为换凭据=换身份，503 没有这层语义）。
//
// 形状与 MarkRateLimited 一致（锁内自增 → 选档 → 写状态 → StampAccountError），
// sync（engine）与 async（asyncpool）两条路径共用本函数，行为必须保持一致。
//
// FailCount 的递增折在同一次 Update 里：旧实现在调用点是「先 bumpFail 再 mark」
// 两次落库，语义完全等价（503 恒 ErrorKindUpstreamUnavailable、FailCount 恒 +1），
// 合并后少一半持久化 IO，也不会再出现两路只调其一的漂移。
func MarkUpstreamUnavailable(st *store.Store, provider, idOrName, errMsg string, now time.Time) (secs, streak int) {
	_, _ = st.Update(provider, idOrName, func(acc *model.Account) {
		acc.FailCount++
		acc.Upstream503Streak++
		streak = acc.Upstream503Streak
		secs = upstream503CoolingSeconds(st, streak)
		until := float64(now.Add(time.Duration(secs)*time.Second).UnixNano()) / 1e9
		acc.Status = model.StatusCooling
		acc.CoolingUntil = &until
		StampAccountError(acc, model.ErrorKindUpstreamUnavailable, errMsg, now)
	})
	if streak == 0 {
		// 账号已被删除（并发删除）：不落库，但仍给日志一个可用的秒数。
		secs = upstream503CoolingSeconds(st, 1)
	}
	return secs, streak
}

// ResetUpstream503Streak 成功调用后清零「连续收到上游 503」计数。
// 与 ResetRateLimitStreak 同语义：必须传 store.Update 回调里的 live 对象；
// sync（engine.success）与 async（asyncpool.forwardSSE）两条路径都要调用。
func ResetUpstream503Streak(acc *model.Account) {
	acc.Upstream503Streak = 0
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
// 与瞬时限流（1302）区分：上限族按「模型耗尽」换号，限流按冷却换号。
//
// ⚠️ 1305 刻意不在这里：官方定义它是「平台服务过载」，与单一账户的调用行为无关
// （1302 才是账户维度的速率限制）。它由 IsUpstreamOverload 单列处理——既不标账号
// 状态、也不冷却，否则一次平台过载会把整池健康账号逐个踢出调度。
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

// IsUpstreamOverload 识别平台级服务过载：官方业务码 1305 的文案是「该模型当前访问量
// 过大」，官方文档明确它属于「平台服务过载，与单一账户的调用行为无直接关系」。
//
// 判据必须同时看状态码与业务码：上游在 Anthropic 兼容面用 HTTP 529 承载它，而官方
// 错误码表把它标成 429——只认 429 会漏掉整类过载，只认 529 则会漏掉上游改用 429 的
// 情形。若上游将来换成 Anthropic 原生的 {"type":"error","error":{"type":"overloaded_error"}}
// 形态（无 code 字段），状态码一侧仍能兜住。
//
// 与 1302 的区别是这条判定的存在意义：1302 是账户维度的速率限制，冷却该账号语义正确；
// 1305 是平台过载，冷却账号既不解决问题、又白白缩短可用池。
func IsUpstreamOverload(statusCode int, text string) bool {
	if statusCode == statusOverloaded {
		return true
	}
	return UpstreamBusinessCode(text) == "1305"
}

// ErrorPreview 生成上游错误体的日志预览：优先取业务码与可读文案，单行化后截断到
// 200 字符；形态认不出来就退回原文截断。
//
// 存在的理由：上游错误日志此前只记状态码（`上游错误 HTTP %d`），于是「状态码看不出
// 问题、业务码才是关键」的失败在日志里完全不可见——1305 平台过载与风控 405 都是这类，
// 线上排查只能依赖用户贴出客户端报错（实测一整份 5362 行日志里 1305 出现 0 次）。
//
// 必须截断：错误体可能回显用户内容，也可能是一整页 HTML。
func ErrorPreview(text string) string {
	preview := text
	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err == nil {
		parts := []string{}
		if code := payload["code"]; code != nil {
			parts = append(parts, fmt.Sprintf("code=%v", code))
		}
		for _, key := range []string{"msg", "message"} {
			if s, ok := payload[key].(string); ok && s != "" {
				parts = append(parts, s)
				break
			}
		}
		// Anthropic 风格：{"error":{"type":"..","message":".."}}
		if len(parts) == 0 {
			if errObj, ok := payload["error"].(map[string]any); ok {
				if t, ok := errObj["type"]; ok && fmt.Sprint(t) != "" {
					parts = append(parts, fmt.Sprintf("type=%v", t))
				}
				if s, ok := errObj["message"].(string); ok && s != "" {
					parts = append(parts, s)
				}
			}
		}
		if len(parts) > 0 {
			preview = strings.Join(parts, " ")
		}
	}
	preview = strings.TrimSpace(strings.Join(strings.Fields(preview), " "))
	// 按 rune 而非字节截断：上游文案多为中文/多字节字符，按字节切会把最后一个字符
	// 截成半个 UTF-8 序列，日志里就是乱码。
	if r := []rune(preview); len(r) > errorPreviewLimit {
		return string(r[:errorPreviewLimit]) + "…"
	}
	return preview
}

// errorPreviewLimit 日志预览的字符（rune）上限。够放下业务码 + 一句上游文案即可；
// 上游文案偶有整段 JSON/HTML，不设限会把日志撑爆。
const errorPreviewLimit = 200

// ErrorDetail 把错误体预览拼成日志后缀。
//
// 上游错误日志此前只记状态码与业务码，于是「状态码看不出问题、业务码才是关键」的
// 分支（401/403、402、3010、529/1305、1005、3007）在日志里没有 body，排查只能靠用户
// 贴客户端报错。这里统一提供后缀；**没有可读内容时返回空串**，避免在既有文案后面留下
// 一个孤零零的冒号（JWT 上游的裸 401 就是空 body）。
//
// 导出给 asyncpool 复用：同一次上游失败在 sync/async 两条路径下必须给出相同的日志口径。
func ErrorDetail(text string) string {
	preview := ErrorPreview(text)
	if preview == "" {
		return ""
	}
	return ": " + preview
}
