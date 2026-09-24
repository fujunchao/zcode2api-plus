// Package asyncpool Async 空闲池路由（POST /async/v1/messages）：
// ticket + SSE keepalive + 等待 ready + 转发响应，仅支持 OAuth (JWT) 账号。
// 对应 Python 版 app/routes/async_pool.py；泄漏防护三件套（SSE 退出释放 +
// 中止后台任务、孤儿 ticket 建票时清扫）与流中断终止语义逐一对齐。
package asyncpool

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"zcode2api/internal/auth"
	"zcode2api/internal/captcha"
	"zcode2api/internal/config"
	"zcode2api/internal/gateway"
	"zcode2api/internal/model"
	"zcode2api/internal/proxy"
	"zcode2api/internal/store"
	"zcode2api/internal/upstream"
	"zcode2api/internal/web"
)

// ticketSweepGrace 孤儿 ticket 清扫的宽限期（秒）。
const ticketSweepGrace = 60 * time.Second

// keepaliveInterval SSE 静默期心跳间隔。
const keepaliveInterval = 10 * time.Second

// ticketEvent 后台任务投递给 SSE 流的事件（对齐 Python queue 里的 dict）。
type ticketEvent struct {
	Type string // ready | chunk | done | error
	Data any    // chunk / error 的负载
}

// ticket 单个异步请求的票务状态。
type ticket struct {
	status    string
	body      map[string]any
	queue     chan ticketEvent
	createdAt time.Time
	cancel    context.CancelFunc // 中止后台任务（释放票务时调用）

	// shortID 6 位十六进制短标识，与 sync 路径的 reqID 同构。async 此前只用 36 字符
	// ticketID，与 sync 的 6 hex 风格不一致，排查时无法用同一套检索习惯对照两条路径。
	// 诊断行同时带 reqID=<shortID> 与 ticket=<ticketID>，两种检索方式都不破坏。
	shortID string
}

// Pool Async 空闲池：票务存储 + 入口路由 + 后台处理。
type Pool struct {
	Store   *store.Store
	Auth    *auth.Service
	Captcha *captcha.Manager
	Client  *http.Client // nil 时使用与网关一致的上游客户端

	// RateLimitRetryDelay 瞬时限流原地重试的等待时长（默认 gateway.RateLimitRetryDelay；测试可置 0）。
	RateLimitRetryDelay time.Duration
	// OverloadRetryDelays 平台过载（529 / 业务码 1305）的原地退避档位，
	// 默认 gateway.OverloadRetryDelays（1s/3s；测试可缩短）。与 RateLimitRetryDelay
	// 分开配置：过载与账号无关，既不冷却也不换号。
	OverloadRetryDelays []time.Duration

	// OnQuotaRefresh 上游侧断流后的额度刷新钩子（main 接 qs.FetchQuota，与
	// gateway.Engine.OnQuotaRefresh 同形）。断流账号的额度快照停在旧值，会以
	// 「幽灵最富」持续黏住选号（2026-09-22 事故）；刷新让下一次选号回到真实读数。
	// nil 时跳过（测试默认）。
	OnQuotaRefresh func(*model.Account)

	mu      sync.Mutex
	tickets map[string]*ticket
}

// NewPool 创建空闲池。
func NewPool(st *store.Store, au *auth.Service, cm *captcha.Manager) *Pool {
	return &Pool{
		Store:               st,
		Auth:                au,
		Captcha:             cm,
		RateLimitRetryDelay: gateway.RateLimitRetryDelay,
		OverloadRetryDelays: gateway.OverloadRetryDelays,
		tickets:             map[string]*ticket{},
	}
}

// Register 在 mux 上注册异步路由（调用方按 config.AsyncEnabled 决定是否挂载，
// 对齐 Python 的条件 include_router；端点内的 503 检查作为运行期兜底保留）。
func (p *Pool) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /async/v1/messages", p.handleAsyncMessages)
}

// handleAsyncMessages 创建 async ticket 并 SSE 等待结果。
func (p *Pool) handleAsyncMessages(w http.ResponseWriter, r *http.Request) {
	if e := p.Auth.VerifyGatewayKey(r); e != nil {
		writeJSONStatus(w, e.Status, map[string]any{"detail": e.Message})
		return
	}

	if !config.AsyncEnabled {
		writeJSONStatus(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]any{"message": "Async 路由未启用", "type": "feature_disabled"},
		})
		return
	}

	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"message": "请求体不是合法 JSON", "type": "invalid_request"},
		})
		return
	}

	// 入口整形一次，与 /v1/messages 一致（去掉 provider/ 前缀、套用名称映射）。
	// 缺了它，`anthropic/GLM-5.3` 这类写法在前者能过、在这里 400，
	// 与「模型白名单与 /v1/messages 一致」的承诺不符；票内 model 名若不归一，
	// Select 的模型分档与额度比对也会用错键。
	// 传 false：system 注入不幂等，必须留在每账号副本上（见 processTicket）。
	gateway.NormalizeBody(body, false)

	// 模型白名單與 /v1/messages 一致：僅開放清單內模型，其餘在建票前一律拒絕
	if !gateway.ModelAllowed(body["model"]) {
		modelName := anyToString(body["model"])
		web.Warn("async", fmt.Sprintf("模型 %s 不在開放清單內，拒絕建票", modelName))
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{
				"message": fmt.Sprintf("模型 %s 不在可用清單內，僅支持 %s", modelName, strings.Join(gateway.AvailableModels, ", ")),
				"type":    "model_not_allowed",
			},
		})
		return
	}

	ticketID := p.newTicket(body)

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	p.streamTicket(r.Context(), func(s string) error {
		if _, err := io.WriteString(w, s); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}, ticketID)
}

