# 实施计划：导入记账、最近成功免删、吞吐探针（shadow）

> 依据：`docs/analysis-20261019-night.md`（静态判定只能回答「能不能连」）
> 目标版本：**v2.10.27-go**（第 1、2 项）+ **v2.10.28-go**（第 3 项，shadow）
> 状态：**待批准实施**（已按外部评审修订，见第十一节）

---

## 一、三件事各解决什么

| # | 问题 | 改法 | 风险 |
|---|---|---|---|
| 1 | 删除有日志、导入没有，收支对不上 | 导入成功后打一行 `proxy-import` | 无（纯日志） |
| 2 | 正在被用户使用的线路，探针一时不通就被删 | 巡检删除前增加「最近 24h 有成功」免删 | 延迟清理最多 24h |
| 3 | 静态探针只测「可达」，测不出「传不动」 | 隧道裸写 1MiB，shadow 只记录不判定 | 默认不改任何决定 |

---

## 二、第 1 项：导入记账

### 2.1 位置

`internal/store/proxy_subscription.go` 的 `FinishSubscriptionRun`，在 `commitProxyBundleLocked` 成功之后、
`queueFreshProxyChecksLocked` 之前（约 203–210 行）。

### 2.2 格式

```
[~] proxy-import source=<run.ID> added=7 skipped=3 received=12 total=104
```

- `added`：`r.Added`；`skipped`：`r.CommitSkipped`；`received`：`r.Received`
- `total`：落库后的池子条数（`len(profiles)`）——**与 `proxy-health … removed=N` 对账的关键**

只在 `r.Added > 0` 时打，避免每 15 分钟的空轮刷屏。

手动新增（`internal/store/store.go:880` 的 `AddProxyProfile`，注意不在 `proxy_transactions.go`）补一行：

```
[~] proxy-import source=manual id=proxy-xxxx total=105
```

### 2.3 锁内日志必须异步

`FinishSubscriptionRun` 与 `AddProxyProfile` 全程持有 `s.mu`。`store.go:522` 的约定写得很清楚：
锁内不做 I/O，`web.Warn` 是同步 stdout 写，管道阻塞时会把整个 Store 锁住。

两处都用 `go web.Warn(...)`（`logPersistFailure` 已给出先例）。

---

## 三、第 2 项：最近成功免删

### 3.1 数据来源

`model.Account.LastUsedAt`（float64 unix 秒），由 `MarkSuccess`（`engine.go:1186`）在**上游成功交付**时写入，
async 池复用同一函数。它天然是「这个账号最近一次真的成功过」的证据，不需要新增状态。

### 3.2 作用范围：只挡巡检删除，不挡严格退役

这一点比初稿收窄了，原因见 3.4：

| 路径 | 是否受 recent_success 影响 |
|---|---|
| 网络巡检确认失败 → 删除（`purgeProxyProfilesIfCurrentLocked`） | ✅ 是 |
| 断流熔断 / 登录失败清理 / 手动删除 | ❌ 否（保持现状） |
| 模型归因严格退役（405 归因，`startStrictRetirementLocked`） | ❌ 否 |

### 3.3 实现

新增一个**巡检专用**过滤器，在 `purgeProxyProfilesIfCurrentLocked`（`proxy_transactions.go:74`）里、
调用 `removeProxiesLocked` 之前应用：

```go
// recentSuccessProtectionsLocked 返回「最近 ZCODE_PROXY_PROTECT_RECENT_HOURS 内成功交付过」的线路身份。
func (s *Store) recentSuccessProtectionsLocked() map[string]time.Time
```

```
now := time.Now()
for _, a := range s.allAccountsLocked():
    identity := strictProxyIdentity(derefStr(a.ProxyURL))
    if identity == "" || a.LastUsedAt == nil: continue
    at := time.Unix(0, int64(*a.LastUsedAt*1e9))
    if now.Sub(at) > window: continue
    recent[identity] = max(recent[identity], at)
```

`purgeProxyProfilesIfCurrentLocked` 里过滤 `wanted`：命中 recent 的 ID 直接移出集合，
再交给原有删除逻辑（原有 `proxyAutoDeleteProtectionsLocked` 继续管 manual/default/managed）。

### 3.4 为什么不对「严格退役」生效

严格退役是 405 归因链的刹车：一条脏线路会持续烧伤账号。`LastUsedAt` 是**账号级**的，
一条线只要还有账号在上面成功过，就说明它「最近被用过」——用它去否决 405 退役，
等于给脏线路 24 小时的免死金牌，正好推翻 v2.10.21–23 三版做的事。

