# ZCode v3144（3.14.4）更新兼容性分析

日期：2026-09-30

审计基线：`f66d3eb0aaa759a4e8f66b3005175e9dae9699fd`

状态：已完成本机新客户端静态分析、新旧关键模块差分、公开配置实测及离线复现；未做真实账号端到端调用。

> 后续进展（2026-09-30）：已按用户批准完成代码适配及离线回归，方案与结果见 `D:/augment-projects/zcode2api-plus/docs/plan-zcode-v3144-captcha-fix.md`。下文保留修复前基线的分析证据，不表示当前代码仍存在这些缺口。

## 0. 一句话结论

**需要适配，但不是上游接口整体失效：新版新增的 `captcha.skip_model_request=true` 已在线生效，项目却忽略它，仍可能因一个已经不需要的模型验证码而本地返回 503。应只跳过模型取码，保留套餐领取验证码；仅升级版本号或关闭浏览器均不能解决。**

## 1. 范围与证据边界

- 用户提供更新日志：2026-09-29，v3144，「为了进一步优化免费套餐的使用体验，关闭模型请求验证码校验」。
- 项目侧证据来自当前 Go/Rust 源码和离线测试；未读取 `data/` 账户凭据，未发送真实账户模型/领取请求。
- 本机 `ZCode.exe` 的 FileVersion/ProductVersion 均为 **3.14.4.7912**；`app.asar/package.json` 的 `@zcode/desktop` 版本为 **3.14.4**，与用户所说 v3144 对应。客户端新逻辑及公开配置采样见第 6 节。
- 仓库旧注释里「官方实测」「上游返回 405」等历史观察，只说明现有设计背景，不能独立证明新客户端/服务端仍保持该行为。

## 2. 项目侧结论

**存在明确的兼容性差距，但不是“新版本一定让所有请求失败”：当前项目忽略新配置字段，仍在 JWT 模型请求前强制取验证码。**

模型验证码校验已关闭时，验证码求解成功的部署可能继续工作，但承担不必要的浏览器耗时、资源占用和故障依赖；求解器不可用的部署仍会在访问模型接口之前返回本地 `503 captcha_required`。API Key 路径不经过这段验证码前置逻辑。是否已有真实账号受影响，需要独立的账户端到端验证，不能由离线测试推断。

| 项目路径 | 现有实现 | 与本次更新相关的影响 |
| --- | --- | --- |
| JWT 模型请求 | 每次尝试先获取验证码；未识别模型级跳过字段 | 新增字段生效后仍多做验证；求解失败仍本地阻断模型请求 |
| API Key 模型请求 | 不取验证码，走另一上游端点 | 没有发现由本次验证码变更直接触发的兼容性问题 |
| 套餐领取 | 与模型共用管理器，但要求 token 非空 | 不能通过全局关闭验证码来代替模型级跳过，否则会先在本地拒绝领取 |
| 版本/请求头 | 默认仍报告 3.14.3 | 存在版本漂移；仅改版本不能让代码认识新字段 |
| 签名/重试 | 不生成签名，保留既有验证码/限流重试 | 新客户端仍明确排除 start-plan/off-peak 的请求签名；验证码重试策略需要与模型级跳过分离 |

## 3. 核心发现及精确源码位置

### 3.1 新字段会被 JSON 解析忽略

`captcha.Config` 只有 `Enabled/Prefix/Region/SceneID` 四个字段；配置响应内部结构也只接收 `enabled/prefix/region/sceneId`，构建结果同样没有模型请求跳过属性。因此，即使配置返回 `skip_model_request:true`，当前代码也只能看到 `Enabled:true`。

来源：

- `D:/augment-projects/zcode2api-plus/internal/captcha/captcha.go:28-34`
- `D:/augment-projects/zcode2api-plus/internal/captcha/captcha.go:130-154`

