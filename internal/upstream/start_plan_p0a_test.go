package upstream

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"zcode2api/internal/config"
)

type startPlanGoldenShape struct {
	Source  string `json:"source"`
	Profile struct {
		BarePlatform string `json:"bare_platform"`
		Arch         string `json:"arch"`
		OSRelease    string `json:"os_release"`
		Shell        string `json:"shell"`
		CWD          string `json:"cwd"`
	} `json:"profile"`
	Blocks                int      `json:"blocks"`
	BlockChars            []int    `json:"block_chars"`
	BlockBytes            []int    `json:"block_bytes"`
	CacheControl          []bool   `json:"cache_control"`
	Block2Prefix          string   `json:"block2_prefix"`
	Block2Contains        []string `json:"block2_contains"`
	Forbidden             string   `json:"forbidden"`
	CurrentDateBlockChars int      `json:"current_date_block_chars"`
}

func loadStartPlanGolden(t *testing.T) startPlanGoldenShape {
	t.Helper()
	raw, err := os.ReadFile("testdata/start_plan_system_shape.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden startPlanGoldenShape
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	return golden
}

func goldenProfile(g startPlanGoldenShape) config.ZcodeProfile {
	return config.ZcodeProfile{
		BarePlatform: g.Profile.BarePlatform,
		Arch:         g.Profile.Arch,
		OSRelease:    g.Profile.OSRelease,
		Shell:        g.Profile.Shell,
		CWD:          g.Profile.CWD,
	}
}

func TestStartPlanFallbackSystemGoldenShape(t *testing.T) {
	golden := loadStartPlanGolden(t)
	blocks := BuildFallbackSystem(nil, goldenProfile(golden))
	if len(blocks) != golden.Blocks {
		t.Fatalf("block 数不符: got=%d want=%d", len(blocks), golden.Blocks)
	}
	for i, raw := range blocks {
		block := raw.(map[string]any)
		text := block["text"].(string)
		if got, want := len([]rune(text)), golden.BlockChars[i]; got != want {
			t.Errorf("block %d 字符数不符: got=%d want=%d", i, got, want)
		}
		if got, want := len([]byte(text)), golden.BlockBytes[i]; got != want {
			t.Errorf("block %d 字节数不符: got=%d want=%d", i, got, want)
		}
		_, hasCache := block["cache_control"]
		if hasCache != golden.CacheControl[i] {
			t.Errorf("block %d cache_control 不符: got=%t want=%t", i, hasCache, golden.CacheControl[i])
		}
	}
	dynamic := blocks[2].(map[string]any)["text"].(string)
	if !strings.HasPrefix(dynamic, golden.Block2Prefix) {
		t.Fatalf("block 2 前缀不符: %q", dynamic[:min(80, len(dynamic))])
	}
	for _, want := range golden.Block2Contains {
		if !strings.Contains(dynamic, want) {
			t.Errorf("block 2 缺少 %q", want)
		}
	}
	for _, raw := range blocks {
		if strings.Contains(raw.(map[string]any)["text"].(string), golden.Forbidden) {
			t.Fatalf("不得再出现 %q", golden.Forbidden)
		}
	}
	currentDate := currentDateBlock(time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC), time.UTC)
	if got := len([]rune(currentDate["text"].(string))); got != golden.CurrentDateBlockChars {
		t.Fatalf("currentDate 块字符数不符: got=%d want=%d", got, golden.CurrentDateBlockChars)
	}
}

func TestBuildFallbackSystemMergesThirdPartyTextIntoBlockTwo(t *testing.T) {
	golden := loadStartPlanGolden(t)
	profile := goldenProfile(golden)
	for _, tc := range []struct {
		name   string
		system any
		want   []string
	}{
		{"string", "第三方字符串指令", []string{"第三方字符串指令"}},
		{"blocks", []any{
			map[string]any{"type": "text", "text": "第三方块 A"},
			map[string]any{"type": "text", "text": "第三方块 B", "caller_metadata": "保留文本"},
		}, []string{"第三方块 A", "第三方块 B"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := BuildFallbackSystem(map[string]any{"system": tc.system}, profile)
			if len(blocks) != 3 {
				t.Fatalf("第三方 system 合并后必须保持 3 块，实际 %d", len(blocks))
			}
			dynamic := blocks[2].(map[string]any)["text"].(string)
			for _, want := range tc.want {
				if !strings.Contains(dynamic, want) {
					t.Fatalf("block 2 缺少第三方文本 %q", want)
				}
			}
			if !strings.HasPrefix(dynamic, golden.Block2Prefix) {
				t.Fatal("合并第三方文本不能破坏 dynamic 块的官方前缀")
			}
		})
	}
}

func TestInjectContextPrefixMergesIntoFirstUserMessage(t *testing.T) {
	loc := time.FixedZone("Asia/Shanghai", 8*60*60)
	now := time.Date(2026, 10, 27, 17, 30, 0, 0, time.UTC)
	body := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "mcs"},
		map[string]any{"role": "user", "content": "第一问"},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "第二问"}}},
	}}
	InjectContextPrefix(body, now, loc)
	messages := body["messages"].([]any)
	first := messages[1].(map[string]any)
	content := first["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("首条 user 应合并为 currentDate + 原文两块，实际 %d", len(content))
	}
	dateBlock := content[0].(map[string]any)
	dateText := dateBlock["text"].(string)
	for _, want := range []string{"<system-reminder>", "# currentDate", "Today's date is 2026-10-28.", "</system-reminder>\n"} {
		if !strings.Contains(dateText, want) {
			t.Fatalf("currentDate 块缺少 %q: %q", want, dateText)
		}
	}
	if _, ok := dateBlock["cache_control"]; ok {
		t.Fatal("currentDate 块不应带 cache_control")
	}
	if content[1].(map[string]any)["text"] != "第一问" {
		t.Fatalf("用户原文应保留在第二块: %v", content[1])
	}
	if messages[2].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] != "第二问" {
		t.Fatal("只应改首条 user 消息")
	}
}

func TestInjectContextPrefixArrayAndDuplicateGuard(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 10, 28, 1, 0, 0, 0, time.UTC)
	body := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "图片说明"},
			map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "AA=="}},
		}},
	}}
	InjectContextPrefix(body, now, loc)
	first := body["messages"].([]any)[0].(map[string]any)
	content := first["content"].([]any)
	if len(content) != 3 || content[0].(map[string]any)["type"] != "text" {
		t.Fatalf("数组 content 应前置 currentDate 块: %v", content)
	}
	InjectContextPrefix(body, now, loc)
	content = body["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(content) != 3 {
		t.Fatalf("重复调用不得再次插入 currentDate: %d", len(content))
	}
	count := 0
	for _, raw := range content {
		if text, _ := raw.(map[string]any)["text"].(string); strings.Contains(text, "# currentDate") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("currentDate 只能出现一次，实际 %d", count)
	}

	unchanged := map[string]any{"messages": []any{map[string]any{
		"role": "user", "content": "已有 # currentDate 的客户端文本",
	}}}
	before := unchanged["messages"].([]any)[0].(map[string]any)["content"]
	InjectContextPrefix(unchanged, now, loc)
	if unchanged["messages"].([]any)[0].(map[string]any)["content"] != before {
		t.Fatal("首条 user 已含 currentDate 时必须原样跳过")
	}
}
