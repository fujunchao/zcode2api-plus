package upstream

import (
	"strings"

	"zcode2api/internal/config"
)

const midConversationSystemBeta = "mid-conversation-system-2026-04-07"

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
