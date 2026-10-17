# 实施计划：选号能力补强（抄 expire_first、试请求级预算与异步对齐）

> 依据：`docs/analysis-zcode-pool-parity-20261012.md`、本轮三仓库选号对比（v2.10.16-go，`19887ec`）
> 参照：`fujunchao/zcode-pool`（`aefa46b`）、上游 `gakiyukr/zcode2api-plus`（冻结在 `v2.0.9-go`）
> 状态：**待批准实施**
> 前置事实：上游冻结、`zcode-pool` 本轮无更新；唯一仍有差距的维度是「额度到期感知」，其余为自测发现的预算与异步缺口。

---

## 〇、置顶确认（按 v2.8.0 教训）

1. **P4「pinned 单号」含 UI**：账号池每行加一个钉住按钮（一键钉 30 分钟、再点解除），行内显示「釘住 · 剩 N 分」徽标；**要重建 `frontend/dist` 并提交**。不做设置页表单，避免把运维动作混进策略配置。
2. **P5「会话粘性」进计划但默认关闭**，且以 P3 的度量结果为准入条件——达标才实施，不达标直接放弃（阈值见 §六）。
3. **不改任何账号数据契约**：无新增 `accounts.data` 键；pinned 用进程内状态（重启即清），sticky 也只存进程内映射。
4. **不本地跑测试**：全部由 GitHub Actions 验证，本地只做 `gofmt`（仅 LF 文件）、前端 `npm run build` 与差异检查。

---

## 一、要解决的问题（按证据）

| 编号 | 事实 | 出处 | 影响 |
|---|---|---|---|
| G1 | 首字节 watchdog 超时**不消耗**换线预算，只 `switchAccount + avoid` | `engine.go:464` | 最坏 5 次 × 90s = **450s**，5.5MB 慢线场景仍可复现「转圈 7 分钟」 |
| G2 | 额度排序只看「剩余最多」，不看「什么时候作废」 | `store.go:1836-1860` | 快过期的套餐额度可能整批作废；`one_time` 之外无任何到期维度 |
| G3 | `asyncpool` 无 watchdog、无 avoid（`firstByteWatchdog`/`timeoutStatus` 0 命中） | `asyncpool/pool.go:455-644` | 异步票 300s 墙钟，慢线白等到墙钟才失败 |
| G4 | 诊断行没有 session 维度 | `diagnostics.go:184-211` | 「换号是否伤害 prompt cache」无法度量，粘性只能靠猜 |
| G5 | 无「临时钉住某个号」的手段 | — | 真机复验只能停用其余 60 个号 |

`zcode-pool` 值得抄的就一条真金：**G2 的到期优先**。其余（必须有额度快照、`MODEL_AUTO` 静默换模型、405 不换不标记、**全局且无 TTL** 的粘性）明确不抄，理由见 §九；按会话、带 TTL、可被风控打断的粘性是我们自己的方案（§八）。

---

## 二、范围与版本编排

| 阶段 | 内容 | 目标版本 | 提交数 | 风险 | 含 UI |
|---|---|---|---|---|---|
| P0 | 请求级总预算 + watchdog 剩余预算 | v2.10.17-go | 2（修文档漂移 + 功能） | 低 | 否 |
| P1 | 到期优先（插在 promo 之前） | v2.10.18-go | 1 | 中 | 否 |
| P2 | 异步池补齐 watchdog + avoid | v2.10.19-go | 1 | 中 | 否 |
| P3 | 度量基建（diag `session=` + 账号级 TTFB 取数） | v2.10.19-go | 1（与 P2 同版） | 极低 | 否 |
| P4 | pinned 单号（API + 账号行 UI） | v2.10.20-go（含 dist 重建） | 3（后端 / 前端+dist / 版本） | 低 | **是** |
| P5 | 会话粘性试点（默认关，P3 达标才做） | 待定，独立版本 | 1 | 中 | 否 |

可合并：P0+P1 也可压成 v2.10.17（两提交一版本），但建议分开，G1 是用户可见故障，先落地先验证。

---

## 三、P0：请求级总预算（修 G1）

### 3.1 参数

