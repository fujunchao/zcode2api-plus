package gateway

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"zcode2api/internal/config"
	"zcode2api/internal/upstream"
)

func TestNormalizeBodyModel(t *testing.T) {
	body := map[string]any{"model": "zai/glm-5.3"}
	NormalizeBody(body, false)
	if body["model"] != "GLM-5.3" {
		t.Fatalf("前缀剥离 + 映射不符: %v", body["model"])
	}

	// 不在映射表内的模型保持原样（白名单按 normalize 后比对）
	body2 := map[string]any{"model": "glm_5_3_flash"}
	NormalizeBody(body2, false)
	if body2["model"] != "glm_5_3_flash" {
		t.Fatalf("未映射模型不应改动: %v", body2["model"])
	}
}

func TestNormalizeBodyContentBridge(t *testing.T) {
	original := map[string]any{"role": "user", "content": "hi"}
	body := map[string]any{"messages": []any{original, map[string]any{
		"role":    "assistant",
		"content": []any{map[string]any{"type": "text", "text": "already-block"}},
	}}}
	NormalizeBody(body, false)

	msgs := body["messages"].([]any)
	first := msgs[0].(map[string]any)
	blocks, ok := first["content"].([]any)
	if !ok || len(blocks) != 1 {
		t.Fatalf("字符串 content 应桥接为 text block: %v", first)
	}
	if blocks[0].(map[string]any)["text"] != "hi" {
		t.Fatalf("桥接内容不符: %v", blocks[0])
	}
	// 原始消息 map 不被改写（对齐 Python 版的 {**msg, ...} 拷贝）
	if original["content"] != "hi" {
		t.Fatalf("原始 map 不应被改写: %v", original)
	}
	// 已是列表的 content 原样保留
	second := msgs[1].(map[string]any)
	if _, isString := second["content"].(string); isString {
		t.Fatal("列表 content 不应被二次桥接")
	}
}

func TestNormalizeBodySystemInjection(t *testing.T) {
	useLegacyClientContext(t)
	blocks := upstream.ZcodeFallbackSystemBlocks()

	// 键不存在 → 注入 blocks
	body := map[string]any{}
	NormalizeBody(body, true)
	sys, ok := body["system"].([]any)
	if !ok || len(sys) != len(blocks) {
		t.Fatalf("缺失 system 应整体注入: %v", body["system"])
	}

	// 字符串 system → 并入 block 2，块数仍为 3
	body2 := map[string]any{"system": "be brief"}
	NormalizeBody(body2, true)
	sys2 := body2["system"].([]any)
	if len(sys2) != len(blocks) {
		t.Fatalf("第三方 system 合并后应保持 %d 块，实际 %d", len(blocks), len(sys2))
	}
	if !strings.Contains(sys2[len(sys2)-1].(map[string]any)["text"].(string), "be brief") {
		t.Fatalf("用户文本应并入 dynamic 块: %v", sys2[len(sys2)-1])
	}

	// 列表 system → 并入 block 2，块数仍为 3
	userBlocks := []any{map[string]any{"type": "text", "text": "user"}}
	body3 := map[string]any{"system": userBlocks}
	NormalizeBody(body3, true)
	sys3 := body3["system"].([]any)
	if len(sys3) != len(blocks) {
		t.Fatalf("列表 system 合并后应保持 %d 块，实际 %d", len(blocks), len(sys3))
	}
	if !strings.Contains(sys3[len(sys3)-1].(map[string]any)["text"].(string), "user") {
		t.Fatal("用户块文本应并入 dynamic 块")
	}

	// 其它类型不动（对齐 Python elif 链）
	body4 := map[string]any{"system": 1.0}
	NormalizeBody(body4, true)
	if body4["system"] != 1.0 {
		t.Fatalf("未知类型不应改动: %v", body4["system"])
	}

	// 不需要注入时不改动
	body5 := map[string]any{}
	NormalizeBody(body5, false)
	if _, ok := body5["system"]; ok {
		t.Fatal("非 JWT 账号不应注入 system")
	}
}

