// Package OpenAI（GPT）兼容端点的请求转换层：OpenAI Chat Completions →
// Anthropic Messages。契约见 PLAN.md §5.7（Go 版增量，Python 版无对应实现）。
package openai

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// convertError 请求转换失败（对应 HTTP 400，OpenAI 错误形态）。
type convertError struct{ msg string }

func (e *convertError) Error() string { return e.msg }

// ConvertRequest 把 OpenAI Chat Completions 请求体转换为 Anthropic Messages
// 请求体（原地构造新 map，不修改入参）。模型白名单校验在 handler 侧完成。
func ConvertRequest(body map[string]any) (map[string]any, error) {
	if n, ok := body["n"]; ok {
		if v, isNum := n.(float64); isNum && v > 1 {
			return nil, &convertError{"仅支持 n=1"}
		}
	}

	out := map[string]any{}
	if m, ok := body["model"].(string); ok && m != "" {
		out["model"] = m
	}

	// max_tokens / max_completion_tokens → max_tokens（两者皆缺省 8192）
	if mt := body["max_tokens"]; mt != nil {
		out["max_tokens"] = mt
	} else if mct := body["max_completion_tokens"]; mct != nil {
		out["max_tokens"] = mct
	} else {
		out["max_tokens"] = float64(8192)
	}

	// temperature / top_p 透传（存在才带）
	for _, key := range []string{"temperature", "top_p"} {
		if v, ok := body[key]; ok && v != nil {
			out[key] = v
		}
	}

	// stop（string 或 array）→ stop_sequences
	switch stop := body["stop"].(type) {
	case string:
		if stop != "" {
			out["stop_sequences"] = []any{stop}
		}
	case []any:
		if len(stop) > 0 {
			out["stop_sequences"] = stop
		}
	}

	if stream, ok := body["stream"]; ok {
		out["stream"] = stream
	}
	// stream_options.include_usage 是 OpenAI 侧参数，Messages API 没有对应字段，
	// 故不写入上游请求体；handler 直接从原始 body 读取（见 handler.go）。

	// 思维链：客户端显式 thinking 优先，其次按 reasoning_effort / reasoning.effort 档位开启
	thinking, err := resolveThinking(body, numberOr(out["max_tokens"], 0))
	if err != nil {
		return nil, err
	}
	if thinking != nil {
		out["thinking"] = thinking
	}

	if err := applyTools(body, out, false); err != nil {
		return nil, err
	}

	// messages：system/developer 归并到顶层 system；user/assistant/tool 映射
	rawMessages, _ := body["messages"].([]any)
	messages := make([]any, 0, len(rawMessages))
	var systemBlocks []any
	for _, item := range rawMessages {
		msg, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		switch role {
		case "system", "developer":
			blocks, err := contentToTextBlocks(msg["content"])
			if err != nil {
				return nil, err
			}
			systemBlocks = append(systemBlocks, blocks...)
		default:
			converted, err := convertMessage(msg, role)
			if err != nil {
				return nil, err
			}
			messages = appendAnthropicMessage(messages, converted)
		}
	}
	if len(systemBlocks) > 0 {
		// 归并后的 system 交给引擎 NormalizeBody 注入 zcode_system 块时
		// 追加在其后（见 gateway/body.go：blocks 在前、existing 在后）
		out["system"] = systemBlocks
	}
	out["messages"] = messages
	return out, nil
}

// appendAnthropicMessage 合并同一轮相邻的同角色内容，尤其是并行工具调用及其结果。
// 否则两个 tool_result 会被拆成两条 user 消息，部分上游只在紧邻 assistant 的消息中找结果。
func appendAnthropicMessage(messages []any, next map[string]any) []any {
	if len(messages) > 0 {
		last, ok := messages[len(messages)-1].(map[string]any)
		left, leftOK := last["content"].([]any)
		right, rightOK := next["content"].([]any)
		if ok && leftOK && rightOK && last["role"] == next["role"] {
			joined := make([]any, 0, len(left)+len(right))
			joined = append(joined, left...)
			joined = append(joined, right...)
			last["content"] = joined
			return messages
		}
	}
	return append(messages, next)
}

