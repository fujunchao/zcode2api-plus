package asyncpool

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"zcode2api/internal/model"
	"zcode2api/internal/upstream"
)

func TestDiagnosticOutboundSnapshotBothRoutes(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/async/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			actual := make(chan map[string]any, 1)
			p, srv, logs := newDiagnosticHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var b map[string]any
				if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
					t.Error(err)
				}
				actual <- b
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"usage\":{\"input_tokens\":1}}}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
			}))
			a := p.Store.ListAccounts(model.ProviderZai)[0]
			_, err := p.Store.Update(a.Provider, a.ID, func(a *model.Account) {
				a.Quota = map[string]map[string]any{"GLM-5.3": {"remaining": 84.0, "available": 42.0}}
				at := float64(time.Now().Add(-time.Minute).Unix())
				a.LastCheckedAt = &at
				a.StartPlanObservation = model.NewStartPlanObservation(a, at, false)
			})
			if err != nil {
				t.Fatal(err)
			}
			body := map[string]any{"model": "GLM-5.3", "stream": true, "max_tokens": 128000,
				"thinking": map[string]any{"type": "enabled"}, "output_config": map[string]any{"effort": "max"},
				"system":   append(upstream.ZcodeSystemBlocks(), map[string]any{"type": "text", "text": "private-system-sentinel"}),
				"messages": []any{map[string]any{"role": "user", "content": "private-body-sentinel"}}}
			payload, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			_, status, response := postDiagnosticHTTP(t, srv, path, string(payload))
			p.Close()
			if status != 200 || !strings.Contains(response, "message_stop") {
				t.Fatalf("请求应正常完成: %d %s", status, response)
			}
			got := <-actual
			if got["max_tokens"] != float64(128000) || got["model"] != "GLM-5.3" {
				t.Fatal("观测不应改写出站参数")
			}
			for _, want := range []string{"outbound_model=glm-5.3", "outbound_max_tokens=128000", "outbound_thinking=enabled", "outbound_effort=max",
				"system_text_bytes=", "system_text_sha256=", "official_prefix_match=true", "official_identity_match=true",
				"account_status_before=active", "risk_extra_window_s=0", "risk_extra_active=false", "quota_model_state=available", "quota_available=42", "quota_observed_at=",
				"auth_dual_equal=true", "metadata_session_matches_header=true"} {
				if !strings.Contains(logs.String(), want) {
					t.Errorf("实际出站诊断缺少 %q", want)
				}
			}
			for _, secret := range []string{"private-system-sentinel", "private-body-sentinel", "header.payload.signature", "private-incoming-id"} {
				if strings.Contains(logs.String(), secret) {
					t.Fatalf("诊断泄漏 %s", secret)
				}
			}
		})
	}
}
