// 上游断流的终态语义与「未出内容前的预缓冲」。
//
// 背景（docs/plan-truncate-resilience-20261107.md）：上游在 SSE 响应体中途被掐断时，
// HTTP 200 与已写出的字节都无法回滚，网关只能补一个终态。补什么决定客户端怎么反应：
// 2026-11-07 复盘里 ZCode 把 api_error 判成「不可重试」（reason=unknown /
// retryable=false），直接判死整轮 turn，而它本来有 11 次重试预算。
//
// 本文件提供三件事：
//   - 终态形态（retryable / graceful / abort / legacy）的具体字节与降级规则；
//   - 断流前的预缓冲：在「首个内容增量」之前不提交响应头，这段时间的失败可以换号
//     重打而对客户端完全不可见；
//   - 一个只观察不改写的流状态探针（sseTail），graceful 收尾据此决定能不能补帧。
package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/sse"
)

// NothingDeliveredError 表示交付在写出任何字节之前就失败（响应头尚未提交）。
//
// 这种失败对客户端完全不可见，引擎可以换号重打；一旦有字节写出就只能补终态。
type NothingDeliveredError struct{ Err error }

func (e *NothingDeliveredError) Error() string {
	return "交付未写出任何字节: " + e.Err.Error()
}

// Unwrap 让 errors.Is/As 能穿透到原始错误。
func (e *NothingDeliveredError) Unwrap() error { return e.Err }

// AbortedDeliveryError 表示按 abort 策略主动断开连接（响应头已提交）。
//
// HTTP 层把它翻译成 panic(http.ErrAbortHandler)：chunked 编码不收尾，客户端看到的是
// 传输层中断（reason=network_error，可重试），而不是「服务端业务错误」。
type AbortedDeliveryError struct{ Err error }

func (e *AbortedDeliveryError) Error() string {
	return "按 abort 策略断开连接: " + e.Err.Error()
}

// Unwrap 让 errors.Is/As 能穿透到原始错误。
func (e *AbortedDeliveryError) Unwrap() error { return e.Err }

// sseTail 观察已经转发给客户端的 SSE 事件，回答「断流后怎么收尾」：
// 消息有没有开始、哪个内容块还开着、是不是 tool_use、最近一次 usage 是多少。
//
// 只观察、不改写字节；解析失败只置 dirty，让 graceful 主动放弃——宁可退回可重试错误，
// 也不能凭猜测写坏协议。
type sseTail struct {
	decoder   sse.Decoder
	pending   []byte
	started   bool
	stopped   bool
	dirty     bool
	nextIndex int
	openIndex int
	openType  string
	openTool  bool
	usage     map[string]any
	content   bool
}

// feed 餵入一段已经下发给客户端的字节，按行成帧解析。
func (t *sseTail) feed(chunk []byte) {
	t.pending = append(t.pending, chunk...)
	for {
		idx := bytes.IndexByte(t.pending, '\n')
		if idx < 0 {
			return
		}
		line := string(t.pending[:idx])
		t.pending = t.pending[idx+1:]
		t.observe(t.decoder.FeedLine(line))
	}
}

// finish 冲刷尾部不完整事件（EOF 时没有尾空行是常态）。
func (t *sseTail) finish() {
	if len(t.pending) > 0 {
		t.observe(t.decoder.FeedLine(string(t.pending)))
		t.pending = nil
	}
	t.observe(t.decoder.Finish())
}

// observe 更新状态；任何解析异常只置 dirty，不打断转发。
func (t *sseTail) observe(event *sse.Event) {
	if event == nil {
		return
	}
	eventName := strings.TrimSpace(event.Name)
	data := strings.TrimSpace(event.Data)
	if data == "" || data == "[DONE]" {
		return
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		t.dirty = true
		return
	}
	kind := eventName
	if kind == "" {
		kind, _ = payload["type"].(string)
	}
	switch kind {
	case "message_start":
		t.started = true
	case "content_block_start":
		index := intFromAny(payload["index"])
		block, _ := payload["content_block"].(map[string]any)
		blockType, _ := block["type"].(string)
		t.openIndex, t.openType = index, blockType
		t.openTool = blockType == "tool_use" || blockType == "server_tool_use"
		if index >= t.nextIndex {
			t.nextIndex = index + 1
		}
		if text, _ := block[blockType].(string); text != "" {
			t.content = true
		}
	case "content_block_delta":
		delta, _ := payload["delta"].(map[string]any)
		if delta == nil {
			return
		}
		switch delta["type"] {
		case "text_delta":
			if text, _ := delta["text"].(string); text != "" {
				t.content = true
			}
		case "thinking_delta":
			if text, _ := delta["thinking"].(string); text != "" {
				t.content = true
			}
		case "input_json_delta":
			if text, _ := delta["partial_json"].(string); text != "" {
				t.content = true
			}
		}
	case "content_block_stop":
		if intFromAny(payload["index"]) == t.openIndex {
			t.openType, t.openTool = "", false
		}
	case "message_delta":
		if usage, ok := payload["usage"].(map[string]any); ok {
			t.usage = usage
		}
	case "message_stop":
		t.stopped = true
	}
}

