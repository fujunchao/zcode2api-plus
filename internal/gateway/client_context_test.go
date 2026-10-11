package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"zcode2api/internal/captcha"
	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

func TestJWTKeepsCallerSystemOverHTTP(t *testing.T) {
	for _, tc := range []struct {
		name   string
		system any
	}{
		{"text", "只使用本地客户端声明的工具，工作目录为 /client/work。"},
		{"empty_text", ""},
		{"empty_blocks", []any{}},
		{"blocks_with_extensions", []any{
			map[string]any{"type": "text", "text": "调用者规则", "cache_control": map[string]any{"type": "ephemeral", "ttl": "1h"}},
			map[string]any{"type": "text", "text": "调用者规则", "caller_metadata": "不可丢失"},
		}},
		{"complete_client_context", []any{
			map[string]any{"type": "text", "text": "You are ZCode, an interactive coding agent", "cache_control": map[string]any{"type": "ephemeral"}},
			map[string]any{"type": "text", "text": "这是客户端提供的扩展身份与交互规则。"},
			map[string]any{"type": "text", "text": "# Environment\nPrimary working directory: /client/work"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldData := config.DataDir
			config.DataDir = t.TempDir()
			t.Cleanup(func() { config.DataDir = oldData })
			f := newFixture(t)
			f.cm.SetConfigProvider(func(context.Context) (captcha.Config, error) {
				return captcha.Config{Enabled: true, SkipModelRequest: true}, nil
			})
			if _, err := f.st.AddAccount(model.ProviderZai, "context-fixture", "header.payload.signature"); err != nil {
				t.Fatal(err)
			}
			f.respond = jsonResp(http.StatusOK, okUpstreamJSON)
			body := msgBody()
			body["system"] = tc.system
			body["tools"] = []any{map[string]any{"name": "client_read", "input_schema": map[string]any{"type": "object"}}}
			body["tool_choice"] = map[string]any{"type": "auto"}
			body["max_tokens"] = float64(1024)
			before, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			status, raw := f.post(t, body, "sk-test")
			if status != http.StatusOK || raw != okUpstreamJSON {
				t.Fatalf("客户端应正常收到响应: %d %s", status, raw)
			}
			call := f.lastCall()
			if !reflect.DeepEqual(call.Body["system"], tc.system) {
				t.Fatalf("调用者 system 不应被追加网关身份/环境: got=%#v want=%#v", call.Body["system"], tc.system)
			}
			for _, key := range []string{"tools", "tool_choice", "max_tokens"} {
				if !reflect.DeepEqual(call.Body[key], body[key]) {
					t.Fatalf("不得替本地客户端改写 %s: got=%#v want=%#v", key, call.Body[key], body[key])
				}
			}
			if call.Header.Get("Anthropic-Beta") != "" {
				t.Fatal("只有顶层 system 时不能自动声明 MCS")
			}
			after, err := json.Marshal(body)
			if err != nil || string(before) != string(after) {
				t.Fatal("不得修改客户端持有的原请求")
			}
		})
	}
}
