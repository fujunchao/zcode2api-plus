# 官方客户端的账号鉴权链路 vs 本网关

对象：本机实装的官方客户端 3.14.3（`resources/glm/zcode.cjs`，14.8MB 单行 bundle）+
该客户端**已登录账号**留下的凭据文件；对照本仓库 `internal/upstream`、`internal/oauth`、
`internal/claim`、`internal/quota`、`internal/captcha` 的现有实现。

方法：读客户端自带的内建 provider 定义（权威规格）+ 静态还原 bundle 里的鉴权代码路径 +
只读探查本机凭据文件的结构（**本文不含任何凭据内容、账号标识与本机路径**）。

---

## 〇、结论先行

> ⚠️ **本节已按 2026-09-24 下午的 start plan 专项复核修订（见 §六）。**
> 初版的 P0-1 / P0-2 建立在「本机账号是 individual coding plan」这个**错误前提**上
> （依据是 `setting.json` 的滞后字段）。实测该账号是 **ZCode Start Plan**，
> 我们的 JWT + `zcode-plan` 端点**与 start plan 的官方路径一致** ⇒ 两条 P0 撤销。

修订后的结论：

1. ✅ **端点选对了**：start plan 的官方端点正是 `https://zcode.z.ai/api/v1/zcode-plan/anthropic`，
   与我们的 `UpstreamZai` 逐字相同（§六）。
2. ✅ **凭据类型对了**：start plan 走 **`oauth-session` 凭据**（OAuth 登录态：`access_token` /
   `zcodejwttoken`），**不是** `account-provider:…:api-key` 那把（§六）—— 即我们用的 JWT 正是这一类。
3. ⚠️ **凭证的「形态」仍未确证**：桌面端把 provider 交给 agent 时如何把 oauth-session 凭据挂上去
   （`x-api-key`／`Authorization`／SDK 的 `authToken`），没有拿到一手抓包。见 §六.4。
4. ⚠️ **「我们池子里的账号是否就是本机登录的这个账号」——本机无法判定**（§六.3）。
5. ⚠️ 仍存疑：官方 anthropic provider 会**同时**发 `x-api-key` 与 `Authorization: Bearer <同一凭据>`
   （`dRs` 的行为，§1.5），我们二选一；以及验证码头是否为「选错端点」的副产品。
   —— 这两条都要等一手抓包才能定，start plan 下验证码**仍可能**是必需的。

---

## 一、官方链路（内建 provider + 静态还原）

### 1.1 端点不是「一个」，而是按 plan 类型分派

客户端自带的内建 provider 定义（`<安装目录>/resources/config/provider/zcode-builtin.json`）
是权威规格，共 8 条，其中与 z.ai 相关的：

| providerId | access.mode | api.type | **baseUrl** |
|---|---|---|---|
| `account:zai-individual-coding-plan` | `individual-coding-plan` | `anthropic-messages` | **`https://api.z.ai/api/anthropic`** |
| `account:zai-team-coding-plan` | `team-coding-plan` | `anthropic-messages` | **`https://api.z.ai/api/anthropic`** |
| `account:zai-start-plan` | `start-plan` | `anthropic-messages` | `https://zcode.z.ai/api/v1/zcode-plan/anthropic` |
| `account:zai-offpeak-idle-plan`（hidden） | `off-peak` | `anthropic-messages` | `https://zcode.z.ai/api/v1/off-peak/anthropic` |

`access` 统一是 `{type:"zhipu-account", accountType:"zai", mode:<上面那列>}`。
baseUrl 会经一个规范化函数补成以 `/v1` 结尾再交给 SDK ⇒ 实际请求路径
`{baseUrl}/v1/messages`。

> **coding plan 与 start plan / off-peak 走的是两个不同的域名与路径前缀。**
> 前者在 `api.z.ai`（智谱开放平台侧），后者在 `zcode.z.ai` 的 plan 专用前缀下。

### 1.2 登录：poll（设备码式）流程，不是重定向兑换

```
POST https://zcode.z.ai/api/v1/oauth/cli/init     Authorization: Bearer <pollToken>
     body: {"provider":"zai"}
  → data: {authorize_url, expires_at, flow_id, poll_interval_sec, poll_token}
     （pollToken = 32 随机字节的 hex，客户端本地生成）

GET  https://zcode.z.ai/api/v1/oauth/cli/poll/{flow_id}   Authorization: Bearer <pollToken>
  → status: pending | failed | ready
     ready ⇒ {token, user:{user_id,email,name,avatar}, zai:{access_token}}
```

落到凭据文件的三项（键名即官方常量）：

| 键 | 来源 |
|---|---|
| `zcodejwttoken` | 响应里的 `token` |
| `oauth:zai:access_token` | 响应里的 `zai.access_token` |
| `oauth:zai:user_info` | 响应里的 `user` |

同时写入 `oauth:active_provider`（**加密**，值为 `zai`）。

### 1.3 从 OAuth 换出模型凭据：三步铸造 `apiKey.secretKey`

用上一步的 `access_token`，按 `family` 分两路（zai 走 `https://api.z.ai`）：

