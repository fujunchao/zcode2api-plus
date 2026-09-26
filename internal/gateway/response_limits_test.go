package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

// 正常 JSON 不得套用错误体的 64 KiB 上限：长文本和多字节文本都必须完整交付。
func TestLargeJSONResponsePreservesBodyAndUsage(t *testing.T) {
	for _, text := range []string{strings.Repeat("x", 70000), strings.Repeat("中文", 12000)} {
		t.Run(string([]rune(text)[0]), func(t *testing.T) {
			oldData := config.DataDir
			config.DataDir = t.TempDir()
			t.Cleanup(func() { config.DataDir = oldData })
			f := newFixture(t)
			acc, err := f.st.AddAccount(model.ProviderZai, "large-json", "sk-large-json")
			if err != nil {
				t.Fatal(err)
			}
			payload, err := json.Marshal(map[string]any{
				"id": "msg_large", "type": "message", "role": "assistant", "model": "GLM-5.3",
				"content":     []any{map[string]any{"type": "text", "text": text}},
				"stop_reason": "end_turn", "usage": map[string]any{"input_tokens": 11, "output_tokens": 18000},
			})
			if err != nil {
				t.Fatal(err)
			}
			f.respond = jsonResp(http.StatusOK, string(payload))
			status, raw := f.post(t, msgBody(), "sk-test")
			if status != http.StatusOK || raw != string(payload) {
				t.Fatalf("正常 JSON 必须完整透传：status=%d upstream=%d downstream=%d valid_json=%v",
					status, len(payload), len(raw), json.Valid([]byte(raw)))
			}
			got := f.st.FindAny(acc.ID)
			if got.UseCount != 1 || got.TotalInputTokens != 11 || got.TotalOutputTokens != 18000 {
				t.Fatalf("长响应的调用和用量必须正常累计：calls=%d in=%d out=%d",
					got.UseCount, got.TotalInputTokens, got.TotalOutputTokens)
			}
		})
	}
}

// 正常响应仍要有独立的内存预算，超限必须明确失败，不能返回被裁坏的 HTTP 200。
func TestOversizedJSONResponseFailsExplicitly(t *testing.T) {
	oldData := config.DataDir
	config.DataDir = t.TempDir()
	t.Cleanup(func() { config.DataDir = oldData })
	f := newFixture(t)
	acc, err := f.st.AddAccount(model.ProviderZai, "oversized-json", "sk-oversized-json")
	if err != nil {
		t.Fatal(err)
	}
	f.respond = jsonResp(http.StatusOK, `{"content":[{"type":"text","text":"`+strings.Repeat("x", 16<<20)+`"}]}`)
	status, raw := f.post(t, msgBody(), "sk-test")
	if status != http.StatusBadGateway || !json.Valid([]byte(raw)) || !strings.Contains(raw, `"upstream_response_too_large"`) {
		t.Fatalf("超过 16 MiB 的 JSON 应明确返回 502：status=%d bytes=%d", status, len(raw))
	}
	if got := f.st.FindAny(acc.ID); got.UseCount != 0 || got.TotalOutputTokens != 0 {
		t.Fatalf("超限响应不得记为成功：calls=%d out=%d", got.UseCount, got.TotalOutputTokens)
	}
	if f.callCount() != 1 {
		t.Fatalf("超限是响应体问题，不得换号重发：calls=%d", f.callCount())
	}
}
