# new-api 侧取证：`上游流式响应中断: unexpected EOF` 的产生与处置

- 取证时间：2026-11-07 18:40–19:05 BJT（10:40–11:05 UTC）
- 取证对象：`new-api`（calciumion/new-api:latest，127.0.0.1:3001→3000，Up 2 days）、`zcode2api`（v2.13.0-go，127.0.0.1:3000）
- 方式：只读取证。宿主机 `/tmp/na24.log`、`/tmp/z24.log` 为 `docker logs --since 24h` 快照；DB 用 `cp` 到 `/tmp/nadb/` 后以 Python sqlite3 只读查询（未修改 `/data`、未重启任何容器）
- 时区约定：new-api 日志 ts 为**北京时间（记作 BJT，UTC+8）**；zcode2api 的 `ts=` 为 **UTC**

---

## 0. 结论先行（TL;DR）

1. **错误文本不是 new-api 产生的。** new-api 运行镜像里检索 `上游流式响应中断` 命中 **0 次**；该中文串由 **zcode2api** 在 `internal/gateway/handler.go:113-117` 拼装，以 **SSE `event: error` 帧**的形式写进已经返回 200 的响应体。new-api 只是**收到并记录**为 `error_type=claude_error / error_code=api_error / error_source=upstream`。
2. **new-api 是「无辜的转发者 + 合格的记账员」，不是放大器。** 它做了 4 件正确的事：识别上游 error 事件（`soft_errors=1`）、判为异常结束（`reason=handler_stop`）、按 error log 落库且 `quota=0`（不计费）、不改渠道健康度（`health=unchanged`）。它没有替上游改错、没有编造新的错误、没有把错误算成成功。
3. **new-api 完全没有重试。** 24h 内 `重试` 日志行 **0 条**；根因是配置 **`RetryTimes=0`**。即使不为 0，**glm-5.3-flash 也只挂在 channel #4 一条渠道上**（`abilities` 表仅 1 行），物理上没有可换的渠道。
4. **调大 `RetryTimes` 不是解药，反而有污染风险。** 本地可得的 relay 重试循环（`temp/v3-implementation/newapi-relay.go:158-214`）里**没有**「已向客户端写出字节就不再重试」的守卫；断流发生在写出 5k–15k 个 SSE 事件之后，此时再重试只会把第二段流追加进同一个已写出的 SSE 响应。200 + 已写字节在 Anthropic 协议下不可回滚，**new-api 侧不存在「断流无感切换」的能力**。
5. **真正值得关注的隐患是「反向」的**：channel #4 的 101 次成功流**全部**以 `end_reason=eof` 结束，说明 new-api 对 Anthropic 格式流**没有**「是否见到 `message_stop`」的校验——它判断截断**完全依赖上游主动补写的 `event: error` 帧**。若 zcode2api 哪天静默断连（不补帧），new-api 会把这 4 次全记成 `reason=eof` 成功并照常计费。
6. **唯一能「让客户端不看到这个错误」的修复点在 zcode2api 一侧**（它是唯一知道自己断了、且还能决定怎么收尾的那一层）。new-api 侧的可行动作只有「加监控告警/降级开关」类，不能消除客户端可见的中断。

---

## 1. 24h 日志分布统计（命令 + 数字）

快照：`docker logs --since 24h new-api > /tmp/na24.log 2>&1` → **14,498 行**（覆盖 2026-11-06 18:40:47 ~ 2026-11-07 18:40:16 BJT）。
> 注：`docker logs ... 2>&1 > file` 这种顺序会把 stderr 漏到终端、文件为空，必须写成 `> file 2>&1`。

### 1.1 `stream ended: reason=...` 分布

```bash
grep -o 'stream ended: reason=[a-zA-Z_]*' /tmp/na24.log | sort | uniq -c | sort -rn
```

| reason | 条数 | 占比 | 语义（源代码 `relay/common/stream_status.go:12-22`） | 是否算正常结束 |
|---|---:|---:|---|---|
| `done` | 1348 | 82.8% | 收到 `[DONE]` 标记 | ✅ |
| `eof` | 261 | 16.0% | 扫描器读尽（Anthropic 流无 `[DONE]`，正常情况下也是 eof） | ✅ |
| `client_gone` | 16 | 1.0% | 客户端/请求上下文取消（`context canceled`） | ❌ |
| `handler_stop` | **4** | 0.25% | dataHandler 主动 stop —— **本次 4 次断流全部落在这里** | ⚠️ 名义正常，但被 soft error 拉成 [ERR] |
| `timeout` / `scanner_error` / `panic` / `ping_fail` | **0** | — | 空闲超时/扫描器错误/崩溃/心跳失败 | — |
| **合计** | **1629** | 100% | | |

