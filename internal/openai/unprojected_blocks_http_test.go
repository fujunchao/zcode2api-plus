// OpenAI 投影对 MCP / 内建服务端工具块的防御性兼容。
//
// 上游（ZCode 3.18.0 起）会在一轮对话里穿插 mcp_tool_use / server_tool_use 及其结果块，
// 它们的参数同样以 input_json_delta 到达。Chat 与 Responses 都只投影 text / thinking /
// 调用方声明的函数工具：这类块由上游自己执行，网关既不代跑也没有 OpenAI 侧的结果形态可
// 回传，因此只登记占位。否则同 index 的参数增量会被判成「引用了未声明的内容块」，让整条
// 本可正常收尾的流中断，并计入账号断流。
package openai

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"zcode2api/internal/model"
)

// ── 合成上游 SSE 的帧构造 ────────────────────────────────────────────────────

const (
	// streamHead 是最小上游开场（带 usage，便于同时核对用量透传）。
	streamHead = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"server_blocks\",\"model\":\"GLM-5.3\",\"usage\":{\"input_tokens\":5}}}\n\n"
	// streamNormalEnd 以 end_turn 正常收尾。
	streamNormalEnd = "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":4}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	// streamToolEnd 以 tool_use 收尾（普通函数工具轮次）。
	streamToolEnd = "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":4}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
)

// blockStartFrame 构造 content_block_start；block 是 content_block 对象的 JSON。
func blockStartFrame(index int, block string) string {
	return "event: content_block_start\ndata: {\"index\":" + strconv.Itoa(index) + ",\"content_block\":" + block + "}\n\n"
}

// jsonDeltaFrame 构造 input_json_delta（工具 / MCP 参数增量）。
func jsonDeltaFrame(index int, partial string) string {
	return "event: content_block_delta\ndata: {\"index\":" + strconv.Itoa(index) +
		",\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":" + strconv.Quote(partial) + "}}\n\n"
}

// textDeltaFrame 构造 text_delta。
func textDeltaFrame(index int, text string) string {
	return "event: content_block_delta\ndata: {\"index\":" + strconv.Itoa(index) +
		",\"delta\":{\"type\":\"text_delta\",\"text\":" + strconv.Quote(text) + "}}\n\n"
}

// blockStopFrame 构造 content_block_stop。
func blockStopFrame(index int) string {
	return "event: content_block_stop\ndata: {\"index\":" + strconv.Itoa(index) + "}\n\n"
}

// ── Chat 流的解析与断言 ──────────────────────────────────────────────────────

// chatChunks 解析 chat.completion.chunk 帧（跳过 [DONE] 与空行）。
func chatChunks(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var chunks []map[string]any
	for _, line := range strings.Split(raw, "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			t.Fatalf("chunk 不是合法 JSON: %v (%q)", err, line)
		}
		chunks = append(chunks, chunk)
	}
	return chunks
}

// chatDelta 取 chunk 的 delta；usage chunk 的 choices 为空，返回 nil。
func chatDelta(chunk map[string]any) map[string]any {
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return nil
	}
	choice, _ := choices[0].(map[string]any)
	delta, _ := choice["delta"].(map[string]any)
	return delta
}

// chatFinish 取 chunk 的 finish_reason（未携带时为空串）。
func chatFinish(chunk map[string]any) string {
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return ""
	}
	choice, _ := choices[0].(map[string]any)
	reason, _ := choice["finish_reason"].(string)
	return reason
}

// chatLastFinish 取最后一个非空 finish_reason。
func chatLastFinish(t *testing.T, raw string) string {
	t.Helper()
	finish := ""
	for _, chunk := range chatChunks(t, raw) {
		if reason := chatFinish(chunk); reason != "" {
			finish = reason
		}
	}
	return finish
}

// chatToolCall 是一路 tool_calls 的归并结果（id / name 只出现在起始 chunk）。
type chatToolCall struct {
	index     float64
	id        string
	name      string
	arguments string
}

