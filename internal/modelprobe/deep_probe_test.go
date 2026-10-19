package modelprobe

import (
	"context"
	"io"
	"testing"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/networkprobe"
	"zcode2api/internal/store"
	"zcode2api/internal/web"
)

// 深探的三种模式：off 不探、shadow 只记录、enforce 才改判定。
func TestWithDeepProbeModes(t *testing.T) {
	web.SetOut(io.Discard)
	t.Cleanup(func() { web.SetOut(nil) })

	oldMode, oldBudget := config.DeepProbe, config.DeepBudget
	t.Cleanup(func() { config.DeepProbe, config.DeepBudget = oldMode, oldBudget })
	config.DeepBudget = 300 * time.Millisecond

	// 端口 1 必然连不上：隧道一定失败。
	p := store.ProxyProfile{ID: "proxy-x", URL: "http://127.0.0.1:1"}
	base := networkprobe.Report{OK: true}

	config.DeepProbe = "off"
	if got := withDeepProbe(context.Background(), p, base, true); got.Tunnel != nil {
		t.Fatalf("off 模式不应建隧道：%+v", got.Tunnel)
	}

	config.DeepProbe = "shadow"
	if got := withDeepProbe(context.Background(), p, base, false); got.Tunnel != nil {
		t.Fatal("非 first 阶段不应建隧道（preflight/direct/second 都不该深探）")
	}
	got := withDeepProbe(context.Background(), p, base, true)
	if got.Tunnel == nil {
		t.Fatal("shadow 模式必须留下测量结果")
	}
	if !got.OK {
		t.Fatal("shadow 不得改变判定")
	}
	if got.Tunnel.OK {
		t.Fatalf("连不上的代理必须判隧道失败：%+v", got.Tunnel)
	}

	config.DeepProbe = "enforce"
	got = withDeepProbe(context.Background(), p, base, true)
	if got.OK {
		t.Fatal("enforce 下隧道失败必须并入判定")
	}
}

func TestDeepProbeModeNormalization(t *testing.T) {
	old := config.DeepProbe
	t.Cleanup(func() { config.DeepProbe = old })
	for raw, want := range map[string]string{"off": "off", "ENFORCE": "enforce", "shadow": "shadow", "": "shadow", "乱写": "shadow"} {
		config.DeepProbe = raw
		if got := config.DeepProbeMode(); got != want {
			t.Fatalf("DeepProbeMode(%q)=%q，期望 %q", raw, got, want)
		}
	}
}
