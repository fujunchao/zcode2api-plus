# ZCode 3.17.1 P0 版本同步计划（用户体验优先）

> 日期：2026-10-31（北京时间）。依据：[3.17.1 主报告](analysis-zcode-v3.17.1-20261030.md)、[项目影响笔记](analysis-zcode-v3.17.1-project-impact-notes-20261030.md)。
> 用户指令：**先不改代码，先出 P0 计划；一切以用户体验为最优先**。因此本文件只做方案，**不含任何代码改动**（本文亦不改动 `internal/`、`cmd/`、`frontend/`）。
> 项目基线：分支 `go-rewrite`，HEAD `65aa89c`（v2.11.3-go 发布记录提交）；工作区仅有既有未跟踪 `docs/analysis-*`（67 个）与 `temp/`，**无已跟踪改动**；origin `github.com:fujunchao/zcode2api-plus.git`，upstream 只读。

---

## 0. 一页速览（TL;DR）

| 项 | 结论 |
|---|---|
| P0 是什么 | 把默认伪装客户端版本 `3.16.3` → `3.17.1`（唯一一处常量），外加 3 处测试期望值与 4 处注释/文档 |
| 改多少 | 代码 2 个文件（1 行逻辑 + 注释）、测试 2 个文件（3 行期望）、文档 2 个文件；**0 个接口、0 个配置项、0 个前端、0 个依赖** |
| 用户体验 | **零配置、零接口变更、零行为变更**；未显式固定版本的用户升级后自动生效；显式固定的用户不被静默改写（尊重用户配置） |
| 回退 | 一次重启（秒级）：`ZCODE_CLIENT_VERSION=3.16.3`；无需重编、无需回滚数据 |
| 最大风险 | 仅"上游是否认这个版本号"；不改变请求形态与错误处理，**不会引入新的失败路径**，最坏表现是既有错误码路径（如套餐不投放/4xx），不会丢数据 |
| 发版 | **需要用户显式要求**；不发版则只有源码部署用户受益（AGENTS.md 发布门禁） |
| 建议先做 | 双实例 A/B（3.16.3 vs 3.17.1，同一账号池），用 24h 数据确认投放无回归后再全量 |

---

## 1. 背景与判定依据

1. 本机客户端已更新为 **3.17.1**（exe 3.17.1.9553 / asar 374,627,566 B / build `025f7f39`）。
2. 逆向结论（主报告 §4）：3.17.1 相对 3.16.3 的**模型 wire 面零变化** —— 伪装头集合逐行一致、模型请求 UA 三个组成项未变（`ai-sdk/provider-utils/4.0.27`、`runtime/node.js/24`、`ZCode/<ver>`）、模型与计费端点无增删改。
3. 历史硬事实（`docs/analysis-zcode-client-activation-diff.md` §1）：**上游按 `app_version` 判定活动套餐投放资格**；版本落后会直接导致 preview 不投放 —— 这是本项目唯一被实测证实的"版本硬门槛"。
4. 因此本轮 P0 的**全部内容**就是让伪装版本与本机实装客户端一致；其余 3.17.1 新能力（Sites、远程数据同步、MCP 内置 npm、CUA 多光标、遥测归因等）均属"客户端本地面"，**不在本项目实现范围**（影响笔记 §6）。

---

## 2. 目标与非目标

### 2.1 目标
- 默认 `ZCODE_CLIENT_VERSION` = `3.17.1`；`UserAgent`、`ModelUserAgent()`、`X-ZCode-App-Version`、`app_version` 查询参数、`ZcodeProfile.AppVersion` 全部随之自动派生（单一来源，不新增第二处字面量）。
- 测试期望值同步，保证 CI 不红、且"版本单一来源"的守护测试继续有效。
- 升级过程对**正在使用的用户**无感：无接口变更、无需改配置、无需改数据库、无需重新登录。

### 2.2 非目标（明确不做）
- 不新增/修改任何请求头、端点、body 字段；不跟进 `x-zcode-session-features`（影响笔记 P2）。
- 不改 `ZcodeClientAISDKVersion=4.0.27`、`ZcodeClientNodeMajor=24`（3.17.1 证据未变）。
- 不改模型目录、模型映射、思考档位、system prompt 组装、验证码策略、代理/风控参数。
- 不实现 3.17.1 的客户端本地面功能（Sites、远程设备管理、统一数据同步、MCP 安装、CUA 多会话光标、移动端日志/推送等）。
- 不改前端与 `frontend/dist`；不改 `docker-compose.yml` 的变量集合（见 §3.3 关于回退路径的说明）。
- 不发版、不打标签、不推镜像——除非用户在本计划确认后明确要求。

