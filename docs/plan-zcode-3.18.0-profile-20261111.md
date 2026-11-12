# ZCode 3.18.0 客户端身份升级实施计划

> 日期：2026-11-11（Asia/Shanghai）。基线：`go-rewrite` / `c05a02b596fae07560cf3bc4d2df9c8233617109`。工作分支：`codex/zcode-3.18.0-profile`。
> 上游分析：[ZCode 3.18.0 完整逆向](analysis-zcode-v3.18.0-20261111.md)、[项目影响与适配清单](analysis-zcode-v3.18.0-project-impact-notes-20261111.md)。
> 本计划只描述实现顺序与验收口径；本轮不改产品版本、不发版，除非用户另行确认。

## 1. 目标

把网关声明的 ZCode 客户端身份从 **3.17.2** 升级到 **3.18.0**，并保持与官方客户端一致的请求形状：

1. 新增 3180 system 文案档案：block0 35 B / block1 1,233 B / block2 1,595 B，三块逐字节可复核。
2. 3.18.0 的 Environment / Desktop Context 改走普通 `role=user` 的 `<system-reminder>`，不再拼进 system。
3. 版本、档案、请求头、查询参数、Compose 默认值与文档保持同一已核验版本。
4. 保留 3.17.2 / 3.17.1 / 3.16.x 档案与回退能力，任何一步都可以用环境变量退回。
5. 顺带修复两项与新版语义相关的 P1：验证码配置完整性、OpenAI 流对 MCP 事件的条件性中断。

## 2. 非目标（本轮明确不做）

- **不接入 `zai_cn` 中国区登录**：这是新增账号来源，不是 3.18.0 身份升级的前置；需要时单独立项。
- **不模拟每轮 `total_tokens` 用户提醒**：新版新增，但不在已确认的 system 校验边界内；不伪造固定 15,000,000。
- **不实现 Session 管理、Worktree、Design、浏览器、语音、分享等桌面能力**：均由调用端执行。
- **不实现原生 MCP/内建工具的服务器端执行**：只做流投影的防御性兼容，不把上游已执行的工具转成下游函数调用。
- **不新增或修改官方模型目录**：`builtinProviderModelRules` 30 条未变，新增 `gpt-6.1-terra` 只是第三方 BYO-key 模板。

## 3. 现状与差距

| 触点 | 现状 | 目标 |
|---|---|---|
| `internal/config/config.go:613` | 默认 `3.17.2` | 默认 `3.18.0`，可用 `ZCODE_CLIENT_VERSION=3.17.2` 回退 |
| `internal/config/zcode_system_profile.go:38-45` | 只精确登记 3.17.1/3.17.2；3.18.0 回落 legacy | 新增 `ZcodeSystemProfile3180` 并精确映射 |
| `internal/upstream/zcode_system_3171.json` | block1 = 身份 1,233 B + 两换行 + Desktop Context 1,100 B | 保留；3180 使用新文件 |
| `internal/upstream/request.go:29,51` | 只 embed 两个档案 | embed 3180 档案并按档案选择 |
| `internal/upstream/systemprompt.go:103` | 回退路径固定生成「两块静态 + 含 Environment 的 dynamic」 | 3180 使用「三块静态 + 无 Environment」，环境改用户提醒 |
| `internal/gateway/body.go:69-70` | 回退后只注入 currentDate | 3180 额外注入首轮环境提醒 |
| `internal/upstream/system_profile.go:13` | 身份边界用 `# ZCode Desktop Context` | 3180 无该标记，身份段取整块 1,233 B |
| `docker-compose.yml:58` | 默认 `3.17.2` | 默认 `3.18.0` |
| `.env.example` / `README.md` | 描述 3.17.2 | 同步 3.18.0 与回退说明 |
| `internal/captcha/captcha.go:259-309` | 只校验 captcha 对象存在 | 校验业务 envelope、显式 null、主动验证码必需字段 |
| `internal/openai/stream.go:202-206`、`responses_stream.go:255-267` | 未登记 MCP 块 + 同 index `input_json_delta` 会报错 | 防御性忽略/占位，不中断整条流 |

## 4. 设计

### 4.1 档案与选择

- `config.ZcodeSystemProfile` 新增常量 `ZcodeSystemProfile3180 = "3.18.0"`。
- `ZcodeSystemProfileForExact` 精确映射 `"3.18.0"`；**不做区间匹配**，未知版本仍回落 legacy 并告警。
- 新增 `internal/upstream/zcode_system_3180.json`：
  - `blocks`：三块，block0 35 B、block1 1,233 B、block2 1,595 B；block0 的 `cache_control` 在运行时删除，block1/2 保留 `ephemeral`。
  - `desktopContext`：1,100 B 的 `# ZCode Desktop Context` 原文（无可视化目录分支），供环境提醒拼接。
