# ZCode 3.17.2 逆向分析报告

> 日期：2026-11-03（Asia/Shanghai）。对象：本机安装的 ZCode 桌面客户端 3.17.1 → 3.17.2（app.asar buildTime 2026-11-02T06:32:36.912Z，buildCommitId a2d1ba73）。
> 方法：**只读静态逆向**。解包 app.asar 与 resources、抽取随包 CLI、按运行时语义重建 system 提示词、与项目内 3.17.1 抓包校准档案做**字节级**比对；全程未执行客户端代码、未用真实账号发起请求、未联网。
> 产物：本文 + [影响笔记](analysis-zcode-v3.17.2-project-impact-notes-20261103.md)；机器可读证据在 temp/zcode-v3172-20261103/（findings/ 四个分报告 + 差分 JSON）。

---

## 0. 一句话结论

**3.17.2 是「版本号 + 客户端本地功能」级别的补丁，模型 wire 面零变化。**

- system 提示词：block0（35 B / sha16 7779642ea44a4696）与身份段（1235 B / sha16 4058874023c7541f）与 3.17.1 **逐字节相同**（同算法在 3.15.3 上能重建出 legacy 档案，属双向校验，非自证）。
- 伪装来源头：buildZCodeSourceHeadersFromContext 两版均 827 B、11 个头名与取值来源全同（仅 3 个压缩标识符改名）。
- 端点：out/ 路径字面量 143 → 143，**0 增 0 删**；URL 168 → 168、域名计数全同（zcode.z.ai 35/35）。
- UA 组成：ai-sdk/provider-utils 4.0.27、Electron 44.7.0 / Chromium 152.0.7977.130 / Node v24.21.0 全同 ⇒ runtime/node.js/24 不变。
- 模型目录：resources/config/provider/zcode-builtin.json 与 3.17.1 **逐字节相同**。
- minCompatibleVersion：命中 31 → 31 逐文件相同，仍只用于 Source Server 兼容协商，**不是**模型 API 门槛。

**对本项目而言，P0 只有一条**：把伪装客户端版本跟到 3.17.2 —— 并且**必须同时**补上 internal/config/zcode_system_profile.go 的精确档案映射（现在只认字符串 "3.17.1"，改版本号会静默回落 legacy 文案，正好复刻 2026-11-01 的全量 405/3012 事故形态）。

---

## 1. 样本与口径

| 项 | 3.17.1（基线） | 3.17.2（本轮） |
|---|---|---|
| 安装根 | D:\Programs\ZCode | 同 |
| app.asar | 374,627,566 B | 374,653,670 B（sha256 b11a6bc5bb957af5b735701d94452ec6c3119a5d957fb772eb40a3cc94bcb24c） |
| 解包 app/（asar 全量） | 31,849 文件（temp/zcode-asar） | 31,849 文件（temp/zcode-v3172-20261103/app） |
| resources/ 外部资源 | 3,117 文件（哈希表已留存） | 3,117 文件（整份副本已留存） |
| 随包 CLI | 未留存字节 | resources/glm/zcode.cjs 15,408,074 B（sha d9afd58d…） |
| 真实模型请求抓包 | 有（2026-11-01/02，~/.zcode/cli/rollout） | **无**（本机最新 rollout 2026-11-02 09:02，早于 3.17.2 安装） |

差分口径：清单差分用 sha256（identical 31,762 / changed 20 / added 51 / removed 51，其中 51+51 是 out/ chunk 与 renderer 资源的哈希改名）；代码级差分用两份独立口径互相印证 —— esbuild keepNames 函数标签集合、TypeScript AST 抽出的全部字符串字面量多重集。

---

## 2. 构建与安装身份

| 项 | 3.17.1 | 3.17.2 | 结论 |
|---|---|---|---|
| ZCode.exe FileVersion / ProductVersion | 3.17.1.9553 | **3.17.2.9661** | build 号 +1,108 |
| Electron | 44.7.0 | 44.7.0 | 未变 |
| Chromium | 152.0.7977.130 | 152.0.7977.130 | 未变 |
| 内置 Node | v24.21.0 | v24.21.0 | 未变 ⇒ UA 的 runtime/node.js/24 不变 |
| app.asar | 374,627,566 B | 374,653,670 B（+26,104 B = out/ 各文件增量之和） | 增量级 |
| out/ 文件数 | 4,227 | 4,227 | 只有 51 处 chunk/资源改名 |
| resources 外部资源 | 3,117 | 3,117（0 增 0 删，38 改） | 38 处改动里 34 个是原生模块重编译（sharp/node-pty/ssh2/koffi/ax_native 等） |
| 待安装安装包（win-x64） | 233,829,888 B | 233,839,504 B（**+9,616 B**） | 「安装包体积明显减小」在 Windows 口径**不成立**（与 3.17.1 那轮的结论一致） |

