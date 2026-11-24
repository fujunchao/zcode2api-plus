// async 路径的「模型确定性不可用」口径：池内没有任何账号持有请求模型、且确实刷新过
// 额度时，no_account 事件的 message 必须点明「可能已下架」并给出替代模型。
//
// 背景：2026-11-24 生产核实，80 个 zai/jwt（Start Plan）账号的额度快照只剩
// glm-5.3-flash 与 glm-5.4。async 已经先回了 200 SSE，改不了 HTTP 状态，只能在
// 事件里给出与 sync 400 model_not_available 同口径的文案与 details。
package asyncpool

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"zcode2api/internal/model"
)

func TestAsyncNoAccountReportsDeterministicModelUnavailable(t *testing.T) {
	p, st, _, _ := newTestPool(t)
	for i, name := range []string{"acc-a", "acc-b"} {
		acc := addJWTAccountN(t, st, name, i+1)
		if _, err := st.Update(model.ProviderZai, acc.ID, func(a *model.Account) {
			a.Quota = map[string]map[string]any{
				"套餐-1": {"model": "glm-5.3-flash", "remaining": float64(100), "available": float64(100)},
				"套餐-2": {"model": "GLM-5.4", "remaining": float64(100), "available": float64(100)},
			}
		}); err != nil {
			t.Fatal(err)
		}
	}

	tk := insertTicket(p, "ticket-model-gone", map[string]any{"model": "GLM-5.3", "messages": []any{}})
	p.processTicket(context.Background(), "ticket-model-gone")

	var errObj map[string]any
	for _, ev := range drainEvents(tk) {
		if ev.Type != "error" {
			continue
		}
		body, _ := ev.Data.(map[string]any)
		if e, _ := body["error"].(map[string]any); e != nil && e["type"] == "no_account" {
			errObj = e
		}
	}
	if errObj == nil {
		t.Fatal("应投递 no_account 错误事件")
	}
	msg, _ := errObj["message"].(string)
	for _, want := range []string{"可能已下架", "請改用", "glm-5.3-flash", "GLM-5.4"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message 缺少 %q: %s", want, msg)
		}
	}
	details, _ := errObj["details"].(map[string]any)
	if details == nil {
		t.Fatalf("no_account 事件应带 details: %v", errObj)
	}
	if details["model"] != "GLM-5.3" {
		t.Fatalf("details.model 应为请求模型: %v", details)
	}
	if got := eventInt(details["active_accounts"]); got != 2 {
		t.Fatalf("details.active_accounts 应为 2: %v", details)
	}
	if got := eventInt(details["snapshot_accounts"]); got != 2 {
		t.Fatalf("details.snapshot_accounts 应为 2: %v", details)
	}
	if got := modelUnavailableCount(details["model_unavailable"], "absent"); got != 2 {
		t.Fatalf("model_unavailable.absent 应为 2: %v", details)
	}
	suggested, _ := details["suggested_models"].([]string)
	if !reflect.DeepEqual(suggested, []string{"glm-5.3-flash", "GLM-5.4"}) {
		t.Fatalf("suggested_models 应为池内其他有权益模型: %v", details["suggested_models"])
	}
}

// modelUnavailableCount 读取 model_unavailable 的某一类计数：事件 Data 未做 JSON
// 往返时它是 map[string]int，序列化后是 map[string]any，两种形态都要认。
func modelUnavailableCount(raw any, kind string) int {
	switch counts := raw.(type) {
	case map[string]int:
		return counts[kind]
	case map[string]any:
		return eventInt(counts[kind])
	}
	return -1
}

// eventInt 读取事件 details 里的整数：事件 Data 未做 JSON 往返，数值保持 int；
// 兼容未来改为序列化传递后的 float64。
func eventInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	return -1
}