结论：recent_success 只挡「探测判定失败 → 删除」，不挡「流量证据判定退役」。

> 外部评审建议「接受这个外延」；这里选择**收窄**，代价是需要一个独立过滤器而不是往保护 map 里塞一项。
> 但这样可以同时满足「不删正在用的线」和「不放过脏线」。

### 3.5 展示与状态

`ProxyAutoDeleteProtections()`（`proxy_protection.go:41`，供管理端与巡检状态使用）需要在基础原因之上
合并 recent_success：否则线路「没被删但状态仍是 confirmed_failure」，后台看不懂。

```go
byIdentity := s.proxyDeleteProtectionReasonsLocked()  // base + recent_success
```

`automaticProxyRemovalReasonLocked`（`proxy_protection.go:55`）保持只用 base —— 严格退役不受影响。

### 3.6 前端（必须一起改）

`frontend/src/lib/types.ts:263` 把保护原因写成联合类型，`frontend/src/pages/proxies.tsx:56` 的 switch
没有默认分支——新增 `recent_success` 而不改前端，界面上**什么都不会显示**，与「让管理者看懂为什么没删」的目标矛盾。

- `types.ts`：联合类型加 `'recent_success'`
- `proxies.tsx`：加 `case 'recent_success': return '最近 24 小时有成功流量，自动保护'`
- **重建 `frontend/dist` 并提交**（`go:embed` 产物；AGENTS.md 允许本地构建产物）

### 3.7 参数与日志

| 变量 | 默认 | 说明 |
|---|---|---|
| `ZCODE_PROXY_PROTECT_RECENT_HOURS` | `24` | 0 = 关闭该保护 |

被跳过时打一行（同一 identity 每小时最多一次，内存限频；同样 `go web.Warn`）：

```
[~] proxy-protect id=proxy-xxx reason=recent_success last_ok=3.2h
```

---

## 四、第 3 项：吞吐探针（shadow）

### 4.1 为什么不能用 HTTP 请求测

「POST 1MB body 到目标站点」不可行：Cloudflare 读完 headers 就回 401/400，Go 的 transport 会提前中止写入，
测到的是对方的拒绝速度而不是链路带宽。必须走**裸隧道**：建连后直接往连接里写字节，不关心对方怎么回。

### 4.2 新增 `proxy.DialTunnel`

```go
// internal/proxy/tunnel.go
func DialTunnel(ctx context.Context, proxyURL, target string) (net.Conn, error)
```

| 方案 | 实现 |
|---|---|
| `socks4` / `socks5` / `socks5h` | 复用同包 `socksDialer` 的 dial + `socks5Handshake` / `socks4Handshake`（已带 v2.10.24 的超时） |
| `http` | `CONNECT host:port HTTP/1.1` + 读响应头（约 40 行） |
| `https` | 同上，先 `tls.Client` 连代理 |

拨号用 `dialTimeout()`、握手用 `handshakeTimeout()`（`config.ProxyDialTimeout/ProxyHandshakeTimeout`）。
返回的 conn 不设 deadline，由调用方按预算控制。

### 4.3 新增 `networkprobe.CheckTunnelWrite`

```go
type TunnelReport struct {
    OK        bool   `json:"ok"`
    Bytes     int64  `json:"bytes"`
    MS        int64  `json:"ms"`
    Kbps      int64  `json:"kbps"`
    EarlyStop bool   `json:"early_stop,omitempty"`
    Error     string `json:"error,omitempty"`
}

func CheckTunnelWrite(ctx context.Context, proxyURL, target string, size int64, budget time.Duration, minBytes int64, minKbps int64) TunnelReport
```

判据（分级，避免「对端早关」被误判成「链路慢」）：

1. 预算内写满 `size` → **OK**
2. 写错误但已写 ≥ `minBytes`（默认 256KiB）且用时 < 预算的 2/3 → **OK**（对端早关 ≠ 链路慢）
3. 写了 ≥ `minBytes` 但速率 < `minKbps` → **失败**
4. 写的字节 < `minBytes` → **失败**

阈值 85 KB/s：实测「慢但能用」约 68 KB/s，昨天故障线路是 20 KB/s 量级。

### 4.4 接入：只挂巡检 first 阶段

`checkNetwork` 有 4 个调用点，只有 first 阶段该深探：

