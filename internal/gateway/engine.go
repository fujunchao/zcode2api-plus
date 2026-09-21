// 网关引擎：/v1/messages 的选号 + 验证码 + 上游调用 + 错误分类循环。
// 对应 Python 版 routes/gateway.py 的 messages() 与 _try_account()。
// M4 的 OpenAI 兼容层复用本引擎（通过 DeliverFunc 自定义交付）。
package gateway

import (
	"bytes"
	"context"
	cryptoRand "crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"strings"
	"time"

	"zcode2api/internal/captcha"
	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/proxy"
	"zcode2api/internal/store"
	"zcode2api/internal/upstream"
	"zcode2api/internal/web"
)

// Delivery 上游 200 响应的交付物；Body 已被 usage 收集器包装（流式）或已缓冲。
type Delivery struct {
	StatusCode  int
	ContentType string
	Header      http.Header
	Body        io.Reader
}

// DeliverFunc 交付上游 200 响应；返回 nil 表示客户端完整接收（计入 usage 统计），
// 返回错误表示客户端侧中断（不累计 usage，语义对齐 Python 版）。
type DeliverFunc func(d Delivery) error

// Engine 选号 + 验证码 + 上游调用 + 错误分类循环。
type Engine struct {
	Store   *store.Store
	Captcha *captcha.Manager
	Client  *http.Client

	// BusyRetryDelays 3010 并发准入的重试延迟（默认 1s/2s；测试可缩短）。
	BusyRetryDelays []time.Duration
	// RateLimitRetryDelay 瞬时限流原地重试的等待时长（默认 1s；测试可置 0）。
	// 与 BusyRetryDelays 分开配置，两者语义不同：3010 是模型级准入、换号无用，
	// 瞬时限流只值一次对冲。
	RateLimitRetryDelay time.Duration
	// OverloadRetryDelays 平台过载（529 / 业务码 1305）的原地退避档位，默认 1s/3s。
	// 同样与 BusyRetryDelays 分开：过载是平台侧拥塞，与账号无关，既不改账号状态
	// 也不换号，只按自己的档位退避重试。
	OverloadRetryDelays []time.Duration
	// OnQuotaRefresh 成功/耗尽后触发的额度刷新（M3 接入 quota 包；nil 跳过）。
	OnQuotaRefresh func(acc *model.Account)

	now func() time.Time
}

// NewEngine 创建引擎；client 为 nil 时使用默认上游客户端。
func NewEngine(st *store.Store, cm *captcha.Manager, client *http.Client) *Engine {
	if client == nil {
		client = defaultUpstreamClient()
	}
	return &Engine{
		Store:               st,
		Captcha:             cm,
		Client:              client,
		BusyRetryDelays:     defaultBusyRetryDelays,
		RateLimitRetryDelay: RateLimitRetryDelay,
		OverloadRetryDelays: OverloadRetryDelays,
		now:                 time.Now,
	}
}

// SetNow 注入时钟（测试用）。
func (e *Engine) SetNow(fn func() time.Time) { e.now = fn }

// clientFor 返回账号出站客户端：配置了代理（proxy_url，含代理线路指派）时
// 走代理 Transport，否则用引擎默认客户端。代理构造失败回退直连并记日志。
func (e *Engine) clientFor(acc *model.Account) *http.Client {
	if acc == nil || acc.ProxyURL == nil || *acc.ProxyURL == "" {
		return e.Client
	}
	t, err := proxy.TransportFor(*acc.ProxyURL)
	if err != nil {
		web.Warn("gateway", fmt.Sprintf("账号 %s 代理无效，回退直连: %v", acc.Name, err))
		return e.Client
	}
	return &http.Client{Transport: t}
}

// defaultUpstreamClient 对齐 Python 版超时语义：连接 30s、响应头最长 120s、
// 响应体流式读取不设超时（长连接 SSE）。
func defaultUpstreamClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 120 * time.Second,
			MaxIdleConnsPerHost:   8,
			IdleConnTimeout:       90 * time.Second,
		},
	}
}

// runResult 循环结束状态：Delivered=true 表示已通过 deliver 交付上游 200
// （handler 不得再写响应）；否则 Status/Body 是要回给客户端的错误 JSON。
type runResult struct {
	Delivered bool
	Status    int
	Body      any
}

// attemptResult 单次账号尝试的出路：retrySame（同账号再试一次）、
// switchAccount（换下一个账号）、final（终止并返回）。
type attemptResult struct {
	retrySame     bool
	switchAccount bool
	final         runResult
}

// attemptBudget 单账号内的三类重试预算，各自独立计数。
// 此前三者共用同一个循环变量 captchaAttempt：一次验证码重试就会吃掉 3010 的
// 等待预算，且 BusyRetryDelays 会按验证码次数取到错误下标——语义完全不同的两件事
// 共用一个数，扩展时会咬人。
type attemptBudget struct {
	captcha  int // 验证码刷新（含首次尝试，上限 MaxCaptchaRetries）
	busy     int // 3010 并发准入等待（上限 len(BusyRetryDelays)）
	rate     int // 瞬时限流原地重试（上限 MaxRateLimitRetries）
	overload int // 平台过载退避重试（上限 len(OverloadRetryDelays)）
}

