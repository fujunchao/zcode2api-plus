package openai

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestChatStreamPreservesInitialToolInput(t *testing.T) {
	for _, input := range []string{`{"city":"杭州"}`, `{}`} {
		t.Run(input, func(t *testing.T) {
			f := newFixture(t)
			f.respond = func(int) (int, string, string) {
				return http.StatusOK, "text/event-stream",
					"event: message_start\ndata: {\"message\":{\"id\":\"initial_tool\",\"model\":\"GLM-5.3\"}}\n\n" +
						"event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_initial\",\"name\":\"get_weather\",\"input\":" + input + "}}\n\n" +
						"event: content_block_stop\ndata: {\"index\":0}\n\n" +
						"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"tool_use\"}}\n\n" +
						"event: message_stop\ndata: {}\n\n"
			}
			body := compatRequest("chat")
			body["stream"] = true
			status, raw := postCompat(t, f, "chat", body)
			if status != http.StatusOK {
				t.Fatalf("请求失败: %d %s", status, raw)
			}
			arguments := ""
			for _, line := range strings.Split(raw, "\n") {
				if !strings.HasPrefix(line, "data: {") {
					continue
				}
				var event map[string]any
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
					t.Fatal(err)
				}
				choices, _ := event["choices"].([]any)
				if len(choices) == 0 {
					continue
				}
				delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)
				calls, _ := delta["tool_calls"].([]any)
				for _, rawCall := range calls {
					call := rawCall.(map[string]any)
					fn, _ := call["function"].(map[string]any)
					arguments += stringOf(fn["arguments"])
				}
			}
			if arguments != input || !strings.Contains(raw, "data: [DONE]") {
				t.Fatalf("无参数增量时仍须交付完整初始 input: got=%q want=%q", arguments, input)
			}
		})
	}
}

func TestChatStreamReportsUpstreamFailure(t *testing.T) {
	truncated := strings.Split(responseTextStream, "event: message_stop")[0]
	for _, tail := range []string{
		"",
		"event: error\ndata: {\"error\":{\"message\":\"stub failed\"}}\n\n",
		"event: content_block_delta\ndata: invalid-json\n\n",
		"event: content_block_delta\ndata: {\"index\":99,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{}\"}}\n\n",
	} {
		f := newFixture(t)
		f.respond = func(int) (int, string, string) { return http.StatusOK, "text/event-stream", truncated + tail }
		body := compatRequest("chat")
		body["stream"] = true
		status, raw := postCompat(t, f, "chat", body)
		if status != http.StatusOK || !strings.Contains(raw, `"type":"upstream_stream_error"`) || strings.Contains(raw, "data: [DONE]") {
			t.Fatalf("异常串流须显式报错且不能伪装正常结束: %d %s", status, raw)
		}
	}
}

func TestChatInitialToolArgumentsPrecedeFinish(t *testing.T) {
	f := newFixture(t)
	f.respond = func(int) (int, string, string) {
		return http.StatusOK, "text/event-stream",
			"event: message_start\ndata: {\"message\":{\"id\":\"initial\",\"model\":\"GLM-5.3\"}}\n\n" +
				"event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_a\",\"name\":\"get_weather\",\"input\":{\"city\":\"杭州\"}}}\n\n" +
				"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"tool_use\"}}\n\n" +
				"event: message_stop\ndata: {}\n\n"
	}
	body := compatRequest("chat")
	body["stream"] = true
	status, raw := postCompat(t, f, "chat", body)
	argsAt := strings.Index(raw, `"arguments":"{`)
	finishAt := strings.Index(raw, `"finish_reason":"tool_calls"`)
	if status != http.StatusOK || argsAt < 0 || finishAt < 0 || argsAt > finishAt {
		t.Fatalf("工具参数必须在 finish_reason 前发出: %d %s", status, raw)
	}
}
