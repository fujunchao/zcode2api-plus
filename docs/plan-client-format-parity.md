# 计划：请求格式对齐官方 ZCode 客户端 3.14.3

> 依据：本机实装客户端 3.14.3 的打包产物逐字节拆解（`resources/glm/zcode.cjs`）
> 关联：`docs/analysis-riskcontrol-and-client-format-20260924.md`
> 目标不是「让上游看不出」，而是**让我们的出站请求与官方客户端在同一台机器上产生的请求逐字段可比** —— 任何一个单向差异（缺头 / 多头 / 值格式不同 / 互相矛盾）都是可被交叉比对的指纹。

---

## 〇、一句话结论

官方客户端的请求格式是**从一个三元组派生出来的**：`(process.platform, os.release(), process.arch)`。
请求头的 `X-Platform` / `X-Os-Category` / `X-Os-Version`、body 里 `# Environment` 段的 `Platform` / `OS Version`、以及 `Shell`，
全部是这三项的**纯函数**。我们目前把同一批语义散落在 4 个文件里各写一份，于是出现了「头上说 Windows、body 说 Linux」这种**结构性矛盾**。

**补全的核心不是「加几个头」，而是把这套派生关系收敛成唯一来源。**

---

## 一、官方格式基线（权威规格）

### 1.1 端点

| 用途 | 路径 |
|---|---|
| Anthropic 兼容推理 | `{origin}/api/v1/zcode-plan/anthropic/v1/messages` |
| OpenAI 兼容推理 | `{origin}/api/v1/zcode-plan` |
| 计费当前 | `{origin}/api/v1/zcode-plan/billing/current` |
| 计费余额 | `{origin}/api/v1/zcode-plan/billing/balance` |
| 客户端配置（含签名开关） | `{origin}/api/v1/agent/configs` |
| 内建配置下发 | `{origin}/api/v1/client/configs?app_version=..&platform=..` |
| 签名密钥握手 | `{origin}/api/paas/c1f3a7e2/v2/client/get_sign_key` |

生产 `origin` = `https://zcode.z.ai`（可被 `ZCODE_BASE_URL` / `ZCODE_ENDPOINT_ORIGIN` / `ZCODE_PRODUCTION_BASE_URL` 覆盖 —— 这一条对验证极重要，见 §四）。

### 1.2 模型请求头 = **11 个**（`buildCliZCodeSourceHeaders`）

| 头 | 取值公式 | 我方现状 |
|---|---|---|
| `HTTP-Referer` | `endpointOrigin` = `https://zcode.z.ai`（**无尾斜杠**） | ⚠️ 有，但带尾斜杠 |
| `User-Agent` | `ZCode/${appVersion}` | ✅ |
| `X-ZCode-App-Version` | `appVersion` | ✅ |
| `X-Title` | `Z Code@${sourceTitle}`，`sourceTitle` = `cli`（CLI）或 `electron`（app-server/agent-server） | ❌ 缺 |
| `X-Release-Channel` | `ZCODE_ENV==="test" ? "test" : "production"` → **生产恒为 `production`** | ❌ 缺 |
| `X-Client-Language` | `Intl.DateTimeFormat().resolvedOptions().locale` | ❌ 缺 |
| `X-Client-Timezone` | `Intl.DateTimeFormat().resolvedOptions().timeZone` | ❌ 缺 |
| `X-ZCode-Agent` | 恒 `glm` | ✅ |
| `X-Platform` | `${process.platform}-${process.arch}` → `win32-x64` | ❌ 缺 |
| `X-Os-Category` | `darwin→macos` / `win32→windows` / 其余 `linux` | ❌ 缺 |
| `X-Os-Version` | `os.release()` → `10.0.26100` | ❌ 缺 |

外加 ai-sdk anthropic provider 固定注入的 `anthropic-version: 2023-06-01`（我方已有）。
**注意：模型请求头里没有 `X-Device-Mid`。**（见 GAP-7）

### 1.3 非模型请求头 = 11 个（`buildZCodeSourceHeadersFromContext`）

同一套「来源头」，但**差异两处**：**含 `X-Device-Mid`**、**不含 `X-ZCode-Agent`**。
用于 CLI 自己的非模型调用（额度 / 领取 / 激活遥测 / 配置）。
我方 `quota.authHeaders` 与 `claim.authHeaders` 目前**同时**缺 `X-Release-Channel` / `X-Client-Language` / `X-Client-Timezone` / `X-Os-Category` / `X-Os-Version` / `X-Title`，且**多发**了 `X-ZCode-Agent`。