// RunMessages 执行完整循环。body 是入口 NormalizeBody 后的请求体；
// incomingHeaders 为客户端透传头；deliver 在上游 200 时被调用一次。
func (e *Engine) RunMessages(ctx context.Context, body map[string]any, incomingHeaders map[string]string, deliver DeliverFunc) runResult {
	modelName, _ := body["model"].(string)
	stream := bodyBool(body, "stream")
	reqID := randomHex(3)
	// 观测诊断行：每请求恰好一条，收尾期输出（defer 保证早退路径也发）。
	// 刻意不改 web.Req/ReqOk/ReqErr 的形态，也不改本函数的导出签名
	//（reqID 本就在函数内生成，三个外部调用点无需改动）。
	diag := NewReqDiag(reqID, orDash(modelName), stream, body, e.now)
	defer func() {
		// 总耗时在这里统一冻结（而不是在 finishDelivery 里）：早退路径
		//（如 no_available_account）从来没走到交付，也应当有可读的耗时。
		diag.Finish(diag.Now())
		web.Diag(diag.ReqID, diag.Format())
	}()
	web.Req(reqID, orDash(modelName), stream)

	tried := map[string]bool{}
	for range MaxAccountAttempts {
		acc := e.Store.Select(model.ProviderZai, tried, modelName)
		if acc == nil {
			break
		}
		tried[acc.ID] = true
		diag.Attempts++
		res := e.tryAccount(ctx, reqID, acc, body, modelName, stream, incomingHeaders, deliver, diag)
		if res.retrySame {
			continue
		}
		if res.switchAccount {
			continue
		}
		return res.final
	}

	// 选不出号 ≠ 「账号没绑定模型/额度用完」：冷却、风控、停用、整号耗尽都会走到
	// 这里。错误体如实携带池状态分解（文案 + 结构化 details），别再让一次整池
	// 冷却被静态文案误读成绑定问题（2026-09-20 事故）。
	stat := e.Store.PoolStats(model.ProviderZai, modelName)
	detailText := PoolStatusText(stat)
	if modelName != "" {
		if detailText != "" {
			web.ReqErr(reqID, fmt.Sprintf("模型 %s 無可用帳號（%s）", modelName, detailText))
			return errResultWithDetails(http.StatusServiceUnavailable, "no_available_account",
				fmt.Sprintf("模型 %s 目前無可用帳號（%s），請在後台檢查帳號狀態", modelName, detailText),
				PoolStatusDetails(stat, modelName))
		}
		web.ReqErr(reqID, fmt.Sprintf("模型 %s 無可用帳號（未提供此模型或額度已用完）", modelName))
		return errResultWithDetails(http.StatusServiceUnavailable, "no_available_account",
			fmt.Sprintf("模型 %s 目前無可用帳號（帳號未提供此模型或額度已用完），請在後台檢查帳號狀態", modelName),
			PoolStatusDetails(stat, modelName))
	}
	if detailText != "" {
		web.ReqErr(reqID, "无可用账号: "+detailText)
		return errResultWithDetails(http.StatusServiceUnavailable, "no_available_account",
			fmt.Sprintf("所有账号均不可用（%s），请在后台检查账号状态", detailText),
			PoolStatusDetails(stat, modelName))
	}
	web.ReqErr(reqID, "无可用账号 / 额度均已耗尽")
	return errResult(http.StatusServiceUnavailable, "no_available_account",
		"所有账号均不可用或额度已用完，请在后台检查账号状态")
}

