package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"zcode2api/internal/model"
)

// 首条回归从真实 HTTP 入口进入；不支持的卡上下文不能被当作普通请求成功执行。
func TestAccountContextHeadersRejectedBeforeUpstream(t *testing.T) {
	f := newFixture(t)
	f.setResponder(replyText)
	req, err := http.NewRequest(http.MethodPost, f.srv.URL+"/v1/messages",
		strings.NewReader(`{"model":"GLM-5.3","max_tokens":64,"messages":[{"role":"user","content":"合成卡上下文回归"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("X-Highspeed-Card-ID", "synthetic-private-card")
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("未支持的卡上下文必须在选号前返回 400，实际 %d: %s", resp.StatusCode, raw)
	}
	var body struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("错误响应必须是 JSON: %v", err)
	}
	if body.Error.Type != "unsupported_account_context" {
		t.Errorf("应给出明确的账号上下文错误，实际 %q", body.Error.Type)
	}
	if strings.Contains(string(raw), "synthetic-private-card") {
		t.Error("错误响应不得回显调用者的卡 ID")
	}
	f.mu.Lock()
	calls := len(f.calls)
	f.mu.Unlock()
	if calls != 0 {
		t.Errorf("拒绝的请求不得消耗普通账号，上游实际收到 %d 次请求", calls)
	}
}

const accountContextStream = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_context\",\"model\":\"GLM-5.3\",\"usage\":{\"input_tokens\":3}}}\n\n" +
	"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"回答\"}}\n\n" +
	"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
	"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n" +
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

func accountContextPayload(path string, stream bool) map[string]any {
	body := map[string]any{"model": "GLM-5.3", "stream": stream}
	if path == "/v1/responses" {
		body["input"] = "合成账号上下文回归"
	} else {
		body["max_tokens"] = 64
		body["messages"] = []any{map[string]any{"role": "user", "content": "合成账号上下文回归"}}
	}
	return body
}

func postAccountContextRequest(t *testing.T, f *fixture, path, key string, stream bool, headers http.Header) (int, string) {
	t.Helper()
	payload, err := json.Marshal(accountContextPayload(path, stream))
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, f.srv.URL+path, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	for name, values := range headers {
		req.Header[name] = append([]string(nil), values...)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(raw)
}

func assertAccountContextRejected(t *testing.T, status int, raw string, privateValues ...string) {
	t.Helper()
	var body struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if status != http.StatusBadRequest || json.Unmarshal([]byte(raw), &body) != nil || body.Error.Type != "unsupported_account_context" {
		t.Fatalf("未支持的账号上下文应明确拒绝，不能降级为普通请求：%d %s", status, raw)
	}
	for _, value := range privateValues {
		if value != "" && strings.Contains(raw, value) {
			t.Fatal("拒绝响应不得包含调用方的凭据或卡上下文值")
		}
	}
}

// 三个公开同步入口在 JWT/API Key、流式/非流式下共享同一请求契约。
func TestAccountContextHeadersRejectedAcrossSyncAPIs(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"} {
		for _, jwt := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/jwt=%t/stream=%t", path, jwt, stream), func(t *testing.T) {
					f := newClientContextFixture(t, jwt)
					if stream {
						f.setResponder(func(int) (int, string, string) {
							return http.StatusOK, "text/event-stream", accountContextStream
						})
					}
					for _, field := range []string{
						"X-Highspeed-Card-ID", "X-Bigmodel-Authorization",
						"Bigmodel-Target-Type", "Bigmodel-Organization", "Bigmodel-Project",
					} {
						for _, name := range []string{field, strings.ToLower(field), strings.ToUpper(field)} {
							for _, value := range []string{"synthetic-private-context", ""} {
								status, raw := postAccountContextRequest(t, f, path, "sk-test", stream, http.Header{name: {value}})
								assertAccountContextRejected(t, status, raw, value)
							}
						}
					}
					status, raw := postAccountContextRequest(t, f, path, "sk-test", stream, http.Header{
						"X-Highspeed-Card-ID":      {"synthetic-card-one", "synthetic-card-two"},
						"X-Bigmodel-Authorization": {"synthetic-secondary-token"},
					})
					assertAccountContextRejected(t, status, raw, "synthetic-card-one", "synthetic-card-two", "synthetic-secondary-token")
					f.mu.Lock()
					calls := len(f.calls)
					f.mu.Unlock()
					if calls != 0 {
						t.Fatalf("任何被拒绝请求都不得出站，实际 %d 次", calls)
					}
					// 拒绝请求之后同一池仍能服务普通请求，不应把池中凭据误标为失效。
					status, raw = postAccountContextRequest(t, f, path, "sk-test", stream, nil)
					if status != http.StatusOK || !strings.Contains(raw, "回答") {
						t.Fatalf("拒绝客户端上下文不能损伤普通账号：%d %s", status, raw)
					}
					f.mu.Lock()
					calls = len(f.calls)
					f.mu.Unlock()
					if calls != 1 {
						t.Fatalf("只有随后的合法请求应出站，实际 %d 次", calls)
					}
				})
			}
		}
	}
}

// 没有可用账号也必须先识别请求错误；鉴权顺序与原接口一致。
func TestAccountContextHeadersRejectedBeforePoolSelection(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			f := newFixture(t)
			if removed, err := f.st.RemoveAccount(model.ProviderZai, "e2e-acc"); err != nil || !removed {
				t.Fatalf("清空合成账号池失败：%v", err)
			}
			headers := http.Header{"X-Highspeed-Card-ID": {"synthetic-private-card"}}
			status, raw := postAccountContextRequest(t, f, path, "sk-test", false, headers)
			assertAccountContextRejected(t, status, raw, "synthetic-private-card")
			for _, authCase := range []struct {
				key    string
				status int
			}{
				{"", http.StatusUnauthorized},
				{"invalid-synthetic-gateway-key", http.StatusForbidden},
			} {
				status, raw = postAccountContextRequest(t, f, path, authCase.key, false, headers)
				if status != authCase.status || strings.Contains(raw, "unsupported_account_context") {
					t.Fatalf("未授权请求仍应先返回鉴权错误：%d %s", status, raw)
				}
			}
		})
	}
}

// 允许的协议/归因头仍按原有规则上行；普通请求的原地重试和换号不受新校验影响。
func TestAccountContextAllowedHeadersAcrossRetries(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"} {
		for _, jwt := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				for _, switchAccount := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/jwt=%t/stream=%t/switch=%t", path, jwt, stream, switchAccount), func(t *testing.T) {
						f := newClientContextFixture(t, jwt)
						f.eng.RateLimitRetryDelay = 0
						first, second := "sk-e2e-credential", "sk-context-second"
						if jwt {
							first, second = "header.payload.sig", "header.payload.context-second"
						}
						if _, err := f.st.AddAccount(model.ProviderZai, "context-second", second); err != nil {
							t.Fatal(err)
						}
						f.setResponder(func(call int) (int, string, string) {
							if call == 1 {
								if switchAccount {
									return http.StatusUnauthorized, "application/json", `{"error":{"message":"synthetic invalid credential"}}`
								}
								return http.StatusTooManyRequests, "application/json", `{"code":3009,"msg":"rate limited"}`
							}
							if stream {
								return http.StatusOK, "text/event-stream", accountContextStream
							}
							return replyText(call)
						})
						allowed := http.Header{
							"Anthropic-Beta":            {"interleaved-thinking-2025-05-14"},
							"X-Custom-Safe":             {"safe-extension"},
							"X-Highspeed-Card-ID-Trace": {"safe-not-account-context"},
							"X-Session-Id":              {"synthetic-session"},
							"X-Zcode-Trace-Id":          {"synthetic-trace"},
							"X-Zcode-Session-Type":      {"subagent"},
							"X-Query-Id":                {"synthetic-query"},
						}
						status, raw := postAccountContextRequest(t, f, path, "sk-test", stream, allowed)
						if status != http.StatusOK || !strings.Contains(raw, "回答") {
							t.Fatalf("合法请求的原有重试及交付应保持：%d %s", status, raw)
						}
						f.mu.Lock()
						calls := append([]upstreamBody(nil), f.calls...)
						f.mu.Unlock()
						if len(calls) != 2 {
							t.Fatalf("一次失败后应只有一次重试，实际 %d 次", len(calls))
						}
						for _, call := range calls {
							token := strings.TrimPrefix(call.Header.Get("Authorization"), "Bearer ")
							if token != first && token != second || call.Header.Get("X-Api-Key") != token {
								t.Fatal("主鉴权必须来自该次所选的合成账号且双头一致")
							}
							for name, values := range allowed {
								if got := call.Header.Get(name); got != values[0] {
									t.Errorf("允许头 %s 在重试中丢失或被改写：%q", name, got)
								}
							}
							for _, name := range []string{"X-Highspeed-Card-ID", "X-Bigmodel-Authorization", "Bigmodel-Target-Type", "Bigmodel-Organization", "Bigmodel-Project"} {
								if len(call.Header.Values(name)) != 0 {
									t.Errorf("普通请求不应被网关注入账号绑定头 %s", name)
								}
							}
						}
						same := calls[0].Header.Get("Authorization") == calls[1].Header.Get("Authorization")
						if same == switchAccount {
							t.Fatal("429 原地重试与 401 换号的既有策略不应改变")
						}
						if calls[0].Header.Get("X-Request-Id") == "" || calls[0].Header.Get("X-Request-Id") == calls[1].Header.Get("X-Request-Id") {
							t.Fatal("每次上游尝试仍须使用不同的请求 ID")
						}
					})
				}
			}
		}
	}
}