**关键**：`handler_stop` 这 4 条 100% 对应本次 4 次断流，且 4 条**全部**带 `end_error="上游流式响应中断: unexpected EOF" soft_errors=1`。

### 1.2 `channel error` / `record error log` / `[ERR]` 分布

```bash
grep -c 'stream ended' /tmp/na24.log; grep -c 'channel error' /tmp/na24.log
grep -c 'record error log' /tmp/na24.log; grep -c '\[ERR\]' /tmp/na24.log
grep -o '^\[[A-Z]*\]' /tmp/na24.log | sort | uniq -c | sort -rn
```

| 类别 | 24h 条数 |
|---|---:|
| `stream ended`（INFO+ERR） | 1629 |
| `channel error` | **8** |
| `record error log` | **8** |
| `[ERR]` | **194** |
| `[INFO]` | 6507 |
| `[SYS]`（每分钟 options 同步） | 4728 |
| `[GIN]` | 2713 |
| `[WARN]` | 2 |

**`[ERR]` 194 条的消息分类**（`sed` 归一化 id/数字后 `uniq -c`）：

| 归一化消息 | 条数 | 归属 |
|---|---:|---|
| `user <N> \| No available channel for model <M> under group default (distributor)` | 150 | 无可用渠道（模型未配渠道），与断流无关 |
| `stream ended: reason=client_gone end_error="context canceled", received=<N>` | 16 | 客户端主动断开 |
| `stream ended: reason=handler_stop end_error="上游流式响应中断: unexpected EOF" soft_errors=1, received=<N>` | **4** | **本次断流** |
| `relay error: 上游流式响应中断: unexpected EOF` | **4** | **本次断流** |
| `channel error (channel #<ID>, status code: <C>): status_code=500, 上游流式响应中断: unexpected EOF` | **4** | **本次断流** |
| `relay error / channel error / bad response status code 524`（channel #11 千刀） | 3+3+3 | 另一个上游 524 超时，与本次无关 |
| `strict_session_binding_unavailable` | 2 | 会话绑定 |
| `Invalid token` / `total tokens is 0` / `model concurrency limit exceeded` / `error handling last response` | 1+1+1+1 | 零散 |

> `record error log` 是 INFO 级（8 条）+ `channel error` 是 ERR 级（8 条）：**8 条里 4 条就是本次断流**，另 1 条 429 并发限流、3 条 channel #11 的 524。

### 1.3 流量分母（用于判断严重程度）

```bash
grep '\[GIN\]' /tmp/na24.log | grep -oE 'POST /v1/[a-z/]+' | sort | uniq -c | sort -rn
grep -c 'params={"channel_id":4,' /tmp/na24.log
grep 'params={"channel_id":4,' /tmp/na24.log | grep -o '"stream_status":{"end_reason":"[a-z_]*"' | sort | uniq -c
```

| 指标 | 数字 |
|---|---:|
| `POST /v1/chat/completions` | 1399 |
| `POST /v1/responses` | 281 |
| `POST /v1/messages`（Claude 格式，走 channel #4 的入口） | 106 |
| channel #4 成功计费流（`record consume log`） | **101**（glm-5.3 × 87、glm-5.3-flash × 14） |
| channel #4 成功流的 `end_reason` | **eof × 101（100%）** |
| channel #4 失败流 | **4** |
| ⇒ glm-5.3 失败率 | 0/87 = **0%** |
| ⇒ glm-5.3-flash 失败率 | 4/(14+4) = **22.2%** |

**这张表是第 5 条结论的直接证据**：channel #4 的成功流 101/101 都是 `eof`，没有一条是 `done`——因为 Anthropic 流以 `message_stop` 收尾、没有 `[DONE]`。所以 `eof` 是这条链路的「正常结束」，也意味着 **「没收到 `message_stop` 就 EOF」与「正常收完再 EOF」在 new-api 眼里长得一模一样**。

---

## 2. 四次断流的时间对齐（UTC + 北京时间）

### 2.1 起点：zcode2api 的 4 次断流

```bash
docker logs --since 24h zcode2api > /tmp/z24.log 2>&1
sed -E 's/\x1b\[[0-9;]*m//g' /tmp/z24.log | grep -E '流传输中断|err="unexpected EOF"'
sed -E 's/\x1b\[[0-9;]*m//g' /tmp/z24.log | grep 'event=end' | grep -E 'ts=2026-11-07T(09|10):'
```

