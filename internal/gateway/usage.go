// Token 调度统计：从上游 Anthropic Messages 回应中提取 usage。
// 对应 Python 版 app/usage.py。
//
// 上游回应有两种形态：
//   - SSE（text/event-stream）：message_start 携带输入侧（input / cache），
//     message_delta 的 usage.output_tokens 为累计值（取最大，不可累加）。
//   - JSON：顶层 usage 一次带齐全部字段。
//
// 收集器以「块」为单位餵入，内部缓冲不完整行，对上游分块边界不敏感；
// 是否计入账号统计由调用方按 UsageComplete 判定——「上游是否已交出终值」，
// 而不是「客户端有没有把流读完」（见 UsageComplete 的说明）。
package gateway

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"

	"zcode2api/internal/model"
)

// UsageCollector 收集单次回應的 token 用量。
type UsageCollector struct {
	isSSE         bool
	buf           []byte
	input         int
	output        int
	cacheCreation int
	cacheRead     int
	final         bool // 上游已交出最终 usage（见 UsageComplete）
}

// NewUsageCollector 创建收集器；isSSE 决定逐行解析还是缓冲到 Finish 一次解析。
func NewUsageCollector(isSSE bool) *UsageCollector {
	return &UsageCollector{isSSE: isSSE}
}

// Feed 餵入一段回應位元組。
func (u *UsageCollector) Feed(chunk []byte) {
	u.buf = append(u.buf, chunk...)
	if !u.isSSE {
		return
	}
	for {
		idx := bytes.IndexByte(u.buf, '\n')
		if idx < 0 {
			break
		}
		line := u.buf[:idx]
		u.buf = u.buf[idx+1:]
		u.FeedLine(string(line))
	}
}

// FeedLine 解析一行 SSE data；供串流分块解析与 async 池逐行解析共用。
func (u *UsageCollector) FeedLine(line string) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "data:") {
		return
	}
	payloadText := strings.TrimSpace(line[len("data:"):])
	if payloadText == "" || payloadText == "[DONE]" {
		return
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(payloadText), &payload); err != nil {
		return
	}
	switch payload["type"] {
	case "message_start":
		msg, _ := payload["message"].(map[string]any)
		usage, _ := msg["usage"].(map[string]any)
		u.input = max(u.input, toInt(usage["input_tokens"]))
		u.output = max(u.output, toInt(usage["output_tokens"]))
		u.cacheCreation = max(u.cacheCreation, toInt(usage["cache_creation_input_tokens"]))
		u.cacheRead = max(u.cacheRead, toInt(usage["cache_read_input_tokens"]))
	case "message_delta":
		// output_tokens 为累计值，取最大避免重复累加
		usage, _ := payload["usage"].(map[string]any)
		u.output = max(u.output, toInt(usage["output_tokens"]))
		u.markFinalIfComplete(payload, usage)
	}
}

// markFinalIfComplete 记录「上游已交出最终 usage」。
//
// Anthropic 语义：message_delta 的 delta.stop_reason 非空表示本轮生成已终止，
// 且该事件的 usage.output_tokens 是累计终值。两个条件必须同时成立——只看
// stop_reason 会在 usage 缺失时把 output 误记成 0，只看 usage 又会把中途的增量
// 更新当成终值。判定偏保守（宁可说不完整）是可接受的：调用方在不确定时沿用
// 旧的「不计入」行为，不会记错数字。
func (u *UsageCollector) markFinalIfComplete(payload, usage map[string]any) {
	if usage == nil {
		return
	}
	delta, _ := payload["delta"].(map[string]any)
	if delta == nil || AnyToString(delta["stop_reason"]) == "" {
		return
	}
	if _, ok := usage["output_tokens"]; !ok {
		return
	}
	u.final = true
}

// UsageComplete 报告上游是否已交出最终 usage。
//
// 交付因「客户端提前断开」失败时，用它决定这笔用量还能不能计入：zcode 编辑器这类
// 客户端收到 finish_reason（源自 message_delta）就立刻关流是常态，此时上游已经把
// 终值给出，usage 是准的。若因为「客户端没读完」就丢掉，账号用量会系统性少算——
// 线上曾因此漏计一次 57,352 output token 的生成
// （见 docs/analysis-flash-30min-stream-cut.md §5）。
func (u *UsageCollector) UsageComplete() bool { return u.final }

// Finish 回應结束后收尾：非 SSE 模式在此解析缓冲的完整 JSON，并标记 usage 为终值。
func (u *UsageCollector) Finish() {
	if u.isSSE {
		u.buf = nil
		return
	}
	var payload map[string]any
	if err := json.Unmarshal(u.buf, &payload); err == nil {
		usage, _ := payload["usage"].(map[string]any)
		if usage != nil {
			u.input = max(u.input, toInt(usage["input_tokens"]))
			u.output = max(u.output, toInt(usage["output_tokens"]))
			u.cacheCreation = max(u.cacheCreation, toInt(usage["cache_creation_input_tokens"]))
			u.cacheRead = max(u.cacheRead, toInt(usage["cache_read_input_tokens"]))
			// 非 SSE 回應是整体到达后解析的，解析出 usage 即终值
			u.final = true
		}
	}
	u.buf = nil
}

// AsDict 返回累计结果（键与 Account 累计字段约定一致）。
func (u *UsageCollector) AsDict() model.Usage {
	return model.Usage{
		Input:         u.input,
		Output:        u.output,
		CacheCreation: u.cacheCreation,
		CacheRead:     u.cacheRead,
	}
}

// toInt 对应 Python 版 _to_int：int(value or 0) 再由调用方 max(0, ...) 截负。
func toInt(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case float32:
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	case json.Number:
		n, err := strconv.ParseInt(x.String(), 10, 64)
		if err != nil {
			return 0
		}
		return int(n)
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(x))
		if err != nil {
			return 0
		}
		return n
	}
	return 0
}