### 1.4 请求归因头 = 5 个（每个模型请求必带）

| 头 | 取值 |
|---|---|
| `x-request-id` | 每请求 UUID（重试时换新） |
| `x-zcode-session-type` | `main` / `subagent` / `other` |
| `x-zcode-trace-id` | trace id |
| `x-session-id` | 会话 id，去掉 `sess_` / `subagent_agent_` 前缀 |
| `x-query-id` | 可选，`query_` 前缀去掉 |

我方：`dropHeaders` 用 `HasPrefix(lower,"x-zcode")` **一刀切**，把官方必带的 `x-zcode-session-type` / `x-zcode-trace-id` 一起丢掉；`x-request-id` 也没有合成。

### 1.5 签名头 = 6 个（`ClientRequestSigningV4`，本次不做）

`X-Client-Ts` / `X-Client-Version` / `X-Client-Sig` / `X-Client-Nonce` / `X-Client-Pow` / `X-App-Id`。
开关：`GET /api/v1/agent/configs` → `data.codingPlanSignature.enable`（缓存 1h）。
失败面是 **401** `VERIFY_SIGNATURE_INVALID` / `VERIFY_APIKEY_EXPIRED`；握手失败与开关不可用都会 **fail-open 转未签名**。
⇒ 与 405 风控无关，**本计划不实现签名**，但要看住开关（GAP-10）。

### 1.6 body 结构（Anthropic messages）

按构造顺序，**带出现条件**：

| 字段 | 出现条件 |
|---|---|
| `model` / `max_tokens` / `temperature` / `top_k` / `top_p` / `stop_sequences` | 恒有 |
| `thinking: {type, budget_tokens?, display?}` | 仅 `thinking.type ∈ {enabled, adaptive}`；`budget_tokens` 仅 enabled，`display` 仅 adaptive |
| `output_config: {effort?, task_budget?, format?}` | 仅当有 `effort` / `task_budget` / JSON schema 输出 |
| `speed` / `inference_geo` / `cache_control` | 各自可选 |
| `metadata: {user_id}` | 仅当 `metadata.userId` 非空 |
| `mcp_servers` / `container` | 可选 |
| `system` / `messages` | 恒有 |
| `context_management: {edits: [...]}` | 可选 |
| `tools` / `tool_choice` | 可选 |

我方 `body.go` 已处理 `output_config.effort`（对齐）。`thinking.type=disabled` 会被上游 400 —— 这是既有约定，不动。

### 1.7 system 分段模型（关键）

客户端把 system 提示词建模成**分段对象**，每段带两个标签：

- `injectionTarget`：`system` 或 `meta_user`
- `cacheHint`：`stable` 或 `dynamic`

排序函数固定为：

```
[ system+stable ] → [ system+dynamic ] → [ meta_user+stable ] → [ meta_user+dynamic ]
```

`system*` 进顶层 `system` 数组；`meta_user*` 附加到用户消息里。

**已知 target=system 的分段：**

| 段名 | source | cacheHint | 内容要点 |
|---|---|---|---|
| CLI Prefix | `cli_prefix` | stable | `You are ZCode, an interactive coding agent` |
| Agent Identity | `identity` | stable | 身份句 + 安全声明 + `# Harness` |
| Workflow Actor Identity | `workflow_actor_identity` | stable | 仅 workflow 模式 |
| Desktop 若干 | 各自 | stable | 桌面端专属 |
| Environment Info | `env_info` | dynamic | `# Environment` 段（公式见 1.8） |
| System Context | `system_context` | dynamic | git 快照（仅当 cwd 是 git 仓库） |
| Memory | `memory` | dynamic | `# Memory` 段 |
| Output Style / Context Management / Session guidance 等 | 各自 | dynamic | 按条件 |

### 1.8 ⭐ 核心公式：一个三元组派生 5 个值

客户端 `detectEnvInfo` 的实现：

```
cwd       = 真实工作目录
platform  = process.platform                         // "win32"（裸平台名，不是 win32-x64）
shell     = basename($SHELL ?? $ComSpec ?? $COMSPEC) // "cmd.exe" / "bash"
osVersion = `${process.platform} ${os.release()} ${process.arch}`   // "win32 10.0.26100 x64"
```