// tryAccount 单个账号的尝试：内层为同账号重试（验证码刷新 / 3010 等待 / 瞬时限流）。
// 循环的每一次 continue 都必须先从 attemptBudget 对应类目里扣一次预算，二者手动
// 保持一致——预算之和加首次尝试即循环上界，任何一条路径漏记账都会让循环不收敛。
func (e *Engine) tryAccount(
	ctx context.Context,
	reqID string,
	acc *model.Account,
	body map[string]any,
	modelName string,
	stream bool,
	incomingHeaders map[string]string,
	deliver DeliverFunc,
	diag *ReqDiag,
) attemptResult {
	needsCaptcha := acc.Mode == "jwt"

	// 记录出口身份：账号名与线路标签。账号在 Select 之后才可得，这正是起始行
	// （web.Req）打不出它的原因。多次尝试时以最后一次为准，逐次明细仍在既有
	// [~] 行里（那些行本来就带账号名）。
	if diag != nil {
		diag.AccName = acc.Name
		diag.Route = e.Store.ProxyLabel(acc)
	}

	var b attemptBudget
	budget := 1 + MaxCaptchaRetries + len(e.BusyRetryDelays) + MaxRateLimitRetries + len(e.OverloadRetryDelays)

	for range budget {
		var verifyParam, verifyRegion string
		if needsCaptcha {
			token, err := e.Captcha.GetVerifyParam(ctx)
			if err != nil {
				e.Captcha.Invalidate()
				b.captcha++
				e.recordError(acc, model.ErrorKindCaptchaFailed, "验证码自动求解失败: "+err.Error())
				if b.captcha < MaxCaptchaRetries {
					web.Warn(reqID, fmt.Sprintf("验证码自动求解失败，刷新令牌重试（第 %d 次）", b.captcha))
					continue
				}
				return attemptResult{final: e.captchaRequired(reqID, err.Error())}
			}
			if token != nil {
				verifyParam, verifyRegion = token.VerifyParam, token.Region
			}
		}

		// 每个账号在副本上做 NormalizeBody（system 注入不幂等，见 body.go）
		actualBody := shallowCopyBody(body)
		NormalizeBody(actualBody, needsCaptcha)
		payload, err := marshalJSON(actualBody)
		if err != nil {
			web.Err(reqID, fmt.Sprintf("请求体序列化失败: %v", err))
			return attemptResult{final: errResult(http.StatusBadRequest, "invalid_request", "请求体无法序列化")}
		}
		if diag != nil {
			// 真正发给上游的请求体字节数（已含注入的 system）。入口处拿不到这个真值。
			diag.BodyBytes = len(payload)
		}

		req, err := upstream.BuildRequest(acc, verifyParam, verifyRegion, incomingHeaders)
		if err != nil {
			e.mark(acc, model.StatusInvalid, model.ErrorKindAuthFailed, err.Error())
			web.Warn(reqID, fmt.Sprintf("账号 %s 凭证无效，切换下一个", acc.Name))
			return attemptResult{switchAccount: true}
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, req.URL, bytes.NewReader(payload))
		if err != nil {
			e.mark(acc, model.StatusInvalid, model.ErrorKindAuthFailed, err.Error())
			return attemptResult{switchAccount: true}
		}
		for k, v := range req.Headers {
			httpReq.Header.Set(k, v)
		}

		if diag != nil {
			// 首字节延迟与耗时的基准点：紧贴出站调用。
			diag.UpstreamStart = diag.Now()
		}
		resp, err := e.clientFor(acc).Do(httpReq)
		if err != nil {
			if isClientGone(ctx) {
				// 客户端已断开（或本请求已被取消）：账号本身没问题，不冷却、也不换号
				// ——换号只会白烧另一个账号的额度。
				// 只记「归类 + 时间」而**不覆盖 last_error 文案**：用户按一下 ESC 不该
				// 把上一次真实故障的文案冲掉，但「这个号最近发生过什么」仍值得留痕。
				e.recordError(acc, model.ErrorKindClientCanceled, "")
				web.Warn(reqID, fmt.Sprintf("请求已取消（账号 %s）：%v", acc.Name, err))
				return attemptResult{final: errResult(http.StatusBadGateway, "request_canceled", "请求已取消")}
			}
			e.mark(acc, model.StatusCooling, model.ErrorKindConnectionFailed, "连接失败: "+err.Error())
			web.Warn(reqID, fmt.Sprintf("账号 %s 连接失败，切换下一个", acc.Name))
			return attemptResult{switchAccount: true}
		}

		if diag != nil {
			diag.Status = resp.StatusCode
		}

		if resp.StatusCode >= 400 {
			res := e.handleUpstreamError(ctx, reqID, acc, modelName, needsCaptcha, resp, &b)
			if res.retrySame {
				continue
			}
			return res
		}

		contentType := resp.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "application/json"
		}

		// HTTP 200 且为 JSON：先缓冲，处理 HTTP 200 包装的业务错误
		if strings.Contains(contentType, "application/json") {
			buffered, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
			_ = resp.Body.Close()
			if err != nil {
				e.bumpFail(acc, model.ErrorKindConnectionFailed)
				web.ReqErr(reqID, fmt.Sprintf("上游错误体读取失败（账号 %s）", acc.Name))
				return attemptResult{final: errResult(http.StatusBadGateway, "upstream_error", textPreview(err.Error()))}
			}
			res := e.handleUpstreamJSON(ctx, reqID, acc, modelName, needsCaptcha, stream, contentType, buffered, &b, deliver, diag)
			if res.retrySame {
				continue
			}
			return res
		}

		// SSE 成功：tee 读取流，交付完整后计入 usage
		return e.deliverStream(ctx, reqID, acc, contentType, resp, deliver, diag)
	}

	// 预算耗尽（防御分支：每个 continue 都已各自记账，正常路径不会走到这里）
	if needsCaptcha {
		return attemptResult{final: e.captchaRequired(reqID, "验证码重试次数已耗尽")}
	}
	return attemptResult{switchAccount: true}
}

// maxErrorBodyBytes 错误响应体的读取上限。
//
// 正常响应走流式转发（边读边发）不受影响；只有错误分支要整段读进内存做分类，
// 而上游或账号级代理异常时可能回一个任意大的 body，无上限会直接吃光内存。
// 64KB 足以容纳业务码与错误消息。
const maxErrorBodyBytes = 64 << 10

