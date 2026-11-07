# 方案：让「上游流式响应中断」不再打断正在进行的工作（2026-11-07）

> 状态：**已批准并实施（v2.13.1-go）**；分阶段实施记录见 `docs/impl-truncate-resilience-20261107.md`，上线记录见 `docs/releases/v2.13.1-go.md`。配套取证：
> - 上游侧画像：`docs/forensics-truncate-20261107.md`（zcode2api 容器日志，只读）
> - 下游侧画像：`docs/forensics-newapi-side-20261107.md`（new-api 日志 + 数据库 + 源码，只读）
> - 客户端侧：ZCode Desktop 自带的模型适配层源码 `D:\Programs\ZCode\resources\glm\zcode.cjs`（静态分析）+ 客户端本地日志与 model-io 记录

---

## 0. 一句话结论

**断流本身不能根治（掐点在出口线路/上游）；但「被打断」这件事可以根治**——ZCode 本来就有 11 次自动重试预算（`maxAttempts=11`），只是 zcode2api 在断流时补的那条 SSE 错误帧把它翻译成了「不可重试的业务错误」（`reason=unknown` / `retryable=false`），客户端于是当场判死这一轮。

改一处收尾语义（把「不可重试」改成「可重试」），再加一层「未出字节就换号」，就能从「客户端停摆」变成「客户端自动重试 / 用户无感」。

---

## 1. 这条链路上到底发生了什么（证据链）

### 1.1 调用链

```
ZCode Desktop (providerId=new-provider, providerKind=anthropic, POST /v1/messages)
   → new-api（channel #4，glm-5.3-flash，stream，HTTP 200）
   → zcode2api（/v1/messages，40 账号池 + 出口线路池）
   → socks5/http 出口线路（订阅-1e2559… 等 11 条）
   → zcode.z.ai
```

### 1.2 今天 4 次断流（北京时间，全部 unexpected EOF）

| 短ID | 开始 | 结束 | 时长 | 首跳线路 | 首字节 | 断点前已下发 | 账号 |
|---|---|---|---|---|---|---|---|
| d48d55 | 17:24:44 | 17:27:49 | 185.4 s | 订阅-1e255992dc288e69 | 2.9 s | 684,707 B | vtnpdx |
| 843568 | 18:11:10 | 18:12:49 | 99.3 s | 订阅-1e255992dc288e69 | 2.9 s | 534,113 B | ezfiew |
| f452ae | 18:16:00 | 18:25:06 | 546.0 s | 1e2559（25 s 首字节超时）→ 换线 94da8751 | — | — | tlcmlk→qxoodk |
| fcd8cd | 18:31:52 | 18:32:49 | 57.8 s | 订阅-1e255992dc288e69 | 2.9 s | **322 B**（首块后静默约 49 s） | ezfiew |

- 首跳 4/4 落在 `订阅-1e255992dc288e69`（`socks5://69.55.49.177:38182`），该线路当时绑定 **4 个账号**（`proxy-soft … reassigned=4` 与 `[attempt]` 的 acc_id/binding_generation 交叉吻合）。
- 时长 185/99/546/58 s **不聚集**；同期别的线路成功跑完 **920.3 s / 394.6 s / 264.0 s / 242.1 s** 长流 ⇒ **不是 z.ai 的固定时长墙**（对照 2026-09-21 那次真·300 s 墙：15 次全部落在 301–308 s）。
- zcode2api 侧 `context canceled` = 0、`write_failed` = 0，`trunc_total` 4/4 记账 ⇒ **不是下游取消**（下游取消会被 isClientGone 跳过记账）。
- new-api 侧同 4 个请求全部 `reason=handler_stop end_error="上游流式响应中断: unexpected EOF"`，其 client_gone 另计 16 次且时间不重合。
- 结论：断点落在「zcode2api → 出口线路 → z.ai」这一段（**高度可能是线路侧**；只读日志无法 100% 排除「z.ai 针对该出口 IP 掐断」）。

### 1.3 客户端为什么当场判死（决定性证据）

ZCode CLI 本地日志 `~/.zcode/cli/log/zcode-2026-11-07.jsonl` 今天恰好 4 条 `turn.failed`，字段逐字如下（18:32:50 那条的 `request=c9d8f5d0-…` 与你贴的报错同一个）：

```json
{"event":"turn.failed","level":"error","message":"Turn failed",
 "error":{"message":"Turn execution failed","code":"UNKNOWN_ERROR",
  "cause":{"name":"AiSdkModelAdapterError",
   "message":"上游流式响应中断: unexpected EOF","code":"model_request_failed",
   "context":{"attempt":1,"maxAttempts":11,"errorPhase":"stream",
     "exceptionKind":"provider_business","providerId":"new-provider",
     "providerKind":"anthropic","transport":"sse","responseStatus":200,
     "reason":"unknown","retryable":false,
     "responseBodySummary":{"error":{"type":"api_error","message":"上游流式响应中断: unexpected EOF"}}}}}}
```

