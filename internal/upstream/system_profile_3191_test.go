package upstream

import (
	"encoding/json"
	"maps"
	"reflect"
	"strings"
	"testing"

	"zcode2api/internal/config"
)

// Test3191SystemBlocksBytes 锁死 3.19.1 三块 system 的字节数与指纹。
//
// 上游按 app_version 校验「第 0 字节到身份段末尾」，差一个字节就会 405/3012
// （见 docs/analysis-zcode-3.17.1-405-incident-20261101.md）。3.19.1 的首两块与
// 3.18.2 逐字节相同，但 dynamic 块由 1,595 B 增至 4,564 B，因此必须是独立档案。
func Test3191SystemBlocksBytes(t *testing.T) {
	withSystemVersion(t, "3.19.1")
	oldInject := config.UpstreamInjectEnvironment
	config.UpstreamInjectEnvironment = false
	t.Cleanup(func() { config.UpstreamInjectEnvironment = oldInject })

	blocks := ZcodeSystemBlocks()
	if len(blocks) != 3 {
		t.Fatalf("3.19.1 静态 system 应为 3 块，实际 %d", len(blocks))
	}
	want := []struct {
		bytes  int
		sha16  string
		prefix string
	}{
		{35, "7779642ea44a4696", "You are ZCode, an interactive agent"},
		{1233, "aac4ba4cc7b555b3", "\nYou are an agent working with the user toward their goals"},
		{4564, "929856d5f112c283", "\n\nWrite code that reads like the surrounding code"},
	}
	for i, tc := range want {
		text := blockText(t, blocks, i)
		if len(text) != tc.bytes {
			t.Fatalf("第 %d 块字节数 = %d，期望 %d", i, len(text), tc.bytes)
		}
		if got := shortSHA16(text); got != tc.sha16 {
			t.Fatalf("第 %d 块 sha16 = %s，期望 %s", i, got, tc.sha16)
		}
		if !strings.HasPrefix(text, tc.prefix) {
			t.Fatalf("第 %d 块前缀不符: %q", i, text[:min(60, len(text))])
		}
		if i == 0 {
			if _, ok := blocks[i].(map[string]any)["cache_control"]; ok {
				t.Fatal("block 0 不得带 cache_control")
			}
			continue
		}
		cache, _ := blocks[i].(map[string]any)["cache_control"].(map[string]any)
		if cache["type"] != "ephemeral" {
			t.Fatalf("block %d 应保留 ephemeral", i)
		}
	}

	// 新版第三块必须含两段新增指令；这是它与 3180 档案的实质差异。
	dynamic := blockText(t, blocks, 2)
	for _, section := range []string{"# Finishing work", "# Memory, notes and feedback"} {
		if !strings.Contains(dynamic, section) {
			t.Fatalf("3.19.1 block2 缺少新增段 %q", section)
		}
	}

	// 3.19.1 的 Environment 走首轮 user 提醒，旧开关也不得把它追加成第 4 块。
	config.UpstreamInjectEnvironment = true
	blocks = ZcodeSystemBlocks()
	if len(blocks) != 3 {
		t.Fatalf("3191 下旧环境开关不得追加第 4 块，实际 %d 块", len(blocks))
	}
	for i := range blocks {
		if strings.Contains(blockText(t, blocks, i), "# Environment") {
			t.Fatalf("第 %d 块仍含 # Environment", i)
		}
	}
}

// Test3191ProfileIsolation 锁死「3.19.1 精确映射到 3191，且不与 3180 混用」。
func Test3191ProfileIsolation(t *testing.T) {
	profile, exact := config.ZcodeSystemProfileForExact("3.19.1")
	if !exact || profile != config.ZcodeSystemProfile3191 {
		t.Fatalf("3.19.1 映射 = (%q, %v)，期望 (%q, true)", profile, exact, config.ZcodeSystemProfile3191)
	}
	if !profile.UsesUserReminderEnvironment() {
		t.Fatal("3.19.1 必须走「Environment 作为 user 提醒」的装配")
	}
	// 3180 / 3182 的映射与语义不得被本次改动带偏。
	for _, version := range []string{"3.18.0", "3.18.2"} {
		p, ok := config.ZcodeSystemProfileForExact(version)
		if !ok || p != config.ZcodeSystemProfile3180 || !p.UsesUserReminderEnvironment() {
			t.Fatalf("%s 映射 = (%q, %v)，期望 (%q, true)", version, p, ok, config.ZcodeSystemProfile3180)
		}
	}
	// 未核验的相邻版本必须回落 legacy 且 exact=false，不能被前缀匹配带进来。
	for _, version := range []string{"3.19", "3.19.0", "3.19.2"} {
		p, ok := config.ZcodeSystemProfileForExact(version)
		if ok || p != config.ZcodeSystemProfileLegacy {
			t.Fatalf("%s 应回落 legacy 且 exact=false，实际 (%q, %v)", version, p, ok)
		}
	}

	// 两个档案的第三块必须不同：一旦有人把 3.19.1 别名到 3180，这里直接变红。
	withSystemVersion(t, "3.18.2")
	oldDynamic := blockText(t, ZcodeSystemBlocks(), 2)
	withSystemVersion(t, "3.19.1")
	newDynamic := blockText(t, ZcodeSystemBlocks(), 2)
	if oldDynamic == newDynamic {
		t.Fatal("3.19.1 与 3.18.2 的 dynamic 块不应相同")
	}
	if len(newDynamic) != 4564 || len(oldDynamic) != 1595 {
		t.Fatalf("dynamic 块长度 = %d / %d，期望 4564 / 1595", len(newDynamic), len(oldDynamic))
	}
}