// handleUpstreamError 处理上游 >=400 的响应（分类链顺序对齐 PLAN §5.2，不可变）。
func (e *Engine) handleUpstreamError(
	ctx context.Context,
	reqID string,
	acc *model.Account,
	modelName string,
	needsCaptcha bool,
	resp *http.Response,
	b *attemptBudget,
) attemptResult {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	_ = resp.Body.Close()
	if err != nil {
		if isClientGone(ctx) {
			// 同上的取消语义：读错误体被取消不代表账号有问题。
			e.recordError(acc, model.ErrorKindClientCanceled, "")
			web.Warn(reqID, fmt.Sprintf("读上游错误体时请求已取消（账号 %s）：%v", acc.Name, err))
			return attemptResult{final: errResult(http.StatusBadGateway, "request_canceled", "请求已取消")}
		}
		// 读不出错误体：按连接失败处理
		e.mark(acc, model.StatusCooling, model.ErrorKindConnectionFailed, "连接失败: "+err.Error())
		return attemptResult{switchAccount: true}
	}
	text := string(body)

	// 1) 验证码挑战（响应头 / code=3007 / F001 与文本仅限 400/403）
	if needsCaptcha && IsCaptchaError(text, resp.StatusCode, resp.Header) {
		e.Captcha.Invalidate()
		b.captcha++
		e.recordError(acc, model.ErrorKindCaptchaFailed, "上游拒绝验证码 HTTP "+fmt.Sprint(resp.StatusCode))
		web.Warn(reqID, fmt.Sprintf("账号 %s 验证码失效，刷新重试（第 %d 次）", acc.Name, b.captcha))
		if b.captcha >= MaxCaptchaRetries {
			detail := "上游连续拒绝验证码"
			if hasCaptchaChallengeHeader(resp.Header) {
				detail = "上游持续返回验证码挑战"
			}
			return attemptResult{final: e.captchaRequired(reqID, detail)}
		}
		return attemptResult{retrySame: true}
	}

	// 2) 鉴权失败是强信号（JWT 上游为裸 401 空 body；api.z.ai 为 type=1000/1001/1003）
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		e.mark(acc, model.StatusInvalid, model.ErrorKindAuthFailed,
			fmt.Sprintf("鉴权失败 HTTP %d", resp.StatusCode))
		web.Warn(reqID, fmt.Sprintf("账号 %s 鉴权失败 %d，切换下一个%s", acc.Name, resp.StatusCode, ErrorDetail(text)))
		return attemptResult{switchAccount: true}
	}

	// 3) 402 → 该模型耗尽
	if resp.StatusCode == http.StatusPaymentRequired {
		e.markModelExhausted(acc, modelName, fmt.Sprintf("%s 額度已用完", orCurrent(modelName)))
		web.Warn(reqID, fmt.Sprintf("账号 %s 的 %s 額度用完，切換下一個%s", acc.Name, orCurrent(modelName), ErrorDetail(text)))
		e.fireRefresh(acc)
		return attemptResult{switchAccount: true}
	}

	// 4) 429 且 code=3010：模型并发准入限制，账号仍可用，按原版客户端延迟重试
	if IsModelConcurrencyLimit(resp.StatusCode, text) {
		// 账号状态不变，但这次失败要留痕（并发准入是模型级限制，与账号健康无关）。
		e.recordError(acc, model.ErrorKindModelBusy, "模型并发准入受限 HTTP 429 code=3010")
		if b.busy < len(e.BusyRetryDelays) {
			delay := e.BusyRetryDelays[b.busy]
			b.busy++
			web.Warn(reqID, fmt.Sprintf("模型并发准入受限，%g s 后重试（账号仍可用）%s", delay.Seconds(), ErrorDetail(text)))
			if !sleepCtx(ctx, delay) {
				return attemptResult{final: errResult(http.StatusServiceUnavailable, "canceled", "请求已取消")}
			}
			return attemptResult{retrySame: true}
		}
		web.Warn(reqID, fmt.Sprintf("模型并发准入持续受限（账号 %s），保留账号状态%s", acc.Name, ErrorDetail(text)))
		return attemptResult{final: runResult{
			Status: resp.StatusCode,
			Body:   passthroughBodyWithType(text, "upstream_rate_limit"),
		}}
	}

	// 5) 529 / 业务码 1305：平台服务过载。必须排在 429 分支之前——官方错误码表把
	// 1305 标成 429，若先过 429 分支，一次平台过载就会被当成「该账号被限速」而挨上
	// 30→60→120→300s 的递进冷却，整池健康账号会被逐个踢出调度。
	//
	// 官方定义 1305 属于「平台服务过载，与单一账户的调用行为无直接关系」（1302 才是
	// 账户维度的速率限制）。因此这里既不标账号状态、也不换号：换号救不了过载，冷却
	// 只会缩小可用池。只按自己的档位退避重试，用尽后原样透传——让上层看到上游原始的
	// 529 与 1305 文案，而不是被改写成别的错误。
	//
	// 判据走 IsUpstreamOverload 而非直接比 429：上游在 Anthropic 兼容面用 529 承载
	// 1305（线上 30 次实测全部如此），只看 429 会整类漏掉。此处刻意不 bumpFail：
	// 平台过载不是账号的错，与 402/429 一致。
	if IsUpstreamOverload(resp.StatusCode, text) {
		code := orDash(UpstreamBusinessCode(text))
		// 账号状态、冷却、失败计数都不动，但这次失败要留痕——否则前台无从得知
		// 「这个号刚才失败过，而且是平台过载」。
		e.recordError(acc, model.ErrorKindUpstreamOverload,
			fmt.Sprintf("上游平台过载 HTTP %d code=%s", resp.StatusCode, code))
		if b.overload < len(e.OverloadRetryDelays) {
			delay := JitteredDelay(e.OverloadRetryDelays[b.overload])
			b.overload++
			web.Warn(reqID, fmt.Sprintf("上游平台过载 HTTP %d code=%s，%g s 后原地重试（账号状态不变）%s",
				resp.StatusCode, code, delay.Seconds(), ErrorDetail(text)))
			if !sleepCtx(ctx, delay) {
				return attemptResult{final: errResult(http.StatusServiceUnavailable, "canceled", "请求已取消")}
			}
			return attemptResult{retrySame: true}
		}
		web.Warn(reqID, fmt.Sprintf("上游平台过载持续（账号 %s），原样透传 HTTP %d code=%s%s",
			acc.Name, resp.StatusCode, code, ErrorDetail(text)))
		return attemptResult{final: runResult{
			Status: resp.StatusCode,
			Body:   passthroughBodyWithType(text, "upstream_error"),
		}}
	}

	// 6) 429：官方用量上限码族 → 该模型耗尽；其余瞬时限流 → 先原地重试，用尽才冷却换号
	if resp.StatusCode == http.StatusTooManyRequests {
		if quotaExhaustedCodes[UpstreamBusinessCode(text)] {
			e.markModelExhausted(acc, modelName, fmt.Sprintf("%s 額度/用量上限已達", orCurrent(modelName)))
			web.Warn(reqID, fmt.Sprintf("账号 %s 的 %s 觸發用量上限，切換下一個%s", acc.Name, orCurrent(modelName), ErrorDetail(text)))
			e.fireRefresh(acc)
			return attemptResult{switchAccount: true}
		}
		// 瞬时限流的窗口可能只有数秒，先给同一账号一次原地重试的机会。不给多次：
		// 池子里通常还有别的账号，换号成本是百毫秒级，而每次原地等待都是确定的
		// 秒级延迟，且大概率仍然失败、最后照样要换号。
		if b.rate < MaxRateLimitRetries {
			b.rate++
			delay := JitteredDelay(e.RateLimitRetryDelay)
			web.Warn(reqID, fmt.Sprintf("账号 %s 被瞬时限流 429，%g s 后原地重试", acc.Name, delay.Seconds()))
			if !sleepCtx(ctx, delay) {
				return attemptResult{final: errResult(http.StatusServiceUnavailable, "canceled", "请求已取消")}
			}
			return attemptResult{retrySame: true}
		}
		secs, streak := e.markRateLimited(acc, "上游限流 HTTP 429")
		web.Warn(reqID, fmt.Sprintf("账号 %s 连续第 %d 次被限流，冷却 %d s 后切换下一个",
			acc.Name, streak, secs))
		return attemptResult{switchAccount: true}
	}

	// 7) 503 → 按连续次数递进冷却换号（默认 30/60/120s，可后台配置；超阶梯封顶
	//    CoolingSeconds）。固定 300s 的年代，一次上游抖动会把整池在几十秒内冷却
	//    清空（2026-09-20 事故）。日志带 body 预览：503 的关键信息（是 z.ai 侧
	//    还是线路侧的 5xx）在 body 里，只记状态码不可见。
	if resp.StatusCode == http.StatusServiceUnavailable {
		errMsg := "上游服務不可用 HTTP 503: " + ErrorPreview(text)
		secs, streak := e.markUpstreamUnavailable(acc, errMsg)
		web.Warn(reqID, fmt.Sprintf("账号 %s 上游返回 503（連續第 %d 次），冷卻 %d s 並切換下一個: %s",
			acc.Name, streak, secs, ErrorPreview(text)))
		return attemptResult{switchAccount: true}
	}

	// 8) 405 + 风控文案：上游风控拦截，冷却整个账号（按连续命中次数递进），
	// 连续次数超过阶梯长度则置为失效、交人工处理。
	//
	// 先看 body 再看状态码：405 在本项目里有三种完全不同的含义——风控拦截、计费接口
	// 的重复查询、以及 JWT 账号缺顶层 system 注入时上游回的 405。最后一种是**我方构造
	// 请求的缺陷**（见 body.go / upstream/request.go 的说明），换号与冷却都没用，每个
	// 账号都会一样地失败；只看状态码冷却会把它变成对账号的集体惩罚。
	//
	// 风控看身份维度（账号/设备指纹/出口 IP/请求头），与模型无关，所以这里不换模型、
	// 直接停整个账号；冷却期间该号 IsSelectable 为 false，本请求自然换下一个。
	if resp.StatusCode == http.StatusMethodNotAllowed && model.IsRiskControlBody(text) {
		secs, streak, invalid := e.markRiskControl(acc, "上游风控拦截 HTTP 405: "+ErrorPreview(text))
		if invalid {
			web.Warn(reqID, fmt.Sprintf("账号 %s 连续第 %d 次命中风控，已置為失效待人工處理", acc.Name, streak))
		} else {
			web.Warn(reqID, fmt.Sprintf("账号 %s 第 %d 次命中风控，冷却 %d s 后切换下一个", acc.Name, streak, secs))
		}
		return attemptResult{switchAccount: true}
	}

	// 9) 其余错误：非已知信号，不做账号状态推断，直接原样继承上游响应
	e.bumpFail(acc, model.ErrorKindUpstreamError)
	// 日志带上业务码与截断预览：只记状态码时，1305 / 风控这类「关键信息在 body 里」
	// 的失败在日志里完全不可见。
	web.ReqErr(reqID, fmt.Sprintf("上游错误 HTTP %d（账号 %s）: %s",
		resp.StatusCode, acc.Name, ErrorPreview(text)))
	return attemptResult{final: runResult{
		Status: resp.StatusCode,
		Body:   passthroughBodyWithType(text, "upstream_error"),
	}}
}

