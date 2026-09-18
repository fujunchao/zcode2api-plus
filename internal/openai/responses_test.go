// /v1/responses 测试：请求转换、响应转换、流式重编码、previous_response_id 400、
// 以及复用 fixture 的端到端链路。
package openai

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestResponsesConvertStringInput(t *testing.T) {
	got, err := ConvertResponsesRequest(map[string]any{
		"model":             "glm-5.3-flash",
		"instructions":      "你是助手",
		"input":             "你好",
		"max_output_tokens": float64(512),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got["max_tokens"] != float64(512) {
		t.Fatalf("max_output_tokens 未映射: %v", got["max_tokens"])
	}
	sys, _ := got["system"].([]any)
	if len(sys) != 1 || sys[0].(map[string]any)["text"] != "你是助手" {
		t.Fatalf("instructions 未转 system: %v", got["system"])
	}
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("应 1 条消息: %v", msgs)
	}
	msg := msgs[0].(map[string]any)
	if msg["role"] != "user" {
		t.Fatalf("角色应为 user: %v", msg)
	}
}

func TestResponsesConvertItems(t *testing.T) {
	got, err := ConvertResponsesRequest(map[string]any{
		"model": "glm-5.3-flash",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "查天气"},
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "get_weather", "arguments": `{"city":"台北"}`},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "晴 28 度"},
			map[string]any{"type": "reasoning", "summary": "略"}, // 忽略
		},
		"tools": []any{map[string]any{
			"type": "function", "name": "get_weather", "description": "查天气",
			"parameters": map[string]any{"type": "object"},
		}},
		"reasoning": map[string]any{"effort": "high"},
	})
	if err != nil {
		t.Fatal(err)
	}
	tools, _ := got["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "get_weather" {
		t.Fatalf("扁平 tools 未转换: %v", got["tools"])
	}
	oc, _ := got["output_config"].(map[string]any)
	if oc["effort"] != "high" {
		t.Fatalf("reasoning.effort 未映射: %v", got["output_config"])
	}
	if _, invented := got["thinking"]; invented {
		t.Fatalf("原生 effort 不应再被替换成固定思考预算: %v", got["thinking"])
	}
	msgs, _ := got["messages"].([]any)
	// user → assistant(tool_use) → user(tool_result) 共 3 条
	if len(msgs) != 3 {
		t.Fatalf("应 3 条消息: %v", msgs)
	}
	if msgs[1].(map[string]any)["role"] != "assistant" {
		t.Fatalf("function_call 应成 assistant 消息: %v", msgs[1])
	}
	last := msgs[2].(map[string]any)
	if last["role"] != "user" {
		t.Fatalf("function_call_output 应成 user 消息: %v", last)
	}
	block := last["content"].([]any)[0].(map[string]any)
	if block["type"] != "tool_result" || block["tool_use_id"] != "call_1" {
		t.Fatalf("tool_result 绑定不符: %v", block)
	}
}

func TestResponsesPreviousResponseIDRejected(t *testing.T) {
	_, err := ConvertResponsesRequest(map[string]any{
		"model": "glm-5.3-flash", "previous_response_id": "resp_x", "input": "hi",
	})
	if err == nil || !strings.Contains(err.Error(), "previous_response_id") {
		t.Fatalf("previous_response_id 应 400: %v", err)
	}
}

