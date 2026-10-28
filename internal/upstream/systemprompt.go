package upstream

import (
	"maps"
	"reflect"
	"slices"
	"strings"
	"time"
	_ "time/tzdata"

	"zcode2api/internal/config"
)

// ZCode 官方客户端的系统提示词是**分段拼装**的（见 docs/plan-client-format-parity.md §1.7）：
// 每段带 injectionTarget(system|meta_user) 与 cacheHint(stable|dynamic)，
// 排序固定为 system+stable → system+dynamic → meta_user+*。
//
// 我方注入的等价物有三段，顺序与官方 3.16.3 一致：
//
//	块 0 CLI Prefix        (system+stable)   ← zcode_system.json，固定文本，无 cache
//	块 1 Agent Identity    (system+stable)   ← 同上（含 # Harness / Desktop Context）
//	块 2 Dynamic           (system+dynamic)  ← 本文件按档案生成 Environment 后与静态动态段合并
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

const (
	currentDateMarker = "# currentDate"
	currentDateIntro  = "As you answer the user's questions, you can use the following context:"
	currentDateOutro  = "      IMPORTANT: this context may or may not be relevant to your tasks. You should not respond to this context unless it is highly relevant to your task."
)

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
	return map[string]any{
		"type":          "text",
		"text":          environmentTextFor(p),
		"cache_control": map[string]any{"type": "ephemeral"},
	}
}