// handleUpstreamJSON 处理 HTTP 200 且 content-type 为 JSON 的响应：
// ZCode 的业务错误有时仍使用 HTTP 200，不能当成 Anthropic 成功回應。
func (e *Engine) handleUpstreamJSON(
	ctx context.Context,
	reqID string,
	acc *model.Account,
	modelName string,
	needsCaptcha bool,
	stream bool,
	contentType string,
	buffered []byte,
	b *attemptBudget,
	deliver DeliverFunc,
	diag *ReqDiag,
) attemptResult {
	text := string(buffered)
	code := UpstreamBusinessCode(text)

	switch {
	case code == "1005":
		e.markModelExhausted(acc, modelName, fmt.Sprintf("%s 每日額度已用完", orCurrent(modelName)))
		web.Warn(reqID, fmt.Sprintf("帳號 %s 的 %s 每日額度用完，切換下一個%s", acc.Name, orCurrent(modelName), ErrorDetail(text)))
		e.fireRefresh(acc)
		return attemptResult{switchAccount: true}

	case needsCaptcha && code == "3007":
		e.Captcha.Invalidate()
		b.captcha++
		e.recordError(acc, model.ErrorKindCaptchaFailed, "上游拒絕驗證碼 code=3007")
		web.Warn(reqID, fmt.Sprintf("帳號 %s 驗證碼失效，刷新重試（第 %d 次）%s", acc.Name, b.captcha, ErrorDetail(text)))
		if b.captcha >= MaxCaptchaRetries {
			return attemptResult{final: e.captchaRequired(reqID, "上游連續拒絕驗證碼")}
		}
		return attemptResult{retrySame: true}

	case code != "" && code != "0":
		e.bumpFail(acc, model.ErrorKindUpstreamError)
		web.ReqErr(reqID, fmt.Sprintf("上游業務錯誤 code=%s（帳號 %s）", code, acc.Name))
		return attemptResult{final: runResult{
			Status: http.StatusBadGateway,
			Body: map[string]any{"error": map[string]any{
				"message": MessageFromJSON(text, buffered),
				"type":    "upstream_error",
				"code":    code,
			}},
		}}

	case stream:
		e.bumpFail(acc, model.ErrorKindInvalidResponse)
		web.ReqErr(reqID, fmt.Sprintf("上游串流請求返回非 SSE JSON（帳號 %s）", acc.Name))
		return attemptResult{final: errResult(http.StatusBadGateway, "invalid_upstream_response", "上游未返回有效的 SSE 串流")}
	}

	// 成功（业务码缺失 / 0）：交付缓冲体
	e.success(acc)
	usage := NewUsageCollector(strings.Contains(contentType, "text/event-stream"))
	usage.Feed(buffered)
	usage.Finish()
	err := deliver(Delivery{
		StatusCode:  http.StatusOK,
		ContentType: contentType,
		Header:      http.Header{},
		Body:        bytes.NewReader(buffered),
	})
	return e.finishDelivery(ctx, reqID, acc, usage, err, diag)
}

