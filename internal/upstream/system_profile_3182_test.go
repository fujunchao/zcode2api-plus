package upstream

import (
	"encoding/json"
	"testing"

	"zcode2api/internal/config"
)

// Test3182AliasSameAs3180 锁死「3.18.2 复用 3.18.0 档案」这条契约。
//
// 上游按 app_version 校验「system 第 0 字节到身份段末尾」（见
// docs/analysis-zcode-3.17.1-405-incident-20261101.md），所以 3.18.2 的默认三块、
// 动态块与档案级语义必须与 3.18.0 完全一致：一旦有人给 3.18.2 新造档案，或把
// UsesUserReminderEnvironment 的判断改成比较版本字符串，这里会直接变红。
//
// 比较用整体 JSON 快照，避免逐字段列举时漏掉以后新增的字段。
func Test3182AliasSameAs3180(t *testing.T) {
	oldInject := config.UpstreamInjectEnvironment
	config.UpstreamInjectEnvironment = false
	t.Cleanup(func() { config.UpstreamInjectEnvironment = oldInject })

	withSystemVersion(t, "3.18.0")
	baseJSON, err := json.Marshal(ZcodeSystemSummary())
	if err != nil {
		t.Fatalf("序列化 3.18.0 档案摘要失败: %v", err)
	}

	withSystemVersion(t, "3.18.2")
	aliasSummary := ZcodeSystemSummary()
	aliasJSON, err := json.Marshal(aliasSummary)
	if err != nil {
		t.Fatalf("序列化 3.18.2 档案摘要失败: %v", err)
	}

	if string(baseJSON) != string(aliasJSON) {
		t.Fatalf("3.18.2 档案摘要与 3.18.0 不一致：\n3.18.0=%s\n3.18.2=%s", baseJSON, aliasJSON)
	}

	// 映射必须精确命中 3180，而不是静默回落 legacy。
	profile, exact := config.ZcodeSystemProfileForExact("3.18.2")
	if !exact || profile != config.ZcodeSystemProfile3180 {
		t.Fatalf("3.18.2 映射 = (%q, %v)，期望 (%q, true)", profile, exact, config.ZcodeSystemProfile3180)
	}
	// 3.18.2 仍须走「Environment 走 role=user 提醒」的新装配，不能退回旧路径。
	if !profile.UsesUserReminderEnvironment() {
		t.Fatalf("3.18.2 映射到 %q，未启用用户提醒环境装配", profile)
	}
	// 未核验的 3.18.1 不得被前缀匹配带进来。
	if got, ok := config.ZcodeSystemProfileForExact("3.18.1"); ok || got != config.ZcodeSystemProfileLegacy {
		t.Fatalf("3.18.1 应回落 legacy 且 exact=false，实际 (%q, %v)", got, ok)
	}
}
