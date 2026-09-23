# ZCode 客户端「激活上报」流程 vs 网关实现 —— 差异分析

> 分析对象：本机安装的 ZCode 桌面客户端（Electron，`ZCode.exe` FileVersion **3.14.3.7762**）
> 对比对象：本项目 `internal/claim/telemetry.go`、`internal/claim/claim.go`、`internal/upstream/request.go`、`internal/config/config.go`
> 方法：解包客户端 `resources/app.asar`（27068 个文件），定位遥测与计费模块源码后逐字段比对。

---

## 一、结论摘要

1. **激活事件体本身完全一致** —— 16 个字段、端点（`/api/v1/event/report`）、两个事件名（`app_launch` / `app_daily_active`）逐项对得上，这部分没有问题。
2. **真正的差异在「请求头」与「版本号」上**：客户端 events 请求带 **11 个伪装头**，我们只带 `Content-Type`；客户端 `app_version` 是 **3.14.3**，我们默认写死 **3.11.2**（落后 3 个中版本）。
3. 因此「激活上报成功但 preview 恒空/领取失败」最可疑的两个原因，按可能性排序是：**① 版本号过期导致上游不再投放；② 上报请求头形态不像官方客户端（上游可交叉比对头与事件体）**。二者都不需要改事件体。

---

## 二、客户端真实的上报激活流程（代码级）

### 2.1 模块与位置

| 能力 | asar 内位置 | 关键符号 |
|---|---|---|
| 遥测核心 | `/out/main/chunk-VN4HYEPZ.js` | `createTelemetryCore` → `reportEvent` / `reportAppLaunch` / `reportAppDailyActive` / `flushPendingReports` |
| 伪装请求头 | `/out/host/chunk-B7L5SK4K.js` | `buildZCodeSourceHeadersFromContext` |
| 计费/领取 | `/out/host/index.js` | `getManualClaimPlanPreviews` / `claimManualPlan` / `billing/balance` |
| 端点常量 | `/out/host/chunk-*.js` | `buildZCodeEndpointUrls` |

端点常量解析结果（与我们一致）：
`https://zcode.z.ai/api/v1/zcode-plan` + `/billing/preview` · `/billing/claim`；事件独立在 `https://zcode.z.ai/api/v1/event/report`。

### 2.2 事件发送（sendReport）

```
POST https://zcode.z.ai/api/v1/event/report
headers: { Content-Type: application/json, ...11 个伪装头, ...(有 JWT 时 Authorization: Bearer <jwt>) }
body: { event_id, client_timezone, client_language, element_name, event_region, event_type,
        event_text, event_extra_detail, user_id, screen_resolution, app_version,
        device_os_category, device_os_version, device_mid, mac_id, marketing_params }
```

- 超时 **5000ms**；重试上限 **2 次**，仅对 `timeout / network / HTTP 408 / 429 / 5xx` 重试，退避 **300ms**（429 用 **1000ms**）。
- `redirect: "error"`；`marketing_params` 来自 `/api/v1/marketing/touch` 的归因快照（取不到时发 `{}`）。
- `device_os_category` 由 `platform` 映射（`win32→windows`、`darwin→macos`、其它→`linux`）。

### 2.3 两个激活事件

**app_launch**（启动即发，无去重）：
`eventId = randomUUID()`，`userId = JWT.user_id`，`deviceMid = 持久化设备 UUID`。

**app_daily_active**（每设备每天一次）：
- 日期键 = `Intl.DateTimeFormat("en-CA", { timeZone: clientTimezone })` → `YYYY-MM-DD`；
- 去重依赖本地状态文件 `~/.zcode/v2/telemetry-state.json` 的 `lastDailyActiveDate`；
- 带 **in-flight 锁**（写入 `dailyActiveInFlight={date,startedAt}`，300s 内视为进行中，失败回滚）；
- `deviceMid` 也持久化在这个文件里（**一台设备一个固定 UUID，事件与 billing 共用**）。

本机实测该文件内容形态：`{ deviceMid: <uuid>, lastDailyActiveDate: "2026-09-23" }` —— 即客户端今天已正常上报过。

### 2.4 还有一个我们没有的事件：session_create

