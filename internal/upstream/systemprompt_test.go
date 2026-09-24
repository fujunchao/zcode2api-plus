// 系统提示词块的结构与「头体一致」守卫。
//
// 背景：曾经 zcode_system.json 里写死了 `Platform: linux-x64` 与三处 unknown，
// 而请求头与其他端点声称 win32-x64 —— 同一个账号对同一上游同时说两套身份。
// 本文件盯住的正是这个回归：Environment 段必须由伪装档案生成，且不得再写死。
package upstream

import (
	"strings"
	"testing"

	"zcode2api/internal/config"
)

// wantEnvText 按给定档案生成期望文本（手写常量，不复用被测代码）。
func wantEnvText(cwd, platform, shell, osVersion string) string {
	return strings.Join([]string{
		"# Environment",
		"You have been invoked in the following environment:",
		"- Primary working directory: " + cwd,
		"- Is a git repository: no",
		"- Platform: " + platform,
		"- Shell: " + shell,
		"- OS Version: " + osVersion,
	}, "\n")
}

// TestEnvironmentBlockTemplate 钉住 # Environment 段的模板：
// 逐行对齐官方 3.14.3 的 buildEnvInfoContent（单换行、无空行、五条标签）。
func TestEnvironmentBlockTemplate(t *testing.T) {
	p := config.ZcodeProfile{
		BarePlatform: "win32",
		Arch:         "x64",
		OSRelease:    "10.0.26100",
		Shell:        "cmd.exe",
		CWD:          `C:\projects\demo`,
	}
	block := environmentBlockFor(p)
	if block["type"] != "text" {
		t.Fatalf("块类型应为 text: %v", block["type"])
	}
	want := wantEnvText(`C:\projects\demo`, "win32", "cmd.exe", "win32 10.0.26100 x64")
	if got := block["text"]; got != want {
		t.Fatalf("Environment 段不符:\n got=%q\nwant=%q", got, want)
	}
	if cc, ok := block["cache_control"].(map[string]any); !ok || cc["type"] != "ephemeral" {
		t.Fatalf("cache_control 应保持 ephemeral: %v", block["cache_control"])
	}
}

// TestEnvironmentBlockUsesBarePlatform 段的 Platform 行必须是**裸平台名**：
// 官方此处填 process.platform（win32），填成复合形态 win32-x64 就是伪造痕迹。
func TestEnvironmentBlockUsesBarePlatform(t *testing.T) {
	p := config.ZcodeProfile{BarePlatform: "win32", Arch: "x64", OSRelease: "10.0.26100"}
	text, _ := environmentBlockFor(p)["text"].(string)
	if !strings.Contains(text, "- Platform: win32\n") {
		t.Fatalf("Platform 行应为裸平台名 win32:\n%s", text)
	}
	if strings.Contains(text, "Platform: win32-x64") {
		t.Fatalf("Platform 行不得是复合形态:\n%s", text)
	}
}

// TestEnvironmentBlockNoPlaceholders 回归守卫：禁止再出现写死的 unknown / linux
// 之类与伪装身份矛盾的占位值。
func TestEnvironmentBlockNoPlaceholders(t *testing.T) {
	text, _ := environmentBlockFor(config.ZcodeClientProfile())["text"].(string)
	for _, bad := range []string{"unknown", "linux", "linux-x64", "win32-x64"} {
		if strings.Contains(text, bad) {
			t.Fatalf("Environment 段含可疑占位/矛盾值 %q:\n%s", bad, text)
		}
	}
}

// TestEnvironmentBlockTracksProfile 段内容必须跟随配置变化（不做缓存）。
func TestEnvironmentBlockTracksProfile(t *testing.T) {
	old := config.ZcodeClientBarePlatform
	t.Cleanup(func() { config.ZcodeClientBarePlatform = old })

	config.ZcodeClientBarePlatform = "darwin"
	text, _ := environmentBlockFor(config.ZcodeClientProfile())["text"].(string)
	if !strings.Contains(text, "- Platform: darwin\n") {
		t.Fatalf("档案改动未反映到 Environment 段:\n%s", text)
	}
}

