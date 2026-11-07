# 第二批实施计划：让系统认得出「慢」

- 版本基线：`dc5ffe4`（v2.12.0-go）
- 编写时间：2026-11-07
- 前置：第一批（改派 + 首字节预算 90s→25s）已执行，见 `change-20261107-ttft-p0.md`

---

## 0. 先纠三条判断（避免做无用功）

第一批之前的三份报告里，有三条结论在细读代码后**需要修正**。如果按原结论实施，会做三处无用功甚至有害改动。

### ❌ 纠正 1：429「不写 avoid」不是缺陷，是对的

原判断：「429 不写 avoid，同号换线后照样能被选中」。

**实际**：`engine.go:276` 有 `tried[acc.ID] = true`，在每次尝试**之前**就标记。
下一轮 `SelectAvoiding(provider, tried, modelName, avoid)` 会跳过该账号。

- `tried` = **账号级**回避（本请求不再选这个号）
- `avoid` = **线路级**回避（本请求不再选这条线）

429 是账号级限流，正确做法就是「不回避线路、只回避账号」——现状已经如此。
**→ 这项不改。**

### ❌ 纠正 2：首字节超时「不冷却账号」是有意设计，不该简单改成冷却

代码注释（`engine.go:556-563`）明确写了原因：

> 24 个账号共享同一条慢线时，逐个冷却会瞬间清空池子。

而且它已经做了正确的事：`avoid: watchdog.Identity()` —— 把**线路**加入本请求回避集合。

**真正的问题不是「没冷却账号」，而是「`avoid` 是请求级的」**：
`avoid` 是 `RunMessages` 里的局部 map，下一个请求来时会重新从空开始，
于是又可能选到同一个慢线路上的账号。

**→ 改成冷却账号是错的方向（会清空池子）；正解是让「线路慢」跨请求持久化。**

### ❌ 纠正 3：`Slow` 排序不是独立改动项

`Slow`（首字节 >30s 但成功）与「上行带宽」不等价：
- 带宽慢 → 一定导致首字节慢
- 上游排队慢 → 首字节慢，但带宽可能很快

如果同时做「Slow 排序」和「带宽门槛」，两者会互相覆盖，且都退化成"收窄到最优一组"。
**→ 并入 P1-A 统一用带宽做门槛，`Slow` 保留为观测指标，不单独做排序。**

### ✅ 那真正要修的是什么

一句话：**慢的信息从来没有跨请求活下来。**

| 现状 | 后果 |
|---|---|
| `avoid` 请求级，请求结束即消失 | 下个请求重新踩同一条慢线 |
| `Slow` 只写不读 | 慢样本纯占内存 |
| 深探针 `shadow` 只记录不改判定 | 测出来也不影响任何决策 |
| 订阅每 15 分钟导入 50 条新线 | 新线未测速就被 `leastLoadedProxy` 均匀分配 |

---

## 1. P1-A：线路带宽状态持久化 + 选路门槛（核心）

### 1.1 为什么不是「改 leastLoadedProxy」

`leastLoadedProxy`（`proxy_selection.go:45`）只在**建号 / 删线 / 隔离改派**时决定账号绑哪条线，
**不在请求派发路径上**。请求派发走的是 `selectAccount`（`store.go:1914`），
它只看账号当前绑定的线路「是否启用且身份一致」，不看带宽。

所以要改的是 `selectAccount`，不是 `leastLoadedProxy`。
（`leastLoadedProxy` 可以在 P2 再改，用于"绑定时刻"的质量，属加分项。）

### 1.2 新增：`internal/store/proxy_uplink.go`

