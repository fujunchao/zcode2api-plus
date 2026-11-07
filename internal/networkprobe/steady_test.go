package networkprobe

import (
	"errors"
	"testing"
)

// TestSteadyKbpsSkipsBufferWindow 验证稳态速率确实跳过了前 skipBytes 的缓冲吸收段。
//
// 构造的形态就是实测里最常见的那种：前 128KB 在个位数毫秒内写完（全是内核缓冲），
// 之后才按真实链路速率走。若不跳过，全程平均会被前段拉高。
func TestSteadyKbpsSkipsBufferWindow(t *testing.T) {
	marks := []tunnelMark{
		{bytes: 32 << 10, ms: 1},
		{bytes: 64 << 10, ms: 2},
		{bytes: 96 << 10, ms: 3},
		{bytes: 128 << 10, ms: 4},
		{bytes: 384 << 10, ms: 1000},
		{bytes: 640 << 10, ms: 2000},
		{bytes: 896 << 10, ms: 3000},
		{bytes: 1024 << 10, ms: 4000},
	}

	kbps, ok := steadyKbps(marks, 128<<10)
	if !ok {
		t.Fatal("应能算出稳态速率")
	}
	// 从 128KB@4ms 到 1024KB@4000ms：dw = 896KB，dt = 3996ms
	// kbps = 917504 * 1000 / 3996 / 1024 ≈ 224
	if kbps < 215 || kbps > 235 {
		t.Fatalf("稳态速率应约为 224 KB/s，实际 %d", kbps)
	}

	// 全程平均：1024KB / 4000ms ≈ 256 KB/s，明显高于稳态值。
	overall := int64(1024<<10) * 1000 / 4000 / 1024
	if overall <= kbps {
		t.Fatalf("全程平均(%d)应高于稳态(%d)，否则说明测试构造不出「前段被缓冲抬高」的形态", overall, kbps)
	}
}

func TestSteadyKbpsInsufficientSamples(t *testing.T) {
	if _, ok := steadyKbps(nil, 128<<10); ok {
		t.Fatal("空采样不应给出结果")
	}
	// 只有一个点：无法构成区间。
	if _, ok := steadyKbps([]tunnelMark{{bytes: 200 << 10, ms: 100}}, 128<<10); ok {
		t.Fatal("单点不应给出结果")
	}
	// 全部落在 skipBytes 之前：稳态区间为空。
	marks := []tunnelMark{{bytes: 32 << 10, ms: 1}, {bytes: 64 << 10, ms: 2}}
	if _, ok := steadyKbps(marks, 128<<10); ok {
		t.Fatal("全程未越过 skipBytes 时不应给出结果")
	}
	// 时间不前进：除零保护。
	marks = []tunnelMark{{bytes: 128 << 10, ms: 5}, {bytes: 256 << 10, ms: 5}}
	if _, ok := steadyKbps(marks, 128<<10); ok {
		t.Fatal("时间未前进时不应给出结果")
	}
}

// TestJudgeTunnelOKRequiresSteadyRate 是本轮改造的核心断言：
// 写满但稳态速率不达标的线路必须判失败。
//
// 旧判据「写满即 OK」把 minKbps 变成了隐式条件（size/budget 恰好等于速率底线），
// 一旦有人下调 size 就失守——2026-11 的 4MiB/30s + 显式 steady 判据就是为了堵这个。
func TestJudgeTunnelOKRequiresSteadyRate(t *testing.T) {
	base := tunnelJudgement{
		bytes:    4 << 20,
		size:     4 << 20,
		ms:       30000,
		budgetMS: 30000,
		minBytes: 512 << 10,
		minKbps:  85,
	}
	base.kbps = 100
	if !judgeTunnelOK(base) {
		t.Fatal("写满且稳态达标应判 OK")
	}

	slow := base
	slow.kbps = 20
	if judgeTunnelOK(slow) {
		t.Fatal("写满但稳态不达标必须判失败")
	}
}

func TestJudgeTunnelOKEarlyStop(t *testing.T) {
	ok := tunnelJudgement{
		bytes:    1 << 20,
		size:     4 << 20,
		ms:       10000,
		budgetMS: 30000,
		minBytes: 512 << 10,
		minKbps:  85,
		kbps:     200,
		writeErr: errors.New("broken pipe"),
	}
	if !judgeTunnelOK(ok) {
		t.Fatal("早停但已写够、够快、够早，应判 OK（对端提前关闭是正常现象）")
	}

	lateStop := ok
	lateStop.ms = 25000 // 超过预算的 2/3
	if judgeTunnelOK(lateStop) {
		t.Fatal("早停但用时过长，应判失败")
	}

	fewBytes := ok
	fewBytes.bytes = 64 << 10 // 低于 minBytes
	if judgeTunnelOK(fewBytes) {
		t.Fatal("早停但写入量不足，应判失败")
	}

	noErr := ok
	noErr.writeErr = nil // 既没写满也没错误，属于异常
	if judgeTunnelOK(noErr) {
		t.Fatal("未写满且无写错误，应判失败")
	}
}
