package adminapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"zcode2api/internal/auth"
	"zcode2api/internal/captcha"
	"zcode2api/internal/gateway"
	"zcode2api/internal/model"
)

type statsTransport func(*http.Request) (*http.Response, error)

func (f statsTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMonitorCountsRequestsRatherThanAccountSuccesses(t *testing.T) {
	mux, st, _ := setup(t)
	_, _ = st.AddAccount(model.ProviderZai, "test", "sk-test-upstream")
	status := 200
	client := &http.Client{Transport: statsTransport(func(r *http.Request) (*http.Response, error) {
		body := "{\"id\":\"m1\",\"content\":[],\"usage\":{\"output_tokens\":1}}"
		if status != 200 {
			body = "{\"error\":{\"message\":\"failed\"}}"
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	h := gateway.Handler{Engine: gateway.NewEngine(st, captcha.NewManager(), client), Auth: auth.New(st)}
	h.Register(mux)
	for _, code := range []int{200, 500} {
		status = code
		request := httptest.NewRequest("POST", "/v1/messages", strings.NewReader("{\"model\":\"GLM-5.3\",\"messages\":[]}"))
		request.Header.Set("Authorization", "Bearer "+st.GatewayKey())
		mux.ServeHTTP(httptest.NewRecorder(), request)
	}
	_, body := do(t, mux, st, "GET", "/admin/api/monitor", nil)
	requests := body["requests"].(map[string]any)
	if requests["total"] != float64(2) || requests["errors"] != float64(1) || requests["success_rate"] != float64(50) {
		t.Fatalf("一次成功加一次失败应为 2 次请求、50%%：%v", requests)
	}
}