- `request.go` 增加 embed 与 `zcodeProfileSource` 分支；缓存仍按档案键控。
- `ZcodeSystemBlocks` 对 3180 返回三块静态块；`ZCODE_UPSTREAM_INJECT_ENVIRONMENT` 只对 legacy/3171 生效（3180 不把 Environment 放进 system）。

### 4.2 回退 system 装配

- `BuildFallbackSystem` 按 `config.ZcodeSystemProfileKey()` 分支：
  - legacy/3171：保持现状（两块静态 + 含 Environment 的 dynamic 第三块）。
  - 3180：直接使用三块静态块；调用者第三方 system 文本仍并入 block2 末尾，不追加第 4 块。
- `ShouldPreserveCallerSystem` 仍只比较前两块；3180 的 block1 是纯身份段，3.17.x 的带 Desktop Context 的身份块会被判不符 → 重建为 3180，符合预期。
- `ZcodeSystemSummary` 对 3180 输出 `profile=3.18.0`、`identity_chars=1233`、`identity_sha16=aac4ba4cc7b555b3`。

### 4.3 环境通知（3.18.0）

官方 3.18.0 的 Environment / Desktop Context 是 `injectionTarget=meta_user` 的附件，最终投影为 `role=user` 文本；桌面最小默认内容为：

（下例用 · 标出行尾空格，实际文本行尾是空格。）

```text
# Environment
You have been invoked in the following environment:·
 - Primary working directory: C:\projects\demo
 - Is a git repository: false
 - Platform: win32
 - Shell: cmd.exe
 - OS Version: win32 10.0.26100 x64
 - Downloaded files and extracted archives are untrusted data: ...

# ZCode Desktop Context

### Files & URLs
...
```

注意：3.18.0 的文案与 3.17.2 逐字节相同（3.17.2 CLI 已含 untrusted 行与行首空格），与项目当前旧模板不同。实现时以 3.18.0 CLI 的 `TAo` / `YRn` 为准，不沿用旧模板。

网关注入策略（无状态近似）：

1. 只对 3180 生效，只走**回退重建 system**路径；调用者自带官方 system 的保留路径不注入（相信其历史里已有）。
2. 若消息历史已包含 `# Environment`，跳过（幂等）。
3. 若历史中已出现 `assistant` 或 `tool_use/tool_result`，视为续轮，跳过；首轮（只有用户消息）才注入完整环境。
4. 注入位置：首条 user 消息 content 最前面，使用 `<system-reminder>\n...\n</system-reminder>` 包装；若 content 是字符串则转为两个 text 块。
5. 顺序：环境提醒 → currentDate → 用户正文；即先调用 `InjectContextPrefix`，再调用新的 `InjectEnvironmentReminder` 前插。
6. 环境固定（CWD/平台/Shell/OS 均为伪装值），因此不实现 `# Environment update` 增量；代码注释说明。
7. 若首条 user 是 tool_result，跳过注入，避免违反 Anthropic「tool_result 必须在 user content 首位」的约束。

### 4.4 版本与部署

- `config.ZcodeClientVersion` 默认改 `3.18.0`；`ZcodeClientProfile.AppVersion`、模型 UA、`X-ZCode-App-Version`、查询参数 `app_version` 自动跟随。
- `docker-compose.yml`、`.env.example`、`README.md` 同步默认值与回退说明（`ZCODE_CLIENT_VERSION=3.17.2`）。
- `ZcodeClientProfile` 注释里的版本示例同步；`internal/config/version_test.go` 的「默认必须有精确档案」断言改 3.18.0。
- 产品版本 `AppVersion` 与发版号不在本计划内自动修改；如需发版，候选 `2.14.0-go`，由用户确认。

### 4.5 验证码配置完整性（P1）

`fetchConfigHTTP` 增加与 3.18.0 `readValidatedRequestVerificationConfig` 对齐的校验：

- 业务 envelope：`code` 存在且非 0、`success` 显式为 false → 返回错误。
- `captcha` 必须存在且为对象；`enabled` 必须是显式布尔（显式 null / 类型错误 → 错误）。
- 当 `enabled=true` 时，`prefix` / `region` / `sceneId` 必须是非空、非纯空白的字符串。
- `skip_model_request` 必须是布尔（缺失视为 false）。
- 失败不得覆盖 `lastGoodConfig`；无 last-good 时仍回退 `DefaultConfig`，保留 30 秒退避。
- 保留合法 `enabled=false` 的最小配置（允许字段为空）。

