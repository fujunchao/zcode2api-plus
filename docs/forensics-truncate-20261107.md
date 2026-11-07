
# 上游断流 / 连接失败 取证报告（2026-11-07）

> 取证对象：zcode2api 容器（镜像 `ghcr.io/fujunchao/zcode2api-plus:v2.13.0-go`，127.0.0.1:3000）
> 取证方式：只读。日志备份与解析脚本只落在服务器 `/tmp`；**未改代码、未重启/删除任何容器、未写入 `/data`**。
> 时区：日志内 `ts=` 为 UTC；北京时间 = UTC+8（下文写作 BJ）。

---

## 0. 结论摘要（先看这里）

1. **今天 4 次断流全部发生在同一跳**：z.ai ↔ zcode2api 之间（`err="unexpected EOF"`，chunked 体被上游掐断），**不是下游 new-api / 客户端取消**。
2. **4 次的首次尝试线路全是 `订阅-1e255992dc288e69`**（`socks5://69.55.49.177:38182`）；`f452ae` 在该线路上先吃 25s 首字节超时被换线，第 2 跳改走 `订阅-94da87515c35892a` 后仍在 520.2s 处被断。**「4 次全在 1e2559」按"首跳"口径成立。**
3. **不是 z.ai 的固定时长墙**：4 次时长 185.4s / 99.3s / 546.0s / 57.8s 无聚集；同一时段 z.ai 经别的线路正常交付 920.3s、394.6s、264.0s、242.1s 长流。
4. **不是下游取消**：new-api 侧 4 个请求全部记 `reason=handler_stop end_error="上游流式响应中断: unexpected EOF"`（其 `client_gone` 是独立原因，今天 6 次，与这 4 次不重合）；zcode2api 侧 `context canceled` 计数 = 0，`write_failed=true` = 0。
5. **1e2559 是"连不通 + 中途中断"双形态坏线路**：09:37 因 2 连 `transport_timeout` 被隔离（`reassigned=4`），10:07 解禁后 25 分钟内又断 3 次，10:32:49 第二次隔离（`upstream_truncate`，`reassigned=4`）。
6. **软隔离在起作用**：每次 `proxy-soft ... action=quarantine` 都带 `reassigned=4/3`，即熔断时一次性改派 4 / 4 / 3 个账号——这是"同一线路绑定多账号"的直接证据。
7. **健康巡检没抓到它**：340 轮 `proxy-health`、96 个 `removed_ids` 里**没有** 1e2559 与 94da8751；它们是被断流熔断隔离的，不是被巡检判死 → 巡检口径（deep 探针）对"能连通但会中途掐断"的线路不敏感。
8. **证据强度**：①同秒双侧对齐（zcode2api + new-api）＞②首跳线路唯一性＞③同线路另一形态故障（25s 首字节超时）＞④跨线长流对照＞⑤时长不聚集＞⑥`proxy-deep` 只能作弱旁证（100% false 的线路很多，其中 2 条今天还在跑 920s / 242s 长流）。

> ⚠️ 覆盖局限（先看 §1）：容器今天 10:36:28 BJ 重建，**昨天的 docker 日志已随旧容器消失**，7 天窗口拿不到。主数据是**今天 UTC 02:36:56–10:40:12（BJ 10:36:56–18:40:12，8h03m）**的完整窗口，外加一份 2026-11-06 的 3 小时遗留样本作对照。

---

## 1. 日志覆盖范围与备份

```bash
# 本次实际执行的备份 + 去色
docker logs zcode2api 2>&1 | sed -r 's/\x1b\[[0-9;]*m//g' > /tmp/zc_all.log        # 2562 行
docker logs -t zcode2api 2>&1 | sed -r 's/\x1b\[[0-9;]*m//g' > /tmp/zc_t.log      # 2566 行（带 docker 时间戳）
docker inspect zcode2api --format '{{.State.StartedAt}}'                            # 2026-11-07T02:36:28Z
```

