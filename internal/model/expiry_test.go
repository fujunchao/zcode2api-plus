package model

import (
	"testing"
	"time"
)

// 到期门槛的判定必须守住三条边界：
//  1. 只认「用不完就作废」的额度列（one_time / 周月套餐），日额度不算；
//  2. 到期时刻优先 expires_at、缺失回落到 period_end，单位按量级归一；
//  3. 无余额、无法识别的数值一律跳过，绝不把长期池误判成即将作废。
func TestSoonestNonRefreshingExpiry(t *testing.T) {
	now := time.Unix(1_760_000_000, 0)
	day := 24 * time.Hour
	in2d := float64(now.Add(2 * day).Unix())
	in30d := float64(now.Add(30 * day).Unix())

	build := func(entries ...map[string]any) *Account {
		quota := map[string]map[string]any{}
		for i, e := range entries {
			e["model"] = "GLM-5.3"
			quota[string(rune('a'+i))] = e
		}
		return &Account{Quota: quota}
	}

	t.Run("one_time 窗口内到期且有余量", func(t *testing.T) {
		a := build(map[string]any{
			"remaining": float64(100), "period": "one_time", "expires_at": in2d,
		})
		got, ok := a.SoonestNonRefreshingExpiry("GLM-5.3", now, 3*day)
		if !ok || got != in2d {
			t.Fatalf("应识别 one_time 到期：ok=%v got=%v", ok, got)
		}
	})

	t.Run("日额度不算作废", func(t *testing.T) {
		a := build(map[string]any{
			"remaining":    float64(100),
			"period":       "daily",
			"period_start": float64(now.Add(-12 * time.Hour).Unix()),
			"period_end":   float64(now.Add(12 * time.Hour).Unix()),
		})
		if _, ok := a.SoonestNonRefreshingExpiry("GLM-5.3", now, 3*day); ok {
			t.Fatal("跨度≈24h 的日额度不应进到期门槛")
		}
	})

	t.Run("月套餐按 period_end 兜底", func(t *testing.T) {
		a := build(map[string]any{
			"remaining":    float64(100),
			"period":       "monthly",
			"period_start": float64(now.Add(-28 * day).Unix()),
			"period_end":   in2d,
		})
		got, ok := a.SoonestNonRefreshingExpiry("GLM-5.3", now, 3*day)
		if !ok || got != in2d {
			t.Fatalf("应识别月套餐到期：ok=%v got=%v", ok, got)
		}
	})

	t.Run("无余额不参与", func(t *testing.T) {
		a := build(map[string]any{
			"remaining": float64(0), "period": "one_time", "expires_at": in2d,
		})
		if _, ok := a.SoonestNonRefreshingExpiry("GLM-5.3", now, 3*day); ok {
			t.Fatal("余额为 0 的额度列不应触发门槛")
		}
	})

	t.Run("窗口外不参与", func(t *testing.T) {
		a := build(map[string]any{
			"remaining": float64(100), "period": "one_time", "expires_at": in30d,
		})
		if _, ok := a.SoonestNonRefreshingExpiry("GLM-5.3", now, 3*day); ok {
			t.Fatal("30 天后到期不应进 3 天窗口")
		}
	})

	t.Run("已过期不参与", func(t *testing.T) {
		a := build(map[string]any{
			"remaining": float64(100), "period": "one_time",
			"expires_at": float64(now.Add(-time.Hour).Unix()),
		})
		if _, ok := a.SoonestNonRefreshingExpiry("GLM-5.3", now, 3*day); ok {
			t.Fatal("已过期的额度列不应被提为优先，快照余额可能已失效")
		}
	})

	t.Run("毫秒时间戳归一", func(t *testing.T) {
		a := build(map[string]any{
			"remaining": float64(100), "period": "one_time", "expires_at": in2d * 1000,
		})
		got, ok := a.SoonestNonRefreshingExpiry("GLM-5.3", now, 3*day)
		if !ok || got != in2d {
			t.Fatalf("毫秒时间戳应归一为秒：ok=%v got=%v want=%v", ok, got, in2d)
		}
	})

	t.Run("相对秒数不当作时间戳", func(t *testing.T) {
		a := build(map[string]any{
			"remaining": float64(100), "period": "one_time", "expires_at": float64(3600),
		})
		if _, ok := a.SoonestNonRefreshingExpiry("GLM-5.3", now, 3*day); ok {
			t.Fatal("3600 更像 TTL 而不是绝对时间戳，应跳过")
		}
	})

	t.Run("多列取最早", func(t *testing.T) {
		a := build(
			map[string]any{"remaining": float64(100), "period": "one_time", "expires_at": in30d},
			map[string]any{"remaining": float64(100), "period": "one_time", "expires_at": in2d},
		)
		got, ok := a.SoonestNonRefreshingExpiry("GLM-5.3", now, 3*day)
		if !ok || got != in2d {
			t.Fatalf("应取最早到期：ok=%v got=%v", ok, got)
		}
	})

	t.Run("窗口关闭", func(t *testing.T) {
		a := build(map[string]any{
			"remaining": float64(100), "period": "one_time", "expires_at": in2d,
		})
		if _, ok := a.SoonestNonRefreshingExpiry("GLM-5.3", now, 0); ok {
			t.Fatal("window=0 应关闭判定")
		}
	})
}