于是：

```
X-Platform        = `${platform}-${arch}`                  = win32-x64
X-Os-Category     = normalizeOsCategory(platform)          = windows
X-Os-Version      = os.release()                           = 10.0.26100
# Environment 的 Platform   = platform                      = win32
# Environment 的 OS Version = `${platform} ${release} ${arch}` = win32 10.0.26100 x64
# Environment 的 Shell      = basename($SHELL ?? $ComSpec)  = cmd.exe
```

`# Environment` 段的完整模板（注意是**单换行**、无 `cache_control`）：

```
# Environment
You have been invoked in the following environment:
- Primary working directory: {cwd}
- Is a git repository: {yes|no}
- Platform: {platform}
- Shell: {shell}
- OS Version: {osVersion}
[- You are powered by the model named {providerId}/{modelId}.]      ← 可选行
```

### 1.9 查询参数：注意 **两套 platform 格式**

| 位置 | 格式 | 公式 |
|---|---|---|
| 请求头 `X-Platform` | `win32-x64` | `${process.platform}-${process.arch}` |
| 查询参数 `platform=` | `windows-x86_64` | `platform==="win32"?"windows":platform` + `arch==="x64"?"x86_64":"aarch64"` |

官方在 `/api/v1/client/configs` 上发的是 **`app_version=<ver>&platform=windows-x86_64`**。
我方 `captcha.go` / `quota.go` / `claim.go` 把 **同一个常量 `config.ZcodeClientPlatform`（`win32-x64`）同时用在两种格式的位置** —— 这是 GAP-8。

---

## 二、差异清单

| # | 差异 | 现状 | 官方 | 影响 |
|---|---|---|---|---|
| **GAP-1** | 推理请求缺 7 个来源头 | `X-Platform`/`X-Os-Category`/`X-Os-Version`/`X-Client-Language`/`X-Client-Timezone`/`X-Title`/`X-Release-Channel` 全缺 | 11 个恒发 | 高：单请求即可判非官方 |
| **GAP-2** | 归因头被丢弃 | `dropHeaders` 的 `HasPrefix("x-zcode")` 一刀切；从不合成 | 5 个必带 | 高 |
| **GAP-3** | **body 与头的平台互相矛盾** | `zcode_system.json` 写死 `Platform: linux-x64` | 与头同源 | **最高：可直接交叉比对** |
| **GAP-4** | `# Harness` 第 3 条是旧版措辞 | `<system-reminder> tags … injected by the harness` | `The system may send updates, reminders, or modifications to rules via mid-conversation system turns…` | 中：版本指纹 |
| **GAP-5** | `# Environment` 是占位+删减 | `unknown` / `no` / 缺可选行 | 真实值、含可选行 | 高（与 GAP-3 同源） |
| **GAP-6** | `HTTP-Referer` 多尾斜杠 | `https://zcode.z.ai/` | `https://zcode.z.ai` | 低但零成本 |
| **GAP-7** | **多发** `X-Device-Mid` | 推理路径发 | 官方模型请求**不发**（仅非模型接口发） | **已修**（阶段 2b）：默认不再发送，开关保留作应急回滚 |
| **GAP-8** | 一个常量当两种 platform 格式用 | `X-Platform: win32-x64` 与 `platform=win32-x64` 同值 | 头 `win32-x64`、查询 `windows-x86_64` | **golden 实测确认**查询用 `windows-x86_64` |
| **GAP-9** | 非模型端点头不齐 | quota/claim 缺 6 头、多 `X-ZCode-Agent` | 见 §1.3 | 中 |
| **GAP-10** | 从不查询签名开关 | 无 | `GET /api/v1/agent/configs` | 低（可观测性）；golden 显示开关为 true 但客户端仍发未签名 |
| **GAP-11** | 模型请求 `User-Agent` 缺 ai-sdk 后缀 | 裸 `ZCode/<ver>` | `ZCode/<ver> ai-sdk/provider-utils/<v> runtime/node.js/<major>` | **已修**（golden 实测） |
| **GAP-12** | `x-query-id` 仅在透传时发送 | 缺失即不发 | **恒发**（官方每轮次生成） | **已修**（golden 实测） |
| **GAP-13** | body 无 `metadata.user_id` | 完全不发 | 恒发，且 `device_id` = 设备指纹 | **已修**（阶段 2b）：与 GAP-7 合并处置——头去掉、body 加上 |

