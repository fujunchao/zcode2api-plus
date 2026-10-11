# New API 链路诊断增强与 v2.9.12-go 发布计划

## 目标与边界

服务链路为“调用者 → New API → Docker 网关 → 模型上游”。本次解决的是日志缺口，
不是根据有限日志认定供应商的风控规则，也不重放生产风险请求。

- 不改变鉴权、上下文处理、账号/代理调度、重试预算、风险阈值或冷却策略。
- 不采集最终调用者身份、IP、API Key、请求/响应全文、工具参数或代码；不新增正文采样开关。
- 关联 ID 仅用于诊断，不参与鉴权、会话分组、缓存键或重放判定。
- 保留原 `>>>` / `<<<` / `<!>` 行形态；新增字段追加到诊断行尾。

## 一、已核验的 New API rc.41 接口

对应源码：`QuantumNous/new-api` 的 `v1.0.0-rc.41`，固定提交
`2035a82aeb5414253a728bd937d4b8f97aa99b9b`。

1. `middleware/request-id.go` 为 New API 自己生成请求 ID，写入上下文和**响应**，
   没有写入发往渠道的请求头。
2. `common/constants.go` 的 `RequestIdKey` 实际为 `X-Oneapi-Request-Id`，
   不能凭名称猜成 `X-New-Api-Request-Id`。
3. `relay/channel/api_request.go` 的 `doRequest` 读取渠道**响应**中的此头，保存为
   `UpstreamRequestIdKey`；`model/log.go` 的错误/消费日志把它写入独立字段
   `upstream_request_id`，与 New API 的 `request_id` 并列。
4. 渠道 Header Override 只支持 `{api_key}` 和 `{client_header:<name>}`，
   不支持读取内部请求 ID 的 `{request_id}`。不能用复制调用者自填头代替可信关联。

因此本版采用更简单的反向关联：网关生成 UUID，同时回传
`X-Zcode2api-Request-Id` 与兼容别名 `X-Oneapi-Request-Id`。
New API 无需透传最终用户标识，由其日志保存用户/令牌 → New API 请求 → 网关请求的映射。
不增加信任外部头的环境变量；入站同名值一律不能决定网关 ID，也不转发到模型上游。

这属于固定版本源码核验，不等于已经验证用户服务器的代理设置与日志开关。
消费日志关闭、响应头被反代删除、无响应头的网络失败，以及 New API 未落盘的中间重试，
仍可能使关联不完整。不会宣称任意一次客户端操作的全部跨层重试都能自动还原。

## 二、实现步骤

1. 在四个模型 HTTP 入口统一生成请求标识，补 UTC 时间戳、协议、开始/结束事件与 HTTP 状态。
   Messages / Chat Completions / Responses / Async 共用实现，不读正文、不缓冲 SSE。
2. 网关短 ID、完整 UUID、异步 ticket、逐次上游 request ID 建立对应关系；
   上游响应追踪头采用白名单，只输出受限 ID 或摘要，不输出整个 Header。
3. HTTP 错误及异常 JSON 分支增加独立响应体观测事件：HTTP 状态、读取字节数、
   超限标志、读取失败分类、白名单业务码/类型及消息摘要。正文内容不写入新事件。
   同步与异步使用同一摘要函数，读失败与“拿到响应头时无连接错误”明确区分。
4. 记录整形后的结构：上下文保留开关、system 来源/形态/块数、角色数量、工具数量、
   实际出站 MCS 声明；不记录工具名、system 文本或其它用户内容。
5. 将诊断收尾安装在重放检查之前。零出站拒绝同样有完整 UUID、入口内容引用、
   `upstream_attempts=0` 和剩余防护时间；读取剩余时间不得刷新防护窗口。
6. 编写运维说明：通过 New API 的 `upstream_request_id` 查询网关日志，区分
   HTTP 完成、协议完整、重复提交及本地保护阈值，不误判为模型回答正常或正文必然有问题。

## 三、验证与反馈环

本次不尝试用合成测试复现供应商内部规则；该根因没有可控实验条件。
反馈环针对已经确认的**观测缺口**：错误体读断后缺少原因、重放早退缺少汇总、
New API 没有可保存的网关响应 ID。先新增会在旧实现上失败的 HTTP 回归，再实现修复。

- 开发分支：`codex/diagnostics-newapi-rc41`。
- 本地仅代码阅读、编辑、`gofmt` 与差异检查，不运行自动化测试或构建。
- 首次提交只含计划和回归；推送后通过 GitHub Actions 的 `go test ./...` 获得红灯证据。
- 定向反馈命令在 CI Ubuntu 执行：
  `go test ./internal/gateway ./internal/asyncpool -run '^TestDiagnostic' -count=1`。
- 后续覆盖合成 New API 响应头关联、恶意同名头、响应追踪白名单、部分/超限错误体、
  同步/异步、四种入口、SSE flush、重放 TTL、无正文泄漏与现有行为不变。
- 只使用隔离 SQLite、合成凭据、本地 `httptest` 服务。生产 ZIP、账号和 `data/` 不进入 CI。

## 四、发布门禁

1. 等待工作分支当前 SHA 的完整 CI 全绿；合入 `go-rewrite` 后再验同一提交的分支 CI。
2. 更新应用版本及 `docs/releases/v2.9.12-go.md`，版本提交也必须经过完整分支 CI。
3. 仅向 `origin` 推送。分支 CI 通过后才推 `v2.9.12-go` 标签。
4. 等待 `release.yml` 复用的完整验证及产物发布，核对五个二进制附件、双架构镜像、
   `latest` 与版本标签的 digest/revision。推送成功不等于发版完成。

不提交现有未跟踪的分析报告、`.zcode/` 或 `.zcodeignore`。
