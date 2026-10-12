package gateway

import (
	"fmt"
	"strings"

	"zcode2api/internal/config"
	"zcode2api/internal/upstream"
)

// requestShape 只看固定结构和计数，不读取文本、工具名/描述、参数或 schema。
type requestShape struct {
	stage, systemSource, systemForm string
	systemBlocks, messages, tools   int
	user, assistant, system         int
	developer, tool, other          int
	toolUses, toolResults           int
	mcsDeclared                     bool
}

func inspectRequestShape(body map[string]any) requestShape {
	s := requestShape{stage: "normalized_input", systemSource: "unprepared", systemForm: "missing"}
	if value, exists := body["system"]; exists {
		switch value := value.(type) {
		case nil:
			s.systemForm = "null"
		case string:
			s.systemForm, s.systemBlocks = "string", 1
		case []any:
			s.systemForm, s.systemBlocks = "blocks", len(value)
		default:
			s.systemForm = "other"
		}
	}
	tools, _ := body["tools"].([]any)
	s.tools = len(tools)
	messages, _ := body["messages"].([]any)
	s.messages = len(messages)
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		role, _ := message["role"].(string)
		switch role {
		case "user":
			s.user++
		case "assistant":
			s.assistant++
		case "system":
			s.system++
		case "developer":
			s.developer++
		case "tool":
			s.tool++
		default:
			s.other++
		}
		blocks, _ := message["content"].([]any)
		for _, raw := range blocks {
			block, _ := raw.(map[string]any)
			switch block["type"] {
			case "tool_use":
				s.toolUses++
			case "tool_result":
				s.toolResults++
			}
		}
	}
	return s
}

// SetPreparedBody 记录每账号整形后的结构，来源仅表示本网关做了什么。
// input 可能已经由 New API/协议适配器改写，不能把它解释为最终用户原始输入。
func (d *ReqDiag) SetPreparedBody(input, prepared map[string]any, isJWT bool) {
	if d == nil {
		return
	}
	s := inspectRequestShape(prepared)
	s.stage = "prepared"
	s.systemSource = "none"
	if input["system"] != nil {
		s.systemSource = "input"
	} else if upstream.HasSystemMessages(input) {
		s.systemSource = "input_messages"
	}
	if isJWT && !(config.PreserveClientContext && upstream.ShouldPreserveCallerSystem(input)) {
		switch input["system"].(type) {
		case nil:
			s.systemSource = "gateway_fallback"
		case string, []any:
			s.systemSource = "legacy_merge"
		}
	}
	d.shape = s
	d.outbound = inspectOutboundBody(prepared)
}

func (s requestShape) format(preserve bool) string {
	return fmt.Sprintf(" preserve_context=%t shape_stage=%s system_source=%s system_form=%s system_blocks=%d messages=%d roles_user=%d roles_assistant=%d roles_system=%d roles_developer=%d roles_tool=%d roles_other=%d tools=%d tool_use_blocks=%d tool_result_blocks=%d mcs_declared=%t",
		preserve, s.stage, s.systemSource, s.systemForm, s.systemBlocks, s.messages,
		s.user, s.assistant, s.system, s.developer, s.tool, s.other, s.tools, s.toolUses, s.toolResults, s.mcsDeclared)
}

func hasMCSDeclaration(beta string) bool {
	for _, feature := range strings.Split(beta, ",") {
		if strings.TrimSpace(feature) == "mid-conversation-system-2026-04-07" {
			return true
		}
	}
	return false
}