---

## 三、实施计划

### 阶段 1：单一来源重构（地基，无行为变化）

新增 `internal/config/profile.go`：

```go
// ZcodeProfile 伪装身份的唯一定义。所有派生值必须由本结构计算，
// 禁止在别处出现平台/版本/语言/时区字面量（由静态守卫测试强制）。
type ZcodeProfile struct {
    Platform     string // "win32"          ← process.platform
    Arch         string // "x64"            ← process.arch
    OSRelease    string // "10.0.26100"     ← os.release()
    Shell        string // "cmd.exe"        ← basename($SHELL ?? $ComSpec)
    CWD          string // 伪装工作目录（建议给一个真实感路径，如 <用户目录>\projects\demo）
    AppVersion   string // "3.14.3"
    SourceTitle  string // "electron" | "cli"
    ReleaseChan  string // "production"
    Language     string // "zh-CN"
    Timezone     string // "Asia/Shanghai"
}

func (p ZcodeProfile) XPlatform() string   { return p.Platform + "-" + p.Arch }        // win32-x64
func (p ZcodeProfile) OSCategory() string  { /* darwin→macos, win32→windows, else linux */ }
func (p ZcodeProfile) QueryPlatform() string { /* win32→windows, arch x64→x86_64 */ }   // windows-x86_64
func (p ZcodeProfile) EnvironPlatform() string { return p.Platform }                    // win32
func (p ZcodeProfile) EnvironOSVersion() string {
    return p.Platform + " " + p.OSRelease + " " + p.Arch                                 // win32 10.0.26100 x64
}
```

- 旧变量 `ZcodeClientPlatform` / `ZcodeClientOSVersion` / `ZcodeClientLanguage` / `ZcodeClientTimezone` 降级为**读同一份 Profile 的兼容别名**，避免一次改爆所有调用点。
- 环境变量名保持向后兼容（`ZCODE_CLIENT_PLATFORM` 等），但**解析后写回 Profile**。

**守卫测试**（照 `internal/config/version_test.go` 的既有做法）：静态扫描生产代码，禁止出现
`"win32-x64"` / `"windows"` / `"10.0.26100"` / `"zh-CN"` / `"Asia/Shanghai"` / 版本号 字面量。

### 阶段 2：推理路径头齐平（GAP-1 / 2 / 6 / 7）

改 `internal/upstream/request.go`：

1. `fixed` 头补齐 `X-Title` / `X-Release-Channel` / `X-Client-Language` / `X-Client-Timezone` / `X-Os-Category` / `X-Os-Version` / `X-Platform`，取值全部来自 `ZcodeProfile`。
2. `HTTP-Referer` 去掉尾斜杠。
3. **`X-Device-Mid` 从推理路径移除**（GAP-7）——
   ⚠️ 这是一处**行为变更**，必须先确认上游是否有基于 `X-Device-Mid` 的活动投放/额度判定（已知 `quota`/`claim` 用它是必要的）。
   做法：先把移除开关化（`ZCODE_UPSTREAM_SEND_DEVICE_MID`，默认保持现状），抓包对比后再翻转默认值。
4. `dropHeaders` 收窄：**只剔除**鉴权类与传输类（`authorization`/`x-api-key`/`host`/`content-length`/`connection`/`accept-encoding`/`cookie`），
   把 `x-zcode-*` 从一刀切里拿出来，改为**显式白名单**（只剔除客户端可伪造的签名头 `X-Client-*` / `X-App-Id`）。
5. 新增归因头合成：`x-request-id`（每请求 UUID）、`x-zcode-session-type`（默认 `main`）、`x-zcode-trace-id`。
   `x-session-id` / `x-query-id` 优先沿用下游送来的值，缺失时用 `x-request-id` 派生 —— 保证**永远存在**。

### 阶段 3：body 对齐（GAP-3 / 4 / 5）

重建 `internal/upstream/zcode_system.json`：

- 块 0（CLI Prefix）、块 1（Agent Identity / `# Harness`）按 3.14.3 原文逐字重抄，修正 `# Harness` 第 3 条。
- 块 2（`# Environment`）**不再写死**，改为运行时生成：
  - 内容模板见 §1.8；
  - 取值全部取自 `ZcodeProfile`；
  - 建议**不注入**可选行（`- You are powered by the model named …`），因为 provider/model 与真实客户端不一致，注入了反而是新的矛盾点。
