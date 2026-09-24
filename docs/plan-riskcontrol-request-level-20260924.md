# 修复方案：风控的「请求级」与「账号级」分离

> 依据：`docs/analysis-riskcontrol-pool-cascade-20260924.md`（2026-09-24 整池雪崩事故）
> 目标版本：**v2.6.0-go**（行为变更，非纯修复）
> 本文只定方案，不含已实施代码。确认后按「阶段 1 → 2 → 3」分提交实施。

---

## 零、目标与验收标准

### 要解决的问题

上游拒的是**一个请求**，网关按**一个账号**结算，调用方又把这个动作**重放 14 次**。
三者相乘 = 67 个健康账号 × 300 秒。

### 验收标准（可证伪）

| # | 场景 | 当前行为（错） | 期望行为 |
|---|---|---|---|
| A1 | 同一请求体在 2 个不同账号上都被 405+风控文案拒绝 | 两个账号各冷却 300s | **0 个账号冷却**，向上游客户端原样返回 405；日志留下「判定为请求级」的一行 |
| A2 | 同一请求体只有 1 个账号被 405+风控，第 2 个账号 200 | 首个账号冷却 300s | **完全不变**：首个账号冷却 300s（账号级风控，换号有效） |
| A3 | 池中只有 1 个账号且被 405+风控 | 冷却 300s，请求 503 | **完全不变** |
| A4 | 同上一份 body 被调用方重放 N 次 | 每次烧掉最多 5 个账号 | **每次烧 0 个账号**（阶段 3 后可做到 0 次上游调用） |
| A5 | 405 风控命中 | 日志只有状态码，无 body | `[~]` 行带上游 body 预览（与 503 分支对齐） |

### 不可破坏的既有行为（回归红线）

1. 单账号池 / 只有一个可用账号时，风控行为与今天逐字相同 → `Test405RiskControlCoolsAndSwitches` 必须保持绿。
2. 非风控 405 原样透传且不冷却 → `Test405WithoutRiskBodyDoesNotCool` 必须保持绿。
3. 分类链 **9 条顺序不可变**（`internal/gateway/engine.go:350` 起，PLAN §5.2）。
4. `>>>` / `<<<` / `<!>` 三类行文本冻结（`internal/web/logs_test.go` 的 `TestLegacyLineFormatsFrozen`）。
5. `[#]` 诊断行**字段不增不减**（外部脚本按字段名解析，PLAN §5.14）。请求级判定只走 `[~]` 行，不加 diag 字段。
6. `attemptBudget` 的四类计数（captcha/busy/rate/overload）语义不变：**不给风控加预算项**，`budget :=` 上界不动。
7. sync 与 async 对同一账号必须写出相同字段（`internal/asyncpool` 与 `internal/gateway` 共用 `Mark*`）。
8. `Store.Update` 是唯一写入口且**锁内不可重入** → 回滚必须锁外逐账号调用。

---

## 一、方案总览

### 核心判据

> **风控是身份维度的信号。若同一请求体在 ≥2 个完全不同的身份（账号 + 出口线路）上被同一个风控信号拒绝，
> 身份已不是变量，剩下的只有请求体。**

阈值取 **2** 的依据：对照组（09-23 全天 17 次零星风控）**17/17 都在第 2 个账号上成功**，因此阈值 2 不会误伤任何一次真实的账号级风控。

### 三个阶段

| 阶段 | 内容 | 文件 | 必要性 |
|---|---|---|---|
| **1** | sync 路径请求级熔断 + 冷却回滚 + 日志补全 | `gateway/engine.go`、`gateway/classify.go`、新 `gateway/riskscope.go` | P0，本次事故直接病灶 |
| **2** | async 路径同语义（共用阈值与判定） | `asyncpool/pool.go` | P0 同批，防两条路径语义漂移 |
| **3** | payload 重放快速失败（TTL 记忆） | `gateway/engine.go` + `Engine` 字段 | P1 可选，把重放成本从「1 次上游调用」降到 0 |

### 被拒绝的替代方案（记录理由，避免反复讨论）

