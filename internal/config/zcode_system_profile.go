package config

import "strings"

// ZcodeSystemProfile 标识「官方 system 文案」的版本档案。
//
// 为什么需要它：上游从 system 第 0 字节校验到身份段末尾，**文案必须与 app_version
// 成对**。3.17.1 把 CLI Prefix 从 "You are ZCode, an interactive coding agent" 改成
// "You are ZCode, an interactive agent"，同时改写身份段首行与 Harness 缩进；
// 只升版本、不换文案会被判 405/3012
// （见 docs/analysis-zcode-3.17.1-405-incident-20261101.md）。
type ZcodeSystemProfile string

const (
	// ZcodeSystemProfileLegacy 3.15.x–3.16.x 共用的文案（block0 含 "coding"）。
	ZcodeSystemProfileLegacy ZcodeSystemProfile = "3.16.x"
	// ZcodeSystemProfile3171 3.17.x 共用的文案（block0 去掉 "coding"、身份段改写）。
	// 3.17.2 的 CLI 文案与 3.17.1 逐字节相同（2026-11-03 逆向核验：block0 35 B /
	// sha16 7779642e…，身份段 1235 B / sha16 4058874023c7541f），故两个版本共用同一档案。
	ZcodeSystemProfile3171 ZcodeSystemProfile = "3.17.1"
)

// ZcodeSystemProfileFor 返回指定客户端版本应使用的文案档案。
//
// 映射**必须是精确枚举**：未知版本一律回落 legacy——与升级前的行为完全一致，
// 避免「新版本号 + 旧文案」的静默错配扩大。不要改成区间/语义化比较，那会把
// 「新版本静默复用旧文案」制度化，一旦某版真的改了文案就会静默发错且无人告警。
func ZcodeSystemProfileFor(version string) ZcodeSystemProfile {
	profile, _ := ZcodeSystemProfileForExact(version)
	return profile
}

// ZcodeSystemProfileForExact 同 ZcodeSystemProfileFor，但额外返回「是否命中精确映射」。
//
// 未命中意味着在用一个**没有为它核验过文案**的版本号发请求，调用方应据此告警
// （静默回落正是 3.17.1 事故里最难发现的一环）。已知版本必须逐条列入，
// 包括仍走 legacy 的 3.15.3 / 3.16.2 / 3.16.3——它们的档案同样经过核验。
func ZcodeSystemProfileForExact(version string) (ZcodeSystemProfile, bool) {
	switch strings.TrimSpace(version) {
	case "3.17.1", "3.17.2":
		return ZcodeSystemProfile3171, true
	case "3.15.3", "3.16.2", "3.16.3":
		return ZcodeSystemProfileLegacy, true
	}
	return ZcodeSystemProfileLegacy, false
}

// ZcodeSystemProfileKey 以当前伪装的客户端版本选择文案档案。
func ZcodeSystemProfileKey() ZcodeSystemProfile {
	return ZcodeSystemProfileFor(ZcodeClientVersion)
}