// streamTicket SSE keepalive + 等待结果；退出时（含客户端断开）释放 ticket
// 并中止后台任务。write 负责写出并 flush；返回错误即视为连接已断开。
func (p *Pool) streamTicket(ctx context.Context, write func(string) error, ticketID string) {
	tk := p.getTicket(ticketID)
	if tk == nil {
		_ = write("event: error\ndata: " + sseJSON(map[string]any{"error": "ticket not found"}) + "\n\n")
		return
	}
	defer p.releaseTicket(ticketID)

	send := func(event, data string) error {
		if event == "chunk" {
			return write("data: " + data + "\n\n")
		}
		return write("event: " + event + "\ndata: " + data + "\n\n")
	}

	deadline := tk.createdAt.Add(time.Duration(config.AsyncTicketTimeout) * time.Second)
	if err := send("ticket", sseJSON(map[string]any{"id": ticketID, "status": "pending"})); err != nil {
		return
	}
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			// 客户端断开：finally 是唯一必经的清理点（defer releaseTicket）
			return
		case ev, ok := <-tk.queue:
			if !ok {
				return
			}
			switch ev.Type {
			case "ready":
				if err := send("ready", sseJSON(map[string]any{"status": "processing"})); err != nil {
					return
				}
			case "chunk":
				if err := send("chunk", sseJSON(ev.Data)); err != nil {
					return
				}
			case "done":
				_ = send("done", "{}")
				return
			case "error":
				_ = send("error", sseJSON(ev.Data))
				return
			}
		case <-time.After(keepaliveInterval):
			if err := write(": keepalive\n\n"); err != nil {
				return
			}
		}
	}
	// 逾时：必须显式投递终止事件，否则客户端只看到连接关闭而无法区分
	// 「已完成」与「被超时截断」（defer releaseTicket 会中止后台任务）。
	_ = send("error", sseJSON(map[string]any{"error": map[string]any{
		"message": fmt.Sprintf("请求超时（%d 秒未完成）", config.AsyncTicketTimeout),
		"type":    "ticket_timeout",
	}}))
}

// ── 票务生命周期 ────────────────────────────────────────────────────────────

// newTicket 创建 ticket 并启动后台任务，返回 ticket_id。
func (p *Pool) newTicket(body map[string]any) string {
	p.sweepExpiredTickets()
	ticketID := newUUID()
	ctx, cancel := context.WithCancel(context.Background())
	tk := &ticket{
		status:    "pending",
		body:      body,
		queue:     make(chan ticketEvent, 256),
		createdAt: time.Now(),
		cancel:    cancel,
		shortID:   newShortID(),
	}
	p.mu.Lock()
	p.tickets[ticketID] = tk
	p.mu.Unlock()
	go p.processTicket(ctx, ticketID)
	return ticketID
}

// getTicket 读取票务（并发安全）。
func (p *Pool) getTicket(ticketID string) *ticket {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tickets[ticketID]
}

// releaseTicket 移除 ticket 并中止仍在运行的后台任务。
// 客户端断开或放弃后继续请求上游只会白耗账号额度，后台任务一并取消；
// 任务已自然结束时 cancel 是无操作（对齐 Python cancel 语义）。
func (p *Pool) releaseTicket(ticketID string) {
	p.mu.Lock()
	tk := p.tickets[ticketID]
	delete(p.tickets, ticketID)
	p.mu.Unlock()
	if tk != nil && tk.cancel != nil {
		tk.cancel()
	}
}

// sweepExpiredTickets 清扫超过生命周期的孤儿 ticket（如客户端在建票后、
// SSE 启动前消失）。正常退出由 streamTicket 的 defer 负责清理；此处按
// 建票时间加宽限兜底。
func (p *Pool) sweepExpiredTickets() {
	cutoff := time.Now().Add(-time.Duration(config.AsyncTicketTimeout)*time.Second - ticketSweepGrace)
	p.mu.Lock()
	var stale []string
	for id, tk := range p.tickets {
		if tk.createdAt.Before(cutoff) {
			stale = append(stale, id)
		}
	}
	p.mu.Unlock()
	for _, id := range stale {
		p.releaseTicket(id)
	}
}