配置请求的 `app_version` 来自 `config.ZcodeClientVersion`，`platform` 来自 `config.ZcodeClientPlatform`，超时 15 秒。配置成功缓存默认 10 分钟；失败/缺少 captcha 对象回退 `Enabled:true` 的默认配置。`Invalidate()` 只清 token，不清配置缓存。因此未来适配时还需考虑配置变化和失败回退的行为，不能默认每次验证码重试都会刷新远端策略。

来源：

- `D:/augment-projects/zcode2api-plus/internal/captcha/captcha.go:36-37,91-121,145-147,216-222`
- `D:/augment-projects/zcode2api-plus/internal/config/config.go:81-83`

### 3.2 JWT 模型请求在出站前被验证码阻断

`needsCaptcha := acc.Mode == "jwt"`，不区分免费/付费套餐，也不查询模型级验证码策略。每次 HTTP 尝试前先执行 `GetVerifyParam`；报错会累计最多 3 次，然后返回 `503 captcha_required`。真正的 HTTP 出站在后面，故求解失败时不是模型服务拒绝，而是项目根本没有尝试访问模型服务。

来源：

- `D:/augment-projects/zcode2api-plus/internal/gateway/engine.go:253,266-286,313-337,986-990`
- `D:/augment-projects/zcode2api-plus/internal/gateway/classify.go:21-22`

现有路由测试直接验证此现象：`TestJWTWithoutSolverReturnsCaptchaRequired` 断言 503，并断言模型上游调用次数为 0。这也是本次更新后应该重新设计的旧行为约束。

来源：`D:/augment-projects/zcode2api-plus/internal/gateway/engine_test.go:1144-1163`

### 3.3 关闭“浏览器求解器”不等于关闭“模型验证码要求”

`ZCODE_CAPTCHA_BROWSER=false` 只使管理器不调用求解器；在 `Enabled:true` 且无有效人工缓存时，管理器仍返回 `ErrUnavailable`。Go 默认值为 false，Docker Compose 默认值为 true，两个部署入口的默认行为不同。不能建议用户简单关浏览器开关来解决这次变更。

来源：

- `D:/augment-projects/zcode2api-plus/internal/captcha/captcha.go:172-193`
- `D:/augment-projects/zcode2api-plus/internal/config/config.go:89-96`
- `D:/augment-projects/zcode2api-plus/docker-compose.yml:50`
- `D:/augment-projects/zcode2api-plus/cmd/zcode2api/main.go:104-109`

### 3.4 `enabled:false` 的 nil token 行为：模型支持、领取不支持

管理器在全局配置明确禁用时返回 `(nil,nil)`。网关检查 `token != nil` 后才取字段，所以会正常组装无验证码头的模型请求；出站构造器仅在 `verifyParam != ""` 时添加验证码头。

来源：

- `D:/augment-projects/zcode2api-plus/internal/captcha/captcha.go:157-175`
- `D:/augment-projects/zcode2api-plus/internal/gateway/engine.go:273-286`
- `D:/augment-projects/zcode2api-plus/internal/upstream/request.go:302-307`

但是领取服务将 `err != nil || token == nil` 都视为本地验证码失败，并且不会调用 `/billing/claim`。因此把新的模型级 `skip_model_request` 直接映射成共享 `Enabled:false`，或让共享 `GetVerifyParam` 在该字段为 true 时全局返回 nil，都会误伤领取。

来源：`D:/augment-projects/zcode2api-plus/internal/claim/claim.go:344-355`

共用关系不是推测：主程序把同一个 `cm` 注入网关与后台；后台通过 `claim.NewService(h.Captcha)` 创建领取服务；异步池也拿到这个管理器。

来源：

- `D:/augment-projects/zcode2api-plus/cmd/zcode2api/main.go:104-127`
- `D:/augment-projects/zcode2api-plus/internal/adminapi/adminapi.go:32`
- `D:/augment-projects/zcode2api-plus/internal/adminapi/claim.go:232,377,445`

另一个缓存细节：人工 token 命中发生在配置检查之前。即使远端后续关闭验证，已缓存 token 仍可能被复用至 TTL 过期；后续实现模型级跳过应先判模型策略，避免继续发送陈旧验证码头。

