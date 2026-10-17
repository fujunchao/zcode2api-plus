# 实施计划：首字节预算与换线重试（P0 + P1）

> 依据：`docs/analysis-long-stall-20261017.md`（v2.10.15-go 线上 17:50 复盘）
> 目标版本：**v2.10.16-go**（P0 + P1 一起，一个版本）
> 状态：**待批准实施**

---

## 一、要解决的问题

用户侧「一直在跑、不回消息」的直接机制：

| 事实 | 数据 |
|---|---|
| 请求体 5.59MB，正常线路首字节 30–65s | 订阅线路 5.41MB → 30.5s / 63.6s / 42.3s |
| 默认出口 `home-epg-ipv6` 同样体积要 279–296s | 8 个请求里 6 个 >300s |
| 网关在 body 写完前没有超时 | `ResponseHeaderTimeout` 是「写完 body 之后」才计时 |
| 524 原样透传，不换号不重试 | `engine.go` 第 9 分支 |

四件事合起来 = 用户盯着转圈 5 分钟。

## 二、设计总览

```
发起请求 ──► [首字节预算 90s] ──超时──► 换号 + 记住这条线路（avoid）
                │                        └─► 下次选号跳过绑定该线路的账号
                │
                └─收到响应头──► 正常流式交付（不再计时）
                        │
                        └─408/524──► 换号重试 1 次（记住 avoid）
```

四条规则：

1. **预算只覆盖"发起到收到响应头"**，收到响应头就停表——绝不限制长流。
2. **超时不惩罚账号，只避开线路**：账号不冷却（不是账号的错），线路进入本请求的 avoid 集合。
3. **重试必须换线**：只换号会落到同一条慢线上再等一个 90 秒。
4. **慢≠坏**：首字节 30–90s 的成功请求只记"慢样本"，不计入软失败率（否则会把还能用的线路隔离掉）。

## 三、详细设计

### 3.1 首字节看门狗（P0-1）

新增 `internal/gateway/firstbyte.go`：

```go
type firstByteWatchdog struct {
    ctx    context.Context
    cancel context.CancelCauseFunc
    timer  *time.Timer
    fired  atomic.Bool
    identity string   // 本次出站线路 identity，用于 avoid
}

func newFirstByteWatchdog(parent context.Context, timeout time.Duration, identity string) *firstByteWatchdog
func (w *firstByteWatchdog) Headers(*http.Response)  // 收到响应头 → Stop 计时
func (w *firstByteWatchdog) Stop()                   // 尝试结束时释放
func (w *firstByteWatchdog) Fired() bool
func (w *firstByteWatchdog) Identity() string
```

- 用 `context.WithCancelCause`，超时时 `cancel(errFirstByteTimeout)`，让 `client.Do` 立刻返回。
- 计时起点 = 出站调用前（含建连与上传），终点 = `client.Do` 返回响应头。
- `Headers()` 只停表、不取消：流式交付继续用同一个 ctx。
- `Stop()` 在 `defer release()` 旁（尝试真正结束时）调用，避免流未结束就取消。

### 3.2 超时后的处置（P0-1）

在 `tryAccount` 的 `client.Do` 错误分支最前面加：

```go
if watchdog.Fired() {
    ObserveModelAttemptTimeout(attemptCtx, e.now())          // 记一次 transport_timeout 软失败
    e.recordError(acc, model.ErrorKindConnectionFailed, "上游首字节超时")  // 只留痕，不冷却账号
    web.Warn(reqID, fmt.Sprintf("上游 %s 内未返回首字节（账号 %s，线路 %s），换线重试", timeout, acc.Name, egress.Label))
    return attemptResult{switchAccount: true, avoid: watchdog.Identity()}
}
```

要点：
- **不调用 `e.mark(Cooling)`**：24 个账号绑同一条慢线时，逐个冷却会瞬间把池子清空。
- `ObserveModelAttemptTimeout` 复用既有 `observeModelAttemptHealth(kind="timeout")`，让线路落一次软失败（3 次即隔离）。

### 3.3 524 / 408 有界换号（P0-2）

`attemptBudget` 增加 `timeout int`，新增常量 `MaxTimeoutRetries = 1`。

`handleUpstreamError` 第 9 分支之前插入：

