package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"

	"zcode2api/internal/model"
)

// ReqDiag 单次请求的观测诊断容器。
//
// 背景：现有的 `>>>` / `<<<` / `<!>` 三类行是「定长参数 + 纯字符串拼接」，塞不下
// 「账号名、出口线路、请求体字节数、首字节延迟、最大 chunk 间隔」这些要到请求中后段
// 才知道的字段；而账号身份要等 Store.Select 之后才可得，起始行在结构上就打不出来。
// 于是观测数据集中到收尾期输出一条独立的 `[#]` 汇总行（见 web.Diag）。
//
// 两类硬约束：
//  1. **不改变既有三类行的文本**（有外部分析脚本依赖它们做配对）——本容器只服务新行。
//  2. **不驱动任何状态机**：里面的字段只用于打印；账号侧的累计计数走
//     RecordStreamTruncate（capability 明确的纯观测入口），不走 MarkAccount。
//
// 时钟由调用方注入（sync 用 Engine.now，可被测试替换），首字节/间隔的测量因此可断言。
type ReqDiag struct {
	ReqID     string // 6 hex，与同请求的 >>>/<<</<!> 行一致，可 join
	Model     string
	Stream    bool
	Effort    string // 思考档（output_config.effort / reasoning_effort / thinking.type）
	MaxTokens string
	Ticket    string // async 专用：同时带 36 字符 ticketID，保留既有检索习惯

	BodyBytes int    // 真正发给上游的请求体字节数（注入 system 之后）
	BodyHash  string // 请求体的 sha256 前 12 位十六进制；空串 = 未构建出请求体
	Attempts  int    // 本次请求内的选号尝试次数（>1 说明发生过换号/重试）

	// RiskAccounts 本请求内给出风控信号的**不同账号数**（按账号 ID 去重）。
	// 0 = 没出现过风控信号；≥2 即请求级（同一 body 换谁都被拒）。
	//
	// 存在的理由：`body=` 只有字节数，**字节数相同 ≠ 内容相同**，所以线上无法直接
	// 回答「这个 body 打过几个账号」。2026-09-24 那次整池冷却事故只能靠「14 条
	// 记录的 body 恰好都是 278081」这种旁证定性（见
	// docs/analysis-riskcontrol-and-client-format-20260924.md）。有了 BodyHash 与
	// 本字段，同一个 body 的跨账号命中可以直接检索出来。
	RiskAccounts int

	AccName string // 最后一次尝试的账号
	Route   string // 出口线路：线路名 / proxy:***@host / direct

	UpstreamStart time.Time // 上游请求发出时刻（首字节与耗时的基准）
	FirstByte     time.Duration
	MaxGap        time.Duration
	Bytes         int64 // 从上游读到的总字节数
	lastRead      time.Time

	Status        int
	Usage         model.Usage
	UsageComplete bool
	TruncTotal    int // 账号侧累计被掐断次数（进程内）
	ErrText       string

	startedAt time.Time
	total     time.Duration
	clock     func() time.Time
}

// NewReqDiag 建立诊断容器；clock 传 nil 时用 time.Now。
// body 用于抽取「思考档 / max_tokens」这两个请求体画像字段（缺失时留空，打印为 -）。
func NewReqDiag(reqID, modelName string, stream bool, body map[string]any, clock func() time.Time) *ReqDiag {
	if clock == nil {
		clock = time.Now
	}
	return &ReqDiag{
		ReqID:     reqID,
		Model:     modelName,
		Stream:    stream,
		Effort:    diagEffort(body),
		MaxTokens: diagMaxTokens(body),
		startedAt: clock(),
		clock:     clock,
	}
}

// Now 取当前时刻（走注入的时钟）。
func (d *ReqDiag) Now() time.Time { return d.clock() }

// bodyHashHexLen body 指纹的十六进制长度。12 位 = 48 bit，碰撞概率在「单日数千请求」
// 的量级下可忽略，又短到能在日志行里直接读、直接做检索键。
const bodyHashHexLen = 12

// SetBody 登记请求体画像，一次调用写两个字段（分开设置迟早漂移）：
//
//   - payload：**真正发给上游**的字节，字节数取它；
//   - contentView：**内容视图**，指纹取它 —— 即注入逐账号身份（设备指纹、会话 id）
//     **之前**的请求体。为空时退回用 payload（等价于旧行为）。
//
// 为什么指纹不取 payload：设备身份是每账号一份的信封（见 upstream.InjectDeviceMetadata），
// 直接哈希 payload 会让**同一份内容在换号后得到不同指纹**，而本字段的用途恰恰是
// 「这个 body 被送到了几个账号」（见 ReqDiag.RiskAccounts）—— 用途与实现必须对齐。
// 字节数则相反，要的是实际发出的真值。
//
// nil 接收者安全：部分调用点（如异步票的早退分支）不保证有 diag。
func (d *ReqDiag) SetBody(payload, contentView []byte) {
	if d == nil {
		return
	}
	d.BodyBytes = len(payload)
	if len(contentView) == 0 {
		contentView = payload
	}
	sum := sha256.Sum256(contentView)
	d.BodyHash = hex.EncodeToString(sum[:])[:bodyHashHexLen]
}

// SetRiskAccounts 记录本请求内已命中的**不同**风控账号数。
// 由风控分支在每次命中后调用，最后一次写入即为本请求的最终值。
func (d *ReqDiag) SetRiskAccounts(n int) {
	if d == nil {
		return
	}
	d.RiskAccounts = n
}