// chatToolCalls 按 tool_calls.index 归并参数增量，顺序为首次出现顺序。
func chatToolCalls(t *testing.T, raw string) []*chatToolCall {
	t.Helper()
	var calls []*chatToolCall
	byIndex := map[float64]*chatToolCall{}
	for _, chunk := range chatChunks(t, raw) {
		rawCalls, _ := chatDelta(chunk)["tool_calls"].([]any)
		for _, rawOne := range rawCalls {
			one, _ := rawOne.(map[string]any)
			index, _ := one["index"].(float64)
			call := byIndex[index]
			if call == nil {
				call = &chatToolCall{index: index}
				byIndex[index] = call
				calls = append(calls, call)
			}
			if id, ok := one["id"].(string); ok {
				call.id = id
			}
			fn, _ := one["function"].(map[string]any)
			if name, ok := fn["name"].(string); ok {
				call.name = name
			}
			call.arguments += stringOf(fn["arguments"])
		}
	}
	return calls
}

// assertChatProjection 断言 chat 流只投影出 want 正文：没有伪造的 tool_calls、
// 以 stop 正常收尾，并以 [DONE] 结束。
func assertChatProjection(t *testing.T, raw, want string) {
	t.Helper()
	text, calls := "", 0
	for _, chunk := range chatChunks(t, raw) {
		delta := chatDelta(chunk)
		if s, ok := delta["content"].(string); ok {
			text += s
		}
		if rawCalls, ok := delta["tool_calls"].([]any); ok {
			calls += len(rawCalls)
		}
	}
	finish := chatLastFinish(t, raw)
	if text != want || finish != "stop" || calls != 0 || !strings.Contains(raw, "data: [DONE]") {
		t.Fatalf("chat 投影不符: text=%q finish=%q calls=%d raw=%s", text, finish, calls, raw)
	}
}

// assertResponsesProjection 断言 Responses 流只投影出 want 正文：服务端执行块不占用
// 输出项目索引、不产生 function_call 事件，最终以 response.completed 收尾。
func assertResponsesProjection(t *testing.T, raw, want string, wantItems int) {
	t.Helper()
	events := responseEvents(t, raw)
	items, text := 0, ""
	for _, event := range events {
		switch event["type"] {
		case "response.output_item.added":
			item, _ := event["item"].(map[string]any)
			if item["type"] != "message" || event["output_index"] != float64(items) {
				t.Fatalf("服务端执行块不得占用输出项目: %v", event)
			}
			items++
		case "response.output_text.delta":
			text += stringOf(event["delta"])
		case "response.function_call_arguments.delta", "response.function_call_arguments.done":
			t.Fatalf("MCP / 内建块不得投影成 function_call: %v", event)
		}
	}
	last := events[len(events)-1]
	response, _ := last["response"].(map[string]any)
	output, _ := response["output"].([]any)
	if last["type"] != "response.completed" || items != wantItems || len(output) != wantItems || text != want {
		t.Fatalf("responses 投影不符: items=%d text=%q last=%v", items, text, last)
	}
}

// ── 用例 ────────────────────────────────────────────────────────────────────

// MCP / 内建服务端块的起始块与参数增量都不能让两种投影中断：块本身不投影，
// 紧跟其后的正文照常交付，且不得计入账号断流。
func TestServerExecutedBlocksDoNotInterruptStreams(t *testing.T) {
	for _, tc := range []struct{ name, block string }{
		{"mcp_tool_use", `{"type":"mcp_tool_use","id":"mcp_1","name":"search_web","server_name":"web","input":{}}`},
		{"server_tool_use", `{"type":"server_tool_use","id":"srv_1","name":"web_search","input":{}}`},
		{"mcp_tool_result", `{"type":"mcp_tool_result","tool_use_id":"mcp_1","is_error":false,"content":[{"type":"text","text":"晴 20 度"}]}`},
		{"web_search_tool_result", `{"type":"web_search_tool_result","tool_use_id":"srv_1","content":[]}`},
	} {
		stream := streamHead +
			blockStartFrame(0, tc.block) +
			jsonDeltaFrame(0, `{"query":"杭州天气"}`) +
			blockStopFrame(0) +
			blockStartFrame(1, `{"type":"text","text":""}`) +
			textDeltaFrame(1, "答案") +
			blockStopFrame(1) +
			streamNormalEnd
		for _, api := range []string{"chat", "responses"} {
			t.Run(tc.name+"/"+api, func(t *testing.T) {
				f := newFixture(t)
				f.setResponder(func(int) (int, string, string) {
					return http.StatusOK, "text/event-stream", stream
				})
				body := compatRequest(api)
				body["stream"] = true
				status, raw := postCompat(t, f, api, body)
				if status != http.StatusOK {
					t.Fatalf("请求失败: %d %s", status, raw)
				}
				if strings.Contains(raw, "overloaded_error") || strings.Contains(raw, "未声明") {
					t.Fatalf("服务端执行块不得中断整条流: %s", raw)
				}
				if api == "chat" {
					assertChatProjection(t, raw, "答案")
				} else {
					assertResponsesProjection(t, raw, "答案", 1)
				}
				accounts := f.st.ListAccounts(model.ProviderZai)
				if len(accounts) != 1 || accounts[0].StreamTruncateCount != 0 {
					t.Fatalf("投影兼容不得计为账号断流: %v", accounts)
				}
			})
		}
	}
}