func TestResponsesConvertResponseShape(t *testing.T) {
	got := ConvertResponsesResponse(map[string]any{
		"id": "msg_a", "model": "GLM-5.3", "stop_reason": "tool_use",
		"usage": map[string]any{"input_tokens": 6, "output_tokens": 3},
		"content": []any{
			map[string]any{"type": "text", "text": "部分"},
			map[string]any{"type": "tool_use", "id": "call_1", "name": "f", "input": map[string]any{"a": 1}},
		},
	})
	if got == nil {
		t.Fatal("不应返回 nil")
	}
	if got["object"] != "response" || got["status"] != "completed" {
		t.Fatalf("顶层形态不符: %v", got)
	}
	output, _ := got["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("应 2 个 output item: %v", output)
	}
	msgItem := output[0].(map[string]any)
	if msgItem["type"] != "message" {
		t.Fatalf("首 item 应为 message: %v", msgItem)
	}
	text := msgItem["content"].([]any)[0].(map[string]any)
	if text["type"] != "output_text" || text["text"] != "部分" {
		t.Fatalf("output_text 不符: %v", text)
	}
	call := output[1].(map[string]any)
	if call["type"] != "function_call" || call["call_id"] != "call_1" || call["arguments"] != `{"a":1}` {
		t.Fatalf("function_call item 不符: %v", call)
	}
	usage := got["usage"].(map[string]any)
	if usage["input_tokens"] != float64(6) || usage["total_tokens"] != float64(9) {
		t.Fatalf("usage 不符: %v", usage)
	}
}

func TestResponsesStreamEvents(t *testing.T) {
	upstream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"m1","model":"GLM-5.3","usage":{"input_tokens":4}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"你好"}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"c1","name":"f"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"a\":"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","usage":{"output_tokens":2}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	var events []string
	if err := reencodeResponsesSSE(strings.NewReader(upstream), func(ev string) error {
		events = append(events, ev)
		return nil
	}, true); err != nil {
		t.Fatal(err)
	}

	names := make([]string, 0, len(events))
	for _, ev := range events {
		name := strings.SplitN(ev, "\n", 2)[0]
		names = append(names, strings.TrimPrefix(name, "event: "))
	}
	want := []string{
		"response.created", "response.in_progress", "response.output_item.added", "response.content_part.added", "response.output_text.delta",
		"response.output_item.added", "response.function_call_arguments.delta",
		"response.output_text.done", "response.content_part.done", "response.output_item.done",
		"response.function_call_arguments.done", "response.output_item.done",
		"response.completed",
	}
	if len(names) != len(want) {
		t.Fatalf("事件序列不符: %v", names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("第 %d 个事件应为 %s，得到 %s", i, want[i], names[i])
		}
	}

	// completed 事件携带完整 output 与 usage
	var payload map[string]any
	for _, ev := range events {
		if strings.Contains(ev, "response.completed") {
			dataLine := strings.SplitN(ev, "\n", 2)[1]
			if err := json.Unmarshal([]byte(strings.TrimPrefix(dataLine, "data: ")), &payload); err != nil {
				t.Fatal(err)
			}
		}
	}
	resp, _ := payload["response"].(map[string]any)
	if resp["status"] != "completed" {
		t.Fatalf("completed 状态不符: %v", resp)
	}
	output, _ := resp["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("completed 应含 message + function_call: %v", output)
	}
	usage, _ := resp["usage"].(map[string]any)
	if usage["input_tokens"] != float64(4) || usage["output_tokens"] != float64(2) {
		t.Fatalf("completed usage 不符: %v", usage)
	}

	// output_item.added 与 function_call_arguments.delta 必须用同一个 item_id，
	// 否则客户端无法把参数增量关联到对应工具调用。
	var addedID, deltaID string
	for _, ev := range events {
		data := strings.TrimPrefix(strings.SplitN(ev, "\n", 2)[1], "data: ")
		var obj map[string]any
		if json.Unmarshal([]byte(data), &obj) != nil {
			continue
		}
		switch {
		case strings.Contains(ev, "response.output_item.added"):
			if item, ok := obj["item"].(map[string]any); ok {
				addedID = stringOf(item["id"])
			}
		case strings.Contains(ev, "response.function_call_arguments.delta"):
			deltaID = stringOf(obj["item_id"])
		}
	}
	if addedID == "" || deltaID == "" {
		t.Fatalf("未捕获到 item id: added=%q delta=%q", addedID, deltaID)
	}
	if addedID != deltaID {
		t.Fatalf("item_id 必须一致: added=%q delta=%q", addedID, deltaID)
	}
}

