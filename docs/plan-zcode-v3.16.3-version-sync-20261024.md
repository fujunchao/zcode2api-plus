# ZCode 3.16.3 客户端版本同步更新计划

> 日期：2026-10-24（北京时间）。依据：本机 ZCode **3.16.3** 逆向分析（[主报告](analysis-zcode-v3.16.3-20261024.md)、[项目影响笔记](analysis-zcode-v3.16.3-project-impact-notes-20261024.md)）。
> 结论前提：P0 = 0；本次只做 **P1 客户端版本声明同步（3.16.2 → 3.16.3）**，并发布 **v2.11.1-go** 小版本（用户显式要求发版）。不改模型协议、不改依赖、不改模型目录与思考档位。
> 项目基线：分支 `go-rewrite`，HEAD `9a65ff1`（v2.11.0-go）；origin `github.com:fujunchao/zcode2api-plus.git`，upstream 只读。制定本计划时未改业务代码。

## 1. 目标与范围

### 1.1 目标

让项目伪装身份与本机实装客户端一致：默认 `ZcodeClientVersion` 由 `3.16.2` 升为 `3.16.3`；`UserAgent`、`ModelUserAgent()`、`X-ZCode-App-Version`、`app_version` 查询参数全部由该常量自动派生。**请求形态、端点、认证、body 一律不动。**

### 1.2 范围

| # | 文件 | 变更 |
|---|---|---|
| 1 | `internal/config/config.go` | `459` 行默认值 `"3.16.2"` → `"3.16.3"`；458 行后补一行 `2026-10-24 同步本机确认的 3.16.3；来源头/归因头/metadata.user_id/端点/提示词均未变，仅版本值更新。` |
| 2 | `internal/config/profile.go` | 仅注释：`69` 行 `AppVersion string // 3.16.3`；`178/180` 行 UA 示例 `ZCode/3.16.3 …`（`AISDKVersion 4.0.27`、`NodeMajor 24` 不变） |
| 3 | `internal/config/version_test.go` | `31` 行期望 `"3.16.3"` |
| 4 | `internal/config/version_env_test.go` | `19`、`21` 行期望 `3.16.3` / `ZCode/3.16.3` |
| 5 | `README.md` | `56` 行"默认客户端版本已同步至 3.16.3"；新增 v2.11.1-go 发布段 |
| 6 | `internal/config/config.go`（发版） | `482` 行 `AppVersion = "2.11.1-go"` |
| 7 | `docs/releases/v2.11.1-go.md` | 新增发版说明（release.yml 自动作为 Release 正文） |
| 8 | `docs/plan-zcode-v3.16.3-version-sync-20261024.md` | 本计划 |

### 1.3 明确不做（Out of Scope）

- 不实现 3.16.3 新增/变化的客户端本地面能力：远程 Source Server 握手复用/错误语义、终端 owner 协议、会话目录 `directorySessionId`、内置组件 bundled release 缓存、插件 `isolation:"workspace"` 进程复用、插件市场 UI、输入法/输入管线、`DeferredToolCall`/`LocalToolSearch` 工具搜索体系；
- 不改 `ZcodeClientAISDKVersion=4.0.27`、`ZcodeClientNodeMajor=24`（3.16.3 证据未变）；
- 不新增任何请求头/端点/body 字段；不实现 `X-ZCode-Authorization`/`/api/v1/ultra*` 代理路由（继续"未知 x-zcode* 不透传"）；
- 不迁移 MiMo 模板、不改 team plan 登录链、不改 provider 目录获取方式；
- 不改前端与 `frontend/dist`。

## 2. 前置条件（执行前核对）

1. 分支 `go-rewrite`，HEAD `9a65ff1`；工作区只有既有未跟踪 `docs/`、`temp/` 文件，**无已跟踪改动**。
2. 远程 `origin` 可推（`gh` 已登录 @fujunchao）；upstream 只读。
3. 分析结论已复核：模型请求主链路静态未变（主报告 §4.5）。
4. 本地只做代码修改 + `gofmt`（LF 基线口径）+ `git diff`；测试、构建、竞态、交叉编译、镜像一律交 GitHub Actions（AGENTS.md）。

