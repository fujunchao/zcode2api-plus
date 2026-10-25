package store

import (
	"testing"

	"zcode2api/internal/config"
)

// 巡检 405 的账号惩罚范围：默认只有 reference 阶段罚号，all 回旧行为，off 完全不写。
// 直接改 config 全局（该变量 init 已求值，t.Setenv 无效）；本用例不 t.Parallel。
func TestProbeRiskPenaltyApplies(t *testing.T) {
	old := config.ProbeRiskPenalty
	t.Cleanup(func() { config.ProbeRiskPenalty = old })

	cases := []struct {
		name, mode, role string
		want             bool
	}{
		{"默认-筛查不罚", "reference", "screen", false},
		{"默认-目标不罚", "reference", "target", false},
		{"默认-参考罚", "reference", "reference", true},
		{"回滚-筛查罚", "all", "screen", true},
		{"回滚-目标罚", "all", "target", true},
		{"回滚-参考罚", "all", "reference", true},
		{"关闭-参考不罚", "off", "reference", false},
		{"关闭-目标不罚", "off", "target", false},
		{"非法值按默认-目标不罚", "bogus", "target", false},
		{"非法值按默认-参考罚", "bogus", "reference", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config.ProbeRiskPenalty = tc.mode
			if got := probeRiskPenaltyApplies(tc.role); got != tc.want {
				t.Fatalf("mode=%s role=%s: got %v want %v", tc.mode, tc.role, got, tc.want)
			}
		})
	}
}
