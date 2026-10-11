package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"zcode2api/internal/captcha"
	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/web"
)

// 在引擎入口保留原对象，同时通过真实 HTTP 捕获每次尝试的实际身份。
func TestMetadataIsolationSyncRetries(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusMethodNotAllowed} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			oldData, oldPreserve := config.DataDir, config.PreserveClientContext
			config.DataDir, config.PreserveClientContext = t.TempDir(), true
			t.Cleanup(func() { config.DataDir, config.PreserveClientContext = oldData, oldPreserve })
			f := newFixture(t)
			f.cm.SetConfigProvider(func(context.Context) (captcha.Config, error) {
				return captcha.Config{Enabled: true, SkipModelRequest: true}, nil
			})
			accounts := map[string]*model.Account{}
			for _, name := range []string{"first", "second"} {
				secret := "header.payload." + name
				acc, err := f.st.AddAccount(model.ProviderZai, name, secret)
				if err != nil {
					t.Fatal(err)
				}
				accounts[secret] = acc
			}
			f.respond = func(n int, _ *http.Request) (int, http.Header, string) {
				if n == 1 {
					body := `{"code":3009,"msg":"rate limited"}`
					if status == 405 {
						body = `{"code":3012,"msg":"request has been blocked due to unusual activity."}`
					}
					return status, http.Header{"Content-Type": {"application/json"}}, body
				}
				return 200, http.Header{"Content-Type": {"application/json"}}, okUpstreamJSON
			}
			body := msgBody()
			body["system"] = "synthetic-client-instructions"
			body["metadata"] = map[string]any{"user_id": `{"device_id":"client","account_uuid":"client","session_id":"synthetic-session"}`, "trace_hint": "keep", "extension": map[string]any{"flag": true}}
			NormalizeBody(body, false)
			before, _ := marshalJSON(body)
			key := ContentKey("GLM-5.3", body)
			buf := &syncBuffer{}
			web.SetOut(buf)
			t.Cleanup(func() { web.SetOut(nil) })
			var response bytes.Buffer
			result := f.eng.RunMessages(context.Background(), body, nil, func(d Delivery) error {
				_, err := io.Copy(&response, d.Body)
				return err
			})
			if !result.Delivered || response.String() != okUpstreamJSON || f.callCount() != 2 {
				t.Fatalf("保持原重试及交付：result=%+v attempts=%d", result, f.callCount())
			}
			after, _ := marshalJSON(body)
			if string(before) != string(after) || ContentKey("GLM-5.3", body) != key {
				t.Error("同一入站内的尝试污染了入口 metadata 或内容键")
			}
			line := buf.diagLine(t)
			wantHash := strings.TrimPrefix(key, "GLM-5.3|")
			if diagField(t, line, "bodyhash") != wantHash || diagField(t, line, "input_ref") != wantHash {
				t.Errorf("同账号重试/换号不应使内容视图漂移：%s", line)
			}
			f.mu.Lock()
			calls := append([]upstreamCall(nil), f.calls...)
			f.mu.Unlock()
			for _, call := range calls {
				acc := accounts[strings.TrimPrefix(call.Header.Get("Authorization"), "Bearer ")]
				if acc == nil {
					t.Fatal("实际尝试必须使用合成账号")
				}
				meta := call.Body["metadata"].(map[string]any)
				var identity map[string]any
				if err := json.Unmarshal([]byte(meta["user_id"].(string)), &identity); err != nil {
					t.Fatal(err)
				}
				if identity["device_id"] != acc.DeviceMidOr(config.DeviceMid()) || identity["session_id"] != "synthetic-session" || identity["session_id"] != call.Header.Get("X-Session-Id") || identity["account_uuid"] != "" {
					t.Fatalf("隔离修复不能改变实际账号/会话身份：%v", identity)
				}
				if meta["trace_hint"] != "keep" || call.Body["system"] != body["system"] {
					t.Fatal("客户端扩展字段和指令不得被改写")
				}
			}
			sameAccount := calls[0].Header.Get("Authorization") == calls[1].Header.Get("Authorization")
			if sameAccount != (status == 429) {
				t.Fatal("429 原地重试与 405 换号的原策略不得改变")
			}
		})
	}
}
