# 实施计划：线路级 405 归因（v2.10.21 → v2.10.23）

> 依据：`docs/plan-405-line-attribution-20261017.md`（v2.10.20 · 20:50–22:26 日志分析）
> 决策已定：**P0-1 阈值 2** / **P0-3 阈值 3 且只计干净线路** / **P1-1 首次 405 只换线不冷却** / **重放 TTL 保持 60 s**
> 覆盖三版：**v2.10.21**（第四~十一节，提交 #1–#5）· **v2.10.22**（第十四节，提交 #6–#9）· **v2.10.23**（第十六节，提交 #10–#11）
> 状态：**三版均已实施并发布**——v2.10.21（`f889786` 画像 · `3f7f680` 归因+换线+首次不冷却 · `a428a8f` 发布）· v2.10.22（`88d3327` 巡检触发+结论回写 · `0e4845d` removed_ids · `6ffa958` 发布）· v2.10.23（`e6e895e` 容量口径 · `e7a02ec` 发布）。三个 tag 的 5 个附件与 GHCR 均已核对。

## 一、目标

把"405 打断整条请求"改成"405 换一条线路继续"。具体三个可量化目标：

| 指标 | 现状（本次日志） | 目标 |
|---|---|---|
| 用户被 405 打断 | 9 次 / 75 请求 | ≤ 2 次 |
| 零出站重放拦截 | 79 次 | ≤ 5 次 |
| 被误冷却的账号 | 18 个 | ≤ 3 个 |

非目标：不消除上游 405（做不到），不改请求内容，不动慢线/大包方案。

## 二、现状代码路径

| 位置 | 现在做什么 | 问题 |
|---|---|---|
| `gateway/engine.go:779-812` | 405 → `MarkRiskControl`（账号冷却阶梯 + 换该号绑定）→ `switchAccount` | 冷却错对象；**不带 avoid**，下一枪可能还在同一条线 |
| `gateway/riskscope.go:49` | `Verdict` 数**账号**，2 个即判请求级 | 本次 9 次里 8 次误判 |
| `gateway/riskscope.go:72` | `MarkRiskControl` = `ApplyRiskControl`（冷却）+ `RotateAccountProxyAt` | 两者耦合，无法只换线 |
| `asyncpool/pool.go:921-948` | 同语义，但返回 `errNetwork`（无 avoid） | async 与 sync 必须同步改 |
| `store/store.go:1791` | `selectAccount` 单一候选池 | 没有"线路已被判脏"这一层 |
| `store/proxy_strict.go:262` | `chooseProxyLocked` 只影响**新绑定** | 已绑在脏线上的账号不受影响 |
| `model/riskcontrol.go:33` | `ApplyRiskControl` 强制写 `Status=cooling` | 无法"只记错误不冷却" |

## 三、设计

### 3.1 线路级 405 画像

新文件 `internal/store/line405.go`，结构参照 `ProxySoftFailure`：

```go
type Line405Failure struct {
    Identity        string `json:"identity"`          // strictProxyIdentity(URL)，别名归并
    ProfileID       string `json:"profile_id"`
    Generation      uint64 `json:"generation"`        // 线路配置代次，变了就重置
    Consecutive405  int    `json:"consecutive_405"`   // 连续 405 次数（成功即清零）
    Total405        int    `json:"total_405"`         // 累计（仅供后台展示）
    OK              int    `json:"ok"`                // 累计成功（仅供后台展示）
    Strikes         int    `json:"strikes"`           // 已隔离次数（决定续期档位）
    FirstAt         int64  `json:"first_at"`
    LastAt          int64  `json:"last_at"`
    QuarantineUntil int64  `json:"quarantine_until,omitempty"`
}
```

持久化复用软失败那套 meta 键模式：

- 键名 `line_405_state`（`const line405Key`）；
- 写：`commitLine405Locked(now)`（裁剪后 `INSERT OR REPLACE INTO meta`）；
- 读：`store.go:296` 同一段 `settings[...]` 里 `json.Unmarshal`，随后 `delete(settings, key)`；
- 过滤：`store.go:488` 与 `proxy_subscription.go:387` 两处白名单各加 `line405Key`；
- 结构体字段加在 `store.go:91` 附近（`line405 line405State`）。

### 3.2 判定规则（连续计数）