// contentToTextBlocks 把 OpenAI content（字符串或 parts 数组）归并为
// Anthropic text blocks；空内容返回空切片。
func contentToTextBlocks(content any) ([]any, error) {
	switch c := content.(type) {
	case nil:
		return nil, nil
	case string:
		if c == "" {
			return nil, nil
		}
		return []any{map[string]any{"type": "text", "text": c}}, nil
	case []any:
		var blocks []any
		for _, part := range c {
			p, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if p["type"] != "text" {
				return nil, &convertError{fmt.Sprintf("system 消息不支持 content part 类型 %s", fmt.Sprint(p["type"]))}
			}
			text, _ := p["text"].(string)
			if text == "" {
				continue
			}
			blocks = append(blocks, map[string]any{"type": "text", "text": text})
		}
		return blocks, nil
	default:
		return nil, &convertError{"system 消息 content 形态无效"}
	}
}

// convertMessage 映射单条 user/assistant/tool 消息。
func convertMessage(msg map[string]any, role string) (map[string]any, error) {
	switch role {
	case "user", "assistant":
		blocks, err := contentBlocks(msg["content"])
		if err != nil {
			return nil, err
		}
		// assistant 的 tool_calls 追加 tool_use blocks（content 与工具调用可共存）
		if role == "assistant" {
			if rawCalls, ok := msg["tool_calls"].([]any); ok {
				for _, raw := range rawCalls {
					call, ok := raw.(map[string]any)
					if !ok {
						continue
					}
					block, err := toolCallToUse(call)
					if err != nil {
						return nil, err
					}
					blocks = append(blocks, block)
				}
			}
		}
		out := map[string]any{"role": role}
		if len(blocks) > 0 {
			out["content"] = blocks
		} else {
			out["content"] = []any{}
		}
		return out, nil

	case "tool":
		// role=tool → user 消息 + tool_result block（tool_use_id 绑定）
		result := map[string]any{
			"type":        "tool_result",
			"tool_use_id": msg["tool_call_id"],
		}
		blocks, err := contentBlocks(msg["content"])
		if err != nil {
			return nil, err
		}
		if len(blocks) > 0 {
			result["content"] = blocks
		} else {
			result["content"] = []any{}
		}
		return map[string]any{"role": "user", "content": []any{result}}, nil
	}
	return nil, &convertError{fmt.Sprintf("不支持的消息角色 %s", role)}
}

// contentBlocks 把 OpenAI content（字符串或 parts 数组）转换为 Anthropic
// blocks：text part → text block；image_url part → image block。
func contentBlocks(content any) ([]any, error) {
	switch c := content.(type) {
	case nil:
		return nil, nil
	case string:
		if c == "" {
			return nil, nil
		}
		return []any{map[string]any{"type": "text", "text": c}}, nil
	case []any:
		blocks := make([]any, 0, len(c))
		for _, part := range c {
			p, ok := part.(map[string]any)
			if !ok {
				continue
			}
			switch p["type"] {
			case "text":
				text, _ := p["text"].(string)
				if text == "" {
					continue
				}
				blocks = append(blocks, map[string]any{"type": "text", "text": text})
			case "image_url":
				block, err := imageURLToBlock(p)
				if err != nil {
					return nil, err
				}
				blocks = append(blocks, block)
			default:
				return nil, &convertError{fmt.Sprintf("不支持 content part 类型 %s", fmt.Sprint(p["type"]))}
			}
		}
		return blocks, nil
	default:
		return nil, &convertError{"消息 content 形态无效"}
	}
}