| 项 | 值 |
|---|---|
| 容器启动（=日志起点） | UTC 2026-11-07T02:36:28Z / BJ 2026-11-07 10:36:28 |
| 日志首行 ts= | UTC 2026-11-07T02:36:56Z / BJ 10:36:56 |
| 日志末行 ts= | UTC 2026-11-07T10:40:12Z / BJ 18:40:12 |
| 覆盖时长 | **8h03m，今天全窗口无缺口** |
| 总行数 | 2562 行（去 ANSI 后；含 `ts=` 的 2486 行） |
| HTTP 入口请求数 | 29（`[ingress] event=start` 29 条 / `event=end` 29 条） |
| 前一个容器 | 已删除，`/var/lib/docker/containers/` 下只剩 `bc19996c…` 一个 zcode2api 目录，旧 json-file 日志不可恢复 |
| `/data` 内日志文件 | 无（容器日志只走 stdout，无落盘、无 logrotate） |

**因此「最近 7 天」无法满足**：能给的是「今天 8h 全量 + 2026-11-06 三小时遗留抽样」。

对照样本（**非本次抓取**，上一会话遗留在 `/tmp/zcode_raw.log`）：

| 项 | 值 |
|---|---|
| 文件 | `/tmp/zcode_raw.log`（4058 行） |
| 覆盖 | UTC 2026-11-06T09:46:53Z–12:45:52Z / BJ 2026-11-06 17:46:53–20:45:52（3h00m） |
| 诊断行 `[#]` | 79 条（78 条 `complete=1`；1 条 `complete=0` 但 `err=-`、`status=429` 属额度，非断流） |
| 断流 | `流传输中断` 0 次、`unexpected EOF` 0 次 |

```bash
grep -c '\[#\]' /tmp/zcode_raw.log                        # 79
grep -c '流传输中断' /tmp/zcode_raw.log                   # 0
grep -o 'ts=2026-[0-9T:-]*Z' /tmp/zcode_raw.log | sort | sed -n '1p;$p'
```

---

## 2. 诊断行 `[#]` 的 err 分布（日期 × 类别）

归一化口径：`unexpected EOF` → **unexpected EOF**（上游掐断 chunked 体）；`context canceled` → **context canceled**（下游取消）；`connection_failed` → **connection_failed**；`timeout / deadline exceeded` → **timeout**；其余 → **其他**。

| 日期 | 诊断行 | unexpected EOF | context canceled | connection_failed | timeout | 其他 |
|---|---|---|---|---|---|---|
| 2026-11-07（8h 全量） | 29 | **4** | 0 | 0 | 0 | 0 |
| 2026-11-06（3h 遗留抽样） | 79 | 0 | 0 | 0 | 0 | 0 |

`[#]` 里只有 `unexpected EOF` 一种非空 err，且与 `complete=0` 一一对应（4/4）；其余 25 条 `err=-`、`complete=1`、`trunc_total=0`。

```bash
grep '\[#\]' /tmp/zc_t.log | grep -oE 'err=("[^"]*"|-)' | sort | uniq -c
#      4 err="unexpected EOF"
#     25 err=-
```

**另一维度：连接失败（`[attempt] event=error` 的 `err_kind`）**——它不进 `[#]` 的 err 字段，但对"连接失败"画像必需：

| 日期 | err_kind | 次数 | 分布（线路） |
|---|---|---|---|
| 2026-11-07 | `connection_failed` | 4 | 订阅-1e255992dc288e69 ×2、订阅-c0273fb37e246ffc ×1、订阅-e163ee192df416cd ×1 |
| 2026-11-07 | `timeout` | 1 | 订阅-94da87515c35892a ×1（elapsed_ms=11364） |

```bash
grep 'event=error' /tmp/zc_t.log | grep -oE 'err_kind=[a-z_]+' | sort | uniq -c
#   4 err_kind=connection_failed
#   1 err_kind=timeout
# 换线重试按线路统计（25s 内未返回首字节 / 连接失败）
grep -oE '线路 [^）]+）' /tmp/zc_t.log | sort | uniq -c
#   2 线路 订阅-1e255992dc288e69）
#   1 线路 订阅-c0273fb37e246ffc）
#   1 线路 订阅-e163ee192df416cd）
#   1 线路 订阅-94da87515c35892a）
```

---

## 3. 两种收尾文案按天计数