```
RecordLine405(e, now):
    if e.Mode != "proxy" or e.Identity() == "" : return ignored   // 直连不参与
    ent := entries[e.identity]
    if ent.Generation != e.Generation: ent = 新建(gen=e.Generation)  // 线路被编辑过，旧账作废
    ent.Consecutive405++
    ent.Total405++
    ent.LastAt = now
    if ent.Consecutive405 >= config.Line405MinStreak(2):
        ent.Strikes++
        d := config.Line405Quarantine(30m) × 2^(strikes-1)，封顶 240m
        ent.QuarantineUntil = max(ent.QuarantineUntil, now+d)
        commit
        return "quarantine"

RecordLineOK(e, now):
    if ent 存在: ent.Consecutive405 = 0; ent.Strikes = 0; ent.OK++; ent.LastAt = now
    // 一次成功即证明这条线能用，立刻解封
```

要点：

- **连续**而非累计：一条时好时坏的线不会被隔离（对免费代理很重要）；
- `Consecutive405` 在隔离到期后不清零 ⇒ 放出来第一枪再 405 会**立刻**二次隔离并升档（30→60→120→240 分钟）；
- 直连/`direct_fallback` 不建档（没有"线路"可隔离）；
- 用 `Generation` 兜住"线路被改过"的情况，避免旧账误伤新配置。

### 3.3 选号接入（两级候选，必须有兜底）

`store.go:selectAccount` 在构造候选时同时记两个切片：

```go
blocked := s.line405BlockedLocked(now)   // map[identity]bool
for _, a := range s.accounts[provider] {
    if 现有资格闸门通过 {
        base = append(base, a)
        if !blocked[s.boundLineIdentityLocked(a)] { preferred = append(preferred, a) }
    }
}
pool := preferred
if len(pool) == 0 { pool = base; line405Fallback = true }   // 绝不返回"无可用账号"
```

- `boundLineIdentityLocked(a)`：`a.ProxyURL` 为空 → `"direct"`；否则 `strictProxyIdentity(url)`；
- 兜底命中时打 `[~] line405 relax=1 candidates=%d blocked=%d`（60 秒限频，复用 `logProxyRelaxLocked` 的写法）；
- `chooseProxyLocked`（新绑定/风控换线/删除改派）把线路隔离集合并进 `proxySelectionBlockedLocked`，与软失败隔离共用同一出口。

### 3.4 请求级判定改按线路

`gateway/riskscope.go`：

```go
type RiskScope struct {
    contentKey string
    lines      map[string]bool   // 计入判定的线路（去重键）
    accounts   map[string]bool   // 仅用于日志
}

// lineDirty: 本次 405 之前该线路是否已被判脏（隔离中）
func (s *RiskScope) Verdict(acc *model.Account, egress store.ProxyEgress, lineDirty bool, now time.Time) bool {
    s.accounts[acc.ID] = true
    key := lineKey(egress)               // 直连统一记 "direct"
    if !lineDirty { s.lines[key] = true }
    return len(s.lines) >= config.RiskRequestLevelLines   // 3
}
```

- `lineKey` 必须在 `RecordLine405` **之前**判定（否则本枪的 405 会把自己算成脏线，永远凑不满阈值）；
- 调用点两个：`engine.go:781`、`asyncpool/pool.go:923`；
- `scope.Accounts()` 保留（日志仍要显示命中账号数）。

### 3.5 首次 405 只换线不冷却

新增 `store` 侧的内存态（不落库，重启丢失可接受）：

```go
account405 map[string]struct{ Identity string; At time.Time }   // TTL 2h
func (s *Store) NoteAccount405(accID, identity string, now time.Time) (firstOnAccount bool)
```

判定：该账号在 TTL 内**没有**在别的线路上 405 过 → `firstOnAccount = true`（本次算线路的锅）。

`gateway/riskscope.go` 新增（不改正 `MarkRiskControl` 的签名，避免动 4 个调用点）：

```go
func NoteRiskControlWithoutCooling(st, snapshot, errMsg, now, attempt...)
    // 写 LastRiskControlAt / LastErrorKind=risk_control / LastError
    // 不动 Status、CoolingUntil、RiskControlStreak
    // 复用抽出的 rotateRiskProxy(...) 完成换线
```

`MarkRiskControl` 内部把"换线"那段抽成 `rotateRiskProxy`，两边共用，避免两套逻辑漂移。

engine 405 分支新流程：

