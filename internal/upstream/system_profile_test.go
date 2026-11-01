package upstream

import (
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

	cases := []struct {
		version string
		profile string
		prefix  string
	}{
		{"3.16.3", "3.16.x", "You are ZCode, an interactive coding agent"},
		{"3.17.1", "3.17.1", "You are ZCode, an interactive agent"},
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
			if got := OfficialSystemPrefix(); got != tc.prefix {
				t.Fatalf("OfficialSystemPrefix = %q, 期望 %q", got, tc.prefix)
			}
			blocks := ZcodeSystemBlocks()
			if len(blocks) < 2 {
				t.Fatalf("system 块数 = %d, 期望 ≥2", len(blocks))
			}
			first, _ := blocks[0].(map[string]any)
			if _, ok := first["cache_control"]; ok {
				t.Fatal("block 0 不应带 cache_control")
			}
		})
	}
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