```
1) POST https://api.z.ai/api/auth/z/login          {"token": <access_token>}      → 业务 token
2) GET  https://api.z.ai/api/biz/customer/getCustomerInfo                          → 机构 / 项目
3) GET  https://api.z.ai/api/biz/v1/organization/{org}/projects/{proj}/api_keys
      · 找 name == "zcode-api-key" 的那把；没有就 POST 同名创建
   GET  .../api_keys/copy/{apiKey}                                                 → secretKey
   ⇒ 最终凭据 = `${apiKey}.${secretKey}`   ★ 含一个点号
```

（第 3 步的 `copy` 接口在 bigmodel 家族上要求必须拿到 secretKey；zai 家族同样按此拼接。）

### 1.4 凭据存哪、以及「有没有资格用」的判据

| 键 | 含义 |
|---|---|
| `account-provider:<providerId>:identity` | 该 provider 的**账号身份** |
| `account-provider:coding-plan:<providerId>:account:<identity>:api-key` | **模型请求用的凭据**（即 `apiKey.secretKey`） |

启动时按这两把键解析：**只有 api-key 那一把存在**，provider 才被标成
`entitled: true`；否则 `entitled: false` ⇒ 该 provider 不可用（这正是「账号没资格 /
未登录」在客户端侧的表现）。

### 1.5 模型请求的最终形态

provider 工厂给出（`anthropic-messages` 分支）：

```
baseURL = normalize(baseUrl)            // 补成以 /v1 结尾
headers = { "anthropic-version": "2023-06-01", "x-api-key": <凭据>, ...extra }
                                            ↑ ai-sdk anthropic 用 apiKey 时写这个头
        + { "Authorization": "Bearer <同一个凭据>" }   ← 客户端额外注入（见下）
```

注入 `Authorization` 的函数语义是：**凭据非空且调用方头里还没有 `Authorization` 时，
补一个 `Authorization: Bearer <凭据>`**。所以官方 anthropic 请求里
**`x-api-key` 与 `Authorization` 同时存在、且取值相同** —— 这也解释了此前 golden
抓包中两个头同时出现（当时那是自定义 provider，但同一段代码路径）。

### 1.6 验证码与签名不属于这条路径

- `x-aliyun-captcha-verify-param` 在 bundle 里只被两处引用：① 日志脱敏名单；
  ② **openai-compatible 且该 provider 配了 captcha** 时才去解读响应头。
  **内建 anthropic 路径不使用它。**
- `ClientRequestSigningV4` 的开关是 provider 级别：`start-plan` / `off-peak`
  显式被排除；coding-plan 类走签名 fetch，但本机凭据形态不满足其前置条件时
  **按设计 fail-open 发未签名请求**（此前已在 golden 报告中定案）。

---

## 二、逐项对照

| 维度 | 官方（已登录账号，individual coding plan） | 本网关 | 差异性质 |
|---|---|---|---|
| 模型请求凭据 | `apiKey.secretKey`（coding-plan API Key） | `Mode=="jwt"` ⇒ JWT | **P0 用错凭据** |
| 凭据来源 | OAuth → 三步铸造（§1.3） | 同样实现了（`oauth.ExchangeAPIKey`），**但铸造后不用** | P0 死数据 |
| 模型端点 | `https://api.z.ai/api/anthropic/v1/messages` | JWT → `https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages` | **P0 端点与 plan 不匹配** |
| 端点分派 | 按 `access.mode` 四选一 | 按 `Mode`（jwt/apiKey）二选一，与 plan 无关 | P0 |
| 鉴权头 | `x-api-key` **且** `Authorization: Bearer <同一凭据>` | 二选一（JWT 只发 `Authorization`；apiKey 只发 `X-Api-Key`） | P1 少一个头 |
| 验证码 | anthropic 路径不需要 | JWT 路径**必须**带 `X-Aliyun-Captcha-Verify-Param`（要浏览器池求解） | P1 多一条重依赖 |
| 设备身份 | body 的 `metadata.user_id.device_id` | 同（v2.7.0 起已对齐） | 已一致 |
| 凭据落库形态 | `account-provider:…:api-key` 单键 | `accounts.data` 里 `jwt_token` + `api_key` 并存，靠 `Mode` 决定用哪个 | 结构差异 |
| 资格判据 | api-key 存在 ⇒ `entitled:true`，否则 provider 不可用 | 无对应概念 | 缺能力 |

### 我方代码证据

| 位置 | 现状 |
|---|---|
| `config.go` | `UpstreamZai = https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages`；`UpstreamZaiFallback = https://api.z.ai/api/anthropic/v1/messages` |
| `upstream/request.go` `BuildRequest` | `if acc.Mode=="jwt" && acc.JWTToken!=nil` ⇒ zcode-plan 端点 + `Authorization: Bearer <jwt>`；`else if acc.APIKey!=nil` ⇒ api.z.ai + `X-Api-Key` |
| `adminapi/login.go` | 建号用 `AddAccountWithIdentity(…, result.Token, …)`（⇒ `Mode="jwt"`）；**随后**调 `oauth.ExchangeAPIKey` 并把结果 `Store.SetAPIKey` |
| `store.SetAPIKey` | 只写 `acc.APIKey`，**不改 `Mode`** ⇒ 该账号永远走 JWT 分支 |
| `captcha/captcha.go` | 拉 `https://zcode.z.ai/api/v1/client/configs` 取阿里云验证码配置，再由浏览器池求解 |
| `oauth/oauth.go` `ExchangeAPIKey` | 与官方 §1.3 三步**完全一致**（含 `zcode-api-key` 名字与 `copy` 取 secretKey） |

