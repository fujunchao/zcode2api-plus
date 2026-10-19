package adminapi

import (
	"os"
	"testing"

	"zcode2api/internal/config"
)

// 巡检相关用例会构造 modelprobe.Manager；深度探针真会建隧道往外部写字节，
// 测试里统一关闭，避免打外网与桩服务收到 CONNECT。
func TestMain(m *testing.M) {
	old := config.DeepProbe
	config.DeepProbe = "off"
	code := m.Run()
	config.DeepProbe = old
	os.Exit(code)
}