| 方案 | 否决理由 |
|---|---|
| **按 body 尺寸设阈值**（如 >256KB 拒绝） | 已反证：成功请求 max 687KB，`≥270KB` 成功 283 条。尺寸不是变量。 |
| **405 一律不冷却** | 会把真实的账号级风控（09-23 那 17 次）也放过，账号反复被选中再被拒，池子持续抖动。 |
| **「延迟惩罚」：先不清算第 1 个账号，等第 2 个账号结果再决定** | 语义更干净（不写再撤），但引入了「请求中途取消 / 客户端断开 → 真正该冷却的账号永不冷却」的漏网窗口，且给 `tryAccount` 增加了一个需要跨账号传递的待决状态。回滚方案的额外成本只是「锁内多捕三个字段」，零额外 IO。 |
| **把风控冷却整体改成「不写账号状态，只记日志」** | 同样丢掉账号级风控的处置能力。 |

---

## 二、阶段 1：sync 路径请求级熔断

### 2.1 新增文件 `internal/gateway/riskscope.go`

```go
// 单次请求内的风控观测范围：把「账号级风控」与「请求级风控」分开。
//
// 背景（2026-09-24 事故，docs/analysis-riskcontrol-pool-cascade-20260924.md）：
// 上游风控有两种语义，此前只有账号级一种处置——
//   - 账号级：换号即成功（09-23 全天 17/17 如此）；
//   - 请求级：同一 body 在多个不同账号 + 多个出口线路上全部被拒（当日 67/67），
//     此时换号纯属把健康账号一个个送出去挨打。
// 判据：同一请求内 ≥2 个**不同账号**给出同一风控信号 ⇒ 身份不是变量 ⇒ 请求级。
// 阈值 2 不误伤：对照组 17 次账号级风控全部在第 2 个账号上成功。
package gateway

// RiskControlRequestLevelThreshold 判定请求级风控所需的不同账号数。1 = 关闭判定
//（退回逐号冷却的旧行为，作为紧急回退开关），默认 2。
var RiskControlRequestLevelThreshold = 2

// riskScope 单次请求的判定范围。**必须按请求创建**：Engine 是跨请求共享的，
// 任何挂在 Engine 上的可变状态都会让并发的两个请求互相误判。
type riskScope struct {
	hit     map[string]bool // 已给出风控信号的账号 ID（按 ID 去重，同一账号重试不重复计数）
	cooled  []RiskControlSnapshot
}

func newRiskScope() *riskScope { return &riskScope{hit: map[string]bool{}} }

// verdict 记录一次风控命中并给出判定。返回 true 表示达到阈值、本次应判为请求级。
func (s *riskScope) verdict(acc *model.Account) bool {
	s.hit[acc.ID] = true
	return len(s.hit) >= RiskControlRequestLevelThreshold
}
```

### 2.2 `internal/gateway/classify.go`：前像/后像 + 回滚

`MarkRiskControl` 保持**对外语义与签名不变**（async 与既有测试都依赖它），新增一个返回快照的版本，前者是后者的薄包装——**两者必须共用同一段锁内逻辑**，分开实现迟早漂移。

```go
// RiskControlSnapshot 一次风控冷却的前像与后像。
//
// 只覆盖「调度效果」三个字段（Status / CoolingUntil / RiskControlStreak），刻意不含
// last_error/last_error_kind/last_error_at：那三个是**证据**，请求级判定成立不代表这个账号
// 没被上游拦过，抹掉会让后台再也看不见这次命中。且 isRiskControlInvalid 只在
// Status==invalid 时才看 kind，留痕不会误触发额度守卫。
type RiskControlSnapshot struct {
	Provider, ID string

	PrevStatus string
	PrevUntil  *float64
	PrevStreak int

	WrittenInvalid bool
	WrittenUntil   *float64
	WrittenStreak  int
}

func MarkRiskControlWithSnapshot(st *store.Store, provider, idOrName, errMsg string, now time.Time) (secs, streak int, invalid bool, snap RiskControlSnapshot)

// RollbackRiskControl 撤销一次风控冷却，用于请求级判定成立后释放被误伤的账号。
//
// 并发守卫：只在「当前值仍等于我们写入的值」时才回滚。若期间被管理员改状态、
// 或另一路风控/成功路径动过这个账号，说明决定权已不属于本请求，放弃回滚（返回 skipped）。
// 已升级 invalid 的账号**不回滚**：那意味着它的风控阶梯早已接近耗尽，与本次请求无关。
func RollbackRiskControl(st *store.Store, snap RiskControlSnapshot) (restored, skipped bool)
```

实现要点：

