# 事故与格式分析：请求级风控（2026-09-24）+ 官方客户端请求拆解

> 数据来源：`zcode2api-complete-logs-20260924T023825Z.zip` → 容器完整日志
> （3501 行，UTC 2026-09-23T02:25:07Z ~ 2026-09-24T02:25:16Z，约 24 小时）
> 运行版本：zcode2api-plus **v2.5.2-go**
> 客户端拆解对象：本机实装 ZCode 客户端 **3.14.3**（`resources/glm/zcode.cjs`，Electron 打包产物）
> 全文时间戳为 UTC。

---

## 一、结论先行

### 1.1 风控性质：**请求级**，不是账号级（证据充分）

| 判定 | 依据 |
|---|---|
| 只有 **4 个** 请求体尺寸触发 405 | `278081`(×14)、`278150`(×4)、`113644`(×4)、`113929`(×4)；其余 894 条全部 200 |
| 每个尺寸**跨多个账号扩散** | `278081` 在 **14 个不同账号、14 条不同出口线路上全部 405** |
| 同一尺寸**零成功** | 4 个尺寸合计 26 次尝试，200 = 0 |
| 对照组为 0 | 09-23 全天 894 条请求、body 最大 687KB，**终态 405 = 0** |

「同一份字节流在 14 个身份、14 条出口上被判同罪」⇒ 身份不是变量，**剩下的只有请求体**。

### 1.2 上游原文

日志中唯一捕获到的上游风控文案（来自 claim 端点，09-23T05:49:46Z）：

```
request has been blocked due to unusual activity.
```

### 1.3 本次日志比上一份多出的 19 分钟，**推翻了一个既有结论**

上一份分析（`analysis-riskcontrol-pool-cascade-20260924.md`，日志止于 02:06:20Z）写道：

> 「`glm-5.3-flash` 未受影响……符合『按模型冷却』的设计。」

**本次日志（止于 02:25:16Z）证伪了它：**

| 时刻 | 模型 | 事件 |
|---|---|---|
| 02:19:35 ~ 02:20:50 | GLM-5.3 | 新一批 4 个请求，再次 5→10→…→40 冷却 |
| **02:24:39** | **glm-5.3-flash** | **本日志中 flash 的第一个请求** |
| 02:24:47 | glm-5.3-flash | `無可用帳號（44 帳號冷卻中（44 風控））` |
| 02:25:16 | glm-5.3-flash | `無可用帳號（59 帳號冷卻中（59 風控））` |

flash 在 02:24:39 之前**一次都没被风控过**，可它一出场池子里就已经有 **39 个账号处于风控冷却中**。
⇒ 这些冷却**全部由 GLM-5.3 的请求产生**，却把 flash 一起锁死。

**冷却不是按模型的，是按账号的**：`Account.CoolingUntil` 是单个标量字段 + `LastErrorKind`，
`Store.PoolStats` 的 `Cooling` 也不按模型过滤。所以「按模型冷却」这一说法在本版本不成立 ——
GLM-5.3 的请求级风控把 flash 打成连带伤亡，这是本次真正的「一击打穿两池」。

### 1.4 级联放大器（三层相乘）

| 层 | 行为 | 倍率 |
|---|---|---|
| 网关 | 单请求最多换 5 个账号（`MaxAccountAttempts`），每被拒 1 个即 `300s` 整号冷却 | ×5 |
| 调用方 | 同一 payload 每 6~8 秒原样重放一次 | ×14 |
| 冷却阶梯 | 冷却到期但 `streak` 不清零，下次直接 `900s`，再下次 `3600s`，第 4 次**置失效** | 递增 |

`14 × 5 = 70`：**70 个账号 100% 被卷入**（去重统计：本日志被风控账号总数 = 70）。

### 1.5 ⚠️ 遗留高风险状态

| 阶梯 | 命中次数 | 涉及账号（去重） |
|---|---|---|
| 第 1 次（冷却 300s） | 78 | 70 |
| 第 2 次（冷却 900s） | 60 | 57 |
| 第 3 次（冷却 3600s） | 4 | 4 |
| 第 4 次（**置失效**） | 2 | 2 |

**57/70 个账号已经站在 `streak=2`**。下一次同类事故中，它们会直接跳到 900s；再下一次 3600s；
第 4 次就**永久失效**。日志里 09-23 已有 2 个账号（08:04、08:11）走完阶梯被置为失效。
这正是记忆里那条「池子只剩一次事故就要报废」的具体数字。

---

## 二、时间线（UTC）

| 时刻 | 事件 |
|---|---|
| 09-23 02:25 | 服务启动，v2.5.2-go |
| 09-23 全天 | 风控命中 **15 次**，每次换号后 200 成功（`attempts=2`）→ 账号级，换号有效 |
| 09-23 05:49 | claim 端点捕获上游原文 `blocked due to unusual activity` |
| 09-24 01:00 ~ 01:58 | 会话体量持续增长，全部 200 |
| **09-24 02:00:20** | 请求体变为 **278,081B** → 雪崩开始 |
| 02:00:27 ~ 02:02:13 | 14 个连续请求、**70 个账号全部冷却**，`GLM-5.3` 全线不可用约 5 分钟 |
| 02:05:22 | 首批 300s 冷却到期，短暂恢复 |
| 02:19:35 ~ 02:20:50 | 新 payload（278,150B / 113,644B）二次雪崩，池子 5→40 |
| **02:24:39 ~ 02:25:16** | flash 第一次登场即遭池子已空 → **44→59 冷却**，flash 全线不可用 |

