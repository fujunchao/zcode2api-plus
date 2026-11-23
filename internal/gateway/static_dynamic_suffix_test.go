package gateway

import (
	"encoding/json"
	"maps"
	"strings"
	"testing"

	"zcode2api/internal/config"
	"zcode2api/internal/upstream"
)

// TestNormalizeBodyStaticDynamicSuffix 在实际整形入口复现3182块内后缀进入3191网关：
// 开启保留时原样透传，关闭时只保留新版底座和调用者后缀，重复请求不得继续叠加。
func TestNormalizeBodyStaticDynamicSuffix(t *testing.T) {
	oldVersion, oldUA, oldPreserve := config.ZcodeClientVersion, config.UserAgent, config.PreserveClientContext
	t.Cleanup(func() {
		config.ZcodeClientVersion, config.UserAgent, config.PreserveClientContext = oldVersion, oldUA, oldPreserve
	})
	config.ZcodeClientVersion, config.UserAgent = "3.18.2", "ZCode/3.18.2"
	caller := upstream.ZcodeSystemBlocks()
	const suffix = "# Context management\n只使用本地客户端提供的工具。"
	third := maps.Clone(caller[2].(map[string]any))
	third["text"] = third["text"].(string) + "\n\n" + suffix
	caller[2] = third
	original, err := json.Marshal(caller)
	if err != nil {
		t.Fatal(err)
	}

	config.ZcodeClientVersion, config.UserAgent = "3.19.1", "ZCode/3.19.1"
	base := upstream.ZcodeSystemBlocks()[2].(map[string]any)["text"].(string)
	for _, tc := range []struct {
		name     string
		preserve bool
	}{
		{"保留客户端上下文", true},
		{"强制回退", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config.PreserveClientContext = tc.preserve
			body := map[string]any{
				"model":    "GLM-5.3",
				"system":   caller,
				"messages": []any{map[string]any{"role": "user", "content": "检查项目"}},
			}
			NormalizeBody(body, true)
			blocks, ok := body["system"].([]any)
			if !ok || len(blocks) != 3 {
				t.Fatalf("整形后的 system 应为三块: %#v", body["system"])
			}
			if tc.preserve {
				actual, err := json.Marshal(blocks)
				if err != nil || string(actual) != string(original) {
					t.Fatal("保留模式不得重写调用者的旧第三块和后缀")
				}
			} else if got := blocks[2].(map[string]any)["text"].(string); got != base+"\n\n"+suffix {
				t.Fatalf("强制回退应只保留3191底座与后缀，实际 %d B，旧词元提示=%t", len(got), strings.Contains(got, "<total_tokens>"))
			}
			after, err := json.Marshal(caller)
			if err != nil || string(after) != string(original) {
				t.Fatal("整形不得修改调用者持有的原始 system")
			}
			first, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			var repeated map[string]any
			if err := json.Unmarshal(first, &repeated); err != nil {
				t.Fatal(err)
			}
			NormalizeBody(repeated, true)
			second, err := json.Marshal(repeated)
			if err != nil || string(second) != string(first) {
				t.Fatal("JSON往返后重复整形不应叠加模板或改变后缀")
			}
		})
	}
}