来源：`D:/augment-projects/zcode2api-plus/internal/captcha/captcha.go:164-175,196-213`

### 3.5 不能直接把 `needsCaptcha` 改为 false

网关不仅用 `needsCaptcha` 控制验证码求解，还将同一布尔值传给 `NormalizeBody(actualBody, needsCaptcha)`。后者的参数实际叫 `needsZcodeSystem`，用于注入 JWT 的 system blocks。简单把 `needsCaptcha` 改成 false 会连同现有 JWT 请求体规范化一起停用，而不是仅取消验证码。

来源：

- `D:/augment-projects/zcode2api-plus/internal/gateway/engine.go:253,289-291`
- `D:/augment-projects/zcode2api-plus/internal/gateway/body.go:11-19,52-67`

适配需要拆分「JWT 请求形态」与「当前模型请求是否要求验证码」两种语义；保留鉴权、system、metadata、归因头等与本次验证码开关无关的行为。

### 3.6 版本、请求头与签名

- Go 默认上行版本为 `3.14.3`，`ZCODE_CLIENT_VERSION` 可覆盖，非模型 UA 默认由该版本派生；模型 UA 还附加 AI SDK/runtime 段。`zcode-switch` Rust 常量也仍为 `3.14.3`。来源：`D:/augment-projects/zcode2api-plus/internal/config/config.go:213-226`；`D:/augment-projects/zcode2api-plus/internal/config/profile.go:161-163`；`D:/augment-projects/zcode2api-plus/zcode-switch/src-tauri/src/quota.rs:23`。
- 示例环境配置注释甚至仍为 `3.11.2`；注释本身不会影响运行，但若用户照抄会覆盖真实默认值。来源：`D:/augment-projects/zcode2api-plus/.env.example:58-59`。
- JWT 与 API Key 按不同端点构造，Authorization 与 X-Api-Key 同值；验证码头是附加字段，不参与代码内的鉴权计算。来源：`D:/augment-projects/zcode2api-plus/internal/upstream/request.go:236-249,260-278,302-307`。
- 下游送来的验证码头会被过滤，所以跳过模型验证码时仍应保持过滤，不能通过透传把它重新加回。来源：`D:/augment-projects/zcode2api-plus/internal/upstream/request.go:44-56,319-329`。
- 当前请求构造器明确剔除 `x-client-sig/ts/version/nonce/pow/sign-verified` 和 `x-app-id`，没有生成相应签名。新客户端仍在 `requiresClientRequestSigning` 中对 start-plan/off-peak 返回 false，未发现免费套餐路径新增强制客户端签名的证据；这不代表其他付费 provider 都不需要签名。来源：`D:/augment-projects/zcode2api-plus/internal/upstream/request.go:63-72` 及第 6.4 节。

### 3.7 重试与跨路径影响

验证码、3010 并发、429 限流、过载重试有独立预算；但它们的同账号重试都回到循环顶部，JWT 会再次取验证码，故验证码依赖还可能使原本应成功的限流/过载重试在本地失败。

来源：`D:/augment-projects/zcode2api-plus/internal/gateway/engine.go:135-143,266-286,477-488,509-523,533-551`

HTTP 验证码挑战及 JSON `code=3007` 仍触发清 token、同账号重试；领取的 3007 则最多换码重试一次。模型策略跳过后是否保留按实际挑战回退，必须明确与新客户端行为对齐，避免一边跳过取码、一边把所有重试不断归类成验证码失败。此处是实现建议，不是已观察到的线上新错误。

来源：`D:/augment-projects/zcode2api-plus/internal/gateway/engine.go:445-458,662-670`；`D:/augment-projects/zcode2api-plus/internal/claim/claim.go:344-375`

## 4. 离线验证

运行环境：`go version go1.25.14 windows/amd64`。本次显式关闭真实浏览器测试入口，未启动正式服务。

```powershell
$env:ZCODE_REAL_BROWSER_TEST=''
$env:ZCODE_CAPTCHA_BROWSER='false'
go test ./internal/gateway ./internal/captcha ./internal/claim ./internal/config ./internal/upstream -count=1
```