// emit 投递事件；票务已释放或上下文取消时返回 false（调用方应停止工作）。
func (p *Pool) emit(ctx context.Context, ticketID string, ev ticketEvent) bool {
	tk := p.getTicket(ticketID)
	if tk == nil {
		return false
	}
	select {
	case tk.queue <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

// ── 后台任务 ────────────────────────────────────────────────────────────────

// processTicket 后台执行 ticket 请求。JWT 走主网关同一套验证码续期；
// 429/503 与网络异常换号重试（指数退避 2^n），已发 chunk 的流中断终止票务。
func (p *Pool) processTicket(ctx context.Context, ticketID string) {
	tk := p.getTicket(ticketID)
	if tk == nil {
		return
	}
	body := tk.body
	modelName, _ := body["model"].(string)
	// 观测诊断行：每票一条，与 sync 路径同口径（复用 gateway.ReqDiag）。
	// 账号/线路要等 Select 之后才可得，故数据集中在收尾期输出。
	diag := gateway.NewReqDiag(tk.shortID, modelName, true, body, time.Now)
	diag.Ticket = ticketID
	// 风控判定范围：**必须按票创建**（Pool 跨票共享，挂上去会让并发票务互相误判）。
	// 与 sync 路径共用 gateway.RiskScope：同一份 body 在两条路径上必须得到同一判定。
	//
	// 刻意与 diag 一起声明在 defer 之前：诊断行要排印本票的风控范围，而 defer 里的
	// 闭包只能引用它之前已声明的变量。
	scope := gateway.NewRiskScope()
	defer func() {
		diag.Finish(time.Now())
		// 与 sync 同口径：风控范围取收尾期的最终值（命中的不同账号数，≥2 即请求级）。
		diag.SetRiskAccounts(scope.Accounts())
		web.Diag(diag.ReqID, diag.Format())
	}()
	// 归因标识**按票算一次**：出站头与 body 的 metadata.user_id.session_id 必须共享
	// 同一个会话 id（官方恒等），两处各算一次必然分叉。换号重试沿用同一份 ——
	// 它仍是同一张票。async 没有下游头可沿用，会话 id 只能合成（见 NewAttribution）。
	attr := upstream.NewAttribution(nil, upstream.MetadataSessionID(body))

	retries := 0
	announcedReady := false
	tried := map[string]bool{}

	for {
		// async 仅支持 JWT 账号，但池中可以混有 apiKey 账号：Select 是 round-robin，
		// 轮到 apiKey 账号时必须跳过并继续找下一个，而不是直接终止整张票——否则池里
		// 明明有可用 JWT 账号，请求却与账号状态无关地间歇性失败。
		// 先记 tried 再判断：漏记会让下一次轮询又选中同一账号。
		acc := p.Store.Select(model.ProviderZai, tried, modelName)
		for acc != nil && acc.Mode != "jwt" {
			tried[acc.ID] = true
			acc = p.Store.Select(model.ProviderZai, tried, modelName)
		}
		if acc == nil {
			// 与 sync 路径同口径：选不出号要如实说明「为什么」——冷却/风控/停用
			// 都会走到这里，别让静态文案把它误读成绑定/额度问题（2026-09-20 事故）。
			stat := p.Store.PoolStats(model.ProviderZai, modelName)
			msg := "无可用 OAuth 账号"
			if text := gateway.PoolStatusText(stat); text != "" {
				msg += "（" + text + "）"
			}
			p.emitErrorWithDetails(ctx, ticketID, msg, "no_account",
				gateway.PoolStatusDetails(stat, modelName))
			return
		}
		tried[acc.ID] = true
		diag.Attempts++
		diag.AccName = acc.Name
		// 出口线路标签由 clientFor 一手交出（只有它知道代理是否真的生效、是否被
		// async_force_direct 强制置空），此处不另起判据——两套判据必然漂移，
		// 而「线路侧 vs 上游侧」的归因正是靠 route 读数判定的，读反即结论反。

		// 每个账号在副本上注入 zcode_system（NormalizeBody 的 system 注入不幂等）
		actualBody := shallowCopyBody(body)
		gateway.NormalizeBody(actualBody, true)
		// 内容视图先取（注入账号身份之前）：与 sync 路径同口径，见 ReqDiag.SetBody。
		contentView, _ := marshalJSON(actualBody)
		// 与 sync 路径同口径：设备身份走 body 的 metadata.user_id（本账号指纹）。
		upstream.InjectDeviceMetadata(actualBody, acc.DeviceMidOr(config.DeviceMid()), attr.SessionID)
		payload, err := marshalJSON(actualBody)
		if err != nil {
			p.emitError(ctx, ticketID, "请求体序列化失败", "build_error")
			return
		}
		diag.SetBody(payload, contentView)

		networkRetry := false
		lastNetworkError := ""
		for attempt := range gateway.MaxCaptchaRetries {
			var verifyParam, verifyRegion string
			token, err := p.Captcha.GetVerifyParam(ctx)
			if err != nil {
				p.Captcha.Invalidate()
				if attempt+1 < gateway.MaxCaptchaRetries {
					web.Warn(ticketID, fmt.Sprintf("验证码自动求解失败，刷新令牌重试（第 %d 次）", attempt+1))
					continue
				}
				p.emitError(ctx, ticketID, fmt.Sprintf("自动验证码暂时失败: %s", err.Error()), "captcha_required")
				return
			}
			if token != nil {
				verifyParam, verifyRegion = token.VerifyParam, token.Region
			}

			req, err := upstream.BuildRequestWithAttribution(acc, verifyParam, verifyRegion, nil, attr)
			if err != nil {
				p.emitError(ctx, ticketID, err.Error(), "build_error")
				return
			}

			if !announcedReady {
				if !p.emit(ctx, ticketID, ticketEvent{Type: "ready"}) {
					return
				}
				announcedReady = true
			}

			midStream, streamErr := p.attemptUpstream(ctx, ticketID, acc, modelName, req, payload, diag, scope)
			if midStream {
				// 已向客户端发出内容块，不能换号重发（会收到重复事件），终止本票
				web.Warn(ticketID, fmt.Sprintf("流转发中断: %s", streamErr.Error()))
				p.emitError(ctx, ticketID, fmt.Sprintf("上游流式响应中断: %s", streamErr.Error()), "upstream_stream_interrupted")
				return
			}
			if streamErr == nil {
				return // 200 流已完整转发并投递 done
			}
			// 上游拒绝验证码：同一账号换令牌重试，次数与求解失败共享内层循环
			//（对齐 Python _is_captcha_error 分支的 continue）
			if errors.Is(streamErr, errCaptchaRejected) {
				if attempt+1 < gateway.MaxCaptchaRetries {
					web.Warn(ticketID, fmt.Sprintf("账号 %s 验证码失效，刷新重试（第 %d 次）", acc.Name, attempt+1))
					continue
				}
				p.emitError(ctx, ticketID, "上游连续拒绝验证码", "captcha_required")
				return
			}
			// 其余错误已作为 error 事件投递给客户端，后台任务直接结束
			if errors.Is(streamErr, errDelivered) {
				return
			}
			// 请求级风控：不得换号。上游原文交给客户端后终止本票——与 errDelivered 同一条
			// 出路，区别是这条还要把上游错误体投递出去（errDelivered 表示已经投递过）。
			var reqLevelRisk errRequestLevelRisk
			if errors.As(streamErr, &reqLevelRisk) {
				p.emitError(ctx, ticketID, reqLevelRisk.body, "upstream_error")
				return
			}
			if ctx.Err() != nil {
				return // 票务已被释放，无需继续
			}
			web.Warn(ticketID, "请求失败: "+streamErr.Error())
			networkRetry = true
			lastNetworkError = streamErr.Error()
			break
		}

		if !networkRetry {
			// 验证码重试循环耗尽（防御分支，对齐 Python for-else）
			p.emitError(ctx, ticketID, "上游连续拒绝验证码", "captcha_required")
			return
		}

		retries++
		// 换号上限在这里，不在别处：这个计数同时被「网络错误换号」与「风控/503 冷却换号」
		// 共用，所以一票最多试 config.AsyncMaxRetries+1 个账号——sync 侧的对应上限是
		// gateway.MaxAccountAttempts，两者数值不同步是刻意的（async 允许更多次，因为它
		// 没有客户端在等），但**语义必须一致**：请求级风控在这条路径上同样不得换号。
		if retries > config.AsyncMaxRetries {
			msg := lastNetworkError
			if msg == "" {
				msg = "重试次数耗尽"
			}
			p.emitError(ctx, ticketID, msg, "max_retries")
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(1<<retries) * time.Second):
		}
	}
}

// emitError 投递 error 事件（票务已释放时静默丢弃）。
func (p *Pool) emitError(ctx context.Context, ticketID, message, errType string) {
	p.emit(ctx, ticketID, ticketEvent{
		Type: "error",
		Data: map[string]any{"error": map[string]any{"message": message, "type": errType}},
	})
}

// emitErrorWithDetails 在错误事件上附加结构化 details（error.details）。
// 与 sync 路径的 errResultWithDetails 同一形态：文案给人看、结构给程序看。
func (p *Pool) emitErrorWithDetails(ctx context.Context, ticketID, message, errType string, details map[string]any) {
	body := map[string]any{"message": message, "type": errType}
	if details != nil {
		body["details"] = details
	}
	p.emit(ctx, ticketID, ticketEvent{
		Type: "error",
		Data: map[string]any{"error": body},
	})
}

// attemptUpstream 发起一次上游请求，并处理两类「不换号先归地重试」的失败：
// 瞬时限流（非额度码族的 429，用尽后递进冷却并交外层换号）与平台过载
// （529 / 业务码 1305，用尽后原样投递上游错误体并终止本票）。
// 两类各有独立预算，互不挤占；次数与延迟都取自 gateway，两条路径不会各自漂移。
//
// scope 是本票的风控判定范围：请求级风控（errRequestLevelRisk）不在此处理，直接上抛给
// processTicket 终止本票——它不是「换号能解决」的错误，重试语义在这里就不该成立。
func (p *Pool) attemptUpstream(
	ctx context.Context,
	ticketID string,
	acc *model.Account,
	modelName string,
	req upstream.Request,
	payload []byte,
	diag *gateway.ReqDiag,
	scope *gateway.RiskScope,
) (bool, error) {
	rateAttempt, overloadAttempt := 0, 0
	for {
		midStream, err := p.attemptUpstreamOnce(ctx, ticketID, acc, modelName, req, payload, diag, scope)
		if err == nil || midStream {
			return midStream, err
		}

		// 平台过载：与账号无关，既不标状态也不换号。换号救不了过载，只会在平台
		// 已经拥塞时替它放大流量；冷却更糟，一次过载就能把整池健康账号踢出去。
		var ol errOverloaded
		if errors.As(err, &ol) {
			if overloadAttempt >= len(p.OverloadRetryDelays) {
				p.emitError(ctx, ticketID, ol.body, "upstream_error")
				return false, errDelivered
			}
			delay := gateway.JitteredDelay(p.OverloadRetryDelays[overloadAttempt])
			overloadAttempt++
			web.Warn(ticketID, fmt.Sprintf("上游平台过载，%g s 后原地重试（账号状态不变）", delay.Seconds()))
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-time.After(delay):
			}
			continue
		}

		var rl errRateLimited
		if !errors.As(err, &rl) {
			return false, err
		}
		if rateAttempt >= gateway.MaxRateLimitRetries {
			secs, streak := gateway.MarkRateLimited(p.Store, acc.Provider, acc.ID, "上游限流 HTTP 429", time.Now())
			web.Warn(ticketID, fmt.Sprintf("账号 %s 连续第 %d 次被限流，冷却 %d s 后切换下一个",
				acc.Name, streak, secs))
			return false, errNetwork{rl.body}
		}
		rateAttempt++
		delay := gateway.JitteredDelay(p.RateLimitRetryDelay)
		web.Warn(ticketID, fmt.Sprintf("账号 %s 被瞬时限流 429，%g s 后原地重试", acc.Name, delay.Seconds()))
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(delay):
		}
	}
}

