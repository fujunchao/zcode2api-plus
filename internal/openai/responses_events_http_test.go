package openai

import (
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
)

const responseTextStream = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"lifecycle\",\"model\":\"GLM-5.3\",\"usage\":{\"input_tokens\":3}}}\n\n" +
	"event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
	"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"回答\"}}\n\n" +
	"event: content_block_stop\ndata: {\"index\":0}\n\n" +
	"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n" +
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

// 模拟严格的 SSE 消费端：逐事件解析并核对事件名、类型与单调序号。
func responseEvents(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var events []map[string]any
	for _, frame := range strings.Split(raw, "\n\n") {
		var name, data string
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "event: ") {
				name = strings.TrimPrefix(line, "event: ")
			}
			if strings.HasPrefix(line, "data: ") {
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if data == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			t.Fatalf("非法 SSE JSON: %v", err)
		}
		if event["type"] != name || event["sequence_number"] != float64(len(events)) {
			t.Fatalf("事件名、类型或序号不符: %v", event)
		}
		events = append(events, event)
	}
	return events
}

func mixedToolStream(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/tools-thinking.sse")
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestMixedResponsesStreamPreservesInterleavedTools(t *testing.T) {
	f := newFixture(t)
	f.respond = func(int) (int, string, string) { return http.StatusOK, "text/event-stream", mixedToolStream(t) }
	body := compatRequest("responses")
	body["stream"] = true
	status, raw := postCompat(t, f, "responses", body)
	if status != http.StatusOK || strings.Contains(raw, "private-test-signature") {
		t.Fatalf("混合流请求异常: %d %s", status, raw)
	}
	events := responseEvents(t, raw)
	added := map[string]float64{}
	arguments := map[string]string{}
	done := map[string]bool{}
	for _, event := range events {
		switch event["type"] {
		case "response.output_item.added":
			item := event["item"].(map[string]any)
			id := item["id"].(string)
			index := event["output_index"].(float64)
			if index != float64(len(added)) {
				t.Fatalf("输出索引必须连续且没有空占位: %v", event)
			}
			added[id] = index
		case "response.function_call_arguments.delta":
			id := event["item_id"].(string)
			index, exists := added[id]
			if !exists || event["output_index"] != index {
				t.Fatalf("工具增量不能定位到已声明项目: %v", event)
			}
			arguments[id] += event["delta"].(string)
		case "response.reasoning_summary_text.delta":
			if event["summary_index"] != float64(0) || event["output_index"] != float64(0) {
				t.Fatalf("思考增量索引缺失: %v", event)
			}
		case "response.output_item.done":
			item := event["item"].(map[string]any)
			done[item["id"].(string)] = true
		}
	}
	if len(added) != 4 || len(done) != 4 {
		t.Fatalf("每个项目都应声明并结束: added=%v done=%v", added, done)
	}
	for id, want := range map[string]string{"fc_call_a": `{"city":"杭州"}`, "fc_call_b": `{"city":"上海"}`} {
		if arguments[id] != want {
			t.Fatalf("交错工具参数串线: %s got=%s want=%s", id, arguments[id], want)
		}
	}
	last := events[len(events)-1]["response"].(map[string]any)
	for i, rawItem := range last["output"].([]any) {
		item := rawItem.(map[string]any)
		if added[item["id"].(string)] != float64(i) {
			t.Fatalf("最终项目顺序和流中索引不同: %v", item)
		}
	}
}

func TestResponsesSSELifecycleOverHTTP(t *testing.T) {
	f := newFixture(t)
	f.respond = func(int) (int, string, string) {
		return http.StatusOK, "text/event-stream", responseTextStream
	}
	body := compatRequest("responses")
	body["stream"], body["parallel_tool_calls"] = true, false
	status, raw := postCompat(t, f, "responses", body)
	if status != http.StatusOK {
		t.Fatalf("请求失败: %d %s", status, raw)
	}
	events := responseEvents(t, raw)
	var names []string
	for _, event := range events {
		names = append(names, event["type"].(string))
	}
	want := []string{
		"response.created", "response.in_progress",
		"response.output_item.added", "response.content_part.added", "response.output_text.delta",
		"response.output_text.done", "response.content_part.done", "response.output_item.done", "response.completed",
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("文本流生命周期不完整: %v", names)
	}
	item := events[2]["item"].(map[string]any)
	for _, i := range []int{3, 4, 5, 6} {
		if events[i]["item_id"] != item["id"] || events[i]["output_index"] != float64(0) || events[i]["content_index"] != float64(0) {
			t.Fatalf("内容分片无法定位: %v", events[i])
		}
	}
	final := events[len(events)-1]["response"].(map[string]any)
	if final["status"] != "completed" || final["parallel_tool_calls"] != false {
		t.Fatalf("最终响应应反映请求及完成状态: %v", final)
	}
	output := final["output"].([]any)
	if len(output) != 1 || output[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] != "回答" {
		t.Fatalf("最终正文不符: %v", output)
	}
}

func TestResponsesStreamTerminationOverHTTP(t *testing.T) {
	truncated := strings.Split(responseTextStream, "event: message_stop")[0]
	for _, tc := range []struct {
		name, stream, terminal, status string
	}{
		{"truncated", truncated, "response.failed", "failed"},
		{"upstream_error", truncated + "event: error\ndata: {\"error\":{\"type\":\"overloaded_error\",\"message\":\"stub failed\"}}\n\n", "response.failed", "failed"},
		{"invalid_json", truncated + "event: content_block_delta\ndata: invalid-json\n\n", "response.failed", "failed"},
		{"token_limit", strings.ReplaceAll(responseTextStream, "end_turn", "max_tokens"), "response.incomplete", "incomplete"},
		{"usage_after_limit", strings.ReplaceAll(strings.ReplaceAll(responseTextStream, "end_turn", "max_tokens"), "event: message_stop", "event: message_delta\ndata: {\"delta\":{},\"usage\":{\"output_tokens\":3}}\n\nevent: message_stop"), "response.incomplete", "incomplete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.respond = func(int) (int, string, string) { return http.StatusOK, "text/event-stream", tc.stream }
			body := compatRequest("responses")
			body["stream"] = true
			status, raw := postCompat(t, f, "responses", body)
			if status != http.StatusOK {
				t.Fatalf("SSE 响应头应已发送: %d %s", status, raw)
			}
			events := responseEvents(t, raw)
			last := events[len(events)-1]
			if last["type"] != tc.terminal {
				t.Fatalf("必须明确终止且不能伪装完成: %v", last)
			}
			response := last["response"].(map[string]any)
			if response["status"] != tc.status {
				t.Fatalf("终止状态错误: %v", response)
			}
			if tc.status == "incomplete" {
				details, _ := response["incomplete_details"].(map[string]any)
				if details["reason"] != "max_output_tokens" {
					t.Fatalf("应保留截断原因: %v", response)
				}
			} else if response["error"] == nil {
				t.Fatal("失败终止必须携带原因")
			}
		})
	}
}

func TestResponsesJSONReflectsLimitAndParallelOption(t *testing.T) {
	f := newFixture(t)
	f.respond = func(call int) (int, string, string) {
		status, contentType, body := replyText(call)
		return status, contentType, strings.ReplaceAll(body, "end_turn", "max_tokens")
	}
	body := compatRequest("responses")
	body["parallel_tool_calls"] = false
	status, raw := postCompat(t, f, "responses", body)
	var response map[string]any
	if status != http.StatusOK || json.Unmarshal([]byte(raw), &response) != nil {
		t.Fatalf("非流式请求失败: %d %s", status, raw)
	}
	if response["status"] != "incomplete" || response["parallel_tool_calls"] != false {
		t.Fatalf("非流式响应丢失状态或串行要求: %v", response)
	}
	details, _ := response["incomplete_details"].(map[string]any)
	if details["reason"] != "max_output_tokens" {
		t.Fatalf("截断原因不符: %v", response)
	}
}
