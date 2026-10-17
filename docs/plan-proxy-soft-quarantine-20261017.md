# 实施计划：代理软失败隔离 → 探针升级 → 自动停用

> 依据：`docs/analysis-proxy-health-20261017.md`（v2.10.14-go 线上 16.2h 复盘）
> 决策：**先 C 后 B** —— 先让复核能看见"能连但跑不动"，再对探针仍漏掉的线路做停用兜底。
> 状态：**待批准实施**（本文只描述方案，未改代码）

---

## 零、阶段总览

| 阶段 | 内容 | 解决什么 | 独立上线 | 版本 |
|---|---|---|---|---|
| **A** | 采集软失败 + 持久化隔离状态 | 让 408/524 第一次进入"证据"视野 | ✅ 只写状态与日志 | v2.10.15-go |
| **B** | 隔离生效：退出选线 + 立即改派 + **新导入线路冷启动预检** | 坏线路不再被新分配/换线命中，止损；新线路不再"未体检先上岗" | ✅ | v2.10.15-go |
| **C** | 全量巡检定时器修复（P1-1） | 空闲坏线也能被 30 分钟全量扫到 | ✅ | v2.10.15-go |
| **D** | 健康摘要暴露隔离状态（可选） | 面板可见、可解释 | ✅ | v2.10.15-go |
| **E** | 深度探针（吞吐档）：先 shadow 后 enforce | 让**删除链**看见 408/524 类故障 | ✅ 分两步 | v2.10.16-go |
| **F** | `soft_retired`：长隔离自动停用 | 兜住 E 仍漏掉的间歇性坏线 | ✅ 需 E 的 24h 数据 | v2.10.17-go |

> 原"选项 C"= 阶段 E，原"选项 B"= 阶段 F。先 E 后 F：E 让删除链自己看见故障，F 只处理"探针也测不出来"的残余。

## 一、四条不可违反的红线

1. **删除仍然要双证**：软失败只产生"隔离"，绝不写删除证据；删除只由 `ApplyNetworkHealth → confirmed_failure` 触发。
2. **隔离 ≠ 删除**：不改 `Enabled`、不改 `Generation`、不删配置；到期自动回池；重启后保留。
3. **宁可继续用坏线路，也不落到直连**：无可用替代时保持原绑定，不允许 `direct_fallback` 扩散（直连 = 风控风险）。
4. **不改人工配置**：裸 URL 人工绑定的账号（`ProxyID == ""`）不参与自动改派。

## 二、概念：软失败

**软失败**：在"代理确实被使用 + 目标确认（`responseVerified`）"前提下，出现下列之一——

- 传输层超时 / 连接重置（`ObserveModelAttemptError` 的 `timeout` / `connection_failed`）
- HTTP `408`（代理侧请求超时）
- HTTP `524` / `504`（边缘或代理链路超时）

**不计入**：客户端取消（`isClientGone`）、`dispatch_changed`、`invalid_target`、3xx、405（走风控链）、402/429/5xx（各有既有语义）。

## 三、状态机（阶段 A/B）

```
正常 ──(30m 窗口内 失败≥3 且 尝试≥3 且 失败率≥50%)──► 隔离(30m)
  ▲                                                    │
  │(到期 + 30m 静默归零)                                │(隔离期内再命中)
  └────────────────────────────────────────── 续期 60m/120m/240m(封顶)
                                                       │
                                            每次隔离/续期都强制入队一次网络复核
```

- 隔离期间：`chooseProxyLocked` 排除该 identity（新绑定、风控换线、删除改派全部复用此函数）；绑定它的账号立即改派。
- 默认线路 / 托管默认线路 / 人工保护线路：**只记录不隔离**，日志 `action=protected`。
- 隔离状态与 `proxy_network_health` **互相独立**：复核通过不会提前解封。

### 3.1 408/524 线路在各阶段后的终局

