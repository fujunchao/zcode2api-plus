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
	captcha int // 验证码刷新（含首次尝试，上限 MaxCaptchaRetries）
	busy    int // 3010 并发准入等待（上限 len(BusyRetryDelays)）
	rate    int // 瞬时限流原地重试（上限 MaxRateLimitRetries）
}

// RunMessages 执行完整循环。body 是入口 NormalizeBody 后的请求体；
// incomingHeaders 为客户端透传头；deliver 在上游 200 时被调用一次。
func (e *Engine) RunMessages(ctx context.Context, body map[string]any, incomingHeaders map[string]string, deliver DeliverFunc) runResult {
	modelName, _ := body["model"].(string)
	stream := bodyBool(body, "stream")
	reqID := randomHex(3)
	web.Req(reqID, orDash(modelName), stream)

	tried := map[string]bool{}
	for range MaxAccountAttempts {
		acc := e.Store.Select(model.ProviderZai, tried, modelName)
		if acc == nil {
			break
		}
		tried[acc.ID] = true
		res := e.tryAccount(ctx, reqID, acc, body, modelName, stream, incomingHeaders, deliver)
		if res.retrySame {
			continue
		}
		if res.switchAccount {
			continue
		}
		return res.final
	}

	if modelName != "" {
		web.ReqErr(reqID, fmt.Sprintf("模型 %s 無可用帳號（未提供此模型或額度已用完）", modelName))
		return errResult(http.StatusServiceUnavailable, "no_available_account",
			fmt.Sprintf("模型 %s 目前無可用帳號（帳號未提供此模型或額度已用完），請在後台檢查帳號狀態", modelName))
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
) attemptResult {
	needsCaptcha := acc.Mode == "jwt"

	var b attemptBudget
	budget := 1 + MaxCaptchaRetries + len(e.BusyRetryDelays) + MaxRateLimitRetries

	for range budget {
		var verifyParam, verifyRegion string
		if needsCaptcha {
			token, err := e.Captcha.GetVerifyParam(ctx)
			if err != nil {
				e.Captcha.Invalidate()
				b.captcha++
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

		req, err := upstream.BuildRequest(acc, verifyParam, verifyRegion, incomingHeaders)
		if err != nil {
			e.mark(acc, model.StatusInvalid, err.Error())
			web.Warn(reqID, fmt.Sprintf("账号 %s 凭证无效，切换下一个", acc.Name))
			return attemptResult{switchAccount: true}
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, req.URL, bytes.NewReader(payload))
		if err != nil {
			e.mark(acc, model.StatusInvalid, err.Error())
			return attemptResult{switchAccount: true}
		}
		for k, v := range req.Headers {
			httpReq.Header.Set(k, v)
		}

		resp, err := e.clientFor(acc).Do(httpReq)
		if err != nil {
			if isClientGone(ctx) {
				// 客户端已断开（或本请求已被取消）：账号本身没问题，不冷却、也不换号
				// ——换号只会白烧另一个账号的额度。
				web.Warn(reqID, fmt.Sprintf("请求已取消（账号 %s）：%v", acc.Name, err))
				return attemptResult{final: errResult(http.StatusBadGateway, "request_canceled", "请求已取消")}
			}
			e.mark(acc, model.StatusCooling, "连接失败: "+err.Error())
			web.Warn(reqID, fmt.Sprintf("账号 %s 连接失败，切换下一个", acc.Name))
			return attemptResult{switchAccount: true}
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
			buffered, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				e.bumpFail(acc)
				web.ReqErr(reqID, fmt.Sprintf("上游错误体读取失败（账号 %s）", acc.Name))
				return attemptResult{final: errResult(http.StatusBadGateway, "upstream_error", textPreview(err.Error()))}
			}
			res := e.handleUpstreamJSON(reqID, acc, modelName, needsCaptcha, stream, contentType, buffered, &b, deliver)
			if res.retrySame {
				continue
			}
			return res
		}

		// SSE 成功：tee 读取流，交付完整后计入 usage
		return e.deliverStream(reqID, acc, contentType, resp, deliver)
	}

	// 预算耗尽（防御分支：每个 continue 都已各自记账，正常路径不会走到这里）
	if needsCaptcha {
		return attemptResult{final: e.captchaRequired(reqID, "验证码重试次数已耗尽")}
	}
	return attemptResult{switchAccount: true}
}

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
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		if isClientGone(ctx) {
			// 同上的取消语义：读错误体被取消不代表账号有问题。
			web.Warn(reqID, fmt.Sprintf("读上游错误体时请求已取消（账号 %s）：%v", acc.Name, err))
			return attemptResult{final: errResult(http.StatusBadGateway, "request_canceled", "请求已取消")}
		}
		// 读不出错误体：按连接失败处理
		e.mark(acc, model.StatusCooling, "连接失败: "+err.Error())
		return attemptResult{switchAccount: true}
	}
	text := string(body)

	// 1) 验证码挑战（响应头 / code=3007 / F001 与文本仅限 400/403）
	if needsCaptcha && IsCaptchaError(text, resp.StatusCode, resp.Header) {
		e.Captcha.Invalidate()
		b.captcha++
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
		e.mark(acc, model.StatusInvalid, fmt.Sprintf("鉴权失败 HTTP %d", resp.StatusCode))
		web.Warn(reqID, fmt.Sprintf("账号 %s 鉴权失败 %d，切换下一个", acc.Name, resp.StatusCode))
		return attemptResult{switchAccount: true}
	}

	// 3) 402 → 该模型耗尽
	if resp.StatusCode == http.StatusPaymentRequired {
		e.markModelExhausted(acc, modelName, fmt.Sprintf("%s 額度已用完", orCurrent(modelName)))
		web.Warn(reqID, fmt.Sprintf("账号 %s 的 %s 額度用完，切換下一個", acc.Name, orCurrent(modelName)))
		e.fireRefresh(acc)
		return attemptResult{switchAccount: true}
	}

	// 4) 429 且 code=3010：模型并发准入限制，账号仍可用，按原版客户端延迟重试
	if IsModelConcurrencyLimit(resp.StatusCode, text) {
		if b.busy < len(e.BusyRetryDelays) {
			delay := e.BusyRetryDelays[b.busy]
			b.busy++
			web.Warn(reqID, fmt.Sprintf("模型并发准入受限，%g s 后重试（账号仍可用）", delay.Seconds()))
			if !sleepCtx(ctx, delay) {
				return attemptResult{final: errResult(http.StatusServiceUnavailable, "canceled", "请求已取消")}
			}
			return attemptResult{retrySame: true}
		}
		web.Warn(reqID, fmt.Sprintf("模型并发准入持续受限（账号 %s），保留账号状态", acc.Name))
		return attemptResult{final: runResult{
			Status: resp.StatusCode,
			Body:   passthroughBodyWithType(text, "upstream_rate_limit"),
		}}
	}

	// 5) 429：官方用量上限码族 → 该模型耗尽；其余瞬时限流 → 先原地重试，用尽才冷却换号
	if resp.StatusCode == http.StatusTooManyRequests {
		if quotaExhaustedCodes[UpstreamBusinessCode(text)] {
			e.markModelExhausted(acc, modelName, fmt.Sprintf("%s 額度/用量上限已達", orCurrent(modelName)))
			web.Warn(reqID, fmt.Sprintf("账号 %s 的 %s 觸發用量上限，切換下一個", acc.Name, orCurrent(modelName)))
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

	// 6) 503 → 冷却换号
	if resp.StatusCode == http.StatusServiceUnavailable {
		e.bumpFail(acc)
		e.mark(acc, model.StatusCooling, "上游服務不可用 HTTP 503")
		web.Warn(reqID, fmt.Sprintf("账号 %s 上游返回 503，進入冷卻並切換下一個", acc.Name))
		return attemptResult{switchAccount: true}
	}

	// 7) 其余错误：非已知信号，不做账号状态推断，直接原样继承上游响应
	e.bumpFail(acc)
	web.ReqErr(reqID, fmt.Sprintf("上游错误 HTTP %d（账号 %s）", resp.StatusCode, acc.Name))
	return attemptResult{final: runResult{
		Status: resp.StatusCode,
		Body:   passthroughBodyWithType(text, "upstream_error"),
	}}
}

// handleUpstreamJSON 处理 HTTP 200 且 content-type 为 JSON 的响应：
// ZCode 的业务错误有时仍使用 HTTP 200，不能当成 Anthropic 成功回應。
func (e *Engine) handleUpstreamJSON(
	reqID string,
	acc *model.Account,
	modelName string,
	needsCaptcha bool,
	stream bool,
	contentType string,
	buffered []byte,
	b *attemptBudget,
	deliver DeliverFunc,
) attemptResult {
	text := string(buffered)
	code := UpstreamBusinessCode(text)

	switch {
	case code == "1005":
		e.markModelExhausted(acc, modelName, fmt.Sprintf("%s 每日額度已用完", orCurrent(modelName)))
		web.Warn(reqID, fmt.Sprintf("帳號 %s 的 %s 每日額度用完，切換下一個", acc.Name, orCurrent(modelName)))
		e.fireRefresh(acc)
		return attemptResult{switchAccount: true}

	case needsCaptcha && code == "3007":
		e.Captcha.Invalidate()
		b.captcha++
		web.Warn(reqID, fmt.Sprintf("帳號 %s 驗證碼失效，刷新重試（第 %d 次）", acc.Name, b.captcha))
		if b.captcha >= MaxCaptchaRetries {
			return attemptResult{final: e.captchaRequired(reqID, "上游連續拒絕驗證碼")}
		}
		return attemptResult{retrySame: true}

	case code != "" && code != "0":
		e.bumpFail(acc)
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
		e.bumpFail(acc)
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
	return e.finishDelivery(reqID, acc, usage, err)
}

// deliverStream 交付 SSE 流式响应；usage 随读取同步收集，客户端完整接收后计入。
func (e *Engine) deliverStream(reqID string, acc *model.Account, contentType string, resp *http.Response, deliver DeliverFunc) attemptResult {
	e.success(acc)
	usage := NewUsageCollector(strings.Contains(contentType, "text/event-stream"))
	err := deliver(Delivery{
		StatusCode:  resp.StatusCode,
		ContentType: contentType,
		Header:      resp.Header,
		Body:        &teeReader{r: resp.Body, c: usage},
	})
	_ = resp.Body.Close()
	return e.finishDelivery(reqID, acc, usage, err)
}

// finishDelivery 交付收尾：累计 usage 并写日志。
//
// 「能不能计入」的判据是上游有没有交出终值（UsageComplete），不是客户端有没有把流读完。
// zcode 编辑器这类客户端收到 finish_reason 就立刻关流是常态，此时 message_delta 的终值
// 已经到手，用量是准的；若沿用旧的「客户端没读完就不计入」，账号用量会系统性少算
// （线上曾漏计一次 57,352 output token 的生成，详见
// docs/analysis-flash-30min-stream-cut.md §5）。usage 不完整时仍旧不计入。
func (e *Engine) finishDelivery(reqID string, acc *model.Account, usage *UsageCollector, err error) attemptResult {
	if err != nil {
		if usage.UsageComplete() {
			got := e.accumulateUsage(acc, usage)
			web.ReqErr(reqID, fmt.Sprintf("流传输中断，上游 usage 已完整，仍计入 %d tok: %v", got.Output, err))
		} else {
			web.ReqErr(reqID, fmt.Sprintf("流传输中断: %v", err))
		}
		return attemptResult{final: runResult{Delivered: true}}
	}
	usage.Finish()
	web.ReqOk(reqID, e.accumulateUsage(acc, usage).Output)
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

// MarkAccount 设置账号状态；status 为 cooling 时按配置写入冷却截止时间。
//
// 按 provider + ID 在 store 锁内改「当前」对象：调用方持有的是账号副本，直接改它
// 既不落库、也会与并发读写相撞；账号已不存在时静默跳过（可能刚被后台删除）。
func MarkAccount(st *store.Store, provider, idOrName, status, errMsg string, now time.Time) {
	_, _ = st.Update(provider, idOrName, func(acc *model.Account) {
		acc.Status = status
		acc.LastError = &errMsg
		if status == model.StatusCooling {
			until := float64(now.Add(time.Duration(config.CoolingSeconds)*time.Second).UnixNano()) / 1e9
			acc.CoolingUntil = &until
		}
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
func MarkModelExhausted(st *store.Store, provider, idOrName string, modelName any, errMsg string) {
	_, _ = st.Update(provider, idOrName, func(acc *model.Account) {
		// 先记额度事实（与账号状态无关，任何情况下都要落库）。
		marked := acc.MarkModelExhausted(modelName)
		strong := isStrongStatus(acc.Status)
		if !marked {
			if !strong {
				acc.Status = model.StatusExhausted
			}
			acc.LastError = &errMsg
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
		acc.LastError = &errMsg
	})
}

func (e *Engine) mark(acc *model.Account, status, errMsg string) {
	MarkAccount(e.Store, acc.Provider, acc.ID, status, errMsg, e.now())
}

func (e *Engine) markModelExhausted(acc *model.Account, modelName any, errMsg string) {
	MarkModelExhausted(e.Store, acc.Provider, acc.ID, modelName, errMsg)
}

// markRateLimited 瞬时限流的递进冷却，返回（本次冷却秒数, 累加后的连续次数）。
//
// 连续次数必须取返回值：acc 是 Select 交出的账号副本，MarkRateLimited 更新的是
// store 内的 live 对象，读 acc.RateLimitStreak 只会拿到旧值（线上曾把「第 1 次」
// 打成「第 0 次」；asyncpool 侧一直用的就是返回值）。
func (e *Engine) markRateLimited(acc *model.Account, errMsg string) (int, int) {
	return MarkRateLimited(e.Store, acc.Provider, acc.ID, errMsg, e.now())
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
		ResetRateLimitStreak(live)
		if live.Status == model.StatusCooling || live.Status == model.StatusExhausted {
			live.Status = model.StatusActive
		}
	})
	e.fireRefresh(acc)
}

func (e *Engine) bumpFail(acc *model.Account) {
	_, _ = e.Store.Update(acc.Provider, acc.ID, func(live *model.Account) {
		live.FailCount++
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

// teeReader 在读取时同步餵入 usage 收集器。
type teeReader struct {
	r io.Reader
	c *UsageCollector
}

func (t *teeReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		t.c.Feed(p[:n])
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