```go
// ProxyUplink 是一条线路的上行带宽观测状态；只保存去敏身份，不含凭据。
// 数据来源**只能是真实请求的实测 up_kbps**，不能是探针 kbps——
// 实测同一条线探针报 2547、真实 617（高估 4x），报 34、真实 2148（低估 63x），
// 方向都不确定（见 change-20261107-ttft-p0.md）。
type ProxyUplink struct {
    Identity   string json:"identity"
    ProfileID  string json:"profile_id"
    Generation uint64 json:"generation"
    // KBps 是指数加权移动平均；Sample 是累计样本数（用于判断可信度）。
    KBps    float64 json:"kbps"
    Sample  int     json:"sample"
    LastAt  int64   json:"last_at"
}

const (
    proxyUplinkKey    = "proxy_uplink_state"
    proxyUplinkLimit  = 4096
    proxyUplinkTTL    = 6 * time.Hour   // 6h 无新样本即淘汰（免费代理寿命以分钟计）
    proxyUplinkAlpha  = 0.3             // EWMA 平滑系数：新样本权重，兼顾抗抖与跟随
    proxyUplinkMinN   = 2               // 少于 2 个样本不给门槛判定，避免单次抖动误伤
)
```

**接口**：

```go
// RecordProxyUplink 记录一次真实请求的实测上行速率，更新 EWMA。
// kbps<=0 或 e.Mode!="proxy" 时忽略（直连不参与）。
func (s *Store) RecordProxyUplink(e ProxyEgress, kbps float64, now time.Time)

// ProxyUplinkSnapshot 返回 identity → KBps 的只读快照，供选路使用。
// 调用方不得持有返回的 map（并发安全靠副本）。
func (s *Store) ProxyUplinkSnapshot() map[string]float64

// ProxyUplinkKBps 返回单条线路的 EWMA 带宽；无样本返回 (0,false)。
func (s *Store) ProxyUplinkKBps(identity string) (float64, bool)
```

**存储方式**：与 `proxy_soft_failure.go` 同构（`meta` 表存 JSON、`s.mu` 保护、TTL 淘汰、上限截断）。

### 1.3 写入点

在 `internal/gateway/engine.go` 的 `client.Do` 返回后、已有 `diag.UpKBps` 赋值处附近（约 527 行）：

```go
if uplink != nil {
    if kbps := uplink.RateKBps(); kbps > 0 {
        e.Store.RecordProxyUplink(egress, kbps, e.now())
    }
}
```

**注意**：
- 只在**成功或已采样**时写，`RateKBps()==0`（未达 `UplinkGateMinBytes` 门槛）不写
- 同时要在 `asyncpool/pool.go` 的对应位置（约 848 行 `diag.UpKBps = uplink.RateKBps()`）做同样处理，
  两条路径对带宽必须用同一份数据，否则 sync/async 的选路会分叉

### 1.4 读取点：`selectAccount` 加一层「带宽门槛」

插入位置：**在「额度优先」层之前、「promo 门槛」之后**。

理由：
- 必须在前面的资格/模型/到期/promo 层之后（那些是硬语义，不能为了带宽跳过）
- 必须在「额度优先」之前（否则额度层会把候选收窄，带宽门槛看不到全局）
- 必须在「轮询」之前（组内仍然轮询，保持负载均衡）

```go
// 带宽门槛：只保留「绑定线路上行带宽」处于最高档的账号。
//
// 为什么是门槛而不是排序：与上面的到期门槛同构——按带宽排序再取前 N 个会把
// 流量压到少数账号（zcode-pool 的 take(4) 就是热点来源）。这里只收窄候选，
// 组内仍由下方游标轮询，保持负载均衡。
//
// 为什么要分档而不是比大小：EWMA 会小幅浮动，逐值比较会让同一批账号
// 每小时换一次「最优组」，反而抖动。分档把差异吸收掉。
//
// 没有带宽样本的账号（新线路、样本不足）归入「未知档」，排在已知快档之后
// 但不是最末——它们可能真的快，只是还没测到。
if uplink := s.proxyUplinkSnapshotLocked(); len(uplink) > 0 {
    // 档位：≥600KB/s(快) / ≥150(中) / 其余(慢) / 无样本(未知)
    // 门槛值来自实测：600KB/s 传 324KB 只需 0.54s；150KB/s 需 2.2s；
    // 低于 150 的线路单次上传就会吃掉 2s 以上，加上排队必然进入慢请求区。
    best := -1
    for _, a := range pool {
        band := uplinkBandOf(uplink[s.boundLineIdentityLocked(a)])
        if band > best { best = band }
    }
    if best >= 0 {
        var kept []*model.Account
        for _, a := range pool {
            if uplinkBandOf(uplink[s.boundLineIdentityLocked(a)]) == best {
                kept = append(kept, a)
            }
        }
        if len(kept) > 0 { pool = kept }
    }
}

func uplinkBandOf(kbps float64) int {
    switch {
    case kbps >= 600: return 3
    case kbps >= 150: return 2
    case kbps > 0:    return 1
    default:          return 0   // 无样本
    }
}
```