```go
func RollbackRiskControl(st *store.Store, snap RiskControlSnapshot) (restored, skipped bool) {
	if snap.WrittenInvalid {
		return false, true
	}
	_, _ = st.Update(snap.Provider, snap.ID, func(acc *model.Account) {
		if acc.Status != model.StatusCooling || acc.RiskControlStreak != snap.WrittenStreak ||
			!sameFloatPtr(acc.CoolingUntil, snap.WrittenUntil) {
			skipped = true
			return
		}
		acc.Status = snap.PrevStatus
		acc.CoolingUntil = cloneFloatPtr(snap.PrevUntil)
		acc.RiskControlStreak = snap.PrevStreak
		restored = true
	})
	return restored, skipped
}
```

`MarkRiskControlWithSnapshot` 的锁内体：先抓 `PrevStatus/PrevUntil/PrevStreak`，再执行**与今天逐字相同**的自增与选档，把写入值填进 `Written*`。`MarkRiskControl` 改为 `secs, streak, invalid, _ := MarkRiskControlWithSnapshot(...)`。

### 2.3 `internal/gateway/engine.go`：接线

**（a）`runWithAccounts` 建 scope 并下传**（`engine.go:155` 外层循环之前）

```go
scope := newRiskScope()
for range MaxAccountAttempts {
	acc := e.Store.Select(model.ProviderZai, tried, modelName)
	...
	res := e.tryAccount(ctx, reqID, acc, body, modelName, stream, incomingHeaders, deliver, diag, scope)
```

**（b）`tryAccount` / `handleUpstreamError` 各加一个 `scope *riskScope` 参数**
（各只有一个调用点：`engine.go:162`、`engine.go:304`，改动面最小）

**（c）`handleUpstreamError` 第 8 条分支重写**（`engine.go:511`）

```go
// 8) 405 + 风控文案：先分清是「账号级」还是「请求级」风控，再决定要不要惩罚账号。
//
// 账号级（换号即成功，09-23 全天 17/17 如此）→ 整号冷却 + 换号，与今日一致。
// 请求级（同一 body 在 ≥2 个不同账号上都吃同一风控信号，2026-09-24 67/67）→
// 身份已不是变量，剩下的只有请求体：此时换号是把健康账号一个个送出去挨打。
// 改为不冷却、不换号，原样返回上游 405，并回滚本请求此前已施加的风控冷却。
if resp.StatusCode == http.StatusMethodNotAllowed && model.IsRiskControlBody(text) {
	preview := ErrorPreview(text)
	if scope != nil && scope.verdict(acc) {
		restored, skipped := scope.rollback(e.Store)
		web.Warn(reqID, fmt.Sprintf(
			"风控判定为请求级（已在 %d 個帳號上復現），停止換號並回滾冷卻（回滾 %d、跳過 %d）: %s",
			len(scope.hit), restored, skipped, preview))
		return attemptResult{final: runResult{
			Status: resp.StatusCode,
			Body:   passthroughBodyWithType(text, "upstream_error"),
		}}
	}
	secs, streak, invalid, snap := MarkRiskControlWithSnapshot(e.Store, acc.Provider, acc.ID,
		"上游风控拦截 HTTP 405: "+preview, e.now())
	if scope != nil {
		scope.record(snap)
	}
	if invalid {
		web.Warn(reqID, fmt.Sprintf("账号 %s 连续第 %d 次命中风控，已置為失效待人工處理（HTTP %d，%s）",
			acc.Name, streak, resp.StatusCode, preview))
	} else {
		web.Warn(reqID, fmt.Sprintf("账号 %s 第 %d 次命中风控，冷却 %d s 后切换下一个（HTTP %d，%s）",
			acc.Name, streak, secs, resp.StatusCode, preview))
	}
	return attemptResult{switchAccount: true}
}
```

**（d）日志补全（A5）**：上面两条 `[~]` 行都补上 `resp.StatusCode` 与 `preview`。
现状（`engine.go:516`）只有账号名与秒数，这正是本次事故无法从日志定性的直接原因。

### 2.4 关键设计决定（写入代码注释，别在 review 里反复解释）