```
dirtyBefore := st.Line405Quarantined(egress, now)     // 只读
st.RecordLine405(egress, now)                          // 可能触发隔离
if scope.Verdict(acc, egress, dirtyBefore, now) {      // ≥3 条干净线路
    MarkRiskControl(...)          // 内容级：保留原冷却（罕见，~2%）
    replay.Record(contentKey)
    return 405
}
if config.RiskCoolOnFirst == 0 && st.NoteAccount405(acc.ID, egress.Identity(), now) {
    NoteRiskControlWithoutCooling(...)                 // 只换线
} else {
    MarkRiskControl(...)                               // 同号换过线仍 405 → 升阶梯
}
return {switchAccount: true, avoid: egress.Identity()}
```

### 3.6 两条路径一致性（硬不变式）

async 侧必须给出与 sync 完全相同的判定：

- 新增 `errLine405{body, avoid string}`（**不要给 `errNetwork` 加字段**——它有 8 处位置式字面量，加字段会全红，且语义也不对）；
- 在 `pool.go:596` 附近按 `errLineTimeout` 的写法处理：加入 `avoid`、`networkRetry = true`，**不消耗 timeout 预算**；
- 405 分支复用同一套 `RecordLine405 / Verdict / NoteAccount405 / NoteRiskControlWithoutCooling`；
- `RiskAttempt{Egress: egress}` 已存在，直接复用。

### 3.7 隔离 ≠ 删除（必须写死）

线路 405 隔离**只影响选号**，不产生任何删除决定，也**不排入网络复核队列**：

- 网络探针是**无凭据的匿名 GET**，脏 IP 对 GET 是通的 ⇒ 复核必然判"恢复"，排进去只会白跑一轮；
- 现有删除链的门槛也远高于"2 次 405"（见下表），所以不会误删。

现有全部删线路径与门槛：

| 机制 | 触发条件 | 是否删除 | 本次窗口发生 |
|---|---|---|---|
| 线路 405 隔离（本计划） | 连续 2 次 405 | ❌ 仅隔离 30→240 min | 未上线 |
| 软失败隔离 | 30 min 内 ≥3 次失败且 ≥50% | ❌ 仅隔离 + 改派 | 0 |
| 网络复核双证 | 两轮探测失败 **且** 独立对照（另一条线或直连）成功 | ✅ 删除 + 改派 | **99 条** |
| 严格代理证据 | 同线同内容 ≥2 次，或 ≥3 账号/≥3 请求/≥2 内容且对照成功 | ✅ 退休删除 | 0 |
| 连续断流熔断 | 同线连续 3 次上游断流 | ✅ 删除 + 改派 | 0 |
| 人工 | 后台单条/批量删除 | ✅ | — |

删除还有三道闸门（`proxy_transactions.go` / `proxy_protection.go`）：
`Policy.Mode == "auto_retire"`（`observe_only` 一律不删）、保护名单（手动保护、默认出口、托管默认线路）、配置代次与绑定快照必须仍然一致。

**结论**：405 脏线在当前系统里几乎不可能被删——本次 21 条"零成功"的线全程留在池中继续返回 405，正是缺少本计划这一层导致的。

## 四、改动清单

| 文件 | 改动 |
|---|---|
| `internal/store/line405.go` **新增** | 结构、规则、隔离查询、后台列表、commit/prune |
| `internal/store/line405_test.go` **新增** | 连续计数/成功清零/隔离续期/代次重置 |
| `internal/store/line405_selection_test.go` **新增** | 脏线账号被跳过；全脏时兜底仍能选号 |
| `internal/store/store.go` | 结构体加 `line405`；`:296` 加载；`:488` 过滤；`selectAccount` 两级候选；`boundLineIdentityLocked` |
| `internal/store/proxy_strict.go` | `proxySelectionBlockedLocked` 合并线路隔离 |
| `internal/store/proxy_subscription.go` | `:387` 过滤键加 `line405Key` |
| `internal/store/account405.go` **新增** | `NoteAccount405`（内存 + TTL + 定期清理） |
| `internal/gateway/riskscope.go` | `Verdict` 改线路；`lineKey`；`NoteRiskControlWithoutCooling`；抽出 `rotateRiskProxy` |
| `internal/gateway/engine.go` | 405 分支重写（顺序见 3.5）；`avoid` 加入 |
| `internal/gateway/proxy_observation.go` | `observeModelAttemptHealth` 成功分支调 `RecordLineOK` |
| `internal/asyncpool/pool.go` | 405 分支同步改造；新增 `errLine405` 与消费点 |
| `internal/config/config.go` | 5 个新变量（见第五节） |
| `internal/adminapi/model_probe.go` | `handleHealthSummary` 增加 `line405` 字段 |
| 日志 | 见第六节 |