安装包完整性：pending/ZCode-3.17.2-win-x64.exe 实算 sha512(base64) 与 update-info.json 的 sha512 字段一致（= 3n9TNUm…），说明下载完整；但本机 resources/app-update.yml 的更新源是实验用的 http://localhost:8081，**安装包来源建议另行复核**。

资源面的两个纠偏：

1. **glm/packages/zcode-site-plugin 不是 3.17.2 新增**：它在 3.17.1 的基线哈希表里已有 25 条记录（package.json、.zcode-plugin/plugin.json、commands/site-{badge,deploy,logs,offline}.md 等）。更新日志里的「新增 ZCode Sites 官方插件」在 3.17.2 里找不到对应的新包。
2. **config/provider/zcode-builtin.json 逐字节相同**（sha256 62412a27…，260,787 B）⇒ 「新增 Kimi K3.1 Pro、MiniMax M3.1 Flash 等模型配置项」不是目录变更。

---

## 3. 版本常量（唯一实质变更点）

| 位置（3.17.2 out/） | 3.17.1 | 3.17.2 | 用途 |
|---|---|---|---|
| host/index.js（fmt L32697） | Sp = "3.17.1" | **kp = "3.17.2"** | host 进程 appVersion（伪装头取它） |
| host shared chunk（fmt L30297） | tW = "3.17.1" | **nW = "3.17.2"** | createCompatibilityManifest 的 minCompatibleVersion 默认值 |
| host shared chunk（fmt L28803） | li = "3.17.1", 025f7f39, 2026-10-29T12:42:31.412Z | li = "3.17.2", a2d1ba73, 2026-11-02T06:32:36.912Z | appVersion + commit + buildTime 三元组 |
| main/chunk-B7KMLVFU.js | "3.17.1" | "3.17.2" | main 侧 appVersion 副本 |
| main shared / preload×7 / scheduler / renderer | 同上 | 同上 | 派生副本 |

out/ 里 3.17.x 字面量：3.17.1 共 16 文件 30 处 → 3.17.2 共 17 文件 31 处。**唯一新增的一处**是 host 入口共享块内 SQLite 迁移记录 {id:"0010_remote_task_ownership", appVersion:"3.17.2", touchedTables:["tasks"]}（客户端本地 DB，与 wire 无关）。

app_version 的**注入点数量未变**：host/index.js 11、main/index.js 15、scheduler 3、preload 1，两版逐条同位置同数量 ⇒ 没有新增任何「带 app_version 的请求」。

---

## 4. system 提示词：与 3.17.1 逐字节相同（决定性结论）

### 4.1 为什么这节最重要

上游对模型请求的校验点是「system 第 0 字节 → 身份段末尾」，且与声明的 app_version 成对比较。2026-11-01 的事故证明：只升版本号、文案不变 = 全量 405 / 3012（[事故分析](analysis-zcode-3.17.1-405-incident-20261101.md)）。
因此每次客户端升版，**第一个要回答的问题就是这两段文案有没有变**。

### 4.2 方法与双向校验

从 3.17.2 的 resources/glm/zcode.cjs 抽取构建函数，按桌面主轮次的真实参数把模板表达式**展开**（不搬运字符串，避免 3.17.1 踩过的「${…} 原样抄进档案」坑）：

- CLI Prefix：hBa = "You are ZCode, an interactive agent"（buildCliPrefixSection / q8e）。
- 身份段：yBa(e,t) = [["", e ? _Ba : gBa, "", cBo].join(NL), "", kDn(t)].join(NL)；桌面默认 e=false ⇒ 以 gBa 开头。
  - gBa = "You are an agent working with the user toward their goals, using your own judgment along the way."（= 3.17.1 的 dBa 变体）
  - _Ba = "You are an interactive agent that helps users according to their Output Style, …"（仅配置了 Output Style 时启用）
  - cBo = 安全检查段，459 字符
  - kDn(e=!0) = Harness 6 条（含前置空格的 " - " 项目符号、第 4 条条件式、Reference code as file_path:line_number）