结果全部通过：

```text
ok zcode2api/internal/gateway  3.443s
ok zcode2api/internal/captcha  7.081s
ok zcode2api/internal/claim    1.055s
ok zcode2api/internal/config   1.444s
ok zcode2api/internal/upstream 2.301s
```

测试通过说明当前代码与既有测试一致，**不代表已支持新字段**。当前相关覆盖包括：

- 显式全局禁用返回 nil：`D:/augment-projects/zcode2api-plus/internal/captcha/captcha_test.go:66-74`。
- 配置对象缺失/HTTP 失败时启用默认配置：同文件 `46-63`。
- JWT 求解成功后发送验证码与 system：`D:/augment-projects/zcode2api-plus/internal/gateway/engine_test.go:1108-1141`。
- JWT 无求解器时本地 503、模型出站为零：同文件 `1144-1163`。
- CAPTCHA 与 3010 重试预算独立：同文件 `500` 起的 `TestCaptchaRetryDoesNotConsumeBusyBudget`。
- 领取 3007 后更换验证码：`D:/augment-projects/zcode2api-plus/internal/claim/claim_test.go:130-164`。

本次不修改业务代码。后续适配至少应补充以下用例：

1. `enabled:true + skip_model_request:true`：JWT 模型成功，无 solver 调用、无验证码头，但 system、metadata 和鉴权仍在。
2. 相同配置：领取仍取码并发送验证码头，证明两条链路没有被全局禁用。
3. 有人工缓存 token 时模型跳过仍生效，不发送缓存头。
4. 字段 false/缺失、全局 disabled、配置缓存刷新与获取失败的确定性行为。
5. 模型跳过状态下 3010/429/过载重试，以及确实返回验证码挑战时的明确策略。

## 5. 建议处理顺序

1. **优先适配模型级策略字段**：增加独立 `SkipModelRequest` 解析与模型调用入口判断；不要全局关闭 Manager，不要删掉领取验证码。
2. **拆开耦合布尔值**：保留 JWT 请求体处理，验证码需求另算；避免误删 system 注入。
3. **补离线回归测试**：覆盖跳过模型但保留领取、人工缓存以及重试场景。
4. **再同步版本声明与文档**：同步 Go 默认版本及环境示例到 3.14.4；若同时维护独立的 zcode-switch，再同步其 Rust 常量。单改 `ZCODE_CLIENT_VERSION=3.14.4` 不足以修复解析缺口。本次实测旧版 3.14.3 也收到新开关，不能断言落后这一个补丁版本已导致领取资格失效。
5. **最后做可控端到端确认**：如获授权，使用明确指定的测试账号各验证一次模型与领取；在此前只报告“代码差距已证实，实际账户可用性尚未验证”。

## 6. 新客户端及官方公开接口：一手证据

### 6.1 样本来源与差分边界

全部安装文件只读，未修改或重启正在运行的客户端：

| 文件 | 识别结果 |
| --- | --- |
| `D:/Programs/ZCode/ZCode.exe` | FileVersion/ProductVersion：3.14.4.7912 |
| `D:/Programs/ZCode/resources/app.asar` | 326,915,607 字节；内嵌 package 版本 3.14.4 |
| `D:/Programs/ZCode/resources/glm/zcode.cjs` | 14,820,968 字节；随客户端分发的 agent/CLI bundle |

SHA-256：

```text
app.asar  172d6f333e61642ce3882250949fafe8180f75b5b8e5552244ca2c59ca05d14e
zcode.cjs fad4c35c4c36ec210d8a06d3fa0e77de23c8545e2eb6ff90aea1eb38d1e6275f
```

本次从 ASAR 目录表按 `8 + headerSize + entry.offset` 提取相关代码，保存于忽略目录 `D:/augment-projects/zcode2api-plus/.workbuddy/zcode-v3144-analysis/app/`。没有读取个人 provider 配置、客户端凭据或账号日志。

