package gateway

import (
	"encoding/json"
	"reflect"
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
	blocks := upstream.ZcodeSystemBlocks()

	// 键不存在 → 注入 blocks
	body := map[string]any{}
	NormalizeBody(body, true)
	sys, ok := body["system"].([]any)
	if !ok || len(sys) != len(blocks) {
		t.Fatalf("缺失 system 应整体注入: %v", body["system"])
	}

	// 字符串 system → blocks + 用户文本
	body2 := map[string]any{"system": "be brief"}
	NormalizeBody(body2, true)
	sys2 := body2["system"].([]any)
	if len(sys2) != len(blocks)+1 {
		t.Fatalf("字符串 system 应拼接在 blocks 之后: %d", len(sys2))
	}
	last := sys2[len(sys2)-1].(map[string]any)
	if last["text"] != "be brief" {
		t.Fatalf("用户文本应在末尾: %v", last)
	}

	// 列表 system → blocks + 列表
	userBlocks := []any{map[string]any{"type": "text", "text": "user"}}
	body3 := map[string]any{"system": userBlocks}
	NormalizeBody(body3, true)
	sys3 := body3["system"].([]any)
	if len(sys3) != len(blocks)+1 {
		t.Fatalf("列表 system 应拼接: %d", len(sys3))
	}
	if !reflect.DeepEqual(sys3[len(sys3)-1], userBlocks[0]) {
		t.Fatal("用户块应原样保留在末尾")
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
	const prefix = "You are ZCode, an interactive coding agent"
	known := map[string]any{
		"type": "text", "text": prefix,
		"cache_control": map[string]any{"type": "ephemeral"},
	}
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
	if len(system) != 3+len(tail) || !reflect.DeepEqual(system[3:], tail) {
		t.Fatalf("只应前置一份三个标准块并原样保留调用者段落，实际共 %d 块", len(system))
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

func TestNormalizeBodyCallerSystemIsIdempotent(t *testing.T) {
	const payload = `{"model":"GLM-5.3","system":[
		{"type":"text","text":"调用者身份","cache_control":{"type":"ephemeral","ttl":"1h"}},
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