// TestSystemBlocksShape 三段结构与顺序：CLI Prefix → Agent Identity → Environment。
// 顺序与官方的分段排序一致（system+stable 先于 system+dynamic），
// 否则前缀缓存命中会变差。
func TestSystemBlocksShape(t *testing.T) {
	blocks := ZcodeSystemBlocks()
	if len(blocks) != 3 {
		t.Fatalf("应注入 3 段，实际 %d", len(blocks))
	}
	wantPrefix := []string{
		"You are ZCode, an interactive coding agent",
		"\nYou are an interactive ZCode agent",
		"# Environment",
	}
	for i, want := range wantPrefix {
		m, ok := blocks[i].(map[string]any)
		if !ok {
			t.Fatalf("第 %d 段不是对象: %T", i, blocks[i])
		}
		if m["type"] != "text" {
			t.Fatalf("第 %d 段类型应为 text: %v", i, m["type"])
		}
		text, _ := m["text"].(string)
		if !strings.HasPrefix(text, want) {
			t.Fatalf("第 %d 段前缀不符：\n got=%q\nwant前缀=%q", i, text[:min(60, len(text))], want)
		}
		if _, ok := m["cache_control"]; !ok {
			t.Fatalf("第 %d 段缺 cache_control", i)
		}
	}
}

// TestSystemBlocksReturnFreshSlice 每次调用必须返回新切片。
// 调用方（gateway/body.go）会把它并入请求体并可能就地 append，
// 共享切片会让一个请求污染后续所有请求。
func TestSystemBlocksReturnFreshSlice(t *testing.T) {
	first := ZcodeSystemBlocks()
	first[0] = map[string]any{"type": "text", "text": "污染"}

	second := ZcodeSystemBlocks()
	m, ok := second[0].(map[string]any)
	if !ok || m["text"] != "You are ZCode, an interactive coding agent" {
		t.Fatalf("共享切片被污染，第二次调用拿到 %v", m)
	}
}

// TestEmbeddedSystemJSONHasNoEnvBlock 静态守卫：zcode_system.json 里不得再出现
// # Environment —— 它必须由 systemprompt.go 按档案生成。若有人图省事把段写回
// JSON，本用例直接变红。
func TestEmbeddedSystemJSONHasNoEnvBlock(t *testing.T) {
	if strings.Contains(string(zcodeSystemJSON), "# Environment") {
		t.Fatal("zcode_system.json 不得内置 # Environment 段：该段必须由伪装档案生成，" +
			"否则 body 与请求头会声称不同平台")
	}
}

// TestIdentityBlockMatchesInstalledClient # Harness 的措辞必须与已实装的客户端版本一致。
//
// 3.14.3 的第 3 条是「The system may send updates, reminders, or modifications to rules
// via mid-conversation system turns…」；更早的版本是「`<system-reminder>` tags …
// injected by the harness」。提示词文案本身就是版本指纹：留着旧措辞，等于一边声称
// X-ZCode-App-Version=3.14.3、一边交出一份 3.14.3 永远不会产生的 system。
func TestIdentityBlockMatchesInstalledClient(t *testing.T) {
	blocks := ZcodeSystemBlocks()
	if len(blocks) < 2 {
		t.Fatalf("应为 3 段，实得 %d", len(blocks))
	}
	m, _ := blocks[1].(map[string]any)
	text, _ := m["text"].(string)

	if !strings.Contains(text, "via mid-conversation system turns") {
		t.Errorf("# Harness 第 3 条应为 3.14.3 的措辞（via mid-conversation system turns）：\n%s", text)
	}
	if strings.Contains(text, "<system-reminder>") {
		t.Errorf("# Harness 仍含旧版措辞（3.14.3 已替换）：\n%s", text)
	}
	// 另外两条 3.14.3 也带的 bullet，顺手钉住存在性。
	for _, want := range []string{
		"# Harness",
		"Github-flavored markdown in a terminal",
		"user-selected permission mode",
		"dedicated file/search tools",
		"file_path:line_number",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("# Harness 缺少 %q：\n%s", want, text)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
