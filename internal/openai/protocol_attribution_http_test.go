package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"zcode2api/internal/asyncpool"
	"zcode2api/internal/auth"
	"zcode2api/internal/config"
	"zcode2api/internal/upstream"
)

var attributionPaths = []string{"/v1/messages", "/v1/chat/completions", "/v1/responses", "/async/v1/messages"}

// 四入口共用真实 HTTP 服务、JWT 凭据、完整 SSE；不以私有构造器的成功代替接线验收。
func newAttributionProtocolFixture(t *testing.T) *fixture {
	t.Helper()
	f := newClientContextFixture(t, true)
	oldEnabled, oldPreserve := config.AsyncEnabled, config.PreserveClientContext
	oldSource, oldInject := config.ZcodeClientSourceTitle, config.UpstreamInjectEnvironment
	config.AsyncEnabled, config.PreserveClientContext = true, true
	config.ZcodeClientSourceTitle, config.UpstreamInjectEnvironment = "electron", false
	p := asyncpool.NewPool(f.st, auth.New(f.st), f.cm)
	p.Register(f.srv.Config.Handler.(*http.ServeMux))
	t.Cleanup(func() {
		p.Close()
		config.AsyncEnabled, config.PreserveClientContext = oldEnabled, oldPreserve
		config.ZcodeClientSourceTitle, config.UpstreamInjectEnvironment = oldSource, oldInject
	})
	f.setResponder(func(int) (int, string, string) { return http.StatusOK, "text/event-stream", accountContextStream })
	return f
}

func attributionProtocolBody(path, modelName, systemKind string) map[string]any {
	text := "合成的会话与协议请求"
	body := map[string]any{"model": modelName, "stream": true}
	messages := []any{map[string]any{"role": "user", "content": text}}
	if path == "/v1/responses" {
		body["input"], body["max_output_tokens"] = text, float64(64)
		body["tools"] = []any{map[string]any{"type": "function", "name": "caller_tool", "parameters": map[string]any{"type": "object"}}}
	} else {
		body["messages"], body["max_tokens"] = messages, float64(64)
		if path == "/v1/chat/completions" {
			body["tools"] = []any{map[string]any{"type": "function", "function": map[string]any{"name": "caller_tool", "parameters": map[string]any{"type": "object"}}}}
		} else {
			body["tools"] = []any{map[string]any{"name": "caller_tool", "input_schema": map[string]any{"type": "object"}}}
		}
	}
	var system string
	switch systemKind {
	case "caller":
		system = upstream.OfficialSystemPrefix() + "\n调用者完整 GUI 上下文：仅使用客户端提供的工具。"
	case "custom":
		system = "客户端自有规则，保留 MCS/Gen UI 这些普通文字，不按关键词删除。"
	case "empty":
		system = ""
	case "mcs":
		messages = append(messages, map[string]any{"role": "system", "content": "中途指令原样保留"}, map[string]any{"role": "user", "content": "下一轮"})
		if path == "/v1/responses" {
			body["input"] = messages
		} else {
			body["messages"] = messages
		}
		return body
	default:
		return body
	}
	if path == "/v1/responses" {
		body["instructions"] = system
	} else if path == "/v1/chat/completions" {
		body["messages"] = append([]any{map[string]any{"role": "system", "content": system}}, messages...)
	} else {
		body["system"] = system
	}
	return body
}

func postAttributionProtocol(t *testing.T, f *fixture, path string, body map[string]any, headers http.Header) upstreamBody {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.srv.URL+path, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	for name, values := range headers {
		req.Header[name] = append([]string(nil), values...)
	}
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	f.mu.Lock()
	before := len(f.calls)
	f.mu.Unlock()
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil || resp.StatusCode != http.StatusOK || strings.Contains(string(raw), "event: error") {
		t.Fatalf("公开入口应完整交付：path=%s status=%d err=%v body=%s", path, resp.StatusCode, readErr, raw)
	}
	wantFinal := "message_stop"
	switch path {
	case "/v1/chat/completions":
		wantFinal = "[DONE]"
	case "/v1/responses":
		wantFinal = "response.completed"
	case "/async/v1/messages":
		wantFinal = "event: done"
	}
	if !strings.Contains(string(raw), wantFinal) {
		t.Fatalf("缺少入口终态 %s：%s", wantFinal, raw)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) != before+1 {
		t.Fatalf("每个合法入口须恰有一次真实出站，got=%d", len(f.calls)-before)
	}
	return f.calls[before]
}