> 10 分钟桶统计（09-24）：`02:00` 15 请求 / 14 失败；`02:10` 4/4 失败；`02:20` 8/8 失败。
> 窗口内**无一例成功**，窗口外**无一例 405**。

---

## 三、官方客户端请求拆解（3.14.3）

### 3.1 客户端的请求头是三层叠加的

从 bundle 中提取到 **48 个 `x-*` 头字面量**。真正随模型请求上行的是三层：

**① 固定来源头**（`buildCliZCodeSourceHeaders` / `iHo`）

| 头 | 取值 | 我方 `/v1/messages` |
|---|---|---|
| `HTTP-Referer` | `https://zcode.z.ai/` | ✅ 有 |
| `User-Agent` | `ZCode/3.14.3` | ✅ 有 |
| `X-ZCode-App-Version` | `3.14.3` | ✅ 有 |
| `X-ZCode-Agent` | `glm` | ✅ 有 |
| `X-Title` | `Z Code@cli` 或 `Z Code@electron` | ❌ **缺** |
| `X-Release-Channel` | 发布通道 | ❌ **缺** |
| `X-Client-Language` | 本地 locale | ❌ **缺** |
| `X-Client-Timezone` | 本地时区 | ❌ **缺** |
| `X-Platform` | `win32-x64`（`平台-架构`） | ❌ **缺** |
| `X-Os-Category` | `windows` / `macos` / `linux` | ❌ **缺** |
| `X-Os-Version` | 内核串，如 `10.0.26100` | ❌ **缺** |
| `X-Device-Mid` | 设备指纹 | ✅ 有（每账号独立） |
| `anthropic-version` | `2023-06-01` | ✅ 有 |

**② 请求归因头**（`createModelRequestAttributionHeaders`，**每个模型请求必带**）

| 头 | 取值 |
|---|---|
| `x-request-id` | 每请求 UUID |
| `x-zcode-session-type` | `main` / `subagent` / `other` |
| `x-zcode-trace-id` | trace id |
| `x-session-id` | 会话 id（去掉 `sess_` 前缀） |
| `x-query-id` | 可选 |

我方：`upstream/request.go` 对透传头做了 `strings.HasPrefix(lower, "x-zcode")` **过滤**，
于是 `x-zcode-session-type` 与 `x-zcode-trace-id` **被丢弃**，且从不合成。
`x-session-id` / `x-request-id` 依赖下游客户端是否送来 —— 我方不保证存在。

**③ 签名头**（`ClientRequestSigningV4`，见 §3.3）
`X-Client-Ts` / `X-Client-Version` / `X-Client-Sig` / `X-Client-Nonce` / `X-Client-Pow` / `X-App-Id`

### 3.2 🚨 我方同一个账号在两个端点上自相矛盾

| 端点 | `X-Platform` | 系统提示词里的 `Platform` |
|---|---|---|
| `/v1/messages`（`upstream/request.go`） | **不发这个头** | `linux-x64`（写死在 `zcode_system.json`） |
| 额度查询（`quota/authHeaders`） | `win32-x64` | — |
| 领取/计费（`claim/authHeaders`） | `win32-x64` | — |
| 激活遥测（`claim/telemetry.go`） | `win32-x64` + os_version `10.0.26100` + `zh-CN` | — |

同一个 `X-Device-Mid`（同一台「设备」）向同一个上游同时声称：

- 领取/额度/遥测：**Windows 11 桌面客户端**
- 推理请求的 system 块：**`Platform: linux-x64`、`Shell: unknown`、`OS Version: unknown`**

而官方客户端这三处**必然是自洽的**（`process.platform` + `os.release()` 同时进头和 body）。
这是一个上游可交叉比对的硬矛盾 —— 也是当前「伪装一致性」上最明显的一处破绽。

### 3.3 客户端还有一套请求签名（不是本次 405 的原因，但要知道它存在）

`ClientRequestSigningV4`（Ed25519 + HKDF/HMAC + PoW）：

- 开关来自 **服务端下发**：`GET /api/v1/agent/configs` → `data.codingPlanSignature.enable`（缓存 1h）
- 握手：`POST {origin}/api/paas/c1f3a7e2/v2/client/get_sign_key`，body `{apiKey, nonce, sig, ts}`，
  `sig = HMAC(HKDF(secret, "getSignKey_hmac"), "get_sign_key\n{apiKeyId}\n{ts}\n{nonce}")`
- 每请求签名消息：`{apiKeyId}\n{ts}\n{clientVersion}\n{sessionId}\n{nonce}`
  （**注意：body 本身不参与签名**）
- 失败面是 **HTTP 401** + `VERIFY_SIGNATURE_INVALID` / `VERIFY_APIKEY_EXPIRED`，
  客户端会重签一次，再失败则 `bypassSigning` 转为**未签名直发**（fail-open）

