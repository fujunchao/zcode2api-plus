package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"zcode2api/internal/captcha"
	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

func TestNativeMidSystemBodyAndBeta(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content any
		want    any
	}{
		{"string", "后续只读操作", "后续只读操作"},
		{"plain_block", []any{map[string]any{"type": "text", "text": "后续只读操作"}}, "后续只读操作"},
		{"cache_block", []any{map[string]any{"type": "text", "text": "缓存指令", "cache_control": map[string]any{"type": "ephemeral"}}}, nil},
		{"extension_block", []any{map[string]any{"type": "text", "text": "扩展指令", "caller_metadata": "保留"}}, nil},
		{"multiple_blocks", []any{map[string]any{"type": "text", "text": "规则一"}, map[string]any{"type": "text", "text": "规则二"}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldData := config.DataDir
			config.DataDir = t.TempDir()
			t.Cleanup(func() { config.DataDir = oldData })
			f := newFixture(t)
			f.cm.SetConfigProvider(func(context.Context) (captcha.Config, error) {
				return captcha.Config{Enabled: true, SkipModelRequest: true}, nil
			})
			if _, err := f.st.AddAccount(model.ProviderZai, "mcs-fixture", "header.payload.sig"); err != nil {
				t.Fatal(err)
			}
			f.respond = jsonResp(http.StatusOK, okUpstreamJSON)
			body := msgBody()
			body["system"] = "客户端初始指令"
			body["messages"] = []any{
				map[string]any{"role": "user", "content": "第一轮"},
				map[string]any{"role": "system", "content": tc.content, "caller_tag": "保留消息属性"},
				map[string]any{"role": "user", "content": "第二轮"},
			}
			payload, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			req, err := http.NewRequest(http.MethodPost, f.srv.URL+"/v1/messages", bytes.NewReader(payload))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("x-api-key", "sk-test")
			req.Header.Set("Anthropic-Beta", "caller-feature, mid-conversation-system-2026-04-07, caller-feature")
			resp, err := f.srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			raw, err := io.ReadAll(resp.Body)
			if err != nil || resp.StatusCode != http.StatusOK || string(raw) != okUpstreamJSON {
				t.Fatalf("原生交付失败: %d %s %v", resp.StatusCode, raw, err)
			}
			up := f.lastCall()
			want := tc.want
			if want == nil {
				want = tc.content
			}
			msgs := up.Body["messages"].([]any)
			mid := msgs[1].(map[string]any)
			if len(msgs) != 3 || mid["role"] != "system" || mid["caller_tag"] != "保留消息属性" || !reflect.DeepEqual(mid["content"], want) {
				t.Fatalf("中途系统消息不得丢属性或改位置: %v", msgs)
			}
			beta := up.Header.Get("Anthropic-Beta")
			if strings.Count(beta, "mid-conversation-system-2026-04-07") != 1 || strings.Count(beta, "caller-feature") != 1 {
				t.Fatalf("应合并并去重能力，保留调用者 beta: %q", beta)
			}
		})
	}
}
