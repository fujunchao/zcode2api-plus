# 第四部分 5 项静态问题修复方案

基线：v2.8.6-go / 7c433a7。仅处理审查报告第四部分；真实账号数据与无关未提交文件不参与。

| 编号 | 修复方案 | 回归边界 | 状态 |
|---|---|---|---|
| 1 | 下载平台、版本、包名统一解析；amd64 映射为 x64，未知平台明确拒绝 | 五平台选择及 Intel macOS 压缩包 | 已修复，定向回归通过 |
| 2 | CLI/后台共用 OAuth 入库方法，重复身份刷新 JWT 并清除旧鉴权状态 | 重登入库、持久化、原线路/指纹/用量保留 | 已修复，定向回归通过 |
| 3 | 分组草稿与远端数据分离；提交版本确认防止迟到响应覆盖新输入 | 页面交互：跨组保存、保存失败、保存期间继续编辑 | 已修复，定向回归通过 |
| 4 | 单账号刷新检查业务 ok=false，优先展示 result.error/message | 页面点击刷新及成功/业务失败/网络失败 | 已修复，定向回归通过 |
| 5 | 新增进程级请求统计，在 HTTP 入口及协议终止处计数，真实上游尝试独立计数 | 鉴权/校验失败、成功、重试、断流、异步票务、并发快照、后台展示 | 已修复，定向回归通过 |

## 指标契约

- 仅统计四个模型 POST 入口，不包含 models、管理接口、额度/领取等后台请求。
- 每个 HTTP 请求只记一次 total；输入与鉴权失败同样属于请求失败。
- succeeded/failed 是已完成请求，active 是在途；canceled 为 failed 的子集。
- 成功率为 succeeded / (succeeded + failed)，无已完成请求时返回 null。
- 上游模型 HTTP 每次尝试计 upstream_attempts；同一请求第一次以后的尝试计 retries。
- HTTP 200 中的 SSE error、缺少正常终止或票务超时不能计作成功。
- 指标从本次进程启动计起，不从旧 UseCount/FailCount 推测，也不修改账号 JSON/schema。

## 实施与发布

先复现再修复，使用本地模拟上游/临时库与前端交互测试。新增前端测试 CI 并检查 dist 与源码一致。
本地执行构建、静态检查、Go/前端回归；推送分支后等待 Linux CI（含 race/SDK）通过，再发布 v2.8.7-go。
沿用已确认的未签名提交，不创建新签名密钥。

## 实施结果与验收

- 浏览器平台回归：[platform_test.go](../internal/captcha/platform_test.go)。已核对公开 SHA256SUMS，macOS 两种架构的压缩包名称均存在；没有执行 macOS 真机运行验收。
- 授权共享入库：[oauth_account.go](../internal/store/oauth_account.go)，CLI 重登与存储失败原子性分别有回归；账号快照不会共享到后续操作。
- 前端采用分组草稿钩子，设置页三种交互、额度刷新四种结果、仪表板新口径共 8 个交互用例通过。测试位于 [frontend/test](../frontend/test/)。
- 请求统计：[requeststats](../internal/requeststats/)；四个模型入口覆盖成功、重试、流内错误、断流、鉴权失败和模型校验失败，另覆盖票务超时、取消、并发快照与 panic。
- 关键 Go 回归连续三轮通过；go vet、构建、前端类型检查与生产构建通过，前端生产依赖审计为 0 个已知漏洞。
- 本机全量 Go 复验的业务断言通过，但出现已在前一版本原提交对照中复现的 Windows TempDir 清理失败。发版以 Linux CI 全量、race、SDK 与新前端检查通过为门槛，不把此处描述为本机稳定全绿。
- 内嵌 dist 已重建；CI 增加 npm test、生产构建及 dist 一致性检查，避免源码修复未进入发布二进制。
- 首轮 CI 发现模板 CRLF 导致的 HTML 产物差异，已用 .gitattributes 固定模板/文本产物 LF，并重新构建；不放宽一致性检查。

## 兼容性

旧账号调用/Token 计数和导出格式不变。monitor 的 requests.errors 为 failed 的兼容别名；usage 新增 requests。
新指标 scope=process，started_at 表示统计起点，进程重启后清零；不提供无法从旧字段准确恢复的历史请求成功率。
内部重试按同一请求中的模型 client.Do 尝试计数，不包含额度、验证码或客户端另发的新请求。