注意三个数字：**attempt: 1 / maxAttempts: 11 / retryable: false**。客户端不是没有重试预算，是**认定这次失败不值得重试**。

原因在 zcode.cjs 里可以逐行对上：

| 环节 | 函数/常量 | 行为 |
|---|---|---|
| 从错误帧取 provider 错误码 | NHs()：`o.providerCode ?? error.providerCode ?? o.error_code ?? error.error_code ?? o.code ?? error.code` | 我们只给了 `{"type":"api_error","message":…}` ⇒ **providerCode = undefined** |
| 错误码→语义映射表 | I4e / Fft() | overloaded_error、engine_overloaded_error ⇒ retryable=true；"500" ⇒ retryable=true；rate_limit_error ⇒ retryable=true |
| 无映射时的兜底 | nlo() / sXs() | 只有 api_error + 三条魔法消息（internal network error 等）才算可重试 ⇒ 我们是中文文案，**不命中** |
| 归一化成失败描述 | MN() → ebn() | 落最后一行：reason=Unknown，retryable=false |
| 止损判定 | Pta() → xmt() → llo() = retryable && reason!=='cancelled' | false ⇒ **不重试，turn.failed** |
| 若 retryable=true 会怎样 | ift(retryBudget, attempt, maxAttempts) | 1 < 11 ⇒ 允许重试，按指数退避再打第 2 次 |

**所以根因不是「线路断了」，而是「网关把一次可重试的传输中断，写成了不可重试的业务错误」。**

### 1.4 错误帧是谁写的

`internal/gateway/handler.go:108-124`（passthroughDeliver）：

```go
if err != nil {
    if strings.Contains(d.ContentType, "text/event-stream") && !IsUpstreamErrorEvent(err) {
        data, _ := marshalJSON(map[string]any{
            "type":  "error",
            "error": map[string]any{"type": "api_error", "message": "上游流式响应中断: " + err.Error()},
        })
        fmt.Fprintf(w, "\n\nevent: error\ndata: %s\n\n", data)
    }
    return err
}
```

它本身是对的（HTTP 200 已发出、字节已写出，Anthropic 协议里没有「回滚重来」，必须显式告诉客户端这轮没跑完）；**问题只在 type 与缺少 code**。

### 1.5 new-api 是无辜的，而且救不了

- 运行镜像里 grep `上游流式响应中断` **命中 0**——这句话是我们的。
- 它把 SSE `data:` 行**原样透传**（`relay/helper/stream_scanner.go` 只切前缀，不重新序列化 JSON）。
- 记账正确：claude_error / api_error / error_source=upstream、soft_errors=1、quota=0、health=unchanged（**没封渠道**）。
- `RetryTimes=0`；abilities 表里 glm-5.3-flash **只有 channel #4 一个渠道**（没有可换的）。
- `STREAMING_TIMEOUT=300` 是**空闲**超时（逐行 reset），546 s 的流没被它砍；`RELAY_TIMEOUT=0`；`auto_ban=0`。
- **调大 RetryTimes 只会把第二段流追加进同一个 SSE 响应**（本地 relay 副本里没有「已写出就不重试」的守卫）⇒ 不但救不了，还会污染输出。
- 附带发现（建议单独立项）：**new-api 不校验 message_stop**——channel #4 的 101/101 成功流全是 end_reason=eof。今天能正确判失败全靠我们主动补的错误帧；若哪天改成「静默断连」，new-api 会记成成功并照常计费。

---

## 2. 「能否解决」的两个层次

| 层次 | 能否根治 | 说明 |
|---|---|---|
| **断流事件本身** | **不能 100%** | 掐点在上游/出口线路，我们只能在「频率」和「止血速度」上做功（见 §3 P0）。任何网关都无法阻止 z.ai 或线路中途关连接。 |
| **「一断流就让本地客户端停摆」** | **能** | 客户端有 11 次重试预算，还有「空响应重试」「流恢复」等机制；我们只要把收尾语义改成客户端认得的形态即可。三条可选路线见 §3 P1。 |

方案空间里被排除的几条：
- **让客户端别把错误当致命**：要改 ZCode 客户端（闭源、会自更新）→ 不作为主方案。
- **靠 new-api 重试**：见 §1.5，无效且有害。
- **砍上下文/减包**：能降低单次耗时与上传压力，但今天 4 次里有 57.8 s 就被掐的，减包救不了。

---

## 3. 方案（按可执行顺序，P0 → P3）

### P0 立刻可做（不改代码，今天就能执行）