| 阶段 | 终局 |
|---|---|
| A + B | 30 分钟内停止使用；隔离↔回池循环，最长 4h 隔离；**仍在池中** |
| 加 C | 空闲的同类线路也会在 30 分钟全量轮被扫到（但仍需 E 才能判失败） |
| 加 E（enforce） | 深度探针判吞吐失败 → 走原删除链 → **真正从池中移除** |
| 加 F | E 测不出的间歇性坏线：strikes≥3 → 自动停用（可一键恢复），**不再接活** |

## 四、阶段 A：采集与持久化

沿用 `quota_recovery.go` 的"meta 键 + 事务 + 内存快照"模式，不新建表。

```go
// internal/store/proxy_soft_failure.go
const proxySoftFailureKey = "proxy_soft_failure_state"

type ProxySoftFailure struct {
    Identity        string `json:"identity"`      // strictProxyIdentity(url)，不含认证
    ProfileID       string `json:"profile_id"`
    Generation      uint64 `json:"generation"`
    Attempts        int    `json:"attempts"`
    Failures        int    `json:"failures"`
    WindowStart     int64  `json:"window_start"`
    LastAt          int64  `json:"last_at"`
    LastSignal      string `json:"last_signal"`   // transport_timeout|transport_reset|status_408|status_504|status_524
    LastStatus      int    `json:"last_status"`
    Strikes         int    `json:"strikes"`
    QuarantineUntil int64  `json:"quarantine_until"`
}
```

公共 API：

```go
func (s *Store) RecordProxySoftOutcome(e ProxyEgress, signal string, status int, ok bool, now time.Time) ProxySoftDecision
func (s *Store) ProxyQuarantine(now time.Time) map[string]time.Time
func (s *Store) ProxySoftFailures() []ProxySoftFailure   // 深拷贝
```

改造点：

| 文件 | 改动 |
|---|---|
| `internal/config/config.go`（约 136 行） | 新增第六节 6 个环境变量 |
| `internal/store/proxy_soft_failure.go`（新） | 结构体 / 窗口滚动 / 续期 / 容量（4096）与 TTL（24h）/ 事务提交 |
| `internal/store/store.go` | 加字段；`load()`（:257）读取并 `delete(settings, key)`；`publishSettings()`（:474）过滤 |
| `internal/store/proxy_subscription.go:381` | 过滤新键 |
| `internal/gateway/proxy_observation.go:119` | `observeModelAttemptHealth`：`timeout`/`connection_failed` 上报失败；`success` 上报成功（算分母） |
| `internal/gateway/proxy_observation.go:224` | `ObserveProxyAttempt`：`408/504/524` 上报失败 |
| `internal/gateway/proxy_observation.go:130` | `invalid_target`/`dispatch_changed`/`canceled` 直接返回 |

> 挂点理由：`proxyAttemptObservation` 已持有 `egress`（identity/generation）与 `responseVerified`，能保证响应确实来自本次目标，不会把重定向后的第三方响应算到线路上。

## 五、阶段 B：隔离生效

| 文件 | 改动 |
|---|---|
| `internal/store/proxy_transactions.go:83` | 把改派段抽成 `reassignAccountsLocked(excludedIdentities map[string]bool, excludedURL string) (ProxyReassign, error)`；`removeProxiesAvoidingLocked` 改调它（删除语义不变） |
| `internal/store/proxy_soft_failure.go` | 隔离/续期时调 `reassignAccountsLocked(quarantined, "")`；不动 profiles、`sub=nil`；无替代则保持原绑定（`NoAlt++`） |
| `internal/store/proxy_strict.go:262` | `chooseProxyLocked` 过滤 `quarantined[identity]`；`allowed` 为空时忽略隔离再选一次（仍保留 suspects），打 `[~] proxy-quarantine relax=1` |
| `internal/store/proxy_failure_observation.go:79` | 隔离时把该线路写入既有 `proxyFailureRechecks`（`Pending=true`），复用现成复核队列 |

**顺序**：先落隔离状态（选线立刻排除），再改派。改派失败时下一轮软失败以 `extended` 重试，自愈。

### 5.1 新导入线路的冷启动预检（属于本阶段，不是可选项）