| 项 | 3.17.2 重建结果 | 3.17.1 抓包校准档案（internal/upstream/zcode_system_3171.json） | 判定 |
|---|---|---|---|
| block0 | 35 B，sha16 7779642ea44a4696 | 35 B，sha16 7779642ea44a4696 | **逐字节相同** |
| 身份段（到 # ZCode Desktop Context 之前） | 1231 字符 / **1235 B**，sha16 **4058874023c7541f** | 1231 字符 / 1235 B，sha16 4058874023c7541f | **EQUAL: True** |
| Harness 行数/缩进 | 6 行、带前导空格 | 同 | 相同 |
| 「coding」旧变体（an interactive coding agent）出现次数 | **0** | — | 已彻底退场 |

反向校验（防止「用同一份档案自证」）：把同一套重建算法作用在 3.15.3 的 CLI 上，得到 legacy 档案的 1217 B / sha16 fb4777a371fac341 —— 与项目内 internal/upstream/zcode_system.json 的 identity 完全吻合。**两代档案同时命中，说明重建方法可信。**

⇒ **结论：3.17.2 不需要重做 system 档案**，现有 3171 档案可直接服务 3.17.2。

### 4.3 装配规则（块边界与 cache）

| 块 | 组成 | cache_control |
|---|---|---|
| block0 | cli_prefix（hBa） | **无** |
| block1 | stable 段按 \n\n 拼接：身份段 + Desktop Context | ephemeral |
| block2 | dynamic 段（前置 \n\n） | ephemeral |

项目档案对 block0 的 cache_control 在出站时剥离（runbook 记录），与 CLI 行为一致，无需改动。

### 4.4 顺带发现（既有偏差，非本轮引入，不参与校验）

项目两份档案的 block1 都**缺** Desktop Context 里的 "### Interactive views" 及后两行（共 276 B）；3.15.3 的 CLI 同样会输出这 3 行。该段位于身份段**之后**，不影响「第 0 字节 → 身份段末尾」的校验；对缓存前缀对齐（cache 命中）可能有细微影响，属可选优化项（见影响笔记 P2）。

---

## 5. 伪装来源头（零变化）

buildZCodeSourceHeadersFromContext 两个版本都是 **827 B**，逐字节差异只有 **5 个字符**，且全部是 esbuild 压缩后的局部标识符改名（X1→Q1、Pue→Tue、Q1→eW）。

| 头名 | 取值来源（3.17.2 证据） | 3.17.1 |
|---|---|---|
| User-Agent | "ZCode/" + appVersion（空则 ZCode/unknown） | 同 |
| HTTP-Referer | 缺省 ni = https://zcode.z.ai | 同 |
| X-Title | Z Code@electron（桌面 spawn 参数含 app-server） | 同 |
| X-ZCode-App-Version | appVersion（非空才发） | 同 |
| X-Platform | platform-arch | 同 |
| X-Release-Channel | test / production（本机为 production） | 同 |
| X-Client-Language | Intl locale，缺省 unknown | 同 |
| X-Client-Timezone | Intl timeZone，缺省 unknown | 同 |
| X-Os-Category | darwin→macos / win32→windows / 其它→linux | 同 |
| X-Os-Version | node:os.version() | 同 |
| X-Device-Mid | telemetry-state.json 的 deviceMid（有才发） | 同 |

所有值先过 normalizeZCodeSourceHeaderValue（trim 后必须匹配 /^[\x20-\x7e]+$/，否则丢弃）。X-ZCode-* 前缀在整个 out/ 只有 X-ZCode-App-Version 一个（两版各 4 次）。

⇒ **头集合、头顺序、取值规则、条件发送逻辑全部无需改；唯一要同步的是 appVersion 的取值（由版本常量派生）。**

---

## 6. 端点与域名（零变化）

沿用上一轮同一套正则做对照（口径一致才可比）：

