// 伪装客户端档案：官方 ZCode 客户端的请求格式是**一个三元组的纯函数**。
//
// 官方 3.14.3 的 detectEnvInfo 只从三项取值：
//
//	platform  = process.platform          → win32（裸平台名，不带架构）
//	arch      = process.arch              → x64
//	osRelease = os.release()              → 10.0.26100
//
// 请求头与 body 里 # Environment 段的平台信息全部由它派生：
//
//	X-Platform        = platform-arch                     → win32-x64
//	X-Os-Category     = darwin→macos / win32→windows / 其余 linux
//	X-Os-Version      = osRelease                         → 10.0.26100
//	env Platform      = platform                          → win32
//	env OS Version    = `${platform} ${osRelease} ${arch}` → win32 10.0.26100 x64
//
// 所以官方客户端**结构上不可能**出现「请求头说 Windows、body 说 Linux」这类矛盾。
// 本文件是上述派生关系的唯一实现处；其它包只许调用 ZcodeClientProfile()，
// 不许再写平台 / 架构 / 内核串 / 语言 / 时区字面量——否则同一个账号会向上游
// 同时声称两个身份，而这正是上游可以交叉比对的破绽。静态守卫见
// profile_literals_test.go。
//
// 特别注意**两种平台串形态不可混用**（官方本身就是两种）：
//
//	请求头 X-Platform  = `${platform}-${arch}`      → win32-x64
//	查询参数 platform= = win32→windows、x64→x86_64  → windows-x86_64
//
// 历史上这两处共用一个常量，是「一个值两种格式」的潜在错误来源。
//
// 背景与完整规格见 docs/plan-client-format-parity.md。
package config

import "strings"

// 伪装档案的可配置项。默认值刻意**不跟随运行环境**：网关多跑在 Linux 容器里，
// 而伪装身份声称桌面客户端，跟随环境会让请求头与 body 自相矛盾。环境变量名
// 保持向后兼容（旧部署只设过 ZCODE_CLIENT_PLATFORM 时行为不变，见 config.go）。
var (
	// ZcodeClientBarePlatform 官方 process.platform 形态（裸平台名，不带架构）。
	ZcodeClientBarePlatform = env("ZCODE_CLIENT_BARE_PLATFORM", "win32")
	// ZcodeClientArch 官方 process.arch 形态。
	ZcodeClientArch = env("ZCODE_CLIENT_ARCH", "x64")
	// ZcodeClientShell 官方 basename($SHELL ?? $ComSpec)；Windows 下即 cmd.exe。
	ZcodeClientShell = env("ZCODE_CLIENT_SHELL", "cmd.exe")
	// ZcodeClientCWD # Environment 段里的工作目录。真实客户端报用户真实工程目录，
	// 这里是伪装值：取一个与桌面身份自洽、且不含任何本机信息的绝对路径即可。
	ZcodeClientCWD = env("ZCODE_CLIENT_CWD", `C:\projects\demo`)
	// ZcodeClientSourceTitle 官方 X-Title 的后半段：cli（命令行）或 electron（桌面端）。
	ZcodeClientSourceTitle = env("ZCODE_CLIENT_SOURCE_TITLE", "electron")
	// ZcodeClientReleaseChannel 官方 X-Release-Channel；生产环境恒为 production
	// （客户端按 ZCODE_ENV 判定：非 test 即 production）。
	ZcodeClientReleaseChannel = env("ZCODE_CLIENT_RELEASE_CHANNEL", "production")
	// ZcodeClientAISDKVersion 官方模型请求 User-Agent 里的 ai-sdk/provider-utils 版本。
	// 2026-09-24 由 golden 抓包实测（见 docs/analysis-client-golden-diff-20260924.md）。
	ZcodeClientAISDKVersion = env("ZCODE_CLIENT_AISDK_VERSION", "4.0.27")
	// ZcodeClientNodeMajor 官方模型请求 User-Agent 里的 runtime/node.js 主版本。
	// 取自实测抓包的运行时（客户端由 Electron 内置 Node 启动）。
	ZcodeClientNodeMajor = env("ZCODE_CLIENT_NODE_MAJOR", "22")
)