---

## 三、问题清单

### P0-1 铸造出来的 coding-plan api-key 从未使用

- **现象**：OAuth 登录后账号同时持有 `jwt_token` 与 `api_key`，但 `BuildRequest`
  在 `Mode=="jwt"` 时无条件选 JWT ⇒ api-key 永不参与模型请求。
- **影响**：① 与官方形态不一致（凭据类型都不同）；② 我们走的是「另一个 plan 的端点」；
  ③ 那条链路才需要验证码，于是我们被迫维护浏览器池 + 验证码续期，并把
  「缺验证码」引入为新的失败模式（`captcha_required`）。
- **根因**：端点/凭据的选择键是 `Mode`，而官方是 **plan 的 `access.mode`**。
  我们把「凭证形态」当作「plan 类型」用了。

### P0-2 端点与 plan 类型不匹配

`account:zai-individual-coding-plan` 的官方端点是 `api.z.ai/api/anthropic`；
我们的 JWT 分支指向 `zcode.z.ai/api/v1/zcode-plan/anthropic`（start plan 的端点）。
即使上游两者都接受，形态上也是「用 A 套餐的入口调 B 套餐的额度」。

### P1-1 少了 `Authorization: Bearer <同一凭据>`

apiKey 模式下我们只发 `X-Api-Key`。官方 anthropic 路径**两个头都发**。
本仓库此前的修复（v2.7.0）已补齐来源头与归因头，但这一条当时未识别出来。

### P1-2 `X-Aliyun-Captcha-Verify-Param` 可能是「选错端点」的副产品

官方 anthropic 路径不使用该头。若切到 api.z.ai 端点后上游不再要求验证码，
则 `internal/captcha` 的浏览器池、`captcha_required` 错误类型与相关重试预算
都可能整体退出主链路 —— **这是收益最大、也最需要先验证的一件事**。

### P2 建议新增「资格判据」概念

客户端的「api-key 存在 ⇒ entitled」是一个明确的可用性信号，且**不花一次上游调用**。
我们可以把它落成账号的一个只读标记（例如 `has_coding_plan_key`），
用于选号前置过滤与后台展示，避免再把「没资格」误读成「额度用完」。

---

## 四、建议的验证顺序（最小改动、可即时回退）

1. **只读验证**：取一个现有账号已存的 `api_key`（`apiKey.secretKey` 形态），
   用 curl/脚本发一次 `POST https://api.z.ai/api/anthropic/v1/messages`，
   头带 `x-api-key` + `Authorization: Bearer <同值>` + `anthropic-version`，
   body 复用现有最小请求。确认 200 且**不带验证码头**。
2. 若成立：把 `BuildRequest` 的**选择键从 `Mode` 改为 plan 类型**
   （先做成开关 `ZCODE_UPSTREAM_AUTH_MODE=jwt|api-key|auto`，默认保持现状），
   端点与鉴权头一并按 §1.5 对齐。
3. 观察 24h：`captcha_required` 是否下降、账号级风控是否变化、额度统计是否仍准。
4. 再决定是否把 JWT 分支降级为回退路径。

> ⚠️ 第 2 步会同时改动**承载流量的主链路**（端点 + 凭据 + 验证码三条一起变），
> 必须开关化、小流量先行，且保留即时回退。

---

## 五、未完全确证的部分（如实标注）

1. **api-key 挂到 provider 上的确切位置**：provider 工厂里 `zhipu-account` 明确
   *不* 从 `access.apiKey` 取键（刻意跳过），而签名 fetch 收到的是空串；
   配置 schema 与覆盖逻辑支持把 access 转成携带 `apiKey` 的
   `api-key` / `zhipu-coding-plan-api-key` 形态。**推断**凭据由该层注入，
   但未定位到那一行代码。这不影响 §1.4/§1.5 的事实（键的存放位置与请求头形态已确证）。
2. **`zcodejwttoken` 的真实用途**：在 bundle 内只检索到写入与清除两处，
   未检索到读取点。它可能是给 webview / 事件上报 / 未来接口预留的，
   也可能是通过解构别名读取而未被字面量搜到。
3. **start-plan / off-peak 用什么鉴权**：`cRs` 显式把它们排除在 api-key 路径之外，
   但未定位其实质凭据（可能是 OAuth access_token 或 JWT）。
   若我们的账号实际是 start plan，则 P0-2 的结论要按 plan 类型重新判定 ——
   **所以第 4 节的第 1 步应先确认账号的 plan 类型**。

---

