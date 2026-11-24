// 400 model_not_available 必须原样透传到 /v1/chat/completions 与 /v1/responses
// （gateway.WriteRunResult 路径）：客户端据此判定「换模型」，而不是当临时故障重试。
//
// 背景：2026-11-24 生产核实，80 个 zai/jwt（Start Plan）账号的额度快照只剩
// glm-5.3-flash 与 glm-5.4；此前的 503 会被中转层当故障反复重投。
package openai

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"zcode2api/internal/model"
)

func TestModelUnavailablePassthrough(t *testing.T) {
	for _, api := range []string{"chat", "responses"} {
		t.Run(api, func(t *testing.T) {
			f := newFixture(t)
			// 唯一账号的额度快照只含 glm-5.3-flash 与 GLM-5.4：请求 GLM-5.3 属于
			// 「池子确定不含此模型」，与官方下架后的生产形态一致。
			acc := f.st.ListAccounts(model.ProviderZai)[0]
			if _, err := f.st.Update(acc.Provider, acc.ID, func(a *model.Account) {
				a.Quota = map[string]map[string]any{
					"套餐-1": {"model": "glm-5.3-flash", "remaining": float64(100), "available": float64(100)},
					"套餐-2": {"model": "GLM-5.4", "remaining": float64(100), "available": float64(100)},
				}
			}); err != nil {
				t.Fatal(err)
			}
			f.setResponder(func(int) (int, string, string) {
				t.Error("确定性不可用不应触达上游")
				return http.StatusBadGateway, "application/json", `{"error":"unexpected"}`
			})

			status, raw := postCompat(t, f, api, compatRequest(api))
			if status != http.StatusBadRequest {
				t.Fatalf("应原样透传 400: %d %s", status, raw)
			}
			var payload map[string]any
			if err := json.Unmarshal([]byte(raw), &payload); err != nil {
				t.Fatalf("错误体不是合法 JSON: %v %s", err, raw)
			}
			errObj, _ := payload["error"].(map[string]any)
			if errObj == nil || errObj["type"] != "model_not_available" {
				t.Fatalf("error.type 应原样保留: %v", payload)
			}
			msg, _ := errObj["message"].(string)
			if !strings.Contains(msg, "可能已下架") || !strings.Contains(msg, "GLM-5.4") {
				t.Fatalf("message 应保留确定性不可用口径: %s", msg)
			}
			details, _ := errObj["details"].(map[string]any)
			if details == nil || details["model"] != "GLM-5.3" {
				t.Fatalf("details 应原样透传: %v", errObj)
			}
			if snapshots, _ := details["snapshot_accounts"].(float64); snapshots != 1 {
				t.Fatalf("details.snapshot_accounts 应为 1: %v", details)
			}
			suggested, _ := details["suggested_models"].([]any)
			if len(suggested) != 2 || suggested[0] != "glm-5.3-flash" || suggested[1] != "GLM-5.4" {
				t.Fatalf("suggested_models 不符: %v", details["suggested_models"])
			}
			f.mu.Lock()
			calls := len(f.calls)
			f.mu.Unlock()
			if calls != 0 {
				t.Fatalf("确定性不可用不得消耗上游调用: %d", calls)
			}
		})
	}
}