通用 `reportEvent` 支持任意事件名；其中 `session_create` 的 `event_id` 是**确定性**的：
`sha256("zcode:session_create:v1", userId, talkId)` 取前 16 字节后置版本/变体位 → UUID 形态。此外请求体还会按需附加 `talk_id` / `message_id`。

即客户端存在「会话创建」这一活跃信号，我们的账号池**完全没有上报**。

### 2.5 客户端 11 个伪装头（`buildZCodeSourceHeadersFromContext`）

| 头 | 取值 |
|---|---|
| `User-Agent` | `ZCode/<版本>`（3.14.3） |
| `HTTP-Referer` | 端点 origin（`https://zcode.z.ai`） |
| `X-Title` | `Z Code@electron` |
| `X-ZCode-App-Version` | 版本号（**有值才带**） |
| `X-Platform` | `${platform}-${arch}`，如 `win32-x64` |
| `X-Release-Channel` | `stable` / `preview`（有值才带） |
| `X-Client-Language` | 如 `zh-CN`（缺省 `unknown`） |
| `X-Client-Timezone` | 如 `Asia/Shanghai`（缺省 `unknown`） |
| `X-Os-Category` | `windows` / `macos` / `linux` |
| `X-Os-Version` | 内核串（有值才带） |
| `X-Device-Mid` | 持久化设备 UUID（有值才带） |

### 2.6 计费接口的两种头形态（注意：**不带** X-Device-Mid）

**preview**：`GET /api/v1/zcode-plan/billing/preview?app_version=<ver>&platform=<plat>`
头只有 `Authorization: Bearer <zcodejwttoken>`；超时 15s。
成功判据：`code === undefined || code === 0`，且 `data` 必须存在；取 `data.plans[]`（`plan_id` 非空即保留，entitlements 全量收下）。

**claim**：`POST /api/v1/zcode-plan/billing/claim`，body `{"plan_id": "..."}`
头：`Authorization: Bearer <jwt>`、`Content-Type`、`X-Aliyun-Captcha-Verify-Param`（+可选 `-Region`）、`X-ZCode-App-Version`、`X-Platform`。
成功判据：`code === 0 && data.plan` 存在；失败时读 `data.plan.ends_at` 作为「下次可领时间」。

---

## 三、逐项对比

| 维度 | 客户端（3.14.3） | 我们 | 判定 |
|---|---|---|---|
| 事件端点 | `/api/v1/event/report` | 同 | ✅ 一致 |
| 事件体 16 字段 | 见 2.2 | 同（逐字段一致） | ✅ 一致 |
| 事件名 | `app_launch` + `app_daily_active`（+ `session_create` 等） | 前两个 | ⚠️ 缺会话类事件 |
| 事件请求头 | 11 个头 + Content-Type + 可选 Authorization | **仅 Content-Type** | ❌ **重大差异** |
| 事件 `app_version` | 3.14.3 | **3.11.2**（默认） | ❌ **重大差异** |
| `User-Agent` | `ZCode/3.14.3` | `ZCode/3.11.2` | ❌ |
| 事件重试 / 超时 | 2 次（5xx/408/429/网络）· 5s | 无重试 · 25s | ⚠️ 差异 |
| 事件 Authorization | 有 JWT 就带 | 刻意不带 | ⚠️ 差异 |
| device_mid 来源 | 单机持久 UUID，全接口共用 | 全局 + 每账号指纹 | ⚠️ 设计差异 |
| daily_active 去重键 | 按 `clientTimezone` 的本地日期 | 自有判定 | ⚠️ 差异 |
| billing base | `/api/v1/zcode-plan` | 同 | ✅ 一致 |
| preview URL 参数 | `app_version` + `platform` | 同 | ✅ 一致 |
| preview 头 | 仅 `Authorization` | 额外带 UA / X-Device-Mid / HTTP-Referer 等 | ⚠️ 多带 |
| claim 头 | Authorization + Content-Type + captcha ×2 + 版本 + 平台 | 同 + UA + X-Device-Mid + HTTP-Referer | ⚠️ 多带 |
| claim 成功判据 | `code==0 && data.plan` | 同 | ✅ 一致 |
| 失败取 `ends_at` | 是 | 是 | ✅ 一致 |
| 业务码宽松度 | 成功认 `undefined/0/200`；claim 另收数字字符串 | 只认 `0` | ⚠️ 偏严 |
| entitlement 过滤 | 全量保留（含 meter/unit_type） | 仅 `model_usage` + `token` | ⚠️ 过窄 |
| 上游伪装头全集（upstream） | `X-Title` / `X-Release-Channel` / `X-Client-Language` / `X-Client-Timezone` / `X-Os-Category` / `X-Os-Version` | 这 6 个头**全部缺失** | ⚠️ 建议补齐 |