### 4.6 OpenAI 流防御（P1，条件性）

不改原生 `/v1/messages` 字节透传，只在 OpenAI 投影层避免「MCP 起始块未登记 → 参数增量报错」：

- Chat/Responses 的 `content_block_start` 遇到 `mcp_tool_use`、`server_tool_use`、`mcp_tool_result` 时登记占位块（或至少记录 index），后续同 index 的 `input_json_delta` 不再返回「未声明内容块」，而是忽略/丢弃。
- 普通 text/thinking/function tool_use 行为不变。
- 用合成 SSE 测试覆盖：MCP start + input_json_delta、孤儿 result、普通 text 混排。
- 不实现 MCP 结果到 OpenAI 工具调用的完整投影；原生通道继续优先。

## 5. 阶段计划

每个阶段独立提交、推送工作分支，以该提交 SHA 的 GitHub Actions 全绿为阶段门禁；本地不跑 `go test`。

### Phase 0：基线与产物

- 创建分支 `codex/zcode-3.18.0-profile`（已完成）。
- 固化 3.18.0 档案生成脚本/产物：`zcode_system_3180.json`、桌面上下文、预期 SHA16 断言。
- 记录基线：`git rev-parse HEAD`、3.17.2 现有测试全绿（远端）。
- 交付：本计划文档 + 生成脚本（放 `temp/`，不进提交）或 JSON 本身。

**验收**：档案块字节数/ SHA16 与逆向产物一致；不改变任何运行时行为（默认仍 3.17.2）。

### Phase 1：档案与选择器

- 改 `zcode_system_profile.go`、`request.go`、新增 JSON。
- 测试：`zcode_system_profile_test.go` 加 3180 精确映射；`system_profile_test.go` 加 3180 指纹（1,233 B / `aac4ba4c…`）；切换缓存测试。
- 保留 3171/legacy 断言。

**验收**：显式 `ZCODE_CLIENT_VERSION=3.18.0` 时 `ZcodeSystemBlocks()` 返回三块且指纹正确；3.17.2/3.16.3 不变。

### Phase 2：回退装配与环境提醒

- `BuildFallbackSystem` 分支；新增 `InjectEnvironmentReminder`；`gateway/body.go` 调用。
- 测试：3180 回退三块、第三方 system 并入 block2、环境不在 system；环境提醒在首条 user、顺序、幂等、续轮跳过、保留路径不注入；3171 行为不变。
- 更新 `body_test.go`、`client_context_test.go`、`preserve_rebuild_test.go`、`systemprompt_test.go`、`desktop_fallback_test.go` 中假定两块/含 Environment 的断言。

**验收**：3.18.0 回退请求 system 仅三块、无 `# Environment`；首条 user 含环境 reminder；3.17.2 回退行为逐字节不变。

### Phase 3：默认版本与部署

- 改 `config.go`、`profile.go` 注释、`docker-compose.yml`、`.env.example`、`README.md`、`version_test.go`、`version_env_test.go`。
- 测试：默认 3.18.0 精确档案；`ZCODE_CLIENT_VERSION=3.17.2` 回退 3171；UA/版本头跟随。

**验收**：默认请求 `X-ZCode-App-Version=3.18.0`、模型 UA `ZCode/3.18.0 ai-sdk/...`；回退后全部回到 3.17.2 形态。

### Phase 4：验证码完整性

- 改 `internal/captcha/captcha.go`，补 `captcha_test.go` 用例。
- 覆盖：非零 code、success=false、显式 null、缺字段、空白字段、合法 disabled/skip、last-good 不被污染。

**验收**：非法配置不写 `lastGoodConfig`；合法配置行为不变；现有测试全绿。

### Phase 5：流防御

- 改 `internal/openai/stream.go`、`responses_stream.go`，补合成 SSE 测试。
- 覆盖：MCP 起始块 + input_json_delta 不报错、后续 text 正常、普通 function 工具不受影响。

**验收**：新增测试通过；现有 Chat/Responses 工具流测试不变红。

### Phase 6：完整 CI 与真实探针

- 推送分支，`gh run watch` 等完整 CI（Go 测试、竞态、重复测试、前端、Windows 专项、Docker 构建等）。
- 失败读日志修复，不跳过断言。
- 用独立授权、隔离账号做单账号 `probe`：3.18.0 身份返回 200，无 3012；对比 3.17.2 回退。
- 观察领取/额度链路是否仍正常；记录结果。
- 发版决策：用户确认后再打版本标签；`release.yml` 复用完整 CI。