// Test3191DesktopContext 3.19.1 复用 3.18.0 的桌面上下文原文（1,100 B）。
func Test3191DesktopContext(t *testing.T) {
	withSystemVersion(t, "3.19.1")
	text := zcodeDesktopContext()
	if len(text) != 1100 {
		t.Fatalf("桌面上下文 = %d B，期望 1100", len(text))
	}
	if shortSHA16(text) != "39a1e86c93452c01" {
		t.Fatalf("桌面上下文 sha16 = %s", shortSHA16(text))
	}
}

// Test3191PreservesCallerSystem 3.19.1 客户端带着完整上下文进来时必须原样保留。
//
// 保留判定只比首两块（上游校验范围到身份段末尾），3.19.1 与 3.18.2 首两块相同，
// 因此两个版本的客户端在 3191 网关上都不应被重建。
func Test3191PreservesCallerSystem(t *testing.T) {
	callerBlocks := func(t *testing.T) []any {
		t.Helper()
		blocks := ZcodeSystemBlocks()
		out := make([]any, 0, len(blocks))
		for i, raw := range blocks {
			block := raw.(map[string]any)
			clone := map[string]any{"type": "text", "text": block["text"]}
			if i > 0 {
				clone["cache_control"] = map[string]any{"type": "ephemeral"}
			}
			out = append(out, clone)
		}
		return out
	}

	withSystemVersion(t, "3.18.2")
	oldClient := callerBlocks(t)
	withSystemVersion(t, "3.19.1")
	newClient := callerBlocks(t)

	for name, system := range map[string][]any{"3.18.2 客户端": oldClient, "3.19.1 客户端": newClient} {
		t.Run(name, func(t *testing.T) {
			body := map[string]any{"system": system}
			if !ShouldPreserveCallerSystem(body) {
				t.Fatal("首两块与当前档案一致时应原样保留调用者上下文")
			}
		})
	}

	// 首块被改成第三方文本时不得保留（否则上游 3012）。
	third := []any{map[string]any{"type": "text", "text": "第三方 harness 指令"}, newClient[1]}
	if ShouldPreserveCallerSystem(map[string]any{"system": third}) {
		t.Fatal("首块非官方前缀时不应保留")
	}
}

// Test3191FallbackStripsKnownTemplates 3182 / 3191 客户端的三块官方模板进入 3191
// 网关回退路径时，旧身份段与旧 dynamic 块必须被精确剥离，只保留调用者自定义后缀。
func Test3191FallbackStripsKnownTemplates(t *testing.T) {
	oldInject := config.UpstreamInjectEnvironment
	config.UpstreamInjectEnvironment = false
	t.Cleanup(func() { config.UpstreamInjectEnvironment = oldInject })

	withSystemVersion(t, "3.18.2")
	oldBlocks := ZcodeSystemBlocks()
	oldDynamic := blockText(t, oldBlocks, 2)
	oldIdentity := blockText(t, oldBlocks, 1)
	withSystemVersion(t, "3.19.1")
	newBlocks := ZcodeSystemBlocks()
	newDynamic := blockText(t, newBlocks, 2)

	custom := map[string]any{"type": "text", "text": "调用者自定义指令"}
	oldWithSuffix := maps.Clone(oldBlocks[2].(map[string]any))
	oldWithSuffix["text"] = oldDynamic + "\n\n调用者自定义指令"
	newWithSuffix := maps.Clone(newBlocks[2].(map[string]any))
	newWithSuffix["text"] = newDynamic + "\n\n调用者自定义指令"
	cases := []struct {
		name       string
		system     []any
		wantCustom bool
		forbidden  []string
	}{
		{"3182 完整三块", []any{oldBlocks[0], oldBlocks[1], oldBlocks[2]}, false, []string{oldDynamic, oldIdentity, "# Environment"}},
		{"3182 三块+独立自定义块", []any{oldBlocks[0], oldBlocks[1], oldBlocks[2], custom}, true, []string{oldDynamic, oldIdentity}},
		{"3182 第三块内自定义后缀", []any{oldBlocks[0], oldBlocks[1], oldWithSuffix}, true, []string{oldDynamic, oldIdentity}},
		{"3191 完整三块", []any{newBlocks[0], newBlocks[1], newBlocks[2]}, false, []string{"# Environment"}},
		{"3191 第三块内自定义后缀", []any{newBlocks[0], newBlocks[1], newWithSuffix}, true, []string{oldIdentity, "# Environment"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blocks := BuildFallbackSystem(map[string]any{"system": tc.system}, config.ZcodeClientProfile())
			if len(blocks) != 3 {
				t.Fatalf("回退应保持 3 块，实际 %d", len(blocks))
			}
			dynamic := blockText(t, blocks, 2)
			for _, forbidden := range tc.forbidden {
				if strings.Contains(dynamic, forbidden) {
					t.Fatalf("新 block2 不应包含旧模板 %q: %q", forbidden, dynamic[max(0, len(dynamic)-160):])
				}
			}
			if tc.wantCustom {
				if dynamic != newDynamic+"\n\n调用者自定义指令" {
					t.Fatalf("回退应只保留新版底座和自定义后缀，实际 %d B", len(dynamic))
				}
				return
			}
			if strings.Contains(dynamic, "调用者自定义指令") {
				t.Fatal("无自定义后缀时不应凭空出现自定义文本")
			}
			if dynamic != newDynamic {
				t.Fatalf("纯官方模板回退后应与 3191 默认第三块一致，实际 %d B", len(dynamic))
			}
		})
	}
}

