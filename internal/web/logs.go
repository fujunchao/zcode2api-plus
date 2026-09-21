// Package web 终端彩色日志与启动横幅（对应 Python 版 app/logs.py），
// 以及前端静态资源托管路由（spa.go；嵌入产物来自仓库根包，受 go:embed
// 目录约束）。
// 日志只记录元信息，不记录消息内容（对齐 Python 版隐私约定）。
package web

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"unicode/utf8"
)

const (
	ansiReset  = "\033[0m"
	ansiGreen  = "\033[32m"
	ansiCyan   = "\033[36m"
	ansiYellow = "\033[33m"
	ansiRed    = "\033[31m"
	ansiDim    = "\033[90m"
	ansiWhite  = "\033[37m"
)

// 横幅拼色用导出常量（对应 Python logs 的 _B/_MAG/_C/_Y/_DIM/_R）。
const (
	Bold    = "\033[1m"
	Magenta = "\033[35m"
	Red     = ansiRed
	Green   = ansiGreen
	Blue    = "\033[34m"
	Cyan    = ansiCyan
	Yellow  = ansiYellow
	Dim     = ansiDim
	Reset   = ansiReset
)

// ansiPattern 匹配 SGR 颜色序列，用于计算横幅可见宽度。
var ansiPattern = regexp.MustCompile("\x1b\\[[0-9;]*m")

// outWriter 日志输出去向。默认 stdout；引入它只为了让测试能捕获日志行，
// 生产路径不会调用 SetOut，行为与直接 fmt.Printf 到 stdout 一致。
//
// 用 atomic.Value 而不是 sync.Mutex：日志在请求热路径上被高频调用，加锁会引入
// 争用；SetOut 只在启动/测试入口调用一次，与写日志并发无竞态。
// ⚠️ 不要改成在 store 持锁路径里同步写日志——stdout 阻塞会锁死整个 Store
// （见 store.logPersistFailure 的脱锁处理）。
// writerHolder 让 atomic.Value 始终存同一个具体类型。
//
// 直接把 io.Writer 存进 atomic.Value 是错的：Store 要求所有值的**具体类型**一致，
// 而 init 存的是 *os.File、测试里存的是自定义 writer，混用会 panic
// （sync/atomic: store of inconsistently typed value into Value）。外面套一层定长
// 结构体即可，接口值仍可自由变化。
type writerHolder struct{ w io.Writer }

var outWriter atomic.Value

func init() { outWriter.Store(writerHolder{io.Writer(os.Stdout)}) }

// SetOut 替换日志输出目标（测试用）；传 nil 回落 stdout。
func SetOut(w io.Writer) {
	if w == nil {
		w = os.Stdout
	}
	outWriter.Store(writerHolder{w})
}

func logW() io.Writer { return outWriter.Load().(writerHolder).w }

// Banner 渲染启动横幅：以 ANSI 剥离后的可见宽度画上下边框，
// 行内文本保留原色（对齐 Python logs.banner）。
func Banner(lines ...string) {
	maxWidth := 0
	for _, line := range lines {
		if w := utf8.RuneCountInString(ansiPattern.ReplaceAllString(line, "")); w > maxWidth {
			maxWidth = w
		}
	}
	border := ansiDim + strings.Repeat("=", maxWidth+4) + ansiReset
	fmt.Fprintf(logW(), "\n%s\n", border)
	for _, line := range lines {
		fmt.Fprintf(logW(), "  %s\n", line)
	}
	fmt.Fprintf(logW(), "%s\n\n", border)
}

// Ok 成功日志。
func Ok(module, msg string) {
	fmt.Fprintf(logW(), "  %s[+]%s %s%s%s %s\n", ansiGreen, ansiReset, ansiDim, module, ansiReset, msg)
}

// Warn 警告日志（可重试的失败等）。
func Warn(module, msg string) {
	fmt.Fprintf(logW(), "  %s[~]%s %s%s%s %s\n", ansiYellow, ansiReset, ansiDim, module, ansiReset, msg)
}

// Err 错误日志。
func Err(module, msg string) {
	fmt.Fprintf(logW(), "  %s[!]%s %s%s%s %s\n", ansiRed, ansiReset, ansiDim, module, ansiReset, msg)
}

// Req 请求日志：req_id + 模型 + 是否流式（不含消息内容）。
func Req(reqID, modelName string, stream bool) {
	s := "sync"
	if stream {
		s = "stream"
	}
	fmt.Fprintf(logW(), "  %s>>>%s %s%s%s  %s%s%s  %s%s%s\n",
		ansiCyan, ansiReset, ansiDim, reqID, ansiReset, ansiWhite, modelName, ansiReset, ansiDim, s, ansiReset)
}

// ReqOk 请求完成（tokens 为本次输出 token 数）。
func ReqOk(reqID string, tokens int) {
	tail := ""
	if tokens > 0 {
		tail = fmt.Sprintf("  %s%dtok%s", ansiDim, tokens, ansiReset)
	}
	fmt.Fprintf(logW(), "  %s<<<%s %s%s%s%s\n", ansiGreen, ansiReset, ansiDim, reqID, ansiReset, tail)
}

// ReqErr 请求错误（只显示 req_id + 简短原因）。
func ReqErr(reqID, msg string) {
	fmt.Fprintf(logW(), "  %s<!>%s %s%s%s  %s\n", ansiRed, ansiReset, ansiDim, reqID, ansiReset, msg)
}

// Diag 观测诊断行：每请求一条收尾汇总，承载 >>>/<<</<!> 三类行装不下的字段
// （账号、出口线路、请求体字节数、首字节延迟、最大 chunk 间隔、已收 usage …）。
//
// 用独立的 [#] 标记，刻意不改动 >>>/<<</<!> 的文案与格式——既有日志分析脚本
// 依赖它们的形态做配对。诊断行自带 reqID，可与同一请求的其它行 join。
func Diag(reqID, msg string) {
	fmt.Fprintf(logW(), "  %s[#]%s %s%s%s %s\n", ansiDim, ansiReset, ansiDim, reqID, ansiReset, msg)
}