**不动**：`model.Account` 结构（不新增持久化字段，避免"加字段同步五处"）、`ApplyRiskControl` 语义、重放防护实现、`IsEndpointUnreachable`。

## 五、配置项

| env | 默认 | 说明 |
|---|---|---|
| `ZCODE_LINE405_MIN_STREAK` | 2 | 连续 405 达到几次隔离；0 = 关闭线路隔离 |
| `ZCODE_LINE405_QUARANTINE_MINUTES` | 30 | 首档隔离时长 |
| `ZCODE_LINE405_MAX_QUARANTINE_MINUTES` | 240 | 续期封顶 |
| `ZCODE_RISK_REQUEST_LEVEL_LINES` | 3 | 判定内容级所需的不同干净线路数；2 = 旧行为 |
| `ZCODE_RISK_COOL_ON_FIRST` | 0 | 0 = 首次 405 不冷却（只换线）；1 = 旧行为 |

全部走 `config` 包级变量 + 环境变量，不新增面板项（与软失败一致）。

## 六、观测

- 每次 405：
  `[~] line405 id=proxy-xxx route=… s405=2 ok=0 action=quarantine until=2026-10-17T23:20:00+08:00`
- 隔离生效：
  `[~] line405-quarantine id=… until=… strikes=1`
- 判定：
  `[~] risk-verdict lines=3 accounts=3 dirty_skipped=1 action=request_level`
- 不冷却：
  `[~] 账号 X 命中风控（HTTP 405，线路 Y），本次换线重试（不冷却）`
- 兜底：
  `[~] line405 relax=1 candidates=2 blocked=2`
- 后台：`GET /admin/api/health/summary` 返回并列字段
  `"line405": [{"profile_id","route","consecutive_405","total_405","ok","strikes","quarantine_until"}]`
  （本版只做 API，不做前端表格，避免重建 `frontend/dist`）

## 七、测试计划

**store/line405_test.go**
1. 连续 2 次 405 + 中间无成功 → 隔离 30 min；
2. 2 次 405 之间夹 1 次成功 → 不隔离（连续计数被清零）；
3. 隔离到期后再 405 → 立刻二次隔离且升档 60 min；
4. 隔离期内成功 → 立即解封；
5. 同一 URL 的两种写法（别名）归并到同一 identity；
6. `Generation` 变化 → 计数重置；
7. 直连 / `direct_fallback` 不建档；
8. commit → 重新 Load 后状态一致。

**store/line405_selection_test.go**

9. 账号绑在隔离线上 → 不被选中；
10. 全部账号都在隔离线 → **仍能选出账号**（兜底），且日志标记 `relax=1`；
11. 隔离到期 → 账号回到候选。

**gateway/riskscope_test.go（扩）**

12. 3 条不同干净线路 → `Verdict` 为真；
13. 2 条 → 假（旧行为回归）；
14. 同一条线两次 405 → 只算 1；
15. 命中时有 1 条线路已是隔离态 → 它不计入（2 干净 + 1 脏 ≠ 3）。

**gateway/risk_first405_test.go（新）**

16. 首次 405：`Status` 仍为 active、`CoolingUntil` 为空、`LastErrorKind=risk_control`、代理已换；
17. 同号换到另一条线再 405 → 进入冷却（300 s）且 `RiskControlStreak=1`；
18. `ZCODE_RISK_COOL_ON_FIRST=1` → 首次即冷却（回退开关可用）。

**gateway/engine_405_test.go（新）**

19. 405 后下一枪的线路 identity 与上一枪不同（avoid 生效）；
20. 3 条线都 405 → 返回 405 + 重放防护已登记（真内容级回归）。

**asyncpool/pool_test.go（扩）**

21. async 405 也带 avoid，且不消耗 timeout 预算；
22. async 与 sync 对同一序列给出相同判定（表驱动，同一组输入跑两条路径）。

**回归**

23. 503 阶梯、软失败隔离、双证删除、`no_available_account` 均不受影响；
24. `go vet` + 全量 `go test -count=1` 由 CI 跑（本地不跑，按 AGENTS.md）。

## 八、提交拆分与发布