| 决定 | 理由 |
|---|---|
| 阈值按**不同账号 ID** 计数，不按尝试次数 | 同一账号的验证码重试/限流原地重试会重复进入本分支，按次数计会把「同一个号被拒两次」误判成请求级。 |
| 回滚**只回滚风控分支**施加的冷却 | 本请求内因 503/429 冷却的账号与本次 405 无关，回滚它们等于篡改别的判定。`riskScope.cooled` 只装风控快照。 |
| 回滚**保留** `last_error_kind=risk_control` | 证据不能被判定结果抹掉；且 `isRiskControlInvalid` 只在 `Status==invalid` 时读它，留痕不会让额度守卫误判。 |
| 已 invalid 不回滚 | 走到 invalid 说明该账号阶梯已耗尽，本次只是压垮它的最后一根稻草；复活它会让下一个请求再吃一次 405。 |
| 请求级判定返回**上游原始 405** | 保持与「非风控 405 原样透传」同一约定，客户端能拿到上游的真实文案与业务码；不要改写成 502/503，那会把上游的风控结论偷偷翻译成网关的故障。 |
| 请求级判定**不换模型** | 与账号级风控同理（`MarkRiskControl` 的既有注释）：请求体是同一份，换模型也是同一份。 |
| 不加 `[#]` diag 字段 | 诊断行字段被外部脚本按名解析，为一次判定扩字段收益低、破坏面大。判定信息放 `[~]` 行。 |

---

## 三、阶段 2：async 路径对齐

`internal/asyncpool/pool.go` 有三处与 sync 不一致，必须同批处理，否则「同一份 body 在 sync 安全、在 async 打穿池子」。

| 位置 | 现状 | 改动 |
|---|---|---|
| `pool.go:651` 405 分支 | `MarkRiskControl` + `errNetwork`（外层换号） | 加同一个 `riskScope`（挂在 `processTicket` 的 `for {}` 之前），达阈值时改为终止本票 |
| `pool.go:657` 日志 | 「進入冷卻並切換下一個」，无 body | 补 `ErrorPreview(bodyText)`（与 503 分支一致） |
| `pool.go:316` 外层 `for {}` | 换号上限 = `config.AsyncMaxRetries`（默认 3）+1 = **4 个账号/票**，且靠 `tried` 集合自然收敛 | 不新增上限；靠阶段 2 的熔断把请求级的消耗压到 0。**但要在这里补一句注释说明上限来源**（现在读代码看不出来） |

新增错误类型以区分「换号」与「终止」：

```go
// errRequestLevelRisk 请求级风控：不得换号，直接把上游原文投递给客户端并终止本票。
// 与 errDelivered 的区别是它仍要把上游错误体交给客户端（errDelivered 表示已投递过）。
type errRequestLevelRisk struct{ body string }
func (e errRequestLevelRisk) Error() string { return "request-level risk control: " + e.body }
```

在 `processTicket` 的错误分支里 `errors.As` 命中后：`p.emitError(ctx, ticketID, rl.body, "upstream_error")` → `return`（与 `errDelivered` 同一条出路，但不重复投递）。

**验证要求**：`internal/asyncpool/pool_test.go` 现有的 `Test405RiskControlInAsyncPool`（单账号，必须仍冷却）与 `Test405WithoutRiskBodyInAsyncPool`（必须仍不冷却）都要保持绿，并新增请求级用例。

---

## 四、阶段 3（P1）：payload 重放快速失败

### 动机

阶段 1 之后，一次重放的成本是「1 次上游调用 + 1 次冷却回滚」，账号不再损失；但 14 次重放 = 14 次上游调用，仍会持续给上游递同一个被判风控的请求（这在**请求级风控由内容触发**时可能加重上游对出口 IP 的印象）。

### 设计

```go
// Engine 新增（跨请求共享，必须带锁与容量上限）
type Engine struct {
	...
	riskMemoMu   sync.Mutex
	riskMemo     map[string]riskMemoEntry // key = payload 指纹
	RiskMemoTTL  time.Duration            // 默认 60s，0 = 关闭
}

type riskMemoEntry struct {
	until  time.Time
	status int
	body   string
}
```

- **指纹**：`RunMessages` 入参 `body map[string]any`，`encoding/json` 对 map 按键名排序 → `marshalJSON(body)` 的字节内容是**确定性的**，可直接 `sha256` 取前 8 字节 hex。不需要原始请求字节，改动面为零。
- **判定点**：`runWithAccounts` 循环开始前查表；命中则不发任何上游请求，直接返回缓存的 405 与 body（`attempts=0`，与今天的「無可用帳號」早退形态一致）。
- **写入点**：请求级判定成立处，`key = 指纹`，TTL 取 `RiskMemoTTL`。
- **容量**：上限 128 条，满则先清过期、再整体清空（风控记忆不是持久状态，宁可丢也不做 LRU 复杂度）。
- **不持久化**：进程重启即失效，符合「这类记忆只防短时重放」的定位。

