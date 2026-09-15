package openai

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// 所有兼容性回归从公开 HTTP 端点进入，上游只替换为本地 httptest 服务。
func compatRequest(api string) map[string]any {
	body := map[string]any{"model": "GLM-5.3"}
	function := map[string]any{
		"name": "get_weather", "description": "查询天气",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{}},
	}
	if api == "chat" {
		body["messages"] = []any{map[string]any{"role": "user", "content": "查询天气"}}
		body["tools"] = []any{map[string]any{"type": "function", "function": function}}
	} else {
		body["input"] = "查询天气"
		function["type"] = "function"
		body["tools"] = []any{function}
	}
	return body
}

func TestUnsupportedToolControlsRejectedBeforeUpstream(t *testing.T) {
	for _, api := range []string{"chat", "responses"} {
		for _, tc := range []struct {
			name  string
			field string
			edit  func(map[string]any, map[string]any)
		}{
			{"strict", "strict", func(_ map[string]any, fn map[string]any) { fn["strict"] = true }},
			{"bad_strict", "strict", func(_ map[string]any, fn map[string]any) { fn["strict"] = "true" }},
			{"bad_schema", "parameters", func(_ map[string]any, fn map[string]any) { fn["parameters"] = []any{} }},
			{"bad_tools", "tools", func(b, _ map[string]any) { b["tools"] = "get_weather" }},
			{"unknown_choice", "tool_choice", func(b, _ map[string]any) { b["tool_choice"] = "sometimes" }},
			{"bad_parallel", "parallel_tool_calls", func(b, _ map[string]any) { b["parallel_tool_calls"] = "false" }},
			{"required_without_tools", "tools", func(b, _ map[string]any) { delete(b, "tools"); b["tool_choice"] = "required" }},
			{"duplicate_names", "重复", func(b, _ map[string]any) { tools := b["tools"].([]any); b["tools"] = append(tools, tools[0]) }},
			{"builtin_tool", "function", func(b, _ map[string]any) { b["tools"] = []any{map[string]any{"type": "web_search"}} }},
		} {
			t.Run(api+"/"+tc.name, func(t *testing.T) {
				f := newFixture(t)
				f.setResponder(replyText)
				body := compatRequest(api)
				fn := body["tools"].([]any)[0].(map[string]any)
				if api == "chat" {
					fn = fn["function"].(map[string]any)
				}
				tc.edit(body, fn)
				status, raw := postCompat(t, f, api, body)
				if status != http.StatusBadRequest || !strings.Contains(raw, tc.field) || !strings.Contains(raw, "invalid_request_error") {
					t.Fatalf("应在入口明确拒绝 %s: %d %s", tc.field, status, raw)
				}
				f.mu.Lock()
				calls := len(f.calls)
				f.mu.Unlock()
				if calls != 0 {
					t.Fatalf("无效请求不应消耗上游调用: %d", calls)
				}
			})
		}
	}
}

func postCompat(t *testing.T, f *fixture, api string, body map[string]any) (int, string) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if api == "chat" {
		return f.postChat(t, "sk-test", string(raw))
	}
	return post(f, t, "sk-test", string(raw))
}

func replyText(int) (int, string, string) {
	return http.StatusOK, "application/json", `{"id":"msg_fixture","model":"GLM-5.3","type":"message","role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"回答"}],"usage":{"input_tokens":3,"output_tokens":2}}`
}

func TestToolControlsReachUpstream(t *testing.T) {
	for _, api := range []string{"chat", "responses"} {
		for _, tc := range []struct {
			name     string
			choice   string
			parallel bool
			want     string
		}{
			{"auto_serial", "auto", false, `{"type":"auto","disable_parallel_tool_use":true}`},
			{"required_serial", "required", false, `{"type":"any","disable_parallel_tool_use":true}`},
			{"named_serial", "named", false, `{"type":"tool","name":"get_weather","disable_parallel_tool_use":true}`},
			{"none", "none", false, `{"type":"none"}`},
			{"default_serial", "", false, `{"type":"auto","disable_parallel_tool_use":true}`},
			{"auto_parallel", "auto", true, `{"type":"auto","disable_parallel_tool_use":false}`},
		} {
			t.Run(api+"/"+tc.name, func(t *testing.T) {
				f := newFixture(t)
				f.setResponder(replyText)
				body := compatRequest(api)
				body["parallel_tool_calls"] = tc.parallel
				if tc.choice == "named" {
					choice := map[string]any{"type": "function"}
					if api == "chat" {
						choice["function"] = map[string]any{"name": "get_weather"}
					} else {
						choice["name"] = "get_weather"
					}
					body["tool_choice"] = choice
				} else if tc.choice != "" {
					body["tool_choice"] = tc.choice
				}
				status, raw := postCompat(t, f, api, body)
				if status != http.StatusOK {
					t.Fatalf("请求失败: %d %s", status, raw)
				}
				up := f.lastUpstream().Body
				if !reflect.DeepEqual(up["tool_choice"], mustJSON(t, tc.want)) {
					t.Fatalf("工具控制未正确到达上游: got=%v want=%s", up["tool_choice"], tc.want)
				}
				if _, leaked := up["parallel_tool_calls"]; leaked {
					t.Fatal("OpenAI 参数应转换而非直接传给 Anthropic")
				}
			})
		}
	}
}

