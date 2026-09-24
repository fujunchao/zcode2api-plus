// Package upstream 构造对 ZCode 上游的请求。
// 对应 Python 版 app/agent.py：按账号凭证选端点、组装请求头。
// 实际发送与流式透传在 internal/gateway。
package upstream

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/textproto"
	"strings"
	"sync"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

//go:embed zcode_system.json
var zcodeSystemJSON []byte

var (
	zcodeOnce         sync.Once
	zcodeStaticBlocks []any
)

// ZcodeSystemBlocks 返回 ZCode 官方系统提示词块（JSON 解析失败时为空，对齐 Python 版）。
// JWT 账号请求必须注入到顶层 system，否则上游返回 405。
//
// 只解析一次静态段（CLI Prefix / Agent Identity，内容是固定的），
// 但**每次都重新生成** # Environment 段：它必须跟随当前伪装档案
// （见 systemprompt.go 的说明），缓存会让改配置后 body 不更新。
func ZcodeSystemBlocks() []any {
	zcodeOnce.Do(func() {
		var blocks []any
		if err := json.Unmarshal(zcodeSystemJSON, &blocks); err != nil {
			blocks = []any{}
		}
		zcodeStaticBlocks = blocks
	})
	return appendEnvironmentBlock(zcodeStaticBlocks)
}

// dropHeaders 透传客户端 header 时需要剔除的字段（对齐 agent.py _DROP_HEADERS）。
var dropHeaders = map[string]bool{
	"host":                           true,
	"content-length":                 true,
	"x-api-key":                      true,
	"authorization":                  true,
	"user-agent":                     true,
	"http-referer":                   true,
	"accept-encoding":                true,
	"connection":                     true,
	"cookie":                         true,
	"x-aliyun-captcha-verify-param":  true,
	"x-aliyun-captcha-verify-region": true,

	// 设备指纹头：模型请求**不发**这个头（官方实测如此，设备身份在 body 的
	// metadata.user_id 里，见 metadata.go）。这里必须一并拦掉下游送来的同名头——
	// 我们不再写固定值，放行等于让下游指定本账号的设备指纹，账号隔离形同虚设。
	"x-device-mid": true,

	// 客户端签名头（ClientRequestSigningV4）。签名与 apiKey / session 绑定，
	// 下游客户端的签名对**本账号**无效，透传只会换来 401 VERIFY_SIGNATURE_INVALID；
	// 本网关不实现签名（见 docs/plan-client-format-parity.md §1.5），故一律剔除。
	"x-client-sig":           true,
	"x-client-ts":            true,
	"x-client-version":       true,
	"x-client-nonce":         true,
	"x-client-pow":           true,
	"x-client-sign-verified": true,
	"x-app-id":               true,
}

// Attribution 一次请求内共享的归因标识，对应官方的
// createModelRequestAttributionHeaders（每个模型请求必带）：
//
//	x-request-id          每请求 UUID
//	x-session-id          会话 id（官方会去掉 sess_ / subagent_agent_ 前缀）
//	x-zcode-trace-id      trace id
//	x-zcode-session-type  main | subagent | other
//	x-query-id            轮次 id（官方会去掉 query_ 前缀）；**总是发送**
//
// ⚠️ 必须**按请求计算一次**，在请求头与 body 之间共享：官方请求里
// metadata.user_id.session_id 与 X-Session-Id 头**恒等**（golden 实测），两处各算
// 一次必然分叉成两个 UUID，而「头里一个会话、body 里另一个会话」是官方从不出现的
// 自相矛盾形态。调用方见 upstream.InjectDeviceMetadata / gateway.RunMessages。
type Attribution struct {
	RequestID   string // x-request-id
	SessionID   string // x-session-id；body 的 metadata.user_id.session_id 同值
	TraceID     string // x-zcode-trace-id
	SessionType string // x-zcode-session-type：main | subagent | other
	QueryID     string // x-query-id（轮次 id）
}

