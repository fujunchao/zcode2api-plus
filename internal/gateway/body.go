// 请求体整形。对应 Python 版 routes/gateway.py 的 _normalize_body。
package gateway

import (
	"maps"
	"reflect"
	"slices"
	"strings"

	"zcode2api/internal/config"
	"zcode2api/internal/upstream"
)

// NormalizeBody 在不改写调用者嵌套对象的前提下规范请求体：
//  1. 模型名去 "provider/" 前缀，并按 MODEL_NAME_MAP 做大小写映射（幂等）；
//  2. 字符串 content 桥接为 [{type:"text"}]（不改写原始消息 map，幂等）；
//  3. JWT 缺少 system 时提供兼容兜底；已有指令由调用者拥有，包括显式空值。
//
// 回退模式保留旧版标准块前置与精确去重，不通过关键词删除调用者内容。
func NormalizeBody(body map[string]any, needsZcodeSystem bool) map[string]any {
	if raw, ok := body["model"].(string); ok {
		m := raw
		if strings.Contains(m, "/") {
			parts := strings.Split(m, "/")
			m = strings.Join(parts[1:], "/")
		}
		if mapped, ok := ModelNameMap[strings.ToLower(m)]; ok {
			m = mapped
		}
		body["model"] = m
	}

	if messages, ok := body["messages"].([]any); ok {
		bridged := make([]any, 0, len(messages))
		for _, item := range messages {
			msg, ok := item.(map[string]any)
			if !ok {
				bridged = append(bridged, item)
				continue
			}
			text, ok := msg["content"].(string)
			if !ok {
				bridged = append(bridged, msg)
				continue
			}
			clone := maps.Clone(msg)
			clone["content"] = []any{map[string]any{"type": "text", "text": text}}
			bridged = append(bridged, clone)
		}
		body["messages"] = bridged
	}

	if needsZcodeSystem {
		if config.PreserveClientContext && body["system"] != nil {
			return body
		}
		blocks := upstream.ZcodeSystemBlocks()
		switch existing := body["system"].(type) {
		case nil:
			// 键不存在或为 null（对应 Python existing_system is None）
			body["system"] = blocks
		case string:
			merged := make([]any, 0, len(blocks)+1)
			merged = append(merged, blocks...)
			merged = append(merged, map[string]any{"type": "text", "text": existing})
			body["system"] = merged
		case []any:
			merged := make([]any, 0, len(blocks)+len(existing))
			merged = append(merged, blocks...)
			for _, block := range existing {
				if slices.ContainsFunc(blocks, func(standard any) bool {
					return reflect.DeepEqual(block, standard)
				}) {
					continue
				}
				merged = append(merged, block)
			}
			body["system"] = merged
		default:
			// 其余类型保持原样（对齐 Python 版的 elif 链）
		}
	}
	return body
}