| # | zcode2api req | 断流时刻 UTC | 断流时刻 BJT | 请求起始 UTC | 总耗时 | 已收字节 | firstbyte / maxgap | 出口线路 |
|---|---|---|---:|---|---:|---:|---|---|
| 1 | `d48d55` | 09:27:49.564 | 17:27:49 | 09:24:44.1 | 185.4s | 684,707 B | 2917ms / 14394ms | `socks5://69.55.49.177:38182`（acc `vtnpdx`） |
| 2 | `843568` | 10:12:49.345 | 18:12:49 | 10:11:09.9 | 99.4s | 534,113 B | 2869ms / 2991ms | 同线路（acc `ezfiew`） |
| 3 | `f452ae` | **10:25:06.486** | **18:25:06** | **10:16:00.3** | 546.0s | 2,083,209 B | 9434ms / 4855ms | `订阅-94da87515c35892a`（acc `qxoodk`，**attempts=2**） |
| 4 | `fcd8cd` | 10:32:49.959 | 18:32:49 | 10:31:52.0 | 57.8s | 322 B | 2913ms / — | 同线路（acc `ezfiew`） |

**⚠️ 对任务书中「10:16:2x」的更正**：第 3 次断流的**发生时刻是 10:25:06.486 UTC**，不是 10:16:2x。10:16:00.3 UTC 是**这个请求的开始时刻**（由 `f452ae` 的 `total_ms=546223` 倒推 + new-api 侧 `18:16:00` 的 `user 1 has enough quota` 行双重印证）。这次请求内 zcode2api 还自己换过一次账号（`attempts=2`），仍以 EOF 收场。

第 4 次（`fcd8cd`）由任务指定自查，原始行（已去 ANSI 色码）：

```
[ingress] fcd8cd event=end req="fcd8cd" protocol=messages ts=2026-11-07T10:32:49.959032691Z http_status=200 response_bytes=443 total_ms=57913 canceled=true
[#] fcd8cd model=glm-5.3-flash stream=true ... bytes=322B ... trunc_total=2 total=57829ms status=200 err="unexpected EOF"
```

### 2.2 new-api 侧的配对记录（1:1 全部命中）

```bash
for id in 202611070924440361434448268d9d6jwUhY9fv ...; do grep "$id" /tmp/na24.log; done
# 再按 request_id 回查 logs 表的 other JSON（见下）
```

| # | new-api request id | 渠道 | 模型 | 入口 | new-api 耗时 | 给客户端的 HTTP 码 | stream ended reason | channel error | 换渠道重试 | billing quota |
|---|---|---|---|---|---|---:|---|---|---|---:|
| 1 | `202611070924440361434448268d9d6jwUhY9fv` | #4 zcode2api | glm-5.3-flash | `POST /v1/messages` | 3m5.549s | **200** | `handler_stop`, `soft_errors=1`, received=5096 | 有（500） | **否** | 0 |
| 2 | `202611071011095739999648268d9d6I9jWUdiT` | #4 | glm-5.3-flash | `POST /v1/messages` | 1m39.79s | **200** | `handler_stop`, `soft_errors=1`, received=3993 | 有（500） | **否** | 0 |
| 3 | `202611071015598012815878268d9d6SSSs5BEx` | #4 | glm-5.3-flash | `POST /v1/messages` | 9m6.564s | **200** | `handler_stop`, `soft_errors=1`, received=15572 | 有（500） | **否** | 0 |
| 4 | `202611071031517523822658268d9d6yu0atpac` | #4 | glm-5.3-flash | `POST /v1/messages` | 57.974s | **200** | `handler_stop`, `soft_errors=1`, received=3 | 有（500） | **否** | 0 |

**耗时与 UTC 起点互相校验通过**（逐条误差 < 1s）：

| # | new-api 起点 UTC | + 耗时 | = 结束 UTC | zcode2api 断流 UTC | 差值 |
|---|---|---|---|---|---:|
| 1 | 09:24:44（id 前缀 `…09244403…`） | 185.549s | 09:27:49.5 | 09:27:49.564 | ~0.06s |
| 2 | 10:11:09（`…10110957…`） | 99.791s | 10:12:49.1 | 10:12:49.345 | ~0.25s |
| 3 | 10:16:00（`…10155980…`） | 546.564s | 10:25:06.5 | 10:25:06.486 | ~0.06s |
| 4 | 10:31:51（`…10315175…`） | 57.974s | 10:32:48.9 | 10:32:49.959 | ~1.0s |

> new-api 的 request id 前缀就是 UTC 起始时刻（`YYYYMMDDHHMMSS`），可当作独立时间源使用。

### 2.3 四次事件的关键日志行（原文，单行 ≤300 字符）