// TestFallbackStripsStaticDynamicSuffixAcrossProfiles 静态底座的块内后缀在新旧档案
// 双向切换时都应保留，独立块的顺序、后缀空白和调用者对象不得被改写。
func TestFallbackStripsStaticDynamicSuffixAcrossProfiles(t *testing.T) {
	const suffix = "\n# Context management\n调用者自定义指令\n\n"
	const standalone = "# Memory, notes and feedback\n独立追加指令"
	versions := []string{"3.18.0", "3.18.2", "3.19.1"}
	for _, sourceVersion := range versions {
		for _, targetVersion := range versions {
			t.Run(sourceVersion+"->"+targetVersion, func(t *testing.T) {
				withSystemVersion(t, sourceVersion)
				caller := ZcodeSystemBlocks()
				third := caller[2].(map[string]any)
				third["text"] = blockText(t, caller, 2) + "\n\n" + suffix
				third["caller_metadata"] = "原始块不能被修改"
				caller = append(caller, map[string]any{"type": "text", "text": standalone})
				before, err := json.Marshal(caller)
				if err != nil {
					t.Fatal(err)
				}

				withSystemVersion(t, targetVersion)
				base := blockText(t, ZcodeSystemBlocks(), 2)
				blocks := BuildFallbackSystem(map[string]any{"system": caller}, config.ZcodeClientProfile())
				if len(blocks) != 3 {
					t.Fatalf("静态档案回退应保持三块，实际 %d", len(blocks))
				}
				if got, want := blockText(t, blocks, 2), base+"\n\n"+suffix+"\n\n"+standalone; got != want {
					t.Fatalf("底座、块内后缀或独立块不符：实际 %d B，期望 %d B", len(got), len(want))
				}
				for i, raw := range blocks {
					block := raw.(map[string]any)
					if i == 0 {
						if _, exists := block["cache_control"]; exists {
							t.Fatal("回退的首块不得带缓存标记")
						}
						continue
					}
					cache, _ := block["cache_control"].(map[string]any)
					if cache["type"] != "ephemeral" {
						t.Fatalf("第 %d 块的缓存布局被改变", i)
					}
				}
				after, err := json.Marshal(caller)
				if err != nil || string(after) != string(before) {
					t.Fatal("回退不得修改调用者持有的原始块")
				}
				repeated := BuildFallbackSystem(map[string]any{"system": blocks}, config.ZcodeClientProfile())
				if !reflect.DeepEqual(repeated, blocks) {
					t.Fatal("重复回退不应叠加底座或改变后缀")
				}
			})
		}
	}
}

