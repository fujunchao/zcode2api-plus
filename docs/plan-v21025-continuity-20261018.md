# 实施计划：v2.10.25 —— 让大包请求与 405 都不再打断工作

> 依据：`zcode-newapi-logs-20261018T064122Z.zip`（v2.10.23 · 08:34–11:40 北京 · 161 请求）
> 前置结论：v2.10.24（15:01 发布）只覆盖了「大包超时」的一部分，**上行闸门 / 巡检删除 / 一次性重试三项均未实现**。
> 状态：**四项均已实施并随 v2.10.25-go 发布**（用户已批准四个默认值）。

## 一、证据回顾（v2.10.23 窗口）

| 现象 | 数据 |
|---|---|
| 405 已基本可控 | 上游 405 36/184 次（19.6%，15 条脏线全 0 成功），**客户端可见 405 仅 1 次** |
| 首次 405 不冷却生效 | 30 次「本次只换线、不冷却账号」，账号冷却仅 6 次 |
| **大包撞 240s** | `d4fcaf` 8.34MiB / `07fdc9` 8.55MiB：3 次尝试全部无首字节 → 503；`e09dfb` 5 次尝试 196s 被客户端取消 |
| 首字节分布 | >1MiB 请求 29 条：P50 6.7s、P90 46.9s、max 65.1s |
| **巡检只到 suspect** | 17 次入队、22 条 `verdict=failed`、**0 条 `model-probe rule=`**；12 条隔离线里 10 条仍在池中 |
| **请求级 405 封锁** | 10:34:30 三条首次命中脏线连击 → 405 + 60s 重放封锁 → 客户端 57s 内重试 11 次全被拦；207s 后同内容换线成功 |

## 二、目标

| 指标 | 现状 | 目标 |
|---|---|---|
| >4MiB 请求成功率 | 未单独统计（本窗口 29 条 >1MiB 中 2 条硬失败、3 条取消） | ≥95% |
| 大包失败耗时 | 240s | **≤60s**（要么成功，要么明确告知线路传不动） |
| 405 脏线残留 | 10/12 条仍在池中 | ≤2 条（其余被巡检删除） |
| 请求级 405 后的手动续跑 | 11 次重试全拦 | 首次重试放行，无需手动「继续」 |
| 连接失败导致的账号冷却 | 5 次/3h | **0**（线路问题不记账号） |

## 三、P0-1 上行速率闸门（解决大包）

### 现状

- `engine.go:433` 用 `bytes.NewReader(payload)` 建请求，`asyncpool/pool.go:776` 用 `strings.NewReader`。
- 首字节看门狗（`firstbyte.go`）只给**整段** 90s 预算；上传阶段没有独立判据，只能等它到点。
- 8.34MiB 在 19–40 KB/s 的线上需要 210–450 秒，必然超预算，但代码要等满 90 秒才知道。

### 设计

新增 `internal/gateway/uplink.go`，同步/异步共用：

```go
// 包装请求体，按「已写字节 / 已耗时间」估算剩余上传时间。
type uplinkMeter struct {
    r        io.Reader
    total    int64      // 已知 body 大小（ContentLength）
    sent     int64
    started  time.Time
    budget   time.Duration // 本次尝试的首字节预算（与 watchdog 同值）
    cancel   context.CancelCauseFunc
    aborted  atomic.Bool
}
```

- 每次 `Read` 后累计 `sent`；当 `sent >= MinBytes`（默认 256 KiB）且 `elapsed >= 1s`：
  `rate = sent/elapsed`，`eta = (total-sent)/rate`；
  **`eta > budget-3s` → `cancel(errUplinkTooSlow)`**。
- 判据用**本次尝试预算**（90s），不是请求总预算（240s）：尝试本来就会在 90s 被看门狗掐断，
  拿 240s 比等于永远不触发。
- 19 KB/s 的线在 ~2 秒内就能判死，8.34MiB 不再白等 90 秒。

### 实现要点（必须注意）

1. **不能破坏 `ContentLength`**：`http.NewRequestWithContext` 对 `bytes.Reader` / `strings.Reader`
   会自动填 `ContentLength`；包装时只替换 `httpReq.Body`，**不要重建请求、不要置零 ContentLength**，
   否则出站退化成 `Transfer-Encoding: chunked`，字节形态偏离官方客户端。