**关键约束**：`boundLineIdentityLocked` 已有（`selectAccount` 里 405 隔离层就在用），直接复用，不要新增取路线的路径。

### 1.5 回归测试（按项目规范，本地不跑，靠 CI）

新增 `internal/store/proxy_uplink_test.go`：

| 测试 | 断言 |
|---|---|
| `TestRecordProxyUplinkEWMA` | 连续两次 100/300 → 第二次后 KBps = 100*0.7+300*0.3 = 160 |
| `TestRecordProxyUplinkIgnoresDirectAndZero` | `Mode="direct"` 与 `kbps=0` 不建档 |
| `TestProxyUplinkTTLPrune` | 6h 前的样本被淘汰 |
| `TestProxyUplinkLimitTruncate` | 超过 4096 条按最旧淘汰 |
| `TestSelectAccountPrefersFastLine` | 3 个账号分别绑 800/200/30KB/s → 选到 800 那个 |
| `TestSelectAccountFallsBackWhenNoSamples` | 全部无样本时不收窄（行为与改造前一致） |
| `TestSelectAccountBandKeepsRotation` | 同档 3 个账号连续选 6 次，每个被选 2 次（组内仍轮询） |

新增 `internal/gateway/uplink_record_test.go`：验证 `client.Do` 后 `RecordProxyUplink` 被调用且值正确。

**⚠️ 必须避免的测试陷阱**（来自项目既有教训 `TestTokenPoolIdleShrinkAndStop`）：
不要用「宽参数」让测试躺过。例如带宽门槛测试必须用**真实差异明显**的值（800 vs 30），
不能用 620 vs 600 这种边界值——那样测不出分档是否生效。

---

## 2. P1-B：导入后测速（订阅侧）

### 2.1 问题

订阅每 15 分钟拉一次，`import_limit: 50`。新导入的线路**从没被测过速**，
就被 `leastLoadedProxy` 按「占用数最少」均匀分配给账号。

P1-A 的带宽门槛能救一部分（新线无样本 → 归入未知档 → 排在快档之后），
但**未知档的线路仍然会被派发**（当没有已知快线时）。

### 2.2 方案

在 `internal/proxysub/manager.go` 的导入流程**之后**，对新增线路排队做一次带宽探测。

**复用现成的探测设施**，不要新写一套：
- `internal/networkprobe`（已有隧道建连 + 写入测量）
- `internal/modelprobe` 的并发批次调度（`networkBatch` 的 sem=8 模式）

**注意**：探测必须用 P1-C 改造后的**准确**测法，否则等于把错误的数字写进 P1-A 的状态表，
比不测更糟。

### 2.3 成本控制（重要）

- **只测新导入的线路**，不重复测已有的
- 每条 4MiB（P1-C 改造后的载荷），50 条 = 200MB/轮
- 加并发上限（建议 4），避免打满带宽影响真实请求
- 失败不阻断导入（测不到就留空，走未知档）

### 2.4 测试

- `TestImportProbeQueuedForNewOnly`：重复导入同一批时不重复排队
- `TestImportProbeFailureDoesNotBlockImport`：探测失败，线路照常入库
- `TestImportProbeRespectsConcurrency`：并发不超过上限

---

## 3. P1-C：把探针改准

### 3.1 现状问题

`internal/networkprobe/probe.go:66` 的 `CheckTunnelWrite`：
- 载荷 1MiB（默认 `DeepBytes=1048576`）
- `Kbps = Bytes*1000/elapsedMS/1024` —— **把内核缓冲吸收算进了速率**

实测偏差：同一条线高估 5.2×、10.8×，也低估 5.4×，**方向都不确定**。

### 3.2 改法

