# ZCode 3.17.2 → zcode2api-plus 项目影响笔记

> 日期：2026-11-03（Asia/Shanghai）。依据：[3.17.2 主报告](analysis-zcode-v3.17.2-20261103.md)（同目录）、temp/zcode-v3172-20261103/ 下的四份分报告与差分产物。
> 前提：本项目是 Go 实现的 ZCode 兼容网关，冒充官方桌面客户端/随包 CLI 走官方 coding-plan 额度；上游是 zcode.z.ai 的模型与计费端点。
> **本文只出清单与建议，未改动任何业务代码**（遵守「分析轮不改代码、用户未要求发版」）。

---

## 一、结论

| 级别 | 数量 | 内容 |
|---|---|---|
| **P0-A（不改会持续失真）** | 1 | 伪装客户端版本 3.17.1 → 3.17.2（含 3 处测试期望值） |
| **P0-B（不改就复刻事故）** | 1 | 版本→system 档案映射只认精确串 "3.17.1"：只升版本会**静默回落 legacy 文案** ⇒ 405 / 3012 |
| P1（建议同步） | 3 | ①README/.env.example 的版本叙述；②config.go 版本演进注释；③发布时 AppVersion 递增（仅当用户要求发版） |
| P2（可选/观察） | 3 | ①档案 block1 缺 276 B（Interactive views，既有偏差，不参与校验）；②x-zcode-session-features 仍建议不发；③host↔CLI 的 MCP workspace 路由（ACP 层，与本项目无关） |
| 明确无需改 | 6 类 | 伪装头集合、端点/域名、UA 组成、模型目录、minCompatibleVersion 语义、system 文案内容 |

**一句话**：3.17.2 本身没改任何 wire 形态（头/端点/UA/文案/模型目录全部零变化），但**我们自己的版本-文案映射是「精确字符串匹配」**，所以「只升版本号」这一步会把 2026-11-01 的事故原样重演一遍。升版本与补映射必须成对。

---

## 二、触点总表