```go
if resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == 524 {
    if b.timeout < MaxTimeoutRetries {
        b.timeout++
        web.Warn(reqID, fmt.Sprintf("上游 HTTP %d（账号 %s），换线重试（第 %d 次）", resp.StatusCode, acc.Name, b.timeout))
        return attemptResult{switchAccount: true, avoid: egressIdentity}
    }
}
```

保留原「用尽后原样透传」的行为。

### 3.4 请求级 avoid 集合（P1）

- `attemptResult` 增加 `avoid string`（线路 identity）。
- `RunMessages` 维护 `avoid := map[string]bool{}`；每次 `switchAccount` 且 `res.avoid != ""` 就写入。
- 选号改为 `e.Store.SelectAvoiding(model.ProviderZai, tried, modelName, avoid)`。
- `SelectAvoiding` 内部：先用 avoid 过滤账号（按 `strictProxyIdentity(a.ProxyURL)`），若过滤后为空则**回退到不过滤**——宁可再慢一次，也不能报「无可用账号」。

这样「换号」在有多条线路时自动变成「换线」；只有全池共用一条线路时才退回原行为。

### 3.5 慢样本（P1，口径修正）

原提法是「首字节 >30s 记一次软失败」。**这条要改口径**：一次 30–90 秒出首字节、最终成功的请求，线路是能用的；把它记成失败会让成功率虚低，3 次就把可用线路隔离掉。

改为：
- 看门狗同时记录 `firstByte` 时长；成功交付且 `firstByte > 30s` → `RecordProxySlowSample`（只加 `Slow` 计数，不动 `Failures`）。
- 后台 `soft` 摘要里多一个 `slow` 字段，用于后续「同分选线时优先快线」的排序依据。
- **不参与隔离判定**。

## 四、代码改造点

| 文件 | 改动 |
|---|---|
| `internal/config/config.go` | `UpstreamFirstByteTimeout`（默认 90s，0 关闭）、`UpstreamTimeoutRetries`（默认 1） |
| `internal/gateway/firstbyte.go`（新） | 看门狗 + `errFirstByteTimeout` |
| `internal/gateway/engine.go:147` | `attemptResult` 增 `avoid string` |
| `internal/gateway/engine.go:159` | `attemptBudget` 增 `timeout int` |
| `internal/gateway/engine.go:225` | `Select` → `SelectAvoiding`，维护 `avoid` 集合 |
| `internal/gateway/engine.go:399` 附近 | `BeginProxyAttempt` 后用看门狗包 ctx；`client.Do` 后 `Headers(resp)` |
| `internal/gateway/engine.go:427` | 错误分支最前面加 `Fired()` 判定 |
| `internal/gateway/engine.go:725` 附近 | 524/408 有界换号 |
| `internal/gateway/proxy_observation.go` | `ObserveModelAttemptTimeout`；`ConfirmProxyAttemptResponse` 记录 `firstByteAt` |
| `internal/store/proxy_egress.go` | `func (e ProxyEgress) Identity() string` |
| `internal/store/store.go:1739` | `SelectAvoiding`（`Select` 保持原签名，内部复用 `selectAccount` 增加 avoid 参数） |
| `internal/store/proxy_soft_failure.go` | `Slow int` 字段 + `RecordProxySlowSample` + 摘要输出 |

> `Select` 只有一个调用点（`engine.go:225`），改成 `SelectAvoiding` 不影响异步池——异步池另有一套选号，本版不动。

## 五、参数

| 变量 | 默认 | 说明 |
|---|---|---|
| `ZCODE_UPSTREAM_FIRST_BYTE_TIMEOUT` | `90` | 秒；0 = 关闭看门狗（回到旧行为） |
| `ZCODE_UPSTREAM_TIMEOUT_RETRIES` | `1` | 408/524 换线重试次数；0 = 关闭 |

90s 的依据：实测正常大包首字节 30–65s，283–296s 是故障。取 90 秒既不误杀 63s 的合法大包，也能把 5 分钟的等待压到 90 秒。

## 六、日志与判读

```
[~] <req> 上游 90s 内未返回首字节（账号 xxx，线路 home-epg-ipv6），换线重试
[~] <req> 上游 HTTP 524（账号 xxx），换线重试（第 1 次）
[~] proxy-soft id=proxy-xxx signal=transport_timeout failures=3 attempts=4 rate=75% action=quarantine …
```

