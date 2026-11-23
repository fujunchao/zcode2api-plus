# ZCode 3.19.1 适配方案（P0：独立 3191 档案 + 身份同步）

> 起草：2026-11-23（Asia/Shanghai）。仓库基线：`codex/socks4-local-resolve` / `175bc87`，产品版本 `2.14.2-go`。
> 依据：本机实装客户端 `D:\Programs\ZCode`（3.19.1.10833）+ 上一轮 3.19.1 静态逆向文档
> （`docs/analysis-zcode-v3.19.1-20261122.md`、`docs/analysis-zcode-v3.19.1-project-impact-notes-20261122.md`），
> 本轮对关键数字做了独立复核（见第 1 节）。本方案只描述改造，不改代码。

## 1. 复核结论（本轮独立验证，未依赖上轮脚本输出）

| 检查 | 方法 | 结果 |
|---|---|---|
| 实装版本 | `ZCode.exe` VersionInfo / `out/metadata/build-meta.json` | `3.19.1.10833`；build `60f20117` / `2026-11-20T06:09:36.805Z`；`minCompatibleVersion=3.19.1` |
| 模型接口路径 | 全量正则扫 `app/out`（21,943 个 js，290.5 MB）对比 3.18.2 | 唯一路径字面量 33 → 33，**增删 0** |
| 请求头名 | 同上 | 101 → 101，**增删 0** |
| CLI（`resources/glm/zcode.cjs`）头名 | 同法对比 | 38 → 39，唯一新增 `x-zcode-keep-branch`；该键是 zod→tool JSON-Schema 转换器的注解键（`U6s`），**不上模型请求** |
| body 字段 | snake_case 字面量集合 | `app/out` 增 55 项、删 0；逐项核对全部是本地 IPC 错误码/事件名（`artifact_*`、`turn_*`、`session_*` 等），**无模型请求字段** |
| 回复语言 | 定位 `replyLanguage` 调用链 | 是 config schema 字段 + 新的 `language` context section（`injectionTarget:"system"`），`reply_language` 只出现在遥测事件名；**不是新 HTTP 参数** |
| 能力头/缓存 | 关键字计数 | `mid-conversation-system-2026-04-07` 1→1、`cache_control` 52→52、`anthropic-beta` 5→5、`thinking` 75→75、`tool_use` 141→141 |
| 官方模型目录 | 两份 `zcode-builtin.json` 哈希 | `resources/config/provider` rev 44、`resources/glm/provider` rev 30，两版**逐字节相同** |
| UA 组成 | provider-utils / node 版本字面量 | `4.0.27`、node 24 均未变 |
| 三块 system | 重建文本 + 客户端内定位 | block0 35 B `7779642ea44a4696`（未变）；block1 1,233 B `aac4ba4cc7b555b3`（未变）；block2 **1,595 → 4,564 B** `929856d5f112c283`；Desktop Context 1,100 B `39a1e86c93452c01`（未变） |
| 新第三块来源 | `zcode.cjs` 内 `UKo`/`zKo`/`$Ko` | `# Finishing work`（`finishing_work`，1,304 B）与 `# Memory, notes and feedback`（`memory_notes`，1,712 B）为新增段；`<total_tokens>` 改为 `totalTokensReminderEnabled`（默认 `false`），显式开启时第三块 4,615 B `36458a63d41391f6` |

**一句话结论：协议零变化，唯一真实变化是"默认动态提示词块"变长 2,969 B。** 我们这套「Docker 网关 + 反代 → 本地 agent」的链路不需要改协议，只需要新增一个 3.19.1 文案档案并把默认身份抬上去。

## 2. 为什么不能只改环境变量

当前代码里 `3.19.1` 未登记，`ZcodeSystemProfileForExact` 返回 `(legacy, false)`（`internal/config/zcode_system_profile.go:49`）：