| # | 文件:行 | 现状 | 3.17.2 事实 | 结论 |
|---|---|---|---|---|
| 1 | internal/config/config.go:488 | ZcodeClientVersion = env("ZCODE_CLIENT_VERSION", "3.17.1") | 客户端版本常量 = 3.17.2（host/index.js kp） | **P0 改默认值** |
| 2 | internal/config/zcode_system_profile.go:26 | if strings.TrimSpace(version) == "3.17.1" | 3.17.2 文案与 3.17.1 逐字节相同 | **P0 补映射**（否则回落 legacy） |
| 3 | internal/config/zcode_system_profile_test.go:14 | 断言 3.17.2 → ZcodeSystemProfileLegacy | 应为 3171 档案 | **P0 改断言**（当前显式锁死错误行为） |
| 4 | internal/config/version_test.go:33 | 断言默认值 == "3.17.1" | — | P0 改期望 |
| 5 | internal/config/version_env_test.go:19,21 | 期望 "3.17.1" / "ZCode/3.17.1" | — | P0 改期望 |
| 6 | internal/upstream/zcode_system_3171.json | identity 1235 B / sha16 4058874023c7541f | 与 3.17.2 逐字节相同 | **无需新档案**（可直接复用） |
| 7 | internal/upstream/request.go:51-56,62-76 | 按档案选内嵌 JSON + 按档案键控缓存 | 无需改 | 无需改 |
| 8 | internal/config/profile.go:184-187 ModelUserAgent() | ZCode/<ver> ai-sdk/provider-utils/4.0.27 runtime/node.js/24 | ai-sdk 4.0.27、Electron 44.7.0 / Node v24.21.0 全同 | 无需改 |
| 9 | internal/config/profile.go:205-220 ZcodeSourceHeaders() | 11 项来源头 | buildZCodeSourceHeadersFromContext 827 B 逐字节等价（5 字符为压缩改名） | 无需改 |
| 10 | internal/upstream/request.go 模型请求头表 | 含 X-ZCode-Agent: glm 等 | 端点与头集合零变化；x-zcode-session-features 7 值枚举未变 | 无需改 |
| 11 | 端点常量（zcode-plan / billing / event/report / client/configs 等） | — | out/ 路径字面量 143→143、URL 168→168，0 增 0 删 | 无需改 |
| 12 | 模型目录 / /v1/models | GLM 系列 | zcode-builtin.json 逐字节相同 | 无需改（Kimi/MiniMax 仍不可走官方额度） |
| 13 | internal/captcha/*、quota、claim 的 app_version 查询参数 | 取自 config.ZcodeClientVersion | 版本值变 | 随 P0-A 自动生效 |
| 14 | README.md:56、.env.example:60-61 | 写 3.17.1 | — | P1 文档 |
| 15 | internal/config/config.go:481-487 注释 | 版本演进史止于 3.17.1 | — | P1 注释 |

---

## 三、P0-A：客户端版本同步（老规矩，一条）

### 3.1 依据

- 3.17.2 的版本常量：host/index.js `kp = "3.17.2"`（3.17.1 为 `Sp`），host-shared `nW = "3.17.2"`（compat manifest 默认值）；User-Agent / X-ZCode-App-Version / app_version 全部由它派生。
- 历史结论（docs/analysis-zcode-client-activation-diff.md §1）：**上游按 app_version 判定活动套餐投放资格**，版本落后会直接导致 preview 不投放。这是硬门槛，不是装饰。
- 本次 3.17.2 相对 3.17.1 的 wire 面**没有任何其它变化**（见主报告 §4-§9），所以「改版本 + 补映射」就是全部。

### 3.2 改动清单（5 处代码/测试 + 3 处注释/文档）

1. internal/config/config.go:488："3.17.1" → "3.17.2"。
2. internal/config/config.go:487 后追加注释（沿用既有格式）：
   // 2026-11-03 同步 3.17.2：system 文案（block0 35 B / 身份段 1235 B sha16 4058874023c7541f）
   // 与 3.17.1 逐字节相同，复用 3171 档案；伪装头/端点/UA/模型目录均未变，仅版本值更新。
3. internal/config/zcode_system_profile.go:26：扩成精确集合（见 P0-B）。
4. internal/config/zcode_system_profile_test.go:14：{"3.17.2", Legacy} → {"3.17.2", ZcodeSystemProfile3171}。
5. internal/config/version_test.go:33：期望值 → "3.17.2"。
6. internal/config/version_env_test.go:19,21："3.17.1" → "3.17.2"、"ZCode/3.17.1" → "ZCode/3.17.2"。
7. internal/upstream/system_profile_test.go 用例表：**追加一行** 3.17.2（同一组基线 1235 / 4058874023c7541f），把「3.17.2 与 3.17.1 文案相同」变成回归断言。
8. README.md:56、.env.example:60-61：版本号与回退口径（回退目标改为「上一个有精确档案的版本」= 3.17.1）。

---

## 四、P0-B：版本→档案映射缺口（本轮真正的新风险）

### 4.1 事实链

- config.ZcodeClientVersion（config.go:488）→ ZcodeSystemProfileKey()（zcode_system_profile.go:33）→ **ZcodeSystemProfileFor()（:25-30，只认精确串 "3.17.1"）** → 其余一切（含 "3.17.2"、"3.18.0"）返回 ZcodeSystemProfileLegacy。
- 下游三处消费：request.go:51-56（选内嵌 JSON）、request.go:62-76（按档案键控缓存）、request.go:95-125（出站静态块）；另有 gateway/body.go:59-74 的 fallback 三块与 capabilities.go:36-56 的 preserve 判据。

### 4.2 只改版本号会发出什么（数值级）

| 项 | legacy 档案 | 3.17.2 应交的 3171 档案 | 首个分叉位 |
|---|---|---|---|
| block0 | "You are ZCode, an interactive coding agent"（42 B / sha16 46dd360a22c87a92） | "You are ZCode, an interactive agent"（35 B / sha16 7779642ea44a4696） | 下标 30 |
| 身份段 | 1217 B / sha16 fb4777a371fac341 | 1235 B / sha16 4058874023c7541f | 下标 12 |
| 桌面上下文尾段 | 1100 B | 1100 B | 相同 |

即「请求头声称 3.17.2 + system 是 3.16.x 文案」。按 3.17.1 事故已证实的机制（上游逐字节校验第 0 字节到身份段末尾，且与 app_version 成对），**必判 405 / 3012**。

### 4.3 为什么这条路径是「静默」的（5 条护栏全失效）

| 护栏 | 为什么拦不住 |
|---|---|
| version_test.go:32-36「默认版本必须有已验证文案对」 | 实现只比较一个字符串常量；改期望值即绿 |
| system_profile_test.go 用例表 | 只有 3.16.3 / 3.17.1 两行；3.17.2 不在表内 |
| outbound_diagnostics.go:49-57 official_prefix_match / official_identity_match | 拿**当前档案**当基准自比，档案自己是 legacy 就都报 true |
| 网关运行日志 | 无明文版本（只有 hash 化 ProfileRef） |
| 环境变量旁路 | 任何人今天设 ZCODE_CLIENT_VERSION=3.17.2 就踩中，CI 不红 |

### 4.4 修法对比

| 方案 | 做法 | 优点 | 风险 | 结论 |
|---|---|---|---|---|
| **A 精确映射（推荐）** | "3.17.1"、"3.17.2" → 3171；未知版本仍回落 legacy | 每个新版本强制人工确认一次；与 3.17.2 断言形成「改一处、红一次、想一遍」 | 下次忘改会静默——但可由新增断言立刻发现 | **采用** |
| B 区间/语义化 | >= 3.17.1 → 3171 | 以后小版本免改代码 | 把「新版本静默复用旧文案」制度化，3.18 改文案时会静默发错且无告警 | **不采用** |
| C 版本由档案派生 / 启动 fail-fast | 档案声明适用版本区间，无精确映射即拒绝启动或打 WARN | 从根上消灭错配 | 改动面大，改变 ZCODE_CLIENT_VERSION 可自由覆盖的契约 | 中期方向 |
| D 静默变响（可与 A 并行） | 增加 ZcodeSystemProfileForExact(version) (profile, exact bool)，未命中时打 WARN | 成本极低 | 无 | **建议同期做** |

### 4.5 建议新增的断言

1. `TestDefaultClientVersionHasExactSystemProfile`：默认版本必须命中精确映射（未设环境变量时生效）。
2. `TestZcodeSystemProfileFollowsClientVersion` 追加 3.17.2 行：期望 3171 档案、prefix/identityHead、1235 B、sha16 4058874023c7541f。
3. （可选）outbound_diagnostics 增加 profile_unmapped 字段。

---

## 五、P1：文档与发布口径

1. README.md:56：「默认客户端版本为 3.17.1」→ 3.17.2；回退说明改成「回退到上一个有精确档案的版本（当前为 3.17.1）」，不要写死 3.16.3。
2. .env.example:60-61：注释示例改 3.17.2 / ZCode/3.17.2。
3. **发版才做**：internal/config/config.go:511 的 AppVersion（当前 2.11.11-go）递增 + docs/releases/v2.11.12-go.md。用户本次只说「看影响」，**未要求发版**，故不落地。

---

## 六、P2：可选与观察

1. **档案 block1 缺 276 B（既有偏差）**：两份档案都缺 Desktop Context 里的 "### Interactive views" 及后两行；3.15.3/3.17.2 的 CLI 都会输出这 3 行。位于身份段之后，**不参与上游校验**；只有做「缓存前缀对齐」时才值得补（补时按「无 Gen UI 目录」分支取文案）。
2. **x-zcode-session-features**：7 值枚举未变；仍建议**不发**（发错值比不发风险大，且一条默认会话本来就不发）。
3. **host↔CLI 的 MCP 身份头 workspace 路由**：ACP（stdio JSON-RPC）层的 -32602 门禁，网关不参与，仅记录。

---

## 七、明确无需改（附证据，避免下一轮重复劳动）

| 面 | 结论 | 证据 |
|---|---|---|
| system 提示词文案 | 与 3.17.1 逐字节相同 | block0 35 B/7779642e…；身份段 1235 B/4058874023c7541f；CLI 内 "an interactive coding agent" 命中 0 |
| 伪装来源头集合 | 逐字节等价 | 827 B ↔ 827 B，仅 5 字符（压缩标识符改名） |
| 端点/域名 | 零变化 | 路径 143→143、URL 168→168、域名计数全同 |
| UA 组成 | 零变化 | ai-sdk 4.0.27；Electron 44.7.0 / Node v24.21.0 ⇒ node.js/24 |
| 模型目录 | 逐字节相同 | zcode-builtin.json sha256 62412a27… |
| minCompatibleVersion | 语义未变（非模型门槛） | 命中 31→31；仍只在 Source Server 协商 |
| 前端 | 零 pin | frontend/ 与 dist 内 3.17.1 命中 0；/meta 只暴露产品版本 |
| CI/工作流 | 零 pin | .github/ 内无版本串 |

---

## 八、与上一轮（3.17.1）的衔接

- 上一轮影响笔记的结论「新增客户端能力都在客户端本地面，没有一个改变我方发给上游的请求形态」**在本轮继续成立**，且这次连 system 文案都不变。
- 上一轮把「CLI 提示词文本变化」记为 P2，结果酿成事故（见事故报告 §5 方法论复盘）。本轮把它提为**第一优先级并做了字节级双向校验**（block0 + 身份段，且用 3.15.3 反向验证方法可信）。
- 上一轮遗留的「system block 抓包复核」本轮无法闭环（本机没有 3.17.2 抓包）。仍然建议：下次客户端产生真实会话后，按 docs/runbook-zcode-prompt-parity.md 抓一版 model-io 复核，并顺便确认 block1 的 276 B 偏差。

---

## 九、风险与未验证

1. **本轮无 3.17.2 真实抓包**：所有「未变」结论来自静态重建 + 与 3.17.1 抓包校准档案的字节比对；上线前仍需单账号探针 200 兜底。
2. **部署侧可能已硬设版本**：若容器/环境显式设了 ZCODE_CLIENT_VERSION=3.17.1 或 UPSTREAM_USER_AGENT=ZCode/3.17.1，改默认值不会生效——升级后必须核对容器 env。
3. **docker-compose.yml 至今未透传 ZCODE_CLIENT_VERSION**（本轮实测确认），所以「写进 .env 就能回退」是错的，回退需改 compose 或 -e。
4. 3.17.1 的 Electron/Chromium/Node 版本号沿用上一轮记录（本机 3.17.1 的 exe 已不在盘上）；3.17.2 侧为实测。
5. 安装包体积：待安装包 233,829,888 → 233,839,504 B（+9,616 B），「明显减小」在 Windows 口径不成立。（注：另一份分报告曾把主程序 ZCode.exe 246,332,312 B 误当安装包对比，已按实测口径纠正。）

---

## 十、建议落地顺序（把风险压到最低）

1. **先只做 P0-B**（补映射 + 改断言 + 新增精确映射断言），**不动默认版本** → 合并。此时生产行为零变化，但 ZCODE_CLIENT_VERSION=3.17.2 立刻可用且被测试守住。
2. 用 `zcode2api probe`（--dry-run 看指纹 / --account 单账号真请求）对 3.17.2 做实测，拿到 200 证据。期望输出：client_version=3.17.2、system_profile=3.17.1、system_identity_chars=1235、system_identity_sha16=4058874023c7541f。
3. 再发版把默认值切到 3.17.2（P0-A）+ 文档（P1），灰度一个实例观察 24h。
4. 任何一步出现 405 / 3012：回退 ZCODE_CLIENT_VERSION=3.17.1 并重启（compose 需 -e），不需要换镜像。

说明：按 AGENTS.md，本地不跑 go test；改动推分支后由 GitHub Actions 验证，用 gh run watch 看结果。
