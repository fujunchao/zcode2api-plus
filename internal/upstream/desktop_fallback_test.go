package upstream

import (
	"strings"
	"testing"

	"zcode2api/internal/config"
)

func TestPrefixCacheLayoutAcrossProfiles(t *testing.T) {
	oldSource, oldInject := config.ZcodeClientSourceTitle, config.UpstreamInjectEnvironment
	t.Cleanup(func() {
		config.ZcodeClientSourceTitle, config.UpstreamInjectEnvironment = oldSource, oldInject
	})
	config.UpstreamInjectEnvironment = false

	for _, source := range []string{"electron", "cli"} {
		t.Run(source, func(t *testing.T) {
			config.ZcodeClientSourceTitle = source
			blocks := ZcodeSystemBlocks()
			if len(blocks) != 2 {
				t.Fatalf("静态段应保持 2 块，实际 %d", len(blocks))
			}
			first := blocks[0].(map[string]any)
			if _, exists := first["cache_control"]; exists {
				t.Fatal("所有档案的固定前缀块都不得附 cache_control")
			}
			cache, _ := blocks[1].(map[string]any)["cache_control"].(map[string]any)
			if cache["type"] != "ephemeral" {
				t.Fatal("稳定身份块应保留 ephemeral")
			}
		})
	}
}

func TestFallbackHasNoCapabilitiesFourthBlock(t *testing.T) {
	oldSource, oldInject := config.ZcodeClientSourceTitle, config.UpstreamInjectEnvironment
	t.Cleanup(func() { config.ZcodeClientSourceTitle, config.UpstreamInjectEnvironment = oldSource, oldInject })
	config.UpstreamInjectEnvironment = false

	for _, source := range []string{"electron", "cli"} {
		t.Run(source, func(t *testing.T) {
			config.ZcodeClientSourceTitle = source
			blocks := ZcodeFallbackSystemBlocks()
			if len(blocks) != 3 {
				t.Fatalf("回退路径应固定 3 块，实际 %d", len(blocks))
			}
			first := blocks[0].(map[string]any)
			if _, exists := first["cache_control"]; exists {
				t.Fatal("block 0 不得附 cache_control")
			}
			for i := 1; i < len(blocks); i++ {
				cache, _ := blocks[i].(map[string]any)["cache_control"].(map[string]any)
				if cache["type"] != "ephemeral" {
					t.Fatalf("block %d 应保留 ephemeral", i)
				}
			}
			dynamic, _ := blocks[2].(map[string]any)["text"].(string)
			if !strings.HasPrefix(dynamic, "\n\n# Communicating with the user") {
				t.Fatalf("block 2 应以两个换行和 dynamic 首段开头: %q", dynamic[:min(80, len(dynamic))])
			}
			for _, want := range []string{"# Environment", "# Context management"} {
				if !strings.Contains(dynamic, want) {
					t.Fatalf("block 2 缺少 %q", want)
				}
			}
			for _, raw := range blocks {
				if strings.Contains(raw.(map[string]any)["text"].(string), "# Gateway Desktop Capabilities") {
					t.Fatal("不得再出现独立的 Gateway Desktop Capabilities 段")
				}
			}
		})
	}
}

func TestSystemBlockObjectsAndNestedCachesAreIsolated(t *testing.T) {
	oldSource := config.ZcodeClientSourceTitle
	config.ZcodeClientSourceTitle = "electron"
	t.Cleanup(func() { config.ZcodeClientSourceTitle = oldSource })

	first, second := ZcodeFallbackSystemBlocks(), ZcodeFallbackSystemBlocks()
	for _, raw := range first {
		block := raw.(map[string]any)
		block["text"] = "污染"
		if cache, ok := block["cache_control"].(map[string]any); ok {
			cache["type"] = "污染"
		}
	}
	for _, blocks := range [][]any{second, ZcodeFallbackSystemBlocks()} {
		for i, raw := range blocks {
			block := raw.(map[string]any)
			if block["text"] == "污染" {
				t.Fatal("调用者不得污染静态块或其他请求")
			}
			if i == 0 {
				if _, ok := block["cache_control"]; ok {
					t.Fatal("block 0 不应带缓存")
				}
				continue
			}
			if block["cache_control"].(map[string]any)["type"] != "ephemeral" {
				t.Fatal("嵌套缓存也必须逐请求复制")
			}
		}
	}
}