// deliverStream 交付 SSE 流式响应；usage 随读取同步收集，客户端完整接收后计入。
func (e *Engine) deliverStream(ctx context.Context, reqID string, acc *model.Account, contentType string, resp *http.Response, deliver DeliverFunc, diag *ReqDiag) attemptResult {
	e.success(acc)
	usage := NewUsageCollector(strings.Contains(contentType, "text/event-stream"))
	err := deliver(Delivery{
		StatusCode:  resp.StatusCode,
		ContentType: contentType,
		Header:      resp.Header,
		Body:        &teeReader{r: resp.Body, c: usage, diag: diag, now: e.now},
	})
	_ = resp.Body.Close()
	return e.finishDelivery(ctx, reqID, acc, usage, err, diag)
}

// finishDelivery 交付收尾：累计 usage 并写日志。
//
// 「能不能计入」的判据是上游有没有交出终值（UsageComplete），不是客户端有没有把流读完。
// zcode 编辑器这类客户端收到 finish_reason 就立刻关流是常态，此时 message_delta 的终值
// 已经到手，用量是准的；若沿用旧的「客户端没读完就不计入」，账号用量会系统性少算
// （线上曾漏计一次 57,352 output token 的生成，详见
// docs/analysis-flash-30min-stream-cut.md §5）。usage 不完整时仍旧不计入。
func (e *Engine) finishDelivery(ctx context.Context, reqID string, acc *model.Account, usage *UsageCollector, err error, diag *ReqDiag) attemptResult {
	if diag == nil {
		// 防御：单测可以只关心状态机而不提供诊断容器。
		diag = &ReqDiag{clock: e.now}
	}
	if err != nil {
		diag.Usage = usage.AsDict()
		diag.UsageComplete = usage.UsageComplete()
		diag.ErrText = err.Error()
		// 纯观测计数，且**只统计上游侧掐断**：这个出口同时承载「客户端写失败/断开」
		// （客户端收到 finish_reason 就关流是常态），把那种也算进去会让计数失真。
		// 判据沿用仓库既有的 isClientGone(ctx)——ctx 结束即客户端侧，不是上游掐断；
		// 这也与日志里 `context canceled` 与 `unexpected EOF` 的区分口径一致。
		// 计数不冷却、不换号、不改状态：响应头已经发给客户端，无法换号重试，且没有
		// 证据表明责任在账号（把上游的时长墙记成账号故障＝集体惩罚）。
		if !isClientGone(ctx) {
			diag.TruncTotal = RecordStreamTruncate(e.Store, acc.Provider, acc.ID)
		}
		if usage.UsageComplete() {
			got := e.accumulateUsage(acc, usage)
			web.ReqErr(reqID, fmt.Sprintf("流传输中断，上游 usage 已完整，仍计入 %d tok: %v", got.Output, err))
		} else {
			web.ReqErr(reqID, fmt.Sprintf("流传输中断: %v", err))
		}
		return attemptResult{final: runResult{Delivered: true}}
	}
	usage.Finish()
	got := e.accumulateUsage(acc, usage)
	diag.Usage = got
	diag.UsageComplete = usage.UsageComplete()
	web.ReqOk(reqID, got.Output)
	return attemptResult{final: runResult{Delivered: true}}
}

// accumulateUsage 把一次交付的 token 用量累加到账号上，返回本次用量。
//
// 用量累加必须在 store 锁内对「当前」对象做：acc 是 Select 交出的账号副本，
// 改副本不落库，也会与并发的额度刷新/领取回写相撞。
func (e *Engine) accumulateUsage(acc *model.Account, usage *UsageCollector) model.Usage {
	got := usage.AsDict()
	_, _ = e.Store.Update(acc.Provider, acc.ID, func(live *model.Account) {
		live.AccumulateTokens(got)
	})
	return got
}

// ── 账号状态标记（对齐 Python 版 _mark / _mark_model_exhausted）──────────────
//
// MarkAccount / MarkModelExhausted 同时导出，供 asyncpool 复用同一套状态机；
// 两条请求路径对同一账号必须标出相同状态（曾因 asyncpool 自行实现而分歧）。

// MarkAccount 设置账号状态并记下这次失败的归类；status 为 cooling 时按配置写入
// 冷却截止时间。
//
// 按 provider + ID 在 store 锁内改「当前」对象：调用方持有的是账号副本，直接改它
// 既不落库、也会与并发读写相撞；账号已不存在时静默跳过（可能刚被后台删除）。
//
// kind 必填而不是可选（见 classify.go「最近错误」一节）：账号状态与错误归类在同一次
// 写入里落库，既省一次持久化，也避免两处对同一次失败给出不同分类。
func MarkAccount(st *store.Store, provider, idOrName, status, kind, errMsg string, now time.Time) {
	_, _ = st.Update(provider, idOrName, func(acc *model.Account) {
		acc.Status = status
		if status == model.StatusCooling {
			until := float64(now.Add(time.Duration(config.CoolingSeconds)*time.Second).UnixNano()) / 1e9
			acc.CoolingUntil = &until
		}
		StampAccountError(acc, kind, errMsg, now)
	})
}