- 生成函数放 `internal/upstream/systemprompt.go`，`ZcodeSystemBlocks()` 改为从 Profile 组装；保留 `//go:embed` 的静态部分（块 0/1）。
- 快照测试：把生成结果与人工核对过的 golden 字符串比对。

⚠️ **风险**：块 0/1 的文案变化可能影响上游对 coding-plan 资格的判定（记忆里 `app_version` 就是这类硬门槛）。
⇒ 文案部分**开关化** + 灰度；块 2（Environment）只是值修正，风险低。

### 阶段 2b：设备身份改走 body（GAP-7 + GAP-13，✅ 已执行 2026-09-24）

这两项是**同一件事**，必须一起做：官方模型请求头里不带 `X-Device-Mid`，设备指纹在 body 的
`metadata.user_id.device_id` 里。只删头会让设备身份彻底消失（反而更像伪造），只加 body 会
留下一个官方不发的头 —— 两头都是可交叉比对的差异。**头去掉、body 加上。**

- **头侧**：`config.UpstreamSendDeviceMid` 默认翻转为 `false`（开关保留作应急回滚）；
  `dropHeaders` 新增 `x-device-mid` —— 我们不再写固定值，若不显式拦掉，下游送来的同名头会
  **原样透传到上游**，等于让客户端指定本账号的指纹，账号隔离形同虚设。
- **body 侧**：新增 `internal/upstream/metadata.go`。`InjectDeviceMetadata` 写官方形态
  `{"device_id":<本账号指纹>,"account_uuid":"","session_id":<会话 id>}`（user_id 是 JSON
  **字符串**而非嵌套对象，键序与 golden 逐字节一致）；`MetadataSessionID` 取下游已在 body 里
  声明的会话，作为头缺失时的兜底来源。
- **会话一致性**：`metadata.user_id.session_id` 与 `X-Session-Id` 头**恒等**（golden 实测），
  因此归因标识必须**按请求算一次**再共享 —— 抽出 `upstream.Attribution` +
  `BuildRequestWithAttribution`，sync / async / 线路探测三条路径传同一份。原先
  `attributionHeaders` 在头与 body 两处各算一次，必然分叉成两个 UUID。
- **观测副作用（已处置）**：`device_id` 每账号一份 ⇒ 最终 payload 逐账号不同，而 `bodyhash`
  的用途正是「这个 body 打过几个账号」。故 `ReqDiag.SetBody` 改为双参：字节数取实际 payload，
  **指纹取内容视图**（注入账号身份之前），跨账号可比；有测试钉住这一不变量。

### 阶段 4：跨端点一致性（GAP-8 / 9）

- `captcha.go` / `quota.go` / `claim.go` 改用 `ZcodeProfile`：
  - 请求头 `X-Platform` → `XPlatform()`（`win32-x64`，值不变）
  - 查询参数 `platform=` → `QueryPlatform()`（`windows-x86_64`，**值会变**）
  - 补齐 §1.3 的缺失头；`X-ZCode-Agent` 从非模型路径**移除**
- `telemetry.go` 的 `device_os_category` 与 `os_version` 同源化。
- ⚠️ `quota` / `claim` 目前**是工作的**（能领到套餐）。改 `platform=` 前必须先在预发用真实账号验证一次，或保留 `ZCODE_QUERY_PLATFORM` 覆盖位；一旦领取失败即刻回退。

### 阶段 5：可观测性

- `[#]` 诊断行追加 `bodyhash=`（body 的 sha256 前 12 位）—— 让「同一 body 打过几个账号」在线上直接可见，不再依赖「14 条记录恰好字节数相同」这种旁证。
- `[#]` 追加 `riskscope=`（本请求命中的不同账号数）。
- 签名开关巡检：定期 `GET /api/v1/agent/configs`，`data.codingPlanSignature.enable === true` 时告警。

### 阶段 6：验证与发版

见 §四。

---

## 四、⭐ 怎么证明「对齐了」—— 用官方客户端自己产出 golden