```
[ERR] 2026/11/07 - 17:27:49 | 202611070924440361434448268d9d6jwUhY9fv | stream ended: reason=handler_stop end_error="上游流式响应中断: unexpected EOF" soft_errors=1, received=5096
[ERR] 2026/11/07 - 17:27:49 | 202611070924440361434448268d9d6jwUhY9fv | channel error (channel #4, status code: 500): status_code=500, 上游流式响应中断: unexpected EOF
[INFO] 2026/11/07 - 17:27:49 | 202611070924440361434448268d9d6jwUhY9fv | record error log: userId=1, channelId=4, modelName=glm-5.3-flash, content=status_code=500, 上游流式响应中断: unexpected EOF
[GIN] 2026/11/07 - 17:27:49 | relay | 202611070924440361434448268d9d6jwUhY9fv | 200 | 3m5.549280662s | 223.88.45.25 | POST /v1/messages
```
（第 2/3/4 次同构，仅时间戳、request id、耗时、`received=` 不同，见上表。）

**`logs` 表里 4 行的 `other` JSON 是本次最硬的证据**（`quota=0`、`type=5` error log）：

```json
{"admin_info":{
  "request_policy":[
    {"attempt":1,"channel_id":4,"elapsed_ms":46,"decision":{"action":"attempt","reason":"channel_selected","source":"routing"}},
    {"attempt":1,"channel_id":4,"status":500,"error_code":"api_error","error_source":"upstream","elapsed_ms":185515,
     "decision":{"action":"failure","reason":"upstream_failure","source":"upstream"}},
    {"attempt":1,"channel_id":4,"status":500,"error_code":"api_error","error_source":"upstream","elapsed_ms":185515,
     "decision":{"action":"stop","reason":"attempt_budget_exhausted","source":"global"},"health":"unchanged"}],
  "use_channel":["4"]},
 "error_code":"api_error","error_type":"claude_error","request_path":"/v1/messages","status_code":500}
```

逐字段解读：

| 字段 | 值 | 含义 |
|---|---|---|
| `error_source` | `upstream` | new-api 明确认定**错误来自上游**，不是自己的网络读失败 |
| `error_type` / `error_code` | `claude_error` / `api_error` | 来自上游 SSE `event: error` 帧里的 `"type":"api_error"`（正是 zcode2api 写的内容） |
| `decision.action / reason` | `stop` / **`attempt_budget_exhausted`** | 重试判定"想重试"，但**预算为 0** → 停 |
| `health` | `unchanged` | **没有**把渠道标记异常/熔断 |
| `use_channel` | `["4"]` | 只用过 1 条渠道 → **实证没有换渠道** |

---

## 3. new-api 配置现状（DB 只读）

挂载：`bind /opt/1panel/apps/local/new-api/data -> /data (rw)`，库为 `/data/one-api.db`（SQLite，33.6MB，含 `-wal`）。
容器内与宿主机**都没有 sqlite3**（`docker exec ... which sqlite3` → 无；宿主机 `which sqlite3` → 无），故：

```bash
mkdir -p /tmp/nadb
cp /opt/1panel/apps/local/new-api/data/one-api.db{,-wal,-shm} /tmp/nadb/
python3 -c "import sqlite3; ..."   # 只读查询副本，未触碰 /data
```

### 3.1 channels 表（全量 13 条，摘出关键列）

| id | name | type | base_url | models | status | priority | weight | auto_ban |
|---:|---|---|---|---|---:|---:|---:|---:|
| **4** | **zcode2api** | **14（Anthropic）** | **http://zcode2api:3000** | **glm-5.3-flash, glm-5.3** | 1（启用） | 0 | 0 | **0（不自动封禁）** |
| 3 | workbuddy2api | 1 | http://workbuddy2api:7863 | deepseek-v4.1-flash, kim* | 1 | 0 | 0 | 0 |
| 5 | cliproxyapi | 1 | http://cli-proxy-api:8317 | gpt-6-luna | 1 | 0 | 0 | 0 |
| 11 | 千刀 | 1 | https://api666.zzzcoding.org | gpt-6-astra 等 | 1 | 0 | 0 | 1 |
| 8 / 9 / 10 / 13 | cliproxyapi_g / agent2api / any / agent | — | — | — | **2（禁用）** | 0 | 0 | — |
| 1 / 2 / 6 / 12 | cat / cato / chatgpt2api / any_an | 24 / 1 / 1 / 14 | — | gemini* / gpt-image* / claude* | 1 | 0 | 0 | — |

channel #4 其余字段：`setting={"force_format":false,"thinking_to_content":false,"proxy":"","pass_through_body_enabled":false,...}`（**渠道级 proxy 为空**、未开 body 透传）。

### 3.2 ⭐ abilities 表：glm-5.3-flash 只有一条路由

```
{'group':'default','model':'glm-5.3',       'channel_id':4,'enabled':1,'priority':0,'weight':0}
{'group':'default','model':'glm-5.3-flash', 'channel_id':4,'enabled':1,'priority':0,'weight':0}
```

