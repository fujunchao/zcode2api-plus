package adminapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"zcode2api/internal/asyncpool"
	"zcode2api/internal/auth"
	"zcode2api/internal/captcha"
	"zcode2api/internal/gateway"
	"zcode2api/internal/model"
	"zcode2api/internal/openai"
)

func TestAllModelRoutesShareRequestAccounting(t *testing.T) {
	paths := []string{"/v1/messages", "/v1/chat/completions", "/v1/responses", "/async/v1/messages"}
	for _, path := range paths {
		for _, scenario := range []string{"success", "retry", "stream-error", "truncated", "unauthorized", "invalid-model"} {
			t.Run(path+"/"+scenario, func(t *testing.T) {
				mux, st, _ := setup(t)
				_, _ = st.AddAccount(model.ProviderZai, "test", "test.e30.signature")
				cm := captcha.NewManager()
				cm.SetConfigProvider(func(context.Context) (captcha.Config, error) { return captcha.Config{Enabled: false}, nil })
				calls := 0
				stream := path == "/async/v1/messages" || (scenario == "stream-error" || scenario == "truncated")
				client := &http.Client{Transport: statsTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					status, contentType, body := 200, "application/json", "{\"id\":\"m1\",\"model\":\"GLM-5.3\",\"content\":[{\"type\":\"text\",\"text\":\"OK\"}],\"stop_reason\":\"end_turn\"}"
					if stream {
						contentType = "text/event-stream"
						body = "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"GLM-5.3\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n"
					}
					if scenario == "retry" && calls == 1 {
						status, contentType, body = 529, "application/json", "{\"code\":1305}"
					}
					if scenario == "truncated" {
						contentType = "text/event-stream"
						body = "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\"}}\n\n"
					}
					if scenario == "stream-error" {
						contentType = "text/event-stream"
						body = "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"failed\"}}\n\n"
					}
					return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body))}, nil
				})}
				au := auth.New(st)
				engine := gateway.NewEngine(st, cm, client)
				engine.OverloadRetryDelays = []time.Duration{0}
				gw := gateway.Handler{Engine: engine, Auth: au}
				gw.Register(mux)
				openai.New(engine, au).Register(mux)
				pool := asyncpool.NewPool(st, au, cm)
				pool.Client = client
				pool.OverloadRetryDelays = []time.Duration{0}
				defer pool.Close()
				pool.Register(mux)
				modelName := "GLM-5.3"
				if scenario == "invalid-model" {
					modelName = "unknown"
				}
				raw := "{\"model\":\"" + modelName + "\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}],\"input\":\"hi\",\"stream\":"
				if stream {
					raw += "true}"
				} else {
					raw += "false}"
				}
				request := httptest.NewRequest("POST", path, strings.NewReader(raw))
				key := st.GatewayKey()
				if scenario == "unauthorized" {
					key = "wrong"
				}
				request.Header.Set("Authorization", "Bearer "+key)
				mux.ServeHTTP(httptest.NewRecorder(), request)
				got := st.Requests.Snapshot()
				successes, attempts, retries := int64(1), int64(1), int64(0)
				switch scenario {
				case "stream-error", "truncated":
					successes = 0
				case "unauthorized", "invalid-model":
					successes, attempts = 0, 0
				case "retry":
					attempts, retries = 2, 1
				}
				if got.Total != 1 || got.Succeeded != successes || got.Failed != 1-successes || got.Active != 0 || got.UpstreamAttempts != attempts || got.Retries != retries {
					t.Fatalf("接口计数与终态不符：%+v", got)
				}
			})
		}
	}
}
