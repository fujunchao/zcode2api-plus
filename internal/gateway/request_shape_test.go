package gateway

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"zcode2api/internal/config"
)

func TestDiagnosticShapeSourcesAndCounts(t *testing.T) {
	old := config.PreserveClientContext
	t.Cleanup(func() { config.PreserveClientContext = old })
	for _, tc := range []struct {
		name          string
		preserve, jwt bool
		system        any
		present, mid  bool
		source, form  string
		blocks        int
	}{
		{"missing_jwt", true, true, nil, false, false, "gateway_fallback", "blocks", 3},
		{"missing_apikey", true, false, nil, false, false, "none", "missing", 0},
		{"null_apikey", true, false, nil, true, false, "none", "null", 0},
		{"empty_text", true, true, "", true, false, "input", "string", 1},
		{"empty_blocks", true, true, []any{}, true, false, "input", "blocks", 0},
		{"mid_only", true, true, nil, false, true, "input_messages", "missing", 0},
		{"legacy", false, true, "private-system-sentinel", true, true, "legacy_merge", "blocks", 4},
		{"other", false, true, false, true, false, "input", "other", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config.PreserveClientContext = tc.preserve
			body := msgBody()
			if tc.present {
				body["system"] = tc.system
			}
			if tc.mid {
				body["messages"] = append(body["messages"].([]any), map[string]any{"role": "system", "content": "private-mid-sentinel"})
			}
			body["tools"] = []any{map[string]any{"name": "private-tool-sentinel", "input_schema": map[string]any{"type": "object"}}}
			before, _ := json.Marshal(body)
			prepared := shallowCopyBody(body)
			NormalizeBody(prepared, tc.jwt)
			diag := NewReqDiag("abcdef", "GLM-5.3", true, body, nil)
			diag.SetPreparedBody(body, prepared, tc.jwt)
			after, _ := json.Marshal(body)
			if string(before) != string(after) {
				t.Fatal("结构观测不能修改入站内容")
			}
			s := diag.shape
			if s.systemSource != tc.source || s.systemForm != tc.form || s.systemBlocks != tc.blocks || s.tools != 1 {
				t.Fatalf("结构画像错误：%+v", s)
			}
			if s.system != boolToInt(tc.mid) || s.user != 1 || s.messages != 1+boolToInt(tc.mid) {
				t.Fatalf("角色计数错误：%+v", s)
			}
			if strings.Contains(diag.Format(), "private-") {
				t.Fatal("结构摘要不应包含内容")
			}
		})
	}
	shape := inspectRequestShape(map[string]any{"messages": []any{
		map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "input": "private-args"}}},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "content": "private-result"}}},
		map[string]any{"role": "developer"}, map[string]any{"role": "tool"}, nil,
	}})
	if !reflect.DeepEqual([]int{shape.messages, shape.assistant, shape.user, shape.developer, shape.tool, shape.other, shape.toolUses, shape.toolResults}, []int{5, 1, 1, 1, 1, 1, 1, 1}) {
		t.Fatalf("工具/异常角色计数错误：%+v", shape)
	}
	if !hasMCSDeclaration("other-beta, mid-conversation-system-2026-04-07") || hasMCSDeclaration("mid-conversation-system-2026-04-07-private") {
		t.Fatal("MCS 声明应按完整能力名判断")
	}
}