**这是「不可能换渠道」的铁证**：路由表里 `glm-5.3-flash` 只有 channel #4 一个候选，priority/weight 都只有一项，**没有任何 fallback 渠道**。所以「断流时自动换渠道」在当前拓扑下**物理不可行**，与 `RetryTimes` 是否开启无关。

### 3.3 options 表：只有 19 个键，与重试/超时相关的只有一个

| key | 值 | 与本问题的关系 |
|---|---|---|
| **`RetryTimes`** | **0** | **零重试**。直接决定 `attempt_budget_exhausted` |
| `SelfUseModeEnabled` | true | 自用模式 |
| `performance_setting.monitor_enabled` | false | 监控关（不影响） |
| `DemoSiteEnabled` / `RegisterEnabled` / `PasswordRegisterEnabled` | false | 无关 |
| `LegacyAccessTokenRetireAt` | 1795243620 | 无关 |
| `CacheRatio` / `CompletionRatio` / `ModelRatio` / `ModelPrice` / `CreateCacheRatio` / `AudioRatio` / `AudioCompletionRatio` / `ImageRatio` | 计费表 | 无关 |
| `billing_setting.*`（3 个） | `{}` | 无关 |
| `theme.frontend` | default | 无关 |

**找不到的键**（重要）：`AutomaticDisableChannelEnabled`、`AutomaticEnableChannelEnabled`、`StreamingTimeout`、`Timeout`、`general_setting.ping_interval_*` **在 options 表中都不存在** → 说明这些项从未被管理面板保存过，当前全部走**运行默认 / 环境变量**：

```bash
docker inspect new-api --format '{{range .Config.Env}}{{println .}}{{end}}'
# → STREAMING_TIMEOUT=300  TZ=Asia/Shanghai  ERROR_LOG_ENABLED=true  BATCH_UPDATE_ENABLED=true  NODE_NAME=new-api-node-1
docker exec new-api sh -c "grep -ac '上游流式响应中断' /new-api"   # → 0
```

| 超时 / 重试实体 | 现值 | 来源 |
|---|---|---|
| `RetryTimes` | **0** | options 表 |
| `STREAMING_TIMEOUT`（**空闲**超时，非总时长） | **300s** | 环境变量（代码默认也是 300，`common/init.go:138`） |
| `RELAY_TIMEOUT`（客户端总超时） | **0 = 不限制** | 代码默认，`common/init.go:109`；`service/http_client.go:129` 仅当 ≠0 才设 `client.Timeout` |
| `STREAM_SCANNER_MAX_BUFFER_MB` | 128 | 代码默认 |
| channel #4 `auto_ban` | **0（关）** | channels 表 → `ProcessChannelError` 的 `ShouldDisableChannel && AutoBan` 分支**永不触发** |
| keepalive ping | DB 无 `general_setting.*` 键；镜像内嵌前端默认值 `ping_interval_enabled=false, ping_interval_seconds=60` | **推断为关闭**（见第 6 节限制） |

---

## 4. 源码确认：new-api 在「已写若干字节后上游断流」时到底做了什么

本地可得源码：`.zcode/diagnostics/session-5bbbdd82/new-api-source/**`（7 个文件）+ `temp/v3-implementation/*.go`（29 个文件）。
**`relay/channel/claude/*`（`ClaudeStreamHandler`）与 `controller/relay.go` 不在副本内**，涉及这两处的地方会明确标注「未逐行确认」。

### 4.1 错误是怎么被「看见」的：soft error 机制

```go
// relay/common/stream_status.go:55-68
func (s *StreamStatus) RecordError(msg string) { s.ErrorCount++; ... }

// relay/common/stream_status.go:97-111  Summary()
fmt.Fprintf(b, "reason=%s", s.EndReason)
if s.EndError != nil { fmt.Fprintf(b, " end_error=%q", s.EndError.Error()) }
if s.ErrorCount > 0 { fmt.Fprintf(b, " soft_errors=%d", s.ErrorCount) }
```

```go
// relay/helper/stream_scanner.go:293-309
select {
case <-ticker.C:  info.StreamStatus.SetEndReason(StreamEndReasonTimeout, nil)
case <-stopChan:  // EndReason already set by the goroutine that triggered stopChan
case <-c.Request.Context().Done(): info.StreamStatus.SetEndReason(StreamEndReasonClientGone, ...)
}
cleanup()
if info.StreamStatus.IsNormalEnd() && !info.StreamStatus.HasErrors() {
    logger.LogInfo(c, fmt.Sprintf("stream ended: %s", info.StreamStatus.Summary()))
} else {
    logger.LogError(c, fmt.Sprintf("stream ended: %s, received=%d", ...))
}
```

对本次现象的解释完整闭合：