**现状问题**：订阅同步（`proxysub.Manager.execute` → `Store.FinishSubscriptionRun`）新增线路后，**没有任何"入队巡检"的动作**：新线路立刻 `Enabled=true`、立刻可被 `chooseProxyLocked` 选中（风控换线就可能抽中它），但要等到下一次全量轮才会被 `networkprobe` 看见——而当前版本全量轮会被单点复检长期顺延。导入前只有订阅侧一次 6s GET 复检（`proxysub/probe.go:180 probeConfig`），同样测不出吞吐问题。

**改动**（不新增调度器）：

| 文件 | 改动 |
|---|---|
| `internal/store/proxy_failure_observation.go:18` | `proxyFailureRecheck` 增加 `Fresh bool`、`InFlight bool`、`InFlightAt time.Time`；`TakeProxyNetworkRechecks` 对 `Fresh` 的 hint 不要求存在绑定账号（新线路可能还没有账号），只校验 `Enabled` + `Generation`；取走时置 `InFlight` |
| `internal/store/proxy_subscription.go:134 FinishSubscriptionRun` | 新增线路后，为每个新 profile 写入一条 `Fresh=true, Pending=true` 的 hint（`At=now`） |
| `internal/store/network_health.go:113 SaveNetworkHealth` | 记录落库时清除对应 profile 的 `InFlight`（门禁解除的唯一正常出口） |
| `internal/store/load()` | 启动时清空所有 `InFlight`（避免重启后悬挂标记永久挡路） |
| `internal/store/proxy_strict.go:262 chooseProxyLocked` | 跳过"`Fresh` 且结果未落库"的线路（判据见下） |
| `internal/store/proxy_soft_failure.go` | 首检失败 → 记一次软失败并隔离 30 分钟（**只隔离，不删除**） |

**门禁判据（必须逐字实现，含超时保底）：**

```
gate(identity) = 存在 hint
              && hint.Fresh
              && (hint.Pending || hint.InFlight)                // 未取走 或 已取走但结果未落库
              && now.Sub(hint.At) < FRESH_GATE_SECONDS (120s)   // 超时保底
```

状态推进：

| 事件 | 状态变化 | 门禁 |
|---|---|---|
| 导入时写入 hint | `Pending=true` | 挡住 |
| `TakeProxyNetworkRechecks` 取走 | `Pending=false, InFlight=true, InFlightAt=now` | **仍然挡住**（探针在跑，结果未出） |
| `SaveNetworkHealth` 落库该线路记录 | 清除 `InFlight` | 解除（成功可用；失败已被隔离排除） |
| 轮次异常/取消/进程重启 | `InFlight` 悬挂 | 由 TTL 兜底放行，并在 `load()` 时清空悬挂标记 |
| TTL 到期仍未落库 | — | 放行 + `[~] proxy-fresh id=… action=timeout`，保留常规复核 |
| profile 被禁用 / 代次变化 | Take 时删除 hint | 门禁自然消失 |

- **不能只按 TTL 实现**：若"取走即放行"，从取走到结果落库之间有 1–12 秒空窗（取决于探针耗时），风控换线可能正好在这个窗口选中未验证线路——这正是本节点要堵的口子。
- 时钟复用 store 现有 `strictObservationNow()`（与 `chooseProxyLocked` 同源），保证测试可注入假时钟。
- 兜底：全部候选都被门禁/隔离挡住时，按第五节 `relax` 逻辑忽略限制再选一次（不允许返回 false → 不允许掉直连）。

**为什么只隔离不删**：新线路的第一次检查发生在导入后 15 秒内，此时它还没被真实业务验证过；用一次刚出炉的探测结果直接删除，误删成本高于收益。让它在隔离里等软失败/全量复核接手。

**与阶段 E 的衔接**：E 上线后，`Fresh` hint 同时置 `Deep=true`（shadow），使新线路第一次体检就带吞吐数据；enforce 后新线路同样受吞吐门槛约束。



## 六、阶段 C：全量巡检定时器（P1-1）

| 文件 | 改动 |
|---|---|
| `internal/modelprobe/network.go:216` | `m.netNext = now.Add(interval)` 只在 `whole == true` 时执行 |
| `internal/modelprobe/network.go:350` | 日志追加 `whole=%d`；`whole==1` 时另打一行 `stage=probe` |

