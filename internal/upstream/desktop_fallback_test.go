package upstream

import (
	"reflect"
	"strings"
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

func TestDesktopCapabilitiesLimitedToGeneratedFallback(t *testing.T) {
	oldSource, oldInject := config.ZcodeClientSourceTitle, config.UpstreamInjectEnvironment
	t.Cleanup(func() { config.ZcodeClientSourceTitle, config.UpstreamInjectEnvironment = oldSource, oldInject })
	config.UpstreamInjectEnvironment = false
	for _, source := range []string{"electron", "cli"} {
		config.ZcodeClientSourceTitle = source
		standard, fallback := ZcodeSystemBlocks(), ZcodeFallbackSystemBlocks()
		if source != "electron" {
			if !reflect.DeepEqual(standard, fallback) {
				t.Fatal("非桌面档案不得追加 Gen UI 能力段")
			}
			if _, cached := standard[0].(map[string]any)["cache_control"]; !cached {
				t.Fatal("本批不得改动 CLI 首块缓存行为")
			}
			continue
		}
		if len(fallback) != len(standard)+1 || !reflect.DeepEqual(fallback[:len(standard)], standard) {
			t.Fatal("桌面回退应保留官方固定块，独立追加真实能力")
		}
		last := fallback[len(fallback)-1].(map[string]any)
		text, _ := last["text"].(string)
		if !strings.Contains(text, "no Gen UI output directory") || !strings.Contains(text, "only the tools declared in this request") {
			t.Fatal("回退能力必须说明目录/渲染不可用且不虚构工具")
		}
		for _, block := range standard {
			if strings.Contains(block.(map[string]any)["text"].(string), "# Gateway Desktop Capabilities") {
				t.Fatal("官方前缀识别函数不能追加网关能力段")
			}
		}
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
			if i > 0 && block["cache_control"].(map[string]any)["type"] != "ephemeral" {
				t.Fatal("嵌套缓存也必须逐请求复制")
			}
		}
	}
}