---

## 3. 用户体验设计（本计划第一优先级）

### 3.1 升级前后"用户可见变化"矩阵

| 视角 | 升级前 | 升级后 | 用户是否感知 |
|---|---|---|---|
| 网关 API 调用方（OpenAI/Anthropic 兼容） | `/v1/chat/completions`、`/v1/responses`、`/v1/models` | **完全一致**（无字段/无错误码变化） | 否 |
| 后台管理界面 | 现有页面与功能 | **完全一致**（不改前端产物） | 否 |
| 数据库/数据目录 | 现状 | **无结构变更、无迁移** | 否 |
| 需要配置的项 | — | **不新增、不修改任何配置项** | 否 |
| 上游识别到的身份 | `ZCode/3.16.3` + `app_version=3.16.3` | `ZCode/3.17.1` + `app_version=3.17.1` | 用户不感知，**上游感知**（这正是目的） |
| 运维可见项 | `/meta` 的 `version` = `2.11.3-go`（产品版本，不变） | 若发版则变为新版本号；伪装版本不出现在 `/meta` | 仅发版时感知 |

> 结论：**这是一次"仅上游可感知"的升级**，用户侧零改动。计划的所有额外工作（灰度、A/B、观察、回退）都是为了把"上游不可预期"压缩到最低。

### 3.2 三类部署形态的升级路径

| 部署形态 | 升级方式 | 用户操作 | 备注 |
|---|---|---|---|
| Docker Compose（默认从源码构建，`build: .`） | 拉新代码 → `docker compose up -d --build` | 1 条命令 | 容器重建期间短时不可用（滚动/停机策略见 §3.4） |
| Docker（用 GHCR 镜像，`image: ghcr.io/...`） | 改 tag → `docker compose pull && up -d` | 2 条命令 | 需先有发版镜像 |
| 预编译二进制 | 下载新 Release 附件 → 替换 → 重启 | 替换+重启 | 配置与 `data/` 不变 |
| 源码自行构建 | `git pull` → 构建 | 同现有流程 | 默认值随代码生效 |
| **显式固定版本**（设了 `ZCODE_CLIENT_VERSION`） | 见 §3.3 | **无需操作**（保持现状） | 尊重用户显式配置，不被静默改写 |

### 3.3 "显式固定版本"的用户：尊重优先 + 自查指引

**原则**：用户显式设置的环境变量优先于编译默认值，升级**不会**改变这类部署的行为——这是"用户可控"而非"被覆盖"。

但有一个真实存在的坑（`.workbuddy/memory/2026-09-23.md` 已记录）：`docker-compose.yml` 的 `environment:` **没有引用** `ZCODE_CLIENT_VERSION`，应用本身也不读 `.env`；写进 `.env` 却没被 `environment:` 引用的变量**不会进容器**。因此：

- 升级说明必须写清"如何判断自己是否被固定"：
  `docker exec zcode2api env | grep ZCODE_CLIENT_VERSION`（无输出 = 走默认值，升级即生效）。
- 若用户确实想固定/回退，必须显式加入 compose（**这是回退路径能否生效的关键**）：
  ```yaml
      environment:
        ZCODE_CLIENT_VERSION: ${ZCODE_CLIENT_VERSION:-3.17.1}
  ```
  并在 `.env` 写 `ZCODE_CLIENT_VERSION=3.16.3`（回退时）。
- 若用户此前用 `-e ZCODE_CLIENT_VERSION=...` 或自定义 compose override，行为同理，升级文档给一句"沿用你现有的 override 即可"。

### 3.4 升级窗口与灰度

| 场景 | 做法 | 理由（用户体验） |
|---|---|---|
| 多副本/多实例 | 先 1 个副本升到 3.17.1，观察 ≥30 分钟；无异常再滚动其余副本 | 任何版本相关异常都被限制在部分流量内，可秒级回退 |
| 单实例 | 选**业务低峰**窗口执行（建议北京时间 10:00–11:30 或 14:00–16:00，避开 20:00–24:00） | 避开个人用户的晚间活跃与错峰/闲时套餐的使用时段，缩短感知窗口 |
| 首次全量前（强烈建议） | **A/B 对照**：保留 1 台跑 3.16.3，新增/切换 1 台跑 3.17.1，同一账号池，对比 24h 的 preview 投放率、claim 成功率、模型 2xx/405 率 | 用数据判断"升版本是否真的不掉投放"，而不是赌 |
| 回滚窗口 | 升级后 72 小时内保持"随时可回退"状态（不改其它变量、不做无关升级） | 单变量变更，回退时无需排除干扰项 |

