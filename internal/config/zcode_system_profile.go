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
	// ZcodeSystemProfile3171 3.17.1 起的文案（block0 去掉 "coding"、身份段改写）。
	ZcodeSystemProfile3171 ZcodeSystemProfile = "3.17.1"
)

// ZcodeSystemProfileFor 返回指定客户端版本应使用的文案档案。
//
// 目前只有 3.17.1 有独立档案；其它版本一律走 legacy——这与升级前的行为完全一致，
// 未知的更高版本也先回退 legacy，避免「新版本号 + 旧文案」的静默错配扩大。
func ZcodeSystemProfileFor(version string) ZcodeSystemProfile {
	if strings.TrimSpace(version) == "3.17.1" {
		return ZcodeSystemProfile3171
	}
	return ZcodeSystemProfileLegacy
}

// ZcodeSystemProfileKey 以当前伪装的客户端版本选择文案档案。
func ZcodeSystemProfileKey() ZcodeSystemProfile {
	return ZcodeSystemProfileFor(ZcodeClientVersion)
}
