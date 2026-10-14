package upstream

import (
	"strings"

	"zcode2api/internal/config"
)

const midConversationSystemBeta = "mid-conversation-system-2026-04-07"

// 这段只描述网关回退上下文的实际能力，不复制采集机路径，也不凭空增加工具。
const desktopFallbackCapabilitiesText = "# Gateway Desktop Capabilities\n" +
	"This gateway fallback context has no Gen UI output directory or built-in Gen UI rendering capability. " +
	"Return results as text or Markdown unless the caller explicitly supplies suitable tools. " +
	"Use only the tools declared in this request; do not assume desktop filesystem or preview tools are available."

// ZcodeFallbackSystemBlocks 只用于网关自行补齐 system 的分支。
// 调用者自带完整官方/GUI 上下文、显式空值或 MCS 时不应调用；非桌面档案保持原样。
func ZcodeFallbackSystemBlocks() []any {
	blocks := ZcodeSystemBlocks()
	if config.ZcodeClientProfile().SourceTitle != "electron" {
		return blocks
	}
	return append(blocks, map[string]any{
		"type": "text", "text": desktopFallbackCapabilitiesText,
		"cache_control": map[string]any{"type": "ephemeral"},
	})
}

// HasSystemMessages 只检查消息序列，不把顶层 system 当成中途系统消息。
func HasSystemMessages(body map[string]any) bool {
	messages, _ := body["messages"].([]any)
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if ok && message["role"] == "system" {
			return true
		}
	}
	return false
}

// ShouldPreserveCallerSystem 判断「保留调用者上下文」模式下是否应原样保留该请求的 system。
//
// 上游从 system 的第 0 个字节逐字节校验到身份段末尾：只有**首段已是官方固定前缀**
// 的 system（官方系客户端自带完整上下文）才能安全原样透传；第三方 harness 的提示词
// 首段不是官方前缀，透传必被上游以 3012 拦截（2026-10-12 DeepSeek Harness 实证），
// 必须回退「官方块前置 + 精确去重」。显式空值与消息内 system（MCS）沿用 v2.9.11
// 语义：调用者声明「无指令 / 自有中途指令」时，网关不叠加自己的身份块。
func ShouldPreserveCallerSystem(body map[string]any) bool {
	switch value := body["system"].(type) {
	case nil:
		return HasSystemMessages(body)
	case string:
		if value == "" {
			return true
		}
		return hasOfficialSystemPrefix(value)
	case []any:
		if len(value) == 0 {
			return true
		}
		first, ok := value[0].(map[string]any)
		if !ok {
			return false
		}
		text, _ := first["text"].(string)
		return hasOfficialSystemPrefix(text)
	default:
		return false
	}
}

// hasOfficialSystemPrefix 文本是否以官方块 0（固定前缀）开头。
func hasOfficialSystemPrefix(text string) bool {
	prefix := OfficialSystemPrefix()
	return prefix != "" && strings.HasPrefix(text, prefix)
}

// OfficialSystemPrefix 官方 system 首段（块 0）的固定文本；zcode_system.json 解析
// 失败时为空串（判定一律不通过，走调用方的合并路径兜底）。
func OfficialSystemPrefix() string {
	blocks := ZcodeSystemBlocks()
	if len(blocks) == 0 {
		return ""
	}
	first, _ := blocks[0].(map[string]any)
	text, _ := first["text"].(string)
	return text
}

// ApplyMessageCapabilities 为当前 ZAI Anthropic 上游声明实际使用的消息能力。
// 不添加上下文或工具；同步与异步必须在最终正文整形后调用。
func ApplyMessageCapabilities(request *Request, body map[string]any) {
	if !config.PreserveClientContext || !HasSystemMessages(body) {
		return
	}
	if request.Headers == nil {
		request.Headers = make(map[string]string)
	}
	features := make([]string, 0)
	seen := make(map[string]bool)
	for _, feature := range append(strings.Split(request.Headers["Anthropic-Beta"], ","), midConversationSystemBeta) {
		feature = strings.TrimSpace(feature)
		if feature != "" && !seen[feature] {
			features = append(features, feature)
			seen[feature] = true
		}
	}
	request.Headers["Anthropic-Beta"] = strings.Join(features, ", ")
}