| 调用点 | 是否深探 | 理由 |
|---|---|---|
| `networkBatch` first（`network.go:231`） | ✅ | 巡检主路径 |
| `networkBatch` second（`network.go:285`） | ❌ | 只对失败线路复核可达性；first 的隧道结果已在记录里 |
| direct 直连检查（`network.go:303`，profile 为空） | ❌ | 没有代理 URL，`DialTunnel` 无意义 |
| `networkPreflight`（`network.go:440`） | ❌ | **每个模型探测租约都会跑**，在这里写 1MiB 等于把流量与特征放大到每次租约 |

实现：给 `networkBatch` 增加 `deep bool` 参数（first 传 true，second 传 false），
`checkNetwork` 增加同参数，其余两个调用点默认 false。

> 这一条同时解决评审指出的耗时问题：深探只在 first 跑，second 不再重复一遍。

### 4.5 结果落库与日志

- `networkprobe.Report` 增加 `Tunnel *TunnelReport` 字段（指针 + omitempty，旧记录反序列化兼容）。
  `NetworkHealthRecord` 由 `First/Second/Control` **三个 Report 字段组合**而成（不是内嵌单个 Report），
  因此不需要改结构，深探结果随 `First` 一起入库。
- 每次深探一行：

```
[~] proxy-deep id=proxy-xxx mode=shadow ok=0 bytes=262144 ms=4800 kbps=43 target=zcode.z.ai:443 early_stop=1
```

### 4.6 参数

| 变量 | 默认 | 说明 |
|---|---|---|
| `ZCODE_PROXY_DEEP_PROBE` | `shadow` | `off` / `shadow` / `enforce` |
| `ZCODE_PROXY_DEEP_TARGET` | `zcode.z.ai:443` | 只接受 host:port（不接受 URL，避免带凭据） |
| `ZCODE_PROXY_DEEP_BYTES` | `1048576` | 写入量 |
| `ZCODE_PROXY_DEEP_BUDGET_MS` | `12000` | 单次预算 |
| `ZCODE_PROXY_DEEP_MIN_BYTES` | `262144` | 早停判定下限 |
| `ZCODE_PROXY_DEEP_MIN_KBPS` | `85` | 速率下限 |

### 4.7 耗时估算（只算 first）

单批 48 条、并发 8、每条最坏 12s ⇒ **最坏 +72 秒**；批次 deadline 是 5 分钟（`network.go:181/227`），
first 阶段最坏 72s + 浅探针 ~30s ≈ 100s，仍有余量。second 阶段不再深探，不会翻倍。

---

## 五、代码改造点

| 文件 | 改动 |
|---|---|
| `internal/store/proxy_subscription.go` | 导入成功后 `go web.Warn` 打 `proxy-import` |
| `internal/store/store.go:880` | `AddProxyProfile` 成功后打 `proxy-import source=manual` |
| `internal/store/proxy_protection.go` | 新增 `recentSuccessProtectionsLocked()`；`ProxyAutoDeleteProtections()` 合并；`automaticProxyRemovalReasonLocked` 保持 base |
| `internal/store/proxy_transactions.go:74` | `purgeProxyProfilesIfCurrentLocked` 过滤 recent；新增限频日志字段 |
| `internal/config/config.go` | `ProxyProtectRecentHours`、`DeepProbe`、`DeepTarget/Bytes/BudgetMS/MinBytes/MinKbps` |
| `internal/proxy/tunnel.go`（新） | `DialTunnel` + http CONNECT |
| `internal/networkprobe/probe.go` | `TunnelReport`、`CheckTunnelWrite`、`Report.Tunnel` |
| `internal/modelprobe/network.go` | `networkBatch`/`checkNetwork` 增 `deep bool`；只在 first 传 true |
| `frontend/src/lib/types.ts` | 保护原因联合类型加 `recent_success` |
| `frontend/src/pages/proxies.tsx` | 补 case 与文案 |
| `frontend/dist` | 重建并提交 |

---

## 六、测试清单

**导入记账**
1. 导入 2 条 → 一行 `proxy-import … added=2 total=N`
2. 导入 0 条 → 不产生该行

**最近成功免删**
3. 账号 1 小时前成功过 → 巡检删除时该线路被跳过，状态为 `protected_failure`
4. 25 小时前成功过 → 正常删除
5. `ZCODE_PROXY_PROTECT_RECENT_HOURS=0` → 完全关闭
6. 手动删除仍然生效
7. `default_proxy` / `manual` 原因不被覆盖
8. **严格退役不受影响**：最近成功的线路在 405 归因下仍可 `startStrictRetirementLocked`

