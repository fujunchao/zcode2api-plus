# ZCode 3.16.2 客户端版本同步更新计划

> 日期：2026-10-20（北京时间）。依据：本机 ZCode **3.16.2** 逆向分析（[主报告](analysis-zcode-v3.16.2-20261020.md)、[项目影响笔记](analysis-zcode-v3.16.2-project-impact-notes-20261020.md)）。
> 结论前提：P0 = 0；本次更新只做 **P1 客户端版本声明同步（3.15.3 → 3.16.2）** 与一条配套防御回归，不改模型协议、不改依赖、不发版。
> 项目基线：分支 `go-rewrite`，HEAD `cb95926`（v2.10.30-go）；origin `github.com:fujunchao/zcode2api-plus.git`，upstream 只读。制定本计划时未改任何代码。

## 1. 目标与范围

### 1.1 目标

让项目伪装身份与本机实装客户端一致：默认 `ZcodeClientVersion` 由 `3.15.3` 升为 `3.16.2`；`UserAgent`、`ModelUserAgent()`、`X-ZCode-App-Version`、`app_version` 查询参数全部由该常量自动派生。**请求形态、端点、认证、body 一律不动。**

### 1.2 范围（只改以下 6 个文件）

| # | 文件 | 变更 |
|---|---|---|
| 1 | `internal/config/config.go` | `458` 行默认值 `"3.15.3"` → `"3.16.2"`；注释 451-457 末尾补一行 `2026-10-20 同步本机确认的 3.16.2；模型请求形态与提示词未变` |
| 2 | `internal/config/profile.go` | 仅注释更新：`70` 行 `AppVersion string // 3.15.3` 改 `3.16.2`；`178-180` 行 UA 示例 `ZCode/3.15.3 → ZCode/3.16.2`（`AISDKVersion 4.0.27`、`NodeMajor 24` 不变） |
| 3 | `internal/config/version_test.go` | `TestDefaultClientVersionMatches3153` 改为按版本参数化/更名 `TestDefaultClientVersionMatchesInstalledClient`，期望 `3.16.2` |
| 4 | `internal/config/version_env_test.go` | `19`、`21` 行 `wantVersion/wantUserAgent` 的 `3.15.3` → `3.16.2` |
| 5 | `internal/upstream/headers_test.go` | 在 `TestClientZcodeHeadersOutcome`（约 199-227 行）incoming 中加 `X-ZCode-Authorization: Bearer synthetic-downstream`，断言不出现在 `req.Headers` |
| 6 | `README.md` | `56` 行"默认客户端版本已同步至 3.15.3"→`3.16.2`；`219` 行等历史发布说明**不改** |

### 1.3 明确不做（Out of Scope）

- 不实现 `X-ZCode-Authorization`/`/api/v1/ultra*` 代理路由（仅保留"不透传"防御回归）；
- 不实现工具搜索（`ToolSearch`/`defer_loading`/`tool_reference`）、不迁移远程会话/Source Server/远控 V2/插件市场/托盘/DB 迁移等客户端能力；
- 不支持 MiMo 模板、不改 team plan 登录链、不改 provider 目录获取方式；
- 不改 `ZcodeClientAISDKVersion=4.0.27`、`ZcodeClientNodeMajor=24`（新版证据未变）；
- 不更新 `AppVersion=2.10.30-go`（运行时版本，与发版绑定）；
- **不发版**：不更新版本标签、不触发 `release.yml`、不推送镜像（按 AGENTS.md，仅用户明确要求发版时执行）。

## 2. 前置条件（执行前核对）

1. 分支 `go-rewrite`，HEAD `cb95926`；工作区只有既有未跟踪 docs/temp 与本次新增分析文档，**无已跟踪改动**（执行前再确认 `git status --short | grep -v '^??'` 为空）。
2. 远程 `origin` 可推（@fujunchao，gh 已登录）；upstream 只读。
3. 分析结论已复核：模型请求主链路静态未变（主报告 §5.1），版本同步不会改变请求头集合之外的语义。
4. 本地只做代码修改 + `gofmt` + `git diff`；测试、构建、竞态、交叉编译一律交 GitHub Actions（AGENTS.md）。

## 3. 实施步骤

### Step 0：建分支

`git checkout -b codex/zcode-3.16.2-version-sync`（从 `go-rewrite` HEAD 拉出；codex/ 前缀符合规范）。

### Step 1：Phase A — 版本声明同步（commit 1）

按 §1.2 的 #1/#2/#3/#4/#6 修改；要点：
- 只改默认值与注释/测试期望；**不新增**任何版本字面量到其它生产代码（`TestNoHardcodedClientVersion` 会拦）。
- `profile.go` 的 `AISDKVersion/NodeMajor` 保持 `4.0.27/24`；`ModelUserAgent()` 公式不改。
- `version_env_test.go` 中 `version_override/user_agent_override/both_overrides` 用例继续用 `3.14.5`/自定义 UA（验证覆盖语义），只改 `defaults` 期望。
- 本地 `gofmt -l internal/config internal/upstream` 应为空。
- commit：`chore: 同步 ZCode 3.16.2 客户端版本声明`

