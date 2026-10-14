package upstream

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
)

// SemanticContentKey 是代理证据专用的语义摘要，不替代重放保护键。
// 只在明确的归因容器排除已知字段，消息、工具及生成参数中的同名键仍参与摘要。
// 非 JSON 正文返回空串，调用方必须将其视为不可用于代理证据。
func SemanticContentKey(body map[string]any) string {
	semantic := maps.Clone(body)
	dropSemanticAttribution(semantic)
	if metadata, ok := body["metadata"].(map[string]any); ok {
		metadata = maps.Clone(metadata)
		dropSemanticAttribution(metadata)
		if raw, ok := metadata["user_id"].(string); ok {
			var identity map[string]any
			if json.Unmarshal([]byte(raw), &identity) == nil && identity != nil {
				dropSemanticAttribution(identity)
				if len(identity) == 0 {
					delete(metadata, "user_id")
				} else {
					// 规范化字符串 JSON 的键序；未知业务属性仍作为内容保留。
					normalized, err := json.Marshal(identity)
					if err != nil {
						return ""
					}
					metadata["user_id"] = string(normalized)
				}
			}
		}
		if len(metadata) == 0 {
			delete(semantic, "metadata")
		} else {
			semantic["metadata"] = metadata
		}
	}
	encoded, err := json.Marshal(semantic)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func dropSemanticAttribution(values map[string]any) {
	for _, key := range []string{
		"device_id", "account_uuid", "request_id", "session_id", "trace_id",
		"query_id", "session_type", "query_source",
	} {
		delete(values, key)
	}
}