// attemptUpstreamOnce 发起一次上游请求并处理响应（瞬时限流不在此重试）。
// 返回 (midStream, err)：
//   - 200 流完整转发（done 已投递）→ (false, nil)；
//   - 已发出过 chunk 后流中断 → (true, err)（调用方终止票务，不得重试）；
//   - 一个 chunk 都没发过时按普通网络错误 → (false, err)（调用方换号重试）。
//
// 非 200 的错误分类（验证码 / 429+503 冷却 / 其余原样透传）在此内联处理，
// 与 Python 版 _process_ticket 的分支顺序一致。
func (p *Pool) attemptUpstreamOnce(
	ctx context.Context,
	ticketID string,
	acc *model.Account,
	modelName string,
	req upstream.Request,
	payload []byte,
	diag *gateway.ReqDiag,
	scope *gateway.RiskScope,
) (bool, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, req.URL, strings.NewReader(string(payload)))
	if err != nil {
		return false, err
	}
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}

	client, route := p.clientFor(acc)
	if diag != nil {
		// 首字节延迟与耗时的基准点：紧贴出站调用（与 sync 路径同）。
		diag.UpstreamStart = diag.Now()
		// 如实报本次实际出口（direct / 线路名 / proxy:掩码URL），与 sync 的
		// Store.ProxyLabel 同口径。route 由 clientFor 一并交出，保证读数与实际
		// 出口不可能不一致。
		diag.Route = route
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if diag != nil {
		diag.Status = resp.StatusCode
	}

	if resp.StatusCode != http.StatusOK {
		// 限长：错误体要整段读进内存做分类，上游或代理异常时可能回一个任意大的
		// body。正常路径是流式转发，不受此限。
		text, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		if readErr != nil {
			return false, readErr
		}
		bodyText := string(text)

		// 分类顺序与 engine.handleUpstreamError 一致（PLAN §5.2），
		// 同一账号在两条路径下必须标出相同状态。
		if gateway.IsCaptchaError(bodyText, resp.StatusCode, resp.Header) {
			// 验证码被拒：令牌作废，由调用方在内层循环内换令牌重试
			p.Captcha.Invalidate()
			gateway.RecordAccountError(p.Store, acc.Provider, acc.ID, model.ErrorKindCaptchaFailed,
				fmt.Sprintf("上游拒絕驗證碼 HTTP %d", resp.StatusCode), time.Now())
			return false, errCaptchaRejected
		}

		// 401/403 → 账号失效，换号
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			gateway.MarkAccount(p.Store, acc.Provider, acc.ID, model.StatusInvalid, model.ErrorKindAuthFailed,
				fmt.Sprintf("鉴权失败 HTTP %d", resp.StatusCode), time.Now())
			web.Warn(ticketID, fmt.Sprintf("账号 %s 鉴权失败 %d，切换下一个%s", acc.Name, resp.StatusCode, gateway.ErrorDetail(bodyText)))
			return false, errNetwork{bodyText}
		}

		// 402 → 该模型额度用完
		if resp.StatusCode == http.StatusPaymentRequired {
			gateway.MarkModelExhausted(p.Store, acc.Provider, acc.ID, modelName,
				fmt.Sprintf("%s 額度已用完", orCurrent(modelName)), time.Now())
			web.Warn(ticketID, fmt.Sprintf("账号 %s 的 %s 額度用完，切換下一個%s", acc.Name, orCurrent(modelName), gateway.ErrorDetail(bodyText)))
			return false, errNetwork{bodyText}
		}

		// 429 且 code=3010：模型并发准入限制，账号仍可用，不标状态
		if gateway.IsModelConcurrencyLimit(resp.StatusCode, bodyText) {
			gateway.RecordAccountError(p.Store, acc.Provider, acc.ID, model.ErrorKindModelBusy,
				"模型并发准入受限 HTTP 429 code=3010", time.Now())
			web.Warn(ticketID, fmt.Sprintf("账号 %s 模型并发准入受限，保留账号状态%s", acc.Name, gateway.ErrorDetail(bodyText)))
			return false, errNetwork{bodyText}
		}

		// 529 / 业务码 1305：平台服务过载，与账号无关。必须排在 429 分支之前——官方
		// 错误码表把 1305 标成 429，若先过 429 分支，一次平台过载就会被当成「该账号
		// 被限速」而挨上递进冷却，整池健康账号会被逐个踢出调度。
		// 不标状态、不换号，交由 attemptUpstream 按 OverloadRetryDelays 原地退避重试；
		// 用尽后原样投递上游错误体并终止本票（与 sync 路径的「原样透传」严格对齐）。
		if gateway.IsUpstreamOverload(resp.StatusCode, bodyText) {
			// 账号状态、冷却、失败计数都不动，但这次失败要留痕（与 sync 路径同）。
			code := gateway.UpstreamBusinessCode(bodyText)
			if code == "" {
				code = "-"
			}
			gateway.RecordAccountError(p.Store, acc.Provider, acc.ID, model.ErrorKindUpstreamOverload,
				fmt.Sprintf("上游平台过载 HTTP %d code=%s", resp.StatusCode, code), time.Now())
			return false, errOverloaded{bodyText}
		}

		// 429：额度上限码族 → 该模型耗尽
		if resp.StatusCode == http.StatusTooManyRequests {
			if gateway.IsQuotaExhaustedCode(bodyText) {
				gateway.MarkModelExhausted(p.Store, acc.Provider, acc.ID, modelName,
					fmt.Sprintf("%s 額度/用量上限已達", orCurrent(modelName)), time.Now())
				web.Warn(ticketID, fmt.Sprintf("账号 %s 的 %s 觸發用量上限，切換下一個%s", acc.Name, orCurrent(modelName), gateway.ErrorDetail(bodyText)))
				return false, errNetwork{bodyText}
			}
			// 其余为瞬时限流：此处不标状态，交由 attemptUpstream 决定原地重试还是冷却
			return false, errRateLimited{body: bodyText}
		}

		// 503 → 按连续次数递进冷却换号（与 sync 路径同一 MarkUpstreamUnavailable，
		// 默认 30/60/120s 可后台配置，超阶梯封顶 CoolingSeconds）。日志带 body 预览。
		if resp.StatusCode == http.StatusServiceUnavailable {
			errMsg := "上游服務不可用 HTTP 503: " + gateway.ErrorPreview(bodyText)
			secs, streak := gateway.MarkUpstreamUnavailable(p.Store, acc.Provider, acc.ID, errMsg, time.Now())
			web.Warn(ticketID, fmt.Sprintf("账号 %s 上游返回 503（連續第 %d 次），冷卻 %d s 並切換下一個: %s",
				acc.Name, streak, secs, gateway.ErrorPreview(bodyText)))
			return false, errNetwork{bodyText}
		}

		// 405 + 风控文案：先分清「账号级」还是「请求级」，与 sync 路径同语义（项目硬不变式：
		// 两条路径不得对同一份 body 给出不同判定）。
		//
		// 账号级（换号即成功）→ 整号冷却（按连续命中递进，超限置失效）并换号。
		// 请求级（同一 body 在 ≥2 个不同账号上都被拒）→ 身份不是变量、剩下的只有请求体，
		// 换号只是把健康账号送出去挨打：改为不冷却（并回滚本票已施加的冷却），把上游原文
		// 终止性投递给客户端。判据与阈值见 gateway.RiskScope。
		//
		// 判定必须先看 body：405 也可能是计费接口的重复查询，或者我方缺 system 注入时上游
		// 回的错（那种情况每个账号都会一样失败，冷却账号等于把代码缺陷变成账号惩罚）。
		if resp.StatusCode == http.StatusMethodNotAllowed && model.IsRiskControlBody(bodyText) {
			preview := gateway.ErrorPreview(bodyText)
			if scope.Verdict(acc) {
				restored, skipped := scope.Rollback(p.Store)
				gateway.RecordAccountError(p.Store, acc.Provider, acc.ID, model.ErrorKindRiskControl,
					"上游风控拦截 HTTP 405（請求級）: "+preview, time.Now())
				web.Warn(ticketID, fmt.Sprintf(
					"风控判定為請求級（已在 %d 個帳號上復現，HTTP %d，%s），停止換號並回滾冷卻（回滾 %d、跳過 %d）",
					scope.Accounts(), resp.StatusCode, preview, restored, skipped))
				return false, errRequestLevelRisk{body: bodyText}
			}
			_, streak, invalid, snap := gateway.MarkRiskControlWithSnapshot(p.Store, acc.Provider, acc.ID,
				"上游风控拦截 HTTP 405: "+preview, time.Now())
			scope.Record(snap)
			if invalid {
				web.Warn(ticketID, fmt.Sprintf("账号 %s 连续第 %d 次命中风控（HTTP %d，%s），已置為失效待人工處理",
					acc.Name, streak, resp.StatusCode, preview))
			} else {
				web.Warn(ticketID, fmt.Sprintf("账号 %s 第 %d 次命中风控（HTTP %d，%s），進入冷卻並切換下一個",
					acc.Name, streak, resp.StatusCode, preview))
			}
			return false, errNetwork{bodyText}
		}

		// 其余错误：原样回传上游错误体，终止本票
		p.bumpFail(acc, model.ErrorKindUpstreamError)
		// 日志带上业务码与截断预览（与 sync 路径同）：只记状态码时，关键信息在 body
		// 里的失败（1305 平台过载、风控 405）在日志里完全不可见。
		web.ReqErr(ticketID, fmt.Sprintf("上游错误 HTTP %d（账号 %s）: %s",
			resp.StatusCode, acc.Name, gateway.ErrorPreview(bodyText)))
		p.emit(ctx, ticketID, ticketEvent{
			Type: "error",
			Data: map[string]any{"error": map[string]any{"message": bodyText, "type": "upstream_error"}},
		})
		return false, errDelivered
	}

	// 200 未必是成功：上游会用 200 包装额度耗尽一类的业务错误（如 code=1005）。
	// 真正的流式响应是 text/event-stream，命中 JSON 说明这不是流——此时若直接
	// 转发 SSE，客户端拿到的是空流，账号也不会被标任何状态、下次仍会被选中。
	if strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		return p.handleUpstreamJSON(ctx, ticketID, acc, modelName, resp)
	}

	// 200：转发 SSE；转发开始后中断按 chunk 计数区分两种出路
	return p.forwardSSE(ctx, ticketID, resp, acc, diag)
}

