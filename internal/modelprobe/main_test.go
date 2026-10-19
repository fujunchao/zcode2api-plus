package modelprobe_test

import (
	"os"
	"testing"

	"zcode2api/internal/config"
)

// 深度探针会真的建隧道、往外写字节。单测只验证判定逻辑，不做出站探测：
// 把它关掉，既避免测试打外网，也避免桩服务收到 CONNECT 而被误判成异常请求。
func TestMain(m *testing.M) {
	old := config.DeepProbe
	config.DeepProbe = "off"
	code := m.Run()
	config.DeepProbe = old
	os.Exit(code)
}