## 七、阶段 E：深度探针（吞吐档）

### 7.1 为什么不能只靠"带 body 的 POST"

朴素做法（POST 1MB 到目标 origin）有个致命问题：服务端（Cloudflare）可能在读完 body 之前就返回 401/400，Go 的 transport 会中止写入，于是**测不到上传能力**，反而得到"早退但没报错"的假阳性。

因此 deep 档改测**隧道裸写吞吐**：不看 HTTP 语义，只看"这条隧道能不能在时限内吃下 1MiB"。

### 7.2 实现

新增 `internal/proxy/tunnel.go`：

```go
// DialTunnel 建立到 target 的代理隧道（socks4/5/5h 复用 socksDialer；
// http/https 自行发 CONNECT，支持 user:pass 认证与 https 代理的 TLS 前跳）。
func DialTunnel(ctx context.Context, rawProxyURL, target string) (net.Conn, error)
```

新增 `internal/networkprobe/throughput.go`：

```go
const (
    DeepTimeout = 12 * time.Second
    DeepBytes   = 1 << 20 // 1MiB
    DeepFloor   = 256 << 10
)

type ThroughputReport struct {
    OK, Skipped bool   `json:"ok"`
    BytesSent   int64  `json:"bytes_sent"`
    MS          int64  `json:"ms"`
    KBps        int    `json:"kbps"`
    Error       string `json:"error"`
}

func CheckTunnelWrite(ctx context.Context, proxyURL, target string) ThroughputReport
```

判据（务必按此实现，否则会误杀好线路）：

| 情况 | 判定 |
|---|---|
| 12s 内写满 1MiB | **通过** |
| 写错误/对端关闭，但已写 ≥256KiB 且用时 <8s | **通过**（对端主动关闭，不是链路慢） |
| 12s 内只写了 <256KiB | **失败** |
| 建隧道失败 / CONNECT 非 200 / SOCKS 握手失败 | **失败**（与现有 `endpoint_unreachable` 同义） |
| `rawProxyURL` 解析失败 / 不支持的协议 | **Skipped**，不参与判定 |

目标：`zcode.z.ai:443`（与业务同目标）。隧道建立后直接写 1MiB 零字节，**不做 TLS 握手、不发任何凭据**。对端看到的是非 TLS 数据，会 RST——这正是我们要的"快速断开"，不产生上游业务副作用，也不构成任何可归因的请求。

> 阈值依据：业务侧 408 出现在 ~30s / 1.25–5.42MB（≈ 40–180KB/s）。取 85KB/s（1MiB/12s）作为"能承载 2MB 上传"的下限，与实测吻合。

### 7.3 接入删除链（关键：不改判定逻辑，只换探测内容）

| 文件 | 改动 |
|---|---|
| `internal/store/proxy_failure_observation.go:18` | `proxyFailureRecheck` 增加 `Deep bool`；`TakeProxyNetworkRechecks` 返回 `[]ProxyRecheck{Profile, Deep}`（软失败来源的置 true） |
| `internal/store/network_health.go:15` | `NetworkHealthRecord` 增加 `Throughput *networkprobe.ThroughputReport`（旧记录反序列化为 nil，兼容） |
| `internal/modelprobe/network.go:87` | `checkNetwork(..., deep bool)`：`deep` 为真时先跑 `CheckTunnelWrite`，`Report.OK = Report.OK && tw.OK` |
| `internal/modelprobe/network.go:225` | 确认阶段：`second` 与 `control` **同样跑 deep**，否则不可比 |
| `internal/modelprobe/network.go:321` | `confirmed` 判据保持原样（两次新鲜探测 + 独立对照） |

### 7.4 两步上线（必须先 shadow）

| 步骤 | 行为 | 目的 |
|---|---|---|
| **E1 shadow** | deep 结果只写 `NetworkHealthRecord.Throughput` 和日志 `[~] proxy-deep id=… ok=0 kbps=37 sent=…`，**不参与 `first.OK`** | 用 24h 真实数据校准阈值与误判率 |
| **E2 enforce** | deep 参与 `first.OK`，失败可触发删除 | 真正把 408/524 线路送进删除链 |