// 服务端执行块占掉上游 index 0 时，紧随其后的普通函数工具仍须投影成索引 0 的
// tool_call / function_call，参数增量照常拼接：修复不能顺手吞掉真正的函数工具。
func TestFunctionToolAfterServerExecutedBlock(t *testing.T) {
	stream := streamHead +
		blockStartFrame(0, `{"type":"mcp_tool_use","id":"mcp_1","name":"search_web","input":{}}`) +
		jsonDeltaFrame(0, `{"query":"杭州天气"}`) +
		blockStopFrame(0) +
		blockStartFrame(1, `{"type":"tool_use","id":"call_a","name":"get_weather","input":{}}`) +
		jsonDeltaFrame(1, `{"city":`) +
		jsonDeltaFrame(1, `"杭州"}`) +
		blockStopFrame(1) +
		streamToolEnd
	for _, api := range []string{"chat", "responses"} {
		t.Run(api, func(t *testing.T) {
			f := newFixture(t)
			f.setResponder(func(int) (int, string, string) {
				return http.StatusOK, "text/event-stream", stream
			})
			body := compatRequest(api)
			body["stream"] = true
			status, raw := postCompat(t, f, api, body)
			if status != http.StatusOK || strings.Contains(raw, "overloaded_error") {
				t.Fatalf("请求失败: %d %s", status, raw)
			}
			if api == "chat" {
				calls := chatToolCalls(t, raw)
				if len(calls) != 1 || calls[0].index != 0 || calls[0].id != "call_a" ||
					calls[0].name != "get_weather" || calls[0].arguments != `{"city":"杭州"}` {
					t.Fatalf("函数工具投影不符: %+v", calls)
				}
				if finish := chatLastFinish(t, raw); finish != "tool_calls" {
					t.Fatalf("终止原因不符: %q", finish)
				}
				return
			}
			events := responseEvents(t, raw)
			added, arguments, callID, name := 0, "", "", ""
			for _, event := range events {
				switch event["type"] {
				case "response.output_item.added":
					item, _ := event["item"].(map[string]any)
					if item["type"] != "function_call" || event["output_index"] != float64(0) {
						t.Fatalf("函数工具项目不符: %v", event)
					}
					callID, name = stringOf(item["call_id"]), stringOf(item["name"])
					added++
				case "response.function_call_arguments.delta":
					arguments += stringOf(event["delta"])
				}
			}
			if added != 1 || callID != "call_a" || name != "get_weather" || arguments != `{"city":"杭州"}` {
				t.Fatalf("function_call 参数拼接不符: added=%d call=%q name=%q args=%q", added, callID, name, arguments)
			}
		})
	}
}