| 环境变量 | 默认 | 说明 |
|---|---|---|
| `ZCODE_REQUEST_DEADLINE_SECONDS` | `240` | 从进入 `RunMessages` 起算的墙钟上限；`0` = 关闭 |
| `ZCODE_UPSTREAM_FIRST_BYTE_TIMEOUT` | `90` | 保持现值，但单次尝试取 `min(90s, remaining)` |

240s 的依据：实测 5.5MB 首字节 30–65s（`docs/analysis-long-stall-20261017.md`），两次尝试（首次 + 一次换线）≈130s，留一倍余量；同时把最坏 450s 压到 240s。

### 3.2 实现

1. `internal/config/config.go` 新增 `RequestDeadline = time.Duration(max(0, envInt("ZCODE_REQUEST_DEADLINE_SECONDS", 240))) * time.Second`，紧跟 `UpstreamFirstByteTimeout`（`config.go:170`）并沿用同一套注释风格。
2. `internal/gateway/engine.go` `RunMessages`：函数入口（`diag` 建好之后）取 `startedAt := e.now()`，`deadlineAt := startedAt.Add(config.RequestDeadline)`。**后续所有判定都用 `e.now()`**（引擎已有可注入时钟，测试才能确定性地构造超时）。
3. 选号循环内、`SelectAvoiding` **之前**判一次：
   ```go
   if config.RequestDeadline > 0 && e.now().After(deadlineAt) {
       diag.DeadlineHit = true
       return errResultWithDetails(http.StatusServiceUnavailable, "request_deadline_exceeded",
           fmt.Sprintf("请求总耗时超过 %s，已停止换号（已尝试 %d 个账号）", config.RequestDeadline, diag.Attempts),
           map[string]any{"attempts": diag.Attempts, "deadline_seconds": int(config.RequestDeadline.Seconds())})
   }
   ```
   - **只判墙钟，不派生 ctx**：流式交付可以超过 deadline（已经在读的流不能被切断），只在「准备发起下一次尝试」时拦截。
4. `tryAccount` 内 watchdog 超时改为 `deadlineAt.Sub(e.now())` 与 `config.UpstreamFirstByteTimeout` 取小：
   - 剩余不足时按剩余计，避免「最后一次尝试又白等 90s」；
   - 剩余 ≤ 0 时不发起，直接走第 3 步的出口。
   - 注意用 `deadlineAt.Sub(e.now())` 而不是 `time.Until(deadlineAt)`：后者是真实时钟，与可注入的 `e.now` 混用会让测试不可控。
5. `ReqDiag` 加 `DeadlineHit bool`，`Format()` **追加在行尾**（`deadline_hit=%d`），不动既有字段位置（外部分析脚本按位置切分）。
6. 文档漂移顺手修：`store.go:1730` 注释「RiskSilentHours 默认24h」→「默认 0（仅阶梯冷却）」。

### 3.3 测试（CI）

- `TestRequestDeadlineStopsBeforeNextAttempt`：deadline=1s + 两个 90s 慢上游 → 第二次尝试前返回 503，`attempts=1`，账号**未冷却**。
- `TestRequestDeadlineDoesNotCutStream`：响应头在 deadline 内返回、body 持续 3×deadline → 完整交付，无截断。
- `TestRequestDeadlineDisabled`：deadline=0 → 行为与 v2.10.16 完全一致。
- `TestFirstByteTimeoutClampedByDeadline`：剩余 5s 时 watchdog 按 5s 触发，日志/诊断可区分。
- 回归：`firstbyte_test.go`、`ttfb_test.go` 全绿。

### 3.4 回滚

`ZCODE_REQUEST_DEADLINE_SECONDS=0` 即回到 v2.10.16 行为，无需回滚镜像。

---

## 四、P1：到期优先（抄 G2）

### 4.1 设计原则

`zcode-pool` 的做法是「按套餐到期升序排，取前 4 个」——排序 + `take(4)` 必然把流量压到少数账号上，我们不照搬。我们只抄它的**判断维度**，保留现有的「并列组 + 组内轮询」结构：

> **把「会作废且窗口内到期」当作一个分档门槛，而不是排序键。**

