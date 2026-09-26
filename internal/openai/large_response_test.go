package openai

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// 两个兼容端点共用引擎，长 JSON 修复不能只覆盖原生 /v1/messages。
func TestLargeNonStreamingResponses(t *testing.T) {
	for _, protocol := range []string{"chat", "responses"} {
		t.Run(protocol, func(t *testing.T) {
			f := newFixture(t)
			text := strings.Repeat("长文本", 9000)
			payload, err := json.Marshal(map[string]any{
				"id": "msg_large", "type": "message", "role": "assistant", "model": "GLM-5.3",
				"content":     []any{map[string]any{"type": "text", "text": text}},
				"stop_reason": "end_turn", "usage": map[string]any{"input_tokens": 11, "output_tokens": 18000},
			})
			if err != nil {
				t.Fatal(err)
			}
			f.setResponder(func(int) (int, string, string) { return http.StatusOK, "application/json", string(payload) })
			request := compatRequest(protocol)
			request["stream"] = false
			status, raw := postCompat(t, f, protocol, request)
			if status != http.StatusOK {
				t.Fatalf("正常长响应应成功：%d %s", status, raw)
			}
			var response map[string]any
			if err := json.Unmarshal([]byte(raw), &response); err != nil {
				t.Fatal(err)
			}
			var got string
			if protocol == "chat" {
				choice := response["choices"].([]any)[0].(map[string]any)
				got = choice["message"].(map[string]any)["content"].(string)
			} else {
				item := response["output"].([]any)[0].(map[string]any)
				got = item["content"].([]any)[0].(map[string]any)["text"].(string)
			}
			if got != text {
				t.Fatalf("长文本丢失：got=%d want=%d", len(got), len(text))
			}
		})
	}
}