### Step 2：Phase B — 防御回归（commit 2，建议但可裁剪）

- 在 `TestClientZcodeHeadersOutcome` 的 incoming 增加 `X-ZCode-Authorization`，断言 `req.Headers` 不含该头；保留原有"未知 x-zcode-* 不透传/官方头照原值上行/固定头必胜"断言。
- 不新增生产逻辑；若认为覆盖已足够可裁剪本 commit，但需在收口说明中注明。
- commit：`test: 固化 X-ZCode-Authorization 不透传回归`

### Step 3：推送并等待 CI

1. `git push -u origin codex/zcode-3.16.2-version-sync`（只推 origin）。
2. `gh run list --branch codex/zcode-3.16.2-version-sync --limit 3` 找到本次 push 的 run，`gh run watch <run-id> --exit-status`。
3. 失败：读对应 job 日志，修复后推送新提交复验；**不得**跳过/删除测试或降覆盖率。
4. 以最终提交 SHA 对应的**完整 CI** 为验收依据（8 个 job 全绿），不引用旧提交成功记录。

### Step 4：记录与收口

- 在本计划与分析文档中记录：默认客户端声明已同步 3.16.2、P1 完成、CI 结果 SHA。
- 是否合入 `go-rewrite`、是否发版由用户另行决定；本计划只做分支验证。

## 4. 验证与验收

### 4.1 本地允许的检查（不跑测试）

- `gofmt -l` 变化文件为空；
- `git diff` 只含预期的 6 个文件（+ ≤2 commit）；
- 静态自查：`grep -rn "3\.15\.3" internal cmd webui.go` 不再命中生产默认值（历史注释/发布说明除外）；
- 不执行 `go test`/`go vet`/`go build`/前端构建（按 AGENTS.md 交 CI）。

### 4.2 CI 验收（GitHub Actions，Ubuntu）

| CI job | 关注点 |
|---|---|
| `workflows` | actionlint |
| `frontend` | 前端静态检查（本计划不改前端） |
| `test` | `go test ./...` + 分组重复回归 + `go test -race -count=1 ./...`；重点版本测试、`TestClientZcodeHeadersOutcome`、`TestNoHardcodedClientVersion` |
| `sdk-compat` / `newapi-compat` | SDK/NewAPI 兼容用例（版本头断言不应受影响） |
| `windows-proxy-journal` | Windows 代理日志用例 |
| `cross-build` | Linux/Windows 交叉编译 |
| `docker` | 镜像构建 |

验收标准：最终 SHA 上 8 个 job 全绿；`claim/quota/captcha/upstream` 中所有 `app_version` 相关契约用例通过。

## 5. 回滚方案

1. **代码回滚**：`git revert <sha>` 或新提交改回 `3.15.3`（本计划仅涉及 6 个文件，风险面小）。
2. **运行期应急**（无需回滚代码）：部署侧设 `ZCODE_CLIENT_VERSION=3.15.3`（`UserAgent`/`X-ZCode-App-Version`/`app_version` 全自动跟随）；若只想回退非模型 UA，可设 `UPSTREAM_USER_AGENT=ZCode/3.15.3`（注意模型请求 UA 由 `ModelUserAgent()` 从常量派生，环境变量 `UPSTREAM_USER_AGENT` 不覆盖模型 UA）。
3. 本计划不发版，不存在已发布二进制需要回收。

## 6. 风险与未证实项

| 风险 | 等级 | 说明与缓解 |
|---|---|---|
| 上游按 `app_version` 分级下发配置 | 中低 | 既有分析表明上游配置按版本分级；3.16.2 可能下发新字段（已知 `uiPluginRecommendations` 等只增字段，项目 captcha 解析会忽略）。若 captcha 语义随版本变化则行为可能改变——上线后观察验证码/领取行为，必要时用环境变量回退。 |
| 服务端风控对版本的灰度 | 低 | 静态证据未见版本硬门槛；换版本是"更像最新官方客户端"方向；观察 401/405 与验证码比例。 |
| 版本测试改名遗漏 | 低 | CI `test` 会拦；`TestNoHardcodedClientVersion` 防漏改。 |
| 潜在行为差异 | 低 | 本次只改字符串；请求头集合/body/端点均未变（主报告 §5.1）。 |

未证实：服务端是否接受/拒绝 3.15.3、3.16.2 的版本分级差异、是否新增业务码——均无法由静态样本证明，验收以 CI 与部署后观察为准。