- `reason=handler_stop` ← dataHandler 主动调 `stop()`（走 `stopChan` 分支，注释明写 "EndReason already set by the goroutine that triggered stopChan"）；
- `end_error="上游流式响应中断: unexpected EOF"` ← 上游 error 帧里的 message 被包成 error 传进 `SetEndReason`；
- `soft_errors=1` ← 恰好一次 `RecordError`；
- 虽然 `IsNormalEnd()` 对 `handler_stop` 返回 **true**（`stream_status.go:88-95`），但 `&& !HasErrors()` 把它降级成 **[ERR]** —— 所以这 4 条落在 `[ERR]` 里而不是 INFO。

### 4.2 错误文本写给客户端了吗？写的是什么形态？

**这不是 new-api 的原创文本**（镜像里 0 命中），形态由 **zcode2api** 决定：

```go
// zcode2api: internal/gateway/handler.go:108-124（passthroughDeliver）
if err != nil {
    // 上游自己的 error 事件已原样转发给客户端，再追加一条「中断」既重复又失实。
    if strings.Contains(d.ContentType, "text/event-stream") && !IsUpstreamErrorEvent(err) {
        // HTTP 200 已发出，只能用 SSE 错误事件通知客户端；额外空行隔开上游可能残留的半行。
        data, _ := marshalJSON(map[string]any{
            "type":  "error",
            "error": map[string]any{"type": "api_error", "message": "上游流式响应中断: " + err.Error()},
        })
        if _, werr := fmt.Fprintf(w, "\n\nevent: error\ndata: %s\n\n", data); werr != nil { return werr }
    }
    return err
}
```

即：断流时 zcode2api 在**已返回 200 的 SSE 响应体里补写一帧**：

```
event: error
data: {"type":"error","error":{"type":"api_error","message":"上游流式响应中断: unexpected EOF"}}
```

new-api 的 scanner 只取 `data:` 行（`stream_scanner.go:254-264` 跳过非 `data:` / `[DONE]` 的行，所以 `event: error` 那一行被丢弃），把 JSON 交给 dataHandler，后者从中拿到 `type=api_error` + message → 记为 `claude_error` / `api_error`。

**new-api 自己往客户端写的内容**：它用 `ClaudeData` / `ClaudeChunkData`（`relay/helper/common.go:61-85`）**重新组装** SSE 帧（`event: <Type>` + `data: <json>`），不是字节透传。因此客户端最终是否看到原文那句中文，取决于 `ClaudeStreamHandler` 对 `type==error` 事件是「转发后再 stop」还是「仅 stop」——**该文件不在本地副本中，未能逐行确认**。

可确证的是**结果**：4 次全部是 `HTTP 200 + 流中断`，客户端拿不到完整回答；且 zcode2api 侧 4 次全部记录 `canceled=true`（见 2.1 的 `event=end` 行），说明 new-api 在收到 error 帧后**立即拆连接**（`cleanup()` → `resp.Body.Close()`，`stream_scanner.go:124-139`），没有尝试优雅收尾。

### 4.3 会换渠道重试吗？

```go
// relay/relay.go:158（temp/v3-implementation/newapi-relay.go:158）
for ; retryParam.GetRetry() <= common.RetryTimes; retryParam.IncreaseRetry() {
    ...
    decision := service.DecideRelayRetry(c, newAPIError, common.RetryTimes-retryParam.GetRetry())  // :207
    processChannelError(c, ...)                                                                    // :209
    if decision.Action != "retry" { break }                                                        // :211
}
```

```go
// service/relay_error.go:21-58  DecideRelayRetry
if types.IsChannelError(err) { return PolicyDecision{Action: "retry", Reason: "channel_error", ...} }
...
if retryTimes <= 0 { return PolicyDecision{Action: "stop", Reason: "attempt_budget_exhausted", Source: "global"} }
```

两层结论：

1. **机制上**：channel error 的判定是「想重试」（`reason=channel_error`），但 `RetryTimes=0` 使循环条件 `0 <= 0` 首次成立后即退出，且 `retryTimes(0) <= 0` → `attempt_budget_exhausted` → `stop`。日志里 `重试` 字样 **24h 内 0 条**，实证从未重试。
2. **有没有「已写出就不重试」的守卫**：本地可得的重试循环副本里**没有**（`grep -E 'Written\(\)|wroteHeader|headersSent'` 在 29 个文件命中 **0**）。结合 `doRequest` 在**发上游请求之前**就 `helper.SetEventStreamHeaders(c)`（`relay/channel/api_request.go:544-545`），可以判断：**把 `RetryTimes` 调大并不能把断流变成无缝续传**，只会在同一个已写出的 SSE 响应里追加第二条上游流（重复内容 / 协议错乱），客户端体验更差。controller 层是否另有兜底守卫未在副本内，标注为未确认。

