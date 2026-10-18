# 实施计划：软失败判据改为连续失败 + 拨号/握手超时收紧

> 依据：`docs/analysis-20261018-day.md`（v2.10.23-go 跑一天复盘）
> 目标版本：**v2.10.24-go**
> 状态：**待批准实施**

---

## 一、两个 P0 各治什么

| 问题 | 证据 | 本计划 |
|---|---|---|
| 软失败隔离空转 | 211 次尝试分散在 84 条线路，单线 2.5 次/3h；「30 分钟 ≥3 次失败」永远达不到，全天仅 2 次隔离 | **P0-1**：改用「连续失败」判据，与流量密度脱钩 |
| 240s 请求的钱花在拨号 | 8.7MB 请求 3 次 `connection_failed` 各约 70s；`dialTimeout=30s` + 握手再用 30s | **P0-2**：拨号 12s / 握手 20s（可配），单次尝试 60–80s → ≤35s |

---

## 二、P0-1：软失败改「连续失败」判据

### 2.1 为什么现在的判据不行

现判据：`30 分钟窗口内 尝试≥3 && 失败≥3 && 失败率≥50%`。

它假设「一条线路吃很多请求」。实测是反的：池子 ~136 条线，3 小时 211 次尝试，**单线平均 2.5 次**。
样本永远攒不到 3 次 ⇒ 判据形状对、数量级错。

### 2.2 新判据

给每条线路加一个「连续失败计数」：

```
成功        → Consecutive = 0
软失败      → 若 Consecutive == 0 或 now-ConsecutiveSince > 窗口 → Consecutive = 1, ConsecutiveSince = now
              否则 Consecutive++
```

隔离判据（A 或 B 成立即可，仍是「只隔离不删除」）：

- **A 连续失败**：`Consecutive >= ZCODE_PROXY_SOFT_CONSECUTIVE`（默认 2）
  且 `now - ConsecutiveSince <= ZCODE_PROXY_SOFT_CONSECUTIVE_WINDOW_SECONDS`（默认 7200）
- **B 失败率兜底**：保持现有 `Attempts>=3 && Failures>=3 && rate>=50%` 不变

为什么要 `ConsecutiveSince` 窗口：一条线路 09:00 失败一次、15:00 又失败一次（中间没有请求），
如果只看计数，它会在从未被验证的情况下被隔离。超过窗口就重新计数，代表「证据已经过期」。

### 2.3 状态机

```
      ┌──────── 成功 ────────┐
      ▼                      │
   正常 ──失败#1──► 观察中 ──失败#2──► 隔离 30m ──到期──► 正常
      ▲                      │              │
      └──────────────────────┘              │隔离期内再失败（5 分钟节流）
                                             ▼
                                    续期 60m/120m/240m
```

- 隔离期间再命中 → 仍走既有 `extended` 分支（strikes++ 升级档位）。
- 隔离到期后第一次失败：`Consecutive` 从 2 变 3，立即再次隔离并升档——这正是「连续失败且从未成功」该有的结果。
- 成功一次即清零，不冤枉「只是抖了一下」的线路。

### 2.4 数据字段

`ProxySoftFailure` 增加两个字段（JSON 只增不减）：

```go
Consecutive      int   `json:"consecutive,omitempty"`
ConsecutiveSince int64 `json:"consecutive_since,omitempty"`
```

### 2.5 与既有机制的关系

- 405 / 402 / 429 仍不进软失败（走各自语义），连续计数只看「超时类」。
- 隔离仍复用 `reassignOffQuarantineLocked` 与 `proxyFailureRechecks`，不新增删除路径。
- `relax` 兜底不变：全池被隔离时仍会选一条，不会返回「无可用账号」。

---

## 三、P0-2：拨号与握手超时

### 3.1 现状

```go
// internal/proxy/client.go:21
const dialTimeout = 30 * time.Second
```

三处使用：`NewTransport.DialContext`、`socksDialer` 的 forward dial、`socks5Handshake` 的兜底 deadline。

一次 `connection_failed` 的耗时上限 = **连代理 30s + 握手 30s = 60s**，与实测「每次 attempt 约 70s」吻合。

> 说明：v2.10.17 的请求级 240s 预算**刻意不派生 ctx**（长流必须能跑完），所以 `socks5Handshake` 里的
> `ctx.Deadline()` 通常不存在，实际生效的就是这个常量——这正是要改的地方。

