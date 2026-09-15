package openai

import (
	"fmt"
	"strings"
)

// applyTools 只承诺客户端执行的函数工具；无法保证的严格采样与内建工具显式拒绝。
func applyTools(body, out map[string]any, responses bool) error {
	if raw := body["tools"]; raw != nil {
		items, ok := raw.([]any)
		if !ok {
			return &convertError{"tools 必须是数组"}
		}
		tools := make([]any, 0, len(items))
		names := map[string]bool{}
		for i, raw := range items {
			tool, ok := raw.(map[string]any)
			if !ok || (tool["type"] != nil && tool["type"] != "function") {
				return &convertError{fmt.Sprintf("tools[%d] 仅支持 type=function 的客户端函数工具", i)}
			}
			fn := tool
			if !responses {
				fn, _ = tool["function"].(map[string]any)
			}
			name, _ := fn["name"].(string)
			if strings.TrimSpace(name) == "" {
				return &convertError{fmt.Sprintf("tools[%d] 缺少 function name", i)}
			}
			if names[name] {
				return &convertError{"tools 函数名称重复: " + name}
			}
			names[name] = true
			if raw := fn["strict"]; raw != nil {
				strict, ok := raw.(bool)
				if !ok {
					return &convertError{"工具 strict 必须是布尔值"}
				}
				if strict {
					return &convertError{"暂不支持 strict=true 的严格工具采样；请设置 strict=false 或省略该参数"}
				}
			}
			params := fn["parameters"]
			if params == nil {
				params = map[string]any{"type": "object", "properties": map[string]any{}}
			} else if _, ok := params.(map[string]any); !ok {
				return &convertError{"工具 parameters 必须是 JSON Schema 对象"}
			}
			entry := map[string]any{"name": name, "input_schema": params}
			if d, ok := fn["description"].(string); ok && d != "" {
				entry["description"] = d
			}
			tools = append(tools, entry)
		}
		if len(tools) > 0 {
			out["tools"] = tools
		}
	}
	return applyToolChoice(body, out, responses)
}

// applyToolChoice 把两套 OpenAI 工具控制参数转换为 Anthropic tool_choice。
// Responses 使用扁平的 name，Chat Completions 使用 function.name。
func applyToolChoice(body, out map[string]any, responses bool) error {
	var choice map[string]any
	switch raw := body["tool_choice"].(type) {
	case nil:
	case string:
		switch raw {
		case "auto", "none":
			choice = map[string]any{"type": raw}
		case "required":
			choice = map[string]any{"type": "any"}
		default:
			return &convertError{"tool_choice 仅支持 auto、none、required 或指定 function"}
		}
	case map[string]any:
		if raw["type"] != "function" {
			return &convertError{"tool_choice 对象仅支持 type=function"}
		}
		function := raw
		if !responses {
			function, _ = raw["function"].(map[string]any)
		}
		name, _ := function["name"].(string)
		if name == "" {
			return &convertError{"tool_choice 缺少函数名称"}
		}
		choice = map[string]any{"type": "tool", "name": name}
	default:
		return &convertError{"tool_choice 必须是字符串或 function 对象"}
	}
	tools, _ := out["tools"].([]any)
	if choice != nil {
		if choice["type"] == "any" && len(tools) == 0 {
			return &convertError{"tool_choice=required 必须同时声明 tools"}
		}
		if choice["type"] == "tool" {
			found := false
			for _, raw := range tools {
				tool, _ := raw.(map[string]any)
				found = found || tool["name"] == choice["name"]
			}
			if !found {
				return &convertError{"tool_choice 指定的函数必须在 tools 中声明"}
			}
		}
	}
	if raw := body["parallel_tool_calls"]; raw != nil {
		parallel, ok := raw.(bool)
		if !ok {
			return &convertError{"parallel_tool_calls 必须是布尔值"}
		}
		if len(tools) > 0 {
			if choice == nil {
				choice = map[string]any{"type": "auto"}
			}
			if choice["type"] != "none" {
				choice["disable_parallel_tool_use"] = !parallel
			}
		}
	}
	if choice != nil {
		out["tool_choice"] = choice
	}
	return nil
}