> ### ✅ 已执行（2026-09-24）
>
> 结果与逐字段比对见 **`docs/analysis-client-golden-diff-20260924.md`**。摘要：
>
> - **块 0 / 块 1 与官方逐字节相等**（42 / 1211 字符）——阶段 3b 的文案改动完全命中；
>   `# Environment` 的标签集与顺序也完全一致；
> - `platform=windows-x86_64` 的查询参数形态**实测确认**（阶段 4 的疑问由事实定案）；
> - 服务端签名开关为 `enable=true`，但客户端因凭据不含 `.` 而**发未签名请求**
>   ⇒ **不实现签名的决定被证实**；
> - 新发现三项：模型 UA 带 ai-sdk 后缀（已修）、`x-query-id` 恒发（已修）、
>   模型请求**不带** `x-device-mid`（已修：见阶段 2b，设备身份改走 body）；
> - 另有 `metadata.user_id` 一项：官方恒发且其 `device_id` 就是设备指纹，我方原先完全不发
>   —— 已随阶段 2b 补上。
>
> 需要人工决策的 5 项列在该文档 §五；其中「设备指纹的放置方式 / metadata」已定案并实施
> （阶段 2b），其余三项（传输层三头、provider 形态、行为指引章节）**超出"格式对齐"
> 范畴，仍不宜默认动手**。

这是本计划里**最有价值**的一步，也是最容易被忽略的。

客户端支持用环境变量覆盖端点（`ZCODE_BASE_URL` / `ZCODE_ENDPOINT_ORIGIN` / `ZCODE_PRODUCTION_BASE_URL`，见 `p1()`）。
因此可以：

1. 本机起一个**记录型反向代理**（只落盘请求，不转发），监听 `127.0.0.1:PORT`。
2. 以 `ZCODE_BASE_URL=http://127.0.0.1:PORT` 启动本机 ZCode 客户端，跑一次真实对话。
3. 代理把**完整的请求行 + 全部头 + 原始 body** 落成 golden 文件。
4. 与网关出站做**逐字段 diff**，产出一张对齐表。

无法访问上游也没关系 —— 我们要的只是**客户端会发出什么**这一事实。

补强：

| 手段 | 覆盖 |
|---|---|
| golden 逐字段 diff | 头名、头值、body 结构、system 分段顺序 |
| `ZcodeProfile` 单测 | 派生公式（`win32` → `win64-x64` / `windows-x86_64` / `win32 10.0.26100 x64`） |
| 静态守卫测试 | 禁止生产代码再散落字面量 |
| 反向验证 | 把 Profile 的平台改回 `linux`，确认「头体一致性」测试变红 |
| 回归 | `quota` / `claim` / `captcha` 全绿，且真实领取成功一次 |

---

## 五、风险与回滚

| 风险 | 处置 |
|---|---|
| 改 `platform=` 查询格式导致领取/额度失效 | 单独提交 + 开关 + 预发验证；失败即刻回退 |
| 移除推理路径的 `X-Device-Mid` 影响投放判定 | 默认值已翻转；`ZCODE_UPSTREAM_SEND_DEVICE_MID=true` 可**即时回滚**，无需改码重发 |
| 设备身份进 body 后 `bodyhash` 跨账号不可比 | 指纹改取「注入账号身份之前」的内容视图（`ReqDiag.SetBody`），已有测试钉住跨账号同指纹 |
| `# Harness` 文案变化影响 coding-plan 判定 | 与 Environment 拆成两次提交；文案可回退 |
| 归因头 `x-session-id` 取值不当反而制造新异常 | 优先透传客户端值，缺失才派生，且派生值形态与官方一致（无 `sess_` 前缀） |
| 一次改太多难以二分 | 严格按 §三 的 6 个阶段分提交，每阶段独立可验证 |

---

## 六、明确不做 / 待确认

**不做：**
- 不实现 `ClientRequestSigningV4`（失败面是 401，与 405 风控无关；且开关默认可能关闭，官方自身也 fail-open 发未签名）。
- 不改 `thinking` / `output_config` 的既有语义（已对齐，且有 400 的坑）。
- 不动 `# Memory` / `Skills` / `meta_user` 类分段（我们的下游客户端自己会带）。

**待确认（2026-09-24 更新：前三项已由 golden 定案，保留原文以示结论来源）：**

1. ~~上游是否真的会因为 `X-Device-Mid` 出现在推理请求上而区别对待~~ → **已按「对齐官方」定案**
   （阶段 2b）：官方不发，我们也不发；设备指纹改由 body 的 `metadata.user_id.device_id`
   承载，账号隔离不依赖请求头。上游是否"在意"这个头仍无直接证据，但形态对齐不再有争议；
   若线上出现与投放/额度相关的回归，`ZCODE_UPSTREAM_SEND_DEVICE_MID=true` 可即时回滚。
