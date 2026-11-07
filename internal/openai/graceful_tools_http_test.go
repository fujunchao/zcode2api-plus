package openai

import (
	"net/http"
	"strings"
	"testing"

	"zcode2api/internal/config"
)

// 从两种公开 HTTP 接口进入，覆盖交错工具、无参数工具和已闭合工具的正反例。
func TestGracefulToolCompletionGuard(t *testing.T) {
	old := config.TruncateTerminal
	config.TruncateTerminal = config.TruncateTerminalGraceful
	t.Cleanup(func() { config.TruncateTerminal = old })
	start := "event: message_start\ndata: {\"message\":{\"id\":\"guard\",\"model\":\"GLM-5.3\"}}\n\n"
	tool := "event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_0\",\"name\":\"get_weather\",\"input\":{}}}\n\n"
	partial := "event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"city\\\":\"}}\n\n"
	closed := "event: content_block_stop\ndata: {\"index\":0}\n\n"
	secondTool := "event: content_block_start\ndata: {\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_1\",\"name\":\"get_weather\",\"input\":{}}}\n\n" +
		"event: content_block_stop\ndata: {\"index\":1}\n\n"
	for _, api := range []string{"chat", "responses"} {
		for _, tc := range []struct {
			name      string
			body      string
			wantError bool
		}{
			{"半截参数", start + tool + partial, true},
			{"未开始参数", start + tool, true},
			{"只关闭后一个工具", start + tool + partial + secondTool, true},
			{"工具已闭合", start + tool + closed, false},
			{"普通文本截断", strings.Split(responseTextStream, "event: message_delta")[0], false},
		} {
			t.Run(api+"/"+tc.name, func(t *testing.T) {
				f := newFixture(t)
				f.setResponder(func(int) (int, string, string) { return http.StatusOK, "text/event-stream", tc.body })
				body := compatRequest(api)
				body["stream"] = true
				status, raw := postCompat(t, f, api, body)
				if status != http.StatusOK {
					t.Fatalf("流开始后应保持 HTTP 200: %d %s", status, raw)
				}
				completed := strings.Contains(raw, "data: [DONE]") || strings.Contains(raw, "event: response.completed")
				if tc.wantError {
					if completed || !strings.Contains(raw, "overloaded_error") {
						t.Fatalf("未闭合工具必须降级为可重试错误: %s", raw)
					}
				} else if !completed || strings.Contains(raw, "overloaded_error") {
					t.Fatalf("可安全收尾的内容仍应支持 graceful: %s", raw)
				}
			})
		}
	}
}