func TestProtocolFallbackAndAttributionAcrossFourEntrypoints(t *testing.T) {
	for _, path := range attributionPaths {
		for _, modelName := range []string{"GLM-5.3", "glm-5.3-flash"} {
			for _, kind := range []string{"default", "caller", "empty", "mcs", "custom"} {
				t.Run(path+"/"+modelName+"/"+kind, func(t *testing.T) {
					f := newAttributionProtocolFixture(t)
					body := attributionProtocolBody(path, modelName, kind)
					body["metadata"] = map[string]any{"user_id": `{"session_id":"sess_body-conflict","device_id":"foreign-device"}`, "native_extension": "keep-native-only"}
					headers := http.Header{
						"x-session-id": {"sess_header-session"}, "X-Zcode-Trace-Id": {"trace.explicit"},
						"X-Query-Id": {"query_fixed-turn"}, "X-Zcode-Session-Type": {"subagent"},
						"x-zcode-query-source": {"compact"}, "X-Zcode-Unknown": {"do-not-forward"},
						"Cookie": {"private-cookie"}, "X-Request-Id": {"caller-request"},
					}
					call := postAttributionProtocol(t, f, path, body, headers)
					for key, want := range map[string]string{
						"X-Session-Id": "header-session", "X-Zcode-Trace-Id": "trace.explicit", "X-Query-Id": "fixed-turn",
						"X-Zcode-Session-Type": "subagent", "X-Zcode-Query-Source": "compact",
					} {
						if got := call.Header.Get(key); got != want {
							t.Fatalf("入站归因丢失/重写：%s=%q want=%q", key, got, want)
						}
					}
					if call.Header.Get("X-Request-Id") == "" || call.Header.Get("X-Request-Id") == "caller-request" || call.Header.Get("Cookie") != "" || call.Header.Get("X-Zcode-Unknown") != "" {
						t.Fatal("尝试 ID 必须刷新，秘密/未知归因不能上送")
					}
					if upstream.MetadataSessionID(call.Body) != "header-session" {
						t.Fatal("正文会话必须与优先头一致")
					}
					meta := call.Body["metadata"].(map[string]any)
					var identity map[string]any
					if err := json.Unmarshal([]byte(meta["user_id"].(string)), &identity); err != nil || identity["device_id"] == "foreign-device" || identity["account_uuid"] != "" {
						t.Fatal("设备身份只能来自当前账号，不能沿用客户端设备/账号")
					}
					if path == "/v1/messages" || path == "/async/v1/messages" {
						if meta["native_extension"] != "keep-native-only" {
							t.Fatal("原生 Anthropic metadata 扩展不能被本批吞掉")
						}
					} else if len(meta) != 1 {
						t.Fatal("兼容协议只能转换明确白名单，不能透传任意 metadata")
					}
					if call.Body["model"] != modelName || call.Body["max_tokens"] != float64(64) {
						t.Fatal("归因修复不得提高上限或统一两种模型")
					}
					tools, _ := call.Body["tools"].([]any)
					if len(tools) != 1 || tools[0].(map[string]any)["name"] != "caller_tool" {
						t.Fatal("不能为了 desktop 外观注入未提供的工具")
					}
					assertProtocolSystem(t, path, kind, body, call)
				})
			}
		}
	}
}

func assertProtocolSystem(t *testing.T, path, kind string, input map[string]any, call upstreamBody) {
	t.Helper()
	system := call.Body["system"]
	blocks, _ := system.([]any)
	if kind == "default" || kind == "custom" {
		want := upstream.ZcodeFallbackSystemBlocks()
		if kind == "custom" {
			want = append(want, map[string]any{"type": "text", "text": "客户端自有规则，保留 MCS/Gen UI 这些普通文字，不按关键词删除。"})
		}
		if !reflect.DeepEqual(blocks, want) {
			t.Fatal("实际 HTTP fallback 应为首块无缓存、稳定块保留缓存、真实能力及原文精确合并")
		}
		return
	}
	if kind == "mcs" {
		if len(blocks) != 0 || call.Header.Get("Anthropic-Beta") != "mid-conversation-system-2026-04-07" {
			t.Fatal("MCS 不应附加 fallback，且能力头必须与消息一致")
		}
		messages, _ := call.Body["messages"].([]any)
		if len(messages) != 3 || messages[1].(map[string]any)["content"] != "中途指令原样保留" {
			t.Fatal("MCS 消息的相对位置或内容被改变")
		}
		return
	}
	if path == "/v1/messages" || path == "/async/v1/messages" {
		if !reflect.DeepEqual(system, input["system"]) {
			t.Fatal("调用者官方/显式空 system 应完全保真")
		}
		return
	}
	if kind == "empty" {
		if blocks == nil || len(blocks) != 0 {
			t.Fatal("兼容协议的显式空指令不能被当作缺省")
		}
	} else if len(blocks) != 1 || blocks[0].(map[string]any)["text"] != upstream.OfficialSystemPrefix()+"\n调用者完整 GUI 上下文：仅使用客户端提供的工具。" {
		t.Fatal("兼容转换不能替换调用者完整 GUI 上下文")
	}
}