2. ~~非模型端点的 `platform=` 到底该用哪种格式~~ → **已定案**：`windows-x86_64`（golden 实测）。
3. ~~`sourceTitle` 该固定为 `electron` 还是 `cli`~~ → **已定案**：由启动方式决定
   （`cli` = 命令行，`electron` = app-server）。作为网关，伪装桌面端取 `electron`。
4. 是否需要用 `ZCODE_ENV=test` 之外的通道值（当前生产恒为 `production`，无分支）→ 仍无分支证据。

**新增待确认（来自 golden）：** ~~设备指纹该放在头还是 body（GAP-7 / GAP-13）~~ →
**已定案并实施**（阶段 2b：头去掉、body 加上）；余下「传输层三头是否模仿」「是否注入行为
指引章节」仍开放 —— 见 `docs/analysis-client-golden-diff-20260924.md` §五。

---

## 七、优先级与顺序

```
阶段 1（单一来源）        ← 必做前置，本身零行为变化
   ├─ 阶段 3（body 对齐） ← 修掉 GAP-3/5，风险最高、收益最高
   ├─ 阶段 2（推理头齐平）← 修掉 GAP-1/2/6，风险中
   └─ 阶段 4（跨端点）    ← 修掉 GAP-8/9，需真实账号验证
阶段 5（可观测性）        ← 与上面并行，独立提交
阶段 6（golden 验证）     ← 贯穿，最终准入
```

**建议的第一次交付**：阶段 1 + 阶段 3 的 Environment 部分（不含 Harness 文案改动）。
理由：这两步直接消掉「头说 Windows、body 说 Linux」这个最硬的矛盾，且不触碰任何正在工作的额度/领取链路。

**当前进度（2026-09-24）**：阶段 1、3a、3b、2、2b、5、6 **已完成**（改动未提交）；
阶段 4（`platform=` 查询参数格式）**未做** —— 额度/领取链路现在是工作的，改前必须用真实账号
验证一次。阶段 2b 的四条路径（sync / async / 线路探测 / 归因头合成）已全部覆盖。

## 第九轮执行记录（2026-09-24 傍晚，调用机制逐项对齐）

依据官方 CLI 日志（重试语义）与 golden 抓包（传输层头），完成第三批对齐：

| # | 维度 | 官方（实测） | 修复前 | 处置 |
|---|---|---|---|---|
| GAP-15 | 鉴权头 | `x-api-key` + `Authorization: Bearer` **双头同值**（两种 plan 模式皆然） | JWT 只发 Authorization；APIKey 只发 X-Api-Key | ✅ 已修：两种模式统一双头同值 |
| GAP-16 | 重试时 request-id | 同一 queryId（轮次）内**每次 attempt 换新** requestId（CLI 日志：4 次重试 4 个 id） | 整轮共享一个 request-id —— 上游可见「同一请求 id 出现在多个账号上」，直接暴露多账号同源 | ✅ 已修：`Attribution.WithFreshRequestID()`，sync 预算循环 / async 换号循环与内层验证码重试都按上游 HTTP 尝试刷新；session/trace/query 保持（轮次连续） |
| GAP-17 | undici 传输层头 | `accept: */*`、`accept-language: *`、`sec-fetch-mode: cors` 恒在（golden 实测） | 三项全缺 | ✅ 已修：加入固定头（客户端透传不得覆盖） |
| — | accept-encoding | `br, gzip, deflate`（undici） | Go 默认 `gzip` | ⚠️ **刻意不改**：对齐需引入 brotli 解压并改流式链路，风险/收益不成比例；编码协商的指纹敏感度远低于身份头 |
| — | 头名字面形态 | undici 全小写（h1.1） | Go Canonical 形态 | ⚠️ **刻意不改**：h2 协商下 Go 自动全小写（与 undici 一致）；h1.1 差异记录为残留 |
| — | maxRetries=10 | 同账号内重试 10 次 | MaxAccountAttempts 换号 + 指数退避 | ⚠️ 产品形态差异（池化），观测项 |
| — | UA runtime 段 | `runtime/node.js/22`（golden 实测） | 同 | ✅ 已一致（勿凭 kit() 源码推断改写 —— golden 是事实） |
| — | 验证码头 | 每请求实时解、恒带 | JWT 每请求从预解池取、恒带；无 token 不裸发 | ✅ 行为等价（上游只看到头） |