### 3.5 回退体验（三层，按恢复速度排序）

| 层级 | 操作 | 生效时间 | 适用 | 代价 |
|---|---|---|---|---|
| L1 运行期（推荐） | 设 `ZCODE_CLIENT_VERSION=3.16.3` 后重启进程/容器 | 秒级（一次重启） | 上游异常、投放下降、405 抬头 | 无；无需重编，无数据影响 |
| L2 镜像/二进制 | 回退到上一版本 tag/附件（`v2.11.3-go`） | 分钟级 | L1 不足以恢复时 | 需重新拉取/替换 |
| L3 代码 | `git revert` 版本同步提交（+ 发版提交） | 小时级（走 CI+发版） | 需要长期回退 | 走完整发布门禁 |

**回退可用性要求**：L1 必须在上线**前**演练一次（在一个非生产实例上验证"改 env → 重启 → 出站 UA 变回 3.16.3"），否则不能宣称"可回退"。

### 3.6 沟通物料（面向用户的语言）

1. **README**：一段"为什么升级 / 对我有什么影响 / 怎么回退"的三句话说明（不是技术细节堆砌）。
2. **Release 说明**（发版时）：用户视角，见附录 B 草稿。
3. **不新增**任何后台弹窗、横幅、强制提示——避免打扰；版本信息保持只在文档与 Release 中呈现。
4. 若做 A/B：在项目内记录实验编号与结论（供后续复盘），但不对外暴露账号/线路信息。

### 3.7 失败时的"用户影响上限"

| 最坏假设 | 用户实际感受 | 为什么不更糟 |
|---|---|---|
| 上游不认 3.17.1（理论上不会，官方客户端就是它） | 套餐投放异常/请求被拒 | 本次零协议变更，失败面与现有任何一次上游策略调整同构；L1 回退秒级恢复 |
| 上游对 3.17.1 加严校验 | 部分账号 4xx/405 抬头 | 有既有的风控静默/冷却与账号隔离机制兜底；可立即回退 |
| 版本值写错（如 3.17 误写 3.17.0） | 指纹不一致 | CI 有 `TestClientVersionShape`（三段版本号）+ `TestDefaultClientVersionMatchesInstalledClient` 守护 |

---

## 4. 变更清单（精确到文件:行）

> 以下为**待批准后**才执行的改动；本轮不落地。

| # | 文件:行 | 改动前 | 改动后 | 类型 |
|---|---|---|---|---|
| 1 | `internal/config/config.go:481` | `ZcodeClientVersion = env("ZCODE_CLIENT_VERSION", "3.16.3")` | `... "3.17.1")` | 逻辑（唯一一处） |
| 2 | `internal/config/config.go:480` 之后 | 注释止于 `2026-10-24 同步本机确认的 3.16.3…` | 追加一行：`// 2026-10-31 同步本机确认的 3.17.1；伪装头集合/模型请求 UA 组成（ai-sdk 4.0.27、node 24）/模型与计费端点/提示词 wire 形态均未变，仅版本值更新；新增 x-zcode-session-features 未跟进。` | 注释 |
| 3 | `internal/config/version_test.go:31` | `ZcodeClientVersion != "3.16.3"` | `!= "3.17.1"` | 测试期望 |
| 4 | `internal/config/version_env_test.go:19` | `{"defaults", "", "", "3.16.3", "ZCode/3.16.3"}` | `{"defaults", "", "", "3.17.1", "ZCode/3.17.1"}` | 测试期望 |
| 5 | `internal/config/version_env_test.go:21` | `{"user_agent_override", "", "synthetic-custom-ua", "3.16.3", ...}` | 版本段 `3.17.1` | 测试期望 |
| 6 | `internal/config/profile.go:69` | `AppVersion     string // 3.16.3` | `// 3.17.1` | 注释 |
| 7 | `internal/config/profile.go:178,180` | `// 3.16.3 的 SDK 拼接形态保持…` / `// ZCode/3.16.3 ai-sdk/...` | `3.17.1`（UA 组成不变，仅版本段） | 注释 |
| 8 | `README.md:56` | "默认客户端版本已同步至 `3.16.3`…" | "…`3.17.1`…" + 三句话影响说明（§3.6） | 文档 |
| 9 | `README.md:77-79`（v2.11.1-go 段） | 历史发布说明 | **不改历史段**；在合适位置新增 3.17.1 说明段（发版时才写 `v2.11.4-go`） | 文档 |
| 10 | `.env.example:60-61` | `# ZCODE_CLIENT_VERSION=3.15.3` / `# UPSTREAM_USER_AGENT=ZCode/3.15.3` | `3.17.1` | 文档 |
| 11 | `internal/config/config.go:504`（**仅发版时**） | `AppVersion = "2.11.3-go"` | `"2.11.4-go"` | 发版 |
| 12 | `docs/releases/v2.11.4-go.md`（**仅发版时**） | — | 新增（内容见附录 B） | 发版 |

