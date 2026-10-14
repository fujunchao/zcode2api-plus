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
//  2. 普通字符串 content 桥接为文本块；system 保留上游要求的文本形态；
//  3. JWT 账号上保证上游从第 0 字节起的「固定前缀 + 身份段」成立：自带官方前缀的
//     调用者（含显式空值与消息内 system）原样保留，其余回退下方的前置合并——
//     上游逐字节校验首段，非官方前缀必然 3012（2026-10-12 DeepSeek Harness 实证）。
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
			if config.PreserveClientContext && msg["role"] == "system" {
				bridged = append(bridged, normalizeSystemMessage(msg))
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
		// 仅当调用者的 system 自带官方前缀（或显式空值 / 消息内 system）时才原样保留；
		// 第三方 harness 的首段不是官方前缀，透传即被上游以 3012 拦截，必须合并前置。
		if config.PreserveClientContext && upstream.ShouldPreserveCallerSystem(body) {
			return body
		}
		blocks := upstream.ZcodeFallbackSystemBlocks()
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

// 只有单个纯文本块可以压成字符串，缓存及扩展字段必须留在原块中。
func normalizeSystemMessage(message map[string]any) map[string]any {
	blocks, ok := message["content"].([]any)
	if !ok || len(blocks) != 1 {
		return message
	}
	block, ok := blocks[0].(map[string]any)
	if !ok || len(block) != 2 || block["type"] != "text" {
		return message
	}
	text, ok := block["text"].(string)
	if !ok {
		return message
	}
	clone := maps.Clone(message)
	clone["content"] = text
	return clone
}
