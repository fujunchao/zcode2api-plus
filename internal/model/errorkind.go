// 账号「最近一次错误」的归类枚举。
//
// 一次请求失败的原因需要能被稳定检索与筛选，而 LastError 的文案是给人看的、
// 会随分支调整而变（同步与异步两条路径的历史文案也并不完全一致）。因此引入一组
// 稳定字符串：它落到 accounts.data 的 JSON 里、出现在后台 PublicView 中，并被
// 前端当作筛选值。**字符串值一经上线不要再改**——改名等于让既有数据失去分类。
//
// 与账号状态（active/cooling/exhausted/invalid/disabled）的分工：状态回答
// 「这个号现在能不能用」，ErrorKind 回答「上一次为什么失败」。二者刻意解耦——
// 平台过载、额度耗尽、并发准入都会让请求失败，却都不该改变账号状态。
package model

// 错误类型取值。
const (
	// ErrorKindUpstreamOverload 平台服务过载（官方业务码 1305；上游在 Anthropic
	// 兼容面以 HTTP 529 返回）。官方明确说明它与单一账户的调用行为无关，因此不标
	// 账号状态、不计入 fail_count——换号与冷却都救不了，只会把健康账号踢出池子。
	ErrorKindUpstreamOverload = "upstream_overload"
	// ErrorKindRateLimited 账户维度的瞬时限流（HTTP 429 且非额度码族，如 1302）。
	ErrorKindRateLimited = "rate_limited"
	// ErrorKindQuotaExhausted 额度/用量上限（402、429 上限码族、HTTP 200 + 业务码 1005）。
	ErrorKindQuotaExhausted = "quota_exhausted"
	// ErrorKindModelBusy 模型并发准入受限（429 + 业务码 3010）。账号仍然可用。
	ErrorKindModelBusy = "model_busy"
	// ErrorKindAuthFailed 凭据失效（401/403，或凭证无法构造出上游请求）。
	ErrorKindAuthFailed = "auth_failed"
	// ErrorKindConnectionFailed 连不上上游：拨号/代理失败、等响应头超时、错误体读不出来。
	ErrorKindConnectionFailed = "connection_failed"
	// ErrorKindUpstreamUnavailable 上游服务不可用（HTTP 503）。
	ErrorKindUpstreamUnavailable = "upstream_unavailable"
	// ErrorKindCaptchaFailed 验证码挑战无法通过（自动求解失败，或上游连续拒绝）。
	ErrorKindCaptchaFailed = "captcha_failed"
	// ErrorKindInvalidResponse 上游响应形态不合法（承诺 SSE 却回 JSON、业务错误体读不成 JSON）。
	ErrorKindInvalidResponse = "invalid_response"
	// ErrorKindClientCanceled 客户端主动取消。与账号健康无关：它是「用户不再需要」，
	// 不是「账号有问题」，因此既不覆盖 last_error 文案，也不参与「账号故障」筛选。
	ErrorKindClientCanceled = "client_canceled"
	// ErrorKindUpstreamError 兜底：已知分类都不匹配的上游错误，原样透传给客户端。
	ErrorKindUpstreamError = "upstream_error"
	// ErrorKindQuotaQueryFailed 额度轮询失败（非鉴权类）。它是旁路观测的失败，
	// 不影响该账号能否转发请求，因此不算账号故障。
	ErrorKindQuotaQueryFailed = "quota_query_failed"
	// ErrorKindRiskControl 上游风控拦截（HTTP 405 + 风控文案，见 IsRiskControlBody）。
	// 风控看的是身份维度——JWT 账号、X-Device-Mid 设备指纹、出口 IP、请求头与 UA——
	// 模型只是 body 里的一个字段，所以按模型冷却毫无意义：换个模型照样被拦。
	// 因此它触发整号冷却，并计入「账号故障」（换个账号/换条线路确实可能成功）。
	ErrorKindRiskControl = "risk_control"
)

// ErrorKindAll 全部错误类型，供遍历与一致性测试使用。
// 顺序与上面的常量声明一致，便于对照阅读；不要依赖它做展示排序。
var ErrorKindAll = []string{
	ErrorKindUpstreamOverload,
	ErrorKindRateLimited,
	ErrorKindQuotaExhausted,
	ErrorKindModelBusy,
	ErrorKindAuthFailed,
	ErrorKindConnectionFailed,
	ErrorKindUpstreamUnavailable,
	ErrorKindCaptchaFailed,
	ErrorKindInvalidResponse,
	ErrorKindClientCanceled,
	ErrorKindUpstreamError,
	ErrorKindQuotaQueryFailed,
	ErrorKindRiskControl,
}

// accountFaultKinds 归因于账号自身、因而值得按「账号故障」聚合筛选的错误类型。
//
// 判据是「换个账号是否可能成功」：
//   - 凭据异常、连不上、上游 503、响应形态不合法、未知上游错误 → 换号可能恢复，算故障；
//   - 平台过载、额度耗尽、并发准入、验证码 → 换号无用或与账号健康无关，不算；
//   - 客户端取消 → 用户自己断的，不算；
//   - 额度轮询失败 → 旁路观测的失败，该账号仍在正常转发请求，不算。
//
// ⚠️ 本表只影响前端的筛选归类，不参与任何状态机与计数决策：fail_count 由各分支
// 自己决定是否自增，与这里无关。
var accountFaultKinds = map[string]bool{
	ErrorKindRateLimited:         true,
	ErrorKindAuthFailed:          true,
	ErrorKindConnectionFailed:    true,
	ErrorKindUpstreamUnavailable: true,
	ErrorKindInvalidResponse:     true,
	ErrorKindUpstreamError:       true,
	ErrorKindRiskControl:         true,
}

// ErrorKindAccountFault 判断某类错误是否归因于账号自身。
// 未知取值（旧数据，或将来新增但未登记的取值）一律返回 false：宁可漏报也不误报，
// 避免把认不出来的类型混进「账号故障」视图。
func ErrorKindAccountFault(kind string) bool {
	return accountFaultKinds[kind]
}

// ErrorKindValid 判断取值是否为已登记的枚举值。
// 用于测试与旧数据兜底：字段可空，读到未登记的取值不应被当成有效分类。
func ErrorKindValid(kind string) bool {
	for _, k := range ErrorKindAll {
		if k == kind {
			return true
		}
	}
	return false
}