| 收尾文案 | 2026-11-07 | 2026-11-06（抽样） | 说明 |
|---|---|---|---|
| `流传输中断` | **4** | 0 | 全部后缀 `: unexpected EOF` |
| `上游在流中返回错误事件` | **0** | 0 | 今天一次都没出现（z.ai 没有在流中回 error 事件） |

```bash
grep -c '流传输中断' /tmp/zc_t.log                    # 4
grep -c '上游在流中返回错误事件' /tmp/zc_t.log         # 0
```

代码口径（`internal/gateway/engine.go:1054-1067`）：两者是同一处收尾的两个标签，区别只在 `IsUpstreamErrorEvent(err)`；并且只有 `!isClientGone(ctx) && !upstreamEvent` 才会计 `trunc_total`（`engine.go:1055-1056`）——**`trunc_total` 被记成 1/2 本身就证明"断流那一刻请求上下文仍存活"，不是下游取消**。

---

## 4. 断流事件明细（今天全部 4 条）

| # | 短ID | UTC | 北京时间 | total | firstbyte | maxgap | bytes | complete | trunc_total | model | acc | route（首次尝试） | endpoint | attempts | binding_gen |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 1 | `d48d55` | 09:27:49.567 | 17:27:49 | 185406ms | 2917ms | 14394ms | 684707B | 0 | 1 | glm-5.3-flash | vtnpdx | 订阅-1e255992dc288e69 | socks5://69.55.49.177:38182 | 1 | 16 |
| 2 | `843568` | 10:12:49.345 | 18:12:49 | 99271ms | 2869ms | 2991ms | 534113B | 0 | 1 | glm-5.3-flash | ezfiew | 订阅-1e255992dc288e69 | socks5://69.55.49.177:38182 | 1 | 41 |
| 3 | `f452ae` | 10:25:06.486 | 18:25:06 | 546039ms | 9434ms | 4855ms | 2083209B | 0 | 1 | glm-5.3-flash | qxoodk | **94da87515c35892a（第 2 跳）**；第 1 跳 tlcmlk@1e2559 25s 首字节超时被换线 | http://61.130.176.21:16299 | 2 | 11 |
| 4 | `fcd8cd` | 10:32:49.959 | 18:32:49 | 57829ms | 2913ms | -（仅 1 块） | 322B | 0 | 2 | glm-5.3-flash | ezfiew | 订阅-1e255992dc288e69 | socks5://69.55.49.177:38182 | 1 | 41 |

四条 `[#]` 的共同特征：`status=200`、`usage=in:0/out:0`（**从未收到上游 usage 收尾**）、`err="unexpected EOF"`、`replay_blocked=0`（已向客户端交付字节，不重放）、`deadline_hit=0`（不是我方超时）、`write_failed=false`。

关键原始行（单行摘录均 ≤300 字符，为排版按字段折行，字段与原文一致）：

```
2026-11-07T09:27:49.567265455Z   [#] d48d55 model=glm-5.3-flash stream=true effort=max maxtok=128000 body=437574B
 attempts=1 acc=vtnpdx route=订阅-1e255992dc288e69 firstbyte=2917ms maxgap=14394ms bytes=684707B
 usage=in:0/out:0/cache_r:0/cache_c:0 complete=0 trunc_total=1 total=185406ms status=200 err="unexpected EOF"

2026-11-07T10:32:49.959941810Z   [#] fcd8cd model=glm-5.3-flash stream=true effort=max maxtok=128000 body=2460586B
 attempts=1 acc=ezfiew route=订阅-1e255992dc288e69 firstbyte=2913ms maxgap=- bytes=322B
 usage=in:0/out:0/cache_r:0/cache_c:0 complete=0 trunc_total=2 total=57829ms status=200 err="unexpected EOF"

2026-11-07T10:32:49.960132582Z   [ingress] fcd8cd event=end req="fcd8cd" protocol=messages
 ts=2026-11-07T10:32:49.959032691Z http_status=200 response_bytes=443 total_ms=57913 canceled=true
 write_failed=false handler_returned=true
```

> 事件数 4 < 40，按任务要求全量列出（无需按天折叠）。

---

## 5. 归因判定：今天为什么被打断

### 5.1 三个假设 × 判据表

