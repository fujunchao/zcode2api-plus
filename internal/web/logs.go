// Package web 终端彩色日志与启动横幅（对应 Python 版 app/logs.py），
// 以及前端静态资源托管路由（spa.go；嵌入产物来自仓库根包，受 go:embed
// 目录约束）。
// 日志只记录元信息，不记录消息内容（对齐 Python 版隐私约定）。
package web

import (
	"fmt"
	"regexp"
	"strings"
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
	fmt.Printf("\n%s\n", border)
	for _, line := range lines {
		fmt.Printf("  %s\n", line)
	}
	fmt.Printf("%s\n\n", border)
}

// Ok 成功日志。
func Ok(module, msg string) {
	fmt.Printf("  %s[+]%s %s%s%s %s\n", ansiGreen, ansiReset, ansiDim, module, ansiReset, msg)
}

// Warn 警告日志（可重试的失败等）。
func Warn(module, msg string) {
	fmt.Printf("  %s[~]%s %s%s%s %s\n", ansiYellow, ansiReset, ansiDim, module, ansiReset, msg)
}

// Err 错误日志。
func Err(module, msg string) {
	fmt.Printf("  %s[!]%s %s%s%s %s\n", ansiRed, ansiReset, ansiDim, module, ansiReset, msg)
}

// Req 请求日志：req_id + 模型 + 是否流式（不含消息内容）。
func Req(reqID, modelName string, stream bool) {
	s := "sync"
	if stream {
		s = "stream"
	}
	fmt.Printf("  %s>>>%s %s%s%s  %s%s%s  %s%s%s\n",
		ansiCyan, ansiReset, ansiDim, reqID, ansiReset, ansiWhite, modelName, ansiReset, ansiDim, s, ansiReset)
}

// ReqOk 请求完成（tokens 为本次输出 token 数）。
func ReqOk(reqID string, tokens int) {
	tail := ""
	if tokens > 0 {
		tail = fmt.Sprintf("  %s%dtok%s", ansiDim, tokens, ansiReset)
	}
	fmt.Printf("  %s<<<%s %s%s%s%s\n", ansiGreen, ansiReset, ansiDim, reqID, ansiReset, tail)
}

// ReqErr 请求错误（只显示 req_id + 简短原因）。
func ReqErr(reqID, msg string) {
	fmt.Printf("  %s<!>%s %s%s%s  %s\n", ansiRed, ansiReset, ansiDim, reqID, ansiReset, msg)
}