## 六、start plan 专项复核（2026-09-24 下午，实测）

### 1. 本机官方客户端登录的账号**确实是 Start Plan**（一手证据）

客户端日志（`~/.zcode/v2/logs/2026-09-24.log`，13:37）里的账单响应原文（结构，值已按需省略）：

```
[coding-plan-availability] billing/balance 请求完成
  {"hasActiveStartPlan":true, "payload":{"code":0,"data":{
     "plans":[{"name":"ZCode Start Plan","description":"免费 GLM 旗舰模型体验",
               "status":"active","priority":90,"starts_at":1790228449,"ends_at":1790611199,
               "entitlements":[
                 {"show_name":"GLM-5.3",      "meter":"model_usage","unit_type":"token","grant_units":3000000,"period":"daily"},
                 {"show_name":"GLM-5.3-Flash","meter":"model_usage","unit_type":"token","grant_units":5000000,"period":"daily"}]}]}}}
```

同一时刻的运行态 provider（另一行日志）：`{"providerId":"account:zai-start-plan","modelId":"GLM-5.3"}`。

`hasActiveStartPlan` 是客户端自己的实现（桌面端 bundle）：
「存在 `status=="active"` 且 `plan_id` 或 `name` 含 `start-plan`/`start plan` 的 plan」——
本例 `name` 命中，故为 **true**。

> ⚠️ **`setting.json` 会骗人**：它的 `providerFamilyConnectionSelections` 仍写着
> `individual-coding-plan`、`modelProviderFamilySelectedKeys` 仍指向
> `coding-plan:builtin:zai-coding-plan`，而**运行态 provider 已经是**
> `account:zai-start-plan`。凭据文件里也仍留着一条
> `account-provider:coding-plan:account:zai-individual-coding-plan:…:api-key`。
> **判断 plan 类型要看日志/账单，不要看 setting.json。**

### 2. 端点：与我们逐字相同

内建定义里 `account:zai-start-plan` 的 `api.baseUrl` =
**`https://zcode.z.ai/api/v1/zcode-plan/anthropic`**，
我们 `config.UpstreamZai` = 同一串 `+ /v1/messages` ⇒ **完全一致**。

（顺带确证 off-peak 是另一条路：`/api/v1/off-peak/anthropic/v1/messages`，
外加 `/api/v1/off-peak/ticket`、`/ticket/status`、`/ticket/{id}/settle` 的票据流程。）

### 3. 「是否同一个账号」——本机判不了，需要网关侧的一条数据

- 本机 `data/` 只有 `device_mid.txt`，**没有账号库** ⇒ 我们池子里的账号不在这台机器上。
- 本机客户端侧能拿到的身份指纹（sha256 前 10 位，不泄露原值）：
  - `account-provider` 凭据键里的 identity 与 `onboarding-record.decisions[0].userId` 同为 `c8542b69af`
  - `onboarding-record.entries[0].userId` 为 `62c29f4d1e`（**另一个身份**）
  ⇒ 这台机器历史上至少出现过两个账号。
- **要判定「是否同一个」，只需网关侧一个值**：账号的 `user_id`（我们是从 OAuth 响应的
  `user.user_id` 落库的，官方客户端也是同一个字段），做同样的 sha256 前 10 位比较即可。
  取法：后台账号列表 / 容器内 `accounts.db` 的 `accounts.data` 里 `user_id` 那一项。

### 4. 凭据来源：start plan 走 `oauth-session`，不走 `account-provider:…:api-key`

桌面端 bundle 里有两处决定性代码：

```js
// 凭据键构造：planKind 决定前缀
`account-provider:${ planKind === "team-coding-plan" ? "team:…" :
                     `${planKind === "start-plan" ? "start-plan" : "coding-plan"}:${providerId}` }:account:${identity}:api-key`

// 凭据读取：按 key 形态分成两个 scope
scope = oauthAllowlist.has(key) ? "oauth-session"
      : isAccountProviderKey(key) ? "account-provider" : skip
```

而本机凭据文件里**只有 `account-provider:coding-plan:…` 那一把，没有 `start-plan` 那一把**
⇒ start plan 的凭据来自 `oauth-session` scope，即登录态本身
（`oauth:zai:access_token` / `zcodejwttoken`）。

### 5. ⚠️ 对上一版报告的两处更正

| 上一版的说法 | 更正 |
|---|---|
| 「`cRs` 判定是否使用 api-key 凭据」 | **错**。`cRs` 判定的是**是否走客户端请求签名**：start-plan / off-peak 返回 false ⇒ 走 `createAccessModeUnsignedFetch`（日志原文「Client request signing skipped by provider access mode」），与 api-key 无关。 |
| 「`zcodejwttoken` 在 bundle 里没有读取点」 | **不成立**。它是被 `readProvisioningCredentials` **遍历凭据文件的键**读出来的（`oauth-session` scope），按字面量搜名字自然搜不到。 |

### 6. 因此，start plan 下仍待验证的两件事