**吞吐探针**
9. `DialTunnel` 经假 socks5 代理连通（测试放在 `internal/proxy` 包内，复用同包 `fakeSocks5`）
10. `DialTunnel` 经 http `CONNECT` 代理连通
11. 写满 1MiB → `ok=true`，kbps 合理
12. 对端读 256KiB 后关闭 → `ok=true`、`early_stop=true`
13. 限速写 < 阈值 → `ok=false`
14. `shadow`：隧道失败但浅探针通过 → `Report.OK` 仍 true
15. `enforce`：同上 → `Report.OK=false`
16. `off`：不建隧道，`Tunnel == nil`

**回归**
17. second / direct / preflight 三处不产生 `Tunnel`（防止深探泄漏到模型租约路径）

---

## 七、提交与发布

| 版本 | 提交 | 内容 |
|---|---|---|
| v2.10.27 | `feat: 导入记账与最近成功免删` + `chore: 发布` | 第 1、2 项（含前端与 dist 重建） |
| v2.10.28 | `feat: 隧道吞吐探针（shadow）` + `chore: 发布` | 第 3 项 |

CI 全绿 → 附注 tag → 核对附件与 GHCR。

**E2（enforce）不在本轮**：shadow 跑满 24 小时，比对「深探失败」与「软失败/524」相关性，≥80% 再切。

---

## 八、风险与回滚

| 风险 | 对策 |
|---|---|
| 最近成功保护让坏线多留 24h | 只挡巡检删除；软失败隔离仍生效；窗口可调，0 即关闭 |
| 保护被误用到严格退役 → 脏线烧伤账号 | 明确不接入退役路径，测试 8 覆盖 |
| 探针把「对端早关」当慢 | 分级判据 2 |
| 深探拖长巡检 | 只挂 first；最坏 +72s，deadline 5min |
| 深探被上游当异常流量 | 只写不读、不带凭据、目标固定；默认 shadow 不影响判定 |
| 前端漏改导致界面无文案 | 类型 + case + 文案 + dist 重建，列入验收 |

回滚：`ZCODE_PROXY_DEEP_PROBE=off`、`ZCODE_PROXY_PROTECT_RECENT_HOURS=0`，两项独立。

---

## 九、验收标准

1. 第六节 17 项测试全绿，两次发布 CI 通过。
2. 上线 24h：
   - `proxy-import` 与 `proxy-health … removed` 能对上池子变化；
   - `proxy-protect reason=recent_success` 出现且不刷屏（≤ 池子条数/天）；
   - 后台代理列表能看到「最近 24 小时有成功流量，自动保护」；
   - `proxy-deep` 覆盖全部被检查线路，`ok=0` 占 5–20%；
   - 删除行为与 v2.10.26 无系统性差异（shadow 不改判定）。
3. shadow 数据支持相关性判断后，单独发 enforce。

---

## 十、不做什么

- 不改浅探针判据（仍是「可达性」），深探只做加法。
- 不让深探参与删除（shadow 只记录）。
- 不让 recent_success 参与严格退役。
- 不做按 body 大小路由（需要先有深探数据）。
- 不提高单批 48 条 / 并发 8 的上限。

---

## 十一、外部评审的吸收情况

| 评审意见 | 处理 |
|---|---|
| 前端「无需改动」不成立（`types.ts:263`、`proxies.tsx:56`） | ✅ 采纳，已列入改造点与验收（含 dist 重建） |
| 保护会外延到严格退役，需要确认 | ✅ 承认外延存在，但**选择收窄**：只挡巡检删除，不接退役链；另做测试 8 守卫 |
| `checkNetwork` 还有 direct 与 preflight 两个调用点 | ✅ 采纳，深探只挂 first，其余显式 false |
| 深探会让 first+second 各跑一遍，逼近 5 分钟 deadline | ✅ 采纳，second 复用 first 结果，不再深探 |
| `AddProxyProfile` 在 `store.go:880` 不在 `proxy_transactions.go` | ✅ 已更正 |
| 「`NetworkHealthRecord` 内嵌 Report」措辞不准 | ✅ 已改为「由三个 Report 组合」 |
| `fakeSocks5` 不跨包导出，测试无法复用 | ✅ 采纳，`DialTunnel` 测试放 `internal/proxy` 包内 |
| 锁内 `web.Warn` 需异步（`store.go:522`） | ✅ 采纳，两处日志均 `go web.Warn` |