// TestStripOfficialStaticDynamicPrefixKeepsCallerText 仅有同名标题或首两块非官方时，
// 不能把调用者文本误判为官方底座后删除。
func TestStripOfficialStaticDynamicPrefixKeepsCallerText(t *testing.T) {
	withSystemVersion(t, "3.18.2")
	blocks := ZcodeSystemBlocks()
	withSuffix := maps.Clone(blocks[2].(map[string]any))
	withSuffix["text"] = blockText(t, blocks, 2) + "\n\n调用者自定义指令"
	custom := map[string]any{"type": "text", "text": "# Context management\n调用者规则\n\n# Finishing work\n调用者收尾规则"}
	foreignPrefix := map[string]any{"type": "text", "text": "第三方前缀"}
	foreignIdentity := map[string]any{"type": "text", "text": "第三方身份"}
	for _, tc := range []struct {
		name   string
		system []any
		want   []any
	}{
		{"同名标题", []any{blocks[0], blocks[1], custom}, []any{custom}},
		{"首块非官方", []any{foreignPrefix, blocks[1], withSuffix}, []any{foreignPrefix, blocks[1], withSuffix}},
		{"身份段非官方", []any{blocks[0], foreignIdentity, withSuffix}, []any{blocks[0], foreignIdentity, withSuffix}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripOfficialSystemPrefix(tc.system); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("不得按标题或未经确认的身份删除调用者文本: %#v", got)
			}
		})
	}
}

// Test3191EnvironmentReminderGoesToUser 3.19.1 的环境提醒仍注入首条 user。
func Test3191EnvironmentReminderGoesToUser(t *testing.T) {
	withSystemVersion(t, "3.19.1")
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "你好"},
		},
	}
	InjectEnvironmentReminder(body, config.ZcodeClientProfile())

	messages := body["messages"].([]any)
	content, ok := messages[0].(map[string]any)["content"].([]any)
	if !ok || len(content) != 2 {
		t.Fatalf("首条 user content 应为两个块，实际 %#v", messages[0])
	}
	reminder, _ := content[0].(map[string]any)["text"].(string)
	if !strings.HasPrefix(reminder, "<system-reminder>") {
		t.Fatalf("环境提醒未包进 system-reminder: %q", reminder[:min(60, len(reminder))])
	}
	if !strings.Contains(reminder, "# Environment") || !strings.Contains(reminder, "# ZCode Desktop Context") {
		t.Fatal("环境提醒应同时包含 # Environment 与桌面上下文")
	}
}

// Test3191AliasFor400 锁死「4.0.0 复用 3191 档案」：默认三块 system 与 Desktop
// Context 必须与 3.19.1 逐字节相同，且相邻的 4.0.x 继续回落 legacy。
//
// 上游按 app_version 校验 system 首段到身份段末尾，若 4.0.0 被错映射到 3180 或
// 复制出第二份会漂移的档案，本用例直接变红；只加版本号不加映射则会静默回落
// legacy（3.17.1 事故同型，见 docs/analysis-zcode-3.17.1-405-incident-20261101.md）。
func Test3191AliasFor400(t *testing.T) {
	profile, exact := config.ZcodeSystemProfileForExact("4.0.0")
	if !exact || profile != config.ZcodeSystemProfile3191 {
		t.Fatalf("4.0.0 映射 = (%q, %v)，期望 (%q, true)", profile, exact, config.ZcodeSystemProfile3191)
	}
	if !profile.UsesUserReminderEnvironment() {
		t.Fatal("4.0.0 必须走「Environment 作为 user 提醒」的装配")
	}
	// 相邻未核验版本不得被前缀匹配带进来。
	for _, version := range []string{"4.0", "4.0.1", "4.0.0.1"} {
		if p, ok := config.ZcodeSystemProfileForExact(version); ok || p != config.ZcodeSystemProfileLegacy {
			t.Fatalf("%s 应回落 legacy 且 exact=false，实际 (%q, %v)", version, p, ok)
		}
	}

	withSystemVersion(t, "3.19.1")
	wantBlocks := ZcodeSystemBlocks()
	wantDesktop := zcodeDesktopContext()

	withSystemVersion(t, "4.0.0")
	gotBlocks := ZcodeSystemBlocks()
	if !reflect.DeepEqual(gotBlocks, wantBlocks) {
		t.Fatal("4.0.0 与 3.19.1 的静态 system 块应逐字节相同")
	}
	if desktop := zcodeDesktopContext(); desktop != wantDesktop {
		t.Fatalf("4.0.0 桌面上下文与 3.19.1 不一致：%d B vs %d B", len(desktop), len(wantDesktop))
	}
	if len(gotBlocks) != 3 {
		t.Fatalf("4.0.0 静态 system 应为 3 块，实际 %d", len(gotBlocks))
	}
	for i, tc := range []struct {
		bytes int
		sha16 string
	}{
		{35, "7779642ea44a4696"},
		{1233, "aac4ba4cc7b555b3"},
		{4564, "929856d5f112c283"},
	} {
		text := blockText(t, gotBlocks, i)
		if len(text) != tc.bytes || shortSHA16(text) != tc.sha16 {
			t.Fatalf("4.0.0 第 %d 块 = %d B / %s，期望 %d B / %s", i, len(text), shortSHA16(text), tc.bytes, tc.sha16)
		}
	}
}