放行条件：E1 跑满 24h，且 `deep 失败` 的线路中 ≥80% 在窗口内还有业务软失败记录（相关性成立）才切 E2。

## 八、阶段 F：`soft_retired` 自动停用

前提：E 上线后仍有线路"软失败达标但 deep 通过"（间歇性坏线、或 deep 的 Skipped 情况）。

规则：
- `Strikes ≥ 3`（即已隔离 3 次、累计隔离 ≥3.5h）且最近一次隔离期内又出现软失败 → **停用**。
- 停用前必须先改派：`reassignAccountsLocked` → 再 `UpdateProxyProfile(id, name, url, false, protection)`（顺序反了会让绑定账号瞬间失去线路）。
- 状态标记：`ProxySoftFailure.Retired = true` + `RetiredAt`，面板显示 `soft_retired`，可一键恢复（恢复时清空该 identity 的 soft 状态与 strikes）。
- 保护名单豁免（默认线路/托管默认线路/人工保护）。
- 开关：`ZCODE_PROXY_SOFT_RETIRE`（默认 **true**，仅在 E2 生效后才允许；E2 之前该开关空转）。

**没有 deep 证据不许删**：F 只做"停用"（可恢复），不调用删除。要删必须走 E 的删除链。

## 九、阶段 D：观测面（可选）

| 文件 | 改动 |
|---|---|
| `internal/adminapi/model_probe.go:55` | `handleHealthSummary` 增加 `"soft": h.Store.ProxySoftFailures()` |
| `frontend/` | 线路表格加两个 chip：`隔离至 HH:MM`、`soft_retired`；不加新页面，改完重建 `frontend/dist` 并提交 |

## 十、参数（环境变量，不进面板）

用户已反馈后台界面偏复杂，本计划**不新增任何表单项**。

| 变量 | 默认 | 生效阶段 |
|---|---|---|
| `ZCODE_PROXY_SOFT_QUARANTINE` | `true` | A/B（总开关，`false` 只记录不隔离） |
| `ZCODE_PROXY_SOFT_WINDOW_SECONDS` | `1800` | A |
| `ZCODE_PROXY_SOFT_MIN_ATTEMPTS` | `3` | A |
| `ZCODE_PROXY_SOFT_MIN_FAILURES` | `3` | A |
| `ZCODE_PROXY_SOFT_MIN_RATE` | `0.5` | A |
| `ZCODE_PROXY_SOFT_QUARANTINE_MINUTES` | `30` | A |
| `ZCODE_PROXY_FRESH_GATE_SECONDS` | `120` | B（新线路冷启动门禁有效期，超时放行） |
| `ZCODE_PROXY_DEEP_PROBE` | `shadow` | E（`off`/`shadow`/`enforce`） |
| `ZCODE_PROXY_SOFT_RETIRE` | `true` | F（E2 前空转） |

## 十一、日志与判读

```
[~] proxy-soft id=proxy-0bccdca2066f078b signal=status_408 failures=4 attempts=6 rate=67% action=quarantine until=… strikes=1 reassigned=1 no_alt=0
[~] proxy-soft id=proxy-xxx signal=transport_timeout action=extended until=… strikes=2
[~] proxy-soft id=proxy-yyy action=protected reason=default_proxy
[~] proxy-quarantine relax=1 candidates=3 quarantined=3
[~] proxy-fresh id=proxy-new-1 queued=3 source=subscription
[~] proxy-deep id=proxy-xxx ok=0 kbps=37 sent=196608 error=timeout
[~] proxy-health round=… stage=probe whole=1 checked=48
[~] proxy-retire id=proxy-zzz strikes=3 reassigned=2
```

判读口径（写进 release notes）：
- `quarantine` 远多于 `expired` → 阈值过严或池子质量差。
- `relax=1` 频繁 → 池子被隔离打空，调 `MIN_FAILURES` 或补线。
- `deep ok=0` 与业务软失败相关性 <80% → 阈值需重新校准（E1 的放行条件）。
- `proxy-fresh` 每 15 分钟一批（与订阅同步同频），`queued` 数应等于本轮新增线路数。
- `proxy-retire` 出现 → 池子在真正变干净，观察是否有账号掉直连。

