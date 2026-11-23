package quota

import (
	"testing"

	"zcode2api/internal/model"
)

// 2026-11-23 起上游把模型身份放进 capabilities，show_name 可能为空且没有 model 字段。
// 旧解析会把 5.3 额度落成通用名 "model"，导致汇总查不到、调度判 absent。
const capabilityPayload = `{"code":0,"data":{
	"plans":[{"plan_id":"start-plan","entitlements":[
		{"entitlement_id":"ent-53","period":"daily","grant_units":3000000},
		{"entitlement_id":"ent-54","period":"daily","grant_units":3000000}]}],
	"balances":[
		{"entitlement_id":"ent-53","show_name":"","capabilities":["model:glm-5.3"],
			"total_units":3000000,"used_units":0,"remaining_units":3000000,"available_units":3000000,
			"period_start":1795363200,"period_end":1795449599,"expires_at":1795449599},
		{"entitlement_id":"ent-54","show_name":"GLM-5.4","capabilities":["model:glm-5.4","vision"],
			"total_units":3000000,"used_units":0,"remaining_units":3000000,"available_units":3000000,
			"period_start":1795363200,"period_end":1795449599,"expires_at":1795449599}]}}`

func TestQuotaParsesModelFromCapabilities(t *testing.T) {
	svc, st, billing := setup(t)
	acc, err := st.AddAccount(model.ProviderZai, "capability", "header.payload.signature")
	if err != nil {
		t.Fatal(err)
	}
	billing.body = capabilityPayload

	svc.FetchQuota(acc)
	got := st.FindAny(acc.ID)
	if got == nil {
		t.Fatal("账号不存在")
	}
	glm53 := got.QuotaEntriesForModel("glm-5.3")
	if len(glm53) != 1 {
		t.Fatalf("应从 capabilities 解析出 glm-5.3 额度列: %v", got.Quota)
	}
	if glm53[0]["model"] != "glm-5.3" || glm53[0]["remaining"] != float64(3000000) {
		t.Fatalf("5.3 额度列不符: %v", glm53[0])
	}
	if glm53[0]["period"] != "daily" {
		t.Fatalf("period 应按 entitlement 关联为 daily: %v", glm53[0])
	}
	glm54 := got.QuotaEntriesForModel("glm-5.4")
	if len(glm54) != 1 || glm54[0]["model"] != "glm-5.4" {
		t.Fatalf("应从 capabilities 解析出 glm-5.4: %v", got.Quota)
	}
	if _, ok := got.Quota["model"]; ok {
		t.Fatalf("不应再落成通用名 model: %v", got.Quota)
	}
	if got.ModelAvailability("glm-5.3") != "available" || got.ModelAvailability("glm-5.4") != "available" {
		t.Fatalf("两个模型都应可调度: 5.3=%s 5.4=%s", got.ModelAvailability("glm-5.3"), got.ModelAvailability("glm-5.4"))
	}
	if got.ModelAvailability("glm-5.2") != "absent" {
		t.Fatalf("快照外的模型仍应 absent: %s", got.ModelAvailability("glm-5.2"))
	}
}

func TestQuotaCapabilitiesFallbacksAndSharedBucket(t *testing.T) {
	svc, st, billing := setup(t)
	acc, err := st.AddAccount(model.ProviderZai, "fallback", "header.payload.signature")
	if err != nil {
		t.Fatal(err)
	}
	// 1) 非 model capability → 回落 show_name；2) 无 capabilities → 回落 model；
	// 3) 都没有 → 占位名 model（调度应为 unknown 而不是 absent）；
	// 4) 多个 model: 共享同一额度桶 → 每个模型各记一列。
	billing.body = `{"code":0,"data":{"plans":[],"balances":[
		{"show_name":"GLM-5.3-Flash","capabilities":["vision"],"total_units":10,"remaining_units":10,"available_units":10},
		{"model":"GLM-5.4","total_units":20,"remaining_units":20,"available_units":20},
		{"total_units":30,"remaining_units":30,"available_units":30},
		{"show_name":"","capabilities":["model:GLM-5.3","model:GLM-5.4"],"total_units":40,"remaining_units":40,"available_units":40}
	]}}`

	svc.FetchQuota(acc)
	got := st.FindAny(acc.ID)
	for _, name := range []string{"GLM-5.3-Flash", "GLM-5.4", "model"} {
		if _, ok := got.Quota[name]; !ok {
			t.Fatalf("缺少额度列 %q: %v", name, got.Quota)
		}
	}
	if len(got.QuotaEntriesForModel("glm-5.3")) != 1 {
		t.Fatalf("共享桶应记入 glm-5.3: %v", got.Quota)
	}
	if got.ModelAvailability("glm-5.3") != "available" {
		t.Fatalf("共享桶下 5.3 应 available: %s", got.ModelAvailability("glm-5.3"))
	}
}