| 判据 | 观测值 | (a) 出口线路掐长连接 | (b) z.ai 固定时长墙 | (c) 下游 new-api/客户端取消 |
|---|---|---|---|---|
| 断流时长是否聚集固定值 | 185.4s / 99.3s / 546.0s / 57.8s，四个值无聚集，最小 57.8s | ✅ 符合（线路抖动，不认时长） | ❌ 不符（历史墙 ~300s，今天无一条落在 300s 附近） | ➖ 无关 |
| 是否集中在同一 route | 首跳 4/4 = `订阅-1e255992dc288e69` | ✅ 强符合 | ➖ | ➖ |
| err 文本 | 4/4 `unexpected EOF`；`context canceled` 全天 0 | ✅ | ✅/➖（同属上游侧形态） | ❌ 取消应是 `context canceled` |
| `trunc_total` 是否记账 | 4/4 记 1、1、1、2（代码只在 `!isClientGone(ctx)` 时记） | ✅ 断流瞬间 ctx 存活 | ➖ | ❌ 下游先取消则不会记账 |
| 下游同秒证据 | new-api 4 个 id 全为 `reason=handler_stop` + `上游流式响应中断: unexpected EOF` | ✅ | ✅ | ❌ 其 `client_gone` 另计 6 次，这 4 次不在其中 |
| maxgap / 静默 | 14.4s / 3.0s / 4.9s / ≈49s（fcd8cd 首块后静默至 57.8s 才 EOF） | ✅ 间歇性掐断，无统一 idle 阈值 | ❌ | ➖ |
| 跨线路同期对照 | 同期 `订阅-23bba36df120a11f` 一条 **920.3s** 成功流（`a0ca43`，5.06MB）；`e163ee` 242.1s；`23bba36` 394.6s / 264.0s 成功 | ✅ | ❌ 全局时长墙应普遍杀长流 | ➖ |
| 同线路自证 | 1e2559 当天也成功交付 138.8s / 177.8s（10:11 / 10:15） | ✅ 间歇性掐断，非硬性上限 | ❌ | ➖ |

> 表内「历史墙 ~300s」的判据来源是仓库既有复盘：`docs/analysis-flash-5min-stream-cut-20260921.md`（15 次 `unexpected EOF` 集中在 303–304s）、`docs/analysis-long-stall-20261017.md`（先回 200、给 322B 就断）。今天这 4 次没有一条落在 300s 附近。

### 5.2 判定结论

- **主因 = (a) 出口线路 `订阅-1e255992dc288e69`（socks5://69.55.49.177:38182）在长连接上被中途掐断**：4 次断流首跳全落在它；它同日还有"连不通"形态（25s 无首字节 ×2）。同一天同一线路两类故障，指向落地/中转侧不稳，而非 z.ai 对某模型的统一策略。
- **(b) z.ai 固定时长墙：否证**——时长不聚集、同期别线可跑 920.3s、同线也能跑 177.8s。
- **(c) 下游取消：否证**——new-api 自己记的是 `handler_stop`（它读上游时被断），不是 `client_gone`；zcode2api 侧 0 次 `context canceled`、0 次 `write_failed=true`。
- **保留的不确定性**：断点落在「zcode2api → socks5 出口 → z.ai」这一段，只读日志无法 100% 区分"出口线路掐断"与"z.ai 针对该出口 IP/会话掐断"。要区分需对照组（关掉代理走 `async_force_direct` 直连同账号同模型，或同账号换线复测）。本次只读取证，未做该实验。

**关于 `canceled=true` 的正确读法**（避免误判成 (c)）：`internal/gateway/ingress.go:51-53` 里 `canceled=%t` 实际是 **handler 退出瞬间** 的 `ctx.Err() != nil`。今天 25 条成功请求全部 `canceled=false`、这 4 条断流全部 `canceled=true`，属于"上游先断 → new-api 收到 500 后立刻断开 → 我方 handler 退出时看到 ctx 已结束"的**结果**，不是原因。真正的因/果判据是 `trunc_total` 是否记账 + err 文本。

### 5.3 四次断流是否都在 1e2559？