1. **凭据挂在哪个头上**（`x-api-key` / `Authorization` / SDK `authToken`）——
   需要**一手抓包**。可行路径现成：本仓库已验证过的方法（`docs/analysis-client-golden-diff-20260924.md`）
   —— 起记录型本地反代 + `ZCODE_BASE_URL` 指向它（该变量会把 `zcode.z.ai` 来源的 URL
   改写到代理，阶段 6 已证实对 `/api/v1/client/configs` 生效）。
   ⚠️ 这次**不要**再覆盖个人 provider 的 baseUrl，要保留内建 start-plan provider，才能抓到它的请求。
2. **验证码头在 start plan 下是否必需**——同一份抓包即可回答。

---

## 七、start plan 的一手出站记录（2026-09-24 下午，客户端自带日志）

**方法**：客户端自己会把每次模型请求的 I/O 落到
`<数据目录>/cli/rollout/model-io-<session>.jsonl`，其中 `request.headers` 是**客户端自组装的
请求头**；CLI 的结构化日志（`<数据目录>/cli/log/zcode-<date>.jsonl`）另有
`model.request.*` / `model.network.*` 事件，带 `context.baseURL` 与 `context.providerId`。
两者都不需要抓包、不产生任何真实上游调用。

### 1. ✅ 运行时的端点与 provider（确证）

CLI 日志里 `providerId → baseURL` 的全部配对（按出现次数）：

| providerId | baseURL | 次数 |
|---|---|---|
| `account:zai-start-plan` | **`https://zcode.z.ai/api/v1/zcode-plan/anthropic`** | 12 |
| 用户自有中转 provider | `<用户自有中转的 /v1>` | 284 |
| 同上（**我阶段 6 的抓包**） | `https://127.0.0.1:18443` | 3 |

⇒ **start plan 的运行端点与我们的 `config.UpstreamZai` 逐字相同**，且这张表还反向验证了
阶段 6 那次抓包确实生效。

### 2. ✅ start plan 的请求是**未签名**的

```
event: model.client_signing.unsigned_sent
message: "Client request signing skipped by provider access mode"
context: {providerId: "account:zai-start-plan", reason: "access_mode"}
```
与 §六.5 的更正一致：start-plan / off-peak 被显式排除在客户端签名之外。

### 3. ✅ 官方 start plan **会带验证码头**（推翻 §六.6 的推测）

model-io 记录里的完整请求头（`account:zai-start-plan`，GLM-5.3-Flash）：

```
http-referer: https://zcode.z.ai          x-zcode-app-version: 3.14.3
user-agent: ZCode/3.14.3                  x-zcode-agent: glm
x-title: Z Code@electron                  x-platform: win32-x64
x-release-channel: production             x-os-category: windows
x-client-language: zh-CN                  x-os-version: 10.0.19044
x-client-timezone: Asia/Shanghai
x-aliyun-captcha-verify-param: [redacted]   ← ★ 有
x-aliyun-captcha-verify-region: cn          ← ★ 有
x-request-id / x-zcode-session-type / x-zcode-trace-id / x-query-id / x-session-id
```

⇒ **验证码不是「选错端点」的副产品**，start plan 这条路上它本来就要带。
我们 `internal/captcha` 的浏览器池与 `captcha_required` 失败面**不是多余的**。
（`x-aliyun-captcha-verify-region` 官方值是 `cn`。）

### 4. ⚠️ 凭据头在客户端日志里**被刻意剔除**（对照实验证明）

上表里既没有 `authorization` 也没有 `x-api-key`。这**不代表**线上没有 ——
同一份日志对**用户自有中转 provider**（阶段 6 我们抓过它的真实请求）也**同样不记录**这两个头，
而那次线上抓包明确有 `x-api-key` 与 `Authorization: Bearer <同一凭据>`。

⇒ 结论：model-io 的 `request.headers` 是「客户端自组装的那一组」，**不含 SDK 层注入的凭据头**。
**凭据头形态仍需一次真实抓包**（见 §六.6 的方法）。

### 5. 🆕 由这批记录新发现的差异（与鉴权无关，但影响伪装一致性）

| 项 | 官方（桌面端 agent） | 我们 | 备注 |
|---|---|---|---|
| `user-agent` | 记录为**裸** `ZCode/3.14.3` | `ZCode/3.14.3 ai-sdk/provider-utils/4.0.27 runtime/node.js/22` | ⚠️ 阶段 6 的**线上**抓包（CLI 0.16.9）确实是带 ai-sdk 后缀的；桌面端这条**未经线上确认**。v2.7.0 的 UA 改动可能需要回退或按路径分叉 |
| `x-os-version` | `10.0.19044`（本机真实内核） | `10.0.26100`（伪装值） | 已知的伪装值差异 |
| system 段 1 长度 | **2313** 字符 | 1211（阶段 6 golden 的 CLI 值） | 该段**不是固定串**：随 memory / skills / 插件等启用项变化 |
| system 段 2 长度 | **7536** 字符，含 `# Communicating with the user` 等行为指引章节 | 较短，仅 Environment 等 | ⚠️ 即此前判为「建议不做」的 G7 —— 官方桌面端**确实注入**这些章节 |