2. 包装位置在 `watchdog` 创建之后、`client.Do` 之前，用同一个 `cancel` 来源：
   `httpReq.Body = newUplinkMeter(httpReq.Body, httpReq.ContentLength, firstByteBudget, watchdog.CancelCause)`。
   需要给 `firstByteWatchdog` 增加一个 `Abort(cause error)` 方法（现有 `cancel` 字段是私有的）。
3. 判据只对 `total >= MinBytes` 生效；小请求完全不参与，避免误杀。
4. 失败归类：新增哨兵 `errUplinkTooSlow`，在 `client.Do` 返回错误后
   **先判 `errors.Is(context.Cause(ctx), errUplinkTooSlow)`，再判 `watchdog.Fired()`**——
   两者都会让 ctx 结束，顺序反了会把闸门误报成超时。
5. **不冷却账号**：与 watchdog 超时同一条出路（`recordError` + `avoid=egress.Identity()`）。
6. **计入软失败**：新增信号 `uplink_slow`（`store.ProxySignalUplinkSlow`），
   映射进 `softOutcomeSignal`。这样同一条线连续两次传不动 → 30 分钟出池，
   正是 v2.10.24「连续 2 次」判据想要的输入。
7. 终态语义：全部尝试都因上行不达标被淘汰 → `503 upstream_uplink_too_slow` + `Retry-After: 30`，
   **不要伪装成 524**（524 表示上游超时，会把人引向错误方向）。
8. 诊断：`[#]` 行增加 `up_kbps=`（本次请求最后一条尝试的实测吞吐）与 `uplink_abort=N`。

### 配置

| env | 默认 | 说明 |
|---|---|---|
| `ZCODE_UPLINK_GATE` | `1` | 0 = 关闭闸门（回到只看门狗） |
| `ZCODE_UPLINK_GATE_MIN_BYTES` | `262144` | 低于此体量不判定 |
| `ZCODE_UPLINK_GATE_MIN_MS` | `1000` | 采样宽限，避免慢启动误判 |
| `ZCODE_UPLINK_GATE_MARGIN_SECONDS` | `3` | ETA 与预算的安全余量 |

### 测试

1. 已知总长 + 极慢 Reader（10 KB/s）→ 1–2 秒内 `errUplinkTooSlow`，不冷却账号；
2. 快 Reader → 不触发，正常读到 EOF；
3. 小请求（<256 KiB）+ 慢 Reader → 不触发；
4. `ZCODE_UPLINK_GATE=0` → 不包装、行为与改造前一致；
5. 包装后 `httpReq.ContentLength` 不变、`GetBody` 仍可用（用 httptest 服务端断言收到的 `Content-Length`）；
6. 全尝试被闸门淘汰 → 503 `upstream_uplink_too_slow` + `Retry-After`；
7. 软失败：同线两次 `uplink_slow` → 隔离 30 分钟；
8. 异步路径同样生效（`errLineTimeout` 语义复用，日志文案区分）。

### 风险

| 风险 | 处置 |
|---|---|
| 慢启动误判（TLS + 首包慢） | 1s + 256 KiB 双门槛；余量 3s；开关可关 |
| 吞吐估算偏高（末段缓冲） | 只用于「明显不够」的判定，不用于精确计时 |
| chunked 化改变字节形态 | 单测断言 `Content-Length` 仍存在 |

## 四、P0-2 连接失败不再冷却账号（sync/async 对齐）

### 现状

- `engine.go:519`：`client.Do` 返回错误 → `e.mark(acc, StatusCooling, ErrorKindConnectionFailed, ...)`，
  **冷却 300 秒**。而 watchdog 超时分支明确不冷却。
- v2.10.24 把拨号/握手超时从 60–80s 压到 ≤32s，于是原本会走 watchdog（不冷却）的死线，
  现在更容易走这条分支（冷却）——**可能把「线路的锅」更快记到账号上**。
- 异步路径 `pool.go:820` 直接 `return false, err`，既不冷却账号，也不把线路加入 avoid。

### 设计

- 传输层错误（dial / TLS / 写入失败 / 连接重置）**一律归线路**：
  - sync：`recordError(...)`（只记错误与证据）+ `avoid=egress.Identity()`，不 `mark(cooling)`；
  - async：新增 `errLineTransport{body, avoid}`，票据侧把线路加入 `avoid` 并走网络重试预算。