| 请求 | 第 1 跳线路 | 结果 | 落在 1e2559 |
|---|---|---|---|
| `d48d55` | 订阅-1e255992dc288e69 | 185.4s 断流 | ✅ |
| `843568` | 订阅-1e255992dc288e69 | 99.3s 断流 | ✅ |
| `f452ae` | 订阅-1e255992dc288e69 | 25.0s 首字节超时 → 换线 | ✅（第 1 跳，形态是超时） |
| `f452ae` 第 2 跳 | 订阅-94da87515c35892a | 520.2s 断流 | ❌（换线后仍断） |
| `fcd8cd` | 订阅-1e255992dc288e69 | 57.8s 断流 | ✅ |

结论：**按"首跳"口径 4/4 都先打在 `订阅-1e255992dc288e69`**；但 `f452ae` 的最终断点在换线后的 `94da87515c35892a`，说明"换到 94da8751 就安全"不成立——该线路 10:25:06 也吃了一次 `upstream_truncate` 隔离（§6.2）。

### 5.4 第 4 次 `fcd8cd` 完整时间线（全链路对齐）

| 时刻 UTC | 北京时间 | 主体 | 事件 |
|---|---|---|---|
| 10:31:52.046 | 18:31:52.046 | zcode2api | `[ingress] event=start`，`gateway_request_id=fcd8cd29-b550-4887-bcdc-7b64b8669dff`，`incoming_session_present=false` |
| 10:31:52.274 | 18:31:52.274 | zcode2api | `>>> fcd8cd glm-5.3-flash stream`（请求体 2,460,586B，messages=29：user 15 / assistant 14） |
| 10:31:57.892 | 18:31:57.892 | zcode2api | `[attempt] event=start` attempt=1，acc=`ezfiew-0876ceb2`，route_id=`proxy-1e255992dc288e69`，endpoint=`socks5://69.55.49.177:38182`，`binding_generation=41`，`upstream_request_id=09c33acd-…` |
| 10:32:00.778 | 18:32:00.778 | zcode2api | `[attempt] event=headers` status=200，err_kind=-，**elapsed_ms=2884**（≈ firstbyte 2913ms） |
| 10:32:00 → 10:32:49 | 18:32:00 → 18:32:49 | — | 只下发 322B（new-api 侧 `received=3` 个 SSE 事件），随后 **≈49s 完全静默**（`maxgap=-`，不足 2 个数据块无法计算间隔） |
| 10:32:49.959 | 18:32:49.959 | zcode2api | `<!> fcd8cd 流传输中断: unexpected EOF` + `[#]`：`complete=0 trunc_total=2 total=57829ms bytes=322B usage=in:0/out:0` |
| 10:32:49.960 | 18:32:49.960 | zcode2api | `[ingress] event=end`：`http_status=200 response_bytes=443 total_ms=57913 canceled=true write_failed=false handler_returned=true` |
| 10:32:49.959 | 18:32:49.959 | zcode2api | `[~] proxy-soft id=proxy-1e255992dc288e69 signal=upstream_truncate failures=3 attempts=5 rate=60% consecutive=2 action=quarantine until=2026-11-07T11:02:49Z`（BJ 19:02:49 解禁）`reassigned=4` |
| 10:32:49 | 18:32:49 | new-api | `[ERR] stream ended: reason=handler_stop end_error="上游流式响应中断: unexpected EOF" soft_errors=1, received=3`，紧跟 `channel error (channel #4, status code: 500)` |

要点：

- 首字节正常（2.9s、status 200），**问题只在"流中途"**，与"连不上"是两种形态。
- `trunc_total` 由 `843568` 的 1 涨到 `fcd8cd` 的 2，且两次都是同一账号 `ezfiew-0876ceb2` → 账号级断流计数在同一账号上连续递增（与历史 `trunc_total` 1→2→3 的判定口径一致）。
- "322B 就断"与仓库既有记录同形（`docs/analysis-long-stall-20261017.md`：「先回 200 再给 322B 就断」），但**那次触发点是 ~300s 墙，这次只有 57.8s**——同形态可由不同触发源产生，不能据此归为时长墙。

---

## 6. 线路池现状

### 6.1 巡检与拆除

```bash
grep -c 'proxy-health' /tmp/zc_t.log        # 340
grep -c '线路不可用' /tmp/zc_t.log          # 0
grep -c 'proxy-soft' /tmp/zc_t.log          # 3
```