这样：同规格账号整体并列 → 行为与今天一致；只有当一部分账号的额度「下周就没了」时，才把它们提到前面。

### 4.2 与既有 promo 层的关系（重要修正）

现有第 3 层只看 `one_time`，会与到期维度打架：A 持有 20 天后过期的 `one_time`，B 的套餐 2 天后到期，现行逻辑选 A，B 的额度照样作废。

**把「到期门槛」插在 promo 之前**（`store.go:1821` 之前），promo 层原样保留：

```
资格 → 模型 → 断流回避 → 线路回避 → 【新：到期门槛】 → promo → 剩余最多组 → RR
```

- 为什么不是合并：`one_time` 里也有「20 天后才到期」的，它不紧急，不该压过「2 天后就作废」的套餐；但「没有更急的账号时」，promo 仍必须优先——这一层不能删。
- 为什么不是排序：排序 + 取前 N 就是 `zcode-pool` 的热点来源；门槛 + 组内轮询能保住分散。

判定函数语义：

```
willExpireSoon(a, model, now, window) = 存在额度列 e，满足：
    remaining(e) > 0
且 e 的到期时刻 t = expires_at ?? period_end（数值型，缺失则该列不参与）
且 t <= now + window
且该列不是「按日刷新」（period 含 one_time，或 period_end-period_start > 36h）
```

- 用 `expires_at` 优先、`period_end` 兜底，是因为前者更像「套餐/活动到期」，后者在日额度上是次日零点。
- `36h` 阈值专门排除 daily（span≈24h）；阈值写进注释，不设环境变量（避免参数爆炸）。
- **实现前必须先核对**：拉一次真实快照，确认 start plan 的 `expires_at` 是套餐结束时间、daily 列的 `span≈24h`。若不符，退回「只认 `one_time` + `expires_at`」，宁可不做也不乱抢。

### 4.3 代码落点

1. `internal/model/account.go` 新增：
   ```go
   // SoonestNonRefreshingExpiry 返回该模型「会作废且窗口内到期」的最早到期时刻（Unix 秒）。
   // 只读快照，不改状态；无此类额度列时 ok=false。
   func (a *Account) SoonestNonRefreshingExpiry(model any, now time.Time, window time.Duration) (expire float64, ok bool)
   ```
2. `internal/store/store.go` `selectAccount`：在 promo 分档**之前**插入
   ```go
   if window > 0 && modelName != "" {
       var expiring []*model.Account
       for _, a := range pool {
           if _, ok := a.SoonestNonRefreshingExpiry(modelName, now, window); ok {
               expiring = append(expiring, a)
           }
       }
       if len(expiring) > 0 { pool = expiring }
   }
   ```
   `modelName == ""` 时该层不生效（无模型维度可比，保持现状）；promo 分档原样保留在其后。
3. 配置：`ZCODE_SELECT_EXPIRE_FIRST_DAYS`（默认 `3`，`0` = 关闭）。环境变量而非 settings：这是一条调度策略，不需要面板常改。
4. `PoolStat` 增加只读计数 `ExpiringSoon int`，供 `no_available_account` 与面板解释「为什么选中它」，不复用不存在的字段。

### 4.4 测试（CI）

- `TestSelectPrefersSoonExpiringQuota`：A 有 2 天后到期的 1M、B 有 30 天后到期的 10M → 选 A。
- `TestSelectExpireBeatsPromo`：A 持 20 天后到期的 `one_time` 5M、B 套餐 2 天后到期 1M → 选 B（新语义，与旧版相反）。
- `TestSelectPromoStillWinsWhenNoExpiring`：A 持 20 天后到期的 `one_time` 5M、C 普通号 30 天后到期 10M → 选 A（promo 层未被削弱）。
- `TestSelectExpireTierNoopWhenAllExpiring`：全池都在窗口内 → 与旧行为逐次一致（RR 序列相同）。
- `TestSelectExpireIgnoresDailyRefresh`：span≈24h 的日额度不进档位 → 不改变选择。
- `TestSelectExpireWindowDisabled`：`days=0` → 与 v2.10.17 行为一致（黄金用例）。
- `TestSelectExpireSkipsZeroRemaining`：到期列 `remaining=0` 不算持有。