## 7. 后续观察项（不在本次改动）

1. `X-ZCode-Authorization` + `/api/v1/ultra*`：若上游开始对普通账号下发代理映射，再评估网关是否需感知；当前"未知 `x-zcode*` 不透传"策略保留即可。
2. 工具搜索 native 字段（`anthropic-beta: advanced-tool-use-2025-11-20`、`defer_loading`、`tool_reference`）：仅在未来需支持"官方客户端经网关"时评估转换层。
3. provider 目录远端刷新（`builtin_provider_config_json`）：记录运维观察项。
4. team plan 账号 Key 兑换：既有缺口，建议另立任务。

## 8. 完成定义（DoD）

- [ ] 6 个文件按 §1.2 修改完成，本地 `gofmt` 干净、`git diff` 符合预期；
- [ ] 两个 commit（版本同步 + 防御回归）已推送到 `origin/codex/zcode-3.16.2-version-sync`；
- [ ] 最终提交 SHA 的完整 CI（8 个 job）全绿；失败已修复复验，未删除/跳过测试；
- [ ] 未提交任何未跟踪 docs/temp 文件；未发版、未推 tag、未推送镜像；
- [ ] 分析文档/发布说明记录同步结果；回滚路径已写入本计划。

## 9. 预计耗时

本地改动 + 自查约 10 分钟；CI 一轮约 10-20 分钟（含 race/交叉编译/docker）；如需修复按失败日志追加一轮。

## 10. 执行边界确认

- 只推 `origin`，不碰 `upstream`；
- 不回滚、不删除既有未跟踪文件；不把 docs/temp 纳入提交；
- 不修改 `data/`、`.zcode/`；不读取账号材料；
- 发布门禁按 AGENTS.md：本次不触发 `release.yml`，不创建版本标签。

## 11. 执行与发布记录（2026-10-20，用户追加要求发版 2.11.0）

> 用户在本计划确认后追加「完成后推送并发 2.11.0 版本」，故 §10 的「不发版」被该显式指令覆盖；实际执行结果如下。

### 11.1 代码与 CI

- 分支 `codex/zcode-3.16.2-version-sync`：`ecaa4c5`（版本声明同步）、`1a5b92e`（防御回归）；CI run [39414412701](https://github.com/fujunchao/zcode2api-plus/actions/runs/39414412701) 全绿（12 个 job）。
- `go-rewrite` 快进合并到 `1a5b92e`；发版提交 `5eff10c`（`chore: 发布 v2.11.0-go`）；发布前分支 CI run [39415273282](https://github.com/fujunchao/zcode2api-plus/actions/runs/39415273282) 全绿。
- 标签 `v2.11.0-go`（annotated，tag object `3f5573dee5216652eac7fbd19b42f3d9e7e2e530` → commit `5eff10c3db29c2b3399c2bb25394959e4384ea52`）；远端 `refs/tags/v2.11.0-go` 已确认。

### 11.2 发布流水线

- release run [39416214649](https://github.com/fujunchao/zcode2api-plus/actions/runs/39416214649) 成功：validate（复用 ci.yml 全部 job）全绿；build 矩阵 5/5 成功；image 成功。
- Release：https://github.com/fujunchao/zcode2api-plus/releases/tag/v2.11.0-go（非 draft/prerelease，published 2026-10-20T11:45:10Z）。
- 附件（均 uploaded）：`zcode2api-linux-amd64` 18,673,848 B；`zcode2api-linux-arm64` 18,088,120 B；`zcode2api-darwin-amd64` 19,176,640 B；`zcode2api-darwin-arm64` 18,611,682 B；`zcode2api-windows-amd64.exe` 17,881,600 B。
- 镜像：`ghcr.io/fujunchao/zcode2api-plus` 的 `2.11.0-go` 与 `latest` 均可匿名拉取，OCI index 含 linux/amd64、linux/arm64（另有 provenance 清单）；两个 tag 指向同一 multi-arch 索引。
- 产物抽查：下载 `zcode2api-windows-amd64.exe`，PE 头 `MZ`、内嵌 `2.11.0-go` 1 处、无 `2.10.30-go`；SHA256 `1edd687274e08c584ec8a6c4ce59a399556e2d92a3e60aecdb65fcdf09b7a6a0`。
- 发布说明正文 = `docs/releases/v2.11.0-go.md`（已抽查首段一致）。

### 11.3 回滚

- 运行期：`ZCODE_CLIENT_VERSION=3.15.3`（UA / `X-ZCode-App-Version` / `app_version` 自动回退，无需重编）；
- 代码：revert `ecaa4c5`/`1a5b92e`；本次无数据迁移、无接口变更。

### 11.4 发布后观察

- 观察上游按 `app_version` 分级配置与验证码/领取行为，以及 401/405 与验证码比例；异常时按 §11.3 回退。