最后两条是**新的、值得单独评估**的差异：我们注入的 system 是官方**最精简**的一种形态，
而真实桌面端会带上更多分段。是否跟进要单独决策（它们影响模型行为，不只是身份）。

### 6. 仍未闭合的一项

**start plan 的凭据挂在哪个头**（`x-api-key` / `Authorization` / 两者）。可用的两条路：

1. **让桌面端走代理**：以 `ZCODE_BASE_URL=https://127.0.0.1:<port>` 启动桌面端
   （该变量会把 `zcode.z.ai` 来源的 URL 改写），用本次的
   `capture-only.js` 端点即可拿到真实请求头。**需要重启编辑器**。
2. **让 CLI 选中内建 provider**：本次尝试未成功（`ZCode Built-in missing`）——
   桌面端会在交付给 agent 时做账号资格解析，独立跑 CLI 复现不了；
   且 CLI 日志显示所有 account provider 在独立运行时都是 `entitled:false`。

## 八、凭据链完整闭环（静态实锤，无需抓包）

> 本节用 bundle 静态分析 + 客户端自有日志，把「start plan 凭据挂在哪个头」彻底闭合。
> 无需重启编辑器抓包 —— 桌面端 host（app.asar）与 agent（zcode.cjs）的代码路径已完整还原，
> 且 model_io 日志（真实请求记录）与 golden 抓包（HTTP 层实测）相互印证。

### 1. 最终答案：凭据挂在哪两个头

**start plan 的模型请求凭据 = `zcodejwttoken`（登录 poll 返回的 `token`）。出站两个头、同值并发：**

```
x-api-key: <zcodejwttoken>
Authorization: Bearer <zcodejwttoken>
```

与阶段 6 golden 抓包观察到的个人 provider 形态**完全一致**（`x-api-key` + `Authorization` 同值双头）。

### 2. 证据链（五步，每步一手代码）

**① agent 侧请求认证入口 —— requestAuth（zcode.cjs）**

每个模型请求前，agent 调 `refreshRuntimeHeadersBeforeAttempt` 向桌面端索要凭据：

```js
// zod schema：requestAuth 是一等概念
requestAuth: m.object({ apiKey: m.string().min(1).optional(),
                        headers: m.record(m.string(), m.string()).optional() }).strict().optional()

// 请求前必须拿到，否则抛 ModelRequestAuthMissing
if (!o.headersApplied || !o.requestAuth) throw new Error("Provider request auth was not returned before model request attempt.")
```

**② 桌面端解析 —— AccountProviderRequestAuthService.resolveCurrent（app.asar）**

```js
async resolveCurrent(t) {
  let r = await this.#t(t.accountAccess);
  if (r.planKind === "start-plan") {
    let s = await this.#e.loadOAuthTokenSet(resolveOAuthProviderId(r.family));
    return { apiKey: requireApiKey(s?.zcodeJwtToken, providerId) };   // ← start-plan 的凭据 = zcodejwttoken
  }
  if (r.planKind === "individual-coding-plan") {
    let s = await this.#e.loadIndividualPlanApiKey(n, r.family);      // ← coding plan 用铸造的 api-key
    return { apiKey: requireApiKey(s, n) };
  }
  let o = await this.#e.resolveTeamPlanApiKey(r);                     // ← team plan 走三步铸造
  return { apiKey: requireApiKey(o, n) };
}
```

三种 plan 的凭据**同形不同源**：start-plan = OAuth JWT；individual coding plan = 铸造的 `apiKey.secretKey`；team plan = 运行时铸造（`getCustomerInfo` → project 校验 → api_keys copy，即我们 `oauth.ExchangeAPIKey` 复刻的三步）。

**③ agent 合并 —— lRs（zcode.cjs）**

```js
function lRs(providerConfig, requestAuth) {
  return requestAuth ? {
    ...providerConfig,
    ...requestAuth.apiKey ? { apiKey: requestAuth.apiKey } : {},     // ← requestAuth.apiKey 并入
    ...requestAuth.headers ? { headers: qan(e.headers, t.headers) } : {}
  } : providerConfig;
}
```

**④ 出站注入 —— 双头同值（zcode.cjs，AI SDK）**

```js
// client 层：apiKey 存在且头里没有 Authorization 时，注入 Bearer
function dRs(e, t) { return !e || pRs(t, "Authorization") ? t : { Authorization: `Bearer ${e}`, ...t }; }

// SDK 层：AI SDK anthropic provider 的认证（apiKey → x-api-key；与 authToken 互斥）
let f = e.authToken ? { Authorization: `Bearer ${e.authToken}` }
                    : { "x-api-key": requireApiKey(e.apiKey, ...) };
```

`fan({ apiKey, baseURL, fetch, headers: dRs(f, g) })` ⇒ 出站 = SDK 注入的 `x-api-key` + client 注入的 `Authorization: Bearer`，**同值**。

**⑤ 旁证 —— usage 查询同源**（app.asar，start-plan 配额面板）

```js
let o = r.apiKey?.trim() ?? "";
return o ? { authorization: /^Bearer\s/i.test(o) ? o : `Bearer ${o}`, ... } : null;
```