// 孤儿结果块（上游只给 mcp_tool_result、没有对应 mcp_tool_use）不得 panic、不得吞掉
// 前后正文，也不得被当成客户端工具。
func TestOrphanToolResultBlocksTolerated(t *testing.T) {
	stream := streamHead +
		blockStartFrame(0, `{"type":"text","text":""}`) +
		textDeltaFrame(0, "前") +
		blockStopFrame(0) +
		blockStartFrame(1, `{"type":"mcp_tool_result","tool_use_id":"missing_use","is_error":true,"content":[{"type":"text","text":"工具报错"}]}`) +
		jsonDeltaFrame(1, `{"ignored":true}`) + // 结果块若被补上参数增量，同样只能丢弃
		blockStopFrame(1) +
		blockStartFrame(2, `{"type":"text","text":""}`) +
		textDeltaFrame(2, "后") +
		blockStopFrame(2) +
		streamNormalEnd
	for _, api := range []string{"chat", "responses"} {
		t.Run(api, func(t *testing.T) {
			f := newFixture(t)
			f.setResponder(func(int) (int, string, string) {
				return http.StatusOK, "text/event-stream", stream
			})
			body := compatRequest(api)
			body["stream"] = true
			status, raw := postCompat(t, f, api, body)
			if status != http.StatusOK {
				t.Fatalf("请求失败: %d %s", status, raw)
			}
			if api == "chat" {
				assertChatProjection(t, raw, "前后")
				return
			}
			assertResponsesProjection(t, raw, "前后", 2)
		})
	}
}

// 边界：只放宽已登记的服务端执行块。真正未声明的块（没有任何 content_block_start）
// 仍须显式报错、不得伪装成正常结束——否则半截参数会被当成完整工具调用交付。
func TestUndeclaredToolBlockStillFails(t *testing.T) {
	stream := streamHead + jsonDeltaFrame(3, `{"city":"杭州"}`) + streamNormalEnd
	for _, api := range []string{"chat", "responses"} {
		t.Run(api, func(t *testing.T) {
			f := newFixture(t)
			f.setResponder(func(int) (int, string, string) {
				return http.StatusOK, "text/event-stream", stream
			})
			body := compatRequest(api)
			body["stream"] = true
			status, raw := postCompat(t, f, api, body)
			if status != http.StatusOK {
				t.Fatalf("SSE 响应头已提交，状态码应为 200: %d %s", status, raw)
			}
			if api == "chat" {
				if !strings.Contains(raw, "overloaded_error") || strings.Contains(raw, "data: [DONE]") {
					t.Fatalf("未声明的工具块必须显式报错且不能伪装收尾: %s", raw)
				}
				return
			}
			events := responseEvents(t, raw)
			if last := events[len(events)-1]; last["type"] != "response.failed" {
				t.Fatalf("未声明的工具块必须以 response.failed 终止: %v", last)
			}
		})
	}
}

// 缺 index 的畸形服务端块不能登记成 index 0：否则紧随其后的函数工具参数会被当成
// 占位块增量丢弃，客户端拿到一个空参数的函数调用。
func TestServerExecutedBlockWithoutIndexKeepsFunctionTool(t *testing.T) {
	stream := streamHead +
		"event: content_block_start\ndata: {\"content_block\":{\"type\":\"mcp_tool_use\",\"id\":\"mcp_x\",\"name\":\"search_web\"}}\n\n" +
		blockStartFrame(0, `{"type":"tool_use","id":"call_a","name":"get_weather","input":{}}`) +
		jsonDeltaFrame(0, `{"city":`) +
		jsonDeltaFrame(0, `"杭州"}`) +
		blockStopFrame(0) +
		streamToolEnd
	for _, api := range []string{"chat", "responses"} {
		t.Run(api, func(t *testing.T) {
			f := newFixture(t)
			f.setResponder(func(int) (int, string, string) {
				return http.StatusOK, "text/event-stream", stream
			})
			body := compatRequest(api)
			body["stream"] = true
			status, raw := postCompat(t, f, api, body)
			if status != http.StatusOK || strings.Contains(raw, "overloaded_error") {
				t.Fatalf("请求失败: %d %s", status, raw)
			}
			// 参数必须完整留在函数工具上，而不是被缺 index 的占位块吞掉。
			if api == "chat" {
				calls := chatToolCalls(t, raw)
				if len(calls) != 1 || calls[0].arguments != `{"city":"杭州"}` {
					t.Fatalf("畸形服务端块吞掉了函数工具参数: %+v", calls)
				}
				return
			}
			arguments := ""
			for _, event := range responseEvents(t, raw) {
				if event["type"] == "response.function_call_arguments.delta" {
					arguments += stringOf(event["delta"])
				}
			}
			if arguments != `{"city":"杭州"}` {
				t.Fatalf("畸形服务端块吞掉了函数工具参数: %q", arguments)
			}
		})
	}
}