- 保留回退开关 `ZCODE_COOL_ON_CONNECTION_FAILURE`（默认 `0`；`1` = 旧行为）。
- 与 P0-1 共用「连续两次 → 软失败隔离」这条线路级止损，不再依赖账号冷却。

### 测试

1. sync：连接失败后账号仍 `active`，`avoid` 含该线路，下一次尝试换线；
2. sync：开关置 1 → 恢复 `cooling` 旧行为；
3. async：连接失败后票据换线，账号不冷却；
4. 两条路径对同一故障给出相同的账号状态（项目硬不变式）。

### 风险

| 风险 | 处置 |
|---|---|
| 凭证问题表现为 TLS 错误，不再冷却账号 | 鉴权失败走 401/403 分支，不受影响；开关可回退 |
| 坏线路被反复选中 | 软失败连续 2 次隔离 + `avoid` 双重兜底 |

## 五、P0-3 巡检结论驱动删除（清掉 405 脏线）

### 现状

- `ApplyLine405ProbeVerdict` 只回写状态：`healthy_*` 解封、`route_correlated|suspect` 记 `failed`，**不删**。
- 删除只在 `retireModelProbeLocked` 里发生，且要求 `probeConfirmedRoundsLocked >= 2`
  （两轮 target 405 + 参考线成功、间隔 ≥5 分钟、10 分钟内证据新鲜）。
- 实测：22 条结论**全是 suspect**，0 次删除。原因是这个门槛在真实节奏下几乎不可达
  （`LastDiagnosis` 每目标 1 小时限频 + 5 分钟等待 + 20 分钟 run 截止）。

### 设计：两条独立证据即可退休

判据改为「真实流量 + 巡检」双证，而不是「巡检两轮」：

```
ent.Strikes >= ZCODE_LINE405_RETIRE_MIN_STRIKES (默认 2)
&& probe verdict == failed（suspect / route_correlated）
&& Policy.Mode == auto_retire
```

- `Strikes >= 2` 的含义：这条线**隔离 30 分钟放出去后又被真实流量 405 了一次**，
  与巡检的 405 相互独立——比「同一个探针跑两轮」更接近真实使用。
- 调用点：`CompleteModelProbe` 里 `applyLine405ProbeVerdictLocked` 之后、`retireModelProbeLocked` 之前；
  直接调 `startStrictRetirementLocked(identity, url, "line405_probe_control", target, ModelProbeRuleVersion, refs, now)`。
- `refs` 取该 run 的 `proofs[].RequestID`（最多 4 条），作为可核对证据。
- 保护名单（手动保护 / 默认出口 / 托管默认线路）、代次校验、`auto_retire` 开关全部沿用
  `startStrictRetirementLocked` 内部既有逻辑，**不新增任何删除路径**。
- 删除后订阅墓碑照常写入（`proxy_subscription.go` 按 `SubscriptionKey` 跳过导入），不会被下一轮拉回。
- `ApplyLine405ProbeVerdict` 需要改成返回 `(state, identity string, ok bool)` 供调用方判断。

### 配置

| env | 默认 | 说明 |
|---|---|---|
| `ZCODE_LINE405_PROBE_RETIRE` | `1` | 0 = 只隔离不删除（回到 v2.10.24 行为） |
| `ZCODE_LINE405_RETIRE_MIN_STRIKES` | `2` | 触发退休所需的隔离次数（即「放出去又犯」次数） |

### 测试

1. `strikes=1` + probe failed → 不删；
2. `strikes=2` + probe failed + `auto_retire` → 删除并改派，`model-probe rule=` 出现；
3. `strikes=2` + probe healthy → 解封且不删；
4. `Mode=observe` → 不删（只记 `qualified_observe`）；
5. 线路在保护名单 → 不删；
6. 代次已变（线路被编辑）→ 不删；
7. `ZCODE_LINE405_PROBE_RETIRE=0` → 行为与 v2.10.24 一致。

### 风险

| 风险 | 处置 |
|---|---|
| 免费池波动导致误删「时好时坏」的线 | 需要「两次真实 405 + 一次巡检 405」；`Strikes` 在成功时清零 |
| 删除过多导致池子变小 | 订阅会补；删除写墓碑防回流；`capacity_short` 会提示扩容 |
| 巡检把账号额度烧在脏线上 | 沿用 30 分钟限频 + 每小时 12 次预算 |

