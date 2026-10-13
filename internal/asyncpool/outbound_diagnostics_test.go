package asyncpool

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"zcode2api/internal/config"
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
			// 静默窗口字段与配置一致（默认 24h；本断言动态取值，不锁定具体窗口）。
			windowField := fmt.Sprintf("risk_extra_window_s=%d", int(config.RiskSilentWindow().Seconds()))
			for _, want := range []string{"outbound_model=glm-5.3", "outbound_max_tokens=128000", "outbound_thinking=enabled", "outbound_effort=max",
				"system_text_bytes=", "system_text_sha256=", "official_prefix_match=true", "official_identity_match=true",
				"account_status_before=active", windowField, "risk_extra_active=false", "quota_model_state=available", "quota_available=42", "quota_observed_at=",
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

func TestModelConcurrencyAsyncDoesNotSwitchOrCool(t *testing.T) {
	var calls atomic.Int32
	p, srv, _ := newDiagnosticHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"code":3009,"msg":"model concurrency limit exceeded"}`)
	}))
	addJWTAccountN(t, p.Store, "unrelated-account", 9)
	_, status, response := postDiagnosticHTTP(t, srv, "/async/v1/messages", `{"model":"GLM-5.3","messages":[{"role":"user","content":"synthetic"}]}`)
	p.Close()
	if status != 200 || !strings.Contains(response, "3009") || !strings.Contains(response, "upstream_rate_limit") || calls.Load() != 3 {
		t.Fatalf("并发有限重试应保留原始码: calls=%d %s", calls.Load(), response)
	}
	marked := 0
	for _, a := range p.Store.ListAccounts(model.ProviderZai) {
		if a.Status != model.StatusActive || a.CoolingUntil != nil || a.RateLimitStreak != 0 {
			t.Fatal("不得冷却账号")
		}
		if a.LastErrorKind != nil && *a.LastErrorKind == model.ErrorKindModelBusy {
			marked++
		}
	}
	if marked != 1 {
		t.Fatal("并发重试不得改用另一个账号")
	}
}

// 异步不覆盖默认值：每档到期即重新调用，不叠加额外静默窗口。
func TestRiskLadderAsyncDefaultCooling(t *testing.T) {
	var calls atomic.Int32
	p, srv, _ := newDiagnosticHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(405)
			_, _ = io.WriteString(w, `{"code":3012,"msg":"blocked due to unusual activity"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	raw := `{"model":"GLM-5.3","messages":[{"role":"user","content":"synthetic"}]}`
	_, _, _ = postDiagnosticHTTP(t, srv, "/async/v1/messages", raw)
	a := p.Store.ListAccounts(model.ProviderZai)[0]
	if a.Status != model.StatusCooling || a.LastRiskControlAt == nil {
		t.Fatal("首次拒绝应进入阶梯")
	}
	_, err := p.Store.Update(a.Provider, a.ID, func(a *model.Account) {
		expired := float64(time.Now().Add(-time.Second).Unix())
		a.CoolingUntil = &expired
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, response := postDiagnosticHTTP(t, srv, "/async/v1/messages", raw)
	p.Close()
	if calls.Load() != 2 || !strings.Contains(response, "event: done") {
		t.Fatalf("默认阶梯到期应重新调用: %s", response)
	}
	if p.Store.FindAny(a.ID).RiskControlStreak != 0 {
		t.Fatal("成功应清零风控连击")
	}
}
