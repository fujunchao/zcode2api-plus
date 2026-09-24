package upstream

import (
	"strings"

	"zcode2api/internal/config"
)

// ZCode 官方客户端的系统提示词是**分段拼装**的（见 docs/plan-client-format-parity.md §1.7）：
// 每段带 injectionTarget(system|meta_user) 与 cacheHint(stable|dynamic)，
// 排序固定为 system+stable → system+dynamic → meta_user+*。
//
// 我方注入的等价物有三段，顺序与官方一致：
//
//	块 0 CLI Prefix        (system+stable)   ← zcode_system.json，固定文本
//	块 1 Agent Identity    (system+stable)   ← 同上（含 # Harness）
//	块 2 Environment Info  (system+dynamic)  ← 本文件生成
//
// 块 2 必须**运行时生成**而不是写死：官方客户端的 # Environment 与请求头
// 同源于 (platform, arch, os.release())，写死就会让 body 与请求头声称不同的
// 平台——而这是上游可以直接交叉比对的破绽。真实案例：写死的 "Platform: linux-x64"
// 与请求头的 win32-x64 长期并存。

// envInfoGitRepositoryLine # Environment 段里 `Is a git repository` 的取值。
//
// 恒为 "no"，刻意：官方在 cwd 是 git 仓库时会**额外**注入 System Context 段
// （分支 / 主分支 / git user / status 快照 / 近期提交）。只声称 yes 而不给这些
// 附属段，比直接声称 no 更像伪造。
const envInfoGitRepositoryLine = "no"

// environmentBlock 按当前伪装档案生成 # Environment 段。
func environmentBlock() map[string]any {
	return environmentBlockFor(config.ZcodeClientProfile())
}

// environmentBlockFor 生成 # Environment 段（可注入档案，便于测试）。
//
// 模板逐行对齐官方 3.14.3 的 buildEnvInfoContent：单换行、无空行，
// 标签集为 Primary working directory / Is a git repository / Platform /
// Shell / OS Version。
//
// 刻意**不含**官方的可选行 `- You are powered by the model named <provider>/<model>`：
// 我方对下游客户端透传的 provider/model 与官方客户端自身的取值不一定一致，
// 注入了反而是新的矛盾点。
//
// cache_control 沿用既往的 ephemeral：本块在网关里是**跨请求稳定**的
// （CWD 是固定伪装值，平台三元组固定），因此可缓存；这也保持与 v2.6.0 及之前
// 的缓存行为一致。官方把 env_info 标为 dynamic，若日后要严格对齐再单独评估。
func environmentBlockFor(p config.ZcodeProfile) map[string]any {
	lines := []string{
		"# Environment",
		"You have been invoked in the following environment:",
		"- Primary working directory: " + p.CWD,
		"- Is a git repository: " + envInfoGitRepositoryLine,
		"- Platform: " + p.EnvPlatform(),
		"- Shell: " + p.Shell,
		"- OS Version: " + p.EnvOSVersion(),
	}
	return map[string]any{
		"type":          "text",
		"text":          strings.Join(lines, "\n"),
		"cache_control": map[string]any{"type": "ephemeral"},
	}
}

// appendEnvironmentBlock 在静态段之后追加运行时生成的 # Environment 段。
//
// 每次都返回**新切片**：调用方（gateway/body.go）会把它并入请求体的 system，
// 共享切片一旦被下游 append 就地修改，就会污染后续请求——这是返回新切片的
// 唯一理由，与性能无关（三段长度固定，开销可忽略）。
func appendEnvironmentBlock(static []any) []any {
	blocks := make([]any, 0, len(static)+1)
	blocks = append(blocks, static...)
	blocks = append(blocks, environmentBlock())
	return blocks
}