func TestResponsesE2ENonStream(t *testing.T) {
	f := newFixture(t)
	f.setResponder(func(int) (int, string, string) {
		return http.StatusOK, "application/json",
			`{"id":"msg_a","type":"message","role":"assistant","model":"GLM-5.3",
			  "stop_reason":"end_turn","usage":{"input_tokens":6,"output_tokens":3},
			  "content":[{"type":"text","text":"回答"}]}`
	})
	code, body := post(f, t, "sk-test", `{"model":"glm-5.3-flash","input":"你好"}`)
	if code != http.StatusOK {
		t.Fatalf("应 200: %d %s", code, body)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["object"] != "response" {
		t.Fatalf("应 response 对象: %s", body)
	}
	up := f.lastUpstream()
	if up.Body["max_tokens"] != float64(8192) {
		t.Fatalf("缺省 max_tokens 应 8192: %v", up.Body["max_tokens"])
	}
}

func TestResponsesE2EStream(t *testing.T) {
	f := newFixture(t)
	f.setResponder(func(int) (int, string, string) {
		return http.StatusOK, "text/event-stream",
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"GLM-5.3\",\"usage\":{\"input_tokens\":4}}}\n\n" +
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"回复\"}}\n\n" +
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	})
	code, body := post(f, t, "sk-test", `{"model":"glm-5.3-flash","input":"hi","stream":true}`)
	if code != http.StatusOK {
		t.Fatalf("应 200: %d %s", code, body)
	}
	if !strings.Contains(body, "event: response.created") ||
		!strings.Contains(body, "event: response.output_text.delta") ||
		!strings.Contains(body, "event: response.completed") {
		t.Fatalf("response.* 事件缺失: %s", body)
	}
}

// post /v1/responses 请求助手（与 postChat 同源 fixture）。
func post(f *fixture, t *testing.T, key, payload string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/v1/responses", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw := make([]byte, 0)
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		raw = append(raw, buf[:n]...)
		if err != nil {
			break
		}
	}
	return resp.StatusCode, string(raw)
}

// ── 思维链（§5.8 增量）────────────────────────────────────────────────────────

func TestResponsesThinkingBecomesReasoningItem(t *testing.T) {
	got := ConvertResponsesResponse(map[string]any{
		"id": "msg_r", "model": "GLM-5.3", "stop_reason": "end_turn",
		"usage": map[string]any{"input_tokens": 5, "output_tokens": 7},
		"content": []any{
			map[string]any{"type": "thinking", "thinking": "先想", "signature": "c2ln"},
			map[string]any{"type": "text", "text": "答案 2"},
		},
	})
	if got == nil {
		t.Fatal("不应返回 nil")
	}
	output, _ := got["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("应 reasoning + message 两个 item: %v", output)
	}
	reasoning := output[0].(map[string]any)
	if reasoning["type"] != "reasoning" || reasoning["status"] != "completed" {
		t.Fatalf("首 item 应为 reasoning: %v", reasoning)
	}
	summary, _ := reasoning["summary"].([]any)
	if len(summary) != 1 || summary[0].(map[string]any)["text"] != "先想" {
		t.Fatalf("summary 未承载思考内容: %v", reasoning["summary"])
	}
	msgItem := output[1].(map[string]any)
	if msgItem["type"] != "message" {
		t.Fatalf("次 item 应为 message: %v", msgItem)
	}
	if text := msgItem["content"].([]any)[0].(map[string]any); text["text"] != "答案 2" {
		t.Fatalf("正文不应被思考内容污染: %v", text)
	}
}