### 3.2 改动

拆成两个可配值：

| 变量 | 默认 | 作用 |
|---|---|---|
| `ZCODE_PROXY_DIAL_TIMEOUT_SECONDS` | `12` | 连接到代理服务器本身（通常 <1s，12s 已很宽松） |
| `ZCODE_PROXY_HANDSHAKE_TIMEOUT_SECONDS` | `20` | SOCKS 握手 + 代理连到目标（跨境线路 5–10s 常见，20s 留足余量） |

- `socks5Handshake` / `socks4Handshake` 的兜底 deadline 用握手值；若 ctx 自带 deadline 取更早者。
- `NewTransport` 的 `DialContext` 与 `socksDialer` 的 forward dial 用拨号值。
- 值 ≤0 时回落到默认（不允许「无限等待」）。

**顺带修一个裸奔点**：`socks4Handshake` 目前**完全没有 deadline**
（`internal/proxy/client.go:329`，`readFull` 只受 conn 自身的读超时约束，而没人设过），
理论上可无限阻塞。同一次改动里补上握手 deadline。

**预期效果**：单次死线尝试 60–80s → **≤35s**；3 次尝试的 240s 请求变成 ~110s 内**以真实错误结束**，不再撞墙。

### 3.3 顺带修一个不一致

`connection_failed` 目前**不进请求级回避集合**（只有首字节超时、408/524 会进）。
但连接失败恰恰说明这条线路此刻不可用。补上：`client.Do` 返回传输错误且非客户端取消时，
`attemptResult.avoid = egress.Identity()`。

这样多个账号共享同一条死线时，第二次尝试直接换线，不用再等一个超时。

### 3.4 可观测性

`[attempt] event=error` 现在只有 `err_kind`，不能验证超时是否生效。增加：

```
elapsed_ms=NNN
```

从 `[attempt] start` 到该 error 的毫秒数。上线后一眼就能看出 `connection_failed` 的耗时分布是否从 60–80s 落到 30s 区间。

---

## 四、代码改造点

| 文件 | 改动 |
|---|---|
| `internal/config/config.go` | `ProxySoftConsecutive`(2)、`ProxySoftConsecutiveWindow`(7200s)、`ProxyDialTimeout`(12s)、`ProxyHandshakeTimeout`(20s) |
| `internal/store/proxy_soft_failure.go` | 两个新字段；成功后清零；失败时按窗口重置/累加；判据 A 接入；日志加 `consecutive=` |
| `internal/proxy/client.go` | 删除 `const dialTimeout`；新增 `dialTimeout()` / `handshakeTimeout()` 两个读取配置的函数；三处调用点替换；`socks4Handshake` 补 deadline |
| `internal/gateway/engine.go` | `connection_failed` 分支返回 `avoid: egress.Identity()`；`[attempt] event=error` 增加 `elapsed_ms` |

> `internal/proxy` 目前不依赖 `internal/config`，`config` 也不依赖 `proxy`，引入不会造成循环。

---

## 五、参数

| 变量 | 默认 | 说明 |
|---|---|---|
| `ZCODE_PROXY_SOFT_CONSECUTIVE` | `2` | 连续软失败多少次隔离；0 = 关闭判据 A |
| `ZCODE_PROXY_SOFT_CONSECUTIVE_WINDOW_SECONDS` | `7200` | 连续计数有效期；超过则重新计数 |
| `ZCODE_PROXY_DIAL_TIMEOUT_SECONDS` | `12` | 连接代理本身 |
| `ZCODE_PROXY_HANDSHAKE_TIMEOUT_SECONDS` | `20` | SOCKS 握手 / 到目标建连 |

既有 `ZCODE_PROXY_SOFT_QUARANTINE`、`ZCODE_PROXY_SOFT_MIN_*` 保持原样，判据 B 仍可独立使用。

---

## 六、日志与判读

```
[~] proxy-soft id=proxy-xxx signal=transport_timeout failures=2 attempts=2 rate=100%
    consecutive=2 action=quarantine until=… strikes=1 reassigned=1 no_alt=0
[attempt] 4f2a1c event=error … err_kind=connection_failed elapsed_ms=31842
```