```go
// 稳态速率：跳过前 skipBytes 的缓冲吸收段。
// 实测所有线路前 ~128KB 会瞬间写进内核缓冲（0.2-0.3s），之后才断崖到真实链路速率。
// 324KB 的请求体恰好整段落在断崖之后，所以必须测断崖之后的速率。
func steadyKbps(marks []mark, skipBytes int64) (float64, bool)
```

参数调整（`internal/config/config.go`）：

| 项 | 现值 | 建议 | 理由 |
|---|---:|---:|---|
| `DeepBytes` | 1 MiB | **4 MiB** | 1MiB 落在突发窗口内，缓冲占比过大 |
| `DeepSkipBytes` | — | **128 KiB** | 新增，稳态统计起点 |
| `DeepBudget` | 12s | **30s** | 4MiB 在 150KB/s 线上需 27s |
| `DeepMinBytes` | 128 Ki | **512 Ki** | 与 4MiB 载荷配套 |

**⚠️ 现有注释明确警告**：「不要单独下调 DeepBytes 来省流量：满写分支不受 kbps/ms 约束，
1MiB/12s 就是 85KB/s 的隐式速率底线。降 size 会让『突发后卡死』的线路蒙混过关」。
本方案是**上调**，且显式引入 `DeepSkipBytes` 后，`DeepMinKbps` 的语义从
"隐式底线"变成"显式判据"，需要同步更新该注释。

### 3.3 确认轮也要跑 deep

`internal/modelprobe/network.go:310`：

```go
second := m.networkBatch(ctx, append(slices.Clone(bad), controls...), p, r.targets, false)
//                                                                              ^^^^^ deep=false
```

后果：即使 `ZCODE_PROXY_DEEP_PROBE=enforce`，第二次确认轮只跑浅探针，
浅探针 OK 就判 `recovered` —— 慢线路永远不会被 enforce 退掉。

改为 `true`。**但这会显著增加巡检流量**（每条确认线 2×4MiB），
需要在 `withDeepProbe` 里加"仅 enforce 模式才在确认轮跑 deep"的条件。

### 3.4 测试

- `TestSteadyKbpsSkipsBufferWindow`：构造 marks（前 128KB 极快、之后慢）→ 应报慢值
- `TestSteadyKbpsInsufficientSamples`：marks 不足时报 `(0,false)`
- `TestConfirmRoundRunsDeepOnlyInEnforce`：shadow 模式确认轮不跑 deep

---

## 4. P2：日志补 `frt_total`

### 4.1 问题

`[#]` 行的 `firstbyte=` 只是**最后一次**尝试的耗时。
实测极端案例：日志写 `firstbyte=1474ms`，用户实际等了 **121965ms**（6 次尝试）。
这会让所有看日志的人误判为"一切正常"。

### 4.2 改法

`internal/gateway/diagnostics.go` 的 `ReqDiag` 增加：

```go
// AttemptsElapsed 本请求所有出站尝试的累计耗时（Σ 每次 client.Do 的墙钟）。
// 与 FirstByte 的区别：FirstByte 只等于**最后一次**尝试；用户实际等待接近这个值。
// 实测 r(frt_total, 用户感知首字)=0.9945，而 r(firstbyte, 用户感知)=0.5774。
AttemptsElapsed time.Duration
```

在 `tryAccount` 的 attempt 收尾处累加，在 `Format()` 里输出 `frt_total=`。

### 4.3 测试

- `TestDiagAccumulatesAttemptsElapsed`：3 次尝试 10/20/30ms → `AttemptsElapsed=60ms`
- `TestDiagFormatIncludesFRTTotal`：`Format()` 含 `frt_total=`

---

## 5. 实施顺序与依赖

```
P1-C（探针改准）──┐
                  ├──> P1-B（导入后测速）
P1-A（带宽状态）──┘         │
   ↑                        │
   └────────────────────────┘  （B 的测量结果写入 A 的状态表）

P2（日志）独立，随时可做
```

**建议顺序**：

| 步骤 | 内容 | 理由 |
|---|---|---|
| 1 | **P1-C 探针改准** | 一切测量的基础；不先做，后面都在错误数据上建 |
| 2 | **P1-A 带宽状态 + 选路门槛** | 核心价值；用真实请求采样，不依赖探针 |
| 3 | **P2 日志 frt_total** | 小改动，但能让后续所有验证可信 |
| 4 | **P1-B 导入后测速** | 依赖 C 的准确测法；成本最高，最后做 |

