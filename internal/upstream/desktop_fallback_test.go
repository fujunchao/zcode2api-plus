package upstream

import (
	"testing"

	"zcode2api/internal/config"
)

func TestDesktopFallbackPrefixHasNoCacheControl(t *testing.T) {
	oldSource, oldInject := config.ZcodeClientSourceTitle, config.UpstreamInjectEnvironment
	t.Cleanup(func() {
		config.ZcodeClientSourceTitle, config.UpstreamInjectEnvironment = oldSource, oldInject
	})
	config.ZcodeClientSourceTitle, config.UpstreamInjectEnvironment = "electron", false
	blocks := ZcodeSystemBlocks()
	if len(blocks) < 2 {
		t.Fatalf("桌面回退至少保留两个固定块：got=%d", len(blocks))
	}
	first := blocks[0].(map[string]any)
	if _, exists := first["cache_control"]; exists {
		t.Fatal("官方桌面固定前缀块不得附 cache_control")
	}
	for i, raw := range blocks[1:] {
		block := raw.(map[string]any)
		cache, _ := block["cache_control"].(map[string]any)
		if cache["type"] != "ephemeral" {
			t.Fatalf("后续稳定块 %d 应保留 ephemeral", i+1)
		}
	}
}
