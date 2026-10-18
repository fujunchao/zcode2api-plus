# 实施计划：v2.10.26 —— 断流计入线路软失败 + 隔离到期观察期

> 依据：`zcode-newapi-logs-20261018T123322Z.zip`（v2.10.25 · 17:49–20:31 北京 · 69 请求）
> 触发案例：`订阅-f7eff3a956775fea` 在 17:57 / 18:48 / 19:01 三次上游断流，全部对客户端可见；
> 该线 17:58 已因「连续 2 次连接失败」隔离到 18:28，到期后**不经任何验证直接回池**，
> 18:48 与 19:01 两次本可避免。
> 状态：**已发布并核对完成**（tag `v2.10.26-go` → `46850f5`；release `39132013714` 成功，
> 5 个附件齐全，GHCR `2.10.26-go` = `latest`，digest `sha256:1fe3ce3f…`）。
> 提交：`83356f3` 软失败 · `7fb6bda` 日志时间戳 · `710a6e9` 发版 · `8d8da0c`/`46850f5` 测试适配。
> 评审收尾补充（纯测试，不改产物）：优先级交互、防孤儿、混合信号、长静默四个用例。

## 一、问题（两条，均已用代码与日志核实）

### 1.1 隔离到期「免费复活」

`store/proxy_soft_failure.go` 现状：

- 到期只判 `QuarantineUntil <= now`，到期即重新参与选号；
- 到期后静默 30 分钟会把 `Strikes / Consecutive` **一起清零**（`proxySoftQuietReset`）。

该线时间线（日志实证）：

```
17:54 连接失败①                       consecutive=1
17:57 断流①（316B，客户端可见）        断流不计软失败
17:58 连接失败② → 隔离至 18:28         consecutive=2 strikes=1 reassigned=2
18:20 网络巡检判 healthy              探测的是连通性，不是流
18:28 隔离到期 → 直接回池              无复检、不清档位/不设观察
18:48 断流②（3443B，客户端可见）       距最后失败 51 分钟，静默清零已生效
19:00 连接失败③                       重新从 1 计数
19:01 断流③（62689B，客户端可见）
19:02 断流熔断（连续 3 次）→ 移除
```

结论：**隔离到期后第一枪是免费的，且静默会抹掉全部档位**。这是设计缺口，不是参数问题。

### 1.2 断流不进软失败统计，阈值倒挂

`gateway/proxy_observation.go` 的 `softOutcomeSignal`（当前第 170 行，v2.10.25 在其前新增了
`ObserveModelAttemptUplinkSlow`，行号已从 164 后移）只认
`success / timeout / connection_failed / uplink_slow`；上游中途断流（已发 chunk、**无法重试**）
只进 `line-guard` 的独立计数（阈值 3）。日志实证：该线 `proxy-soft` 记录是
`failures=2 attempts=2`，三次断流一次都没算。

于是出现倒挂：**客户端看不见的连接失败 2 次就隔离，客户端直接收到残缺响应的断流要 3 次才熔断**。

## 二、方案

### 2.1 A1：断流计入线路软失败

**位置**：`internal/gateway/classify.go` 的 `RecordUpstreamTruncate`。

- 新增信号 `store.ProxySignalUpstreamTruncate = "upstream_truncate"`。
- 在**线路级熔断判定之后**调用一次软失败记录：

```go
// 断流对客户端可见且无法重试，必须进入「线路还能不能用」的统计；
// 但线路刚被熔断移除时不再记账（profile 已不存在，记了只会产生孤儿条目）。
if !removedNow {
    st.RecordProxySoftOutcome(egress, store.ProxySignalUpstreamTruncate, 0, false, now)
}
```

- `removedNow` 由现有 `BumpLineTruncateForAttempt` + `PurgeFailedAccountProxy` 分支给出。
- 只对 `egress.Mode == "proxy"` 生效（沿用函数内既有早退）。
- 三个计数各司其职，不互相替代：账号级 `trunc_total`（观测）、账号回避 60 s、线路软失败（隔离）。

**收益**：该时间线里 17:57 断流即让连击达到 2 → 隔离提前到 17:57（而不是等 17:58 的第二次连接失败）。
**注意**：这一步单独只能把「3 次可见失败」降到 2 次，救不了 18:48——必须配合 A2。

### 2.2 A2：隔离到期进入「观察期」

**位置**：`store/proxy_soft_failure.go` 的 `RecordProxySoftOutcome`。

新增字段：

```go
// Probation 隔离到期后的观察期：线路仍可被选中，但再失败一次立即升档隔离。
// 只有一次「完整成功」才能清空档位——这是观察期唯一的出口。
Probation bool `json:"probation,omitempty"`
```

规则：