| 指标 | 3.17.1 | 3.17.2 | 增 | 删 |
|---|---|---|---|---|
| out/ 路径字面量 distinct 键 | 143 | 143 | **0** | **0** |
| URL distinct | 168 | 168 | 0 | 0 |
| 域名计数（zcode.z.ai / bigmodel.cn / api.z.ai / zcode.chatglm.site） | 35 / 14 / 5 / 11 | 35 / 14 / 5 / 11 | 0 | 0 |

143 个键里唯一计数变化：/usr/bin/osascript 3 → 4（macOS 取应用图标脚本，多了 resolveDarwinAppIconDataUrl 一处调用）。

与项目相关的端点逐条一致：/api/v1/client/configs（11/11）、/api/v1/client/scenes（6/6）、/api/v1/coding-plan/reset（2/2）、/api/v1/off-peak（1/1）、/api/v1/off-peak/anthropic/v1/messages（1/1）、/api/v1/highspeed（1/1）与 /draw（1/1）、/api/v1/oauth/token（8/8）、/api/v1/event/report（1/1）、/api/v1/mcp/usage（2/2）、/api/v1/remote/notification/{events,status}（2/2，上一轮新增）。

随机包 CLI 侧（zcode.cjs）：模型与计费端点 /api/v1/zcode-plan、/api/v1/zcode-plan/anthropic、/zcode-plan/billing/current|balance 同样无变化；相对 3.15.3 的 7 条新增（remote/device、remote/token、remote/ws2、ultra、ultra-zai、sites）**无法归因到 3.17.2**（3.17.1 的 CLI 字节本机未留存），且全部与模型网关无关。

---

## 7. UA 组成（零变化）

| 组成 | 事实 |
|---|---|
| ai-sdk/provider-utils 版本常量 | gto = "4.0.27"（与 3.15.3 逐字节相同） |
| 运行时段 | 走 navigator.userAgent 分支 → runtime/node.js/<major>；process.version.substring(0) 是死分支 |
| 真实模板 | ZCode/<appVersion> ai-sdk/provider-utils/4.0.27 runtime/node.js/<node major> |
| node major | Electron 44.7.0 内置 Node v24.21.0 ⇒ **24** |

⇒ 项目 internal/config/profile.go 的 ModelUserAgent()（ZCode/<ver> + 4.0.27 + node.js/24）**无需改动**。

---

## 8. 模型目录与「新增模型配置项」的真相

- resources/config/provider/zcode-builtin.json：260,787 B，sha256 62412a27…，**与 3.17.1 逐字节相同** ⇒ 官方面模型目录没有变化。
- Kimi K3.1 Pro / MiniMax M3.1 Flash 属于 **provider 模板层**（templateRules / templateModelRules，第三方 BYO-key 模板）元数据，3.17.1 的 rev43 里已存在；更新日志这句话描述的是设置页/模型选择器的**暴露方式**，不是官方 coding-plan 新增可用模型。
- 官方账户可用模型集合仍为 GLM 系（GLM-5.3 / 5.3-Flash / 5.2 / 5-Turbo × 各 providerRules）。

⇒ 项目 /v1/models 与模型映射**不需要新增**；如需对外说明，最多在文档里写清「Kimi/MiniMax 需自备第三方 key，不能走官方额度」。

---

## 9. minCompatibleVersion 语义复核

| 项 | 3.17.1 | 3.17.2 |
|---|---|---|
| minCompatibleVersion 命中次数（out/，排除 node_modules） | 31 | 31（逐文件相同） |
| createCompatibilityManifest 位置 | host-shared fmt L30306 | L30308（区间仅版本字符 + 标识符改名） |
| evaluateSourceServerCompatibility 位置 | fmt L30353 | L30355（区间仅 4 字符差） |
| 消费者 | host/index.js 的 Source Server needs-upgrade 7 处 | 同 7 处 |

⇒ 语义仍是「桌面端 ↔ 企业自建插件源/源服务器归档」的兼容协商，**没有**变成模型/计费 API 的版本门槛。不要用它推断「上游会拒绝低版本客户端」。

---

## 10. 代码级差分：变化落在哪里

### 10.1 函数标签（esbuild keepNames）