## 十二、测试清单（CI 必须有）

**store（`proxy_soft_failure_test.go`）**
1. 3 失败/3 尝试 → `quarantine`，`until = now+30m`
2. 失败率 33% → 不隔离
3. 隔离期内再失败 → `extended`，30→60→120→240 封顶
4. 到期 + 30m 静默 → strikes 归零
5. 默认/托管默认线路 → `protected`，不隔离
6. 重启后从 meta 恢复
7. 容量 4200 条 → 淘汰最旧，长度 ≤4096
8. 成功与失败交织时计数正确
9. `Mode != "proxy"` 直接返回

**store（选线与改派）**
10. 隔离线路不被 `chooseProxyLocked` 选中
11. 全部隔离 → `relax` 回退仍能选出线路
12. 隔离触发改派；裸 URL 人工绑定不动
13. 无替代 → 保持原绑定，`NoAlt=1`，无 `direct_fallback`

**store（阶段 B3 冷启动预检）**
14. `FinishSubscriptionRun` 导入 3 条 → 产生 3 条 `Fresh=true` hint，且 `Pending=true`
15. `Fresh` hint 在**无绑定账号**时也能被 `TakeProxyNetworkRechecks` 取出（不像普通 hint 因账号缺失被丢弃）
16. `Fresh` 且未出结果 → `chooseProxyLocked` 跳过该线路
17. hint 被 Take 但**结果未落库** → 门禁仍然生效（空窗期必须堵住）
18. `SaveNetworkHealth` 落库该线路记录 → 门禁解除，可被选中
19. `Fresh` 超过 `ZCODE_PROXY_FRESH_GATE_SECONDS` 仍未落库 → 门禁放行（TTL 兜底）
20. 进程重启后悬挂的 `InFlight` 被清空，不会永久挡路
21. 首检失败 → 记一次软失败 + 隔离，**不产生删除**（`removeProxies…` 未被调用）
22. 全部线路都是 `Fresh` 且未出结果 → `relax` 兜底仍能选出线路（不允许返回 false）

**gateway**
23. 408/504/524 + `responseVerified=true` → 计一次
24. 重定向后 → 不计
25. 取消 / `dispatch_changed` / `invalid_target` → 不计
26. `client.Do` 超时 → `transport_timeout`
27. `healthOnce` 保证一次请求只上报一次（并发）

**networkprobe（阶段 E）**
28. 用本地假代理（accept 后不读）→ 12s 内写不满 256KiB → `ok=0`
29. 用本地假代理（全速读）→ 1MiB 写满 → `ok=1`
30. 对端写 64KiB 后关闭 → `ok=1`（已写 ≥256KiB 才通过；不足则 `ok=0`）
31. `socks5` / `http` / `https` 三种代理的 `DialTunnel` 建链（本地假服务）
32. `CheckTunnelWrite` 对非法 URL → `Skipped`

**modelprobe（C/E）**
33. 单点复检轮不推进 `netNext`（假时钟 100 轮后不变）
34. 到点全量轮 `checked=全部启用线路`，日志 `whole=1`
35. `deep=shadow` 时 `first.OK` 不看 deep；`enforce` 时看

**回归**
36. `removeProxiesAvoidingLocked` 抽取改派段后删除语义不变
37. 风控换线排除隔离线路

## 十三、提交与发布

一个阶段一个提交，CI 全绿再下一个。**不本地跑测试**（AGENTS.md）。

| # | 提交 | 版本 |
|---|---|---|
| 1 | `feat: 采集代理软失败并持久化隔离状态` | v2.10.15 |
| 2 | `feat: 隔离线路退出选线、改派绑定账号并冷启动预检新线路`（含 B3） | v2.10.15 |
| 3 | `fix: 全量巡检不再被单点复检顺延` | v2.10.15 |
| 4 | `feat: 健康摘要暴露代理隔离状态`（含 dist） | v2.10.15 |
| 5 | `chore: 发布 v2.10.15-go` | — |
| 6 | `feat: 增加代理隧道吞吐探针（shadow）` | v2.10.16 |
| 7 | `feat: 吞吐探针参与网络复核判定`（enforce，需 24h 数据） | v2.10.16 |
| 8 | `chore: 发布 v2.10.16-go` | — |
| 9 | `feat: 长隔离线路自动停用并支持恢复` | v2.10.17 |
| 10 | `chore: 发布 v2.10.17-go` | — |