// handleUpstreamJSON 处理「200 + JSON」的上游响应。分类口径与同步路径
// （gateway.handleUpstreamJSON）逐条对齐——同一账号在两条路径下必须标出相同状态，
// 否则同一次额度耗尽在 sync 侧被记为「模型耗尽」、在 async 侧却被当成成功。
func (p *Pool) handleUpstreamJSON(
	ctx context.Context,
	ticketID string,
	acc *model.Account,
	modelName string,
	resp *http.Response,
) (bool, error) {
	// 同样限长：这是「200 但为 JSON」的异常分支，不是正常流式响应。
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	if err != nil {
		return false, errNetwork{err.Error()}
	}
	text := string(raw)
	switch code := gateway.UpstreamBusinessCode(text); {
	case code == "1005":
		// 每日额度耗尽：标该模型耗尽后换号（与 sync 同）。
		gateway.MarkModelExhausted(p.Store, acc.Provider, acc.ID, modelName,
			fmt.Sprintf("%s 每日額度已用完", orCurrent(modelName)), time.Now())
		web.Warn(ticketID, fmt.Sprintf("账号 %s 的 %s 每日額度用完，切換下一個%s", acc.Name, orCurrent(modelName), gateway.ErrorDetail(text)))
		return false, errNetwork{text}

	case code == "3007":
		// 验证码失效：令牌作废，由调用方换令牌重试（不换号）。
		p.Captcha.Invalidate()
		gateway.RecordAccountError(p.Store, acc.Provider, acc.ID, model.ErrorKindCaptchaFailed,
			"上游拒絕驗證碼 code=3007", time.Now())
		return false, errCaptchaRejected

	case code != "" && code != "0":
		p.bumpFail(acc, model.ErrorKindUpstreamError)
		p.emit(ctx, ticketID, ticketEvent{
			Type: "error",
			Data: map[string]any{"error": map[string]any{
				"message": gateway.MessageFromJSON(text, raw),
				"type":    "upstream_error",
				"code":    code,
			}},
		})
		return false, errDelivered
	}

	// 业务码缺失 / 为 0：本票据承诺的是 SSE 流，上游却回了 JSON 正文。
	// 当作成功转发会破坏 chunk 契约（同步路径同样判为无效流），故投递错误后终止。
	p.bumpFail(acc, model.ErrorKindInvalidResponse)
	p.emit(ctx, ticketID, ticketEvent{
		Type: "error",
		Data: map[string]any{"error": map[string]any{
			"message": "上游未返回有效的 SSE 串流",
			"type":    "invalid_upstream_response",
		}},
	})
	return false, errDelivered
}