| bundle | 3.17.1 | 3.17.2 | 判定 |
|---|---|---|---|
| host/index.js | 3,910 | 3,910 | 完全相同（上一轮是 +187/−26，本轮 0/0） |
| host shared | 962 | 962 | 相同 |
| host 入口共享块 | 329 | 331（+2） | 本地 SQLite 迁移 |
| main/index.js | 1,617 | 1,619（+7/−5） | 本地预览 / 图标 / 托盘角标移除 |
| main shared、main xiausx7w、scheduler、preload | 775 / 488 / 985 / 595 | 同 | 相同 |

新增标签：discoveredProject、repairRemoteTaskOwnership（本地 DB）、createBrowserLocalPreview、installBrowserLocalPreview、isContained（本地预览路径校验）、isFullyTransparent、getFileIconDataUrl、getWorkspaceIconDataUrl、resolveDarwinAppIconDataUrl（图标）。移除：applyDesktopTrayUnreadBadge 等整族 5 个（托盘未读角标）。

**wire/HTTP/认证/计费/模型 分组的新增或移除：0 个。**

### 10.2 字符串字面量多重集差分（语义变化的完整边界）

| bundle | 新增 | 移除 | 计数变化 | 结论 |
|---|---|---|---|---|
| host shared | 0 | 0 | 0 | 只有版本号/commit/buildTime |
| scheduler | 0 | 0 | 0 | 只有 3.17.1→3.17.2（2 处） |
| host/index.js | 3 | 0 | 5 | prepareForkCreation、workspace-routing、MCP 身份头告警文案 |
| host 入口共享块 | 16 | 1 | 7 | 本地 DB 迁移 |
| main/index.js | 24 | 9 | 32 | 本地预览 + 图标 + 托盘 |
| main shared | 2 | 0 | 0 | 版本号 + zcode-html + IPC 频道 |

### 10.3 唯一与 wire 沾边的新增（仍与本项目无关）

host↔CLI 的 ACP（stdio JSON-RPC）新增了「官方 MCP 身份头请求」的工作区路由校验：方法 interaction/requestOfficialMcpAuthHeaders 现在必须带能路由到本连接已注册工作区的 workspace 参数，否则返回 -32602「Request workspace is not owned by this CLI」，并打 workspace-routing 阶段告警。这是本机进程内协议，网关不参与，判定 P2。

---

## 11. 更新日志逐条溯源（摘要）

完整逐条表（36 条 → 代码证据 → 影响分级）见 [features.md](../temp/zcode-v3172-20261103/findings/features.md)。要点：

- **wire 相关：0 条。** 没有任何一条更新日志对应「新增头 / 新增字段 / 新增查询参数 / 新增端点 / 改动提示词」。集合级硬证据：头名字面量 38↔38、/api/ 路径 37↔37、绝对路径 195↔195、zcode* 标识 103↔103 全等；host 主 bundle 的 esbuild keepNames 标签集 3,775↔3,775（零增删）。
- **36 条里只有 6~7 条能在 3.17.1→3.17.2 的字节差分里找到改动**：修 11/16（host 新增本地 SQLite 迁移 0010_remote_task_ownership 与 repairRemoteTaskOwnership）、修 21（main/index.js 的本地 HTML 预览沙箱：createBrowserLocalPreview / isContained / nosniff）、修 22（渲染层新增 63 条 MCP 失败建议文案）、修 23（host 的 workspace-routing 门禁）、修 26（Windows 文件用默认应用打开，Path.GetFullPath）、修 27（macOS 应用图标兜底）。其余条目要么 3.17.1 已存在，要么本机无法证实（逐条标注了证据强度）。
- **安全类 5 条全部只在客户端本地生效**：计划模式权限枚举与子任务权限继承逻辑两版逐字符未变；修 21 是本地自定义协议 zcode-html://（含 realpath 包含性校验、拒绝网络路径、仅 GET/HEAD）；修 23 是 Electron↔host 的 JSON-RPC 门禁（错误码 -32602，不是 HTTP 状态）；「自定义命令脚本需权限确认」未在 out/ 定位到（疑在随包 CLI）。**没有一条改变模型请求形态。**
- **安装包体积「明显减小」**：Windows x64 口径不成立 —— 待安装安装包 233,829,888 → 233,839,504 B（+9,616 B），主程序 ZCode.exe 246,332,312 B、asar +26,104 B，三个口径都是变大。
- **「新增模型配置项」**：目录未变（见 §8）。
- **「新增 ZCode Sites 官方插件」**：插件包在 3.17.1 已存在（见 §2）。
- **日志未提及、但属于「新协议形状」的三项**：本地协议 zcode-html://、本地 IPC 频道 zcode:browser-view-load-url、host 本地能力标记 sessionLifecycleWorkspace —— 全部是客户端进程内机制，不进入发往 zcode.z.ai 的请求。

