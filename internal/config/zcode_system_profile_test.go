package config

import (
	"os"
	"strings"
	"testing"
)

func TestZcodeSystemProfileFor(t *testing.T) {
	cases := []struct {
		version string
		want    ZcodeSystemProfile
	}{
		{"3.17.2", ZcodeSystemProfile3171},
		{"3.17.1", ZcodeSystemProfile3171},
		{" 3.17.1 ", ZcodeSystemProfile3171},
		{"3.16.3", ZcodeSystemProfileLegacy},
		{"3.15.3", ZcodeSystemProfileLegacy},
		{"3.18.0", ZcodeSystemProfile3180},
		{"", ZcodeSystemProfileLegacy},
	}
	for _, tc := range cases {
		if got := ZcodeSystemProfileFor(tc.version); got != tc.want {
			t.Fatalf("ZcodeSystemProfileFor(%q) = %q, 期望 %q", tc.version, got, tc.want)
		}
	}
}

// TestZcodeSystemProfileForExact 守住「精确映射」语义：已知版本必须返回 true，
// 未知版本回落 legacy 且 exact=false，供调用方告警。
func TestZcodeSystemProfileForExact(t *testing.T) {
	cases := []struct {
		version string
		want    ZcodeSystemProfile
		exact   bool
	}{
		{"3.17.2", ZcodeSystemProfile3171, true},
		{"3.17.1", ZcodeSystemProfile3171, true},
		{"3.16.3", ZcodeSystemProfileLegacy, true},
		{"3.15.3", ZcodeSystemProfileLegacy, true},
		{"3.18.0", ZcodeSystemProfile3180, true},
		{"3.17.0", ZcodeSystemProfileLegacy, false},
		{"", ZcodeSystemProfileLegacy, false},
	}
	for _, tc := range cases {
		got, exact := ZcodeSystemProfileForExact(tc.version)
		if got != tc.want || exact != tc.exact {
			t.Fatalf("ZcodeSystemProfileForExact(%q) = (%q, %v), 期望 (%q, %v)",
				tc.version, got, exact, tc.want, tc.exact)
		}
	}
}

// TestDefaultClientVersionHasExactSystemProfile 守住默认伪装版本必须有**精确**文案档案。
//
// 版本与文案错配不会让既有测试变红（档案自比永远自洽），也不会在日志里留下明文版本，
// 只在真实流量上表现为 405/3012（见 docs/analysis-zcode-3.17.1-405-incident-20261101.md）。
// 所以在这里显式钉死：默认值一旦升到没核验过文案的版本，本用例立刻失败。
func TestDefaultClientVersionHasExactSystemProfile(t *testing.T) {
	if v := strings.TrimSpace(os.Getenv("ZCODE_CLIENT_VERSION")); v != "" {
		t.Skipf("ZCODE_CLIENT_VERSION=%q 显式覆盖时由部署侧负责版本与文案配对", v)
	}
	if _, exact := ZcodeSystemProfileForExact(ZcodeClientVersion); !exact {
		t.Fatalf("默认客户端版本 %q 没有精确文案档案（会静默回落 legacy），"+
			"请先在 zcode_system_profile.go 里补映射并核验文案", ZcodeClientVersion)
	}
}