1. **处置坏线路**：`订阅-1e255992dc288e69`（`socks5://69.55.49.177:38182`）今天已两次熔断（transport_timeout 09:37、upstream_truncate 10:32，各 reassigned=4），且 proxy-deep 26/26 从未跑满字节目标。建议**直接停用**，不要等它第三次解禁后复发。
2. **限制「一条线挂多个账号」**：现在 `ZCODE_PROXY_MAX_BINDINGS_PER_LINE=8`，实际一条坏线一次带走 4 个账号 → 断流成串。建议临时降到 **2~3**。
3. **让断流熔断更早生效**：`ZCODE_LINE_TRUNCATE_STRIKES` 3 → **2**；`ZCODE_LINE_TRUNCATE_AVOID_SECONDS` 60 → **600**。
4. **巡检口径合并**：proxy-health 的 deep 探针抓不到「能连通但会中途掐断」的线路（1e2559 从未被巡检移除）。把 proxy-soft 的 upstream_truncate 计数纳入巡检输入，或至少在后台把「断流隔离次数」作为一列展示。
5. **日志落盘**：容器只走 stdout，今天 10:36 重建后昨天日志全丢。加 docker logs 定时归档（或换 log driver），否则跨天复盘永远只能看「今天」。

> P0 只能降低频率，不能消除「被打断」；真正解决靠 P1。

### P1 核心：断流终态语义可配置（代码量小，收益最大）

**改动点**：`internal/gateway/handler.go` 的 passthroughDeliver（原生 SSE 透传），同步覆盖：
- `internal/asyncpool/pool.go:573`（异步池同类收尾）
- `internal/openai/stream.go:57`、`internal/openai/responses_stream.go:52`（OpenAI 兼容层 fail 路径，供 Codex 等客户端）

**新增配置**（internal/config/config.go）：

```go
// TruncateTerminal 上游中途断流时给客户端的终态形态。
//   retryable = SSE error 帧，error.type/code = overloaded_error（默认，客户端会自动重试）
//   graceful  = 补 content_block_stop + message_delta + message_stop，本轮正常结束（内容截断）
//   abort     = 直接断连（不写错误帧），客户端按网络错误重试
//   legacy    = 现状（api_error，不可重试），用于回滚
TruncateTerminal = envString("ZCODE_TRUNCATE_TERMINAL", "retryable")
```

**retryable 形态的载荷**（本次方案的关键一行）：

```
event: error
data: {"type":"error","error":{"type":"overloaded_error","code":"overloaded_error","message":"上游流式响应中断（已转交客户端重试）: unexpected EOF"}}
```

为什么是这两个字段：
- error.type = overloaded_error 是 Anthropic 协议里语义最贴切的可重试类型（529）；
- error.code 是 ZCode NHs() 实际读取 providerCode 的位置之一，命中 I4e 映射表 ⇒ reason=ProviderOverloaded、retryable=true（见 §1.3）；
- 两者都给，任一条被上游/new-api 保留都能生效。

**预期效果**：断流后 ZCode 自动重试第 2 次（退避 0.5 s 起、上限 11 次），UI 显示「重试中」，这一轮不判死；重试请求会因 TruncateAvoidUntil + 线路熔断落到别的账号/线路上，大概率一次就过。

**风险与兜底**：
- 风险1：new-api 若在 claude→claude 转发时重新序列化 JSON，可能丢掉 code 字段。验证手段见 §4.2。若确认丢字段 → 切 abort（不依赖任何自定义字段，客户端按网络错误重试），或改用只靠 type 生效的写法。
- 风险2：客户端重试可能沿用同一个 assistant 消息 → UI 可能看到重复片段。canary 必须检查；若重复，改 graceful。
- 风险3：「可重试」会让客户端多打几次、多耗上游额度；有 11 次上界与递增退避，可接受。

### P2 体验加成：断流发生在「还没出内容」时，网关自己换号重试（对客户端完全透明）

现状：passthroughDeliver 一进来就 WriteHeader(200)，所以哪怕上游一个内容字节都没给，客户端也已吃到 200，只能靠错误帧收场。

改造：
1. passthroughDeliver **延迟写响应头**，先缓冲到「第一个 content_block_delta」或达到 N KB / M ms 阈值再一次性 flush；
2. 若这之前上游就断了（今天的 fcd8cd 就是这种：200 + 322 B 就静默）：
   - 交给引擎既有换号状态机（`attemptResult{switchAccount:true, avoid: 该线路}`）**换号重打**，客户端无感；
   - 换不到号时返回真正的 **HTTP 502 JSON**（而不是 200 + 错误帧）→ 客户端 MN() 里 statusCode>=500 ⇒ retryable=true，同样自动重试。
3. 需要小心：expect: 100-continue、SSE 首包延迟（TTFT 观测口径）、firstbyte 看门狗三处要对齐。

投入约 1~1.5 天（含测试）；收益：把「早期断流」变成零感知。