---

## 12. 与本项目的关系（结论表）

| # | 面 | 3.17.2 事实 | 项目动作 | 等级 |
|---|---|---|---|---|
| 1 | 客户端版本号（ZcodeClientVersion） | 3.17.2 | 升到 3.17.2（头/UA/app_version 全派生） | **P0** |
| 2 | 版本→文案档案映射（zcode_system_profile.go） | 3.17.2 文案 == 3.17.1 | **补一条精确映射**（否则回落 legacy = 405/3012） | **P0** |
| 3 | system 档案内容 | 逐字节未变 | 无需新档案 | 无 |
| 4 | 伪装来源头集合 | 逐字节等价 | 无需改 | 无 |
| 5 | 端点/域名 | 143→143、168→168 | 无需改 | 无 |
| 6 | UA 组成（ai-sdk / node） | 4.0.27 / 24 | 无需改 | 无 |
| 7 | 模型目录 | 逐字节同 | 无需改 | 无 |
| 8 | minCompatibleVersion | 语义未变 | 无需改 | 无 |
| 9 | x-zcode-session-features 枚举 | 7 值未变 | 仍建议不发（保持现状） | P2 |
| 10 | 档案 block1 缺 276 B（Interactive views） | 既有偏差 | 可选补齐（仅影响缓存对齐） | P2 |
| 11 | 文档/注释里的 3.17.1 | — | README、.env.example、注释同步 | P1 |
| 12 | host↔CLI MCP workspace 路由 | 新增（ACP 层） | 无（网关不参与） | P2 |

---

## 13. 不确定与风险

1. **本轮无 3.17.2 真实抓包**：所有「未变」结论来自静态重建 + 与 3.17.1 抓包校准档案的字节比对，未做线上观测。上线前仍需按 runbook 用单账号探针做一次 200 验证。
2. **3.17.1 的 Electron/Chromium/Node 版本号沿用上一轮记录**（本机 3.17.1 的 exe 已不在盘上）；本轮 3.17.2 侧为实测（Chrome/152.0.7977.130、Electron/44.7.0、node.js/v24.21.0）。
3. **安装包来源**：本机 app-update.yml 指向 http://localhost:8081（实验源），安装包完整性只验证到「sha512 与 update-info.json 自洽」，未验证 CDN 来源。
4. **CLI 的 +2,153 B 未能逐块归因**（3.17.1 的 zcode.cjs 本机未留存）；但从「block0/身份段/UA/头/端点 五条链路全部逐字节相同」可判定变化落在非 wire 代码。
5. 未逐文件分析 asar 内 node_modules 的依赖树（清单差分显示无依赖版本变化，只有 chunk 改名与 16 个 unpacked 占位）。

---

## 14. 证据索引

| 证据 | 位置 |
|---|---|
| 3.17.2 解包（asar 31,849 文件） | temp/zcode-v3172-20261103/app |
| 3.17.2 resources 整份副本（含 zcode.cjs、builtin.json） | temp/zcode-v3172-20261103/resources |
| 3.17.1 基线解包 | temp/zcode-asar |
| 全量清单差分 | temp/zcode-v3172-20261103/full-package-comparison.json |
| CLI wire 分报告 | temp/zcode-v3172-20261103/findings/cli-wire.md（+ .json） |
| ASAR 锚点分报告 | temp/zcode-v3172-20261103/findings/asar-anchors.md（+ .json） |
| 项目侧影响测绘 | temp/zcode-v3172-20261103/findings/project-impact.md |
| 更新日志逐条溯源 | temp/zcode-v3172-20261103/findings/features.md |
| Lead 亲验事实（含字节级重建脚本口径） | temp/zcode-v3172-20261103/findings/lead-verified-facts.md |
| 3.17.1 事故复盘（校验机制依据） | docs/analysis-zcode-3.17.1-405-incident-20261101.md |
| 抓包→档案 runbook | docs/runbook-zcode-prompt-parity.md |