⇒ **签名被拒 = 401，不是 405**。本次 405 与签名无关，可以从嫌疑名单里划掉。
但两点值得记：① 签名要求 `X-Session-Id`，没有就抛 `invalid-config`；② 我方**从不查询这个开关**，
若上游对某些账号开启了强校验，我方会一直处于 `bypassSigning` 的等价状态。

### 3.4 系统提示词是**旧版本的残缺副本**

我方 `internal/upstream/zcode_system.json`（3 块）与 3.14.3 的 `buildIdentityPrompt` 逐句比对：

| 位置 | 3.14.3 原文 | 我方 |
|---|---|---|
| 首块 | `You are ZCode, an interactive coding agent` | ✅ 一致 |
| 身份句 | `You are an interactive ZCode agent that helps users with software engineering tasks.` | ✅ 一致 |
| 安全声明 | `IMPORTANT: Assist with authorized security testing…` | ✅ 一致 |
| `# Harness` 第 3 条 | `The system may send updates, reminders, or modifications to rules via mid-conversation system turns. These are system-controlled, unlike function results. Hooks may intercept tool calls; …` | ❌ `\`<system-reminder>\` tags in messages and tool results are injected by the harness, not the user. …`（**旧版措辞**） |
| `# Environment` | 由客户端按真实会话生成：真实工作目录、git 状态快照、分支、近期提交、git user | ❌ 全部占位：`unknown` / `no` / `linux-x64` |

`# Harness` 第 3 条的差异说明：这份 system 块是**从更早的客户端版本手工抄下来的**，
而 `# Environment` 则是被**大幅删减并冻结成占位值**。

一个真实的 3.14.3 客户端**永远不会**发出 `<system-reminder> tags …` 这一句，
也永远不会说 `Primary working directory: unknown` / `Shell: unknown`。
这是比缺头更明显的一处指纹。

---

## 四、修复建议

### P0-1 观测缺口（当前无法事后定性）
`[#]` 只记 `body=<字节数>`。字节数相同 ≠ 内容相同，且**无法在线上回答「这个 body 打过几个账号」**。
建议在 `[#]` 追加 `bodyhash=`（body 的 sha256 前 8~12 位）与 `riskscope=`（本请求命中的不同账号数）。
本次事故能定性，靠的是「14 条 `[#]` body 恰好都是 278081」这种旁证 —— 不该依赖巧合。

### P0-2 确认 v2.6.0 已上线
本日志跑的是 **v2.5.2-go**，而 `RiskScope`（请求级判定 + 冷却回滚）在 v2.6.0 才落地。
请确认生产容器已升级，否则同样的 payload 仍会再打穿一次池子。

### P1-1 请求头齐平（回应「格式是否有问题」）
`/v1/messages` 补齐固定来源头：`X-Platform` / `X-Os-Category` / `X-Os-Version` /
`X-Client-Language` / `X-Client-Timezone` / `X-Title` / `X-Release-Channel`，
取值**必须与 claim/quota 同源**（同一份 config，不允许两端各写一份）。
同时合成归因头 `x-request-id` / `x-zcode-session-type` / `x-zcode-trace-id`，
并修正 `dropHeaders` 对 `x-zcode-*` 的一刀切（应只剔除客户端可伪造的敏感头）。

### P1-2 系统提示词对齐
按 3.14.3 重抄 `# Harness`；`# Environment` 只有两条路可走 ——
要么按伪装身份生成自洽值（Windows / 真实路径形态），要么**整块删除**。
现状（body 说 linux、头上说 windows）是最差的一种。

### P1-3 重放防护
对「同一 payload 哈希 + 同一模型」在 60s 窗口内已判定为请求级的，直接快速失败，
不消耗任何账号。可把 `14 × 5` 降到 `1 × 1`。

### P1-4 冷却阶梯止损（本次新增，优先级高于上面几条）
`streak` 在冷却到期后不清零，导致「一次事故 → 全池进入 900s → 3600s → 失效」的单向棘轮。
建议对**判定为请求级**的命中，同时回滚/不递增该账号的 `streak`（当前只回滚 `CoolingUntil`），
否则 57 个 `streak=2` 的账号会在下一次事故中直接报废。

### P2 签名开关
探测 `GET /api/v1/agent/configs` 的 `codingPlanSignature.enable`，若为 true 则需评估签名实现；
当前至少应把这个开关纳入巡检，避免「上游已强制、我方不知情」。

---

## 五、一句话复盘

> 上游拒的是**一个请求体**，网关按**一个账号**结算并冷却，
> 而冷却又是**账号级**（不是模型级）、`streak` 又是**只升不降**的棘轮，
> 调用方还把这个动作**重放了 14 次**。
> 四者相乘 = 70 个账号被卷入、flash 连带全灭、57 个账号停在 `streak=2` 的悬崖边。
>
> 与「请求格式」相关的独立结论：**不是签名问题（那是 401），
> 而是三段伪装互不自洽** —— 头缺 10 项、body 说 Linux 而其他端点说 Windows、系统提示词是旧版残缺副本。