// imageURLToBlock 把 OpenAI image_url part 转换为 Anthropic image block。
// data URL → base64 source（media_type 从 URL 解析）；http[s] URL → url source
// （上游支持度未知，失败按上游错误原样透传）。
func imageURLToBlock(part map[string]any) (map[string]any, error) {
	inner, ok := part["image_url"].(map[string]any)
	if !ok {
		return nil, &convertError{"image_url part 缺少 image_url 对象"}
	}
	u, _ := inner["url"].(string)
	switch {
	case strings.HasPrefix(u, "data:"):
		mediaType, data, err := parseDataURL(u)
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "image", "source": map[string]any{
			"type": "base64", "media_type": mediaType, "data": data,
		}}, nil
	case strings.HasPrefix(u, "http://"), strings.HasPrefix(u, "https://"):
		return map[string]any{"type": "image", "source": map[string]any{
			"type": "url", "url": u,
		}}, nil
	default:
		return nil, &convertError{"image_url 仅支持 data URL 或 http(s) URL"}
	}
}

// parseDataURL 解析 data:<media_type>;base64,<data> 形态。
func parseDataURL(u string) (string, string, error) {
	rest := strings.TrimPrefix(u, "data:")
	sep := strings.Index(rest, ",")
	if sep < 0 {
		return "", "", &convertError{"data URL 格式无效（缺少逗号）"}
	}
	meta := rest[:sep]
	const marker = ";base64"
	if !strings.HasSuffix(meta, marker) {
		return "", "", &convertError{"data URL 仅支持 base64 编码"}
	}
	mediaType := strings.TrimSuffix(meta, marker)
	if mediaType == "" {
		return "", "", &convertError{"data URL 缺少 media_type"}
	}
	data := rest[sep+1:]
	if data == "" {
		return "", "", &convertError{"data URL 缺少数据段"}
	}
	return mediaType, data, nil
}

// toolCallToUse 把 OpenAI tool_calls[] 项转换为 Anthropic tool_use block；
// arguments JSON 字符串解析为 input（解析失败按空对象，模型侧已生成即认可）。
func toolCallToUse(call map[string]any) (map[string]any, error) {
	fn, ok := call["function"].(map[string]any)
	if !ok {
		return nil, &convertError{"tool_calls 项缺少 function 对象"}
	}
	name, _ := fn["name"].(string)
	if name == "" {
		return nil, &convertError{"tool_calls 项缺少 function.name"}
	}
	input := map[string]any{}
	if raw, _ := fn["arguments"].(string); strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal([]byte(raw), &input); err != nil {
			var arr any
			if err2 := json.Unmarshal([]byte(raw), &arr); err2 != nil {
				return nil, &convertError{fmt.Sprintf("tool_calls 的 arguments 不是合法 JSON: %v", err)}
			}
			input = map[string]any{}
			_ = arr // arguments 为数组等非对象形态：保留空对象，上游按 schema 校验
		}
	}
	return map[string]any{
		"type":  "tool_use",
		"id":    call["id"],
		"name":  name,
		"input": input,
	}, nil
}

// ── 思维链（extended thinking）───────────────────────────────────────────────
// OpenAI 系客户端用 reasoning_effort 表达「想多久」，Anthropic 侧则是一个独立的
// thinking 块（type/budget_tokens）。两个 OpenAI 兼容层共用本组工具做翻译；
// /v1/messages 原生路径不需要它们——那条路是字节级透传，客户端自带 thinking 原样上行。

// minThinkingBudget Anthropic 侧 budget_tokens 的下限；低于此值上游拒绝启用思考。
const minThinkingBudget = 1024

// thinkingBudgets 推理档位 → budget_tokens。取 OpenAI（low/medium/high/minimal）
// 与 Responses（minimal/low/medium/high）两侧档位的并集。
var thinkingBudgets = map[string]float64{
	"minimal": 1024,
	"low":     2048,
	"medium":  4096,
	"high":    8192,
}