| # | 提交 | 内容 |
|---|---|---|
| 1 | `feat: 线路级 405 画像与隔离` | `store/line405.go` + 选择接入 + 配置 + 测试 1–11 |
| 2 | `feat: 405 换线重试与按线路判定请求级` | `riskscope` + `engine` + `asyncpool` + 测试 12–15、19–22 |
| 3 | `feat: 首次 405 只换线不冷却` | `NoteRiskControlWithoutCooling` + 测试 16–18 |
| 4 | `feat: 线路 405 观测与后台摘要` | 日志 + admin summary |
| 5 | `chore: 发布 v2.10.21-go` | `config.AppVersion` + `docs/releases/v2.10.21-go.md` + README |

流程按 AGENTS.md：分支 CI 绿 → 附注 tag → 等 `release.yml` 复用 CI 通过 → 核对 Release 附件与 GHCR（`gh run watch ... -R fujunchao/zcode2api-plus`，镜像 tag 不带 `v`）。

## 九、验收

用本次日志的形状做回放验证（可直接用 `.workbuddy/tmp-logs/z142616/zcode2api-raw.log` 的 114 次尝试序列做离线模拟，不进 CI）：

- [ ] 9 个 405 内容簇中，≥ 7 个在第 2–3 枪成功；
- [ ] 重放防护 arm 次数 ≤ 2；
- [ ] 被冷却账号 ≤ 3；
- [ ] 脏线在第 2 次连续 405 后不再被选中（`line405-quarantine` 日志出现且后续无该线）；
- [ ] 全池脏线时仍能出号（不出现 `no_available_account`）；
- [ ] 真内容级（3 条干净线全 405）仍返回 405 且登记重放防护。

## 十、风险与回退

| 风险 | 回退方式 |
|---|---|
| 免费代理波动大，误隔离可用线路 | `ZCODE_LINE405_MIN_STREAK=0` 关闭；或提高到 3 |
| 不再冷却账号，真热号多打两枪 | `ZCODE_RISK_COOL_ON_FIRST=1` 恢复旧行为 |
| 阈值 3 太宽，真内容级多烧 1 个号 | `ZCODE_RISK_REQUEST_LEVEL_LINES=2` 恢复旧阈值 |
| 全池隔离导致选不出号 | 两级候选兜底（设计上保证）；若仍异常，关闭线路隔离即可 |

## 十一、明确不做

- 不改 `IsEndpointUnreachable`（慢 ≠ 死）。
- 不延长重放防护 TTL（60 s 保持）。
- 不改写请求内容、不重置 device id、不静默降级模型。
- 不新增 `Account` 持久化字段。
- 不做慢线上行闸门（独立议题，等这条验证完再评估）。

## 十二、遗留

1. **模型巡检（probe）路径**：`store/model_probe_dispatch.go:453` 仍直接 `ApplyRiskControl`，会因脏线冷却账号（本次 20:51 那簇就是 probe）。它需要 lease 上的 egress 快照，改动比主链路大，列为下一版；
2. 前端线路健康表（`consecutive_405 / ok / quarantine_until` 可视化）；
3. 若 P1-1 上线后仍出现"同号多线 405"，再评估是否恢复 14 h 级账号标记。

## 十三、线路清理：为什么选"带凭据探针"而不是降低 strict 门槛（v2.10.22 候选）

目标：把"零成功的 405 脏线"真正从池子里清掉，而不只是隔离。

### 13.1 两条路对比

| | 降低 strict 证据门槛 | 定向模型巡检（推荐） |
|---|---|---|
| 证据来源 | 真实用户流量 | 受控小请求，2 轮确认 |
| 额外成本 | 0 请求 | 每线 ≤2 次小请求 |
| 触发难度 | **高**：要求 `ContentRef` 完全相等，而客户端重试会让 body 漂移 32–96 字节 ⇒ 本次窗口 **0 次决策** | 低：主动发起，不受内容漂移影响 |
| 误删风险 | 需同时放宽内容匹配，等于削弱证据 | 有 target/reference 差分对照，全局风控时不会误删 |
| 现有实现 | `strictDecisionFor`（3012 内容证据链，改动敏感） | `retireModelProbeLocked` 已具备（2 轮 + auto_retire + 保护名单 + 墓碑） |
| 是否写订阅墓碑 | 是 | 是（`proxy_subscription.go:177` 会跳过，删了不会又被拉回） |