判读口径：
- `consecutive=2` 出现频率应显著高于过去的 `failures=3` —— 这是判据生效的直接证据。
- `elapsed_ms` 分布应从 60–80s 落到 25–35s；仍见 60000+ 说明某条路径没走新超时。
- `relax=1` 连续出现 → 隔离过激，调高 `SOFT_CONSECUTIVE` 或缩短隔离时长。

---

## 七、测试清单

**store（连续判据）**
1. 连续 2 次失败（窗口内仅 2 次尝试）→ `quarantine`，`consecutive=2`
2. 失败 → 成功 → 失败 → 不隔离（成功清零）
3. 两次失败间隔超过 `CONSECUTIVE_WINDOW` → 不隔离（重新计数）
4. 连续 2 次后隔离，隔离期内再失败 → `extended`（既有档位升级不变）
5. `SOFT_CONSECUTIVE=0` → 判据 A 关闭，只剩失败率兜底
6. 失败率兜底仍生效：3 尝试 3 失败但中间有成功（consecutive 被打断）→ 由 B 触发
7. 直连 / 非 proxy 出口仍不计数（回归）

**proxy（超时）**
8. 假代理 accept 后不响应 → 握手在 `HANDSHAKE_TIMEOUT` 内失败（测试用 50ms）
9. 拨号到黑洞地址 → 在 `DIAL_TIMEOUT` 内失败（测试用 50ms）
10. 配置 ≤0 → 回落到默认值，不会无限等待
11. ctx 自带更早 deadline → 取更早者
12. `socks4` 握手在无响应代理上也会按时失败（补 deadline 的回归）

**gateway**
13. 传输层连接失败 → 下一次选号回避该线路（`avoid` 生效）
14. 客户端取消 → 不写 avoid、不冷却（回归）

**回归**
15. `proxy-soft` 既有测试（窗口/续期/静默归零/保护/重启保留）全绿

---

## 八、提交与发布

| # | 提交 | 版本 |
|---|---|---|
| 1 | `feat: 软失败改连续失败判据并补拨号握手超时` | v2.10.24 |
| 2 | `chore: 发布 v2.10.24-go` | — |

CI 全绿 → 附注 tag → 核对 Release 附件与 GHCR（`check-ghcr.js 2.10.24-go`）。

---

## 九、风险与回滚

| 风险 | 对策 |
|---|---|
| 连续 2 次即隔离，误伤抖动线路 | 隔离只有 30 分钟且自动回池；成功一次即清零；`relax` 兜底 |
| 拨号/握手缩短后把「慢但能用」的跨境线路判死 | 握手 20s 覆盖 5–10s 的常见跨境建连；两项都可调，建议先按默认跑 24h 看 `connection_failed` 的线路是否仍能在别的账号上用成功 |
| 大量线路同时被隔离 → 池子变窄 | `relax` + 30 分钟自动回池；观察 `relax=1` 频率 |
| 两个改动混在一起难定位 | 它们是独立判据：`SOFT_CONSECUTIVE=0` 可单独关 A；两个超时可单独调回 30/30 |

回滚：`ZCODE_PROXY_SOFT_CONSECUTIVE=0`、`ZCODE_PROXY_DIAL_TIMEOUT_SECONDS=30`、
`ZCODE_PROXY_HANDSHAKE_TIMEOUT_SECONDS=30` 可逐项退回旧行为。

---

## 十、验收标准

1. 第七节 15 项测试全绿，CI 通过。
2. 上线 24h：
   - `proxy-soft action=quarantine` 次数 **≥10**（当前是 2），且 `consecutive=2` 是最常见来源；
   - `connection_failed` 的 `elapsed_ms` **p90 ≤ 35s**（当前每次 60–80s）；
   - 240s 请求数下降（当前 3h 内 2 个）；
   - `relax=1` 不连续出现，池子条数不低于 100。
3. 三项回滚开关都能单独生效，行为回到 v2.10.23。

---

## 十一、不做什么

- **不改 `IsEndpointUnreachable` 及其调用方**（claim/quota/初始化链路）。
- **不动请求级 240s 预算**：它是长流的保护，本轮只缩短「失败被发现」的时间。
- **不把 connection_failed 直接算作删除证据**：仍只进回避集合与软失败计数。
- **不做线路并发容量再平衡**（v2.10.23 已论证无效）。
- **不改订阅导入节奏**：池子补充速率当前够用。