| 指标 | 值 |
|---|---|
| `proxy-health` 轮次 | 340（`stage=confirmed` 324 / `stage=probe` 16；`whole=1` 的整轮 32 次） |
| 累计 `checked` | 1086 条次 |
| 累计 `candidates` | 329 条次 |
| 累计 `removed` | 96 条次（**96 个互不相同的 id**，每个被移除一次） |
| 处置模式 | `mode=auto_retire` |
| `线路不可用：` 文案 | **0 次**——v2.13.0 已不再输出该文案，等价语义改由 `proxy-health removed_ids`（巡检移除）与 `proxy-soft quarantine`（断流熔断）表达 |

样例：

```
2026-11-07T10:40:39.656461860Z   [~] proxy-health round=network-1794047976779525302 stage=confirmed
 whole=1 checked=11 candidates=0 removed=0 mode=auto_retire ts=2026-11-07T10:40:39Z
```

### 6.2 两条关键线路的健康足迹

| 项 | 订阅-1e255992dc288e69 | 订阅-94da87515c35892a |
|---|---|---|
| endpoint | socks5://69.55.49.177:38182 | http://61.130.176.21:16299 |
| 被 `proxy-health` 移除 | **从未**（96 个 removed_ids 里没有它） | **从未** |
| `proxy-deep` | 26 次 / **26 次 ok=false（100%）**，最大只跑到 364,896B，从未跑满 | 19 次 / **仅 2 次 false（11%）**，能跑满 4,194,304B |
| `proxy-observation` | 15 条 | 27 条 |
| `proxy-soft` 隔离 | 2 次：09:37:05 `signal=transport_timeout`（until 10:07:04Z / BJ 18:07:04，`reassigned=4`）；10:32:49 `signal=upstream_truncate`（until 11:02:49Z / BJ 19:02:49，`reassigned=4`） | 1 次：10:25:06 `signal=upstream_truncate`（until 10:55:06Z / BJ 18:55:06，`reassigned=3`） |
| 今天承载的 `[#]` | 6 次：3 成功（16.9s / 138.8s / 177.8s）、3 断流 | 3 次：2 成功（17.0s / 227.8s）、1 断流（546.0s） |

原始行：

```
2026-11-07T09:37:05.015519091Z   [~] proxy-soft id=proxy-1e255992dc288e69 signal=transport_timeout failures=2
 attempts=2 rate=100% consecutive=2 probation=0 trigger=consecutive action=quarantine until=2026-11-07T10:07:04Z
 strikes=1 reassigned=4 no_alt=0

2026-11-07T10:32:49.959416981Z   [~] proxy-soft id=proxy-1e255992dc288e69 signal=upstream_truncate failures=3
 attempts=5 rate=60% consecutive=2 probation=0 trigger=consecutive action=quarantine until=2026-11-07T11:02:49Z
 strikes=1 reassigned=4 no_alt=0

2026-11-07T10:25:06.486105150Z   [~] proxy-soft id=proxy-94da87515c35892a signal=upstream_truncate failures=2
 attempts=2 rate=100% consecutive=2 probation=0 trigger=consecutive action=quarantine until=2026-11-07T10:55:06Z
 strikes=1 reassigned=3 no_alt=0

2026-11-07T10:39:46.493057678Z   [~] proxy-deep id=proxy-1e255992dc288e69 mode=shadow ok=false
 bytes=333040 ms=1516 kbps=214 steady_kbps=3522 skip_bytes=131072 target=zcode.z.ai:443 early_stop=true
```

### 6.3 `proxy-deep ok=false` 排行（带 endpoint；语义 = 未跑满字节目标，不等于线路死）