// contentSeen 报告是否已出现真正的内容（文本 / 思考 / 工具参数增量）。
// 预缓冲据此决定什么时候提交响应头——控制事件（message_start / ping）不算。
func (t *sseTail) contentSeen() bool { return t.content }

// gracefulFrames 给出一段「协议上正常结束」的帧序列。
//
// 条件：消息已开始、上游没给出终态、解析没有出过错、且当前打开的不是 tool_use 块。
// 半截工具调用绝不能伪装成功——客户端会拿它去执行，比报错危险得多。
func (t *sseTail) gracefulFrames() ([]string, bool) {
	if !t.started || t.stopped || t.dirty || t.openTool {
		return nil, false
	}
	frames := make([]string, 0, 5)
	if t.openType != "" {
		frames = append(frames, sseFrame("content_block_stop", map[string]any{
			"type": "content_block_stop", "index": t.openIndex,
		}))
	}
	// 追加可见提示：把截断伪装成完整回答更糟——用户会照着半截结论继续干活。
	if notice := strings.TrimSpace(config.TruncateGracefulNotice); notice != "" {
		index := t.nextIndex
		frames = append(frames,
			sseFrame("content_block_start", map[string]any{
				"type": "content_block_start", "index": index,
				"content_block": map[string]any{"type": "text", "text": ""},
			}),
			sseFrame("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": index,
				"delta": map[string]any{"type": "text_delta", "text": "\n\n" + notice},
			}),
			sseFrame("content_block_stop", map[string]any{
				"type": "content_block_stop", "index": index,
			}),
		)
	}
	delta := map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
	}
	if len(t.usage) > 0 {
		delta["usage"] = t.usage
	}
	frames = append(frames, sseFrame("message_delta", delta), sseFrame("message_stop", map[string]any{"type": "message_stop"}))
	return frames, true
}

// terminalErrorFrame 生成断流的错误帧。
//
// retryable（默认）：type 与 code 都写 overloaded_error。ZCode 的 provider 错误码读取
// 链是 o.code → error.code → error_code → code，命中其 I4e 映射表后得到
// reason=ProviderOverloaded / retryable=true ⇒ 自动重试（最多 11 次）。
// legacy：保持改造前的 api_error（不可重试），仅用于回滚。
func terminalErrorFrame(cause error) string {
	message := "上游流式响应中断: " + cause.Error()
	errorType, errorCode := "api_error", ""
	if config.TruncateTerminal != config.TruncateTerminalLegacy {
		message = "上游流式响应中断（已转交客户端重试）: " + cause.Error()
		errorType, errorCode = "overloaded_error", "overloaded_error"
	}
	errorObj := map[string]any{"type": errorType, "message": message}
	if errorCode != "" {
		errorObj["code"] = errorCode
	}
	payload, err := marshalJSON(map[string]any{"type": "error", "error": errorObj})
	if err != nil {
		payload = []byte(`{"type":"error","error":{"type":"api_error","message":"上游流式响应中断"}}`)
	}
	// 前置空行：上游可能残留半行，先隔开再发新事件（沿用改造前的形态）。
	return "\n\nevent: error\ndata: " + string(payload) + "\n\n"
}

// writeTruncateTerminal 在流中途失败后把终态写给客户端，并返回交付错误。
//
// 返回 AbortedDeliveryError 表示「不补帧、直接断开」，由 HTTP 层翻译成
// panic(http.ErrAbortHandler)；其余情况返回原始 cause，引擎照旧记断流账。
func writeTruncateTerminal(write func(string) error, tail *sseTail, cause error) error {
	if config.TruncateTerminal == config.TruncateTerminalAbort {
		return &AbortedDeliveryError{Err: cause}
	}
	if config.TruncateTerminal == config.TruncateTerminalGraceful && tail != nil {
		if frames, ok := tail.gracefulFrames(); ok {
			for _, frame := range frames {
				if err := write(frame); err != nil {
					return err
				}
			}
			return cause
		}
		// 降级：没有可用状态（未开始 / 工具块未闭合 / 解析出错）→ 走可重试错误。
	}
	if err := write(terminalErrorFrame(cause)); err != nil {
		return err
	}
	return cause
}

// sseFrame 序列化一个「event: + data:」形态的 SSE 事件。
func sseFrame(name string, payload map[string]any) string {
	data, err := marshalJSON(payload)
	if err != nil {
		data = []byte("{}")
	}
	return "event: " + name + "\ndata: " + string(data) + "\n\n"
}

