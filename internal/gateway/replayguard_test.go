// 重放防护的单元测试：登记/命中/过期/容量上限。
package gateway

import (
	"testing"
	"time"

	"zcode2api/internal/config"
)

// withoutReplayAllowance 关闭「判定后放行一次」，用于验证防护闸本身仍然生效。
// 默认开（见 config.ReplayAllowOnce），需要「第二枪必被挡」的用例显式关掉。
func withoutReplayAllowance(t *testing.T) {
	t.Helper()
	old := config.ReplayAllowOnce
	config.ReplayAllowOnce = false
	t.Cleanup(func() { config.ReplayAllowOnce = old })
}

func TestReplayGuardBlocksWithinTTL(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	g := NewReplayGuard(60*time.Second, func() time.Time { return now })
	if g == nil {
		t.Fatal("TTL > 0 时不得返回 nil")
	}
	key := "GLM-5.3|deadbeef1234"

	if g.Blocked(key) {
		t.Fatal("未登记前不应命中")
	}
	g.Record(key)
	if !g.Blocked(key) {
		t.Fatal("登记后、窗口内应命中")
	}

	// 时钟推进到窗口边界之外：过期并自清理。
	now = now.Add(61 * time.Second)
	if g.Blocked(key) {
		t.Fatal("窗口外不应命中")
	}
}

func TestDiagnosticReplayRemainingDoesNotExtendWindow(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	g := NewReplayGuard(time.Minute, func() time.Time { return now })
	g.Record("content")
	for i := range 60 {
		if got := g.Remaining("content"); got != time.Duration(60-i)*time.Second || !g.Blocked("content") {
			t.Fatalf("读取剩余时间不得续期，i=%d remaining=%s", i, got)
		}
		now = now.Add(time.Second)
	}
	if g.Remaining("content") != 0 || g.Blocked("content") || len(g.entries) != 0 {
		t.Fatal("边界到期必须清理，与原 Blocked 契约相同")
	}
	var disabled *ReplayGuard
	if disabled.Remaining("content") != 0 || g.Remaining("") != 0 {
		t.Fatal("禁用/空键不应报告保护窗口")
	}
}

// 不同的内容指纹互不影响；空键安全跳过。
func TestReplayGuardKeysAreIndependent(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	g := NewReplayGuard(60*time.Second, func() time.Time { return now })
	g.Record("A|hash-a")
	if g.Blocked("B|hash-b") {
		t.Fatal("不同键不得互相命中")
	}
	g.Record("")
	if g.Blocked("") {
		t.Fatal("空键不得登记/命中")
	}
}

// 容量触顶整体清空：宁可放行（再走一遍账号）也不让表无限增长。
func TestReplayGuardEvictsAtCapacity(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	g := NewReplayGuard(60*time.Second, func() time.Time { return now })
	for i := 0; i < replayGuardMaxEntries+1; i++ {
		g.Record(string(rune('a'+i%26)) + "-" + time.Unix(int64(i), 0).String())
	}
	if len(g.entries) > replayGuardMaxEntries {
		t.Fatalf("触顶后应清空: %d", len(g.entries))
	}
}

// TTL <= 0 = 禁用（nil），调用方判空跳过。
func TestReplayGuardDisabledOnNonPositiveTTL(t *testing.T) {
	if NewReplayGuard(0, nil) != nil {
		t.Fatal("TTL=0 应禁用")
	}
	if NewReplayGuard(-1, nil) != nil {
		t.Fatal("负 TTL 应禁用")
	}
	// nil 接收者安全。
	var g *ReplayGuard
	if g.Blocked("k") || func() bool { g.Record("k"); return false }() {
		t.Fatal("nil 接收者应安全跳过")
	}
}

// ContentKey 稳定性：同 body 同模型同键；模型不同键不同；body 变化键变化。
func TestContentKeyStable(t *testing.T) {
	body := map[string]any{"model": "GLM-5.3", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	k1 := ContentKey("GLM-5.3", body)
	k2 := ContentKey("GLM-5.3", body)
	if k1 == "" || k1 != k2 {
		t.Fatalf("同内容应产出稳定键: %q vs %q", k1, k2)
	}
	if k1[:len("GLM-5.3|")] != "GLM-5.3|" {
		t.Fatalf("键应以模型名开头: %q", k1)
	}
	other := ContentKey("GLM-5.3-Flash", body)
	if other == k1 {
		t.Fatal("不同模型的键应不同")
	}
	body2 := map[string]any{"model": "GLM-5.3", "messages": []any{map[string]any{"role": "user", "content": "hi!"}}}
	if ContentKey("GLM-5.3", body2) == k1 {
		t.Fatal("内容变化键应变化")
	}
}

// 判定成立后允许同内容再试一次：真正的请求级风控会让这次重试再次撞墙，
// 而「三条脏线连击」会被这次重试救回来（2026-10-18 日志）。
func TestReplayTakeAllowsOncePerWindow(t *testing.T) {
	old := config.ReplayAllowOnce
	config.ReplayAllowOnce = true
	t.Cleanup(func() { config.ReplayAllowOnce = old })

	now := time.Unix(1_800_000_000, 0)
	g := NewReplayGuard(time.Minute, func() time.Time { return now })
	g.Record("k")

	if blocked, _ := g.Take("k"); blocked {
		t.Fatal("判定后第一次应放行")
	}
	if blocked, remaining := g.Take("k"); !blocked || remaining <= 0 {
		t.Fatal("放行余额用完后应拦截")
	}
	// 同窗口重复登记不得重新武装：否则客户端重试风暴会把脏线当免费额度。
	g.Record("k")
	if blocked, _ := g.Take("k"); !blocked {
		t.Fatal("同窗口重复登记不得再次放行")
	}
	// 新窗口重新武装一次。
	now = now.Add(61 * time.Second)
	g.Record("k")
	if blocked, _ := g.Take("k"); blocked {
		t.Fatal("新窗口应重新放行一次")
	}
}

func TestReplayTakeRespectsSwitchOff(t *testing.T) {
	old := config.ReplayAllowOnce
	config.ReplayAllowOnce = false
	t.Cleanup(func() { config.ReplayAllowOnce = old })

	g := NewReplayGuard(time.Minute, time.Now)
	g.Record("k")
	if blocked, _ := g.Take("k"); !blocked {
		t.Fatal("关闭一次性放行后应立即拦截")
	}
}
