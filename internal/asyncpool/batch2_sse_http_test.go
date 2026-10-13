package asyncpool

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
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

type batch2Wire struct {
	name, newline, prefix      string
	multiline, eventOnly, tail bool
	eventPadding               string
}

func batch2EncodeEvent(t *testing.T, wire batch2Wire, kind string, payload map[string]any) string {
	t.Helper()
	if wire.eventOnly {
		delete(payload, "type")
	}
	var raw []byte
	var err error
	if wire.multiline {
		raw, err = json.MarshalIndent(payload, "", "  ")
	} else {
		raw, err = json.Marshal(payload)
	}
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	output.WriteString("event:" + wire.eventPadding + kind + wire.eventPadding + wire.newline)
	for _, line := range strings.Split(string(raw), "\n") {
		output.WriteString(wire.prefix + line + wire.newline)
	}
	output.WriteString(wire.newline)
	return output.String()
}

func batch2SSE(t *testing.T, wire batch2Wire, final, stop bool) string {
	t.Helper()
	encode := func(kind string, value map[string]any) string {
		return batch2EncodeEvent(t, wire, kind, value)
	}
	stream := ": keepalive" + wire.newline + wire.newline
	stream += encode("message_start", map[string]any{
		"type": "message_start", "message": map[string]any{
			"id": "msg_batch2", "model": "GLM-5.3", "role": "assistant",
			"usage": map[string]any{"input_tokens": 0},
		},
	})
	stream += encode("content_block_start", map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
	stream += encode("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "text_delta", "text": "多行回答"},
	})
	stream += encode("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	for range 2 {
		stream += encode("message_delta", map[string]any{
			"type":  "message_delta",
			"usage": map[string]any{"input_tokens": 17, "output_tokens": 3, "cache_read_input_tokens": 101, "cache_creation_input_tokens": 5},
		})
	}
	if final {
		stream += encode("message_delta", map[string]any{
			"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"},
			"usage": map[string]any{"output_tokens": 7},
		})
		stream += encode("message_delta", map[string]any{
			"type": "message_delta", "usage": map[string]any{"input_tokens": 0, "output_tokens": 6, "cache_read_input_tokens": -1},
		})
	}
	if stop {
		stream += encode("message_stop", map[string]any{"type": "message_stop"})
	}
	if wire.tail {
		stream = strings.TrimRight(stream, "\r\n")
	}
	return stream
}

func batch2HTTPBody(path string) string {
	if path == "/v1/responses" {
		return `{"model":"GLM-5.3","stream":true,"input":"合成多行回归"}`
	}
	return `{"model":"GLM-5.3","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"合成多行回归"}]}`
}

func batch2ClientPayloads(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var values []map[string]any
	for _, line := range strings.Split(raw, "\n") {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			continue
		}
		var value map[string]any
		if err := json.Unmarshal([]byte(data), &value); err != nil {
			t.Fatalf("网关转换后的单行客户端事件必须合法：%v", err)
		}
		values = append(values, value)
	}
	return values
}