### 4.5 风险与回滚

- 若 `expires_at` 语义与预期不符，档位会误把「长期池」当短期池 → 行为退化为「多提一批账号」，不会造成 503；`days=0` 一键关闭。
- 不引入新的账号状态、不写 `accounts.data`。

---

## 五、P2：异步池补齐（修 G3）

### 5.1 范围

`asyncpool` 与 sync 的语义必须一致（项目硬不变式），本阶段只补「线级失败」这一层，不动风控判定：

1. `attemptUpstreamOnce` 包一层 watchdog（把 `firstbyte.go` 的类型与构造函数导出，或加一个 `gateway.NewAttemptWatchdog` 包装），超时 → 返回 `errNetwork{body:"上游首字节超时", avoid: identity}`；**不冷却账号**（async 现在对 `client.Do` 错误本就不标状态，保持不变）。
2. `errNetwork` 增一个 `avoid string` 字段（`pool.go:1013`）；外层循环 `errors.As` 取到后写入 `avoid` 集合。选号改用 `p.Store.SelectAvoiding`；`AsyncForceDirect` 分支需要新增 `SelectForDirectAvoiding`（现 `SelectForDirect` 没有 avoid 参数，`selectAccount` 已支持，只差一个包装）。
3. 408/524：`attemptUpstreamOnce` 同样返回带 `avoid` 的 `errNetwork`，`attemptUpstream` 计一次**请求级**预算（复用 `ZCODE_UPSTREAM_TIMEOUT_RETRIES`），用尽后原样投递。
4. 不做请求级 deadline：异步票已有 `AsyncTicketTimeout`（300s）墙钟，重复设限会让「长任务」与「慢线」互相顶牛。

### 5.2 测试（CI）

- `TestAsyncFirstByteTimeoutSwitchesLine`：首字节超时 → 换号且换线，账号不冷却。
- `TestAsyncAvoidExhaustedFallsBack`：全部候选被回避 → 仍能选出账号，不报 `no_account`。
- `TestAsyncTimeoutRetryBudgetShared`：两个账号各触发一次 408 → 只换一次（预算请求级共享）。
- `TestAsyncStreamNotCutByWatchdog`：收到响应头后长流不受影响。

---

## 六、P3：度量基建（修 G4，为 P5 决策）

### 6.1 内容

1. `ReqDiag` 新增 `Session string`，在 `engine.go:225` 与 async 对应位置从 `attr.SessionID` 赋值；`Format()` **行尾追加** ` session=%s`（空值打 `-`）。
2. 不改日志格式的既有字段顺序；现有测试用 `diagField(t, line, name)` 按字段名取值（`diagnostics_test.go:425`），追加字段不会破坏既有断言，只需补新字段的断言。
3. 取数方式（不写进代码，写进 `docs/analysis-*`）：用网关日志按 `session=` 分组，比较「同会话连续请求：`acc=` 不变 vs 变化」两组的 `cache_r/(in+cache_r)` 中位数。

### 6.2 决策阈值（写死，避免事后找理由）

| 实验 | 数据条件 | 判定 |
|---|---|---|
| sticky（会话粘性） | 同会话同号 vs 换号，样本各 ≥200 组 | 命中率差 ≥10pp 才实施 P5；否则放弃 |
| TTFB 加权轮询 | 同模型账号间 `firstbyte` p50 的 p90/p10 ≥1.3 | 是 → 单独立项（P6，本计划不排期）；否 → 维持纯 RR |

P3 只负责让数据可得；P5 的准入判定用第一行，TTFB 那一行不在本计划内实施。

---

## 七、P4：pinned 单号（API + 账号行 UI）

### 7.1 后端

- `Store` 新增进程内状态：`pinID string`、`pinUntil time.Time`（`s.mu` 保护，重启即清——调试用的钉子不该跨重启）。
- `selectAccount` 第一层之后：pin 生效且目标账号 `IsSelectable`/未被 skip、线路准入门通过 → 直接返回该账号（**不推进轮转游标**）；否则**静默回落**正常选号，并打一条 `pin_miss` 日志（含原因：冷却 / 停用 / 代理不可用 / 已过去）。
- API（前缀 `/admin/api`）：
  - `POST /accounts/{account_id}/pin` body `{"minutes":30}` → 200 `{"pinned":{id,name,until}}`；`minutes` 缺省 30、钳到 `5..1440`；已钉住别的账号时直接覆盖。
  - `DELETE /pin` → 清除（幂等）。
  - `GET /pin` → 当前状态（无则 `{"pinned":null}`）。