旧版比较样本使用仓库已有的 `D:/augment-projects/zcode2api-plus/.workbuddy/tmp-logs/zcode-app/`。其公共版本常量为 3.14.3。历史提取文件存在开头缺 8 字节、末尾多 8 字节的偏移问题，因此本次只比较完整的内部函数/共同内容，并排除边界、构建标识及 chunk 哈希变化。**没有完整旧版安装包和旧版 glm bundle，不能将以下关键模块比较表述成整个客户端的完整二进制差分。**

### 6.2 真正的变更是模型级开关，不是全局关闭 captcha

**Host 配置映射**，新包 `out/host/index.js` 的 `getCaptchaConfig`：

```javascript
// 旧：直接返回 data.configs.captcha。
// 新：读取新字段，转换成渲染进程接口使用的驼峰命名。
const captcha = (await this.getClientConfigs()).data?.configs?.captcha;
if (!captcha) return null;
const { skip_model_request, ...rest } = captcha;
return {
  ...rest,
  ...(typeof skip_model_request === "boolean"
    ? { skipModelRequest: skip_model_request }
    : {})
};
```

以上为去混淆等价转写；原始代码中 `skip_model_request` 的零基字符偏移为 **892132**。文件：`D:/augment-projects/zcode2api-plus/.workbuddy/zcode-v3144-analysis/app/out/host/index.js`。

**Renderer 模型前置流程**，新包 `out/renderer/assets/styles-Qlp0Bew7.js`：

```javascript
// 原始函数 xnn / Lnn 的等价关键逻辑。
if (!(provider.access.type === "zhipu-account" &&
      provider.access.mode === "start-plan")) return;

const config = await getCaptchaConfig();
if (config?.enabled === false || config?.skipModelRequest === true) {
  clearProviderCaptchaCache(providerId);
  return { headers: {} };
}
// 其他情况保留原有取码流程。
```

原始 `async function Lnn` 从零基字符偏移 **4173483** 开始，`skipModelRequest` 在 **4173649**。旧文件 `styles-DEELZGp2.js` 对应函数没有这条跳过分支。模型跳过还会清理 provider 的验证码缓存，不只是停止新求解。

**领取没有跟着跳过**：同一新 renderer 的领取流程仍调用独立的 `Nnn` 取码，非空后才调用 `claimManualPlan`。Host 的 `getManualClaimPlanPreviews`（1232 字符）和 `claimManualPlan`（1380 字符）与旧样本中的完整方法逐字符相等；领取继续发送 `X-Aliyun-Captcha-Verify-Param`。因此“删掉所有验证码代码/停用整个浏览器池”并非正确适配。

这些偏移均为 Python 解码 UTF-8 后的字符串字符索引，**不是 ASAR 绝对字节偏移**。

### 6.3 官方公开配置实测

时间：**2026-09-30 09:39:00，Asia/Shanghai**。仅无凭据 GET 请求，User-Agent 与所查询版本一致；未使用账号、设备指纹或 Cookie。

| app_version | platform | HTTP / 业务码 | captcha.enabled | captcha.skip_model_request |
| --- | --- | --- | --- | --- |
| 3.14.3 | win32-x64（当前网关形态） | 200 / 0 | true | true |
| 3.14.3 | windows-x86_64（桌面查询形态） | 200 / 0 | true | true |
| 3.14.4 | win32-x64 | 200 / 0 | true | true |
| 3.14.4 | windows-x86_64 | 200 / 0 | true | true |

关键响应：

```json
{
  "enabled": true,
  "prefix": "no8xfe",
  "region": "cn",
  "sceneId": "11xygtvd",
  "skip_model_request": true
}
```

