package upstream

import (
	"strings"

	"zcode2api/internal/config"
)

const midConversationSystemBeta = "mid-conversation-system-2026-04-07"

// ZcodeFallbackSystemBlocks 返回网关自行补齐 system 的默认三块形状。
// 调用者自带完整官方/GUI 上下文、显式空值或 MCS 时不应调用。
func ZcodeFallbackSystemBlocks() []any {
	return BuildFallbackSystem(nil, config.ZcodeClientProfile())
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
// 必须回退「三块 fallback + 第三方文本并入 dynamic」。显式空值与消息内 system（MCS）沿用 v2.9.11
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
		// 块形态必须「首段 + 身份段」都对才保留：块 0 在前缀相近的两个版本间很容易撞上
		// （3.16.x 与 3.17.1 只差几个字），只比对前缀会把身份段不匹配的请求原样送出，
		// 被上游判 3012（2026-11-01 生产事故：preserve 误判导致全量拦截）。
		return callerBlocksMatchOfficial(value)
	default:
		return false
	}
}


// callerBlocksMatchOfficial 判断块形态的 system 是否与当前档案的「首段 + 身份段」逐字节一致。
//
// 只要求前两块一致、允许调用者追加自己的块（例如 Environment/currentDate）：
// 上游的校验范围是「第 0 字节 → 身份段末尾」，后面追加什么不影响它。
func callerBlocksMatchOfficial(blocks []any) bool {
	standard := ZcodeSystemBlocks()
	if len(standard) < 2 || len(blocks) < 2 {
		return false
	}
	for i := range 2 {
		got, ok := blocks[i].(map[string]any)
		if !ok {
			return false
		}
		want, ok := standard[i].(map[string]any)
		if !ok {
			return false
		}
		gotText, _ := got["text"].(string)
		wantText, _ := want["text"].(string)
		if gotText != wantText {
			return false
		}
	}
	return true
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