func TestBatch2SSEAcrossHTTPProtocols(t *testing.T) {
	for _, wire := range []batch2Wire{
		{"single_line", "\n", "data: ", false, false, false, ""},
		{"multiline", "\n", "data: ", true, false, false, ""},
		{"no_space_crlf", "\r\n", "data:", true, false, false, ""},
		{"event_only", "\r\n", "data:", true, true, false, ""},
		{"tail_without_blank", "\n", "data:", true, true, true, ""},
		{"spaced_event", "\n", "data:", true, true, false, " \t"},
	} {
		for _, path := range []string{"/v1/messages", "/async/v1/messages", "/v1/chat/completions", "/v1/responses"} {
			t.Run(wire.name+path, func(t *testing.T) {
				stream := batch2SSE(t, wire, true, true)
				var calls atomic.Int32
				p, srv, _ := newDiagnosticHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					w.Header().Set("Content-Type", "text/event-stream")
					// 主动分段写出；读取端还可能继续合并或拆分，不依赖 TCP 分包边界。
					for data := []byte(stream); len(data) > 0; {
						n := min(19, len(data))
						_, _ = w.Write(data[:n])
						w.(http.Flusher).Flush()
						data = data[n:]
					}
				}))
				_, status, raw := postDiagnosticHTTP(t, srv, path, batch2HTTPBody(path))
				p.Close()
				if status != http.StatusOK || calls.Load() != 1 {
					t.Fatalf("合法流必须完整交付且不重试：status=%d calls=%d body=%s", status, calls.Load(), raw)
				}
				a := p.Store.ListAccounts(model.ProviderZai)[0]
				if a.TotalInputTokens != 17 || a.TotalOutputTokens != 7 || a.TotalCacheReadTokens != 101 ||
					a.TotalCacheCreationTokens != 5 || a.UseCount != 1 || a.StreamTruncateCount != 0 {
					t.Fatalf("完整事件只计一次且不产生假断流：in=%d out=%d read=%d create=%d calls=%d trunc=%d",
						a.TotalInputTokens, a.TotalOutputTokens, a.TotalCacheReadTokens, a.TotalCacheCreationTokens, a.UseCount, a.StreamTruncateCount)
				}
				if path == "/v1/messages" {
					if raw != stream {
						t.Fatal("原生 SSE 不得重排、补写或规范化原字节")
					}
					return
				}
				payloads := batch2ClientPayloads(t, raw)
				// 异步保留 ensure_ascii 的既有转义；按解码后的内容精确断言，不改线格式。
				var text strings.Builder
				for _, payload := range payloads {
					switch path {
					case "/async/v1/messages":
						if payload["type"] == "content_block_delta" {
							delta, _ := payload["delta"].(map[string]any)
							value, _ := delta["text"].(string)
							text.WriteString(value)
						}
					case "/v1/chat/completions":
						choices, _ := payload["choices"].([]any)
						for _, rawChoice := range choices {
							choice, _ := rawChoice.(map[string]any)
							delta, _ := choice["delta"].(map[string]any)
							value, _ := delta["content"].(string)
							text.WriteString(value)
						}
					case "/v1/responses":
						if payload["type"] == "response.output_text.delta" {
							value, _ := payload["delta"].(string)
							text.WriteString(value)
						}
					}
				}
				if text.String() != "多行回答" {
					t.Fatalf("解码后的文本不能丢失或重复：%q", text.String())
				}
				if path == "/async/v1/messages" {
					counts := map[string]int{}
					for _, payload := range payloads {
						kind, _ := payload["type"].(string)
						counts[kind]++
					}
					if !strings.Contains(raw, "event: done") || counts["message_start"] != 1 ||
						counts["content_block_delta"] != 1 || counts["message_delta"] != 4 || counts["message_stop"] != 1 {
						t.Fatalf("异步不能漏事件或丢失 event-only 类型：%v", counts)
					}
					return
				}
				var usage map[string]any
				for _, payload := range payloads {
					if path == "/v1/chat/completions" {
						if value, ok := payload["usage"].(map[string]any); ok {
							usage = value
						}
					} else if payload["type"] == "response.completed" {
						response, _ := payload["response"].(map[string]any)
						usage, _ = response["usage"].(map[string]any)
					}
				}
				inputKey, outputKey, detailKey := "input_tokens", "output_tokens", "input_tokens_details"
				if path == "/v1/chat/completions" {
					inputKey, outputKey, detailKey = "prompt_tokens", "completion_tokens", "prompt_tokens_details"
					if !strings.Contains(raw, "data: [DONE]") {
						t.Fatal("Chat 流应按原协议结束")
					}
				}
				details, _ := usage[detailKey].(map[string]any)
				if usage[inputKey] != float64(123) || usage[outputKey] != float64(7) ||
					usage["total_tokens"] != float64(130) || details["cached_tokens"] != float64(101) {
					t.Fatalf("兼容端点的输入含缓存口径及终值必须保持：%v", usage)
				}
			})
		}
	}
}

func TestBatch2SSEFailuresDoNotReplayOrInventUsage(t *testing.T) {
	wire := batch2Wire{"errors", "\r\n", "data:", true, true, false, ""}
	for _, mode := range []string{"without_final", "without_stop", "business_error", "malformed_event"} {
		for _, path := range []string{"/v1/messages", "/async/v1/messages", "/v1/chat/completions", "/v1/responses"} {
			t.Run(mode+path, func(t *testing.T) {
				final := mode != "without_final" && mode != "malformed_event"
				stream := batch2SSE(t, wire, final, false)
				if mode == "business_error" {
					stream += batch2EncodeEvent(t, wire, "error", map[string]any{
						"type": "error", "error": map[string]any{"type": "overloaded_error", "message": "synthetic business error"},
					})
				}
				if mode == "malformed_event" {
					// 两个独立 JSON 对象挤进同一事件，不得把第一行的 999 当作合法终值。
					stream += "event:message_delta\r\n" +
						"data:{\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":999}}\r\n" +
						"data:{\"unexpected\":true}\r\n\r\n"
				}
				var calls atomic.Int32
				p, srv, _ := newDiagnosticHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, stream)
				}))
				_, _, raw := postDiagnosticHTTP(t, srv, path, batch2HTTPBody(path))
				p.Close()
				if !strings.Contains(raw, "error") || calls.Load() != 1 {
					t.Fatalf("已输出的错误流必须终止，不能换号重放：calls=%d body=%s", calls.Load(), raw)
				}
				if path == "/async/v1/messages" && strings.Contains(raw, "event: done") {
					t.Fatal("异步错误流不能冒充成功")
				}
				a := p.Store.ListAccounts(model.ProviderZai)[0]
				wantIn, wantOut, wantRead, wantCreate := 0, 0, 0, 0
				if final {
					wantIn, wantOut, wantRead, wantCreate = 17, 7, 101, 5
				}
				if a.TotalInputTokens != wantIn || a.TotalOutputTokens != wantOut ||
					a.TotalCacheReadTokens != wantRead || a.TotalCacheCreationTokens != wantCreate {
					t.Fatalf("中断只保留合法完整事件的终值，不接受伪造或半截用量：%+v", a.TotalOutputTokens)
				}
				wantTrunc := 1
				if mode == "business_error" {
					wantTrunc = 0
				}
				if a.StreamTruncateCount != wantTrunc {
					t.Fatalf("业务 error 不能被当作代理/线路截断：got=%d want=%d", a.StreamTruncateCount, wantTrunc)
				}
			})
		}
	}
}