// ZcodeProfile 伪装身份的运行时快照。
type ZcodeProfile struct {
	BarePlatform   string // win32
	Arch           string // x64
	OSRelease      string // 10.0.26100
	Shell          string // cmd.exe
	CWD            string // 伪装工作目录
	AppVersion     string // 3.14.3
	SourceTitle    string // electron | cli
	ReleaseChannel string // production
	Language       string // zh-CN
	Timezone       string // Asia/Shanghai
	AISDKVersion   string // 4.0.27
	NodeMajor      string // 22
}

// ZcodeClientProfile 按当前配置组装档案。
//
// 刻意每次读包级变量、不做 sync.Once 缓存：后台「系統設定」与测试夹具都会改这些
// 变量，缓存会让改动静默不生效（此类 bug 表现为「改了配置但请求没变」，
// 排查成本远高于这里的几次字符串读取）。
func ZcodeClientProfile() ZcodeProfile {
	return ZcodeProfile{
		BarePlatform:   ZcodeClientBarePlatform,
		Arch:           ZcodeClientArch,
		OSRelease:      ZcodeClientOSVersion,
		Shell:          ZcodeClientShell,
		CWD:            ZcodeClientCWD,
		AppVersion:     ZcodeClientVersion,
		SourceTitle:    ZcodeClientSourceTitle,
		ReleaseChannel: ZcodeClientReleaseChannel,
		Language:       ZcodeClientLanguage,
		Timezone:       ZcodeClientTimezone,
		AISDKVersion:   ZcodeClientAISDKVersion,
		NodeMajor:      ZcodeClientNodeMajor,
	}
}

// Platform 请求头 X-Platform 的取值：`${platform}-${arch}`，如 win32-x64。
// 架构为空时退化为裸平台名，不产出带尾连字符的畸形值。
func (p ZcodeProfile) Platform() string {
	if p.Arch == "" {
		return p.BarePlatform
	}
	return p.BarePlatform + "-" + p.Arch
}

// OSCategory 请求头 X-Os-Category 的取值：darwin→macos / win32→windows / 其余 linux。
func (p ZcodeProfile) OSCategory() string {
	switch p.BarePlatform {
	case "darwin":
		return "macos"
	case "win32":
		return "windows"
	default:
		return "linux"
	}
}

// QueryPlatform 查询参数 platform= 的取值。**与 Platform() 不是同一格式**：
// 官方把 win32 写成 windows、x64 写成 x86_64、arm64 写成 aarch64。
func (p ZcodeProfile) QueryPlatform() string {
	plat := p.BarePlatform
	if plat == "win32" {
		plat = "windows"
	}
	arch := p.Arch
	switch arch {
	case "x64":
		arch = "x86_64"
	case "arm64":
		arch = "aarch64"
	}
	if arch == "" {
		return plat
	}
	return plat + "-" + arch
}

// EnvPlatform # Environment 段的 Platform 行：裸平台名，不是复合形态。
// 官方此处填 process.platform（win32），填成 win32-x64 就是伪造痕迹。
func (p ZcodeProfile) EnvPlatform() string { return p.BarePlatform }

// EnvOSVersion # Environment 段的 OS Version 行：`${platform} ${release} ${arch}`。
func (p ZcodeProfile) EnvOSVersion() string {
	return strings.Join([]string{p.BarePlatform, p.OSRelease, p.Arch}, " ")
}

// Title 请求头 X-Title 的取值：`Z Code@${sourceTitle}`。
func (p ZcodeProfile) Title() string { return "Z Code@" + p.SourceTitle }

// ModelUserAgent 模型请求的 User-Agent。
//
// 与 `UserAgent`（裸 `ZCode/<版本>`）不同：模型请求由 ai-sdk 发出，UA 是 ai-sdk 拼的，
// 实测形态为
//
//	ZCode/0.16.9 ai-sdk/provider-utils/4.0.27 runtime/node.js/22
//
// 而非模型接口（`/api/v1/agent/configs`、额度、领取）用的是裸串——所以这两处**必须分开**，
// 用一个常量两处用会留下「模型请求少了 ai-sdk 后缀」这个可被上游识别的差异。
func (p ZcodeProfile) ModelUserAgent() string {
	return "ZCode/" + p.AppVersion + " ai-sdk/provider-utils/" + p.AISDKVersion +
		" runtime/node.js/" + p.NodeMajor
}