start-plan 的账单查询同样用 `Authorization: Bearer <zcodejwttoken>`。

### 3. 为什么 model_io 日志里看不到凭据头（两层原因）

1. **AI SDK 在 SDK 内部注入 `x-api-key`**：发生在 `postJsonToApi` → `withUserAgentSuffix` 阶段，晚于记录点；
2. **记录点取的是 `resolveSnapshot` 的 `a.headers`**：`dRs` 的 `Authorization` 注入发生在 `createFactory` 传参里，同样晚于记录点。

另确认：model_io 有脱敏名单（`authorization / x-api-key / cookie / x-off-peak-ticket-id / x-client-sig / x-client-pow / x-aliyun-captcha-verify-param` 等），个人 provider 的记录同样无凭据头 —— 与阶段 6 golden（HTTP 层，有凭据头）对照即证记录层 ≠ 出站层。

> ⚠️ **记录层视图陷阱（同理适用于 user-agent）**：model_io 里的 `user-agent` 是**裸** `ZCode/3.14.3`，
> 但那是 SDK 注入 `ai-sdk/provider-utils/4.0.27 runtime/node.js/…` 后缀**之前**的视图
> （`withUserAgentSuffix` 在真正 fetch 时才追加）。桌面端与 CLI 直跑走同一条 AI SDK 代码路径，
> **真实出站 UA 均带后缀** —— v2.7.0 的 UA 改动依然正确，不要依据 model_io 回退。

### 4. 对网关的唯一修正项（GAP-15）

我们 JWT 账号的模型请求目前只发 `Authorization: Bearer <jwt>`；官方（同凭据、同源）发**双头同值**：

| 头 | 官方 start-plan | 我方 v2.7.0 |
|---|---|---|
| `x-api-key` | `<zcodejwttoken>` | **缺** |
| `Authorization` | `Bearer <zcodejwttoken>` | `Bearer <jwt>` ✓ |

修法：JWT 模式的 `BuildRequest` 补一个 `X-Api-Key: <同一个 jwt>`。改动一处 + 一条测试，风险极低
（同一凭据、与官方逐字同形）。API Key 模式已是双头（阶段 6 对齐），无需动。

### 5. 附带发现：官方客户端自己也会吃 405（重要）

本机桌面端日志（2026-09-24，官方客户端 + 全新登录的 start-plan 账号 + 官方端点直连）：

```
13:37:51  GLM-5.3-Flash  → 405  request has been blocked due to unusual activity.
13:39:15  GLM-5.3-Flash  → 405  同上
13:39:23  GLM-5.3        → 405  同上（换模型仍拦）
13:41:12  GLM-5.3-Flash  → 200  成功（同环境、同账号，2 分钟后重试即过）
```

**结论**：
1. **405 风控不是我们伪装格式的"专属"结果** —— 官方客户端 + 官方账号 + 官方端点同样触发，文案逐字相同；
2. **405 是暂时性/概率性判定**，不是账号拉黑（2 分钟后同条件恢复）；
3. 这为生产侧的"请求级风控"提供了官方侧对照：连续多次 405 后自然恢复，与我们观察到的
   「冷却到期后成功」一致。排查方向应更多放在 **出口 IP / 环境画像** 维度，而非继续加码请求格式。

（注：此账号 13:39:39 有一次 `oauth-logout:zai` 登出动作，13:41 的成功请求在登出后重新登录的会话上；
无论归因于"等待"还是"重新登录"，"405 可恢复"这一事实不变。）

### 6. 凭据链全景（三种 plan 对照）

| plan | 凭据 | 来源 | 出站头 |
|---|---|---|---|
| **start-plan** | `zcodejwttoken` | 登录 poll 的 `token`（OAuth token set） | `x-api-key` + `Authorization: Bearer`（同值） |
| **individual-coding-plan** | 铸造的 api-key | `oauth.ExchangeAPIKey` 三步（凭据键 `account-provider:…:api-key`） | 同上（golden 实证） |
| **team-coding-plan** | 运行时铸造 api-key | `getCustomerInfo` 校验 project → `api_keys` → `copy` | 同上 |
| **off-peak** | 票据流程 | `/api/v1/off-peak/ticket` → `requestAuth.source.resolve()` | `Authorization: Bearer <票据>`（`x-off-peak-ticket-id` 另带） |

**网关的定位**：我们的 JWT 账号 = 官方 start-plan 账号，凭据同源同值；补上 `x-api-key` 后，
认证形态与官方逐字一致。

# 九、账号级标记模型（2026-09-24 傍晚，依据用户账号对照修正）

> 用户纠正如下的账号对照（本文以 A/B 代称，不记录账号名）：
> - **账号 A** = 池内账号（凌晨在生产池内已被 405 风控）→ 拉到官方客户端测试**依旧 405**；
> - **账号 B** = 全新账号（从未进池）→ 同一台机器、同一客户端、**测试正常**。
>
> §八.5 「405 是暂时性/概率性判定」的结论**作废** —— 当时把「换账号成功」误读为「同账号等待后恢复」。

