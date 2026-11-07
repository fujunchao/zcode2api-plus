package gateway

import (
	"errors"
	"strings"
	"testing"
	"time"

	"zcode2api/internal/model"
	"zcode2api/internal/store"
)

// TestDiagFormatIncludesFRTTotal 验证诊断行输出 frt_total。
//
// 这个字段存在的唯一理由：firstbyte 只等于**最后一次**尝试，有重试时会严重低估
// 用户实际等待（实测 firstbyte=1474ms 对应真实等待 121965ms）。没有 frt_total，
// 看日志的人会把「重试放大」误判成「一切正常」。
func TestDiagFormatIncludesFRTTotal(t *testing.T) {
	diag := NewReqDiag("req-frt-1", "GLM-5.3", true, map[string]any{"model": "GLM-5.3"}, nil)
	diag.AttemptsElapsed = 121965 * time.Millisecond
	out := diag.Format()
	if !strings.Contains(out, "frt_total=121965ms") {
		t.Fatalf("诊断行应包含 frt_total=121965ms，实际:\n%s", out)
	}
}

func TestDiagFRTTotalZeroPrintsDash(t *testing.T) {
	// 零出站（选不出号、重放防护命中）时是合法路径，必须与「测到 0ms」区分开。
	diag := NewReqDiag("req-frt-2", "GLM-5.3", true, nil, nil)
	out := diag.Format()
	if !strings.Contains(out, "frt_total=-") {
		t.Fatalf("未出站时应打印 -，实际:\n%s", out)
	}
}

// TestDiagAccumulatesAttemptsElapsed 验证多次尝试的墙钟被累加。
// 这是 frt_total 的数据来源：每次 client.Do 前后各取一次时间。
func TestDiagAccumulatesAttemptsElapsed(t *testing.T) {
	now := time.Unix(1700000000, 0)
	clock := func() time.Time { return now }
	diag := NewReqDiag("req-frt-3", "GLM-5.3", true, map[string]any{"model": "GLM-5.3"}, clock)

	acc := &model.Account{ID: "acc-1", Name: "号1"}
	egress := store.ProxyEgress{ID: "proxy-1", Label: "线1", Mode: "proxy"}
	someErr := errors.New("upstream boom")

	finish1 := diag.BeginUpstreamAttempt(acc, egress, nil)
	now = now.Add(10 * time.Millisecond)
	finish1(nil, someErr)

	finish2 := diag.BeginUpstreamAttempt(acc, egress, nil)
	now = now.Add(30 * time.Millisecond)
	finish2(nil, someErr)

	if got := diag.AttemptsElapsed; got != 40*time.Millisecond {
		t.Fatalf("累计尝试耗时应为 40ms，实际 %v", got)
	}
	if diag.upstreamAttempts != 2 {
		t.Fatalf("上游尝试次数应为 2，实际 %d", diag.upstreamAttempts)
	}
}