// 已有网关标准块只保留一份；同文但缓存策略/附加字段不同的调用者块不能被误删。
// 通过 JSON 往返模拟下游重新提交同一请求，不依赖 Go 对象的指针身份。
func TestNormalizeBodyKnownSystemBlocksAreIdempotent(t *testing.T) {
	useLegacyClientContext(t)
	blocks := upstream.ZcodeFallbackSystemBlocks()
	const prefix = "You are ZCode, an interactive coding agent"
	// 当前 profile 的精确标准块去重；缓存/扩展不同的调用者块继续保留。
	known := blocks[0]
	user := map[string]any{"type": "text", "text": "保留调用者的原始指令"}
	tail := []any{
		user, user, // 不对任意相同用户段落去重。
		map[string]any{
			"type": "text", "text": prefix,
			"cache_control": map[string]any{"type": "ephemeral", "ttl": "1h"},
		},
		map[string]any{
			"type": "text", "text": prefix,
			"cache_control":   map[string]any{"type": "ephemeral"},
			"caller_metadata": "必须保留",
		},
		map[string]any{"type": "text", "text": prefix + "，这是调用者扩展的身份。"},
	}
	incoming := append([]any{known, known}, tail...)
	before, err := json.Marshal(incoming)
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"model": "GLM-5.3", "system": incoming}
	NormalizeBody(body, true)
	system := body["system"].([]any)
	if len(system) != len(blocks) {
		t.Fatalf("非官方 system 应并入 block 2，实际共 %d 块（标准块 %d）", len(system), len(blocks))
	}
	dynamic, _ := system[len(system)-1].(map[string]any)["text"].(string)
	for _, want := range []string{"保留调用者的原始指令", "调用者扩展的身份"} {
		if !strings.Contains(dynamic, want) {
			t.Fatalf("调用者段落 %q 未并入 block 2: %s", want, dynamic)
		}
	}
	// 带不同 cache/扩展字段的同一段前缀仍各自保留文本；合并后不再保留块级 metadata。
	if got := strings.Count(dynamic, prefix); got != 3 {
		t.Fatalf("调用者前缀块文本应保留 3 份，实际 %d", got)
	}
	after, err := json.Marshal(incoming)
	if err != nil || string(after) != string(before) {
		t.Fatal("规范化不得改写调用者持有的原始 system 切片或块")
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
		t.Fatal("重新提交已有标准块的请求，不应重复注入或改变调用者内容")
	}
}

func useLegacyClientContext(t *testing.T) {
	t.Helper()
	old := config.PreserveClientContext
	config.PreserveClientContext = false
	t.Cleanup(func() { config.PreserveClientContext = old })
}

// 首段自带官方前缀的调用者（官方系客户端）已拥有完整上下文：原样保留，
// 不改写其指令、缓存策略、顺序或扩展字段。
func TestNormalizeBodyOfficialSystemIsIdempotent(t *testing.T) {
	const payload = `{"model":"GLM-5.3","system":[
		{"type":"text","text":"You are ZCode, an interactive coding agent","cache_control":{"type":"ephemeral","ttl":"1h"}},
		{"type":"text","text":"重复但有意保留"},
		{"type":"text","text":"重复但有意保留","caller_metadata":"保留"}
	],"messages":[{"role":"user","content":"问题"}]}`
	var body map[string]any
	if err := json.Unmarshal([]byte(payload), &body); err != nil {
		t.Fatal(err)
	}
	originalSystem, _ := json.Marshal(body["system"])
	NormalizeBody(body, true)
	gotSystem, _ := json.Marshal(body["system"])
	if string(gotSystem) != string(originalSystem) {
		t.Fatal("不得改写调用者的指令、缓存策略、顺序或扩展字段")
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
	if err != nil || string(first) != string(second) {
		t.Fatal("JSON 往返并重复规范化不得增加或丢失上下文")
	}
}

// 第三方 harness（如 DeepSeek Harness）的首段不是官方前缀：必须回退旧版
// 「官方块前置 + 精确去重」——上游从第 0 字节逐字节校验到身份段末尾，缺前缀
// 必然 3012。2026-10-12 事件回归守卫：把保留条件放宽回「有 system 就保留」即变红。
func TestNormalizeBodyForeignSystemGetsOfficialPrefix(t *testing.T) {
	blocks := upstream.ZcodeFallbackSystemBlocks()
	foreign := []any{
		map[string]any{"type": "text", "text": "You are an AI agent powered by DeepSeek Harness."},
		map[string]any{"type": "text", "text": "第三方补充指令"},
	}
	body := map[string]any{"model": "GLM-5.3", "system": foreign}
	NormalizeBody(body, true)
	system := body["system"].([]any)
	if len(system) != len(blocks) {
		t.Fatalf("第三方 system 合并后应保持 %d 块，实际 %d", len(blocks), len(system))
	}
	for i := 0; i < len(blocks)-1; i++ {
		if !reflect.DeepEqual(system[i], blocks[i]) {
			t.Fatalf("第 %d 块应为官方标准块: %v", i, system[i])
		}
	}
	dynamic := ""
	for _, raw := range system {
		text, _ := raw.(map[string]any)["text"].(string)
		if strings.Contains(text, "# Communicating with the user") {
			dynamic = text
			break
		}
	}
	for _, want := range []string{"You are an AI agent powered by DeepSeek Runtime.", "第三方补充指令"} {
		if !strings.Contains(dynamic, want) {
			t.Fatalf("第三方内容 %q 必须保留在 block 2，不得被删除或改写", want)
		}
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
	if err != nil || string(first) != string(second) {
		t.Fatal("重复提交不得重复前置官方块（此时首段已是官方前缀，走保留分支）")
	}
}

// 字符串形态的第三方 system 同样回退三块 fallback，并并入 dynamic 块。
func TestNormalizeBodyForeignStringSystemGetsOfficialPrefix(t *testing.T) {
	blocks := upstream.ZcodeFallbackSystemBlocks()
	body := map[string]any{"model": "GLM-5.3", "system": "第三方指令"}
	NormalizeBody(body, true)
	system := body["system"].([]any)
	if len(system) != len(blocks) {
		t.Fatalf("字符串 system 合并后应保持 %d 块，实际 %d", len(blocks), len(system))
	}
	dynamic, _ := system[len(system)-1].(map[string]any)["text"].(string)
	if !strings.Contains(dynamic, "第三方指令") {
		t.Fatalf("第三方文本必须保留在 block 2: %v", system[len(system)-1])
	}
	firstText, _ := system[0].(map[string]any)["text"].(string)
	if !strings.HasPrefix(firstText, "You are ZCode, an interactive coding agent") {
		t.Fatalf("首段必须是官方固定前缀: %q", firstText)
	}
}

func TestInlineSystemOwnsContextWithoutTopLevelSystem(t *testing.T) {
	message := map[string]any{"role": "system", "content": []any{map[string]any{"type": "text", "text": "客户端后续规则"}}}
	body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "问题"}, message}}
	NormalizeBody(body, true)
	if _, injected := body["system"]; injected {
		t.Fatal("已有消息内系统指令时不能再补一份网关环境")
	}
	if _, ok := message["content"].([]any); !ok {
		t.Fatal("MCS 规范化不能改写调用者持有的原消息")
	}
}