| 线路 id | endpoint | deep 次数 | ok=false | 占比 | 备注 |
|---|---|---|---|---|---|
| proxy-29af8ef740e4efbc | socks5://84.200.89.140:1080 | 191 | 191 | 100% | 今天唯一一次承载是成功（7.5s 短请求） |
| proxy-5eb4a5cd7749d550 | socks5://160.22.200.60:69 | 63 | 63 | 100% | 今天唯一一次承载是成功（18.4s） |
| proxy-b5995117d6828a97 | http://138.124.53.81:8080 | 30 | 29 | 97% | |
| **proxy-1e255992dc288e69** | **socks5://69.55.49.177:38182** | 26 | **26** | **100%** | 今天 3 次断流的首跳 |
| proxy-f6cc0e2272c82947 | 今天未被选中，无 endpoint 记录 | 23 | 23 | 100% | |
| proxy-a2000caab6fcc5b8 | 同上 | 21 | 21 | 100% | |
| proxy-46737d341b43ea75 | 同上 | 17 | 17 | 100% | |
| proxy-6b0f584218f72e09 | 同上 | 17 | 17 | 100% | |
| proxy-e163ee192df416cd | socks5://121.46.226.221:10808 | 16 | 16 | 100% | 今天 5 次承载 **5 次成功**（最长 242.1s） |
| proxy-94da87515c35892a | http://61.130.176.21:16299 | 19 | 2 | 11% | 唯一一次断流发生在 546.0s |
| 全池 | — | 872 | 761 | 87.3% | |

**方法学警告**：`proxy-deep ok=false` 不能预测断流——100% false 的线路有 9 条，其中 `e163ee`、`23bba36` 今天分别成功交付 242.1s / 920.3s 长流；而 `94da8751` deep 只有 11% false 却断了 546.0s。deep 只能作弱旁证。

### 6.4 `proxy-observation` 计数

| action | 条数 | 语义 |
|---|---|---|
| `rate_limited` | 559 | 计费请求未完成，限流窗口内不动线路 |
| `recheck_pending` | 264 | 计费请求未完成，待网络复核；文案明确"本次不删除线路、不改派账号" |
| `stale_snapshot` | 3 | 快照过期 |
| 合计 | **826** | 其中提及 1e2559 的 15 条、94da8751 的 27 条 |

```bash
grep 'proxy-observation' /tmp/zc_t.log | grep -oE 'action=[a-z_]+' | sort | uniq -c
#   559 action=rate_limited
#   264 action=recheck_pending
#     3 action=stale_snapshot
grep '改派' /tmp/zc_t.log | grep -vc '不改派'      # 0 —— 826 条里的"改派"全是"不改派账号"
```

> `proxy-observation` 的文案是「**本次不删除线路**…**不改派账号**」，**不是**改派证据；真正的改派证据只有 `proxy-soft … reassigned=N`。

### 6.5 「有多少账号绑在同一条线路」的证据

日志里没有逐条 rebind 明细行，但有两个可交叉验证的硬指标：`proxy-soft` 的 `reassigned=` 与 `[attempt]` 里的 `acc_id` / `binding_generation`。

| 线路 | `reassigned=` | `[attempt]` 里不同 acc_id | 出现过的 binding_generation | 判定 |
|---|---|---|---|---|
| 订阅-1e255992dc288e69 | **4**（09:37 与 10:32 各一次） | **4**：vtnpdx、ezfiew、tlcmlk、qxoodk | 8、16、38、41、49 | 该线路当时绑定 **4 个账号** |
| 订阅-94da87515c35892a | **3**（10:25 一次） | **3**：uamtww、lelbog、qxoodk | 11、24、57 | 该线路当时绑定 **3 个账号** |

```bash
grep 'proxy-soft' /tmp/zc_t.log | grep -oE 'id=proxy-[0-9a-f]+|reassigned=[0-9]+' | paste - -
# id=proxy-1e255992dc288e69	reassigned=4
# id=proxy-94da87515c35892a	reassigned=3
# id=proxy-1e255992dc288e69	reassigned=4
```

推论：1e2559 一坏，**同一时刻 4 个账号会一起被掐断并被集体改派**——这解释了今天 BJ 17:27–18:32 这段为何呈"成串"而非"零散单发"形态。

---

## 7. 证据强度排序