func TestTransparentSessionAndQueryLifecycleOverFourEntrypoints(t *testing.T) {
	for _, path := range attributionPaths {
		t.Run(path, func(t *testing.T) {
			f := newAttributionProtocolFixture(t)
			body := attributionProtocolBody(path, "GLM-5.3", "default")
			first := postAttributionProtocol(t, f, path, body, nil)
			second := postAttributionProtocol(t, f, path, body, nil)
			for _, key := range []string{"X-Session-Id", "X-Zcode-Trace-Id", "X-Query-Id", "X-Request-Id"} {
				if first.Header.Get(key) == "" || first.Header.Get(key) == second.Header.Get(key) {
					t.Fatalf("缺失 %s 应每入口生成，不按共享认证/正文合并请求", key)
				}
			}
			body["metadata"] = map[string]any{"user_id": `{"session_id":"sess_body-only"}`}
			headers := http.Header{"X-Query-Id": {"query_same-query"}}
			first = postAttributionProtocol(t, f, path, body, headers)
			second = postAttributionProtocol(t, f, path, body, headers)
			if first.Header.Get("X-Session-Id") != "body-only" || second.Header.Get("X-Session-Id") != "body-only" || upstream.MetadataSessionID(first.Body) != "body-only" {
				t.Fatal("各协议仅正文 session 时也应精确透传并同步头体")
			}
			if first.Header.Get("X-Query-Id") != "same-query" || second.Header.Get("X-Query-Id") != "same-query" || first.Header.Get("X-Request-Id") == second.Header.Get("X-Request-Id") || first.Header.Get("X-Zcode-Trace-Id") == second.Header.Get("X-Zcode-Trace-Id") {
				t.Fatal("固定 query 不是可信幂等授权；只给 session 时 trace 仍每入口生成")
			}
			ambiguous := http.Header{
				"X-Session-Id":     {"sess_first", "sess_second"},
				"X-Zcode-Trace-Id": {"trace-first", "trace-second"},
				"X-Query-Id":       {"query_first", "query_second"},
			}
			call := postAttributionProtocol(t, f, path, body, ambiguous)
			if call.Header.Get("X-Session-Id") != "body-only" || upstream.MetadataSessionID(call.Body) != "body-only" {
				t.Fatal("多值 session 头不可任意选一个；应回退合法正文来源")
			}
			if call.Header.Get("X-Zcode-Trace-Id") == "" || strings.Contains(call.Header.Get("X-Zcode-Trace-Id"), "trace-") || call.Header.Get("X-Query-Id") == "" || strings.Contains(call.Header.Get("X-Query-Id"), "first") || strings.Contains(call.Header.Get("X-Query-Id"), "second") {
				t.Fatal("无唯一来值的 trace/query 应生成本入口值，不透传合并多值")
			}
		})
	}
}

func TestQuerySourceMultivalueAndMissingOverFourEntrypoints(t *testing.T) {
	for _, path := range attributionPaths {
		t.Run(path, func(t *testing.T) {
			f := newAttributionProtocolFixture(t)
			body := attributionProtocolBody(path, "GLM-5.3", "default")
			for _, tc := range []struct {
				values []string
				want   string
			}{
				{nil, ""}, {[]string{""}, ""}, {[]string{"main_turn"}, "main_turn"},
				{[]string{"subagent:review.v1"}, "subagent:review.v1"},
				{[]string{"invalid space"}, ""}, {[]string{strings.Repeat("x", 129)}, ""},
				{[]string{"compact", "compact"}, ""}, {[]string{"compact", "main_turn"}, ""},
			} {
				headers := http.Header{}
				if tc.values != nil {
					headers["x-ZcOdE-qUeRy-SoUrCe"] = tc.values
				}
				call := postAttributionProtocol(t, f, path, body, headers)
				if got := call.Header.Get("X-Zcode-Query-Source"); got != tc.want {
					t.Fatalf("真实入口用途不符：values=%v got=%q want=%q", tc.values, got, tc.want)
				}
				if tc.want == "" && len(call.Header.Values("X-Zcode-Query-Source")) != 0 {
					t.Fatal("缺失/非法/多值用途必须不发送，不统一伪造 main_turn")
				}
			}
		})
	}
}