**不改动**：`internal/upstream/**`、`internal/gateway/**`、`internal/claim/**`、`internal/captcha/**`、`internal/openai/**`、`frontend/**`、`docker-compose.yml`、`Dockerfile`、`go.mod`。

---

## 5. 实施步骤（待批准后执行）

1. **Step 0（只读核对）**：确认 `go-rewrite` HEAD 与工作区干净（仅既有未跟踪文件）；用 `.workbuddy/check-client-version.js` 复核本机 exe ProductVersion 与计划目标一致（应输出"3.17.1 vs 3.17.1 一致"）。
2. **Step 1（分支）**：`git checkout -b codex/zcode-3.17.1-version-sync`（从 `go-rewrite` HEAD 拉出）。
3. **Step 2（commit 1：版本同步）**：§4 的 #1-#7 + #8-#10（文档），提交 `chore: 同步 ZCode 3.17.1 客户端版本声明`。
   - 本地只做 `gofmt`（LF 口径）与 `git diff` 自查；`grep -rn "3\.16\.3" internal cmd webui.go` 只应命中历史注释/测试注释与 `.workbuddy`（不属于提交范围）。
4. **Step 3（推送 + CI）**：`git push -u origin codex/zcode-3.17.1-version-sync`；`gh run watch` 等 `ci.yml` 全绿（workflows/frontend/test/sdk-compat/newapi-compat/windows-proxy-journal/cross-build/docker）。
5. **Step 4（合并）**：`go-rewrite` 快进合并到该 SHA 并推送；再次以**该 SHA** 的 CI 结果为验收依据。
6. **Step 5（可选发版，需用户显式指令）**：在发版提交（#11+#12 + README 发布段）上打 annotated tag `v2.11.4-go` 并推送，触发 `release.yml`（validate 复用 `ci.yml` + build 矩阵 + image）。
7. **Step 6（发布核对）**：Release 非 draft/prerelease、附件齐全、镜像 tag 与 index、产物内嵌版本（`2.11.4-go` 与 `3.17.1` 各 1 处、旧值 0 处）。
8. **Step 7（上线与观察）**：按 §3.4 灰度/窗口实施；按附录 C 观察 30 分钟 / 24 小时 / 72 小时；把结果回写本计划 §9。

---

## 6. 验证矩阵

### 6.1 静态（本地，允许）
- `gofmt` 差异为零；`git diff` 仅含 §4 清单；无新增字面量（`TestNoHardcodedClientVersion` 的守护对象不变）。
- 交叉核对：`git grep -n "3\.16\.3" -- internal cmd webui.go` 仅历史注释/测试注释。

### 6.2 单元/契约（CI，Ubuntu；本地不跑）
| 测试 | 守护内容 | 预期 |
|---|---|---|
| `internal/config` `TestDefaultClientVersionMatchesInstalledClient` | 默认版本 == 本次核验版本 | 通过（3.17.1） |
| `internal/config` `TestUserAgentDerivesFromClientVersion` | UA 由常量派生 | 通过 |
| `internal/config` `TestClientVersionShape` | 三段版本号格式 | 通过 |
| `internal/config` `TestClientVersionEnvironmentOverrides` / `TestClientVersionEnvironmentHelper` | env 覆盖语义不变 | 通过（期望值已更新） |
| `internal/config` `TestNoHardcodedClientVersion` | 无散落字面量 | 通过 |
| `internal/claim` `TestClientVersionSingleSource` | 领取/遥测头随常量变化 | 通过 |
| `internal/captcha` / `internal/claim` 的 nonmodel 契约测试 | `app_version` 查询参数取自常量 | 通过 |

### 6.3 上线前（预发/容器内）