// errCaptchaRejected 上游拒绝验证码：调用方在内层循环内换令牌重试（不换号）。
var errCaptchaRejected = errors.New("上游拒绝验证码")

// orCurrent 模型名为空时的占位文案（与 gateway 同语义）。
func orCurrent(modelName string) string {
	if modelName == "" {
		return "當前模型"
	}
	return modelName
}

// errDelivered 非 200 错误体已作为 error 事件投递给客户端，任务直接结束。
var errDelivered = errors.New("已投递错误事件")

// errNetwork 网络 / 限流类错误：携带上游错误体文本，供 max_retries 消息使用。
type errNetwork struct{ body string }

func (e errNetwork) Error() string { return e.body }

// errRequestLevelRisk 请求级风控（同一 body 在 ≥2 个不同账号上都被上游判风控）。
//
// 与 errNetwork 的区别在出路：errNetwork 由外层换号重试，本类型**不得换号**——换号救不了
// 一份被拒的请求体，只会把健康账号一个个送出去挨打（2026-09-24 整池雪崩正是这条路径）。
// processTicket 收到后把上游原文作为 error 事件投递一次并终止本票。
type errRequestLevelRisk struct{ body string }

func (e errRequestLevelRisk) Error() string { return "request-level risk control: " + e.body }

// errRateLimited 瞬时限流（非额度码族的 429）：账号尚未标任何状态，
// 由 attemptUpstream 决定是原地重试还是递进冷却后换号。
type errRateLimited struct{ body string }