| 时机 | 动作 |
|---|---|
| 隔离到期后的第一条事件 | `QuarantineUntil = 0`、`Probation = true`（**不清 Strikes**） |
| 观察期内失败 | 立即 `Strikes++`，按新档位隔离（30→60→120→240），`Probation = false` |
| 观察期内完整成功 | `Strikes = 0`、`Consecutive = 0`、`Probation = false` |
| 观察期且从未被选中 | 保持 `Probation`，不因时间流逝洗白——**但最长持续到条目 TTL（24 小时）**，见下 |
| 删除旧逻辑 | 「静默 30 分钟把 Strikes/Consecutive 一起清零」整段移除 |

**与 24h TTL 的交互（已决策）**

`pruneProxySoftLocked` 会删除 `LastAt < now-24h && QuarantineUntil <= now` 的条目，观察期条目
不在隔离期，因此**24 小时没有任何流量的线路会连 Probation 带 Strikes 一起消失**。

选择**接受这个 TTL 作为隐式出口**，理由：

1. 一天没有任何流量的线路，证据本身已经陈旧；
2. 与 `line405` 的 24h TTL 口径一致，两套线路状态不会出现「一个记一辈子、一个记一天」；
3. 「永不洗白」在容量压力下本来就不可保证——`pruneProxySoftLocked` 的 4096 上限按 `LastAt`
   淘汰最旧条目，观察期条目同样会被淘汰。与其写一条实现不了的不变量，不如把 TTL 写进规格。

因此规格表述改为：**观察期持续到「一次完整成功」或「条目 24 小时无流量被清理」**；
测试 #5 的「长时间静默」限定在 24 小时以内（超过则重新建档、从 1 计数）。

> **实现补充（21:30）**：观察期是**惰性判定**——到期后的第一条事件会在同一次调用里
> 完成「置观察期」与「解析」：成功立即清零、失败立即升档。因此正常流量下不会有长期
> 停留在 `Probation=true` 的条目；能停留的只有「到期后一直没有流量」的条目，正是
> 上面 TTL 讨论覆盖的情形。

为什么出口是「一次完整成功」而不是时间窗：时间窗会让「每小时坏一次」的线路在
隔离↔观察之间永久震荡，或在窗口结束时被无条件洗白；成功是唯一能证明它此刻可用的证据。
`kind == "success"` 的口径现成（`complete && 2xx`），不需要新判据。

回退开关：`ZCODE_PROXY_SOFT_PROBATION=0` 时恢复旧行为（到期直接回池 + 静默清零）。

### 2.3 B：后台日志行补时间戳

**位置**：`internal/web/logs.go` 的 `Warn` / `Err`。

现状 `[~] line-guard …`、`[~] proxy-soft …` 没有 `ts=`，排障只能靠文件行序 + 邻近
`[attempt]`/`[#]` 行反推时间线。改为在**行尾**追加：

```
  [~] proxy-soft id=proxy-f7eff3… action=quarantine … ts=2026-10-18T17:58:49+08:00
```

- 追加在末尾而不是前缀：`[~] <module>` 前缀保持不变，既有 `grep`/脚本不受影响；
- 只在 `Warn` / `Err` 上做（`Req`/`Diag`/`Attempt` 已有自己的时间基准）；
- 时区用本机本地时间（RFC3339），与 `diag` 行的 `ts=` 同格式。

## 三、代码改动清单

| 文件 | 改动 |
|---|---|
| `internal/store/proxy_soft_failure.go` | `ProxySoftFailure.Probation`；到期→观察期；观察期首败升档；成功清零；移除静默清零（开关关闭时保留）；日志加 `probation=` / `trigger=` |
| `internal/store/proxy_soft_failure.go` | 常量 `ProxySignalUpstreamTruncate` |
| `internal/gateway/classify.go` | `RecordUpstreamTruncate` 记账（排除刚熔断的线路） |
| `internal/config/config.go` | `ProxySoftProbation`（默认 1）、`ProxySoftTruncate`（默认 1） |
| `internal/web/logs.go` | `Warn`/`Err` 行尾追加 `ts=` |
| `internal/store/proxy_soft_failure_test.go` | 用例替换/新增（见下） |
| `internal/gateway/linetruncate_test.go` | 断流计入软失败的断言 |

## 四、测试

| # | 用例 | 断言 |
|---|---|---|
| 1 | 两次断流 | `proxy-soft` 出现 `signal=upstream_truncate`，连击 2 即隔离 |
| 2 | 断流 + 连接失败混合 | 连击合并计数，2 次即隔离 |
| 3 | 隔离到期后首次失败 | 直接升档（strikes 2、60 分钟），不回到「从 1 数」 |
| 4 | 隔离到期后一次完整成功 | `Strikes=0`、`Probation=false` |
| 5 | 到期后长时间静默（<24h）再来失败 | **仍按观察期升档**（替换原「静默归零」用例） |
| 6 | `ZCODE_PROXY_SOFT_PROBATION=0` | 恢复旧行为（到期回池 + 静默清零） |
| 7 | `ZCODE_PROXY_SOFT_TRUNCATE=0` | 断流不记账，只走 line-guard |
| 8 | 断流触发熔断 | 不产生孤儿软失败条目 |
| 9 | `Warn` 行 | 以 ` ts=` 结尾且可被 `time.Parse(RFC3339)` 解析 |
| 10 | 既有回归 | 连续失败隔离、成功清零、续期封顶、持久化、开关关闭全部不变 |
| 11 | 观察期条目静默超过 24h | 被 `pruneProxySoftLocked` 清理（重新建档从 1 计），与规格一致 |