> ⚠️ 两个"看起来能验证、实际不能"的坑，先说清楚，避免误判：
> - `/meta` 只返回 `config.AppVersion`（产品版本，如 `2.11.4-go`），**不暴露伪装版本**，不能用它验证 3.17.1。
> - 出站头**不会落库明文**：`ModelRequestObservation` 把整组头 `json.Marshal` 后只存 `ProfileRef = observationRef(profileJSON)`（`internal/gateway/proxy_observation.go:215,228`），后台/DB 里查不到 `X-Zcode-App-Version` 原文。

**可执行的三种验证（按成本从低到高）：**

1. **二进制内嵌字符串（无需账号，30 秒）** —— 容器内二进制路径是 `/usr/local/bin/zcode2api`（`Dockerfile:57`），镜像基于 Debian 且**未装 binutils**，用 `grep -a` 而不是 `strings`：
   ```bash
   docker exec zcode2api sh -c 'grep -ac "3\.17\.1" /usr/local/bin/zcode2api'   # 期望 ≥1
   docker exec zcode2api sh -c 'grep -ac "3\.16\.3" /usr/local/bin/zcode2api'   # 期望 0；若命中先确认是不是无关依赖的版本号
   ```
2. **CI 契约测试（无需账号，首选权威口径）**：§6.2 的 6 个测试直接断言"默认值、UA 派生、头集合、`app_version` 参数"；它们通过即等价于证明线上会发 3.17.1。
3. **端到端回显（需要一次真实账号请求，非必须）**：把上游指向本地回显服务再发一条真实请求，直接看请求头：
   ```bash
   # config.UpstreamZai = env ZAI_UPSTREAM_URL（internal/config/config.go:334）
   ZAI_UPSTREAM_URL=http://127.0.0.1:9090/anthropic/v1/messages ./zcode2api serve
   # 另开终端，用任意回显 HTTP 服务打印 method/path/headers，然后从后台/客户端发一条模型请求
   # 期望看到：User-Agent: ZCode/3.17.1 ai-sdk/provider-utils/4.0.27 runtime/node.js/24
   #           X-Zcode-App-Version: 3.17.1
   ```

**回退演练（§3.5 L1）**：改 env → 重启 → 用方法 1 确认二进制声明回退（若走方法 3，则确认头部回到 `ZCode/3.16.3`）→ 再切回 3.17.1。演练必须在上线前完成。

### 6.4 上线后（真实流量）
| 时点 | 观察项 | 通过标准 | 异常处置 |
|---|---|---|---|
| +30 分钟 | 启动日志无异常；模型请求 2xx 率；405/风控触发数；preview 是否投放 | 与升级前基线同量级；preview 正常 | 立即 L1 回退 |
| +24 小时 | claim 成功率、账号池健康、错误码分布（3012 等）、P95 延迟、SSE 首字延迟 | 无显著劣化（建议阈值：错误率 +≤2pp、P95 +≤10%） | 分析后决定回退与否 |
| +72 小时 | A/B 结论（若做）、投放稳定性、用户工单/反馈 | 与 3.16.3 组无显著差异或更优 | 复盘并更新文档 |

---

## 7. 风险登记

| # | 风险 | 等级 | 影响 | 缓解 |
|---|---|---|---|---|
| R1 | 上游对 3.17.1 的校验比 3.16.3 更严（本次静态不可见） | 中 | 投放/请求异常 | 灰度 + A/B + 30 分钟观察 + L1 回退 |
| R2 | 显式固定版本的用户升级后"看起来没变" | 中 | 用户困惑 | §3.3 自查命令 + 文档说明（尊重显式配置） |
| R3 | compose 未透传 `ZCODE_CLIENT_VERSION` 导致"回退无效" | 中 | 回退失败 | 文档给出必须新增的 `environment:` 行；回退演练覆盖 |
| R4 | 测试期望值漏改 → CI 红 | 低 | 阻塞合并 | §4 #3-#5 已穷举（`TestDefaultClientVersionMatchesInstalledClient` 会直接失败提示） |
| R5 | 误改其它版本字符串（如 `AppVersion` 产品版本） | 低 | `/meta` 显示错乱 | diff 复核 + CI `TestClientVersionShape` |
| R6 | CLI/registry 基线缺口导致"其实还有变化" | 低中 | 少跟一个变更 | 已在主报告 §7 标注；上线观察兜底；后续抓包复核（影响笔记 P2） |
| R7 | B13"安装包变小"未证实（实测变大 43MB） | 低 | 用户下载时间略增 | Release 说明不承诺体积；如需优化另开任务 |
| R8 | 72 小时内叠加其它变更，污染回退判断 | 低 | 回退时难以定位 | §3.4 回滚窗口内只允许这一个变更 |

---