**P1-A 可以不等 P1-C 先上**——它用的是真实请求的 `up_kbps`（uplinkMeter 在 body≥256KB
且上传≥1s 时采样，324KB 走 TLS 真实上传，远超 128KB 缓冲，数据可信），与探针无关。

---

## 6. 分支与验证流程（按 AGENTS.md）

1. 分支：`codex/proxy-uplink-aware-selection`（从 `go-rewrite` 切出）
2. **本地只做**：`gofmt`、`go vet`、代码阅读、diff 检查
3. **不本地跑** `go test`、竞态检测
4. 推送后由 GitHub Actions 跑完整 CI，以**当前提交 SHA 的 CI 结果**为验收依据
5. 失败读远端日志修复，不删测试、不降覆盖率
6. 只推 `origin`，不推 `upstream`

---

## 7. 风险与回滚

| 风险 | 缓解 |
|---|---|
| 带宽门槛让候选池过窄 → `no_available_account` | 门槛是**分档收窄**不是硬过滤；无样本归未知档；若收窄后为空则**回落到收窄前**（必须写这个兜底，并加测试） |
| EWMA 被单次抖动带偏 | `alpha=0.3` + `Sample>=2` 才参与判定 |
| 记录带宽引发出站热路径锁竞争 | `RecordProxyUplink` 只在尝试收尾调用一次，且复用 `s.mu`（与现有软失败一致） |
| 探针流量翻倍（4MiB × 确认轮） | 确认轮仅在 enforce 跑；`DeepBudget` 上调但并发仍 8 |
| 导入后测速拖慢订阅 | 异步排队，不阻塞导入；失败即放弃 |

**回滚开关**（都是配置级，不需回滚代码）：

```bash
ZCODE_SELECT_UPLINK_BAND=false     # 关闭选路带宽门槛（P1-A）
ZCODE_PROXY_DEEP_PROBE=shadow      # 探针退回只记录（P1-C）
ZCODE_IMPORT_PROBE=false           # 关闭导入后测速（P1-B）
```

---

## 8. 验收标准

| 指标 | 现状 | 目标 | 取数方式 |
|---|---|---|---|
| `>30s` 占比 | 12.9% | **<2%** | new-api `frt` 分布 |
| `>10s` 占比 | 31.1% | **<8%** | 同上 |
| 平均 frt | 13692ms | **<4000ms** | 同上 |
| 账号绑定线路带宽 | 40/40 ≥605（第一批后） | **维持 ≥600** | `ProxyUplinkSnapshot` |
| 慢线路被选中的比例 | 未统计 | **<5%** | 选路日志加打 `uplink_band=` |
| 日志可观测性 | `firstbyte` 只报末次 | **补 `frt_total=`** | 抽查 `[#]` 行 |

---

## 9. 明确不做（附理由）

| 项 | 理由 |
|---|---|
| **429 写 avoid** | 已有 `tried` 做账号级回避（`engine.go:276`），再多写 avoid 会误伤线路 |
| **首字节超时冷却账号** | 代码注释明确解释「共享慢线会清空池子」；正解是持久化线路带宽（P1-A） |
| **`Slow` 单独做排序** | 与带宽门槛功能重叠，两者叠加会互相覆盖；保留为观测指标 |
| **改 `leastLoadedProxy` 做带宽感知** | 它不在派发路径上；P1-A 改 `selectAccount` 才有用。可在 P2 作为"绑定时刻"加分项 |
| **请求体压缩 / 瘦身** | `plan-attempt-ttfb-20261017.md:221` 已否决 |
| **HTTP/2** | 上游 `zcode.z.ai` ALPN 只给 http/1.1 |

---

## 10. 一句话

**第二批不是"加几个 if"，而是把「慢」从请求级提升到系统级。**

现在的系统：慢 → 本次请求记住 → 请求结束 → 忘掉 → 下个请求再踩一次。
改完之后：慢 → 写进线路状态 → 所有后续请求都看得见 → 选路自动避开。

这比"冷却账号"更安全（不清空池子），比"Slow 排序"更准（直接量带宽）。