func (e errRateLimited) Error() string { return e.body }

// errOverloaded 平台过载（529 / 业务码 1305）：与账号无关，账号状态完全不变。
// 由 attemptUpstream 按 OverloadRetryDelays 原地退避重试，用尽后原样投递上游
// 错误体并终止本票——不换号，因为换号救不了平台过载。
type errOverloaded struct{ body string }

func (e errOverloaded) Error() string { return e.body }

// bumpFail 记一次失败计数，并一并写下这次失败的归类。改的是 store 锁内的「当前」
// 对象：acc 是 Select 交出的账号副本，直接 `acc.FailCount++` 既不落库、也会与并发
// 读写相撞。
//
// 刻意不覆盖 LastError 文案（与 sync 侧同名方法一致）：LastError 的既有契约是
// 「带可读文案的失败原因」，由 MarkAccount / MarkModelExhausted / MarkRateLimited 负责写。
func (p *Pool) bumpFail(acc *model.Account, kind string) {
	now := time.Now()
	_, _ = p.Store.Update(acc.Provider, acc.ID, func(live *model.Account) {
		live.FailCount++
		gateway.StampAccountError(live, kind, "", now)
	})
}

// maxErrorBodyBytes 异常响应的读取上限。
//
// 正常路径是流式转发（边读边发），只有错误分支与「200 但为 JSON」的异常分支要整段
// 读进内存做分类，而上游或代理异常时可能回一个任意大的 body。64KB 足够容纳业务码
// 与错误消息。
const maxErrorBodyBytes = 64 << 10

// forwardSSE 把上游 SSE 行转成 ticket chunk 事件，并累计账号 token 用量。
// 完整结束时投递 done 并返回 (false, nil)；转发开始后中断时返回
// (true, err)，零 chunk 时返回 (false, err) 由调用方换号重试。
// 中断时若上游已交出终值，用量仍由 accumulateFinalUsage 补记。
//
// diag 为观测诊断容器（可为 nil）：每读到一行就记一次「首字节/最大间隔/字节数」，
// 与 sync 路径的 teeReader 同口径——async 走 bufio.Scanner，不是 teeReader，
// 所以这两个指标必须在各自路径上单独采集。
func (p *Pool) forwardSSE(ctx context.Context, ticketID string, resp *http.Response, acc *model.Account, diag *gateway.ReqDiag) (bool, error) {
	usage := gateway.NewUsageCollector(true)
	chunksSent := 0
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		usage.FeedLine(line)
		if diag != nil {
			// scanner.Text() 去掉了换行符，补 1 让字节数与「上游实际发出的量」同量级。
			diag.MarkRead(diag.Now(), len(line)+1)
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		chunkData := line[len("data: "):]
		if strings.TrimSpace(chunkData) == "[DONE]" {
			continue
		}
		var payload any
		if err := json.Unmarshal([]byte(chunkData), &payload); err != nil {
			continue
		}
		if !p.emit(ctx, ticketID, ticketEvent{Type: "chunk", Data: payload}) {
			// 消费者提前断开（票务被释放）：上游若已交出终值，这笔用量仍要计入
			p.accumulateFinalUsage(ticketID, acc, usage)
			if diag != nil {
				diag.Usage = usage.AsDict()
				diag.UsageComplete = usage.UsageComplete()
				diag.ErrText = fmt.Sprintf("consumer gone: %v", ctx.Err())
			}
			return false, ctx.Err()
		}
		chunksSent++
	}
	if err := scanner.Err(); err != nil {
		p.accumulateFinalUsage(ticketID, acc, usage)
		if diag != nil {
			diag.Usage = usage.AsDict()
			diag.UsageComplete = usage.UsageComplete()
			diag.ErrText = err.Error()
		}
		// 只统计「上游侧掐断」并驱动线路级止血：ctx 已结束说明是票务被释放/消费者
		// 断开，那属于客户端侧，记进去会让「断流是否集中在某账号/线路」失真。
		// 记录与熔断不依赖诊断容器（修掉此前 diag==nil 时整段漏记的缺陷——
		// 诊断行只是展示，账号计数与线路熔断必须照常发生）。
		if ctx.Err() == nil {
			total := gateway.RecordUpstreamTruncate(p.Store, acc, time.Now())
			if diag != nil {
				diag.TruncTotal = total
			}
			p.fireQuotaRefresh(acc)
		}
		if chunksSent > 0 {
			return true, err
		}
		return false, err
	}

	// 统计落库失败不应触发换号重发
	usage.Finish()
	got := usage.AsDict()
	if diag != nil {
		diag.Usage = got
		diag.UsageComplete = usage.UsageComplete()
	}
	if _, err := p.Store.Update(acc.Provider, acc.ID, func(live *model.Account) {
		live.AccumulateTokens(got)
	}); err != nil {
		web.Warn("async", "用量统计落库失败: "+err.Error())
	}
	// 除 token 外还要记调用次数/最后使用时间、清零连续失败计数并把状态复位——
	// 与 engine.success 共用同一份（gateway.MarkSuccess）。否则后台用量页漏算
	// async 流量，且冷却已到期的账号即使这里已成功也仍停在 cooling，要等下一轮
	// 额度轮询（默认 60s）才回到调度。
	// ⚠️ 必须是独立的第二次 Update：Store.Update 持锁内不可再调（自锁）。
	gateway.MarkSuccess(p.Store, acc.Provider, acc.ID, time.Now())
	// 完整成功交付：清零该线路的「连续断流」计数（与 sync 的 finishDelivery
	// 成功分支同口径；不能进 MarkSuccess，见 gateway.ResetLineTruncate 注释）。
	gateway.ResetLineTruncate(p.Store, acc)
	p.emit(ctx, ticketID, ticketEvent{Type: "done"})
	return false, nil
}