- 「换线重试」出现后仍失败 → 说明全池都慢，属于容量问题，不是单条线路问题。
- `signal=transport_timeout` 现在包含首字节超时，不再只来自 `client.Do` 超时。

## 七、测试清单

**gateway（`firstbyte_test.go` 新）**
1. 上游 200ms 后回响应头 → 看门狗不触发，流正常读完
2. 上游永不回响应头 → 90s（测试用 100ms）触发，`Fired()=true`，`client.Do` 返回错误
3. 看门狗触发后走「换线重试」：账号**不**进入冷却（`Status` 不变）
4. 触发时记一条软失败（`transport_timeout`），线路进入隔离计数
5. `ZCODE_UPSTREAM_FIRST_BYTE_TIMEOUT=0` → 看门狗关闭，行为与旧版一致
6. 流式响应持续 3s（远大于测试阈值）→ 不被看门狗打断（证明只覆盖首字节前的阶段）

**gateway（重试）**
7. 首次 524 → `switchAccount`，且第二次 524 → 原样透传（预算 1）
8. 首次 408 → 同上
9. 524 换号后成功 → 用户拿到 200

**store（选号）**
10. `SelectAvoiding` 跳过绑定在 avoid 线路上的账号
11. 全部账号都在 avoid 线路上 → 回退到不过滤，仍返回账号（不能返回 nil）
12. `Select` 行为不变（avoid 为 nil 时与旧路径等价）

**回归**
13. 普通连接失败仍走既有 `mark(Cooling)` 路径（不被新分支改写）
14. 客户端取消仍然不换号、不冷却

## 八、提交与发布

| # | 提交 | 版本 |
|---|---|---|
| 1 | `feat: 首字节预算与请求级换线重试` | v2.10.16 |
| 2 | `feat: 记录慢首字节样本供选线排序` | v2.10.16 |
| 3 | `chore: 发布 v2.10.16-go` | — |

CI 全绿 → 附注 tag → 核对 Release 附件与 GHCR（`check-ghcr.js 2.10.16-go`）。

## 九、风险与回滚

| 风险 | 对策 |
|---|---|
| 90s 误杀合法慢请求（63.6s 的观测距阈值不远） | 环境变量可调到 120s；上线后看「换线重试」里成功比例 |
| 24 个账号同线，一个个换号各等 90s | avoid 集合让第二次起直接换线；最坏 1 次超时而非 5 次 |
| 突然大面积触发 → 看起来像故障 | 日志只打一行 Warn，不影响既有错误分类；`Fired()` 分支不冷却账号 |
| 524 重试放大上游压力 | 预算固定 1 次，且只对 408/524 |
| 慢样本被误用成失败 | `Slow` 与 `Failures` 完全分离，参与隔离的唯一字段仍是 `Failures` |

回滚：`ZCODE_UPSTREAM_FIRST_BYTE_TIMEOUT=0` + `ZCODE_UPSTREAM_TIMEOUT_RETRIES=0` 可分别关掉两条新行为。

## 十、验收标准

1. 第七节 14 项测试全绿，CI 通过。
2. 复现本次故障形态（5.5MB 经慢线路）：最坏 **90 秒**返回错误或换线成功，不再出现 300 秒无输出。
3. 上线 24h：
   - `换线重试` 之后成功的比例 > 30%（说明换线有效）；
   - 524 的用户可见次数下降（基线：37 分钟 3 次）；
   - 账号冷却数量不因新分支增加（`pool` 的 cooling 数保持平稳）。
4. `ZCODE_UPSTREAM_FIRST_BYTE_TIMEOUT=0` 回滚后行为与 v2.10.15 完全一致。

## 十一、不做什么

- **不给流式交付设总时长上限**：长回答本来就该长，预算只覆盖首字节之前。
- **不改 `IsEndpointUnreachable`**：首字节超时走独立的 `ObserveModelAttemptTimeout`。
- **不动异步池的选号**：`/async/v1/messages` 有独立工单与超时语义，另开一版。
- **不把慢线隔离成坏线**：`Slow` 只做观察与排序输入。
- **不做请求体压缩/裁剪**：5.5MB 从哪来是客户端侧的问题，网关不该改写用户内容。