## 1. 日志验证的完整时间线（与用户叙述逐点吻合）

```
13:37:27  restoreCachedSession restored: zai …       ← 恢复既有会话（账号 A）
13:37:47  收到 provider runtime headers 请求
13:37:48–51  captcha-diagnostics 全流程              ← 官方客户端照常解验证码
13:37:51  GLM-5.3-Flash → 405                        ← 带着有效验证码 token 仍被拦
13:39:15  GLM-5.3-Flash → 405
13:39:23  GLM-5.3      → 405
13:39:39  oauth-logout:zai                           ← 登出账号 A
13:39:48  host 重启；13:40:45 onboarding-record / credentials.json 更新 ← 登录账号 B
13:41:01  runtime headers 请求 + captcha 流程（照常）
13:41:12  GLM-5.3-Flash → 200                        ← 账号 B 成功
```

## 2. 两段式风控模型（统一解释全部观察）

**第一段：内容/行为级触发** —— 特定请求特征触发 405。
**第二段：账号级标记** —— 一旦某账号触发过，服务端给该账号打标记；标记期内该账号**无论发什么都 405**。

| 观察 | 解释 |
|---|---|
| 生产窗口内只有 4 个固定 body 尺寸被拦（跨 14 账号 14 线路），其他 body 成功 | 内容级触发：特征在 body，不在账号 |
| 账号 A 在官方客户端（正常 body、有效验证码、直连、真实设备指纹）也 405 | 账号级标记：A 在凌晨池内触发过，标记持续 ≥14 小时 |
| 账号 B 同机同客户端立即成功 | B 无标记；**IP 与设备指纹都不是拦截依据**（A/B 同 IP 同设备，一个拦一个放） |
| 官方客户端带有效验证码 token 仍 405 | 405 与验证码无关，发生在认证之后的业务风控层 |
| 目前池内**所有**账号都处于风控状态 | 见 §3 —— 换号重试把标记扩散到全池 |

## 3. 🚨 关键推论：换号重试 = 标记扩散器

凌晨窗口的「同一 body 换号仍 405」此前被解读为「请求级风控、账号无辜」。在两段式模型下：

1. 账号 1 发特征 body → 内容级 405，**账号 1 被服务端标记**；
2. 网关换号 → 账号 2 发**同一个** body → 内容特征仍在 → 405，**账号 2 也被标记**；
3. MaxAccountAttempts 轮转下去，**每个被尝试的账号都被标记**。

池内 70 个账号被卷入、如今全池风控 —— 换号重试机制把一次内容触发放大成了全池标记。
**生产数据与该模型完全自洽**（窗口内同一 body 的多次尝试 = 逐个标记的过程）。

## 4. 对 v2.6.0 `RiskScope`（请求级判定）的重审

当前行为：同一请求内 ≥2 个账号命中 ⇒ 判定「请求级」⇒ **不冷却账号、回滚本请求已施加的风控冷却**。

在账号级标记模型下，这个行为**方向反了**：

| | 旧理解 | 新理解 |
|---|---|---|
| 「换号仍 405」的含义 | body 有问题，账号无辜 | body 有问题，**且每个被尝试的账号都已被服务端标记** |
| 回滚冷却的效果 | 「不冤枉账号」 | **有害**：账号已被服务端标记，接下来任何 body 都 405；本地不冷却 → 立即被再次选中 → 再 405 → 白耗重试预算并拖慢请求 |
| 正确行为 | — | 请求级命中时**照常冷却**（甚至更长，对齐标记持续期 ≥14h 量级）；换号**立即停止**（换号只会扩散标记） |

⚠️ 未排除的反面情形：若存在「纯请求级、账号不标记」的风控形态，长冷却会误伤可用账号。
但账号 A 的实测（官方客户端 + 正常 body 仍拦）证明**本案中标记真实存在且持续**。
建议：请求级判定改为「冷却不回滚 + 立即停换号」，并加观测字段区分
「标记型账号」（换 body 后仍 405）与「纯内容触发」（换 body 后即恢复）。

## 5. 行动建议（优先级重排）

1. **P1-3 重放防护升为最高优先**：同指纹 body 已触发过 405 ⇒ 60s 内直接快速失败。
   新理由不再是「省账号」，而是**阻止标记扩散** —— 这是全池覆灭的直接机制。
2. **RiskScope 行为重审**（§4）：请求级命中 → 冷却不回滚 + 停止换号；新增观测字段。
   与 P1-4（streak 棘轮）合并处理：标记真实存在时 streak 递增并不冤枉，重点是**别回滚冷却**。
3. **新账号 B 入池防护**：B 是当前唯一「干净」账号。入池后务必先上 P1-3，否则一次特征请求
   又会从 B 开始扩散。
4. GAP-15（JWT 补 `x-api-key` 双头）维持原优先级 —— 格式对齐与风控机制独立。
5. **标记解除观测**：A 账号在官方客户端里隔天重测（或等 24h 后用网关测），
   确认服务端标记是否有 TTL —— 这决定「冷却时长对齐多少」。