- `GET /admin/api/accounts` 响应顶层追加 `pinned`（同结构，无则 `null`）：账号行靠轮询（5 秒）拿状态，不必为钉住单独加一个查询。
- 诊断行追加 `pin=<命中|回落|->`，保证「这几条为什么都打同一个号」可追溯。

### 7.2 前端（`frontend/src/pages/accounts.tsx`）

- 行操作区（`HeartPulse` 那一排，`accounts.tsx:1043-1105`）加一个图标按钮：
  - 未钉住：`Pin` 图标 + tooltip「釘住 30 分鐘（真機複驗）」→ 直接 `POST /accounts/{id}/pin`（等价 `/pin` 带 account），成功后 toast「已釘住 · 30 分鐘」。
  - 已钉住在本行：`PinOff` 图标 + tooltip「解除釘住」→ `DELETE /pin`。
  - 其它行被钉住时仍可点，语义是「改钉到这一行」（后端覆盖）。
- 账号名旁加徽标：`釘住 · 剩 N 分`（`Badge variant="outline"`，与「探測專用」徽标同一行）；到点自动消失（下一次轮询刷新）。
- `frontend/src/lib/types.ts`：`AccountsResponse` 加 `pinned?: { id: string; name: string; until: number } | null`；`Account` 不加字段（钉住是全局单值，不是账号属性）。
- 故意不做设置页表单、不做时长选择器：真机复验只需要「钉住/解除」，需要更长 TTL 时用 API 的 `minutes`。
- 改完 `npm run build` 重建 `frontend/dist` 并一起提交（UI 繁中，与现有页面一致）。

### 7.3 测试

- `TestPinOverridesQuotaTier`：pin 的号剩余最少仍被选中。
- `TestPinFallsBackWhenCooling`：pin 的号在冷却 → 正常选号、不失败，`pin_miss` 有日志。
- `TestPinExpires`：TTL 到期自动失效。
- `TestPinDoesNotAdvanceRotation`：连续 pin 请求后，解除 pin 的首次轮转顺序未被打乱。
- `TestPinAPIRoundTrip`：POST → GET 可见 → DELETE 幂等清除；`minutes` 越界钳位。
- 前端无自动化测试（与现状一致），由 `tsc`/构建 + 手工点一遍三态（未钉 / 钉住本行 / 他行钉住）覆盖。

---

## 八、P5：会话粘性试点（默认关，P3 达标才实施）

### 8.1 为什么单列一阶段

- 收益是**假设**：同一账号的 prompt cache 命中更高。没有 P3 的数据，做了也只是「感觉更快」。
- 风险是**真实**的：`zcode-pool` 是**全局粘性**（所有请求钉在一个号上），那才是热点与 405 反复撞墙的来源。
- 我们只做**按会话**粘：不同会话仍由 RR 分到不同账号，天然分散；同一会话内部才复用。这跟抄它的 sticky 不是一回事。
- 准入：§六 第一行阈值达标（同会话同号 vs 换号，命中率差 ≥10pp，样本各 ≥200 组）。**不达标就不做**。

### 8.2 设计

- 状态：`Store` 进程内 `map[key]stickyEntry{accountID, until}`，容量上限 4096（超限按最老淘汰），
  `key = sha256(session_id + "|" + model)` 前 16 位——不存原始 session，日志里也只出现哈希前缀。
- 选号：在最终并列组（第 6 层 `top`）内判断：
  - 该 key 的绑定账号仍在 `top` 中 → 直接返回它（**不推进轮转游标**）；
  - 否则按现有 RR 选一个并写入绑定，TTL = `ZCODE_SELECT_STICKY_SECONDS`（试验值 600）。