// intFromAny 取 JSON 数字的整数形态；缺失或非数字返回 0。
func intFromAny(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0
		}
		return int(i)
	}
	return 0
}

// ── 预缓冲：首个内容增量之前不提交响应头 ────────────────────────────────────

// prebufferGate 暂存「尚未提交」的响应字节，并在需要时一次性提交。
//
// 提交时机：首个内容增量 / 待发字节超过上限 / 时间预算到期。三种时机都为了同一件事：
// 提交之前的失败可以换号重打（客户端一个字节都没收到），提交之后只能补终态。
//
// 读写都在锁内完成：时间预算到期由独立的 timer goroutine 触发提交，与读取循环并发。
type prebufferGate struct {
	mu          sync.Mutex
	w           http.ResponseWriter
	flusher     http.Flusher
	status      int
	contentType string
	maxBytes    int
	pending     []byte
	committed   bool
	abandoned   bool
	writeErr    error
	timer       *time.Timer
}

// newPrebufferGate 创建预缓冲；maxBytes 为待发字节上限（<=0 表示不限）。
func newPrebufferGate(w http.ResponseWriter, status int, contentType string, maxBytes int) *prebufferGate {
	return &prebufferGate{
		w: w, status: status, contentType: contentType, maxBytes: maxBytes,
		flusher: flusherOf(w),
	}
}

// push 暂存或直写一段字节；返回「是否应当立即提交」与错误。
//
// 已提交（含 timer 抢在读取循环之前提交）时退化为直写，不丢字节。
func (g *prebufferGate) push(chunk []byte) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.abandoned {
		return true, nil
	}
	if g.committed {
		return true, g.writeLocked(chunk)
	}
	g.pending = append(g.pending, chunk...)
	if g.maxBytes > 0 && len(g.pending) >= g.maxBytes {
		return true, g.commitLocked()
	}
	return false, nil
}

// commit 提交响应头与暂存字节（幂等）。
func (g *prebufferGate) commit() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.commitLocked()
}

// write 提交后直写一段字节（内部先确保已提交）。
func (g *prebufferGate) write(chunk []byte) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.abandoned {
		return nil
	}
	if !g.committed {
		if err := g.commitLocked(); err != nil {
			return err
		}
	}
	return g.writeLocked(chunk)
}

// writeLocked 直写并刷新；写失败后缓存错误，后续调用返回同一错误。
func (g *prebufferGate) writeLocked(chunk []byte) error {
	if g.writeErr != nil {
		return g.writeErr
	}
	if _, err := g.w.Write(chunk); err != nil {
		g.writeErr = err
		return err
	}
	if g.flusher != nil {
		g.flusher.Flush()
	}
	return nil
}

// startTimer 启动「预算到期强制提交」；关闭预缓冲时是空操作。
func (g *prebufferGate) startTimer(budget time.Duration) {
	if budget <= 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.committed || g.abandoned || g.timer != nil {
		return
	}
	g.timer = time.AfterFunc(budget, func() { _ = g.commit() })
}

// claimUndelivered 原子地宣告「本次交付对客户端不可见」：停表并确认尚未提交。
//
// 必须与 commit 互斥，否则会出现「timer 刚提交、读取循环却按未提交处理」，
// 导致同一请求写两遍响应。
func (g *prebufferGate) claimUndelivered() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stopTimerLocked()
	if g.committed {
		return false
	}
	g.abandoned = true
	return true
}

func (g *prebufferGate) stopTimerLocked() {
	if g.timer != nil {
		g.timer.Stop()
		g.timer = nil
	}
}

func (g *prebufferGate) commitLocked() error {
	if g.committed || g.abandoned {
		return g.writeErr
	}
	g.committed = true
	g.stopTimerLocked()
	g.w.Header().Set("Content-Type", g.contentType)
	g.w.Header().Set("Cache-Control", "no-cache")
	g.w.WriteHeader(g.status)
	if len(g.pending) > 0 {
		pending := g.pending
		g.pending = nil
		if err := g.writeLocked(pending); err != nil {
			return err
		}
	}
	if g.flusher != nil {
		g.flusher.Flush()
	}
	return nil
}

// flusherOf 取底层 ResponseWriter 的 Flusher（不支持刷新时返回 nil）。
func flusherOf(w http.ResponseWriter) http.Flusher {
	if w == nil {
		return nil
	}
	if f, ok := w.(http.Flusher); ok {
		return f
	}
	return nil
}

// prebufferBudget 返回预缓冲时间预算；<=0 表示关闭。
func prebufferBudget() time.Duration { return config.TruncatePrebufferBudget }