## 五、验收（用本次日志回放）

- 同一条线重演：17:54 连接失败 → 17:57 断流即隔离 → 18:28 进观察期 → 18:48 断流立即升档 60 分钟 →
  **19:00 与 19:01 不再发生**（可见失败 3 → 2）；
- `proxy-soft` 行出现 `trigger=probation`；
- 所有 `[~]` / `[!]` 行带 `ts=`，可用一条 grep 直接排出时间线；
- 正常线（成功一次）不会被观察期误隔离：观察期内成功即清零。

## 六、风险与回滚

| 风险 | 处置 |
|---|---|
| 上游自身抖动被记到线路上 | 需连续 2 次才隔离；观察期需成功才清零；`ZCODE_PROXY_SOFT_TRUNCATE=0` 可关 |
| **上游全局掐流风暴**（9-21 事故型：所有线同时断） | A1 在该场景仍是**净改善**：现状 `line-guard` 3 连击是**不可逆删除**，A1 把止损提前到 2 连击且**可逆**（隔离≠删除，成功一次即清零）。级联风险由既有 relax 兜底：`proxySelectionBlockedLocked` 汇总隔离线路，`chooseProxyLocked` 在所有候选都被挡住时回落到 eligible 并记 `relax=1`，池子不会被隔离清空。观测信号：`proxy-soft action=quarantine` 日志速率异常 + `relax=1`；应急开关 `ZCODE_PROXY_SOFT_TRUNCATE=0`（只停断流记账，其余判据不变） |
| 删除静默清零后旧一次性坏线长期带 strikes=1 | 影响仅限「下次失败直接升档」，不阻断使用；成功即清零；24h 无流量由 TTL 清理 |
| 观察期线路仍可被选中，首败仍会打断一个请求 | 这是刻意取舍：不做「到期先探测」的出口闸（留 v2.10.27），避免把简单改动做成探测调度 |
| 日志行变长 | 只在行尾加一个字段，按 `key=value` 取值不受影响 |

## 七、验证流程（按 AGENTS.md「本地开发，远端验证」）

1. 本地只做代码修改、`gofmt -l` 与差异检查；**不跑 `go test`**（本机性能与环境噪声不作为验收依据）。
   仅允许秒级的单包 `go build` / `go vet` 用于捕捉语法错误，不替代测试。
2. 提交并推送到 `go-rewrite`，用 `gh run watch <id> -R fujunchao/zcode2api-plus --exit-status >/dev/null; echo $?`
   等待分支 CI（注意不要接管道，`$?` 会被吞）。
3. **以当前 SHA 的完整 CI 结果为准**，旧提交的成功记录、单个 job 通过都不算。
4. 失败时读 `gh run view <id> --log-failed` 定位，修复后重新推送复验；不得删除断言或降低覆盖。
5. 发布（提交 4）**仅在用户明确要求发版时执行**，且必须在当前 SHA 的 CI 全绿之后；打 tag → `release.yml`
   复用完整 CI → 核对 5 个附件与 GHCR 镜像后才算完成。

## 八、提交拆分

| # | 提交 | 内容 |
|---|---|---|
| 1 | `feat: 断流计入线路软失败` | 信号常量 + classify 记账 + 测试 1/2/7/8 |
| 2 | `feat: 软失败隔离到期进入观察期` | Probation 字段与状态机 + 移除静默清零 + 测试 3/4/5/6 |
| 3 | `feat: 后台日志行补时间戳` | web/logs + 测试 9 |
| 4 | `chore: 发布 v2.10.26-go` | 版本号 + release notes + README（**条件执行**：仅在用户明确要求发版、且提交 1–3 的 CI 全绿后） |

## 九、明确不做

- **不做「隔离到期先跑流式探测」的出口闸**：需要新增探测调度、预算与状态机，留 v2.10.27。
  另外要说清：合成探测也不是保证——本次断流是间歇性的（19:01 那次传了 62 KB 才断），
  探测可能恰好落在它正常的窗口里。它的价值是增加一道证据，不是消灭第二次失败。
- 不改 line-guard 的 3 次熔断（保留为观察期之后的升级手段）。
- 不改 405 与断流的合并判据（v2.10.27 再议「线路健康分」）。
- 不动 P3 带宽选线、P4 内容级 405 连击折扣。
