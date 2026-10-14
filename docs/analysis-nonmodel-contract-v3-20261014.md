# v3 非模型接口契约与派发边界

日期：2026-10-14（Asia/Shanghai）。本轮仅修改仓库、编写隔离回归；没有访问生产服务器、读取运行 `data/` 或使用真实账号。

## 已确认的接口差异

| 用途 | 目标与认证 | 本轮处理 |
| --- | --- | --- |
| 验证码公共配置 | `zcode.z.ai/api/v1/client/configs`；不带 JWT、API Key、Cookie | 复用 `config.ZcodeSourceHeaders()`；保留版本/平台查询与缓存，拒绝跟随重定向 |
| 额度 | 配置的 ZCode billing origin，`/billing/balance`；JWT 使用 Bearer，API Key 使用 `x-api-key`，两者不混发 | 保留完整来源档案及账号设备，实际请求前冻结 route；派发与失败清理使用同一快照 |
| 领取预览/领取 | 相同 billing origin，`/billing/preview`、`/billing/claim`；沿用各自 JWT/验证码契约 | 注入 Store 派发准入，后台、手动、定时和 CLI 均覆盖；不把计费 405/3012 算成模型淘汰证据 |
| 激活遥测 | `zcode.z.ai/api/v1/event/report`；无 Authorization | 继续复用账号客户端；不因其他同源调用的证据就盲目改遥测来源头/认证 |
| OAuth token | `zcode.z.ai/api/v1/oauth/token`；授权码在 JSON，不带现有账号认证 | 明确 `provider=zai`，补同源来源档案；每次真实 HTTP 重新取得当前登录路由许可 |
| OAuth 业务 token | `api.z.ai/api/auth/z/login`；OAuth access token 在 JSON | 不向不同源业务站套用 ZCode 来源头或模型双鉴权 |
| 客户资料/个人 Key | `api.z.ai/api/biz/...`；customerInfo 裸业务 token，Key 管理使用 Bearer | 保留 3.15.3 定点认证差异；共享 context，每一步重新准入，旧登录结果不得回写新凭据 |

公开配置没有账号绑定，仍使用进程默认公共配置客户端（包括其环境代理语义）；它不声称使用某个账号线路。账号相关的额度、领取、遥测及 OAuth 空线路现在都使用显式直连 Transport，不再把环境代理误标为 direct。这里验证的是选定 Transport/本地代理实际收到请求，不是公网出口 IP 或供应商风控结论。

## 官方来源与不扩大证据的边界

本轮读取既有 3.15.3 安装包的格式化副本，未运行客户端或发送真实上游请求：

- `formatted-host/chunk-OUCOFHX4.js:29401–29439`：`buildZCodeSourceHeadersFromContext` 的统一来源头；`X-Device-Mid` 是有设备值才附带的可选字段，没有模型 `X-ZCode-Agent`。
- `formatted-host/index.js:60479–60500`、`60585–60700`：`NodeApiClient` 仅对 `endpointOrigin` 同源请求注入来源档案；不能把这批头无条件推广到 `api.z.ai` 或 CDN。
- 同文件 `48946–48985`：公共配置 `credentials=omit`、`redirect=error`，缓存由请求 origin/版本/平台决定。
- 同文件 `42595–42644`：Z.AI token 兑换正文明确包含 `provider=zai`，没有账号 Bearer；来源头由上层同源客户端补入。
- 同包 `shared.diff`/`chunk-OUCOFHX4.js:32235–32292`：3.15.3 的客户资料认证与个人 Key 认证分开。

原始 `host/index.js` SHA-256：`dfa58d6a16de14c5cc43a0d0bc0c8f8d7da6f50d7454d987841e35203efa6fe3`；原始 shared chunk：`e87794b90a7b781003406d13d59a53903443818e5a133f8dfd26cd8a41b58cb1`。本地证据根为 `D:/augment-projects/zcode2api-plus/temp/zcode-v3153-20261014`，`key-files-manifest.json` 记录原件及格式化副本哈希。安装包副本不提交到 Git，也不把安装包哈希当作完整官方 Git 提交对应证明。

遥测的实际 fetch 调用链未取得同等级证据，因此本轮只固定其无认证/同账号出口/事件字段契约，不宣称其来源头已经全量官方对齐。TLS、压缩、头顺序、实际公网出口及供应商接受度仍未在本轮验证。

## 并发与旧身份保护

- claim/quota 的原生客户端在真实 `RoundTrip` 前取得 Store 许可；已删除线路、旧凭据/路由代次拒绝派发，不能把本地拒绝再当作代理故障删除一次。claim/quota/OAuth 不自动跟随重定向，307/308 也不能把认证头、授权码、业务 token 或验证码正文转往新目标。
- 登录命名线路使用 `ProxyProfile.Generation`；裸 URL 也校验严格淘汰集合。自动登录允许一次零出站的本地重选，手工退役线路明确拒绝。
- OAuth Key 兑换的五步链均共享取消与准入；失败收尾传原始线路代次，不能用迟到错误删除 A→B→A 后的新版本。
- 自动登录直连保存 `auto_pending_proxy`，不在登录成功后重新挑线；显式人工直连仍保留。已有有效人工绑定不会被自动收尾覆盖。
- `ClaimState` 和 OAuth Key 回写改为 `UpdateForRequest`，账号 ID 相同但凭据已变时不应用旧结果。
- 双次空额度判据继续要求真正新号、两次独立完整观察、未使用等条件，并关联两次查询的进程内代次证明；不以相同 URL 字符串代替相同线路版本。

## 回归与验收口径

新增 `internal/{claim,quota,captcha,oauth}/nonmodel_contract_test.go` 覆盖实际 HTTP 目标、认证隔离、来源档案、代理/直连、公共配置重定向，以及 OAuth 各步骤准入/释放。`internal/adminapi/claim_contract_test.go` 固定旧身份领取结果不能覆盖新登录状态。已有 HTTP 407、3007、业务 401、风控失效/冷却、取消和并发回归继续保留。

本地只做编辑、gofmt 和差异检查，不运行测试/构建。最终以当前发布提交的完整 GitHub Actions 结果为准；单个新增测试、旧提交绿灯、或该文档本身均不是验收通过证明。