### 4.4 stream scanner 的空闲 / 总时长判定 + keepalive

| 维度 | 实现 | 数值 | 本次是否触发 |
|---|---|---|---|
| **空闲超时** | `ticker := time.NewTicker(streamingTimeout)`，**每扫到一行就 `ticker.Reset`**（`:250`），`select` 命中即 `reason=timeout`（`:293-295`） | `STREAMING_TIMEOUT=300` s | **否**（4 次 maxgap 最大 14.4s；第 3 次总时长 546s > 300s 也没被砍，证明是「空闲」而非「总时长」） |
| **总时长** | 无。`RELAY_TIMEOUT=0` → `client.Timeout` 不设 | 不限 | — |
| **单次写阻塞** | `ExtendWriteDeadline`：每次写前把写 deadline 推到 now+30s | 30s | 否 |
| **扫描器缓冲** | 初始 64KB，上限 `STREAM_SCANNER_MAX_BUFFER_MB` | 128MB | 否（`scanner_error` 0 条） |
| **keepalive 心跳** | `doRequest` 里若 `generalSettings.PingIntervalEnabled` 且未 `DisablePing`，则 `startPingKeepAlive` 向客户端写 SSE 注释帧 `": PING\n\n"`（`relay/helper/common.go:110-123`）；另有 `stream_scanner.go:150-197` 的第二处 pingTicker（`DefaultPingInterval=10s` 兜底） | DB 无 `general_setting.*` 键；镜像内嵌前端默认 `enabled=false / 60s` | **推断为关闭**（见限制） |

**限制（必须写明）**：`ping_interval_enabled` 的**后端运行时默认值**定义在 `setting/operation_setting`（不在本地副本内）。能确证的只有：① options 表里没有该键（从未被面板保存）；② 镜像内嵌前端默认值是 `false`；③ 代码里 `DefaultPingInterval=10s` 只是「间隔值为 0 时的兜底」，不是「开关默认开」。故「当前 keepalive 关闭」属**高置信度推断而非实测**。补充：keepalive 只影响「下游长时间无字节被中间层掐断」，**无法掩盖上游 EOF**，因此对本次结论不构成影响。

### 4.5 渠道处置：没有熔断、没有降权

```go
// service/relay_error.go:68-74
logger.LogError(c, fmt.Sprintf("channel error (channel #%d, status code: %d): %s", ...))
if ShouldDisableChannel(err) && channelError.AutoBan { gopool.Go(func() { DisableChannel(channelError, reason) }) }
```

channel #4 的 `auto_ban=0` → 短路；日志 `health: "unchanged"` 与 DB 中 `status=1` 双重印证：**断流 4 次后 channel #4 仍然健康**。这既是「没有自动换渠道」的原因之一，也是「不会因为上游抖动把唯一渠道打死」的保护。

---

## 5. 结论：放大器还是无辜的转发者？

### 5.1 定性

**无辜的转发者，而且是一个「记账合格」的转发者。**

| 判据 | new-api 的表现 | 证据 |
|---|---|---|
| 是否**产生**该错误 | **否**（文本来自上游 SSE error 帧） | 镜像内 `上游流式响应中断` 命中 0；`error_source=upstream` |
| 是否**篡改 / 放大**错误 | 否，原样记录 message | `content=status_code=500, 上游流式响应中断: unexpected EOF` |
| 是否**误计费** | 否，`quota=0` + `RefundFailedRequestBilling` | logs 表 4 行 `quota=0`；`relay.go:144-146` |
| 是否**误判渠道** | 否，未封禁未降权 | `health=unchanged`；`auto_ban=0` |
| 是否**能够**救场 | **否**，且客观上做不到 | 200 + 已写字节不可回滚；`abilities` 无备选渠道；`RetryTimes=0` |
| 是否**新增**伤害 | 一处轻微：收到 error 帧后立即拆连接（`canceled=true`），未等上游自然收尾 —— 但对已失败的回答无实质差别 | 4 次 `event=end ... canceled=true` |

唯一可以说它「像放大器」的地方是**观感与噪音**：

- HTTP 状态码固定为 **200**（流已开始，无法改），所以监控 / 客户端在 HTTP 层看不到 5xx，只能从「流中断 + 业务错误」里发现；
- 同一事件在 24h 日志里被记 **4 次**（`stream ended` / `channel error` / `relay error` 三条 `[ERR]` + 一条 `record error log` INFO），排查时容易误以为有 4 个独立问题。

### 5.2 有没有配置项能让它在断流时自动换渠道 / 自动重试，不暴露给客户端？