⚠️ 缓存的是上游错误体（≤ `maxErrorBodyBytes` = 64KB）。必须限长后再存，且**不写入任何日志文件**（内容可能回显用户请求）。

---

## 五、阶段 4（P2）：可配置化

### 新增设定项（1 个）

| 项 | 键 | 默认 | 0 值语义 |
|---|---|---|---|
| 请求级风控阈值 | `risk_request_level_threshold` | 2 | 1 = 关闭判定（退回旧行为） |

沿用 `LineTruncateStrikes` 的既有形态（`store.go:1006-1019`）：`claimInt(key, config 默认, 下界)`。
**不同步做 `MaxAccountAttempts` 可配置化**：它是常量、被 `runWithAccounts` 的循环上界直接使用，改成配置要同步 3 处（含 async 侧的 `AsyncMaxRetries` 对照），收益远小于风险。熔断落地后，单请求的账号消耗已被压到「最多 1 个且会回滚」，没有再调的动机。

### 加一个设定项的完整清单（缺一处就静默失效）

1. `internal/config/config.go`：`RiskRequestLevelThreshold = envInt("ZCODE_RISK_REQUEST_LEVEL_THRESHOLD", 2)`
2. `internal/store/store.go`：`RiskRequestLevelThresholdKey` 常量
3. `internal/store/store.go`：`func (s *Store) RiskRequestLevelThreshold() int`（`claimInt`）
4. `internal/adminapi`：GET 返回 + PUT 校验并落库（与 `line_truncate_strikes` 同一批）
5. `frontend/src`：设置页输入框 + 文案（繁体 UI）；改完必须重建 `frontend/dist` 并提交（go:embed）
6. 设定项一致性测试（若有 `TestSettingsKeysCovered` 一类的守卫，同步加键）

---

## 六、守护测试清单

### 阶段 1/2

| 测试名 | 位置 | 断言 | 反向验证（把阈值置 1 → 必须变红） |
|---|---|---|---|
| `Test405RequestLevelRiskDoesNotCoolPool` | `gateway/engine_test.go` | 3 个账号、上游恒 405+风控文案：响应 405 + 上游 body 原文；**3 个账号 Status 全为 active、CoolingUntil 全 nil、RiskControlStreak 全 0** | ✅ 会红（阈值 1 时第 1 个账号即判请求级，虽账号数仍为 0——故本测试的**主断言应是回滚生效**：`RiskControlStreak==0` 且 `LastErrorKind==risk_control`） |
| `Test405RequestLevelRiskRollsBackFirstAccount` | 同上 | 断言「证据保留、调度恢复」的组合：`Status==active` / `CoolingUntil==nil` / `RiskControlStreak==0` / `LastErrorKind==risk_control` | ✅ |
| `Test405AccountLevelRiskStillCools` | 同上 | 2 个账号：第 1 个 405+风控、第 2 个 200 → 第 1 个仍是 cooling + streak 1，第 2 个 active，响应 200 | ❌ 与阈值无关（守 A2，防熔断扩大化） |
| `Test405RiskControlCoolsAndSwitches`（既有） | `engine_test.go:621` | 单账号仍冷却 300s | ❌ 必须始终绿（守 A3） |
| `Test405WithoutRiskBodyDoesNotCool`（既有） | `engine_test.go:653` | 非风控 405 原样透传、不冷却 | ❌ 必须始终绿 |
| `Test405RiskControlLogsUpstreamBody` | `gateway/logs_test.go` 或 engine_test | `[~]` 行含 `ErrorPreview` 内容（用 `web.SetOut(w)` 捕获） | ✅ |
| `Test405RiskControlInAsyncPool`（既有） | `asyncpool/pool_test.go:841` | 单账号仍冷却 | ❌ 必须始终绿 |
| `Test405RequestLevelRiskInAsyncPool`（新增） | 同上 | 多帐号同 body → 0 冷却 + 上游原文投递 + `error` 事件一次 | ✅ |
| `TestRollbackRiskControlSkipsOnConcurrentChange` | `gateway/classify_test.go` | 快照写入后再由第三方改 `Status` → 回滚返回 `skipped`，不覆盖 | — |

