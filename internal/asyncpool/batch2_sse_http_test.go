package asyncpool

import (
	"io"
	"net/http"
	"testing"

	"zcode2api/internal/model"
)

// 合法多行 JSON 事件必须完整计量，同时原生 SSE 字节保持不变。
func TestBatch2NativeMultilineSSEUsage(t *testing.T) {
	stream := "event: message_start\r\n" +
		"data: {\r\n" +
		"data: \"type\":\"message_start\",\r\n" +
		"data: \"message\":{\"usage\":{\"input_tokens\":17,\"cache_read_input_tokens\":101,\"cache_creation_input_tokens\":5}}\r\n" +
		"data: }\r\n\r\n" +
		"event: message_delta\r\n" +
		"data:{\r\n" +
		"data:\"type\":\"message_delta\",\r\n" +
		"data:\"delta\":{\"stop_reason\":\"end_turn\"},\r\n" +
		"data:\"usage\":{\"output_tokens\":7}\r\n" +
		"data:}\r\n\r\n" +
		"event: message_stop\r\ndata:{\"type\":\"message_stop\"}\r\n\r\n"
	p, srv, _ := newDiagnosticHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, stream)
	}))
	_, status, raw := postDiagnosticHTTP(t, srv, "/v1/messages",
		`{"model":"GLM-5.3","stream":true,"messages":[{"role":"user","content":"合成多行用量回归"}]}`)
	p.Close()
	if status != http.StatusOK || raw != stream {
		t.Fatalf("原生合法 SSE 必须逐字节透传：status=%d body=%q", status, raw)
	}
	accounts := p.Store.ListAccounts(model.ProviderZai)
	if len(accounts) != 1 {
		t.Fatal("夹具必须只使用一个隔离的合成账号")
	}
	got := accounts[0]
	if got.TotalInputTokens != 17 || got.TotalOutputTokens != 7 ||
		got.TotalCacheReadTokens != 101 || got.TotalCacheCreationTokens != 5 || got.UseCount != 1 {
		t.Fatalf("完整多行事件必须准确且仅一次落账：input=%d output=%d read=%d create=%d calls=%d",
			got.TotalInputTokens, got.TotalOutputTokens, got.TotalCacheReadTokens, got.TotalCacheCreationTokens, got.UseCount)
	}
}