- 解绑（立即，不等 TTL）：绑定账号在本次请求中命中 405 / 401 / 403 / 429、进入冷却、模型额度耗尽、断流回避。
  实现点：`gateway` 的失败分支在 `Store.Update` 之后按 `acc.ID` 清理所有指向它的绑定（一次遍历，容量有界）。
- 不做：全局粘性、跨模型粘（key 已含 model）、持久化（重启即清）。
- 开关：`ZCODE_SELECT_STICKY_SECONDS`，**默认 0（关闭）**；上线后验证期手动开，观察命中率与 405 率再决定是否改默认。

### 8.3 观测与验收

- P3 已给 `session=`、`cache_r=`；P5 再追加 `sticky=<hit|miss|off>`（行尾）。
- 验收：开 24h 后 `sticky=hit` 占比 >50%，且 405 率不高于关闭期；`cache_r/(in+cache_r)` 中位数较关闭期提升 ≥10pp。三条同时成立才考虑默认开启。

### 8.4 测试（CI）

- `TestStickySameSessionReusesAccount`：同 session 连续 10 次 → 同号（并列组内）。
- `TestStickyBrokenByRiskControl`：绑定号 405 → 下一次换号。
- `TestStickyRespectsTieGroup`：绑定号跌出并列组（额度被消耗）→ 换号。
- `TestStickyTTLExpiry`：TTL 到期后回到 RR。
- `TestStickyDisabledByDefault`：`=0` 时与 v2.10.19 行为完全一致（黄金用例）。
- `TestStickyKeyIncludesModel`：同 session 不同模型不互相粘。

---

## 九、明确不做（附理由）

| 项 | 理由 |
|---|---|
| 候选必须「有额度快照」 | `zcode-pool` 的硬伤：取不到额度就整池 502；我们的 `unknown` 后备是有意设计 |
| `MODEL_AUTO` 静默换模型 | 对外网关不得替客户端改模型，计费与语义都会错 |
| 405 不换号也不标记 | 单号场景能避免扩散，但等于让 sticky 反复撞同一个热号；我们已有「请求级 vs 账号级」判定，比一刀切细 |
| 无 TTL 的全局粘性 | 会把并发压到单号，与 60 号池的容量目标冲突 |
| `nextAccount` 预演 | 需要面板配合才有价值，而面板已偏复杂；运维收益低于成本，暂缓 |
| 严格按到期排序 + take(N) | 就是热点来源；我们只借维度、不借排序 |

---

## 十、验证与发布门禁

1. 每阶段一个 feature 提交（简中提交信息，`feat:`/`fix:` 前缀英文），推 `go-rewrite` 分支等 CI 全绿。
2. 版本提交只 bump `config.AppVersion` + 写 `docs/releases/vX-go.md`（tag 名逐字一致）+ 同步 README 顶部版本段。
3. 附注 tag 推上去后：`gh run watch <id> -R fujunchao/zcode2api-plus --exit-status`（不接管道）→ `.workbuddy/check-ghcr.js X.Y.Z-go` → 核对 Release 附件。
4. 任一阶段 CI 红：只修该阶段，不带病推进下一阶段。

## 十一、验收清单

- [ ] P0：5.5MB 慢线请求最坏 240s 内出结果（503 `request_deadline_exceeded`），账号冷却数为 0，长流不被截断。
- [ ] P1：构造「2 天后到期 vs 30 天后到期」两个账号，`Select` 稳定选前者；`days=0` 完全回到旧行为。
- [ ] P2：异步票首字节超时后换线成功，票内不出现重复内容块。
- [ ] P3：日志可按 `session=` 分组；两组 cache 命中率与 firstbyte 分布能直接算出来。
- [ ] P4：pin 生效 / 回落 / TTL 三条路径都有日志与测试；账号行按钮与徽标三态正确（未钉 / 钉住本行 / 他行钉住），`frontend/dist` 已重建并与源码同提交。
- [ ] P5（若 P3 达标并经批准）：`sticky=0` 时行为与 v2.10.19 完全一致；开启后 `sticky=hit` >50%、405 率不升高。
- [ ] 全程无新增 `accounts.data` 键；前端改动仅限 P4 的账号行按钮与徽标。