1. 声明版本变 3.19.1，但 system 用 legacy 文案（block0 含 `coding`）→ 版本与文案错配，历史上等价于 405/3012 事故形态（`docs/analysis-zcode-3.17.1-405-incident-20261101.md`）；
2. `UsesUserReminderEnvironment()` 为 false → Environment/Desktop Context 回到 system，不再走首轮 user 的 `<system-reminder>`；
3. 启动只在 `cmd/zcode2api/main.go:211` 打一行黄字告警，不会阻止发请求。

反过来，**保持 3.18.2 不动今天也是安全的**：3.19.1 客户端首两块与 3180 档案逐字节一致，`callerBlocksMatchOfficial`（`internal/upstream/capabilities.go:62`）命中 → 网关原样保留其完整上下文（含新的 4,564 B 第三块）。所以本方案是"跟版本 + 消除错配陷阱"，不是"修故障"。

## 3. 改动清单

| # | 文件 | 改动 | 说明 |
|---|---|---|---|
| 1 | `internal/config/zcode_system_profile.go` | 新增 `ZcodeSystemProfile3191 ZcodeSystemProfile = "3.19.1"` 常量；`ZcodeSystemProfileForExact` 增加 `case "3.19.1": return ZcodeSystemProfile3191, true` | 精确枚举，**不加 `3.19.x` 区间**，不把 3.19.1 别名到 3180 |
| 2 | 同上 `:67` | `UsesUserReminderEnvironment()` 改为 `p == ZcodeSystemProfile3180 \|\| p == ZcodeSystemProfile3191` | 3191 与 3180 同为"环境走 user 提醒"语义 |
| 3 | `internal/upstream/zcode_system_3191.json`（新） | 三块：35 B / 1,233 B / 4,564 B，`desktopContext` 1,100 B | 结构照抄 `zcode_system_3180.json`；block0 无 `cache_control`，block1/2 为 `ephemeral` |
| 4 | `internal/upstream/request.go:35` | `//go:embed zcode_system_3191.json` + 变量 | |
| 5 | `internal/upstream/request.go:61` | `zcodeProfileSource` 增加 3191 分支 | |
| 6 | `internal/upstream/request.go:126` | `knownOfficialTemplates` 的 source 列表加入 3191 JSON | 否则 3191 客户端走回退时旧模板剥离不干净 |
| 7 | `internal/config/config.go:619` | 默认值 `3.18.2` → `3.19.1`，并在注释链补一行 3.19.1 结论 | 版本单一来源，UA/头/查询参数自动跟随 |
| 8 | `docker-compose.yml:58` | `${ZCODE_CLIENT_VERSION:-3.19.1}` | 与默认值成对 |
| 9 | `.env.example:98-100` | 注释示例改 3.19.1；回退示例写 `3.18.2` | |
| 10 | `README.md:56` | 默认版本段改 3.19.1，写清"3180/3191 首两块相同、第三块不同、不可互相别名" | |
| 11 | `internal/config/version_test.go:34` | 期望默认值改 `3.19.1` | 现有硬闸门，不改必红 |

**不用改**：`internal/model`（模型白名单无版本派生）、`internal/claim` / `internal/quota` / `internal/captcha`（`app_version` 全部由常量派生）、`Dockerfile`、`zcode_system_3180.json`、`ZcodeClientAISDKVersion`/`ZcodeClientNodeMajor`（4.0.27 / 24 未变）、OpenAI 兼容层与 Responses 状态语义。

## 4. 回归测试（新增 `internal/upstream/system_profile_3191_test.go`）