// isStrongStatus 判断账号状态是否强于「额度信号」能改写的范围。
//
// invalid 需要人工介入、cooling 处于冷却窗口、disabled 是管理员主动停用：三者都与
// 「当前额度用没用完」无关。配额信号（402 / 200+1005 / 429 上限码族）不得把它们
// 刷回 active —— 否则并发下刚被判失效的账号会被另一条请求放回轮询。
func isStrongStatus(status string) bool {
	switch status {
	case model.StatusInvalid, model.StatusCooling, model.StatusDisabled:
		return true
	}
	return false
}

// isClientGone 判断本次失败是否源于客户端侧（客户端断开、客户端自己的 deadline
// 到期、服务停机）——判据是「请求 ctx 是否已经结束」，而不是错误链里出现了哪个
// 错误值。
//
// 这与账号健康无关：据此冷却账号会把一个完好的账号踢出池子整整一轮冷却时长，
// 而一次客户端中断最多可连锁影响 MaxAccountAttempts 个账号。
//
// ⚠️ 不要改回 errors.Is(err, context.DeadlineExceeded)：Go 的 net.Dialer 把
// Timeout 实现为 context deadline，Transport 的响应头超时同样携带该错误值
// （本机 Go 1.25 实测），于是「拨号 30s 超时」「120s 等不到响应头」这类
// 线路/上游故障也会命中——账号不冷却、坏线路不被标记，且客户端收到误导性的
// 502「请求已取消」（线上日志：代理连接超时 13 次全部被误判）。这些场景属于
// 连接失败，必须落入冷却换号分支；只有 ctx 真的结束才算客户端侧取消。
func isClientGone(ctx context.Context) bool {
	return ctx.Err() != nil
}

// MarkModelExhausted 只停用已耗尽的请求模型；所有已知模型皆耗尽时才停用整号。
// 账号已处于强状态时只更新模型耗尽清单，不动账号状态与冷却窗口。
//
// 整段读改写都在 store 锁内完成——包括 isStrongStatus 守卫与对 Quota 的遍历。
// 这两处都需要读「当下」的值：守卫若在锁外判定，判定与写入之间仍有一个窗口，
// 另一路请求足以把刚判 invalid 的账号刷回可用。
//
// 错误归类固定为 quota_exhausted：本函数的全部调用点（402、429 上限码族、200+1005）
// 都是额度/用量上限这一件事，kind 由函数语义唯一确定，故不开放成参数。
func MarkModelExhausted(st *store.Store, provider, idOrName string, modelName any, errMsg string, now time.Time) {
	_, _ = st.Update(provider, idOrName, func(acc *model.Account) {
		// 先记额度事实（与账号状态无关，任何情况下都要落库）。
		marked := acc.MarkModelExhausted(modelName)
		strong := isStrongStatus(acc.Status)
		if !marked {
			if !strong {
				acc.Status = model.StatusExhausted
			}
			StampAccountError(acc, model.ErrorKindQuotaExhausted, errMsg, now)
			return
		}
		anyState := false
		allExhausted := true
		for name, quota := range acc.Quota {
			entryModel, _ := quota["model"].(string)
			if entryModel == "" {
				entryModel = name
			}
			anyState = true
			if acc.ModelAvailability(entryModel) != "exhausted" {
				allExhausted = false
				break
			}
		}
		if !strong {
			if anyState && allExhausted {
				acc.Status = model.StatusExhausted
			} else {
				acc.Status = model.StatusActive
			}
			acc.CoolingUntil = nil
		}
		StampAccountError(acc, model.ErrorKindQuotaExhausted, errMsg, now)
	})
}

func (e *Engine) mark(acc *model.Account, status, kind, errMsg string) {
	MarkAccount(e.Store, acc.Provider, acc.ID, status, kind, errMsg, e.now())
}

func (e *Engine) markModelExhausted(acc *model.Account, modelName any, errMsg string) {
	MarkModelExhausted(e.Store, acc.Provider, acc.ID, modelName, errMsg, e.now())
}

// recordError 写一次「最近错误」，不改账号状态、不计失败次数。
// 用于「该失败不影响账号可用性」的分类：平台过载、并发准入、验证码、客户端取消。
func (e *Engine) recordError(acc *model.Account, kind, detail string) {
	RecordAccountError(e.Store, acc.Provider, acc.ID, kind, detail, e.now())
}

// markRateLimited 瞬时限流的递进冷却，返回（本次冷却秒数, 累加后的连续次数）。
//
// 连续次数必须取返回值：acc 是 Select 交出的账号副本，MarkRateLimited 更新的是
// store 内的 live 对象，读 acc.RateLimitStreak 只会拿到旧值（线上曾把「第 1 次」
// 打成「第 0 次」；asyncpool 侧一直用的就是返回值）。
func (e *Engine) markRateLimited(acc *model.Account, errMsg string) (int, int) {
	return MarkRateLimited(e.Store, acc.Provider, acc.ID, errMsg, e.now())
}

// markRiskControl 上游风控的递进冷却，返回（本次冷却秒数, 累加后的连续次数, 是否已失效）。
// 与 markRateLimited 同理：连续次数只能取返回值，acc 是副本，读 acc.RiskControlStreak
// 拿到的是旧值。
func (e *Engine) markRiskControl(acc *model.Account, errMsg string) (int, int, bool) {
	return MarkRiskControl(e.Store, acc.Provider, acc.ID, errMsg, e.now())
}

// markUpstreamUnavailable 上游 503 的递进冷却，返回（本次冷却秒数, 累加后的连续次数）。
// 与 markRateLimited 同理：streak 只能取返回值（acc 是 Select 交出的副本）。
func (e *Engine) markUpstreamUnavailable(acc *model.Account, errMsg string) (int, int) {
	return MarkUpstreamUnavailable(e.Store, acc.Provider, acc.ID, errMsg, e.now())
}