**关键事实**：strict 的对照要求 `bad.ContentRef == good.ContentRef`（`proxy_strict.go:389`）。本次日志里重试体量差 32–96 字节，语义相同但指纹不同 ⇒ 严格路径自然沉默（`strict-proxy` 日志 0 条）。要让它生效必须放宽内容匹配，那是把"确凿证据"降级成"大致相似"，风险大于收益。

### 13.2 方案

1. `RecordLine405` 触发隔离时，顺带入队一次定向巡检：
   `QueueModelProbe(profileID, "line405", now)`（`model_probe_dispatch.go:68`）；
2. **限频**：同一线路 30 分钟内只入队一次（避免隔离续期反复触发）；
3. 巡检按现有逻辑跑 2 轮，`probeConfirmedRoundsLocked >= 2` 且 target/reference 有差分 ⇒
   `retireModelProbeLocked` → `startStrictRetirementLocked("model_probe_route_control")` → 删除 + 改派 + 订阅墓碑；
4. 巡检判"健康"（线路只是偶发 405）⇒ **不删除**，并把 `Strikes` 归零、隔离不再升级；
5. 受 `Policy.Mode == auto_retire`、保护名单、`EvidenceIncompleteUntil` 等既有闸门约束，与手工探针同一套。

### 13.3 成本与收益

- 成本：按本次 21 条脏线算，约 **42 个小请求 / 96 分钟**（0.4 次/分钟），消耗极小额度；
- 收益：这 21 条线从此不再进池，预计后续 405 次数从 33 次/96 分钟降到个位数；
- 对比现状：现在这些线会持续吃流量、烧账号（本次 18 个号被冷却），清理成本更低。

### 13.4 风险

| 风险 | 处置 |
|---|---|
| 探针内容本身被上游判风控 | target/reference 差分：所有线路都失败时不会产生"某条线更差"的结论，不会误删 |
| 巡检消耗账号额度 | 用巡检专用的最小请求（低 max_tokens），且只对被隔离线路触发 |
| 免费池线路 IP 会恢复 | 墓碑是永久性的，但订阅若给出**新 URL** 会作为新线路导入，不受影响 |

### 13.5 不做

- 不放宽 `strictDecisionFor` 的内容匹配（保持"确凿证据"语义）；
- 不对全池做定期模型巡检（只在 405 隔离后定向触发，避免额度浪费）。

## 十四、v2.10.22 实施条目：405 线路清理（巡检联动）

### 条目 1｜隔离触发定向巡检（含限频与预算）

- **位置**：`store/line405.go` 的 `RecordLine405` 末尾（持锁内）调用 `s.queueModelProbeLocked(profileID, "line405", now)`。
- **为什么用 locked 变体**：`QueueModelProbe` 会再次 `s.mu.Lock()`，在 `RecordLine405` 内调用会自锁。
- **限频**：`line405ProbeAt map[string]time.Time`，同一 profile **30 分钟**内只入队一次（隔离续期不重复触发）。
- **预算**：全局每小时最多 `ZCODE_LINE405_PROBE_PER_HOUR`（默认 12）次；超限只记 `probe_budget_exhausted`，不影响隔离。
- **前置条件**：`Policy.Enabled` 且 target 被允许；不满足时静默跳过，不得把隔离回滚。
- **延迟**：Manager 通过 `Store.TakeModelProbe` 在下一个 tick 取走，可接受 30–60 s；不强制 `Wake`（引擎侧没有 Manager 句柄，不为此加依赖）。
- **测试**：隔离触发一次入队 / 30 min 内二次隔离不再入队 / 巡检关闭时静默跳过且隔离仍生效 / 超预算跳过。

### 条目 2｜巡检结论回写线路画像

- 新增 `Store.ApplyLine405ProbeVerdict(profileID, verdict string, now time.Time)`：
  - `healthy`（含 `recovered`）→ `Consecutive405=0`、`Strikes=0`、`QuarantineUntil=0`：**提前解封**；
  - `failed` / `retired` → 保持隔离，删除由既有退休链（`retireModelProbeLocked`）完成，**这里绝不自己删**；
  - 其他/未知 → 不动。
- **调用点**：`model_probe_dispatch.go` 在 run 收尾、结论落库之后。
- **测试**：健康结论解封；失败结论不解除；未知结论不改状态；对不存在的 profile 幂等。

### 条目 3｜观测