// MarkRead 记录一次「从上游读到 n 字节」。
//
// 落点是流式读取的必经之处（sync: teeReader.Read；async: forwardSSE 的 scanner 循环），
// 因此能同时得到两个关键指标：
//   - FirstByte：上游首字节延迟（识别「缓冲型线路」最便宜的探针）；
//   - MaxGap：相邻两次读之间的最大间隔（区分「固定时长墙」与「空闲超时/静默卡死」——
//     这两类故障的修法完全相反，此前无法从日志区分）。
//
// 非流式（JSON 缓冲交付）不经此路径，这两个字段留空、打印为 `-`，语义正确。
func (d *ReqDiag) MarkRead(now time.Time, n int) {
	if !d.UpstreamStart.IsZero() {
		if d.FirstByte == 0 {
			d.FirstByte = now.Sub(d.UpstreamStart)
		}
		if !d.lastRead.IsZero() {
			if gap := now.Sub(d.lastRead); gap > d.MaxGap {
				d.MaxGap = gap
			}
		}
	}
	d.lastRead = now
	d.Bytes += int64(n)
}

// Finish 标记请求收尾，冻结总耗时。
func (d *ReqDiag) Finish(now time.Time) {
	if d.startedAt.IsZero() {
		return
	}
	d.total = now.Sub(d.startedAt)
}

// Format 渲染诊断行的消息体（不含前缀与 reqID，由 web.Diag 补）。
func (d *ReqDiag) Format() string {
	msg := fmt.Sprintf("model=%s stream=%t effort=%s maxtok=%s body=%s attempts=%d acc=%s route=%s",
		dashText(d.Model), d.Stream, dashText(d.Effort), dashText(d.MaxTokens),
		dashBytes(d.BodyBytes), d.Attempts, dashText(d.AccName), dashText(d.Route))
	if d.Ticket != "" {
		// async：两个 id 都给，既可用短 id 与 sync 对照，也不破坏按 ticketID 的既有检索。
		msg += " ticket=" + d.Ticket
	}
	msg += fmt.Sprintf(" firstbyte=%s maxgap=%s bytes=%s",
		dashDur(d.FirstByte), dashDur(d.MaxGap), dashCount(d.Bytes))
	msg += fmt.Sprintf(" usage=in:%d/out:%d/cache_r:%d/cache_c:%d complete=%d trunc_total=%d",
		d.Usage.Input, d.Usage.Output, d.Usage.CacheRead, d.Usage.CacheCreation,
		boolToInt(d.UsageComplete), d.TruncTotal)
	msg += fmt.Sprintf(" total=%s status=%s err=%s",
		dashDur(d.total), dashInt(d.Status), dashErr(d.ErrText))
	// 新增字段**一律追加在行尾**，不插进既有字段之间：外部分析脚本可能按位置切分，
	// 插在中间会让它们既有的下标全部错位。
	//
	// riskscope 刻意打印裸整数（而不是沿用上面的 `-` 表示未知）：这个字段的 0 有
	// 明确含义（本请求没出现过风控信号），判读时直接看 `riskscope>=2` 即可，
	// 与 bodyhash 的「空串 = 没构建出请求体」是两回事。
	msg += fmt.Sprintf(" bodyhash=%s riskscope=%d",
		dashText(d.BodyHash), d.RiskAccounts)
	return msg
}

// ── 格式化辅助 ──────────────────────────────────────────────────────────────
//
// 一律用 `-` 表示「未知/不适用」，而不是 0：0 在这几个字段上都是合法的真实值
// （例如首字节 0ms、状态码 0），把「没测到」和「测到 0」混在一起会让判读出错。

func dashText(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func dashInt(v int) string {
	if v == 0 {
		return "-"
	}
	return strconv.Itoa(v)
}

func dashBytes(v int) string {
	if v == 0 {
		return "-"
	}
	return fmt.Sprintf("%dB", v)
}

func dashCount(v int64) string {
	if v == 0 {
		return "-"
	}
	return fmt.Sprintf("%dB", v)
}

func dashDur(d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	return fmt.Sprintf("%dms", d.Milliseconds())
}

func dashErr(s string) string {
	if s == "" {
		return "-"
	}
	return strconv.Quote(s)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// diagEffort 抽取思考档。NormalizeBody 会把客户端的 reasoningLevel 归一化到
// output_config.effort（见 body.go / openai/reasoning.go），这里按优先级宽松回落，
// 只为日志可读性，不参与任何请求构造。
func diagEffort(body map[string]any) string {
	if oc, ok := body["output_config"].(map[string]any); ok {
		if s, ok := oc["effort"].(string); ok && s != "" {
			return s
		}
	}
	if s, ok := body["reasoning_effort"].(string); ok && s != "" {
		return s
	}
	if th, ok := body["thinking"].(map[string]any); ok {
		if s, ok := th["type"].(string); ok && s != "" {
			return "thinking:" + s
		}
	}
	return ""
}

// diagMaxTokens 抽取 max_tokens（JSON 数字在不同入口下可能是 float64/int）。
func diagMaxTokens(body map[string]any) string {
	switch v := body["max_tokens"].(type) {
	case float64:
		return strconv.FormatInt(int64(v), 10)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case string:
		return v
	}
	return ""
}