发布动作（与历史流程一致）：
1. 分支 CI 绿 → `git tag -a vX-go -m …` → 推 tag
2. `gh run watch <id> -R fujunchao/zcode2api-plus --exit-status`（不接管道）
3. `.workbuddy/check-ghcr.js X.Y.Z-go` 核对镜像 tag（不带 v）
4. 核对 Release 附件 + GHCR

## 十四、明确不做的事

- **不改 `IsEndpointUnreachable`**：它牵动 claim/quota/初始化链路。
- **不动 `ModelProbeSettings` 结构**：不给面板加开关。
- **不在 E 之前删任何软失败线路**：E2 之前 soft 只有隔离权限。
- **不做"POST 带 body 到 origin"的探针**：服务端早退会让测量失真（见 7.1）。
- **不把 F 做成删除**：停用可恢复，删除必须走 E 的双证链。

## 十五、验收标准

1. 第十二节测试 1–37 全绿，CI 通过。
2. 复现 `mjuqky` 场景（同一线路 11:22–11:43 五次尝试四次失败）→ 30 分钟内 `action=quarantine` + 改派成功。
3. 冷启动预检验收：订阅导入新线路后 ≤15s 出现该线路的 `proxy-health` 记录；在结果出来前不会被选为出站（用一次风控换线复现）；判定失败只隔离不删。
4. A/B/C 上线 24h：`quarantine/expired` 比例可解释；408/524 用户可见数下降（基线 16h/20 次）；`relax=1` 不连续出现；池内线路数不因本功能减少。
5. E1 shadow 24h：`deep` 失败与业务软失败相关性 ≥80%，才允许切 E2。
6. E2 上线 24h：出现 `proxy-deep ok=0` → 删除 → 该线路不再被使用；无账号掉直连。
7. F 上线 24h：`proxy-retire` 数量与"E 测不出的软失败线路"数量匹配；恢复入口可用。
8. `ZCODE_PROXY_SOFT_QUARANTINE=0` / `ZCODE_PROXY_DEEP_PROBE=off` / `ZCODE_PROXY_FRESH_GATE_SECONDS=0` 可分别回滚到对应旧行为。

## 十六、风险与对策

| 风险 | 概率 | 对策 |
|---|---|---|
| 408 其实来自上游非代理 → 误隔离 | 中 | 只统计 `Mode=="proxy"` + `responseVerified`；失败率 ≥50%；30 分钟自动回池 |
| 误隔离抽空池子 → 掉直连 | 低 | `relax` 回退 + `NoAlt` 保持原绑定，**永不直连** |
| 冷启动门禁让池子暂时变窄（批量导入后无人可选） | 中 | `ZCODE_PROXY_FRESH_GATE_SECONDS=120` 超时放行；全 `Fresh` 时 `relax` 兜底；门禁只挡"未出结果"，不挡"检查失败已隔离" |
| deep 探针误判好线路（对端早关） | 中 | 分级判据（≥256KiB 且 <8s 视为通过）+ E1 shadow 校准 |
| deep 探针把流量写到上游 | 低 | 只写隧道、不做 TLS、不发凭据；1MiB 上限；仅对软失败候选执行 |
| `DialTunnel` 对 https 代理支持不全 | 中 | 单测覆盖 http/https/socks5/socks5h；不支持的协议返回 `Skipped` 而非 `失败` |
| F 停用后账号绑定失效 | 中 | 先改派再停用；保护名单豁免；恢复入口一键回滚 |
| 阶段 C 改动导致巡检频率上升 | 低 | 全量轮仍 30 分钟一次；观察 `stage=probe` 行数（应 ~48/天） |
