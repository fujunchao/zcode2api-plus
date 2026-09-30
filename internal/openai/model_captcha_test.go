package openai

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"zcode2api/internal/captcha"
	"zcode2api/internal/model"
)

func TestCompatibleAPIsSkipJWTModelCaptcha(t *testing.T) {
	for _, api := range []string{"chat", "responses"} {
		for _, stream := range []bool{false, true} {
			name := api + "/JSON"
			if stream {
				name = api + "/SSE"
			}
			t.Run(name, func(t *testing.T) {
				f := newFixture(t)
				if ok, err := f.st.RemoveAccount(model.ProviderZai, "e2e-acc"); err != nil || !ok {
					t.Fatalf("替换测试账号失败: %v", err)
				}
				if _, err := f.st.AddAccount(model.ProviderZai, "jwt-model-skip", "header.payload.sig"); err != nil {
					t.Fatal(err)
				}
				f.cm.SetConfigProvider(func(context.Context) (captcha.Config, error) {
					return captcha.Config{Enabled: true, SkipModelRequest: true}, nil
				})
				f.setResponder(replyText)
				if stream {
					f.setResponder(func(int) (int, string, string) {
						return http.StatusOK, "text/event-stream",
							"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_skip\",\"model\":\"GLM-5.3\",\"usage\":{\"input_tokens\":4}}}\n\n" +
								"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"回答\"}}\n\n" +
								"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n" +
								"data: {\"type\":\"message_stop\"}\n\n"
					})
				}
				body := map[string]any{"model": "glm-5.3-flash", "stream": stream}
				if api == "chat" {
					body["messages"] = []any{map[string]any{"role": "user", "content": "hi"}}
				} else {
					body["input"] = "hi"
				}
				status, raw := postCompat(t, f, api, body)
				if status != 200 || !strings.Contains(raw, "回答") {
					t.Fatalf("JWT 模型跳过应在兼容接口正常返回: %d %s", status, raw)
				}
				if stream && (api == "chat" && !strings.Contains(raw, "data: [DONE]") || api == "responses" && !strings.Contains(raw, "response.completed")) {
					t.Fatalf("兼容流应正常结束: %s", raw)
				}
				call := f.lastUpstream()
				if call.Header.Get("Authorization") != "Bearer header.payload.sig" {
					t.Fatalf("兼容接口必须使用 JWT 鉴权: %v", call.Header)
				}
				for _, key := range []string{"X-Aliyun-Captcha-Verify-Param", "X-Aliyun-Captcha-Verify-Region"} {
					if call.Header.Get(key) != "" {
						t.Fatalf("兼容接口不能重新加回模型验证码: %v", call.Header)
					}
				}
				system, ok := call.Body["system"].([]any)
				if !ok || len(system) < 3 {
					t.Fatalf("兼容接口不能丢失 JWT system: %v", call.Body["system"])
				}
			})
		}
	}
}