## 6. 测试矩阵

| 档案 | 默认 system | 回退 system | 环境提醒 | 保留路径 |
|---|---|---|---|---|
| 3.16.3 legacy | 2 块（无 Environment） | 3 块，含旧 Environment | 无 | 需块0+块1匹配 |
| 3.17.2 / 3171 | 2 块（无 Environment） | 3 块，含旧 Environment | 无 | 需旧 1,235 B 身份块 |
| 3.18.0 / 3180 | 3 块（block2 1,595 B） | 3 块，无 Environment | 首轮 user reminder | 需新 1,233 B 身份块 |

额外组合：
- 3.18.0 + 调用者 3.17.x 官方块 → 判不符 → 重建 3180。
- 3.18.0 + 调用者 3180 块 + 额外块 → 保留。
- 3.18.0 + 空 system / 字符串 system / 消息内 system → 各自既有语义不变。
- 3.18.0 + 首轮/续轮/已有环境提醒/首条 tool_result。

## 7. 风险与回退

| 风险 | 处理 |
|---|---|
| 档案字节错 1 B → 405/3012 | 阶段 1 用 SHA16 断言；探针失败立即 `ZCODE_CLIENT_VERSION=3.17.2` 回退 |
| 默认切换后旧测试假定两块 | 阶段 0 先列出，阶段 2/3 按档案 pin 版本；不删除断言 |
| 环境提醒位置不符合上游预期 | 只影响 user 文本，不影响 system 校验；探针观察；可开关关闭 |
| 验证码校验过严导致合法配置被拒 | 保留 disabled/skip 最小配置用例；失败不覆盖 last-good；30 秒退避 |
| 流防御改变正常工具流 | 只对 MCP/server 类型生效；现有 function 工具测试必须全绿 |
| 新版本 app_version 触发风控/投放变化 | 单账号探针 + 观察领取；必要时回退 |

## 8. 验收口径

1. 当前提交 SHA 的完整 GitHub Actions 全绿（不以本地或旧 SHA 代替）。
2. `probe` 使用 3.18.0 身份返回 200，无 3012；回退 3.17.2 同样 200。
3. 默认请求的 `X-ZCode-App-Version`、模型 UA、system 三块指纹、Docker 环境变量一致。
4. 现有 3.17.2/3.16.x 档案测试与回退路径全绿。
5. 文档、Compose、`.env.example`、README 与代码默认值一致。
6. 未读取真实账号数据、未把真实凭据交给 CI；探针使用隔离授权。

## 9. 待确认决策

1. **默认版本切换时机**：完成 Phase 1-2 后直接切默认（推荐），还是先只支持显式 `ZCODE_CLIENT_VERSION=3.18.0` 灰度、下个版本再切？
2. **范围**：本轮是否包含 Phase 4（验证码）与 Phase 5（流防御）？建议包含；`zai_cn` 登录不包含。
3. **发版**：完成后是否按 `2.14.0-go` 发版，还是只推工作分支等真实探针结果？

## 10. 实施记录（2026-11-11）

按推荐方案执行：Phase 1-5 全部完成，默认版本已切 3.18.0，服务器单账号探针已通过，未发版，`zai_cn` 未做。

| 阶段 | 状态 | 提交 / 证据 |
|---|---|---|
| Phase 1 档案与选择器 | 完成 | `7544eaa`；`zcode_system_3180.json` block0/1/2 = 35 / 1,233 / 1,595 B |
| Phase 2 回退装配与环境提醒 | 完成 | `7544eaa`；3180 三块 system、首轮 user 环境提醒、3.17.x 路径不变 |
| Phase 3 默认版本与部署 | 完成 | `2f58b18`；`ZCODE_CLIENT_VERSION=3.18.0`、Compose/.env/README 同步 |
| Phase 4 验证码完整性 | 完成 | `7c42a6e`；envelope/captcha/enabled/skip/三字段校验，last-good 保护 |
| Phase 5 流事件防御 | 完成 | `7c42a6e`；Chat/Responses 登记不投影块、丢弃同 index 参数增量 |
| Phase 6 完整 CI | 完成 | `b2c8454` → run `43031596400`；`1c86204` → run `43032612395`，14/14 全绿 |
| 真实单账号探针 | **已执行，通过** | 2026-11-11 22:04 服务器测试镜像；3.18.0 返回 200 / 无 3012，3.17.2 回退同账号 200；见 [验证报告](verify-3.18.0-probe-20261111.md) |
| 发版 | **未执行** | 等探针结果与版本号确认 |