**反向验证**：V1b（断言红）/ V2 / V4 / V5b（新增端到端守卫 `TestRequestIDRefreshesAcrossAttempts`）全红；
V3 撤销 —— 固定头最后写回的结构本身保证恒胜，透传覆盖 Accept 不是可达威胁；V6b 验证套件仍绿。
端到端实拍：出站 **25 头**与官方形态逐项对齐；换号对照 Request-Id 换新 / Session、Query 保持 / 双头同值。

**一致性验证方法**（后续如何确认与官方一致）：
1. **静态层**：`go run ./.workbuddy/tmp-logs/dump` 实拍出站头，与 golden 抓包（`golden-capture.jsonl`）逐字段 diff（脚本 `golden-diff.js`）；
2. **行为层**：`TestRequestIDRefreshesAcrossAttempts`（换号语义）+ `TestAuthDualHeadersSameValue` + `TestUndiciTransportHeaders` + `TestSourceHeadersMatchOfficial` 守卫套件；
3. **线上层**：v2.7.1 上线后抓一次网关出站（或看 `[#]` 行的 bodyhash 与头部日志），与官方桌面端 model_io 记录对照；
4. **官方侧基准更新**：官方日志通道（`cli/log/zcode-*.jsonl` 的 `model.network.*`）可持续提供新的 attempt/requestId 样本，任何重试语义变化都能在客户端升级后第一时间发现。

## 第十轮执行记录（2026-09-24 傍晚，P1-3 重放防护 + RiskScope 冷却不回滚）

按账号级标记模型（§九）落地阻止标记扩散的两个机制：

**1. 重放防护（P1-3）—— 新 `internal/gateway/replayguard.go`**

- `ReplayGuard` 按「模型 + 内容指纹（入口 body 的 sha256 前 12 位）」记录最近一次请求级风控判定；
- 窗口 `ZCODE_REPLAY_GUARD_TTL_SECONDS`（默认 60s，0 = 禁用，应急回退开关）内同内容请求
  **直接快速失败**：sync 返回 503 `risk_control_cooldown`、async 投递同名 error 事件，均不消耗任何账号；
- 挂在 Engine / Pool 字段上**跨请求/跨票共享** —— 防护的意义就在跨请求（上一个请求刚打爆两个账号，
  下一个相同 body 的请求必须立即被挡住）；懒清理过期项 + 触顶 1024 整体清空（放行优于 OOM）。

**2. RiskScope 冷却不回滚**

- 删除 `Rollback` / `Record` / `RiskControlSnapshot` / `RollbackRiskControl`（riskscope.go 163 行瘦身）；
  回滚冷却等于替服务端解封已被标记的账号；
- `MarkRiskControlWithSnapshot` 合并回 `MarkRiskControl`（classify.go 的薄包装删除，主实现回 riskscope.go）；
- 关键语义变化：请求级判定成立时**判定账号本身也吃 `MarkRiskControl`**（它同样吃了 405、同样被上游
  标记），随后停止换号。日志文案：`风控判定為請求級（已在 N 個帳號上復現…），停止換號；冷卻保留
  （上游已標記帳號，300 s），同內容 60 s 內直接拒絕`。

**守卫与验证**

- 新增 `replayguard_test.go`（TTL 窗口 / 键独立 / 容量触顶 / 禁用态 / ContentKey 稳定性）；
- 端到端 `Test405RequestLevelRiskKeepsCoolingAndGuardsReplay`（sync）：恰好 2 个账号保留冷却、
  1 个未动，同内容再请求返回 503 且上游调用数不增；async 侧 `Test405RequestLevelRiskInAsyncPool`
  同语义改造（冷却保留 + 重放拦截）；
- 反向验证 RB1-RB4 全红（断言红，非编译红）：不登记防护 / Blocked 恒 false / 退回不冷却 /
  async 不登记，各对应守卫逐一点名变红；
- 全量门禁：BUILD OK、17 包通过（仅 captcha 已知沙箱符号链接失败）、gofmt 107 文件 0 不合规。

**上线观察项**：`[#]` 行 `riskscope=2` 之后应紧跟 `risk_control_cooldown` 快速失败（而非继续换号）；
风控冷却账号数在事故后的增长应被截止在「参与判定的账号」范围内，不再出现整池扩散。
