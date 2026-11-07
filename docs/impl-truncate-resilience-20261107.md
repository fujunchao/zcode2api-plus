# 实施计划：断流韧性改造（P0–P3，v2.13.1-go）

> 状态：**已获批准，按阶段执行**（2026-11-07）。方案依据 `docs/plan-truncate-resilience-20261107.md`，
> 取证依据 `docs/forensics-truncate-20261107.md` 与 `docs/forensics-newapi-side-20261107.md`。
> 决策记录：默认策略 = **retryable**（客户端自动重试）；批准客户端 canary；坏线路**直接删除**；
> 暂不调低单线绑定上限（免费代理供给未知）；P0–P3 全部实施，完成后发 v2.13.1 小版本。

---

## 0. 阶段总览

| 阶段 | 内容 | 产出 | 依赖 |
|---|---|---|---|
| A | P0 线上止血（删坏线、改熔断参数、日志归档） | 服务器侧配置 + 归档任务 | 无（已并行执行） |
| B | P1 断流终态语义（retryable / graceful / abort / legacy） | `internal/gateway/truncate.go`、handler/engine/openai 改造 | 无 |
| C | P2 未出内容时透明换号 + 502 收尾 | handler 预缓冲闸门、engine 换号出路 | B |
| D | P3 graceful 收尾（无未闭合 tool_use 才用） | sseTail 状态机 + 补帧 | B |
| E | 演练通道（canary 用） | `ZCODE_DRILL_KEY` + `serveDrill` | B |
| F | 文档与发布 | README、发布说明、v2.13.1-go | A–E |

阶段 B/C/D/E 都在同一条交付路径上，必须一起进 CI；但它们的行为开关彼此独立（见 §4 回滚）。

---

## A. P0 线上止血（不改代码）

**A1 删除坏线路**
- 目标：`proxy-1e255992dc288e69`（`socks5://69.55.49.177:38182`，今天 4 次断流首跳全在它）。
- 做法：`DELETE /admin/api/proxies/proxy-1e255992dc288e69`（管理密钥 `X-Admin-Key`）。
- 验收：`GET /admin/api/proxies` 不再返回该 id；无账号因删除变成无线绑定；等待一个订阅导入周期（15 分钟）确认没有被重新导入。
- 回滚：`POST /admin/api/proxies` 重新添加同名 url 即可。

**A2 熔断参数**
- 目标：`line_truncate_strikes` 3 → 2、`line_truncate_avoid_seconds` 60 → 600（后台设置，写库生效）。
- 代码默认值同步改为 2 / 600（新部署无需再配）。
- 验收：`GET /admin/api/settings` 返回 2 / 600；下一次断流日志出现 `action=quarantine` 更早（2 连击）。
- 回滚：后台改回 3 / 60。

**A3 日志归档**
- 目标：容器只走 stdout，重建即丢。加 `/etc/cron.d/zcode2api-log-archive`，每天 03:20 归档 25h 日志到 `/var/log/zcode2api/`，7 天前 gzip。
- 验收：手动执行一次，文件非空。
- 回滚：删 cron 文件。

**A4 不在本次范围**：单线绑定上限（`ZCODE_PROXY_MAX_BINDINGS_PER_LINE`）、巡检纳入断流计数（需要单独设计误杀边界）。

---

## B. P1 断流终态语义（核心）

