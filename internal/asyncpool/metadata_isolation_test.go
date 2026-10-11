package asyncpool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"zcode2api/internal/captcha"
	"zcode2api/internal/config"
	"zcode2api/internal/gateway"
	"zcode2api/internal/model"
	"zcode2api/internal/web"
)

func TestMetadataIsolationAsyncRetries(t *testing.T) {
	for _, status := range []int{429, 405} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			p, st, cm, _ := newTestPool(t)
			t.Cleanup(p.Close)
			config.AsyncMaxRetries = 1
			oldPreserve := config.PreserveClientContext
			config.PreserveClientContext = true
			t.Cleanup(func() { config.PreserveClientContext = oldPreserve })
			accounts := map[string]*model.Account{}
			for i := range 2 {
				a := addJWTAccountN(t, st, fmt.Sprintf("metadata-account-%d", i), i)
				accounts[*a.JWTToken] = a
			}
			cm.SetConfigProvider(func(context.Context) (captcha.Config, error) {
				return captcha.Config{Enabled: true, SkipModelRequest: true}, nil
			})
			type observed struct {
				header http.Header
				body   map[string]any
			}
			var mu sync.Mutex
			var calls []observed
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
					w.WriteHeader(500)
					return
				}
				mu.Lock()
				calls = append(calls, observed{r.Header.Clone(), payload})
				n := len(calls)
				mu.Unlock()
				if n == 1 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(status)
					if status == 405 {
						_, _ = io.WriteString(w, `{"code":3012,"msg":"request has been blocked due to unusual activity."}`)
					} else {
						_, _ = io.WriteString(w, `{"code":3009,"msg":"rate limited"}`)
					}
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5}}}\n\n"+
					"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":7}}\n\n"+
					"data: {\"type\":\"message_stop\"}\n\n")
			}))
			t.Cleanup(up.Close)
			config.UpstreamZai = up.URL
			body := map[string]any{"model": "GLM-5.3", "stream": true, "system": "synthetic-client-instructions", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
				"metadata": map[string]any{"user_id": `{"device_id":"client","session_id":"synthetic-session"}`, "trace_hint": "keep"}}
			gateway.NormalizeBody(body, false)
			before, _ := marshalJSON(body)
			key := gateway.ContentKey("GLM-5.3", body)
			buf := &diagBuf{}
			web.SetOut(buf)
			t.Cleanup(func() { web.SetOut(nil) })
			ticket := insertTicket(p, "metadata-ticket", body)
			ctx, cancel := context.WithCancel(context.Background())
			ticket.cancel = cancel // 手工建票也补齐真实票务的取消能力，保证 Close 的生命周期契约。
			t.Cleanup(cancel)
			p.processTicket(ctx, "metadata-ticket")
			events := drainEvents(ticket)
			done := 0
			for _, e := range events {
				if e.Type == "error" {
					t.Fatalf("合成重试应完成：%v", e.Data)
				}
				if e.Type == "done" {
					done++
				}
			}
			if done != 1 {
				t.Fatalf("票据应恰好完成一次：%d", done)
			}
			after, _ := marshalJSON(body)
			if string(before) != string(after) || gateway.ContentKey("GLM-5.3", body) != key {
				t.Error("票据重试污染了入口 metadata/内容键")
			}
			wantHash := strings.TrimPrefix(key, "GLM-5.3|")
			match := regexp.MustCompile(`\bbodyhash=([0-9a-f]+)`).FindStringSubmatch(buf.String())
			if len(match) != 2 || match[1] != wantHash {
				t.Errorf("异步内容视图应稳定：%s", buf.String())
			}
			mu.Lock()
			observedCalls := append([]observed(nil), calls...)
			mu.Unlock()
			if len(observedCalls) != 2 {
				t.Fatalf("实际出站次数应保持两次：%d", len(observedCalls))
			}
			for _, call := range observedCalls {
				a := accounts[strings.TrimPrefix(call.header.Get("Authorization"), "Bearer ")]
				if a == nil {
					t.Fatal("未知合成账号")
				}
				meta := call.body["metadata"].(map[string]any)
				var id map[string]any
				if err := json.Unmarshal([]byte(meta["user_id"].(string)), &id); err != nil {
					t.Fatal(err)
				}
				if id["device_id"] != a.DeviceMidOr(config.DeviceMid()) || id["session_id"] != "synthetic-session" || id["session_id"] != call.header.Get("X-Session-Id") || id["account_uuid"] != "" || meta["trace_hint"] != "keep" {
					t.Fatalf("实际出站身份/扩展字段错误：%v", id)
				}
			}
			if (observedCalls[0].header.Get("Authorization") == observedCalls[1].header.Get("Authorization")) != (status == 429) {
				t.Fatal("不能改变原地重试/换号策略")
			}
		})
	}
}
