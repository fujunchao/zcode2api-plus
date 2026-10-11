package asyncpool

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"zcode2api/internal/model"
)

const lateUsageStart = "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_late\",\"model\":\"GLM-5.3\",\"usage\":{\"input_tokens\":0}}}\n\n" +
	"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n"
const lateUsageUpdate = "data: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":17,\"cache_read_input_tokens\":101,\"cache_creation_input_tokens\":5,\"output_tokens\":3}}\n\n"
const lateUsageFinal = "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":7}}\n\n"
const lateUsageStop = "data: {\"type\":\"message_stop\"}\n\n"

// 同一合成流：输入/cache 在中间补报，终态只补输出；四种入口都必须如实计数。
func TestLateUsageAcrossHTTPProtocols(t *testing.T) {
	stream := lateUsageStart + lateUsageUpdate + lateUsageUpdate + lateUsageFinal +
		"data: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":0,\"cache_read_input_tokens\":0,\"cache_creation_input_tokens\":0,\"output_tokens\":6}}\n\n" + lateUsageStop
	for _, tc := range []struct{ path, body, terminal string }{
		{"/v1/messages", `{"model":"GLM-5.3","stream":true,"messages":[{"role":"user","content":"hi"}]}`, "message_stop"},
		{"/async/v1/messages", `{"model":"GLM-5.3","stream":true,"messages":[{"role":"user","content":"hi"}]}`, "event: done"},
		{"/v1/chat/completions", `{"model":"GLM-5.3","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`, "data: [DONE]"},
		{"/v1/responses", `{"model":"GLM-5.3","stream":true,"input":"hi"}`, "response.completed"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			p, srv, buf := newDiagnosticHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, stream)
			}))
			_, status, raw := postDiagnosticHTTP(t, srv, tc.path, tc.body)
			p.Close()
			if status != 200 || !strings.Contains(raw, tc.terminal) {
				t.Fatalf("不能改变流式终态：%d %s", status, raw)
			}
			if tc.path == "/v1/messages" && raw != stream {
				t.Fatal("原生 SSE 必须逐字节透传，不修补或重写上游内容")
			}
			accs := p.Store.ListAccounts(model.ProviderZai)
			if len(accs) != 1 {
				t.Fatal("夹具应只有一个合成账号")
			}
			a := accs[0]
			if a.TotalInputTokens != 17 || a.TotalOutputTokens != 7 || a.TotalCacheReadTokens != 101 || a.TotalCacheCreationTokens != 5 || a.UseCount != 1 {
				t.Errorf("迟到用量必须恰好落账一次：in=%d out=%d read=%d create=%d calls=%d", a.TotalInputTokens, a.TotalOutputTokens, a.TotalCacheReadTokens, a.TotalCacheCreationTokens, a.UseCount)
			}
			if !strings.Contains(buf.String(), "usage=in:17/out:7/cache_r:101/cache_c:5 complete=1") {
				t.Errorf("汇总不能继续漏报输入/cache：%s", buf.String())
			}
			if tc.path == "/v1/messages" || tc.path == "/async/v1/messages" {
				return
			}
			var usage map[string]any
			for _, line := range strings.Split(raw, "\n") {
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				var payload map[string]any
				if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload) != nil {
					continue
				}
				if tc.path == "/v1/chat/completions" {
					if u, ok := payload["usage"].(map[string]any); ok {
						usage = u
					}
				} else if payload["type"] == "response.completed" {
					response, _ := payload["response"].(map[string]any)
					usage, _ = response["usage"].(map[string]any)
				}
			}
			inputKey, outputKey, detailKey := "input_tokens", "output_tokens", "input_tokens_details"
			if tc.path == "/v1/chat/completions" {
				inputKey, outputKey, detailKey = "prompt_tokens", "completion_tokens", "prompt_tokens_details"
			}
			details, _ := usage[detailKey].(map[string]any)
			if usage[inputKey] != float64(123) || usage[outputKey] != float64(7) || usage["total_tokens"] != float64(130) || details["cached_tokens"] != float64(101) {
				t.Errorf("兼容响应必须保留跨多次 delta 的累计值，并沿用缓存计入总输入的口径：%v", usage)
			}
		})
	}
}

func TestLateUsageInterruptedStreamAccounting(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/async/v1/messages"} {
		for _, final := range []bool{false, true} {
			name := path + "/without-final"
			if final {
				name = path + "/with-final"
			}
			t.Run(name, func(t *testing.T) {
				stream := lateUsageStart + lateUsageUpdate
				if final {
					stream += lateUsageFinal
				}
				p, srv, buf := newDiagnosticHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, stream)
				}))
				_, _, raw := postDiagnosticHTTP(t, srv, path, `{"model":"GLM-5.3","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
				p.Close()
				if !strings.Contains(raw, "error") || strings.Contains(raw, "event: done") {
					t.Fatal("缺少 message_stop 必须仍作为流中断交付")
				}
				a := p.Store.ListAccounts(model.ProviderZai)[0]
				wantIn, wantOut, wantRead, wantCreate := 0, 0, 0, 0
				if final {
					wantIn, wantOut, wantRead, wantCreate = 17, 7, 101, 5
				}
				if a.TotalInputTokens != wantIn || a.TotalOutputTokens != wantOut || a.TotalCacheReadTokens != wantRead || a.TotalCacheCreationTokens != wantCreate {
					t.Errorf("中断只能保留已确认终值，且不能重复累加：in=%d out=%d read=%d create=%d", a.TotalInputTokens, a.TotalOutputTokens, a.TotalCacheReadTokens, a.TotalCacheCreationTokens)
				}
				attempts := 0
				for _, line := range strings.Split(buf.String(), "\n") {
					if strings.Contains(line, "[attempt]") && strings.Contains(line, "event=start") {
						attempts++
					}
				}
				if attempts != 1 {
					t.Fatalf("已开始交付的流中断不得换号重发：%d", attempts)
				}
			})
		}
	}
}
