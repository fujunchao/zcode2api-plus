// 请求体整形。对应 Python 版 routes/gateway.py 的 _normalize_body。
package gateway

import (
	"maps"
	"strings"

	"zcode2api/internal/upstream"
)

// NormalizeBody 对请求体做三件事（与 Python 版一致）：
//  1. 模型名去 "provider/" 前缀，并按 MODEL_NAME_MAP 做大小写映射（幂等）；
//  2. 字符串 content 桥接为 [{type:"text"}]（不改写原始消息 map，幂等）；
//  3. needsZcodeSystem（JWT 账号）时把 zcode_system.json 注入顶层 system
//     —— 否则上游返回 405。
//
// 注意：system 注入**不幂等**（重复调用会重复拼接 blocks），调用方须保证
// 对同一个 body 只注入一次（Python 版同样是入口一次 + 账号副本一次）。
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
			merged = append(merged, existing...)
			body["system"] = merged
		default:
			// 其余类型保持原样（对齐 Python 版的 elif 链）
		}
	}
	return body
}