- 日志：`line405-probe queued id=… source=line405 budget=N`、`line405-probe verdict=healthy id=…`、`line405-probe skipped reason=disabled|budget|cooldown`。
- `GET /admin/api/health/summary` 的 `line405[]` 增加 `probe_state`（`none|queued|running|healthy|failed`）。

### 条目 4｜配置

| env | 默认 | 说明 |
|---|---|---|
| `ZCODE_LINE405_PROBE_COOLDOWN_MINUTES` | 30 | 同线路定向巡检最小间隔 |
| `ZCODE_LINE405_PROBE_PER_HOUR` | 12 | 全局每小时定向巡检上限；0 = 只隔离不巡检 |

### 条目 5｜删除日志补 `removed_ids`（巡检可复盘）

- **位置**：`modelprobe/network.go:359` 的 `stage=confirmed` 日志行。
- **现状**：只有 `removed=14` 计数，事后无法核对删了哪条线，也无法复盘误删。
- **改法**：`ApplyNetworkHealth` 已返回 `[]string removed`，直接拼接：
  `removed_ids=p1,p2,…`（最多 20 个，超出追加 `+N`），单行长度上限 512 字符。
- **注意**：`stage=probe` 行不加（那里还没删），只加在 `stage=confirmed`；`removed=0` 时省略该字段，保持日志紧凑。
- **测试**：删 1 条时日志含该 id；删 25 条时前 20 个 + `+5`；删 0 条时无 `removed_ids` 字段。

### 提交拆分

| # | 提交 | 内容 |
|---|---|---|
| 6 | `feat: 405 隔离触发定向模型巡检` | 触发点 + 限频 + 预算 + 测试 |
| 7 | `feat: 巡检结论回写线路画像` | `ApplyLine405ProbeVerdict` + 调用点 + 测试 |
| 8 | `feat: 线路 405 巡检观测` | 日志 + admin `probe_state` + `removed_ids`（条目 5） |
| 9 | `chore: 发布 v2.10.22-go` | 版本号 + release notes |

## 十五、代理巡检是否需要优化

**结论：需要，但只补三处，骨架不动。**

### 15.1 已经对的，不要动

| 现状 | 证据 |
|---|---|
| 全量轮 30 min + 定向复检 | 本次 96 分钟：`whole=1` 3 次、`whole=0` 107 次，符合设计（30 min 一轮全量） |
| 双证删除（两轮 + 独立对照） | `removed` 与 `checked` 比例合理，最大单轮 48 查 14 删 |
| 保护名单 | 默认出口 / 托管默认线路 / 手动保护均未误删 |

### 15.2 补强 A（P0，已并入 v2.10.22）：巡检要能"看见 405"

网络探针是**匿名 GET**，脏 IP 对 GET 是通的 ⇒ 结构上永远测不到 405。本次 21 条零成功的线路，全部靠真实流量才暴露。
必须靠定向模型巡检补这个维度（第十四节条目 1–2）。

### 15.3 补强 B（P1，**已并入 v2.10.22 条目 5**）：删除日志补 `removed_ids`

现在只有 `removed=14`，事后无法核对删了哪条线，也无法复盘误删。
改：`stage=confirmed` 行加 `removed_ids=[p1,p2,…]`（最多 20 个，超出记 `+N`），并在后台保留最近一轮明细。

### 15.4 补强 C（P1，**已拆为 v2.10.23 实施条目，见第十六节**）：改派加"单线绑定软上限"

本次删了 99 条线，改派会把账号压到剩余线路上；上一份日志出现过 **24 个号同绑 home-epg**（一条 19 KB/s 的慢线全池共担）。
改 `leastLoadedProxy`：增加软上限（默认 8），优先选未超限的；全部超限才回落（不做硬约束，避免 `auto_pending_proxy` 堆积）。

### 15.5 不做

- 不改全量轮频率（30 min）。
- 不改探针内容（GET 根路径，6 s）。
- 不降双证门槛。
- 不在巡检里做"带 body 的上传测速"（那是慢线议题）。

### 15.6 优先级

```
v2.10.21  line405 隔离 + 换线 + 判定（用户无感优先）
v2.10.22  定向巡检 + 结论回写 + removed_ids
v2.10.23  改派软上限（+ 可选：线路状态统一视图）
```

**遗留**：`NetworkHealthyAt` 目前不参与派发，`line405` 又是另一套状态。后台最终应合并成一张"线路状态表"（连通性 / 405 连续次数 / 隔离到 / 最近巡检结论），但那是展示层重构，不进本三版。