func TestResponsesStreamEmitsReasoningEvents(t *testing.T) {
	upstream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"m9","model":"GLM-5.3","usage":{"input_tokens":4}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"先想一下"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"c2ln"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"答案是 2"}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	var events []string
	if err := reencodeResponsesSSE(strings.NewReader(upstream), func(ev string) error {
		events = append(events, ev)
		return nil
	}, true); err != nil {
		t.Fatal(err)
	}

	names := make([]string, 0, len(events))
	for _, ev := range events {
		names = append(names, strings.TrimPrefix(strings.SplitN(ev, "\n", 2)[0], "event: "))
	}
	want := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_text.delta",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.reasoning_summary_text.done",
		"response.reasoning_summary_part.done",
		"response.output_item.done",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	}
	if len(names) != len(want) {
		t.Fatalf("事件序列不符: %v", names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("第 %d 个事件应为 %s: %v", i, want[i], names)
		}
	}

	last := events[len(events)-1]
	if !strings.Contains(last, `"type":"summary_text"`) || !strings.Contains(last, "先想一下") {
		t.Fatalf("reasoning summary 未回填思考内容: %s", last)
	}
	// reasoning item 必须排在 message item 之前（与 output_index 分配一致）
	if r, m := strings.Index(last, `"type":"reasoning"`), strings.Index(last, `"type":"message"`); r < 0 || m < 0 || r > m {
		t.Fatalf("reasoning item 应排在 message 之前: %s", last)
	}
	if strings.Contains(last, "c2ln") {
		t.Fatalf("思考签名不应出现在响应中: %s", last)
	}
}

// function_call_output 的 output 允许是内容块数组——Codex 在工具返回结构化内容时
// 就会发数组。只做字符串断言会把整段工具输出静默变成空串。
func TestFunctionCallOutputAcceptsContentArray(t *testing.T) {
	cases := []struct {
		name string
		raw  any
		want string
	}{
		{"字符串", "plain", "plain"},
		{"单元素数组", []any{map[string]any{"type": "input_text", "text": "from-array"}}, "from-array"},
		{"多元素数组", []any{
			map[string]any{"type": "input_text", "text": "a"},
			map[string]any{"type": "text", "text": "b"},
		}, "a\nb"},
		{"单个内容块", map[string]any{"type": "input_text", "text": "one"}, "one"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := functionCallOutputText(c.raw); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
	// 非文本内容块没有 text 字段：退化为紧凑 JSON，而不是丢掉。
	if got := functionCallOutputText([]any{map[string]any{"type": "input_image", "image_url": "u"}}); !strings.Contains(got, "input_image") {
		t.Fatalf("无 text 的内容块不应被丢弃: %q", got)
	}
}

// 数组形态必须真的穿过转换链路进到 tool_result，而不只是辅助函数能解析。
func TestFunctionCallOutputArrayReachesToolResult(t *testing.T) {
	msgs, err := convertResponsesInput([]any{
		map[string]any{"type": "function_call", "call_id": "call_1", "name": "lookup", "arguments": "{}"},
		map[string]any{"type": "function_call_output", "call_id": "call_1",
			"output": []any{map[string]any{"type": "input_text", "text": "tool-said-hi"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(msgs)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), "tool-said-hi") {
		t.Fatalf("数组形态的工具输出丢失: %s", encoded)
	}
	if !strings.Contains(string(encoded), "tool_result") {
		t.Fatalf("应转换为 tool_result: %s", encoded)
	}
}

// Responses 的 input_tokens 必须并入缓存两系，并暴露 input_tokens_details：
// Codex 正是按 cached_tokens 判断上下文压缩时机的。
func TestResponsesUsageIncludesCacheTokens(t *testing.T) {
	usage := responsesUsage(map[string]any{
		"input_tokens":                float64(10),
		"output_tokens":               float64(5),
		"cache_read_input_tokens":     float64(3),
		"cache_creation_input_tokens": float64(2),
	})
	if usage["input_tokens"] != float64(15) || usage["total_tokens"] != float64(20) {
		t.Fatalf("input 应含缓存两系: %v", usage)
	}
	details, _ := usage["input_tokens_details"].(map[string]any)
	if details == nil || details["cached_tokens"] != float64(3) {
		t.Fatalf("应暴露 cached_tokens: %v", details)
	}
	if details["cache_write_tokens"] != float64(2) {
		t.Fatalf("应暴露 cache_write_tokens: %v", details)
	}
	// 无缓存时也要给出 details，保持字段形态稳定
	plain := responsesUsage(map[string]any{"input_tokens": float64(1), "output_tokens": float64(1)})
	if plain["input_tokens"] != float64(1) {
		t.Fatalf("无缓存时 input 不应被改动: %v", plain)
	}
}
