package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"zcode2api/internal/captcha"
	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

func newClientContextFixture(t *testing.T, jwt bool) *fixture {
	t.Helper()
	f := newFixture(t)
	if jwt {
		if ok, err := f.st.RemoveAccount(model.ProviderZai, "e2e-acc"); err != nil || !ok {
			t.Fatalf("替换合成账号失败: %v", err)
		}
		if _, err := f.st.AddAccount(model.ProviderZai, "client-context", "header.payload.sig"); err != nil {
			t.Fatal(err)
		}
		f.cm.SetConfigProvider(func(context.Context) (captcha.Config, error) {
			return captcha.Config{Enabled: true, SkipModelRequest: true}, nil
		})
	}
	f.setResponder(replyText)
	return f
}

func clientContextRequest(t *testing.T, api string) map[string]any {
	t.Helper()
	body := compatRequest(api)
	if api == "chat" {
		body["messages"] = mustJSON(t, `[
			{"role":"system","content":"客户端基础指令"},
			{"role":"developer","content":"客户端补充指令"},
			{"role":"user","content":"第一轮"},
			{"role":"assistant","content":"第一轮回答"},
			{"role":"developer","content":"仅使用客户端提供的 get_weather 工具"},
			{"role":"user","content":"第二轮"}
		]`)
	} else {
		body["instructions"] = "客户端基础指令"
		body["input"] = mustJSON(t, `[
			{"type":"reasoning","summary":[]},
			{"type":"message","role":"developer","content":"客户端补充指令"},
			{"type":"message","role":"user","content":"第一轮"},
			{"type":"message","role":"assistant","content":"第一轮回答"},
			{"type":"message","role":"system","content":"仅使用客户端提供的 get_weather 工具"},
			{"type":"message","role":"user","content":"第二轮"}
		]`)
	}
	return body
}

func assertClientContextRoles(t *testing.T, body map[string]any, want []string) []any {
	t.Helper()
	msgs, ok := body["messages"].([]any)
	if !ok {
		t.Fatalf("缺少消息序列: %v", body["messages"])
	}
	roles := make([]string, 0, len(msgs))
	for _, raw := range msgs {
		msg, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("消息形态错误: %T", raw)
		}
		roles = append(roles, stringOf(msg["role"]))
	}
	if !reflect.DeepEqual(roles, want) {
		t.Fatalf("中途指令和历史位置不得丢失: got=%v want=%v", roles, want)
	}
	return msgs
}

func TestClientContextAcrossCompatibleAPIs(t *testing.T) {
	for _, api := range []string{"chat", "responses"} {
		for _, jwt := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/jwt=%t/stream=%t", api, jwt, stream), func(t *testing.T) {
					f := newClientContextFixture(t, jwt)
					if stream {
						f.setResponder(func(int) (int, string, string) {
							return http.StatusOK, "text/event-stream",
								"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_context\",\"model\":\"GLM-5.3\",\"usage\":{\"input_tokens\":4}}}\n\n" +
									"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"回答\"}}\n\n" +
									"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n" +
									"data: {\"type\":\"message_stop\"}\n\n"
						})
					}
					body := clientContextRequest(t, api)
					body["stream"] = stream
					status, raw := postCompat(t, f, api, body)
					if status != http.StatusOK || !strings.Contains(raw, "回答") {
						t.Fatalf("客户端交付协议不能变化: %d %s", status, raw)
					}
					if stream && (api == "chat" && !strings.Contains(raw, "data: [DONE]") || api == "responses" && !strings.Contains(raw, "response.completed")) {
						t.Fatalf("流式终态不完整: %s", raw)
					}
					up := f.lastUpstream()
					wantSystem := mustJSON(t, `[{"type":"text","text":"客户端基础指令"},{"type":"text","text":"客户端补充指令"}]`)
					if !reflect.DeepEqual(up.Body["system"], wantSystem) {
						t.Fatalf("仅初始指令进入顶层，不追加另一份环境: %v", up.Body["system"])
					}
					msgs := assertClientContextRoles(t, up.Body, []string{"user", "assistant", "system", "user"})
					if msgs[2].(map[string]any)["content"] != "仅使用客户端提供的 get_weather 工具" {
						t.Fatalf("中途文本应按 Anthropic 兼容形态保留: %v", msgs[2])
					}
					if up.Header.Get("Anthropic-Beta") != "mid-conversation-system-2026-04-07" {
						t.Fatalf("功能声明必须与实际消息一致: %v", up.Header)
					}
					tools, _ := up.Body["tools"].([]any)
					if len(tools) != 1 || tools[0].(map[string]any)["name"] != "get_weather" {
						t.Fatalf("只能发送本地客户端声明的工具: %v", tools)
					}
					if up.Body["max_tokens"] != float64(8192) || up.Body["thinking"] != nil || up.Body["output_config"] != nil {
						t.Fatal("上下文兼容不得自动提高上限或生成思考参数")
					}
				})
			}
		}
	}
}