| 强度 | 证据 | 指向 |
|---|---|---|
| ★★★★★ | 4 次断流首跳 100% 落在 `订阅-1e255992dc288e69`（09:27 / 10:12 / 10:16 / 10:32） | (a) 线路 |
| ★★★★★ | new-api 侧同秒 4 个 id 全为 `reason=handler_stop end_error="上游流式响应中断: unexpected EOF"`，其 `client_gone` 另计 6 次且时间不重合 | 排除 (c) |
| ★★★★☆ | `err="unexpected EOF"` ×4、`context canceled` ×0、`trunc_total` 4/4 记账（gate: `!isClientGone(ctx)`）、`write_failed=false` | 排除 (c)，定位上游侧 |
| ★★★★☆ | 1e2559 同日另有 2 连 25s 首字节超时 → `transport_timeout` 隔离 `reassigned=4` | (a) 线路（两类故障同源） |
| ★★★☆☆ | 跨线对照：同期 z.ai 经 `23bba36` 交付 920.3s、`e163ee` 242.1s、`23bba36` 394.6s / 264.0s | 排除 (b) |
| ★★★☆☆ | 时长 185.4 / 99.3 / 546.0 / 57.8s 不聚集；`maxgap` 14.4 / 3.0 / 4.9 / ≈49s 无统一阈值 | 排除 (b) |
| ★★☆☆☆ | 1e2559 的 `proxy-deep` 26/26 从未跑满字节目标（最大 365KB vs 健康线 4MB） | (a) 弱旁证 |
| ★☆☆☆☆ | `canceled=true` 4/4（handler 退出瞬间 ctx 已结束） | 干扰项：是 new-api 收到 500 后断开的**结果** |

---

## 8. 复现命令清单（全部只读）

```bash
# 1) 备份 + 去色
docker logs --since 168h zcode2api 2>&1 | sed -r 's/\x1b\[[0-9;]*m//g' > /tmp/zc_all.log
docker logs -t zcode2api 2>&1 | sed -r 's/\x1b\[[0-9;]*m//g' > /tmp/zc_t.log

# 2) 诊断行 / 收尾文案
grep '\[#\]' /tmp/zc_t.log | grep -v 'err=- ' | cut -c1-300
grep -c '流传输中断' /tmp/zc_t.log ; grep -c '上游在流中返回错误事件' /tmp/zc_t.log

# 3) 单请求全链路
grep 'fcd8cd' /tmp/zc_t.log | cut -c1-760

# 4) 线路池
grep 'proxy-health' /tmp/zc_t.log | grep -oE 'stage=[a-z]+|removed_ids=\S+' | sort | uniq -c | head
grep 'proxy-deep' /tmp/zc_t.log | grep 'ok=false' | grep -oE 'id=[a-z0-9-]+' | sort | uniq -c | sort -rn | head
grep 'proxy-soft' /tmp/zc_t.log | cut -c1-330
grep 'proxy-observation' /tmp/zc_t.log | grep -oE 'action=[a-z_]+' | sort | uniq -c

# 5) 下游对照（new-api）
docker logs --since 150m new-api 2>&1 | sed -r 's/\x1b\[[0-9;]*m//g' | grep -E 'handler_stop|client_gone' | cut -c1-260
```

---

## 9. 本次未做的事 / 局限

1. **7 天窗口不可得**：容器 2026-11-07T02:36:28Z 重建，旧容器与其 json-file 日志已删除；`/data` 内无应用日志文件。要满足 7 天，需从现在起把 stdout 落盘（定时 `docker logs` 归档或换 log driver），属另一件事。
2. 未做**主动对照实验**（`async_force_direct` 直连同账号同模型、或同账号换线复测），因此「出口线路 vs z.ai 针对该出口」最后一步只能给到"高度可能为线路侧"。
3. 未改任何配置、未重启容器、未触碰 `/data`；中间产物只在服务器 `/tmp`（`zc_all.log`、`zc_t.log`、`parse1..4.py`、`na_probe.log`）。

---

## 10. 建议的下一步（供 lead 决策，不含执行）

1. **短期止血**：1e2559 已被两次自动隔离（最近一次 until BJ 19:02:49），隔离期内观察是否还出现断流；若解禁后 30 分钟内再断，考虑人工停用该线路而不是等第 3 次熔断。
2. **口径改进**：健康巡检（`proxy-deep`）对"能连通但会中途掐断"无感，可考虑把 `proxy-soft` 的 `upstream_truncate` 计数纳入巡检输入（当前二者完全独立）。
3. **取证前置**：给 zcode2api 的 stdout 加落盘归档，否则下次跨天复盘仍然只有一个"今天"。
