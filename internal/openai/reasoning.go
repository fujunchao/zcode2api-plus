package openai

import (
	"maps"
	"math"
	"strings"

	"zcode2api/internal/model"
)

// 仅用于本项目当前开放的 GLM-5.3 / GLM-5.3-Flash，不是通用模型能力白名单。
// 来源：https://docs.z.ai/guides/capabilities/thinking
// Coding Plan 将兼容档位归一为原生 low / high / max；none 不代表关闭思考。
var glm53EffortAliases = map[string]string{
	"none": "low", "minimal": "low", "low": "low",
	"medium": "high", "high": "high",
	"xhigh": "max", "max": "max",
}

// applyGLM53Reasoning 将模型原生推理程度写入 Anthropic 的 output_config.effort。
// effort 不是 token 预算：不再猜测 budget_tokens，也不擅自提高 max_tokens。
// GLM-5.3 系列强制思考；无显式配置时保留上游默认 max。
func applyGLM53Reasoning(body, out map[string]any) error {
	maxTokens := numberOr(out["max_tokens"], 0)
	if maxTokens <= 0 || math.IsInf(maxTokens, 0) || math.IsNaN(maxTokens) || math.Trunc(maxTokens) != maxTokens {
		return &convertError{"max_tokens / max_completion_tokens / max_output_tokens 必须是正整数"}
	}
	switch model.NormalizeModelName(out["model"]) {
	case "glm-5.3", "glm-5.3-flash":
	default:
		return nil // 模型白名单由 HTTP 入口统一拒绝，避免将此配置套到其它模型。
	}

	if raw := body["thinking"]; raw != nil {
		explicit, ok := raw.(map[string]any)
		if !ok {
			return &convertError{"thinking 必须是对象"}
		}
		switch explicit["type"] {
		case "disabled":
			return &convertError{"GLM-5.3 / GLM-5.3-Flash 不支持关闭思考；请使用 reasoning_effort=low"}
		case "enabled":
			if value := explicit["budget_tokens"]; value != nil {
				budget := numberOr(value, 0)
				if budget < 1024 || budget >= maxTokens || math.Trunc(budget) != budget || math.IsNaN(budget) {
					return &convertError{"thinking.budget_tokens 必须是至少 1024 且小于 max_tokens / max_output_tokens 的整数"}
				}
				// 显式 Anthropic 预算是独立约束，不覆盖客户端同时请求的原生 effort。
				thinking := maps.Clone(explicit)
				delete(thinking, "clear_thinking")
				out["thinking"] = thinking
			}
			// Pi ZAI 的无预算 enabled 开关与强制思考一致，不伪造 Anthropic 预算。
		default:
			return &convertError{"GLM-5.3 / GLM-5.3-Flash 的 thinking.type 仅支持 enabled"}
		}
	}

	effort, err := requestedReasoningEffort(body)
	if err != nil {
		return err
	}
	if effort == nil {
		return nil
	}
	name, _ := effort.(string)
	native, ok := glm53EffortAliases[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return &convertError{"GLM-5.3 系列支持 low、high、max；兼容 none/minimal→low、medium→high、xhigh→max"}
	}
	out["output_config"] = map[string]any{"effort": native}
	return nil
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