## 8. 回滚预案（含触发条件）

**触发条件（满足任一即回退）**：preview 连续 30 分钟不投放；模型请求 405/风控触发显著高于基线；4xx/5xx 比例突增；账号池出现异常冷却级联。

**步骤**：
1. 记录时间点与现象（保留证据，便于复盘）。
2. 设 `ZCODE_CLIENT_VERSION=3.16.3`（compose 需按 §3.3 显式加 `environment:` 行），重启实例。
3. 按 §6.3 方法 1（二进制字符串）或方法 3（本地回显头）确认声明已回到 `3.16.3`；注意 `/meta` 不反映伪装版本，别用它判断。
4. 若 L1 无效（极少见），按 L2 回退镜像/二进制到 `v2.11.3-go`。
5. 回写本计划 §9 与复盘结论（含是否与本次变更相关）。

**不可回退项**：无（本次无数据迁移、无接口变更、无配置项增删）。

---

## 9. 执行与观察记录（2026-10-31，用户要求“按计划执行，完成后推送并发版”）

### 9.1 代码与 CI

| 项 | 结果 |
|---|---|
| 分支 | `codex/zcode-3.17.1-version-sync`（从 `go-rewrite` HEAD `65aa89c` 拉出） |
| commit 1 | `98c1099` `chore: 同步 ZCode 3.17.1 客户端版本声明`（6 文件，+14/−10） |
| commit 2 | `1f36978` `chore: 发布 v2.11.4-go`（3 文件，+45/−1） |
| 分支 CI（98c1099） | run [41197379212](https://github.com/fujunchao/zcode2api-plus/actions/runs/41197379212) 全绿（12 job：workflows/frontend/test/sdk-compat/newapi-compat/windows-proxy-journal/cross-build×5/docker） |
| 分支 CI（1f36978） | run [41198331572](https://github.com/fujunchao/zcode2api-plus/actions/runs/41198331572) 全绿 |
| 合并 | `go-rewrite` 快进合并 `65aa89c → 1f36978` 并推送 |
| 合并后 CI（1f36978） | run [41198884066](https://github.com/fujunchao/zcode2api-plus/actions/runs/41198884066) 全绿 |

### 9.2 发布流水线

- 标签：annotated `v2.11.4-go`，tag object `b1f67d2b5483261e16fa6d23396d017dd6ff36a7` → commit `1f36978e393d94fd8d2913409dd05e562353feb9`；已推送 `origin`。
- release run [41199353905](https://github.com/fujunchao/zcode2api-plus/actions/runs/41199353905) 成功：validate 复用 `ci.yml` 全部 job 全绿（含 cross-build×5）；build 矩阵 5/5 成功；image 成功。
- Release：https://github.com/fujunchao/zcode2api-plus/releases/tag/v2.11.4-go （非 draft / 非 prerelease，published 2026-10-31T12:10:14Z）；正文 = `docs/releases/v2.11.4-go.md`。
- 附件（5 个，均上传）：`zcode2api-linux-amd64` 19,189,944 B；`zcode2api-linux-arm64` 18,546,872 B；`zcode2api-darwin-amd64` 19,691,200 B；`zcode2api-darwin-arm64` 19,109,634 B；`zcode2api-windows-amd64.exe` 18,395,136 B。

### 9.3 镜像

- 标签：`ghcr.io/fujunchao/zcode2api-plus:2.11.4-go` 与 `:latest` **指向同一 OCI image index** `sha256:05d53aa84dfdbb58affb6a1db01eab8aecdebe3f9a75ae5d0c3b338deff52b87`（匿名 pull 校验通过）。
- 平台：`linux/amd64`、`linux/arm64`（index 内另含 provenance/attestation 条目）；镜像 label 内 `org.opencontainers.image.version=2.11.4-go`、`revision=1f36978…`。

### 9.4 产物抽查（windows-amd64）

- 大小 18,395,136 B；SHA256 `8e23c2fa6f671cdb28d5e6d8c88bf64162ebda625b5447d47641f91c1856a881`；PE 头 `MZ`。
- 内嵌版本：`2.11.4-go` 1 处、`2.11.3-go` 0 处；`3.17.1` 1 处（紧邻 `ZCode/`、`4.0.27` 等身份常量）、`3.16.3` 0 处。

### 9.5 用户可见交付

- `README.md` 顶部已更新默认版本 + 新增“升级不改变接口/配置/数据”的三句说明；发布段指向本版说明。
- Release 正文面向用户写明：无需改配置、接口不变、如何自查是否被固定、如何回退（含 Compose 必须在 `environment:` 引用变量的提示）。

### 9.6 回滚可用性

- L1（推荐）：部署侧设 `ZCODE_CLIENT_VERSION=3.16.3` 后重启即可（无需重编，秒级）。
- L2：回退镜像/二进制到 `v2.11.3-go`；L3：`git revert 1f36978 98c1099`。
- 本次无数据迁移、无接口变更，回退不涉及数据处置。

### 9.7 待部署侧回填（本机未执行真实流量验证）

| 项 | 状态 |
|---|---|
| A/B（3.16.3 vs 3.17.1 同池对照 24h） | 未执行（属部署侧运维动作；建议上线时按 §3.4 做） |
| 上线窗口与灰度顺序 | 待部署侧确认 |
| +30min / +24h / +72h 观察（preview 投放、2xx、405、claim、延迟） | 待部署侧按附录 C 回填 |
| 是否触发回退 | 待填 |

---

## 9.8 事故与回滚（2026-11-01 追记）

- **v2.11.4-go 上线后全量 405/3012**：3.17.1 改了官方 system 首段（block0 与身份段），
  而上游从 system 第 0 字节校验到身份段末尾、并按 `app_version` 期望对应文案。
  只升版本号 = 文案不匹配 = 每个请求被判 unusual activity，网关阶梯冷却后表现为池级 503。
- **处置**：部署侧设 `ZCODE_CLIENT_VERSION=3.16.3` 回退，日志恢复 87/87 成功。
- **跟进**：本计划 §4.1「只改版本值」的判断被推翻——版本升级必须与**文案档案**成对；
  修复见 `v2.11.5-go`（默认回退 + `zcode_system_3171.json` + `probe` 探针）。
- 根因与取证：[analysis-zcode-3.17.1-405-incident-20261101.md](analysis-zcode-3.17.1-405-incident-20261101.md)；
  探针操作：[runbook-zcode-3.17.1-probe-20261101.md](runbook-zcode-3.17.1-probe-20261101.md)。

---

## 10. 完成定义（DoD）

- [x] 用户确认本计划（2026-10-31 明确指令：按计划执行、完成后推送并发版）。
- [x] §4 清单按预期修改；本地 `gofmt`（CRLF 仅为检出格式，gofmt 输出与 LF 原文件逐字节相同）/`git diff` 自查通过，无越界文件（6 文件，+14/−10）。
- [x] 分支推送后 `ci.yml` 全绿（run 41197379212 / 41198331572，以对应 SHA 为准）；未删除或跳过任何测试。
- [x] `go-rewrite` 已快进合并并推送（65aa89c → 1f36978），合并 SHA 的 CI 全绿（run 41198884066）。
- [x] tag `v2.11.4-go` 已推送；`release.yml` 全绿（run 41199353905）；Release 非 draft/prerelease、5 个附件、镜像 2.11.4-go/latest 同 digest、产物内嵌 `2.11.4-go` 与 `3.17.1` 各 1 处且旧值 0 处。
- [ ] 回退演练（§3.5 L1）**未执行**：本机无生产实例可演练；已写入部署侧上线检查清单（§9.7）。
- [ ] 上线后 30 分钟/24 小时/72 小时观察**待部署侧回填**（§9.7，阈值见附录 C）。
- [x] 用户文档已更新：README 顶部默认版本 + 升级/回退三句说明、`docs/releases/v2.11.4-go.md`（即 Release 正文）。

---

## 11. 执行边界

- 只推 `origin`，不碰 `upstream`；分支用 `codex/` 前缀。
- 不把 `temp/` 与未跟踪的分析 `docs/analysis-*` 纳入提交（沿用 v2.11.x 口径：逆向报告留档本机）。
- 不读/不改 `data/`、`.zcode/`；不把真实账号材料交给 CI。
- 本地不跑自动化测试；Go 测试/构建/交叉编译/镜像一律由 GitHub Actions 执行（AGENTS.md）。
- 不因本计划调整依赖、放宽成功条件或绕过发布门禁；发版必须由用户显式要求。

---

## 附录 A：预期 diff 摘要（示意，执行时以实际为准）

```diff
--- a/internal/config/config.go
+++ b/internal/config/config.go
@@
 	// 2026-10-24 同步本机确认的 3.16.3；来源头/归因头/metadata.user_id/端点/提示词均未变，仅版本值更新。
+	// 2026-10-31 同步本机确认的 3.17.1；伪装头集合/模型请求 UA 组成（ai-sdk 4.0.27、node 24）/
+	// 模型与计费端点/提示词 wire 形态均未变，仅版本值更新；新增 x-zcode-session-features 未跟进。
-	ZcodeClientVersion = env("ZCODE_CLIENT_VERSION", "3.16.3")
+	ZcodeClientVersion = env("ZCODE_CLIENT_VERSION", "3.17.1")
```

```diff
--- a/internal/config/version_test.go
+++ b/internal/config/version_test.go
@@
-	if strings.TrimSpace(os.Getenv("ZCODE_CLIENT_VERSION")) == "" && ZcodeClientVersion != "3.16.3" {
+	if strings.TrimSpace(os.Getenv("ZCODE_CLIENT_VERSION")) == "" && ZcodeClientVersion != "3.17.1" {
```

```diff
--- a/internal/config/version_env_test.go
+++ b/internal/config/version_env_test.go
@@
-		{"defaults", "", "", "3.16.3", "ZCode/3.16.3"},
+		{"defaults", "", "", "3.17.1", "ZCode/3.17.1"},
@@
-		{"user_agent_override", "", "synthetic-custom-ua", "3.16.3", "synthetic-custom-ua"},
+		{"user_agent_override", "", "synthetic-custom-ua", "3.17.1", "synthetic-custom-ua"},
```

---

## 附录 B：发版说明草稿（`docs/releases/v2.11.4-go.md`，仅发版时使用）

```markdown
# v2.11.4-go

## ZCode 3.17.1 客户端版本声明同步

本版只做一件事：把默认伪装的官方客户端版本从 3.16.3 同步到 3.17.1。

### 对你有什么影响
- **不需要改任何配置**，也不需要重新登录；数据库与 `data/` 无结构变更。
- 网关接口（`/v1/chat/completions`、`/v1/responses`、`/v1/models`）与后台功能完全不变。
- 升级后，网关向上游声明的 `User-Agent` / `X-ZCode-App-Version` / `app_version` 变为 3.17.1。

### 为什么升级
官方客户端已更新到 3.17.1。历史实测表明上游会按 `app_version` 判定套餐投放资格，
版本落后可能导致活动/套餐不投放，因此保持与官方一致是稳定性需要。

### 我怎么确认生效了
`docker exec zcode2api env | grep ZCODE_CLIENT_VERSION` 应为空（走默认值）；
若你显式固定了版本，升级不会改变你的设置。

### 出问题怎么回退
设置 `ZCODE_CLIENT_VERSION=3.16.3` 后重启即可（compose 需在 `environment:` 中显式引用该变量）。

### 本版不含
3.17.1 客户端的桌面能力（Sites、远程设备管理、统一数据同步、MCP 安装、Computer Use 多会话光标等）
均属客户端侧功能，与本网关无关，未实现也不影响使用。
```

---

## 附录 C：上线观察清单与建议阈值

| 指标 | 基线来源 | 观察窗口 | 建议阈值（超阈值即止损） |
|---|---|---|---|
| preview 投放（能否拿到套餐配置） | 升级前 24h | +30min / +24h | 30 分钟内 0 投放 → 立即回退 |
| 模型请求 2xx 率 | 升级前 24h | +30min / +24h | 下降 > 2pp |
| 405 / 风控触发数（如 3012） | 升级前 24h | +30min / +24h | 上升 > 1 倍且持续 15 分钟 |
| claim 成功率 | 升级前 7d | +24h | 下降 > 3pp |
| P95 端到端延迟 / SSE 首字延迟 | 升级前 24h | +24h | 上升 > 10% |
| 账号池冷却/恢复级联 | 升级前 7d | +72h | 出现连续冷却 ≥3 次 |

---

## 附录 D：FAQ（预计用户会问）

1. **会不会重置我的配置/账号？** 不会。本次不触发任何数据迁移，`data/` 结构不变。
2. **升级后接口会变吗？** 不会。无新增/删除字段，错误码与限流策略不变。
3. **我固定了 `ZCODE_CLIENT_VERSION`，会被覆盖吗？** 不会，显式环境变量优先；但请注意 compose 需要在 `environment:` 里显式引用该变量才会进容器。
4. **升级后多久能看到效果？** 立即（重启后首次请求即带新版本号）。
5. **出问题多久能回退？** 一次重启（秒级），不需要重编、不需要回滚数据。
6. **为什么不顺便支持 3.17.1 的新功能？** 那些是客户端桌面能力（Sites、远程设备、MCP 安装等），网关不需要；贸然实现反而扩大与上游的差异面。