1. **字节与指纹**：`ZcodeSystemBlocks()` 在 `3.19.1` 下返回 3 块，`len` 分别 35 / 1,233 / 4,564，`shortSHA16` 分别 `7779642ea44a4696` / `aac4ba4cc7b555b3` / `929856d5f112c283`；block0 无 `cache_control`，block1/2 为 `ephemeral`。
2. **档案隔离**：`3.18.0`/`3.18.2` 仍映射 3180 且第三块 1,595 B；`3.19.1` 映射 3191；`3.19.0`、`3.19` 一律 `(legacy,false)`（防止前缀匹配带进来）。
3. **环境装配**：3191 下 `UsesUserReminderEnvironment()` 为真；`UpstreamInjectEnvironment=true` 也不得追加第 4 块、system 内不得出现 `# Environment`。
4. **Desktop Context**：3191 档案 `zcodeDesktopContext()` 为 1,100 B / `39a1e86c93452c01`。
5. **保留路径**：构造"3180 首两块 + 4,564 B 新第三块"的调用者 system → `ShouldPreserveCallerSystem` 为真，且 `NormalizeBody` 不改写第三块。
6. **回退路径**：第三方 system（首段非官方前缀）→ `BuildFallbackSystem` 仍为 3 块、第三方文本并入 block2 末尾；3182 客户端的三块进入 3191 网关时，旧第三块被 `stripOfficialSystemPrefix` 精确剥离（含"旧块 + 自定义后缀"形状）。
7. **四入口同源**：`/v1/messages`、`/v1/chat/completions`、`/v1/responses`、`/async/v1/messages` 的最终正文与 `X-ZCode-App-Version` 同源（沿用现有表驱动用例扩一个 3191 版本行）。

## 5. 验证与发布

1. 本地只做格式化与 `git diff` 复核，**不跑 `go test`**；推送分支由 GitHub Actions 跑完整 `ci.yml`。
2. 分支 CI 全绿后，用 `probe --dry-run` 在容器内核对 `client_version=3.19.1 / profile=3.19.1 / blocks=3` 与三块 sha16；再做一次隔离账号真机探针（200 且无 3012/405）。
3. 版本号、tag、镜像推送按"发布门禁"流程走，是否发 `2.14.3-go` 由你决定。
4. 部署：生产 `/opt/1panel/apps/local/zcode2api/docker-compose.yml` 当前未显式设 `ZCODE_CLIENT_VERSION`，改镜像 tag 即生效；升级后核对 `docker exec zcode2api env | grep ZCODE_CLIENT_VERSION` 为空（走默认值）。

## 6. 回退

- 运行期秒级回退：compose 里加 `ZCODE_CLIENT_VERSION: "3.18.2"` → `docker compose up -d`（无需换镜像，3180 档案仍在）。
- 若显式设过 `UPSTREAM_USER_AGENT`，同步改 `ZCode/3.18.2`。
- 极端情况再退镜像 tag 到 `2.14.2-go`。

## 7. 风险与明确不做的事

| 风险 | 判断 | 处理 |
|---|---|---|
| 3191 档案字节抄错 1 B | 与 3.17.1 事故同形态（405/3012） | 第 4 节第 1 条用字节 + sha16 双锁；探针不过立即回退 |
| 默认第三块变长 → 回退路径每请求多约 800 token | 只影响"网关自建 system"的第三方 agent；保留路径不变 | 可接受；如在意可先只加映射、暂不抬默认（方案 B） |
| 上游其实已不接受 3.18.2 | 本轮无证据（`minCompatibleVersion` 是客户端自指） | 抬到 3.19.1 只会更安全；真出现 405 再按第 6 节回退 |
| 客户端 300 s 共享冷却 / 登录时钟容错 / 凭据恢复 | 纯客户端行为，不进 HTTP 契约 | **不跟进**，不改网关重试与账号池语义 |
| 文档工具（LibreOffice/Python/Poppler）、design 生图、语音、MCP 异步、子任务后台、技能热更新 | 本地执行器/插件能力 | **不搬进网关**，服务器仍只做请求转换与长流 |

## 8. 待确认

1. 是否按本方案实施（P0 全部，含抬默认到 3.19.1）？还是先只加映射、默认仍留 3.18.2（方案 B，零行为变化）？
2. 实施后是否直接发 `2.14.3-go` 并部署生产，还是先只推分支等 CI？