实现与计划的差异：

- `enabled` 按 3.18.0 客户端**实际**语义实现为“可选布尔、缺失按启用处理”，而不是计划最初写的“必须显式”；显式 null/非布尔仍拒绝。
- 环境是网关固定伪装值，不存在变化，因此只实现首轮完整环境提醒，未实现 `# Environment update` 增量。
- 未模拟每轮 `total_tokens` 提醒；未接入 `zai_cn`；未实现原生 MCP 执行。

CI 日志已直接显示默认请求 `system_text_bytes=[35,1233,1595]` 且三项 SHA256 与档案一致；服务器探针进一步确认上游接受 3.18.0 完整装配。分支已合并 `go-rewrite` 并发版 **v2.14.0-go**；下一步部署决策。

### 10.1 复审修复（2026-11-12）

外部审核先后提出 5 项（首轮 3 项 + 追加 2 项 P2），逐项判定并修复：

| 审核项 | 判定 | 修复 |
|---|---|---|
| P2 续轮需保留基础环境提醒（`systemprompt.go:204-205`） | **真实 bug**：`historyIsContinuation` 假设“首轮环境提醒已持久化”，但网关是无状态的，调用方历史里没有网关注入的附件；续轮直接 return 会让模型从第二轮起看不到环境 | 删除该跳过；续轮仍向首条 user 补环境提醒。已有环境提醒（本地 ZCode 客户端自带）或首条 user 是 tool_result 时仍跳过 |
| P2 重建时精确剥离旧官方模板（`systemprompt.go:138`） | **真实 bug/边界**：3.16/3.17 的 Prefix/Identity 进入 3.18 回退时会被 `callerSystemText` 当作第三方指令并入 block2，旧 Desktop Context/身份段会漂进新提示词 | 新增三档案已知 Prefix/Identity/块文本集合；仅当首两块确为官方前缀+身份时，剥离连续已知官方块；调用者自定义块保留 |
| P2 旧动态第三块仍被拼回（`request.go:130`） | **真实 bug**：旧档案只有两块静态 Blocks；`dynamicSections` 与运行时 Environment 不在集合里，旧完整三块会整体并入 3180 block2 | 登记旧档案 dynamic 模板；按 beforeEnvironment/Environment/afterEnvironment 结构识别，只保留 afterEnvironment 之后的自定义后缀；补完整三块+自定义后缀测试 |
| P2 独立 Environment 第三块未剥离（`systemprompt.go:183`） | **真实 bug**：`UpstreamInjectEnvironment=true` 时旧 `ZcodeSystemBlocks()` 第三块是裸 `# Environment`，没有 before/after 包裹，原逻辑无法识别 | 新增独立环境结构识别（到 `- OS Version:` 行），保留其后自定义；补该配置分支回归 |
| P3 探针输出示例层级（`runbook:24-28`） | **文档错误**：`probe --dry-run` 的指纹在 `system` 对象内，示例写成了真实请求报告的顶层字段 | runbook 改为嵌套示例，并注明真实请求报告才是顶层字段 |

修复提交：`26364c6`（续轮/旧静态）、`a084f67`（旧动态第三块）、`1d3ee26`（旧独立环境第三块）、`4f8caee`（runbook）。对应 CI run `43136344414`、`43137713157`、`43138434119` 均 **14/14 全绿**。服务器测试镜像已按 `1d3ee26` 重建（digest `ff558988…`），首轮探针指纹不变。

### 10.2 发布（2026-11-12）

- **v2.14.0-go 已发布**：tag `v2.14.0-go` → 提交 `9c63748`；release run `43140173198` success；Release 5 个二进制资产上传完成。
- GHCR：`2.14.0-go` 与 `latest` 指向同一 multi-arch digest `sha256:a58116e22384669a6b027d9cf69ed5717e143d72017683f26c90eb2d38d7846a`（linux/amd64 + linux/arm64）。
- 生产已于 2026-11-12 10:28 部署 `2.14.0-go`：容器 healthy、`/meta` 返回 `2.14.0-go`，probe dry-run 显示 `client_version=3.18.0`；旧 `2.13.6-go` 镜像保留，compose 备份 `docker-compose.yml.bak_pre2140_20261112_102743`。
- 回退：恢复备份 compose 后 `docker compose up -d`，或设置 `ZCODE_CLIENT_VERSION=3.17.2`。