## 3. 实施步骤

1. **Step 0**：`git checkout -b codex/zcode-3.16.3-version-sync`（从 `go-rewrite` HEAD 拉出）。
2. **Step 1（commit 1）**：§1.2 的 #1-#5，提交 `chore: 同步 ZCode 3.16.3 客户端版本声明`。
3. **Step 2（commit 2）**：§1.2 的 #6-#8 + README 发布段，提交 `chore: 发布 v2.11.1-go`。
4. **Step 3**：`git push -u origin codex/zcode-3.16.3-version-sync`；`gh run watch` 等 CI 全绿（12 个 job）。
5. **Step 4**：`go-rewrite` 快进合并到分支 HEAD 并推送 origin；再次确认分支 CI 对应 SHA 全绿。
6. **Step 5**：在发版提交上打 annotated tag `v2.11.1-go` 并 `git push origin v2.11.1-go`，触发 `release.yml`（validate 复用 `ci.yml` + build 5 平台 + image）。
7. **Step 6**：核对 Release（非 draft/prerelease、5 个附件、正文=发版说明）、镜像 tag、产物内嵌版本。
8. **Step 7**：补记本计划 §9 的发布记录并推送 docs 提交。

## 4. 验证与验收

### 4.1 本地允许（不跑测试）

- `gofmt` 以 LF 基线检查（工作区 CRLF 属 Windows 检出格式，非本次引入）；
- `git diff` 只含 §1.2 清单；`grep -rn "3\.16\.2" internal cmd webui.go` 只应命中历史注释；
- 不执行 `go test`/`go vet`/`go build`/前端构建。

### 4.2 CI 验收（GitHub Actions，Ubuntu）

- 分支 push 触发 `ci.yml` 全量（workflows/frontend/test/race/sdk-compat/newapi-compat/windows-proxy-journal/cross-build/docker 等）；
- 发版 tag 触发 `release.yml`：validate 复用同一份 `ci.yml` 全绿，build 矩阵 5/5，image 成功；
- 以实际提交 SHA 的完整结果为准，不引用旧提交成功记录。

## 5. 回滚方案

1. **运行期应急（无需重编）**：`ZCODE_CLIENT_VERSION=3.16.2`（UA / `X-ZCode-App-Version` / `app_version` 自动回退）；只想回退非模型 UA 可设 `UPSTREAM_USER_AGENT=ZCode/3.16.2`。
2. **代码回滚**：revert 版本同步提交与发版提交；本次无数据迁移、无接口变更。
3. 本版只改版本字符串，无状态/结构变化。

## 6. 风险与未证实项

| 风险 | 等级 | 说明与缓解 |
|---|---|---|
| 上游按 `app_version` 分级下发配置 | 中低 | 3.16.3 未见 captcha/领取语义变化；上线后观察验证码与领取行为，异常用环境变量回退 |
| 服务端风控版本灰度 | 低 | 静态无版本硬门槛证据；升到最新官方版本方向一致 |
| 版本测试遗漏 | 低 | `TestNoHardcodedClientVersion` 防散落字面量，CI 全量兜底 |

未证实：服务端是否按版本分级/拒绝、`minCompatibleVersion` 是否还有其它消费点、3.16.2 CLI 逐字节差异（原文未留存）——均以 CI 与部署后观察为准。

## 7. 完成定义（DoD）

- [x] §1.2 文件按预期修改；本地 `gofmt`/diff 自查通过；
- [x] 两个 commit 已推送到 `origin/codex/zcode-3.16.3-version-sync`；
- [x] 分支最新 SHA 的完整 CI 全绿；失败已修复复验，未删除/跳过测试；
- [x] `go-rewrite` 已快进合并并推送；
- [x] `v2.11.1-go` annotated tag 已推送，`release.yml` 全绿；
- [x] Release 附件/镜像/正文与产物内嵌版本核对通过；
- [x] 发布记录回写本计划，未把本机分析目录纳入提交。