// success 记录成功调用的账号状态；并异步触发一次额度刷新
// （对齐 Python 200 成功路径的 create_task(_safe_refresh)）。
func (e *Engine) success(acc *model.Account) {
	ts := float64(e.now().UnixNano()) / 1e9
	_, _ = e.Store.Update(acc.Provider, acc.ID, func(live *model.Account) {
		live.UseCount++
		live.LastUsedAt = &ts
		// 成功即认为限流窗口已过：清零连续计数，下一次再被限流从最短档重新起算。
		// 必须对 live 调用——对副本清零不会落库，会出现「一边归零、一边继续递增」。
		// 风控计数同理：冷却到期后能真正打通一次，说明这次拦截是偶发的，回到最低档。
		// 503 计数同理：上游链路真正恢复了一次，下次抖动从最短档重新起算。
		ResetRateLimitStreak(live)
		ResetRiskControlStreak(live)
		ResetUpstream503Streak(live)
		if live.Status == model.StatusCooling || live.Status == model.StatusExhausted {
			live.Status = model.StatusActive
		}
	})
	e.fireRefresh(acc)
}

// bumpFail 记一次失败计数，并一并写下这次失败的归类。
// kind 必填：调用点都是「已知分类但不需要改账号状态」的出口，漏给会编译失败。
//
// 刻意不覆盖 LastError 文案：LastError 的既有契约是「带可读文案的失败原因」，由
// MarkAccount / MarkModelExhausted / MarkRateLimited 负责写。这里只补归类与时间，
// 保持 last_error 语义不变（store 侧用 last_error == nil 表示账号已恢复健康）。
func (e *Engine) bumpFail(acc *model.Account, kind string) {
	now := e.now()
	_, _ = e.Store.Update(acc.Provider, acc.ID, func(live *model.Account) {
		live.FailCount++
		StampAccountError(live, kind, "", now)
	})
}

// fireRefresh 触发额度刷新（M3 接入 quota 包；仅 JWT 账号，对齐 _safe_refresh）。
// 只读账号上的两个不变量字段（provider / mode）；quota 内部会按 ID 重新取快照，
// 因此这里传副本是安全的。
func (e *Engine) fireRefresh(acc *model.Account) {
	if e.OnQuotaRefresh == nil {
		return
	}
	if acc.Provider != model.ProviderZai || acc.Mode != "jwt" {
		return
	}
	go e.OnQuotaRefresh(acc)
}

// captchaRequired 验证码不可用的统一 503 响应。
func (e *Engine) captchaRequired(reqID, detail string) runResult {
	web.ReqErr(reqID, "验证码不可用: "+detail)
	return errResult(http.StatusServiceUnavailable, "captcha_required",
		"自动验证码暂时失败，请稍后重试；如仍失败再打开后台 /admin/captcha")
}

// ── 小工具 ──────────────────────────────────────────────────────────────────

// sleepCtx 等待 delay 或 ctx 结束；返回 false 表示请求已被取消。
// 所有重试等待都走这里：等待期间客户端可能已经断开，继续重试只是白烧上游配额。
func sleepCtx(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return ctx.Err() == nil
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(delay):
		return true
	}
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

// teeReader 在读取时同步餵入 usage 收集器，并把「首字节 / 最大 chunk 间隔 / 总字节数」
// 记进诊断容器（读上游 body 的唯一必经点；非流式缓冲交付不经这里）。
type teeReader struct {
	r    io.Reader
	c    *UsageCollector
	diag *ReqDiag
	now  func() time.Time
}

func (t *teeReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		t.c.Feed(p[:n])
		if t.diag != nil {
			if t.now != nil {
				t.diag.MarkRead(t.now(), n)
			} else {
				t.diag.MarkRead(time.Now(), n)
			}
		}
	}
	return n, err
}

func shallowCopyBody(body map[string]any) map[string]any {
	out := make(map[string]any, len(body)+1)
	maps.Copy(out, body)
	return out
}

func bodyBool(body map[string]any, key string) bool {
	b, _ := body[key].(bool)
	return b
}

func errResult(status int, errType, msg string) runResult {
	return runResult{
		Status: status,
		Body:   map[string]any{"error": map[string]any{"message": msg, "type": errType}},
	}
}

// errResultWithDetails 在错误体上附加结构化 details（error.details）。
// 仅用于 no_available_account 这类「成因需要程序化判读」的出口；message 仍是
// 完整可读文案，details 只是同一信息的机器可读形态，不携带账号名等敏感信息。
func errResultWithDetails(status int, errType, msg string, details map[string]any) runResult {
	return runResult{
		Status: status,
		Body: map[string]any{"error": map[string]any{
			"message": msg,
			"type":    errType,
			"details": details,
		}},
	}
}

// passthroughBodyWithType 解析上游错误体透传；解析失败时构造兜底错误结构。
func passthroughBodyWithType(text, fallbackType string) any {
	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err == nil {
		return payload
	}
	return map[string]any{"error": map[string]any{
		"message": textPreview(text),
		"type":    fallbackType,
	}}
}

// MessageFromJSON 取业务错误的 msg/message 字段，回退到正文预览。
// 导出给 asyncpool 复用：两条路径对同一份上游错误体必须给出同样的文案。
func MessageFromJSON(text string, raw []byte) string {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err == nil {
		for _, key := range []string{"msg", "message"} {
			if s, ok := payload[key].(string); ok && s != "" {
				return s
			}
		}
	}
	return textPreview(text)
}

func textPreview(text string) string {
	if len(text) > 500 {
		return text[:500]
	}
	return text
}

func hasCaptchaChallengeHeader(header http.Header) bool {
	for _, name := range captchaHeaders {
		if header.Get(name) != "" {
			return true
		}
	}
	return false
}

func orCurrent(modelName string) string {
	if modelName == "" {
		return "當前模型"
	}
	return modelName
}

func orDash(modelName string) string {
	if modelName == "" {
		return "-"
	}
	return modelName
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = cryptoRand.Read(b)
	return fmt.Sprintf("%x", b)
}