## 六、P1 请求级 405：允许一次同内容重试

### 现状

- `engine.go:793` / `pool.go:943`：判定成立 → `replay.Record(key)`，之后 60 秒内同内容直接 503。
- 10:34 事件里客户端 57 秒内重试 11 次，全被拦；而 207 秒后同内容换线成功。
  说明「内容没问题，只是连撞三条脏线」，封锁反而把可自愈的情况变成必须手动「继续」。

### 设计：一次性放行令牌

`ReplayGuard` 条目从 `time.Time` 改为 `{at time.Time, allowances int}`：

- `Record(key)`：**同一窗口内只武装一次**（已有未过期条目 → 不改 `allowances`），避免「每次判定都重新放行」形成无限循环；
- 新增 `Take(key) (blocked bool, remaining time.Duration)`：
  - 未登记 / 已过期 → `false, 0`；
  - `allowances > 0` → `allowances--`，返回 `false, 0`（放行一次）；
  - 否则 → `true, remaining`。
- 调用点：`engine.go` 入口的重放检查、`pool.go:448` 的票据入口，全部改用 `Take`。
- 成本上界：每个内容每 60 秒窗口最多多试 3 条线；相比现状（1 次都不放行）可控。

### 配置

| env | 默认 | 说明 |
|---|---|---|
| `ZCODE_RISK_REPLAY_ALLOW_ONCE` | `1` | 0 = 旧行为（判定后一律封锁 60s） |

### 测试

1. 判定后第一次同内容 → 放行；第二次 → 503；
2. 60 秒后再来 → 放行（窗口已过）；
3. 判定成立两次（同一窗口）→ 仍只放行一次（不重复武装）；
4. `ALLOW_ONCE=0` → 与改造前逐字段一致；
5. sync 与 async 行为一致。

### 风险

| 风险 | 处置 |
|---|---|
| 内容确实坏时多烧 3 条线 | 每窗口仅 1 次；账号不再因首次 405 冷却，代价有界 |
| 客户端重试风暴 | 只放行一次，其余仍被 60s 窗口挡住 |

## 七、提交拆分

| # | 提交 | 内容 |
|---|---|---|
| 1 | `feat: 上行速率闸门` | `gateway/uplink.go` + watchdog.Abort + sync/async 接入 + 软失败信号 + 8 测试 |
| 2 | `feat: 连接失败归因线路` | engine 不冷却 + async `errLineTransport` + 开关 + 4 测试 |
| 3 | `feat: 405 巡检双证退休` | `ApplyLine405ProbeVerdict` 返回值 + `startStrictRetirementLocked` 调用 + 7 测试 |
| 4 | `feat: 请求级 405 一次性放行` | `ReplayGuard.Take` + 两处入口 + 5 测试 |
| 5 | `chore: 发布 v2.10.25-go` | 版本号 + release notes + README |

## 八、验收（用本次日志回放）

- 8.34 / 8.55 MiB 两条请求：**不再 240s 才失败**，要么 60s 内成功，要么 ≤60s 返回 `upstream_uplink_too_slow`；
- 10:34 那种「三连脏线」事件：客户端第一次重试即放行，**不需要手动「继续」**；
- 12 条隔离脏线中 `strikes>=2` 的那批：在 1 小时内被 `line405_probe_control` 删除并改派；
- 账号冷却事件数 ≤3/3h（现状 6/3h）；
- `>4MiB` 请求成功率 ≥95%。

## 九、明确不做

- 不做请求体 gzip（需单账号 canary 验证 3012 风险，另立项）；
- 不做 hedged 双发（同 session 并发有风控风险 + 双计费）；
- 不做主动再平衡（把已绑账号从超限线路挪走，会改既有绑定）；
- 不改 `IsEndpointUnreachable` 判据、不改错误分类链 9 条顺序；
- 不把探针内容换成真实请求体（会消耗更多额度且改变风控面）。

## 十、开放问题（需拍板）

1. 闸门默认开（`ZCODE_UPLINK_GATE=1`）是否可以？我倾向开——它只在「明显传不完」时生效。
2. 连接失败默认不冷却账号，是否接受？回退开关保留。
3. 巡检双证退休默认开（需 `auto_retire` 模式），是否接受？误删风险由「两次真实 405」兜底。
4. 一次性放行默认开，是否接受？若担心内容确实坏时多烧线路，可先置 0 观察一周。
