package upstream

import (
	"strings"
	"testing"

	"zcode2api/internal/config"
)

// 版本与文案必须成对：这是 3.17.1 405 事故的回归测试。
// 上游从 system 第 0 字节校验到身份段末尾，只升版本不换文案必然 3012。
func TestZcodeSystemProfileFollowsClientVersion(t *testing.T) {
	oldVersion, oldUA := config.ZcodeClientVersion, config.UserAgent
	t.Cleanup(func() {
		config.ZcodeClientVersion, config.UserAgent = oldVersion, oldUA
	})

	// identityHead 是身份段首行：3.17.1 在没有 Output Style 时走 dBa 变体
	// （"You are an agent working with the user…"），配错变体会被上游判 3012。
	cases := []struct {
		version       string
		profile       string
		prefix        string
		identityHead  string
		identityChars int
		identitySHA16 string
	}{
		{
			"3.16.3", "3.16.x",
			"You are ZCode, an interactive coding agent",
			"You are an interactive ZCode agent that helps users with software engineering tasks.",
			1217, "fb4777a371fac341",
		},
		{
			// 身份段**字节数**直接锁死（Go 的 len(string) 就是上游口径，含多字节字符）：
			// 2026-11-01 的真实抓包显示上游按 app_version 校验
			// 「system 第 0 字节到身份段末尾」，差 88 字节（模板字面量没展开）就会被判 3012。
			"3.17.1", "3.17.1",
			"You are ZCode, an interactive agent",
			"You are an agent working with the user toward their goals",
			1235, "4058874023c7541f",
		},
	}
	for _, tc := range cases {
		t.Run(tc.version, func(t *testing.T) {
			config.ZcodeClientVersion = tc.version
			config.UserAgent = "ZCode/" + tc.version

			summary := ZcodeSystemSummary()
			if summary.Profile != tc.profile {
				t.Fatalf("档案 = %q, 期望 %q", summary.Profile, tc.profile)
			}
			if summary.FirstText != tc.prefix {
				t.Fatalf("block0 = %q, 期望 %q", summary.FirstText, tc.prefix)
			}
			if summary.FirstChars != len(tc.prefix) {
				t.Fatalf("block0 长度 = %d, 期望 %d", summary.FirstChars, len(tc.prefix))
			}
			if summary.IdentityChars == 0 || summary.IdentitySHA16 == "" {
				t.Fatalf("身份段指纹缺失: %+v", summary)
			}
			if summary.IdentityChars != tc.identityChars {
				t.Fatalf("身份段长度 = %d, 期望 %d（多/少一个字节都会 3012）", summary.IdentityChars, tc.identityChars)
			}
			if summary.IdentitySHA16 != tc.identitySHA16 {
				t.Fatalf("身份段 sha16 = %s, 期望 %s", summary.IdentitySHA16, tc.identitySHA16)
			}
			if got := OfficialSystemPrefix(); got != tc.prefix {
				t.Fatalf("OfficialSystemPrefix = %q, 期望 %q", got, tc.prefix)
			}
			blocks := ZcodeSystemBlocks()
			if len(blocks) < 2 {
				t.Fatalf("system 块数 = %d, 期望 ≥2", len(blocks))
			}
			if head := identityHeadOf(blocks); !strings.HasPrefix(head, tc.identityHead) {
				t.Fatalf("身份段首行 = %q, 期望以 %q 开头", head, tc.identityHead)
			}
			first, _ := blocks[0].(map[string]any)
			if _, ok := first["cache_control"]; ok {
				t.Fatal("block 0 不应带 cache_control")
			}
		})
	}
}

// identityHeadOf 取 block 1 里身份段的第一行（去掉前导空行）。
func identityHeadOf(blocks []any) string {
	if len(blocks) < 2 {
		return ""
	}
	second, _ := blocks[1].(map[string]any)
	body, _ := second["text"].(string)
	lines := strings.Split(strings.TrimPrefix(body, "\n"), "\n")
	if len(lines) == 0 {
		return ""
	}
	return lines[0]
}

// 两个档案必须真的不同：相同就意味着档案没生效（embed 或选择器写错）。
func TestZcodeSystemProfilesDiffer(t *testing.T) {
	oldVersion := config.ZcodeClientVersion
	t.Cleanup(func() { config.ZcodeClientVersion = oldVersion })

	config.ZcodeClientVersion = "3.16.3"
	legacy := ZcodeSystemSummary()
	config.ZcodeClientVersion = "3.17.1"
	current := ZcodeSystemSummary()

	if legacy.FirstSHA16 == current.FirstSHA16 {
		t.Fatal("3.16.x 与 3.17.1 的 block0 指纹相同，档案未生效")
	}
	if legacy.IdentitySHA16 == current.IdentitySHA16 {
		t.Fatal("3.16.x 与 3.17.1 的身份段指纹相同，档案未生效")
	}
}

// 切换版本后必须重新解析：缓存不能把旧文案留给新版本。
func TestZcodeSystemProfileSwitchInvalidatesCache(t *testing.T) {
	oldVersion := config.ZcodeClientVersion
	t.Cleanup(func() { config.ZcodeClientVersion = oldVersion })

	config.ZcodeClientVersion = "3.17.1"
	first := ZcodeSystemSummary().FirstText
	config.ZcodeClientVersion = "3.16.3"
	second := ZcodeSystemSummary().FirstText
	config.ZcodeClientVersion = "3.17.1"
	third := ZcodeSystemSummary().FirstText

	if first != third {
		t.Fatalf("回到 3.17.1 后 block0 = %q, 期望 %q", third, first)
	}
	if first == second {
		t.Fatal("切换版本后仍拿到同一份文案，缓存未失效")
	}
}