## 8. 执行边界确认

- 只推 `origin`，不碰 `upstream`；
- 不把 `temp/` 与未跟踪的分析 `docs/analysis-*` 纳入提交（与 v2.11.0 发版口径一致：逆向报告留档本机）；
- 不修改 `data/`、`.zcode/`；不读取账号材料；
- 发布门禁按 AGENTS.md：先等分支 CI 全绿，再由 tag 触发 `release.yml` 复用完整 `ci.yml`。

## 9. 执行与发布记录（2026-10-24，用户显式要求发版 v2.11.1-go）

> 用户在本计划确认后要求「按 P1 进行一次版本同步，并发布小版本」；以下为实际执行结果。

### 9.1 代码与 CI

- 分支 `codex/zcode-3.16.3-version-sync`：`6e8b4f6`（版本声明同步）、`91074a1`（`chore: 发布 v2.11.1-go`）；分支 CI run [40056932838](https://github.com/fujunchao/zcode2api-plus/actions/runs/40056932838) 全绿（12 个 job，SHA `91074a1`）。
- `go-rewrite` 快进合并到 `91074a1` 并推送；同名 SHA 的 go-rewrite CI run [40057586575](https://github.com/fujunchao/zcode2api-plus/actions/runs/40057586575) 全绿。
- 标签 `v2.11.1-go`（annotated，tag object `4caec8b03df9dfa94677d8776f4348dc0af0531a` → commit `91074a197fbc9f1c8b0333bb96776c318d4756df`）；远端 `refs/tags/v2.11.1-go` 已确认。

### 9.2 发布流水线

- release run [40057596890](https://github.com/fujunchao/zcode2api-plus/actions/runs/40057596890) 成功：validate（复用 `ci.yml` 全部 job）全绿；build 矩阵 5/5 成功；image 成功。
- Release：https://github.com/fujunchao/zcode2api-plus/releases/tag/v2.11.1-go（非 draft/prerelease，published 2026-10-24T03:23:41Z）。
- 附件（5 个，均 uploaded）：`zcode2api-linux-amd64` 18,673,848 B；`zcode2api-linux-arm64` 18,088,120 B；`zcode2api-darwin-amd64` 19,176,640 B；`zcode2api-darwin-arm64` 18,611,682 B；`zcode2api-windows-amd64.exe` 17,881,600 B。
- 镜像：`ghcr.io/fujunchao/zcode2api-plus` 的 `2.11.1-go` 与 `latest` 指向同一 OCI image index `sha256:418e59c76fcca64134d4ea3a8acec69a7bce5b9feb20b506272a9352a61fd080`，含 `linux/amd64`、`linux/arm64`（匿名 pull 校验通过，另有 provenance 清单）。
- 产物抽查：下载 `zcode2api-windows-amd64.exe`，PE 头 `MZ`；内嵌 `2.11.1-go` 1 处、`2.11.0-go` 0 处；内嵌 `3.16.3` 1 处、`3.16.2` 0 处；SHA256 `174e7cc7b4d1ab73505a81c7f744979f8d96afb475faf87e91c56d2ef763c89a`。
- 发布说明正文 = `docs/releases/v2.11.1-go.md`（已抽查首段一致）。

### 9.3 回滚

- 运行期：`ZCODE_CLIENT_VERSION=3.16.2`（UA / `X-ZCode-App-Version` / `app_version` 自动回退，无需重编）；
- 代码：revert `6e8b4f6` / `91074a1`；本次无数据迁移、无接口变更。

### 9.4 发布后观察

- 观察上游按 `app_version` 分级配置与验证码/领取行为，以及 401/405 与验证码比例；异常时按 §9.3 回退。