## 十六、v2.10.23 实施条目：改派单线绑定软上限（容量口径）

> 背景：本次 96 分钟删了 99 条线，每次删除都触发改派；更早的日志出现过 **24 个账号同绑 home-epg**（一条 19 KB/s 的慢线全池共担）。账号怎么分，决定了单条线变差时的影响面。
>
> ⚠️ **实施时更正（2026-10-18）**：`leastLoadedProxy` 本来就选绑定最少的线路，而
> 「未超限」的绑定数必然严格小于「已超限」的，所以「先选未超限、再回落」的两趟
> 逻辑**与单趟结果必然一致**——软上限不能改变选线结果，也变不出线路。24 个号同绑
> 一条线的真实原因是当时只有那一条线可用（其余被隔离/删除），属于容量问题。
> 因此本版把它定位为**容量口径 + 告警**，而不是「分散器」；真正要分散只能加线路。
> 原「单线绑定数 ≤ 8」的验收标准随之作废。

### 条目 1｜`leastLoadedProxy` 支持软上限（语义等价 + 容量口径）

- **位置**：`store/proxy_selection.go:40`。
- **签名**：`leastLoadedProxy(profiles, occupancy, excludeID, excludeURL string, cap int) (ProxyProfile, bool)`。
- **两趟选择**：先在 `occupancy[p.ID] < cap` 的线路里挑最少；一条都没有则回落全量
  （**不做硬约束**，避免 `auto_pending_proxy` 堆积）。
- `cap <= 0` → 单趟，不看上限。
- **调用点**：`store/proxy_strict.go:chooseProxyLocked` 两处（`allowed` 与 `eligible` 回落），`PickAvailableProxyProfileExcluding` 间接经过；逐处传入 `config.ProxyMaxBindingsPerLine`。

### 条目 2｜日志（容量不足 + 落点分布）

- 当选中的**最空闲**线路 `bindings >= cap` → 记
  `[~] line-cap capacity_short=1 cap=8 bindings=24 candidates=1`（60 秒限频）。
  这是「线路数不足」的唯一可见信号，仍然照常分配。
- 改派结束补落点分布：`[~] proxy-reassign assigned=11 top_targets=p1:6,p2:4,p3:3,+1`
  （按绑定数降序、同数按 ID 升序，最多 3 条其余折 `+N`）。

### 条目 3｜配置

| env | 默认 | 说明 |
|---|---|---|
| `ZCODE_PROXY_MAX_BINDINGS_PER_LINE` | 8 | 单线绑定容量口径；0 = 关闭判定（不告警） |

### 提交拆分

| # | 提交 | 内容 |
|---|---|---|
| 10 | `feat: 改派单线绑定软上限与容量告警` | `leastLoadedProxy` + 调用点 + 日志 + 配置 + 测试 |
| 11 | `chore: 发布 v2.10.23-go` | 版本号 + release notes |

### 测试

1. 未超限线路存在时（9 / 8 / 3 绑定，cap=8 / 4 / 0）结果恒为绑定最少的那条；
2. 全部 ≥ cap → **仍能分配**（不产生 pending），落回绑定最少的一条；
3. `cap=0` → 与改造前行为一致（回归）；
4. 选中线路已达 cap → 日志出现 `capacity_short=1`，且**仍返回线路**；
5. 改派落点聚合：`p1:6,p2:4,p3:3,+1`；`top<=0` 输出全部。

### 验收

- [ ] 线路数充足时，新绑定始终落在绑定最少的线路上（与改造前一致）；
- [ ] `capacity_short` 在「最空闲线路也已超限」时出现，可据此扩池；
- [ ] `auto_pending_proxy` 数量不因本改动上升（因为从不拒绝分配）；
- [ ] 删线后的落点分布可从 `top_targets` 直接读出。

### 风险

| 风险 | 处置 |
|---|---|
| 误以为软上限能自动分散 | 文档与代码注释写明语义；分散只能靠增加线路 |
| 池子太小导致全部回落 | 回落即旧行为，功能不退化，只记 `capacity_short` |

### 明确不做

- 不做硬上限（会产生 pending）。
- 不做「主动再平衡」（把已绑账号从超限线路挪走）：那会改动既有绑定，需单独评估。
- 不改 `proxyOccupancyLocked` 的口径（仍统计停用/归档账号，避免"看起来有空位"）。
- 不把 cap 做成面板项（与 line405 参数一致，走环境变量）。