func TestThinkingControlsOverHTTP(t *testing.T) {
	for _, modelName := range []string{"GLM-5.3", "glm-5.3-flash"} {
		for _, api := range []string{"chat", "responses"} {
			for _, tc := range []struct {
				name     string
				effort   any
				limit    float64
				explicit any
				want     string
			}{
				{"minimal_alias", "minimal", 8192, nil, "low"},
				{"low", "low", 8192, nil, "low"},
				{"medium_alias", "medium", 8192, nil, "high"},
				{"high", "high", 8192, nil, "high"},
				{"max", "max", 8192, nil, "max"},
				{"xhigh_alias", "xhigh", 8192, nil, "max"},
				{"none_alias", "none", 8192, nil, "low"},
				{"small_output_limit", "max", 1024, nil, "max"},
				{"explicit_disabled", "high", 8192, map[string]any{"type": "disabled"}, ""},
				{"explicit_budget", "max", 8192, map[string]any{"type": "enabled", "budget_tokens": float64(3000)}, "max"},
				{"pi_switch", "max", 8192, map[string]any{"type": "enabled", "clear_thinking": false}, "max"},
				{"unknown", "extreme", 8192, nil, ""},
				{"wrong_type", true, 8192, nil, ""},
				{"invalid_limit", "max", 0, nil, ""},
				{"invalid_explicit", "low", 8192, map[string]any{"type": "sometimes"}, ""},
				{"invalid_budget", "low", 8192, map[string]any{"type": "enabled", "budget_tokens": float64(8192)}, ""},
			} {
				t.Run(modelName+"/"+api+"/"+tc.name, func(t *testing.T) {
					f := newFixture(t)
					f.setResponder(replyText)
					body := compatRequest(api)
					body["model"] = modelName
					if api == "chat" {
						body["reasoning_effort"], body["max_tokens"] = tc.effort, tc.limit
					} else {
						body["reasoning"] = map[string]any{"effort": tc.effort}
						body["max_output_tokens"] = tc.limit
					}
					if tc.explicit != nil {
						body["thinking"] = tc.explicit
					}
					status, raw := postCompat(t, f, api, body)
					if tc.want == "" {
						if status != http.StatusBadRequest || !strings.Contains(raw, "invalid_request_error") {
							t.Fatalf("无效思考参数应明确返回 400: %d %s", status, raw)
						}
						return
					}
					if status != http.StatusOK {
						t.Fatalf("思考请求失败: %d %s", status, raw)
					}
					up := f.lastUpstream().Body
					config, _ := up["output_config"].(map[string]any)
					if config["effort"] != tc.want {
						t.Fatalf("原生 effort 转换不符: got=%v want=%s", config, tc.want)
					}
					if up["max_tokens"] != tc.limit {
						t.Fatal("不得擅自提高调用方的输出上限")
					}
					explicit, _ := tc.explicit.(map[string]any)
					if explicit["budget_tokens"] != nil {
						if !reflect.DeepEqual(up["thinking"], explicit) {
							t.Fatalf("显式预算必须独立于 effort 保留: %v", up["thinking"])
						}
					} else if _, invented := up["thinking"]; invented {
						t.Fatalf("不能用虚构的固定 thinking 预算代替原生 effort: %v", up["thinking"])
					}
				})
			}
		}
	}
}

