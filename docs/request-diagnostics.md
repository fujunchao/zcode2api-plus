# New API 分发链路的请求诊断

适用版本：网关 `v2.9.12-go` 起；New API 对接依据为 `v1.0.0-rc.41` 的固定源码。
本功能不改变客户端请求格式、流式交付、上下文处理、账号调度或重试策略。

## 1. 不必把最终用户身份传给网关

网关的四个模型入口都会生成独立 UUID，在响应中返回：

```text
X-Zcode2api-Request-Id: <网关 UUID>
X-Oneapi-Request-Id: <相同 UUID，供 New API 兼容读取>
```

包括 `/v1/messages`、`/v1/chat/completions`、`/v1/responses`、`/async/v1/messages`，
以及这些入口的鉴权失败、参数错误和重放拒绝。ID 在写响应正文前就已设置。
入站同名头不会被信任、回显或用作网关 ID。

New API rc.41 收到渠道响应后，会把 `X-Oneapi-Request-Id` 存入日志字段
`upstream_request_id`。它自己的 `request_id` 不会因此被改写：

```text
New API 的用户 / 令牌 / 渠道记录
  └─ New API request_id
       └─ New API upstream_request_id = 网关 gateway_request_id
            └─ 网关短 ID / async ticket
                 └─ attempt=1,2,... + 每次实际出站的 upstream_request_id
                      └─ 模型响应的 response_request_id / response_log_id 等
```

这里有两个**不同作用域**的 `upstream_request_id`：

- New API 日志中的字段：指向本网关 UUID。
- 网关 `[attempt]` 日志中的字段：发给模型上游的每次 HTTP 尝试 UUID。

网关不记录或推断最终用户、令牌持有人；共享的 IP、网关密钥、会话摘要均不是用户身份。
本版无需新增环境变量或 New API 请求头透传配置。
内部的 `X-Oneapi-Request-Id`、`X-Upstream-Request-Id`、`X-New-Api-Request-Id`，
以及已有过滤范围内的 `X-Zcode2api-*` 不会送给模型上游。

### rc.41 核验依据

固定提交：`2035a82aeb5414253a728bd937d4b8f97aa99b9b`。

