package openai

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"zcode2api/internal/model"
)

// 适配层拒绝的上游对象不能继续充当严格淘汰的完整成功对照。
// 正例使用同一 HTTP 链路，仅补回缺失 id，防止用全面关闭观察掩盖问题。
func TestCompatibleMissingMessageIDCannotSupplyStrictSuccess(t *testing.T) {
	for _, api := range []string{"chat", "responses"} {
		for _, valid := range []bool{false, true} {
			name := "missing-id"
			if valid {
				name = "valid-id"
			}
			t.Run(api+"/"+name, func(t *testing.T) {
				f := newFixture(t)
				response := map[string]any{
					"type": "message", "role": "assistant", "model": "GLM-5.3",
					"content": []any{map[string]any{"type": "text", "text": "合成输出"}}, "stop_reason": "end_turn",
					"usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
				}
				if valid {
					response["id"] = "msg_valid_control"
				}
				encoded, err := json.Marshal(response)
				if err != nil {
					t.Fatal(err)
				}
				var badCalls, controlCalls atomic.Int32
				badServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					badCalls.Add(1)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusMethodNotAllowed)
					_, _ = io.WriteString(w, `{"code":3012,"message":"synthetic risk control"}`)
				}))
				t.Cleanup(badServer.Close)
				controlServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					controlCalls.Add(1)
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(encoded)
				}))
				t.Cleanup(controlServer.Close)
				bad, err := f.st.AddProxyProfile("bad-route", badServer.URL, true)
				if err != nil {
					t.Fatal(err)
				}
				control, err := f.st.AddProxyProfile("control-route", controlServer.URL, true)
				if err != nil {
					t.Fatal(err)
				}
				account := f.st.ListAccounts(model.ProviderZai)[0]
				payload, err := json.Marshal(compatRequest(api))
				if err != nil {
					t.Fatal(err)
				}
				path := "/v1/chat/completions"
				if api == "responses" {
					path = "/v1/responses"
				}
				for i, id := range []string{bad.ID, bad.ID, control.ID} {
					// 仅在隔离合成存储中固定可比条件，不改变生产的冷却或换号上限。
					if _, err := f.st.Update(account.Provider, account.ID, func(live *model.Account) {
						live.Status, live.CoolingUntil, live.LastRiskControlAt, live.RiskControlStreak = model.StatusActive, nil, nil, 0
					}); err != nil {
						t.Fatal(err)
					}
					if _, err := f.st.AssignProxyProfile(account.ID, id); err != nil {
						t.Fatal(err)
					}
					req, err := http.NewRequest(http.MethodPost, f.srv.URL+path, bytes.NewReader(payload))
					if err != nil {
						t.Fatal(err)
					}
					req.Header.Set("Authorization", "Bearer sk-test")
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("X-Session-Id", "sess_same-synthetic-session")
					req.Header.Set("X-Zcode-Trace-Id", "same-synthetic-trace")
					resp, err := f.srv.Client().Do(req)
					if err != nil {
						t.Fatal(err)
					}
					raw, err := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					if err != nil {
						t.Fatal(err)
					}
					if i == 2 {
						if valid && resp.StatusCode != http.StatusOK {
							t.Fatalf("合法消息应完整交付：%d %s", resp.StatusCode, raw)
						}
						if !valid && (resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(raw), "invalid_upstream_response")) {
							t.Fatalf("缺id应明确返回502而不是空resp_：%d %s", resp.StatusCode, raw)
						}
					}
				}
				summary, err := f.st.StrictProxySummary()
				if err != nil || badCalls.Load() != 2 || controlCalls.Load() != 1 || summary.ActiveDispatch != 0 {
					t.Fatalf("对照必须经过3次实际HTTP且释放许可：summary=%+v bad=%d control=%d err=%v", summary, badCalls.Load(), controlCalls.Load(), err)
				}
				if (len(summary.Blocks) == 1) != valid {
					t.Fatalf("只有合法消息可作严格成功对照：valid=%t blocks=%+v", valid, summary.Blocks)
				}
				if !valid {
					for _, profile := range f.st.ListProxyProfiles() {
						if profile.ID == bad.ID {
							return
						}
					}
					t.Fatal("适配失败不能造成代理被移除")
				}
			})
		}
	}
}
