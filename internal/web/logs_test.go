package web

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// captureLogs 把日志输出重定向到管道，返回「结束并取回全部输出」的函数。
// 调用方必须 defer 该函数（它负责恢复 stdout 并关闭管道）。
func captureLogs(t *testing.T) func() string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("建管道失败: %v", err)
	}
	SetOut(w)
	return func() string {
		_ = w.Close()
		SetOut(nil)
		out, _ := io.ReadAll(r)
		_ = r.Close()
		return string(out)
	}
}

func TestSetOutNilRestoresStdout(t *testing.T) {
	SetOut(nil)
	if got := logW(); got != io.Writer(os.Stdout) {
		t.Fatalf("SetOut(nil) 应回落 stdout")
	}
}

// TestDiagLineFormat 钉死诊断行的形态：tag [#]、reqID、msg 三段。
func TestDiagLineFormat(t *testing.T) {
	done := captureLogs(t)
	Diag("a1b2c3", "model=glm-5.3-flash complete=0")
	got := done()

	want := "  " + ansiDim + "[#]" + ansiReset + " " + ansiDim + "a1b2c3" + ansiReset + " model=glm-5.3-flash complete=0\n"
	if got != want {
		t.Fatalf("诊断行形态不符\n got=%q\nwant=%q", got, want)
	}
}

// TestLegacyLineFormatsFrozen 冻结既有三类行的文本。
//
// >>>  / <<< / <!> 是 .workbuddy/analyze-gateway-log.js 等既有分析脚本赖以配对
// 请求的依据；诊断行必须是**新增**的一行，绝不能改动它们的文案与分隔。
// 本用例是那条约定的守卫——若有人「顺手优化」了这些格式，测试会红。
func TestLegacyLineFormatsFrozen(t *testing.T) {
	done := captureLogs(t)
	Req("a1b2c3", "glm-5.3-flash", true)
	ReqOk("a1b2c3", 42)
	ReqOk("a1b2c3", 0)
	ReqErr("a1b2c3", "流传输中断: unexpected EOF")
	got := done()

	dim, reset := ansiDim, ansiReset
	want := "  " + ansiCyan + ">>>" + reset + " " + dim + "a1b2c3" + reset + "  " + ansiWhite + "glm-5.3-flash" + reset + "  " + dim + "stream" + reset + "\n" +
		"  " + ansiGreen + "<<<" + reset + " " + dim + "a1b2c3" + reset + "  " + dim + "42tok" + reset + "\n" +
		"  " + ansiGreen + "<<<" + reset + " " + dim + "a1b2c3" + reset + "\n" +
		"  " + ansiRed + "<!>" + reset + " " + dim + "a1b2c3" + reset + "  流传输中断: unexpected EOF\n"
	if got != want {
		t.Fatalf("既有行形态被改动（违反契约）\n got=%q\nwant=%q", got, want)
	}
}

// TestReqSyncModeAndEmptyModel 覆盖 sync 模式与空模型名的展示（不 panic、不变形）。
func TestReqSyncModeAndEmptyModel(t *testing.T) {
	done := captureLogs(t)
	Req("d48109", "-", false)
	got := done()
	if !strings.Contains(got, ansiDim+"sync"+ansiReset) {
		t.Fatalf("sync 模式未按预期渲染: %q", got)
	}
	if !strings.Contains(got, "-") {
		t.Fatalf("空模型名未按预期渲染: %q", got)
	}
}

// TestAllPrimitivesHonorSetOut 确认 7 个原语都走注入的 writer（漏一个就会打进真 stdout）。
func TestAllPrimitivesHonorSetOut(t *testing.T) {
	done := captureLogs(t)
	Banner("zcode2api v9")
	Ok("main", "ok")
	Warn("main", "warn")
	Err("main", "err")
	Req("a1b2c3", "m", true)
	ReqOk("a1b2c3", 1)
	ReqErr("a1b2c3", "x")
	Diag("a1b2c3", "y")
	got := done()
	for _, frag := range []string{"zcode2api v9", "[+]", "[~]", "[!]", ">>>", "<<<", "<!>", "[#]"} {
		if !strings.Contains(got, frag) {
			t.Fatalf("原语未走注入 writer，缺少片段 %q；got=%q", frag, got)
		}
	}
}

// 后台行必须带可解析的时间戳：此前 [~] 行没有 ts=，复盘断流线路时只能靠
// 文件行序 + 邻近 [attempt] 行反推时间线。
func TestWarnAndErrCarryTimestamp(t *testing.T) {
	buf := &strings.Builder{}
	SetOut(buf)
	t.Cleanup(func() { SetOut(nil) })

	Warn("line-guard", "线路 X 连续 3 次上游断流")
	Err("proxy-soft", "软失败状态落库失败")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("应输出两行，实际 %d：%q", len(lines), buf.String())
	}
	for _, line := range lines {
		plain := ansiPattern.ReplaceAllString(line, "")
		if !strings.HasPrefix(strings.TrimSpace(plain), "[~]") && !strings.Contains(plain, "[!]") {
			t.Fatalf("标记被破坏：%s", plain)
		}
		idx := strings.LastIndex(line, " ts=")
		if idx < 0 {
			t.Fatalf("缺少 ts=：%s", line)
		}
		ts := line[idx+len(" ts="):]
		parsed, err := time.Parse(time.RFC3339, ts)
		if err != nil || parsed.IsZero() {
			t.Fatalf("ts 不可解析：%q（%v）", ts, err)
		}
	}
	plain := ansiPattern.ReplaceAllString(buf.String(), "")
	if !strings.Contains(plain, "[~] line-guard 线路 X") {
		t.Fatalf("既有前缀必须保持不变：%q", plain)
	}
}
