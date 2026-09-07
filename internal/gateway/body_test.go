package gateway

import (
	"reflect"
	"testing"

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