- [请求 ID 中间件](https://github.com/QuantumNous/new-api/blob/2035a82aeb5414253a728bd937d4b8f97aa99b9b/middleware/request-id.go)：
  生成 ID 并写**响应**，不是自动透传渠道请求头。
- [头名定义](https://github.com/QuantumNous/new-api/blob/2035a82aeb5414253a728bd937d4b8f97aa99b9b/common/constants.go#L187-L190)：
  实际名称为 `X-Oneapi-Request-Id`。
- [读取渠道响应](https://github.com/QuantumNous/new-api/blob/2035a82aeb5414253a728bd937d4b8f97aa99b9b/relay/channel/api_request.go#L582-L584)：
  写入独立的上游 ID 上下文。
- [错误与消费日志](https://github.com/QuantumNous/new-api/blob/2035a82aeb5414253a728bd937d4b8f97aa99b9b/model/log.go#L277-L383)：
  分别保存 `request_id` 和 `upstream_request_id`。

该版本 Header Override 的 `{client_header:...}` 读取的是客户端请求头，
不是服务端内部请求 ID；`{request_id}` 不属于其已实现的模板变量。
不要用用户自填 ID 或固定字符串冒充 New API 的内部请求关联。

## 2. 部署后怎样查一条失败请求

1. 从 New API 的错误/消费日志找到相应 `request_id`，读取同条记录的
   `upstream_request_id`。界面未显示时查看日志详情或日志 API/导出中的字段。
2. 在网关日志中搜索这个完整 UUID，即 `gateway_request_id`：

   ```sh
   docker logs --timestamps --since 30m zcode2api > gateway.log 2>&1
   grep -F '替换为完整网关UUID' gateway.log
   ```

3. 先看 `[ingress] event=end` 的 `http_status`，再看 `[attempt]` 的各次状态与
   `event=response_body`，最后看 `[#]` 的协议完整性、入口内容引用及重放标志。
4. 同一内容反复提交时，对照 `input_ref` 和 `replay_blocked`；这只能说明入口内容的
   短摘要相同，不能据此认定是同一个用户或同一层重试。

部署首验可使用一次正常业务调用，不需要重放之前被拒绝的请求。
确认 New API 日志的 `upstream_request_id` 与网关日志 UUID 一致即可。
若为空，检查反代是否隐藏了 `X-Oneapi-Request-Id`、是否请求到了新镜像、
是否走了该渠道，以及相应日志是否启用；不要先开启全量请求头透传。

源码兼容不等于部署验收：本次自动化回归在本地测试服务中模拟 rc.41 的读取规则，
没有操作用户的生产 New API。消费日志关闭、没有收到渠道响应头的连接失败、
New API 在到达网关前的拒绝，以及未落盘的中间渠道重试，都可能缺少关联。
rc.41 的上游 ID 上下文会随后续渠道响应更新，不能保证单条最终日志保留所有中间尝试。

## 3. 日志字段

原 `>>>` / `<<<` / `<!>` 行保持原形态。
新增 `[ingress]`，并在原 `[attempt]` / `[#]` 行尾补充字段；这些诊断事件带 UTC
`ts`（RFC3339，含小数秒）。`docker logs --timestamps` 可额外为旧日志行添加容器采集时间。

| 字段 / 事件 | 含义 |
| --- | --- |
| `[ingress] event=start/end` | 一次进入模型 HTTP 路由的调用，包含本地校验失败 |
| `gateway_request_id` | 网关生成的完整 UUID；跨系统关联用它，而非六位短 ID |
| `protocol` | 本网关收到的协议，不代表经过 New API 转换前的协议 |
| `http_status` / `response_bytes` | 写给直接调用方的 HTTP 状态 / 实际写出正文总字节数，不是业务成功判据 |
| `canceled` / `write_failed` | 入站请求退出时的取消 / 写响应失败状态 |
| `attempt` / `selection` | 实际模型 HTTP 尝试序号 / 选号次数，原地重试只增加前者 |
| `upstream_request_id` | 本次发给模型上游的请求 ID，不与网关 UUID 复用 |
| `response_*_id` / `response_*_ref` | 模型响应追踪头的受限原始 ID / 短摘要，与出站请求 ID 分开 |
| `event=response_body` | 错误体或异常 JSON 读完/读断后的独立事件 |
| `read_err_kind` | `unexpected_eof`、`timeout`、`canceled`、`connection_reset`、`connection_closed`、`read_failed` 等；`-` 表示本次读取未报错 |
| `body_read_bytes` / `declared_bytes` | 实际读到的字节数 / Content-Length，后者 `-1` 表示未知 |
| `body_truncated` | 本次读取因上限截断；错误路径多读一个字节确认是否超过 64 KiB |
| `error_json` / `error_meta` | 是否解析到完整错误 JSON 对象 / 白名单业务码、错误类型、嵌套代码、可重试标志与消息摘要 |
| `response_body_ref` | 本次已读取字节的 SHA-256 前 12 位；超限/读断时不代表完整响应 |
| `input_ref` | 已归一化入口 body 的 SHA-256 前 12 位，取自原重放键 |
| `bodyhash` | 整形后、逐账号设备/会话注入前的内容视图摘要，语义与 `input_ref` 不同 |
| `upstream_attempts` | 当前入站真实模型出站次数；重放拒绝时为 0 |
| `replay_blocked` / `replay_wait_ms` | 是否本地重放拒绝 / 检查时剩余窗口毫秒数；查询不会续期 |
| `preserve_context` | 本次上下文保留开关 |
| `shape_stage` | `normalized_input` 为尚未按账号整形，`prepared` 为最后一次准备的出站结构 |
| `system_source` | `input`、`input_messages`、`gateway_fallback`、`legacy_merge`、`none`；未准备时 `unprepared` |
| `system_form` / `system_blocks` | 顶层指令形态 / 块数；空字符串算一个字符串单元，空数组为 0 |
| `messages` / `roles_*` | 消息总数 / 固定角色计数，未知角色只计入 other，不输出原值 |
| `tools` / `tool_use_blocks` / `tool_result_blocks` | 工具定义数 / 调用块数 / 结果块数，不记录名字或内容 |
| `mcs_declared` | 出站事件中表示实际 Anthropic-Beta 是否含 MCS 能力；零出站时没有头部观测 |

完整 `[#]` 汇总从引擎开始执行后生成，包括重放早退；鉴权/参数校验在引擎之前结束时，
只有 `[ingress]` 开始/结束事件，没有虚构的模型调用记录。

上游响应头白名单为 `X-Request-Id`、`Request-Id`、`X-Log-Id`、`X-Zhipu-Request-Id`、
`Traceparent`。只原样保留 UUID、受限长度十六进制 ID 和 W3C traceparent；其它值只记短摘要。
`id=-` 且 `ref` 非空表示存在不符合公开输出形态的值，不是丢失了请求。
未知错误代码/类型也只记摘要；自由文本消息只记录字节数与摘要。
因此这些字段不能还原正文，也不保证能展示供应商尚未公开的每一项诊断信息。

## 4. 结论边界

- `riskscope>=2` 是本网关的跨账号保护阈值，不是供应商给出的“提示词有问题”结论。
- `[attempt] event=headers status=200` 只说明收到了响应头；
  `[#] complete=1` 表示协议完整，仍不等于模型在语义上正常推进任务。
- `event=headers err_kind=-` 不覆盖之后的响应体读取；需检查 `read_err_kind`。
- 相同 body 大小、输出 token 数、甚至短摘要，都不是最终用户身份标识。
- 自动重试可能发生于客户端、New API 渠道或本网关；没有跨层对应记录时不把所有重放归咎于某个客户端。

新的诊断事件不输出请求正文、响应全文、工具参数、凭据或任意响应头；
既有错误分支的受限预览仍保留，本版没有扩大为全文抓包，也没有修改原有日志导出内容。
无需数据库迁移；回退镜像后只是缺少这些诊断字段，现有业务配置不受影响。