// NewAttribution 计算一次请求的归因标识。
//
// bodySessionID 是下游在 body 的 metadata.user_id 里声明的会话 id（可为空），
// 只作 X-Session-Id 头缺失时的兜底来源——否则会出现上面说的头体不一致。
//
// 取值优先沿用下游送来的同名头——同一会话的 trace/session 才连续；缺失才合成。
// 目的是**保证这几项永远存在**：官方请求里不会缺它们。
//
// ⚠️ 本函数直接读 incoming，**不依赖透传过滤的结果**。官方客户端必带的
// x-zcode-session-type / x-zcode-trace-id 正是靠这里取回并作为固定头重新写出的；
// 透传循环里对 x-zcode-* 的一刀切丢弃因此不会伤到它们（写测试时别把两件事混起来：
// 改那处过滤不会让本函数的行为变化）。
func NewAttribution(incoming map[string]string, bodySessionID string) Attribution {
	pick := func(name string) string {
		if v := strings.TrimSpace(incoming[name]); v != "" {
			return v
		}
		// 透传头的键形态不受控（可能全小写），再按大小写不敏感找一遍。
		for k, v := range incoming {
			if strings.EqualFold(k, name) {
				if trimmed := strings.TrimSpace(v); trimmed != "" {
					return trimmed
				}
			}
		}
		return ""
	}

	requestID := config.NewUUIDv4()
	sessionID := stripInternalPrefixes(pick("X-Session-Id"), "sess_", "subagent_agent_")
	if sessionID == "" {
		// 头里没有就用下游 body 里声明的会话；再没有才回落到本次请求 id。
		sessionID = strings.TrimSpace(bodySessionID)
	}
	if sessionID == "" {
		sessionID = requestID
	}
	traceID := pick("X-Zcode-Trace-Id")
	if traceID == "" {
		traceID = config.NewUUIDv4()
	}
	sessionType := pick("X-Zcode-Session-Type")
	if sessionType == "" {
		sessionType = "main"
	}
	// x-query-id：**总是**发送。官方每个"轮次"都会生成一个（内部带 query_ 前缀，
	// 输出时剥掉），实测抓包确认它恒在（docs/analysis-client-golden-diff-20260924.md）；
	// 早先只在客户端送来时才发，形态上就是"缺一个官方必带的头"。
	queryID := stripInternalPrefixes(pick("X-Query-Id"), "query_")
	if queryID == "" {
		queryID = config.NewUUIDv4()
	}
	return Attribution{
		RequestID:   requestID,
		SessionID:   sessionID,
		TraceID:     traceID,
		SessionType: sessionType,
		QueryID:     queryID,
	}
}

// Headers 归因标识的出站形态。
func (a Attribution) Headers() map[string]string {
	return map[string]string{
		"X-Request-Id":         a.RequestID,
		"X-Session-Id":         a.SessionID,
		"X-Zcode-Trace-Id":     a.TraceID,
		"X-Zcode-Session-Type": a.SessionType,
		"X-Query-Id":           a.QueryID,
	}
}

// stripInternalPrefixes 去掉官方客户端在输出归因头时会剥掉的内部前缀。
//
// 官方对 x-session-id 剥 sess_ / subagent_agent_（normalizeModelSessionIdForAttribution）、
// 对 x-query-id 剥 query_（modelQueryHeaderValue），所以官方请求里这几项**从来不含**
// 前缀。下游若送来带前缀的形态，原样转发就与官方形态不同。
//
// 语义刻意与官方一致：逐个前缀累进剥离；若剥离后为空则退回原值（官方 `n || e`）。
func stripInternalPrefixes(v string, prefixes ...string) string {
	out := v
	for _, p := range prefixes {
		if strings.HasPrefix(out, p) && len(out) > len(p) {
			out = out[len(p):]
		}
	}
	if out == "" {
		return v
	}
	return out
}

// Request 构造结果：目标 URL 与请求头。
type Request struct {
	URL     string
	Headers map[string]string
}

// BuildRequest 对应 Python 版 build_request：
// JWT 账号走 zcode.z.ai 主端点（Bearer），API Key 账号走 api.z.ai 回退端点（x-api-key）。
// verifyParam/verifyRegion 为验证码令牌（仅 JWT 账号）；incomingHeaders 为客户端透传头。
//
// 归因标识由本函数内部现算（不接收 body 的会话），只适合不关心 body 的调用方与测试；
// 网关请用 BuildRequestWithAttribution，把**按请求算一次**的标识同时喂给头与 body。
func BuildRequest(acc *model.Account, verifyParam, verifyRegion string, incomingHeaders map[string]string) (Request, error) {
	return BuildRequestWithAttribution(acc, verifyParam, verifyRegion, incomingHeaders,
		NewAttribution(incomingHeaders, ""))
}

