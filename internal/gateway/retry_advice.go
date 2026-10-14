package gateway

import (
	"maps"
	"net/http"
	"strconv"
	"time"
)

// RetryAdvice 只约束当前网关相同保护范围内的立即重投，不更改其他渠道的重试策略。
func RetryAdvice(remaining time.Duration) map[string]any {
	if remaining <= 0 {
		return nil
	}
	seconds := int64((remaining + time.Second - 1) / time.Second)
	return map[string]any{"retry_policy": "wait_same_gateway", "retry_after_seconds": seconds}
}

func withRetryAdvice(result runResult, remaining time.Duration) runResult {
	advice := RetryAdvice(remaining)
	if advice == nil {
		return result
	}
	result.RetryAfter = remaining
	if original, ok := result.Body.(map[string]any); ok {
		body := maps.Clone(original)
		if originalError, ok := body["error"].(map[string]any); ok {
			errBody := maps.Clone(originalError)
			maps.Copy(errBody, advice)
			body["error"] = errBody
		} else {
			maps.Copy(body, advice)
		}
		result.Body = body
	}
	return result
}

// WriteRunResult 统一四协议中尚未开始交付的错误响应；只输出本地生成的等待建议。
func WriteRunResult(w http.ResponseWriter, result runResult) {
	if advice := RetryAdvice(result.RetryAfter); advice != nil {
		w.Header().Set("Retry-After", strconv.FormatInt(advice["retry_after_seconds"].(int64), 10))
		w.Header().Set("X-Zcode2api-Retry-Policy", "wait_same_gateway")
	}
	WriteJSON(w, result.Status, result.Body)
}