func TestEmptyClientInstructionsAreNotReplaced(t *testing.T) {
	for _, api := range []string{"chat", "responses"} {
		t.Run(api, func(t *testing.T) {
			f := newClientContextFixture(t, true)
			body := compatRequest(api)
			if api == "chat" {
				body["messages"] = mustJSON(t, `[{"role":"system","content":""},{"role":"user","content":"问题"}]`)
			} else {
				body["instructions"] = ""
			}
			status, raw := postCompat(t, f, api, body)
			if status != http.StatusOK {
				t.Fatalf("请求失败: %d %s", status, raw)
			}
			up := f.lastUpstream()
			blocks, ok := up.Body["system"].([]any)
			if !ok || len(blocks) != 0 {
				t.Fatalf("显式空指令不是缺省指令: %v", up.Body["system"])
			}
			if up.Header.Get("Anthropic-Beta") != "" {
				t.Fatal("不能仅因为有初始空指令就启用 MCS")
			}
		})
	}
}

func TestClientInstructionLegacyConversion(t *testing.T) {
	old := config.PreserveClientContext
	config.PreserveClientContext = false
	t.Cleanup(func() { config.PreserveClientContext = old })
	for _, api := range []string{"chat", "responses"} {
		t.Run(api, func(t *testing.T) {
			f := newClientContextFixture(t, false)
			body := clientContextRequest(t, api)
			status, raw := postCompat(t, f, api, body)
			if status != http.StatusOK {
				t.Fatalf("回退请求失败: %d %s", status, raw)
			}
			up := f.lastUpstream()
			assertClientContextRoles(t, up.Body, []string{"user", "assistant", "user"})
			want := mustJSON(t, `[{"type":"text","text":"客户端基础指令"},{"type":"text","text":"客户端补充指令"},{"type":"text","text":"仅使用客户端提供的 get_weather 工具"}]`)
			if !reflect.DeepEqual(up.Body["system"], want) || up.Header.Get("Anthropic-Beta") != "" {
				t.Fatal("关闭开关应恢复旧版指令统一提升与无自动 beta 行为")
			}
		})
	}
}

func TestInstructionConversionDoesNotMutateCaller(t *testing.T) {
	for _, api := range []string{"chat", "responses"} {
		t.Run(api, func(t *testing.T) {
			body := clientContextRequest(t, api)
			before, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			convert := ConvertRequest
			if api == "responses" {
				convert = ConvertResponsesRequest
			}
			first, err := convert(body)
			if err != nil {
				t.Fatal(err)
			}
			assertClientContextRoles(t, first, []string{"user", "assistant", "system", "user"})
			second, err := convert(body)
			if err != nil || !reflect.DeepEqual(first, second) {
				t.Fatal("对同一调用者输入重复转换必须稳定")
			}
			after, err := json.Marshal(body)
			if err != nil || string(before) != string(after) {
				t.Fatal("不得改写客户端的嵌套消息对象")
			}
		})
	}
}

func TestExplicitThinkingSwitchReachesUpstream(t *testing.T) {
	for _, api := range []string{"chat", "responses"} {
		t.Run(api, func(t *testing.T) {
			f := newClientContextFixture(t, true)
			body := compatRequest(api)
			body["thinking"] = map[string]any{"type": "enabled", "clear_thinking": false}
			if api == "chat" {
				body["max_tokens"] = float64(1024)
				body["reasoning_effort"] = "low"
			} else {
				body["max_output_tokens"] = float64(1024)
				body["reasoning"] = map[string]any{"effort": "low"}
			}
			status, raw := postCompat(t, f, api, body)
			if status != http.StatusOK {
				t.Fatalf("请求失败: %d %s", status, raw)
			}
			up := f.lastUpstream().Body
			if !reflect.DeepEqual(up["thinking"], map[string]any{"type": "enabled"}) {
				t.Fatalf("显式 enabled 必须保留，不能猜测预算: %v", up["thinking"])
			}
			if up["max_tokens"] != float64(1024) || !reflect.DeepEqual(up["output_config"], map[string]any{"effort": "low"}) {
				t.Fatal("不能覆盖客户端的输出上限和思考档位")
			}
		})
	}
}