官方接口来源：[旧版参数/网关平台](https://zcode.z.ai/api/v1/client/configs?app_version=3.14.3&platform=win32-x64)、[新版参数/桌面平台](https://zcode.z.ai/api/v1/client/configs?app_version=3.14.4&platform=windows-x86_64)。四份完整公开响应及 UTC 采样时间保存于 `D:/augment-projects/zcode2api-plus/.workbuddy/zcode-v3144-analysis/public-config-*.json`。

这同时排除了两种误判：

1. **不是 `enabled` 已变成 false 而现有代码会自动适配**：它仍为 true。
2. **不是只要版本报 3.14.4 就能解决**：两个版本都得到新字段，而现有解析器都会丢弃它。

这里只证明公开客户端策略已下发，不能替代某一具体账号、代理出口与模型的在线成功验证。

### 6.4 其他协议检查结果

| 检查项 | 新样本事实 | 对项目的判断 |
| --- | --- | --- |
| 免费套餐模型接口 | bundled provider `account:zai-start-plan` 仍为 `anthropic-messages`，baseUrl 为 `https://zcode.z.ai/api/v1/zcode-plan/anthropic` | 未发现需要迁移模型端点/协议的迹象 |
| 领取接口 | Host 仍用 `/api/v1/zcode-plan/billing/preview` 和 `/billing/claim`，完整方法与旧样本相等 | 保留现有领取流程，不应套用模型跳过 |
| 激活遥测 | 新 `out/main/chunk-ITPGAKRE.js` 与旧 `chunk-VN4HYEPZ.js` 的差分仅有导入 chunk、构建标识、版本及旧提取边界 | 未发现此次改变激活事件体的证据 |
| 模型 system 静态块 | 从新 glm 的静态字符串/数组还原，与项目两块分别逐字符相等：42 / 1211 字符 | 无需因本次验证码更新删改 system 静态块 |
| 客户端签名 | 新 glm 的 `cRs` 注册名为 `requiresClientRequestSigning`，仍对 start-plan/off-peak 显式返回 false | 没有证据表明免费套餐以强制签名替代此次验证码；不扩展到所有付费模式 |
| 模型鉴权与归因 | 新 glm 保留 Anthropic Authorization、API key 合并逻辑，以及 request/session/query/trace 归因构造函数 | 不应把关闭验证码理解成关闭鉴权或其他风控 |

来源：`D:/Programs/ZCode/resources/config/provider/zcode-builtin.json`；本次解包的 host/main 文件；`D:/Programs/ZCode/resources/glm/zcode.cjs` 中 `cRs`（约 3711950）、注册名（约 3725803）、`xst`（3938248）及 `# Harness`（4827732）附近的代码。system 比对只解析静态字符串和数组，未执行官方 bundle。

未实际发送新的模型流请求，所以本次**不宣称验证了流式成功、首字延迟改善、30 分钟断流修复或全部风控已关闭**；更新日志和上述静态证据都不足以得出这些结论。

## 7. 对新字段的离线复现及完整回归

为了不改变业务源码，使用 Go `-overlay` 将临时探针映射进 captcha 包。夹具使用上述公开 `captcha` 对象，仅请求本地 `httptest.Server`，无真实求解器或账号：

```powershell
go test -overlay .workbuddy/zcode-v3144-analysis/overlay.json ./internal/captcha -run '^TestV3144Probe' -count=1 -v
go test ./internal/gateway -run '^TestJWTWithoutSolverReturnsCaptchaRequired$' -count=1 -v
```

实际结果：

- `enabled=true + skip_model_request=true` 且无 solver：仍返回 `ErrUnavailable`。
- 同样配置、有计数型假 solver：**solver 仍被调用 1 次**并返回验证码 token。
- 既有网关无 solver 测试：**503 captcha_required，模型上游调用数 0**。

探针通过表示“当前缺口被稳定复现”，不是“适配已修好”。临时文件位于忽略目录 `D:/augment-projects/zcode2api-plus/.workbuddy/zcode-v3144-analysis/`；本次没有把旧错误行为新增为项目的长期测试契约。

另运行了 captcha/upstream/gateway/claim/config/openai/asyncpool 七个相关包测试，全部通过；全项目 `go test ./...`、`go vet ./...`、`go build ./...` 均通过。既有测试没有识别新模型开关，因此通过不能作为兼容性已完成的证明。

**交付状态：只新增本分析文档；没有修改业务代码、运行时配置、数据库或安装客户端，也没有提交/推送。**