// thinkingFromEffort 把推理档位翻译成 Anthropic 的 thinking 块。
//
// 预算上限取 max_tokens 的一半，而不是贴着 max_tokens 给满：max_tokens 是「思考 + 正文」
// 的总上限，若把预算给到 max_tokens-1（如 high=8192 配默认 max_tokens=8192），正文只剩
// 个位数额度，模型思考完会被立刻截断——比不思考更糟。留一半给正文。
// 放不下最低预算时明确报错，不静默关闭思考，也不擅自放大输出上限。
func thinkingFromEffort(effort any, maxTokens float64) (map[string]any, error) {
	s, _ := effort.(string)
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "none" {
		return map[string]any{"type": "disabled"}, nil
	}
	budget, ok := thinkingBudgets[s]
	if !ok {
		return nil, &convertError{"reasoning_effort / reasoning.effort 仅支持 none、minimal、low、medium、high"}
	}
	if ceiling := math.Floor(maxTokens / 2); budget > ceiling {
		budget = ceiling
	}
	if budget < minThinkingBudget {
		return nil, &convertError{"开启思考时 max_tokens / max_output_tokens 至少为 2048，才能保留最低思考预算与正文空间"}
	}
	return map[string]any{"type": "enabled", "budget_tokens": budget}, nil
}

// resolveThinking 决定最终写入上游请求体的 thinking 块，优先级：
//  1. 客户端显式传了 Anthropic 形态的 thinking（type 为 enabled / disabled）→
//     校验预算后原样透传；只有 enabled 开关时按 effort 或 medium 补齐预算；
//  2. 否则按 reasoning_effort（chat/completions）或 reasoning.effort（Responses）开启；
//  3. 两者皆无 → 返回 nil，不写该字段。默认不主动开启思考，避免改变既有用户的
//     响应形态与 token 消耗，也让旧行为与旧测试保持成立。
func resolveThinking(body map[string]any, maxTokens float64) (map[string]any, error) {
	if maxTokens <= 0 || math.IsInf(maxTokens, 0) || math.IsNaN(maxTokens) || math.Trunc(maxTokens) != maxTokens {
		return nil, &convertError{"max_tokens / max_completion_tokens / max_output_tokens 必须是正整数"}
	}
	if raw := body["thinking"]; raw != nil {
		explicit, ok := raw.(map[string]any)
		if !ok {
			return nil, &convertError{"thinking 必须是对象"}
		}
		switch explicit["type"] {
		case "disabled":
			return explicit, nil
		case "enabled":
			if explicit["budget_tokens"] == nil {
				// Pi 的 ZAI/DeepSeek 适配器只发送启用开关；补齐 Anthropic 所需预算。
				// 有 effort 时按档位，只有开关时用 medium；不透传 clear_thinking 等异协议字段。
				effort, err := requestedReasoningEffort(body)
				if err != nil {
					return nil, err
				}
				if effort == nil {
					effort = "medium"
				}
				thinking, err := thinkingFromEffort(effort, maxTokens)
				if err == nil && thinking["type"] == "disabled" {
					return nil, &convertError{"thinking.type=enabled 与 reasoning_effort=none 相互矛盾"}
				}
				return thinking, err
			}
			budget := numberOr(explicit["budget_tokens"], 0)
			if math.Trunc(budget) != budget || budget < minThinkingBudget || budget >= maxTokens || math.IsNaN(budget) {
				return nil, &convertError{"thinking.budget_tokens 必须是至少 1024 且小于 max_tokens / max_output_tokens 的整数"}
			}
			return explicit, nil
		default:
			return nil, &convertError{"thinking.type 仅支持 enabled 或 disabled"}
		}
	}
	effort, err := requestedReasoningEffort(body)
	if err != nil {
		return nil, err
	}
	if effort != nil {
		return thinkingFromEffort(effort, maxTokens)
	}
	return nil, nil
}

func requestedReasoningEffort(body map[string]any) (any, error) {
	if effort := body["reasoning_effort"]; effort != nil {
		return effort, nil
	}
	if raw := body["reasoning"]; raw != nil {
		reasoning, ok := raw.(map[string]any)
		if !ok {
			return nil, &convertError{"reasoning 必须是对象"}
		}
		return reasoning["effort"], nil
	}
	return nil, nil
}

// numberOr 宽松取数值（JSON 数字解码为 float64）；不可用时回退默认值。
func numberOr(v any, fallback float64) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return fallback
}
