package asyncpool

import (
	"testing"

	"zcode2api/internal/config"
)

// withoutReplayAllowance 关闭「判定后放行一次」，用于验证防护闸本身仍然生效。
// 默认开（见 config.ReplayAllowOnce）；需要「第二张同内容票必被挡」的用例显式关掉。
func withoutReplayAllowance(t *testing.T) {
	t.Helper()
	old := config.ReplayAllowOnce
	config.ReplayAllowOnce = false
	t.Cleanup(func() { config.ReplayAllowOnce = old })
}
