package openai

import "testing"

func TestResponsesPreservesInstructionRoles(t *testing.T) {
	input := []any{
		map[string]any{"role": "system", "content": "系统规则"},
		map[string]any{"role": "user", "content": "问题"},
		map[string]any{"type": "message", "role": "developer", "content": []any{map[string]any{"type": "input_text", "text": "开发者规则"}}},
	}
	out, err := ConvertResponsesRequest(map[string]any{"model": "GLM-5.3", "instructions": "顶层规则", "input": input})
	if err != nil {
		t.Fatal(err)
	}
	system, _ := out["system"].([]any)
	if len(system) != 3 {
		t.Fatalf("指令应归并为三块：%v", system)
	}
	for i, want := range []string{"顶层规则", "系统规则", "开发者规则"} {
		if system[i].(map[string]any)["text"] != want {
			t.Fatalf("指令顺序错误：%v", system)
		}
	}
	messages := out["messages"].([]any)
	if len(messages) != 1 || messages[0].(map[string]any)["role"] != "user" {
		t.Fatalf("指令泄漏到普通消息：%v", messages)
	}
	if input[0].(map[string]any)["role"] != "system" {
		t.Fatal("转换不能修改调用方输入")
	}
}

func TestResponsesRejectsUnknownMessageRole(t *testing.T) {
	_, err := ConvertResponsesRequest(map[string]any{"model": "GLM-5.3", "input": []any{map[string]any{"role": "unknown", "content": "test"}}})
	if err == nil {
		t.Fatal("未知角色不能静默降为 user")
	}
}