### 阶段 3

| 测试名 | 断言 |
|---|---|
| `TestPayloadRiskMemoFastFailsReplay` | 同一 body 连发 2 次：第 1 次 405 并写记忆，第 2 次 **`upstreamCall` 计数不增加**、响应体与第 1 次一致 |
| `TestPayloadRiskMemoKeyIsBodyOrderIndependent` | 同内容不同键序的两个 JSON 打出同一指纹（防 map 序列化顺序假设被破坏） |
| `TestPayloadRiskMemoExpires` | `RiskMemoTTL` 置 1ms，过期后重新走上游 |

### 反向验证流程（沿用既有约定）

1. **先写测试**（此时生产代码未改）→ 跑 `go test ./internal/gateway/ -run '405RequestLevel' -count=1`，**必须红**；
2. 实施阶段 1 → 同一命令**绿**；
3. **反向验证**：把 `RiskControlRequestLevelThreshold` 置 1 → 主断言（回滚生效）**必须重新变红**，证明测试真的在守这条行为；
4. 恢复 → `"D:/Program Files/Go/bin/go.exe" test ./... -count=1` 全量绿（`internal/captcha` 的 `TestExtractTarGzPreservesSymlink` 本机必失败，属沙箱符号链接降级，**不改测试**）；
5. `node .workbuddy/gofmt-check.js`。

---

## 七、发版与线上验证

### 发版（沿用既有流程）

1. `config.AppVersion` bump 到 `2.6.0`（无 v 前缀）；
2. 写 `docs/releases/v2.6.0-go.md`，说明：行为变更（405 风控新增请求级判定）、新增设定项、日志格式变化（`[~]` 行新增 body 预览，可能影响既有日志解析脚本 → 这正是选 `[~]` 而非 `[#]` 的原因）；
3. 三个提交：`feat(gateway): 区分请求级与账号级风控`（阶段 1+2）→ 如需阶段 3 单独一笔 → 前端/`dist` → `chore: release`；
4. 推分支等 CI 绿（`gh run list --branch go-rewrite`）→ 打轻量 tag `v2.6.0-go`；
5. 推前跑 `node .workbuddy/redact-scan-all.js`（本次新增的 docs 已脱敏，仍需全仓扫一遍）。

### 线上验证（灰度顺序）

1. **先只观察**：上线后确认 `[~]` 行的 body 预览是否出现、账号级风控是否仍按 300/900/3600 递进（对照组不能消失）；
2. **人为复现**：用同一份会触发风控的 body 连续发 2 次（可用本地直连 z.ai 的脚本，或等下一次真实触发），确认出现 `风控判定为请求级` 行且后台账号状态未被改动；
3. **阈值可回退**：若发现误判（账号级风控被当成请求级），把后台阈值改为 1 即时退回旧行为（阶段 4 落地后），或改常量重发版（阶段 4 未落地时）；
4. 观察 24 小时：确认 `no_available_account` 的 503 数量与「风控冷却账号数」的日曲线回到 09-23 的量级（全天 ≤20 次风控、无整池事件）。

---

## 八、工作量与风险

| 阶段 | 改动面 | 风险 | 风险缓解 |
|---|---|---|---|
| 1 | `engine.go` 3 处、`classify.go` 2 个函数、新文件 1 个、测试 4 个 | 回滚与并发写入竞争 | 并发守卫（对比写入值）+ 专项测试 |
| 2 | `asyncpool/pool.go` 3 处、测试 1 个 | 与 sync 语义漂移 | 共用 `gateway` 的阈值与 `Mark*`；两边各写一个「单账号仍冷却」用例 |
| 3 | `engine.go` 入参指纹 + 记忆表、测试 3 个 | 记忆表内存增长 / 指纹碰撞 | 容量 128 + TTL 60s + 指纹取 8 字节；碰撞后果仅为一次错误的快速失败，1 分钟内自愈 |
| 4 | config/store/adminapi/frontend 共 6 处 | 新增设定项漏同步 → 静默失效 | 按第 5 节的六点清单逐项核对 |

**建议的落地顺序**：阶段 1 + 阶段 2 同批（互相依赖，缺一不可）→ 直接发 v2.6.0-go；阶段 3 视「重放是否仍在发生」决定是否进同一版；阶段 4 留待有第二个需要调的旋钮时一起做（单独为它拉一版前端不值得）。