---

## 四、可执行修复清单（按性价比排序）

1. **升版本号到 3.14.3**（一行常量，影响 UA、`X-ZCode-App-Version`、preview query、事件体 `app_version` 四处）：
   `config.ZcodeClientVersion` 默认值 `3.11.2` → `3.14.3`；同步 `ZCODE_CLIENT_OS_VERSION` 等伪装字段核对一次。
   若上游按版本灰度投放，这一步可能直接解决问题。
2. **给 `PostActivationEvent` 补齐客户端同款请求头**（当前只有 `Content-Type`）：
   至少补 `User-Agent`（`ZCode/<ver>`）、`HTTP-Referer`、`X-Title: Z Code@electron`、`X-ZCode-App-Version`、`X-Platform`、`X-Client-Language`、`X-Client-Timezone`、`X-Os-Category`、`X-Os-Version`、`X-Device-Mid`、`X-Release-Channel`。
   事件体里已有 language/timezone/os 字段，头上缺失会造成「头与体不可互证」，是明显的非官方特征。
3. **事件上报带上账号 JWT**（`Authorization: Bearer <jwt>`）——客户端就是这么做的，我们注释里的「端点不校验登录态」只说明不强制，不代表带上无益。
4. **补 `session_create` 事件**（若上游以会话数衡量活跃）：`event_id` 用 `sha256("zcode:session_create:v1", userId, talkId)`，无 `talk_id` 时退回随机 UUID。
5. **放宽业务码判定**：成功认 `0` 与数字字符串形态（客户端还认 `undefined`/`200`，可只在 preview 上放宽以免误判）。
6. **entitlement 过滤改为全量保留**（`meter`/`unit_type` 只作为标签存下），避免上游换字段名后 grants 静默变空。
7. **preview/claim 的额外头做减法**：客户端这两个接口不带 `X-Device-Mid`/`UA`/`Referer`，若上游对 `/billing/*` 做头白名单，多带反而是风险点；建议与 events 头策略分开配置。
8. **事件重试对齐**（可选）：对 5xx/408/429 做 1～2 次短退避重试，减少瞬时失败导致「当天日活没记上」。

---

## 五、验证建议

1. **同账号对照**：用一个真实登录 z.ai 的账号，分别由（a）官方客户端、（b）网关修改前后 发起 `event/report` 与 `billing/preview`，对比上游响应（尤其 preview 的 `data.plans` 是否非空）。
2. **在网关加一次性 DEBUG 出口**：把 event/report 与 preview 的实际请求头/响应体打印到 `[#]` 诊断行（**必须脱敏**：去掉 JWT、device_mid、邮箱）。
3. **排除运营侧原因**：日志中 `上游无投放套餐` 可能是该批次账号确实未被投放。判定办法——同一账号在官方客户端登录后看「领取」入口是否出现；若不出现，则问题不在请求形态。
   （本机客户端 9-22 的 `billing/balance` 曾正常返回 `ZCode Start Plan` 与两条 entitlement，说明上游当时在正常投放。）

---

## 附录：证据索引

- 客户端版本：`ZCode.exe` FileVersion `3.14.3.7762`
- 事件模块：`out/main/chunk-*.js` → `createTelemetryCore`、`sendReport`（构造体与头）、`reportAppDailyActive`
- 头生成：`out/host/chunk-*.js` → `buildZCodeSourceHeadersFromContext`
- 计费：`out/host/index.js` → `getManualClaimPlanPreviews`（约偏移 2713470xx）、`claimManualPlan`（2713482xx）
- 端点常量：`Po = "https://zcode.z.ai"`、`buildZCodeEndpointUrls`（`/api/v1/zcode-plan/...`）
- 本机客户端状态：`~/.zcode/v2/telemetry-state.json`（`deviceMid` / `lastDailyActiveDate`）
