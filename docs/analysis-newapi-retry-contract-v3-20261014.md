# v3：New API rc.41 重试建议消费端合同

日期：2026-10-14（Asia/Shanghai）。**提供固定版本的默认关闭补丁和远端合成验收；没有修改、重启或部署生产 New API，也没有重新确认生产当前版本。**

## 版本证据

历史 2026-10-07 的 `/api/status` 核查及后续日志分析记录过 `v1.0.0-rc.41`，仅作为历史部署证据。本轮重新读取官方 GitHub 标签引用，确认 rc.41 当前指向固定提交：

`2035a82aeb5414253a728bd937d4b8f97aa99b9b`

不以当前 main 代替 rc.41，也不以历史版本记录冒充现在运行的容器版本。

一手源码（均固定上述提交）：

- [错误响应解析](https://github.com/QuantumNous/new-api/blob/2035a82aeb5414253a728bd937d4b8f97aa99b9b/service/error.go)：`RelayErrorHandler` 读取正文，没有消费 `Retry-After` 或本网关策略头。
- [唯一重试决策](https://github.com/QuantumNous/new-api/blob/2035a82aeb5414253a728bd937d4b8f97aa99b9b/service/relay_error.go)：`DecideRelayRetry` 查看渠道约束、skipRetry、预算及 HTTP 状态范围，不查看任意 JSON 的重试建议。
- [默认状态范围](https://github.com/QuantumNous/new-api/blob/2035a82aeb5414253a728bd937d4b8f97aa99b9b/setting/operation_setting/status_code_ranges.go)：默认包含 405、429、503；不能靠网关只增加响应头就保证 rc.41 不重投。
- [真实外层重试循环](https://github.com/QuantumNous/new-api/blob/2035a82aeb5414253a728bd937d4b8f97aa99b9b/controller/relay.go)：先规范化错误，再调用上述重试决策。
- [错误结构与 skipRetry 选项](https://github.com/QuantumNous/new-api/blob/2035a82aeb5414253a728bd937d4b8f97aa99b9b/relaykit/types/error.go)：已有 `ErrOptionWithSkipRetry`，可不更改原始 HTTP 状态/业务码地停止本次重试。
- [rc.41 标签引用](https://api.github.com/repos/QuantumNous/new-api/git/ref/tags/v1.0.0-rc.41)。

## 为什么不能只配全局状态码

把 405/503 从全局重试范围删除，会同时改变普通错误的既有恢复；严格渠道亲和/单次渠道 pin 也会停止该范围下其他故障重试。它们都不能精确表达“仅当前网关已经耗尽的风险恢复范围”。本方案不推荐用全局关闭 503、修改错误状态或关闭网关重放闸门替代定点消费。

## 同步契约与补丁

网关同步保留原错误正文和状态，仅在首次风险预算耗尽的 405、或本地 `risk_control_cooldown` 的 503 返回：

```text
X-Zcode2api-Retry-Policy: wait_same_gateway
Retry-After: <真实剩余秒数，向上取整>
```

`scripts/newapi-compat/prepare.py` 只接受上述固定 HEAD；为独立 New API checkout 安装合成测试，并在 `patched` 模式给原 `RelayErrorHandler` 加一个收尾钩子，同时在 `controller/relay.go` 的真实尝试入口传入受信任的私有 context。rc.41 实际协议处理器传的是 `c.Request.Context()`，不能仅在消费者里强转 `gin.Context`，否则只有手写单测通过、生产入口却收不到渠道信息。桥接保留原取消/截止时间。实现源在 `zcode2api_retry.go.txt`，不编译进本项目主程序。

补丁默认关闭。部署方在自己核实版本、渠道编号和真实 BaseURL 后才能配置，例如仅作示例：

```text
NEWAPI_ZCODE2API_RETRY_GATEWAYS={"7":"http://zcode2api:8080"}
```

这里的 `7` 必须是实际可信的 New API 渠道 ID，不能照抄；URL 必须匹配实际发送的 scheme/host/port，可限制额外路径前缀。映射中禁止用户信息、查询及片段。信任取自认证调度上下文的渠道 ID 与 `http.Response.Request.URL`，不取客户端自报头。

只有同时满足以下条件才停止**当前外层请求**的后续投递：

1. 明确允许的渠道和真实目标；
2. 单值、精确策略头，单值、有效正整数 `Retry-After`（最多 7 天）；
3. HTTP 405 且正文 `code`/`error.code` 为 3012，或 HTTP 503 且 `error.type=risk_control_cooldown`。

通过已有 `skipRetry` 标志终止当前外层请求是保守实现：不再试同一网关，也不在这个外层请求中继续切其他渠道。它不是一个跨渠道持久黑名单。其他请求、其他渠道、普通 503/3009、缺头/非法头不改变原有规则。尚未写响应头时把真实剩余等待转给客户端；不缓存或延长保护窗口，不添加其他用户的引用/账号/代理信息。

异步票据已经返回 HTTP 200 时的 `error.details.retry_policy` / `retry_after_seconds` 是票据消费者契约，**不是 rc.41 同步中继补丁已覆盖的功能**。该补丁也不增加会话托管、全局 trace 重写或多副本状态同步。

## 远端验收方法

CI 在独立目录 checkout 真正 rc.41 源码，按其 `go.mod` 安装 Go，再执行：

```bash
python3 gateway/scripts/newapi-compat/prepare.py newapi baseline
(cd newapi && go test ./controller ./relay ./relay/channel ./service -run '^TestZcode2api(Retry|Attribution)Contract' -count=1 -timeout=180s)
python3 gateway/scripts/newapi-compat/prepare.py newapi patched
(cd newapi && ZCODE2API_CONTRACT_PATCHED=1 go test ./controller ./relay ./relay/channel ./service -run '^TestZcode2api(Retry|Attribution)Contract' -count=1 -timeout=180s)
```

这些是 **Ubuntu CI 命令，不在开发机运行**。第一次安装无生产调用点的辅助函数和测试，但不改原错误处理器/controller/重试逻辑，期望仍然重投，以证明兼容缺口；第二次在同一固定源码上接入补丁验证消费。既不复制简化版 `DecideRelayRetry`，也不以某个自写 mock 重试函数替代真实包。另固定 controller 接线数量并编译真实 controller/relay/service 包，避免接口桥接只存在于测试。

覆盖：顶层/嵌套 3012、首次 405、后续 503、递减等待、新外层请求、普通 503、3009、仅 Retry-After、仅正文建议、无效等待、不同渠道与不同实际目标。原版本每个合成外层最多四次投递，补丁版在指定两类风险响应后一次即止；新的外层请求仍进入合成网关重放边界。

边界：消费者测试使用本地 HTTP 的**合成网关合同夹具**。真实 `gateway.Engine` 的“两账号失败、随后零模型出站、不续期”等由本仓库 gateway 回归独立验证；不能把两套测试写成已经完成真实服务器的端到端部署验收。日志与夹具只含合成材料。

## v3 §8.2：归因字段在哪一层丢失，怎样精确透传

rc.41 [公共请求头生成](https://github.com/QuantumNous/new-api/blob/2035a82aeb5414253a728bd937d4b8f97aa99b9b/relay/channel/api_request.go) 默认只复制 Content-Type/Accept；[Claude adapter](https://github.com/QuantumNous/new-api/blob/2035a82aeb5414253a728bd937d4b8f97aa99b9b/relay/channel/claude/adaptor.go) 另外设置渠道认证/Anthropic 版本，[OpenAI adapter](https://github.com/QuantumNous/new-api/blob/2035a82aeb5414253a728bd937d4b8f97aa99b9b/relay/channel/openai/adaptor.go) 另外设置渠道认证。它们**不会默认复制** `X-Session-Id`、`X-Zcode-Trace-Id`、`X-Query-Id`、`X-Zcode-Session-Type`、`X-Zcode-Query-Source`。

因此，在确认为原版默认配置的 rc.41 路径，客户端已经发送这些合法单值头，但 New API 出站没有这些头时，缺失责任层在 New API；只改网关不能补回已丢失的原值。本结论不是“已读取当前生产的渠道覆盖配置”，实际部署可能已有覆盖或运行其他版本。

rc.41 已原生支持 `{client_header:<name>}`，且在 adapter 设置认证后应用 Header Override。合法单值的恢复不需要全量头透传补丁；对明确的 zcode2api 渠道使用附带 `scripts/newapi-compat/attribution-headers.rc41.json` 的五字段配置：

```json
{
  "X-Session-Id": "{client_header:X-Session-Id}",
  "X-Zcode-Trace-Id": "{client_header:X-Zcode-Trace-Id}",
  "X-Query-Id": "{client_header:X-Query-Id}",
  "X-Zcode-Session-Type": "{client_header:X-Zcode-Session-Type}",
  "X-Zcode-Query-Source": "{client_header:X-Zcode-Query-Source}"
}
```

缺值/空白值会跳过，不填 `main_turn`、不生成新 trace；字段名读取大小写不敏感，合法单值原样传递。不同重试仍从同一外层请求读取这些逻辑标识，实际 HTTP request ID 留给网关逐次生成。不要使用 `*` 或宽泛正则，也不要添加客户端 Authorization、Cookie、x-api-key、CallerScope、代理口令或任意内部头。配置只能在管理员已有覆盖中定点合并，不能覆盖其他合法配置。

原版对占位符使用 `Header.Get`，多值会任取首值，逗号合并值/非法归因也不会由它验证。因此，仅配置上面 JSON 只能称合法单值恢复，不能称多值契约完成。固定版本补丁进一步在 `relay/channel/api_request.go` 增加一个五字段占位符钩子（`zcode2api_attribution.go.txt`）：仅来源名属于这五个归因头时，要求合并大小写别名后恰好一个值；三个 ID 遵循本网关 1024 字节可打印 ASCII、无逗号的快照预算，session-type 遵循 `main/subagent/side_chat/other`，query-source 遵循 `^[A-Za-z0-9_.:-]{1,128}$`。这不是强制 UUID 的新增供应商规则。无效/多值仅省略，不惩罚账号、不生成替代值；其他占位符保留原行为。

新增 `zcode2api_attribution_test.go.txt` 在真正的 rc.41 `relay/channel` 测试包运行：实例化原 Claude/OpenAI adapter，走 `DoApiRequest` 到本地 HTTP 服务，分别断言**默认确实缺失**、**五字段配置精确恢复**、**原版多值首值与补丁版拒绝歧义**。覆盖 Messages、Chat、Completions、Responses、Responses→Claude 的渠道 HTTP 目标，两模型、两次尝试、缺值、多值、大小写别名冲突、逗号合并、非法 source/type、过长 ID，以及调用者秘密不透传。另固定其他占位符不改变首值语义。这里验证的是归因头传递，不把预制请求体当作 Responses 正文转换已验收。

此配置及补丁不提供可信多用户 CallerScope，不隔离用户故意复用的 session，不修改任何 session/query 前缀，也不把 query 当作幂等授权。网关仍按自身归因契约校验。管理员若改用 `*` 或任意其他透传规则，不属于这个五字段专项的配置/回归保证。

## 发布/部署完成条件

- 本仓库当前发布 SHA 的完整 CI（包括真实 rc.41 原版与补丁版合同 job）通过；
- 若部署消费端补丁，另核实生产 New API 的版本/构建 SHA、BaseURL、渠道范围和配置，再按消费端自己的发布流程构建；本项目发版不会自动改造现有 New API；
- 新版本若不再是 rc.41，必须重新核对解析/重试/转换调用点，脚本不会静默跨版本打补丁；
- 关闭新增环境配置即停止消费补丁，但网关原有重放保护继续有效。未经上述部署及实际链路检查，只能称“固定版本消费者合成合同已验证”，不能称“线上已遵守建议”。