// currentDate 只合并进网关自行补 system 的首条 user 消息；已有 currentDate 或调用者
// 自带官方 system 时必须保持原样，避免重复注入和破坏客户端上下文。
func TestNormalizeBodyInjectsCurrentDateOnlyOnFallback(t *testing.T) {
	oldPreserve := config.PreserveClientContext
	config.PreserveClientContext = true
	t.Cleanup(func() { config.PreserveClientContext = oldPreserve })

	body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "你好"}}}
	NormalizeBody(body, true)
	messages := body["messages"].([]any)
	content, ok := messages[0].(map[string]any)["content"].([]any)
	if !ok || len(content) != 2 {
		t.Fatalf("fallback 首条 user 应合并 currentDate + 原文: %v", messages[0])
	}
	if text, _ := content[0].(map[string]any)["text"].(string); !strings.Contains(text, "# currentDate") || !strings.Contains(text, "Today's date is 20") {
		t.Fatalf("currentDate 应按官方 system-reminder 形态生成: %q", text)
	}
	NormalizeBody(body, true)
	content = body["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("重复规范化不得重复插入 currentDate: %d", len(content))
	}

	official := "You are ZCode, an interactive coding agent\n调用者完整上下文"
	preserved := map[string]any{
		"system":   official,
		"messages": []any{map[string]any{"role": "user", "content": "你好"}},
	}
	NormalizeBody(preserved, true)
	if preserved["system"] != official {
		t.Fatal("调用者官方 system 必须原样保留")
	}
	preservedContent, _ := preserved["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(preservedContent) != 1 || preservedContent[0].(map[string]any)["text"] != "你好" {
		t.Fatal("调用者官方 system 路径不得注入 currentDate（content 只应桥接原文块）")
	}
}

// 调用者自带的官方三块 system 必须逐字节保留，不能重新组装或混入 currentDate。
func TestNormalizeBodyOfficialThreeBlockSystemPreserved(t *testing.T) {
	oldPreserve := config.PreserveClientContext
	config.PreserveClientContext = true
	t.Cleanup(func() { config.PreserveClientContext = oldPreserve })

	official := upstream.ZcodeFallbackSystemBlocks()
	before, err := json.Marshal(official)
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{
		"system":   official,
		"messages": []any{map[string]any{"role": "user", "content": "客户端原文"}},
	}
	NormalizeBody(body, true)
	after, err := json.Marshal(body["system"])
	if err != nil || string(after) != string(before) {
		t.Fatal("调用者官方三块 system 必须原样保留")
	}
	content, _ := body["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["text"] != "客户端原文" {
		t.Fatal("保留官方 system 时不得注入 currentDate（content 只应桥接原文块）")
	}
}