func environmentTextFor(p config.ZcodeProfile) string {
	lines := []string{
		"# Environment",
		"You have been invoked in the following environment:",
		"- Primary working directory: " + p.CWD,
		"- Is a git repository: " + envInfoGitRepositoryLine,
		"- Platform: " + p.EnvPlatform(),
		"- Shell: " + p.Shell,
		"- OS Version: " + p.EnvOSVersion(),
	}
	return strings.Join(lines, "\n")
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

// BuildFallbackSystem 组装「网关自行补 system」路径的三块官方形状。
//
// 与 ZcodeSystemBlocks 的区别：这里总是生成 dynamic 块，且不会依赖
// ZCODE_UPSTREAM_INJECT_ENVIRONMENT 的历史开关。body 中存在非官方前缀的
// 调用者 system 时，其文本会并入块 2 末尾，而不是再追加第 4 块；因此
// 第三方 system 合并后仍保持 3 块上限。调用者自带官方前缀时 NormalizeBody
// 会先走保留分支，不会调用本函数。
//
// profile 必须由调用方传入：Environment 与请求头同源，不能在这里另读一次
// 全局档案，否则测试/请求构造期间改配置会出现头体不一致。
func BuildFallbackSystem(body map[string]any, profile config.ZcodeProfile) []any {
	blocks := copyZcodeStaticBlocks()
	dynamic := fallbackDynamicText(profile)
	generated := slices.Clone(blocks)
	generated = append(generated, map[string]any{
		"type":          "text",
		"text":          dynamic,
		"cache_control": map[string]any{"type": "ephemeral"},
	})
	if body != nil {
		if extra := callerSystemText(body["system"], generated); extra != "" {
			dynamic += "\n\n" + extra
		}
	}
	return append(blocks, map[string]any{
		"type":          "text",
		"text":          dynamic,
		"cache_control": map[string]any{"type": "ephemeral"},
	})
}

// fallbackDynamicText 按官方顺序拼接 dynamic 段：
// 两个换行 + beforeEnvironment + 两个换行 + Environment + 两个换行 + afterEnvironment。
func fallbackDynamicText(profile config.ZcodeProfile) string {
	return "\n\n" + zcodeDynamic.BeforeEnvironment +
		"\n\n" + environmentTextFor(profile) +
		"\n\n" + zcodeDynamic.AfterEnvironment
}

// callerSystemText 把第三方 system 压成一段文本，供并入 dynamic 块。
// 生成块自身（例如重复规范化后的 3 块）会被过滤，避免幂等调用越滚越大。
func callerSystemText(raw any, generated []any) string {
	baseDynamic := ""
	if block, ok := generated[len(generated)-1].(map[string]any); ok {
		baseDynamic, _ = block["text"].(string)
	}
	switch value := raw.(type) {
	case string:
		trimmed := strings.TrimSpace(value)
		if trimmed == "" || (baseDynamic != "" && strings.HasPrefix(trimmed, baseDynamic)) {
			return ""
		}
		for _, item := range generated {
			if block, ok := item.(map[string]any); ok && block["text"] == trimmed {
				return ""
			}
		}
		return value
	case []any:
		parts := make([]string, 0, len(value))
		for _, item := range value {
			block, ok := item.(map[string]any)
			if !ok || block["type"] != "text" {
				continue
			}
			skip := false
			for _, standard := range generated {
				if reflect.DeepEqual(block, standard) {
					skip = true
					break
				}
			}
			if skip {
				continue
			}
			text, _ := block["text"].(string)
			trimmed := strings.TrimSpace(text)
			if trimmed == "" || (baseDynamic != "" && strings.HasPrefix(trimmed, baseDynamic)) {
				continue
			}
			parts = append(parts, text)
		}
		return strings.Join(parts, "\n\n")
	default:
		return ""
	}
}

// ClientTimezoneLocation 返回 ZCODE_CLIENT_TIMEZONE 对应的时区。
// 默认 Asia/Shanghai；time/tzdata 已嵌入，Docker 运行镜像无需安装 zoneinfo。
func ClientTimezoneLocation() *time.Location {
	name := strings.TrimSpace(config.ZcodeClientTimezone)
	if name == "" {
		name = "Asia/Shanghai"
	}
	if loc, err := time.LoadLocation(name); err == nil {
		return loc
	}
	if name == "Asia/Shanghai" {
		return time.FixedZone(name, 8*60*60)
	}
	return time.UTC
}

// InjectContextPrefix 把官方 currentDate system-reminder 合并进首条 user 消息。
//
// 只由网关自行补 system 的路径调用；调用者自带官方 system 时不应调用。
// 若首条 user content 是字符串，转为 [currentDateBlock, textBlock]；若已是
// 数组，则在最前面插入 currentDateBlock。不新增独立 user 消息，也不改写
// MCS / 尾部 system 消息。首条 user 已含 # currentDate 时幂等跳过。
func InjectContextPrefix(body map[string]any, now time.Time, loc *time.Location) {
	messages, ok := body["messages"].([]any)
	if !ok {
		return
	}
	for index, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok || message["role"] != "user" {
			continue
		}
		if contentHasCurrentDate(message["content"]) {
			return
		}
		block := currentDateBlock(now, loc)
		cloned := maps.Clone(message)
		switch content := message["content"].(type) {
		case string:
			cloned["content"] = []any{
				block,
				map[string]any{"type": "text", "text": content},
			}
		case []any:
			merged := make([]any, 0, len(content)+1)
			merged = append(merged, block)
			merged = append(merged, content...)
			cloned["content"] = merged
		default:
			return
		}
		next := slices.Clone(messages)
		next[index] = cloned
		body["messages"] = next
		return
	}
}

func currentDateBlock(now time.Time, loc *time.Location) map[string]any {
	if loc == nil {
		loc = ClientTimezoneLocation()
	}
	date := now.In(loc).Format("2006-01-02")
	text := "<system-reminder>\n" + currentDateIntro + "\n" +
		currentDateMarker + "\nToday's date is " + date + ".\n\n" +
		currentDateOutro + "\n</system-reminder>\n"
	return map[string]any{"type": "text", "text": text}
}

func contentHasCurrentDate(content any) bool {
	switch value := content.(type) {
	case string:
		return strings.Contains(value, currentDateMarker)
	case []any:
		for _, raw := range value {
			block, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			text, _ := block["text"].(string)
			if strings.Contains(text, currentDateMarker) {
				return true
			}
		}
	}
	return false
}