**改哪些文件**
- `internal/config/config.go`：新增 `TruncateTerminal` / `TruncateGracefulNotice` / `TruncatePrebufferBudget` / `TruncatePrebufferBytes` / `DrillKey` 与常量组；`LineTruncateStrikes`/@Avoid` 默认值改 2 / 600。
- `internal/gateway/truncate.go`（新）：`NothingDeliveredError`、`AbortedDeliveryError`、`sseTail`、`terminalErrorFrame`、`writeTruncateTerminal`、`sseFrame`。
- `internal/gateway/handler.go`：`passthroughDeliver` 重写；`handleMessages` 处理 `AbortConnection`；演练入口。
- `internal/gateway/engine.go`：`runResult.AbortConnection`、`attemptResult.streamEarlyTruncate`、`finishDelivery` 识别 abort、`deliverStream` 识别未交付、`RunMessages` 早期断流终态。
- `internal/openai/stream.go`、`responses_stream.go`、`handler.go`、`responses.go`：同一套策略的 OpenAI 形态。

**行为变化（对外）**
| 场景 | v2.13.0 | v2.13.1（retryable 默认） |
|---|---|---|
| 已出内容后断流 | `event: error` + `api_error` ⇒ 客户端不可重试、整轮失败 | `event: error` + `type/code=overloaded_error` ⇒ 客户端**自动重试**（≤11 次，指数退避） |
| 未出内容就断流（单账号/全部尝试） | 200 + `api_error` | **HTTP 502** `upstream_stream_interrupted`（客户端按 5xx 重试） |
| 未出内容就断流（多账号） | 同上 | **换号重打，客户端无感** |
| 上游自己的 error 事件 | 原样透传 | 不变 |

**测试**
- `internal/gateway/truncate_terminal_test.go`：四种策略逐字节断言；tool_use 未闭合降级；预缓冲换号；全部失败 502；演练通道。
- 既有测试调整：`linetruncate_test.go` 的 `cutUp` 在掐断前补一个内容增量（本文件关注线路记账，不关注收尾形态）。

**验收**
- CI（Go 1.25 / Ubuntu）全绿，含 `-race`。
- 字节链路：`curl -N` 经 new-api 打演练帧，响应里能看到 `overloaded_error` 与 `code`（证明 new-api 原样透传）。
- 客户端 canary：ZCode 日志出现 `model_retry_scheduled` / `attempt=2`，最终 `model_request_completed`，UI 无重复文本。

**回滚**
- `ZCODE_TRUNCATE_TERMINAL=legacy`：回到 v2.13.0 的字节形态（api_error，不可重试）。
- `ZCODE_TRUNCATE_TERMINAL=abort`：不写帧直接断连（若确认 new-api 会丢自定义字段时使用）。

---

## C. P2 未出内容时透明换号

**行为**
- SSE 且在「首个内容增量」之前：不提交响应头，最多缓冲 `ZCODE_TRUNCATE_PREBUFFER_MS`（默认 12s）或 `ZCODE_TRUNCATE_PREBUFFER_BYTES`（默认 64 KiB）。
- 提交前失败 ⇒ `NothingDeliveredError` ⇒ 引擎换号重打（客户端一个字节都没收到）。
- 全部尝试都失败 ⇒ 502 `upstream_stream_interrupted`。
- 定时器与读取循环用互斥闸门协调；`claimUndelivered` 原子停表，避免「定时器刚提交、读取循环以为没提交」写两遍响应。

**验收**
- 单测覆盖：换号后客户端只收到第二条流的完整消息；只有一条流时返回 502。
- 线上：早期断流的请求 `[#]` 行出现 `attempts>1` 且客户端侧无错误。

**回滚**：`ZCODE_TRUNCATE_PREBUFFER_MS=0`（立刻提交响应头，退回逐块透传）。

---

## D. P3 graceful 收尾

**行为**：`ZCODE_TRUNCATE_TERMINAL=graceful` 时，若消息已开始、未收到终态、解析无异常、**没有未闭合 tool_use 块**，则补：
```
content_block_stop（关闭当前块）→ 新开一个 text 块写可见提示 → content_block_stop
→ message_delta(stop_reason=end_turn[, usage]) → message_stop
```
否则自动降级为 retryable。

**默认不启用**（默认 retryable）：graceful 会让模型误以为任务已完成，只适合「宁可截断也不要重跑」的场景。

**验收**：单测四种降级分支（工具块未闭合 / 未开始 / 解析失败 / 正常）。

---

## E. 演练通道（canary 用）

- `ZCODE_DRILL_KEY` 非空时，带 `X-Zcode-Drill: <key>`（可选 `X-Zcode-Drill-Mode: ok|truncate`，或请求体 `metadata.zcode_drill` 兜底）的请求由网关直接产出一段可控 SSE，不接触账号池与上游。
- 生产默认关闭（空 key）；只在 canary 期间临时开启。
- 用途：验证 new-api 是否原样透传 `error.code`，以及客户端是否真的自动重试。

---

## F. 发布 v2.13.1-go

1. 分支 `codex/truncate-resilience-20261107` 推送 origin，等分支 CI 全绿；
2. 更新 `AppVersion = "2.13.1-go"`、README、发布说明；
3. 打标签 `v2.13.1-go` 触发 `release.yml`（复用完整 ci.yml）；
4. 发布后核对：Release 附件、`ghcr.io/fujunchao/zcode2api-plus:2.13.1-go` 与 `latest` 镜像；
5. 部署：`/opt/1panel/apps/local/zcode2api` 的 compose 镜像号改为 2.13.1-go 并重启（或沿用现有部署脚本）；
6. 部署后 30 分钟观察：`[#]` 行 `err=` 分布、是否有 502 `upstream_stream_interrupted`、客户端是否出现 retry。

---

## 验收清单（全部须成立）

- [ ] 已出内容后断流：客户端日志不再出现 `turn.failed`（改为自动重试并成功）；
- [ ] 早期断流：客户端完全无感（同 session 只看到一个完整回答）；
- [ ] 全部早期断流失败：HTTP 502，客户端按 5xx 重试；
- [ ] 上游 error 事件（overloaded_error 等）仍原样透传，且不计断流；
- [ ] 405/3012 风控、验证码、额度、计费、断流熔断口径不变；
- [ ] `ZCODE_TRUNCATE_TERMINAL=legacy` 能一键回到 v2.13.0 行为；
- [ ] §A 的线上改动可见（线路已删、设置 2/600、归档文件存在）。