| 候选配置 | 现在 | 调了会怎样 | 能否「不暴露给客户端」 |
|---|---|---|---|
| `RetryTimes` → 1~3 | 0 | 允许重试。但**只在「还没往客户端写出任何字节」时有效**；本次是写出 5k–15k 事件后断流，重试会把第二段流塞进同一 SSE 响应 | ❌ 反而更糟（内容重复 / 协议错乱） |
| 给 `glm-5.3-flash` 加第二条渠道（备选 priority） | 只有 #4 | 有了候选，但仍需等「下一次请求」才生效；**已开始的流依然救不了** | ❌ |
| `auto_ban=1` + `AutomaticDisableChannelEnabled` | `auto_ban=0`，且该键不存在 | 断流后自动禁渠道 → 下一次请求换渠道（但无渠道可换 → 直接报「无可用渠道」） | ❌，且会让唯一渠道被上游抖动打死 |
| `STREAMING_TIMEOUT`（300s） | 300 | 只影响「完全无数据」的空闲判定；本次断流前一直有数据 | ❌ 无关 |
| `RELAY_TIMEOUT` | 0 | 设了只会更早砍断长回答（本次最长 546s） | ❌ 有害 |
| `ping_interval_enabled` | 推断关 | 只是 SSE 注释保活，防中间层掐空闲连接 | ❌ 无关 |

**净结论：new-api 侧不存在任何能「让客户端看不到这次断流」的配置。** 它面对的是 Anthropic 协议的硬约束 —— **200 已经发出、字节已经写出，协议里没有「回滚重来」的表达**。要让客户端无感，只有两条路，都在上游一侧：

1. zcode2api 在断流时**不要立刻收尾**，而是自己先换线路 / 账号续拉（第 3 次它已做过 `attempts=2`），把可续的部分补齐再优雅结束；
2. zcode2api 若必须失败，应保证**错误帧 + 正确的终止语义**（现在已做），并把力气花在降低断流发生率上。

### 5.3 顺带发现的记账风险（建议单独立项）

见 1.3：channel #4 的 101/101 成功流都以 `end_reason=eof` 结束，说明 new-api **不校验 `message_stop`**。本次 4 次断流能被正确记为失败，**完全依赖 zcode2api 主动补写的 `event: error` 帧**（`internal/gateway/handler.go:110` 的 `!IsUpstreamErrorEvent(err)` 分支）。若出现「上游静默断连、不写 error 帧」的情形（例如 zcode2api 进程被杀，或 TCP 被 RST 且未走到 `passthroughDeliver` 的错误分支），new-api 会以 `reason=eof` + `[INFO]` + 照常计费，**客户端拿到截断回答而账单正常扣除**。这是比本次 4 次断流更值得防的问题。

---

## 6. 附：方法与限制

**做了什么（全程只读）**

- `docker logs --since 24h` 快照到宿主机 `/tmp/na24.log`（14,498 行）、`/tmp/z24.log`（2,565 行）；所有聚合在服务器侧完成，未把大段日志拉回本地。
- 数据库三件套 `cp` 到 `/tmp/nadb/` 后用 Python `sqlite3` 只读查询（`/data` 未被写入）；未重启、未停止、未删除任何容器。
- 只读探测运行镜像：`docker exec new-api sh -c "grep -ac ... /new-api"`（仅读取二进制文件）。
- 本地源码阅读：`temp/v3-implementation/*.go`（29 个，含 relay 主循环、claude handler、claude adaptor、service/relay_error、api_request、helper/common）+ `.zcode/diagnostics/session-5bbbdd82/new-api-source/**`（7 个，含 stream_scanner、stream_status、init、env、http_client）+ zcode2api 自身 `internal/gateway/handler.go`。

**没做到的（限制，供复核）**

1. `new-api/relay/channel/claude/claude.go`（`ClaudeStreamHandler`）**不在本地副本**：无法逐行确认收到 `type==error` 事件时是「转发该帧再 stop」还是「仅 stop」。因此「客户端是否原样看到那句中文」只能确证「必然看到流中断」，不能确证帧内容。
2. `new-api/controller/relay.go` **不在副本**：无法排除 controller 层对「已写出响应」另有重试守卫；本文关于「调大 RetryTimes 有污染风险」的判断基于 relay 主循环，已在 4.3 标注。
3. `ping_interval_enabled` 的后端运行时默认值在 `setting/operation_setting`（不在副本）：只能确证 DB 无该键 + 前端默认 false，见 4.4。
4. 未对客户端（ZCode，223.88.45.25）侧做任何抓包 / 复现；未向 new-api 发任何测试请求（避免影响生产配额与上游额度）。
5. 统计窗口以 `docker logs --since 24h` 为准；容器 `Up 2 days`，日志未被轮转截断（首行 `2026-11-06 18:40:47` 覆盖完整 24h）。