// BuildRequestWithAttribution 用调用方算好的归因标识构造请求（其余同 BuildRequest）。
//
// 头合并的优先级是固定的：**网关固定头恒胜**。客户端透传头先落，固定头最后覆写，
// 因此客户端不能改掉鉴权、网关标识，也不能把设备指纹塞进请求头（x-device-mid 直接
// 被丢弃，设备身份只走 body 的 metadata.user_id）。
//
// 出站头分三组（对齐官方 3.14.3，见 docs/plan-client-format-parity.md §1.2/§1.4）：
//
//  1. 来源头：User-Agent / X-Title / X-Release-Channel / X-Client-Language /
//     X-Client-Timezone / X-Platform / X-Os-Category / X-Os-Version /
//     X-ZCode-App-Version / X-ZCode-Agent / HTTP-Referer —— 全部由伪装档案派生；
//  2. 归因头：x-request-id / x-zcode-session-type / x-zcode-trace-id /
//     x-session-id / x-query-id —— 优先沿用下游的值，缺失则合成；
//  3. 鉴权与传输头。
func BuildRequestWithAttribution(acc *model.Account, verifyParam, verifyRegion string, incomingHeaders map[string]string, attr Attribution) (Request, error) {
	var targetURL, authHeader, authValue string
	if acc.Provider == model.ProviderZai {
		if acc.Mode == "jwt" && acc.JWTToken != nil {
			targetURL = config.UpstreamZai
			authHeader, authValue = "Authorization", "Bearer "+*acc.JWTToken
		} else if acc.APIKey != nil {
			targetURL = config.UpstreamZaiFallback
			authHeader, authValue = "X-Api-Key", *acc.APIKey
		} else {
			return Request{}, errors.New("账号缺少有效凭证")
		}
	} else {
		return Request{}, fmt.Errorf("未知提供商: %s", acc.Provider)
	}

	// 伪装档案：平台 / 语言 / 时区 / 标题全部由它派生，与 body 里 # Environment 段
	// **同源**（见 config/profile.go）。这不是风格问题：官方客户端这两处本来就
	// 派生自同一个三元组，任何一处各写一份都会长出头体互相矛盾。
	prof := config.ZcodeClientProfile()

	// 网关固定头：客户端透传头一律不得改写这些取值。
	fixed := map[string]string{
		"Content-Type":      "application/json",
		authHeader:          authValue,
		"Anthropic-Version": "2023-06-01",
		// 模型请求的 UA 是 ai-sdk 拼的，带 provider-utils 与 runtime 后缀；
		// 裸 `ZCode/<版本>` 是非模型接口（配置/额度/领取）用的，两者不可混用。
		"User-Agent":          prof.ModelUserAgent(),
		"X-ZCode-App-Version": config.ZcodeClientVersion,
		"X-ZCode-Agent":       "glm",
		// 官方写的是 origin，**不带尾斜杠**。
		"HTTP-Referer": config.ZcodeEndpointOrigin,
		// 官方来源头（buildCliZCodeSourceHeaders）的其余部分。缺这些头与多发头
		// 一样是差异——官方模型请求恒带这 11 项。
		"X-Title":           prof.Title(),
		"X-Release-Channel": prof.ReleaseChannel,
		"X-Client-Language": prof.Language,
		"X-Client-Timezone": prof.Timezone,
		"X-Platform":        prof.Platform(),
		"X-Os-Category":     prof.OSCategory(),
		"X-Os-Version":      prof.OSRelease,
	}
	// X-Device-Mid 在**官方模型请求里并不存在**（只有非模型接口带，golden 实测）。
	// 设备指纹改由 body 的 metadata.user_id.device_id 承载（upstream.InjectDeviceMetadata），
	// 每账号一份 ⇒ 账号隔离不依赖本头。开关默认 false，只为应急回滚保留。
	if config.UpstreamSendDeviceMid {
		fixed["X-Device-Mid"] = acc.DeviceMidOr(config.DeviceMid())
	}
	// 归因头与固定头同级：客户端不能改写它们（取值已优先沿用客户端送来的值）。
	for key, value := range attr.Headers() {
		fixed[key] = value
	}
	if verifyParam != "" {
		fixed["X-Aliyun-Captcha-Verify-Param"] = verifyParam
		if verifyRegion != "" {
			fixed["X-Aliyun-Captcha-Verify-Region"] = verifyRegion
		}
	}

	// 透传头先合并，并把键统一为规范形式。不规范化的话，固定头与客户端送来的同名
	// 小写头会在 map 里各占一个键；下游 `Header.Set` 又把两者归一到同一名字，
	// 最终取值便取决于 map 的迭代顺序——同名头的结果成了随机的，这正是客户端能
	// 间接影响固定头的成因。
	//
	// x-zcode-* 一律不透传：X-ZCode-Agent / X-ZCode-App-Version 由固定头覆盖，
	// 而 x-zcode-session-type / x-zcode-trace-id 由 NewAttribution 从原始
	// incomingHeaders 里取回并作为固定头写出（见该函数说明），无需在这里放行；
	// 其余未知的 x-zcode-*（如 x-zcode-rpc-client-mode）属于下游自身上下文，
	// 转发出去会让本次请求带上不属于它的语义。
	headers := make(map[string]string, len(fixed)+len(incomingHeaders))
	for key, value := range incomingHeaders {
		lower := strings.ToLower(key)
		if dropHeaders[lower] || strings.HasPrefix(lower, "x-zcode") {
			continue
		}
		headers[textproto.CanonicalMIMEHeaderKey(key)] = value
	}
	// 固定头最后写回，同名透传头被覆盖——固定头必胜。
	for key, value := range fixed {
		headers[key] = value
	}

	return Request{URL: targetURL, Headers: headers}, nil
}