// fireQuotaRefresh 异步触发断流账号的额度刷新（与 engine.fireRefresh 同语义）：
// JWT 账号才有额度接口；go 出去不阻塞收尾路径。
func (p *Pool) fireQuotaRefresh(acc *model.Account) {
	if p.OnQuotaRefresh == nil {
		return
	}
	if acc.Provider != model.ProviderZai || acc.Mode != "jwt" {
		return
	}
	go p.OnQuotaRefresh(acc)
}

// accumulateFinalUsage 在「上游已交出最终 usage」时补记这笔用量。
//
// 转发被中断（消费者断开、或上游在 message_delta 之后才断）时，原本直接返回，
// 一笔已经确定的用量就被丢掉了；同步路径的 gateway.finishDelivery 有同样的问题，
// 两处判据一致（gateway.UsageCollector.UsageComplete）。usage 不完整时保持原样
// 不计入，避免记半截数字。
//
// 不会重复计数：两个中断出口都走向终止（消费者断开由调用方按 ctx.Err 直接 return；
// needChunks 场景返回 midStream=true 后不换号重发），不存在「记一次又重试成功再记一次」。
func (p *Pool) accumulateFinalUsage(ticketID string, acc *model.Account, usage *gateway.UsageCollector) {
	if !usage.UsageComplete() {
		return
	}
	usage.Finish()
	got := usage.AsDict()
	if _, err := p.Store.Update(acc.Provider, acc.ID, func(live *model.Account) {
		live.AccumulateTokens(got)
	}); err != nil {
		web.Warn(ticketID, "用量统计落库失败: "+err.Error())
	}
}

// ── 小工具 ──────────────────────────────────────────────────────────────────

// newShortID 生成 6 位十六进制短标识，与 gateway 的 reqID（randomHex(3)）同构。
// 只用于日志检索，不参与任何身份判定，因此不需要密码学强度之外的特殊处理。
func newShortID() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "000000"
	}
	return fmt.Sprintf("%x", b)
}

// asyncResponseHeaderTimeout 对齐 Python make_async_client(account, timeout=httpx.Timeout(180))：
// 各阶段上限 180s；响应体流式读取（SSE）不能设总超时。
const asyncResponseHeaderTimeout = 180 * time.Second

// clientFor 返回账号的出站客户端，以及本次实际生效的出口线路标签（供 [#] 诊断行读数）。
//
// 与网关一致：账号配置了 proxy_url 时走对应代理。README 与 PLAN 都承诺「该账号的
// 网关请求、额度查询与套餐领取均走对应代理」——async 曾漏掉这一条，配置代理的账号
// 在这条路径上以服务器真实 IP 直连上游（泄露部署 IP、正是配置代理要规避的）。
// 代理无效时回退直连并记日志，与 engine.clientFor / quota.clientFor 同语义。
//
// ⚠️ 只能用 proxy.TransportForTimeout（Transport 层 ResponseHeaderTimeout），
// 不能用 proxy.ClientFor——后者设的是 http.Client.Timeout（整体超时），
// 对 SSE 长连接等于给流设了上限。
//
// 第二个返回值：raw 为空、代理无效回退、或被开关强制直连时均为 "direct"。
func (p *Pool) clientFor(acc *model.Account) (*http.Client, string) {
	if p.Client != nil {
		// 注入口（测试/调试）：出口由注入方决定，未知即如实报 direct。
		return p.Client, "direct"
	}
	// async_force_direct：排障用的控制开关，强制忽略账号代理直连。
	// 用途是保留「线路 vs 直连」的归因对照组——修好代理路由后这根本可复现的控制臂
	// 会消失，改由显式开关声明，而不是继续依赖实现缺陷。
	raw := ""
	if acc != nil && acc.ProxyURL != nil && !p.Store.AsyncForceDirect() {
		raw = *acc.ProxyURL
	}
	t, err := proxy.TransportForTimeout(raw, asyncResponseHeaderTimeout)
	if err != nil {
		// 仅当账号配了非法代理才会失败；回退直连（TransportForTimeout 对空 URL
		// 永不报错，故下面的 t 一定非 nil）。
		web.Warn("async", fmt.Sprintf("账号 %s 代理无效，回退直连: %v", acc.Name, err))
		t, _ = proxy.TransportForTimeout("", asyncResponseHeaderTimeout)
		return &http.Client{Transport: t}, "direct"
	}
	if raw == "" {
		return &http.Client{Transport: t}, "direct"
	}
	return &http.Client{Transport: t}, p.Store.ProxyLabel(acc)
}

// marshalJSON 与网关一致（Python json.dumps(ensure_ascii=False) 形态）：
// 紧凑序列化、不转义 HTML 字符、无尾部换行。
func marshalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// sseJSON 对齐 Python json.dumps 默认的 ensure_ascii=True：非 ASCII 字符
// 转义为 \uXXXX（含 UTF-16 代理对），HTML 字符不转义。分隔符沿用 Go 紧凑
// 形态（Python 默认逗号冒号后带空格，SSE 消费方按 JSON 解析无感知）。
func sseJSON(v any) string {
	data, err := marshalJSON(v)
	if err != nil {
		return "{}"
	}
	var b strings.Builder
	for _, r := range string(data) {
		switch {
		case r < 0x80:
			b.WriteRune(r)
		case r > 0xFFFF:
			hi, lo := utf16.EncodeRune(r)
			fmt.Fprintf(&b, `\u%04x\u%04x`, hi, lo)
		default:
			fmt.Fprintf(&b, `\u%04x`, r)
		}
	}
	return b.String()
}

// shallowCopyBody 顶层浅拷贝（对齐 body.copy()：嵌套结构共享引用）。
func shallowCopyBody(body map[string]any) map[string]any {
	out := make(map[string]any, len(body)+1)
	for k, v := range body {
		out[k] = v
	}
	return out
}

// anyToString 对应 Python str(v or "")：nil → 空串。
func anyToString(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

// newUUID 生成 UUIDv4（不引入第三方依赖）。
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// writeJSONStatus 错误 JSON 响应（与网关 writeJSON 同形态）。
func writeJSONStatus(w http.ResponseWriter, status int, body any) {
	data, err := marshalJSON(body)
	if err != nil {
		http.Error(w, `{"error":{"message":"响应序列化失败","type":"internal_error"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
