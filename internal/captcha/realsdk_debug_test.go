// 临时真机调试用例：真实浏览器走完整 newRodWorker + Solve 路径（CI 不跑）。
package captcha

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestRealSDKPageLoad(t *testing.T) {
	if os.Getenv("ZCODE_REAL_BROWSER_TEST") == "" {
		t.Skip("设 ZCODE_REAL_BROWSER_TEST=1 才运行真机调试用例")
	}
	bin := os.Getenv("ZCODE_CAPTCHA_BROWSER_BIN")
	if bin == "" {
		t.Skip("未设 ZCODE_CAPTCHA_BROWSER_BIN")
	}
	cfg := NewManager().FetchConfig(context.Background())
	t.Logf("真实配置: enabled=%v region=%q prefix=%q scene=%q", cfg.Enabled, cfg.Region, cfg.Prefix, cfg.SceneID)
	if !cfg.Enabled {
		t.Skip("上游未启用验证码")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	w, err := newRodWorker(ctx, bin, cfg)
	if err != nil {
		t.Fatalf("newRodWorker: %v", err)
	}
	defer w.Close()
	token, err := w.Solve(ctx)
	if err != nil {
		t.Fatalf("Solve: %v", err)
	}
	t.Logf("token 长度: %d", len(token))
}