### P3 保底：优雅收尾策略（graceful）

当断流发生在「已有文本、且没有未闭合 tool_use 块」时，可以选择补齐协议让这轮正常结束：

```
（关闭当前打开的 content_block）
event: content_block_stop   data: {"type":"content_block_stop","index":N}
event: message_delta        data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{...}}
event: message_stop         data: {"type":"message_stop"}
```

- 好处：客户端完全不报错，会话继续，用户只是看到「回答断了」；
- 坏处：模型可能误以为任务已完成；**若截断发生在 tool_use 的 JSON 中间，绝对不能 graceful**（会产生非法工具调用），必须退回 retryable；
- 因此 graceful 只作可选项，默认仍用 retryable。

### P4 以后再说：网关内「续写」（continuation）

断流后把已生成文本作为 assistant 前缀再请求一次上游，把后续增量拼进同一条 SSE（零感知、不重跑）。技术上可行（Anthropic 支持 assistant prefill；关闭当前块、开新块即可），但：
- 请求体要改写成 prefill 形态，**偏离官方客户端字节形态**（本项目历来对 3012/风控极谨慎）；
- thinking 块不可续、tool_use JSON 不可拼、可能重复计费；
- 需要独立 canary 与回滚开关。
→ **本轮不做**，等 P1/P2 上线后用真实样本评估投入产出。

---

## 4. 验收与验证手段（不靠猜）

1. **单元测试**（新增 `internal/gateway/truncate_terminal_test.go`）：用 httptest 假上游在第 N 个 chunk 后直接 close，断言四种策略输出的**逐字节**形态；同步断言 web.ReqErr 文案不变（现有 linetruncate_test.go / diagnostics_test.go 依赖它）。
2. **字节链路验证**（关键，能证伪「new-api 丢字段」）：加一个 env 开关 `ZCODE_DRILL=1` 时才启用的演练分支——请求头 `X-Zcode-Drill: truncate` 让网关吐 3 个假 content_block_delta 后按所选策略收尾。然后：

```bash
curl -N -sS http://127.0.0.1:3001/v1/messages -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer <new-api key>' -H 'X-Zcode-Drill: truncate' \
  -d '{"model":"glm-5.3-flash","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}'
```

   看 new-api 出来的字节里 overloaded_error 与 code 是否原样保留（这一步决定 P1 用 retryable 还是 abort）。
3. **客户端实测（canary，需你授权）**：临时给 ZCode 加一个指向 new-api 的测试 provider（打完即删），命中演练帧，观察：日志出现 model_retry_scheduled / attempt=2、最终 model_request_completed；UI 没有重复文本。
4. **线上观测**：断流后 ≤10 s 内同 session 应出现一个新请求（= 客户端确实重试了）；`[#]` 诊断行新增 `terminal=retryable` 标记；客户端日志不再出 turn.failed。
5. **回归**：405/3012 风控、验证码、额度、计费、trunc_total/线路熔断口径全部不变；legacy 开关能一键回到今天的行为。

---

## 5. 影响面与取舍

| 维度 | P1（retryable） | P2（早期断流透明重试） | P3（graceful） |
|---|---|---|---|
| 用户是否被打断 | 否（自动重试，可能慢几秒） | 否（无感） | 否（但回答被截断） |
| 上游额度消耗 | 增加（重跑） | 不增加 | 不增加 |
| 上下文重传 | 是（2~2.5 MB/次） | 是 | 否 |
| 破坏协议风险 | 低 | 低 | 中（tool_use 场景禁入） |
| 代码量 | 约 150 行 + 测试 | 约 250 行 + 测试 | 约 200 行 + 测试 |
| 依赖 new-api 透传 | 是（需 §4.2 验证） | 否 | 否 |

---

## 6. 需要你拍板的四件事

1. **P1 默认策略**：retryable（推荐，自动重试）还是 graceful（本轮正常结束、内容截断）？建议默认 retryable，graceful 只作 P3 的可选开关。
2. **是否批准 canary**：允许我临时在 ZCode 里加一个测试 provider（验证完立即删除）？若不允许，退化为「只用 curl 验证字节 + 生产观察」。
3. **P0 的线路处置**：订阅-1e255992dc288e69 是否直接停用？同线绑定上限是否降到 2~3？
4. **是否一起做 P2**（早期断流透明重试）——与 P1 共享同一处代码，一起做能省一轮发布，但要多 1 天。

---

## 7. 明确不做

- **不改客户端**（闭源、自更新，改了就丢）。
- **不在 new-api 上调 RetryTimes**（会把第二段流追加进同一响应）。
- **不做「静默断连不报错」**（new-api 会把 eof 当成功并计费，用户也看不到任何提示）。
- **不在本轮做续写（P4）与 gzip 压缩请求体**。