func TestFunctionCallsRoundTripOverHTTP(t *testing.T) {
	for _, api := range []string{"chat", "responses"} {
		t.Run(api, func(t *testing.T) {
			f := newFixture(t)
			f.setResponder(func(call int) (int, string, string) {
				if call == 1 {
					return http.StatusOK, "application/json", `{"id":"tool_round","model":"GLM-5.3","stop_reason":"tool_use","usage":{"input_tokens":4,"output_tokens":7},"content":[{"type":"thinking","thinking":"查询两个城市","signature":"private-fixture-signature"},{"type":"tool_use","id":"call_a","name":"get_weather","input":{"city":"杭州"}},{"type":"tool_use","id":"call_b","name":"get_weather","input":{"city":"上海"}}]}`
				}
				return replyText(call)
			})
			body := compatRequest(api)
			status, raw := postCompat(t, f, api, body)
			if status != http.StatusOK {
				t.Fatalf("首轮请求失败: %d %s", status, raw)
			}
			var first map[string]any
			if err := json.Unmarshal([]byte(raw), &first); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(raw, "private-fixture-signature") {
				t.Fatal("思考签名不得暴露")
			}
			results := map[string]string{"call_a": "晴 20 度", "call_b": "阴 18 度"}
			if api == "chat" {
				choice := first["choices"].([]any)[0].(map[string]any)
				message := choice["message"].(map[string]any)
				if choice["finish_reason"] != "tool_calls" || message["reasoning_content"] != "查询两个城市" {
					t.Fatalf("工具或思考输出缺失: %v", choice)
				}
				calls := message["tool_calls"].([]any)
				if len(calls) != 2 {
					t.Fatalf("应返回两个工具调用: %v", calls)
				}
				messages := append(body["messages"].([]any), message)
				for _, rawCall := range calls {
					call := rawCall.(map[string]any)
					id := call["id"].(string)
					messages = append(messages, map[string]any{"role": "tool", "tool_call_id": id, "content": results[id]})
				}
				body["messages"] = messages
			} else {
				output := first["output"].([]any)
				if len(output) != 3 || output[0].(map[string]any)["type"] != "reasoning" {
					t.Fatalf("纯工具轮次应为 reasoning + 两个 function_call，不能插入空 message: %v", output)
				}
				input := []any{map[string]any{"type": "message", "role": "user", "content": "查询天气"}}
				input = append(input, output...)
				for _, rawCall := range output[1:] {
					call := rawCall.(map[string]any)
					id := call["call_id"].(string)
					input = append(input, map[string]any{"type": "function_call_output", "call_id": id, "output": results[id]})
				}
				body["input"] = input
			}
			status, raw = postCompat(t, f, api, body)
			if status != http.StatusOK || !strings.Contains(raw, "回答") {
				t.Fatalf("工具回传后的回答失败: %d %s", status, raw)
			}
			messages := f.lastUpstream().Body["messages"].([]any)
			if len(messages) != 3 {
				t.Fatalf("上游应是 user → assistant(两个工具) → user(两个结果): %v", messages)
			}
			calls := messages[1].(map[string]any)["content"].([]any)
			returned := messages[2].(map[string]any)["content"].([]any)
			if len(calls) != 2 || len(returned) != 2 {
				t.Fatalf("并行工具及结果必须完整保留: %v", messages)
			}
			for i, id := range []string{"call_a", "call_b"} {
				call, result := calls[i].(map[string]any), returned[i].(map[string]any)
				if call["id"] != id || result["tool_use_id"] != id {
					t.Fatalf("工具调用与结果 ID 不匹配: %v", messages)
				}
			}
		})
	}
}

func TestPiZaiThinkingSwitchOverHTTP(t *testing.T) {
	for _, tc := range []struct {
		name, effort string
	}{
		{"with_effort", "high"},
		{"switch_only", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.setResponder(replyText)
			body := compatRequest("chat")
			body["max_tokens"] = 16384
			body["thinking"] = map[string]any{"type": "enabled", "clear_thinking": false}
			if tc.effort != "" {
				body["reasoning_effort"] = tc.effort
			}
			status, raw := postCompat(t, f, "chat", body)
			if status != http.StatusOK {
				t.Fatalf("Pi ZAI 开关式请求应转换后接收: %d %s", status, raw)
			}
			up := f.lastUpstream().Body
			if _, invented := up["thinking"]; invented {
				t.Fatal("强制思考模型的 enabled 开关不应被换成猜测的 token 预算")
			}
			config, _ := up["output_config"].(map[string]any)
			if tc.effort != "" && config["effort"] != tc.effort {
				t.Fatalf("Pi 的原生 effort 未转发: %v", config)
			}
			if tc.effort == "" && config != nil {
				t.Fatal("只开思考开关时应保留上游默认 max，不另造档位")
			}
		})
	}
}

// GLM-5.3-Flash 官方推荐 max；不得在网关内将该合法档位提前拒绝。
func TestGLM53FlashMaxEffortReachesUpstream(t *testing.T) {
	f := newFixture(t)
	f.setResponder(replyText)
	body := map[string]any{
		"model": "glm-5.3-flash", "reasoning_effort": "max", "max_tokens": 8192,
		"messages": []any{map[string]any{"role": "user", "content": "你好"}},
	}
	status, raw := postCompat(t, f, "chat", body)
	if status != http.StatusOK {
		t.Fatalf("max 不应被本地校验拒绝: %d %s", status, raw)
	}
	if f.lastUpstream().Body["model"] != "glm-5.3-flash" {
		t.Fatal("不得通过切换模型规避 max 支持")
	}
	config, _ := f.lastUpstream().Body["output_config"].(map[string]any)
	if config["effort"] != "max" {
		t.Fatalf("必须传递 max 意图，不能只换成更大的 token 预算: %v", config)
	}
}
