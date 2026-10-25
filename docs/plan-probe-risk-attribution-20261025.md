# 巡检 405 不再惩罚业务账号（方案 A）— 实施计划

日期：2026-10-25
状态：已实施，随 v2.11.2-go 发布（保留回滚开关；A+ 延后按日志评估）
关联：`docs/plan-line405-implementation-20261017.md` §十二 遗留 1（本计划收口该项）
分支建议：`codex/probe-risk-attribution`（从 `go-rewrite` 拉出，不叠在 `codex/proxy-protect-window` 上）

---

## 0. 问题

模型巡检（带凭据的定时巡检 + `line405` 定向复核）在 **screen / target 阶段**拿到 HTTP 405 风控响应时，
`store.CompleteModelProbe` 会立刻对**执行巡检的账号**调用 `model.ApplyRiskControl`：

- 写 `status=cooling`、`cooling_until`（默认阶梯 300/900/3600 秒，超档置 `invalid`）；
- 写 `last_risk_control_at`、`risk_control_streak`、`last_error_kind=risk_control`、
  `last_error="模型巡检收到风控响应 HTTP 405"`。

而 screen/target 的 405 本来就只说明「**这条线**被上游拦了」，责任归属要等 reference 对照才成立
（`probeAccountCorrelatedLocked` 要求同一账号在 target + reference **都** 405、另一个账号两条线都成功）。
即使最终 verdict 是 `route_correlated`（线路责任）、线路被退役，这次账号冷却与错误痕迹也**不会回滚**。

主要代价：

1. 健康账号被冷却 5–60 分钟（若运维显式设了 `ZCODE_RISK_SILENT_HOURS>0`，还会连带进静默窗口，
   退出业务选号）；
2. 账号列表出现误导性的「风控」错误，运维会把它当成账号问题排查；
3. **拖慢巡检自身**：target 405 冷却后，reference 对照要等 `CoolingUntil` 结束才开跑
   （`probeRecheckAt`），每轮至少多 5 分钟，线路退役被推迟；
4. 极端情况：连续 405 且一直没有对照成功时，阶梯会一路升到 `invalid`。

证据：代码 `internal/store/model_probe_dispatch.go:451-461`；已知遗留见
`docs/plan-line405-implementation-20261017.md:343`（「模型巡检（probe）路径仍直接 ApplyRiskControl，
会因脏线冷却账号，需要 lease 上的出口快照，列为下一版」）。日志侧该链路在跑：
7.5h 样本里 `line405-probe` 76 条结论（54 failed / 22 healthy）+ 10 queued + 7 retire。

---

## 1. 目标与非目标

### 目标

- screen / target 阶段的 405 **不再写任何账号状态**，只保留巡检 run/proofs 与线路证据；
- reference 阶段（参考线路本应干净）的 405 **保留**现有的账号风控冷却；
- 线路证据、判定链、退役条件、巡检统计全部不变；
- 提供无损回滚开关，上线后可按日志灰度。

### 非目标

- 不改主链路 `gateway/riskscope.MarkRiskControl`（真实流量 405 继续冷却账号 + 换线）；
- 不改 `advanceModelProbeLocked` 的 verdict / evidence 逻辑；
- 不改巡检账号统计窗口（`recordProbeAccountOutcomeLocked`）——它的设计已经隔离：
  probe 的 risk 条目不进入加权 rate / tier（`model_probe_accounts.go:60-83`）；
- 不处理「参考线自身脏导致 reference 误罚」——列为二期 A+；
- 不改 probe 成功时「治愈」账号（清 streak、cooling→active）的语义。

---

## 2. 设计

### 2.1 判定规则

| 巡检角色 | 405 风控响应 | 账号状态 |
|---|---|---|
| `screen`（全量轮/自动轮转筛查） | 只作为「目标线路嫌疑」证据 | **零改动** |
| `target`（诊断目标线） | 只作为线路证据（2 轮确认 + 对照） | **零改动** |
| `reference`（参考线，本应干净） | 账号侧证据成立 | 保留 `ApplyRiskControl`（冷却/失效阶梯） |
| 任意角色成功 | 保留「治愈」：`streak=0`，`cooling→active` | 不变 |
| 其他 kind（timeout/connection_failed 等） | 不写账号 | 不变 |

**为什么是 reference**：判定链已经把「账号有问题」定义为同一账号在 target 和 reference 都 405、
且另一个账号两条线都成功。reference 是账号侧最早成立的证据点；target/screen 的 405 在归因前
罚号是「先惩罚后归因」，也是本次要修的因果错位。

### 2.2 代码改动

#### (1) `internal/config/config.go`：新增回滚开关（推荐，3 行）

放在 `ZCODE_LINE405_*` 巡检配置段落之后：

```go
// ProbeRiskPenalty 巡检 405 时对执行账号的惩罚范围：
//   reference（默认）只惩罚 reference 阶段（参考线失败=账号侧证据）；
//   all 恢复旧行为（screen/target/reference 全部惩罚）；
//   off 巡检完全不写账号状态（只记录 run/proofs 与线路证据）。
// 非法值按 reference 处理：宁可少罚，不可误罚。
ProbeRiskPenalty = env("ZCODE_MODEL_PROBE_RISK_PENALTY", "reference")
```

> 决策点：若希望零新增配置，可去掉开关、固定为 reference；本文推荐保留，
> 因为这是生产账号状态的行为变更，需要一条不重新发版的回退路径（与
> `ZCODE_LINE405_*` 的既有风格一致）。

#### (2) `internal/store/model_probe_dispatch.go`：门控 + 可审计日志

当前（`CompleteModelProbe`，第 451–461 行）：

```go
if current && account.AcceptsRequestResult(l.account) {
    if kind == "risk_control" {
        model.ApplyRiskControl(account, "模型巡检收到风控响应 HTTP 405", now, s.RiskCoolingSteps())
    }
    if kind == "success" && o.Complete {
        account.RiskControlStreak = 0
        if account.Status == model.StatusCooling {
            account.Status = model.StatusActive
        }
    }
}
```

改为：

```go
if current && account.AcceptsRequestResult(l.account) {
    if kind == "risk_control" && probeRiskPenaltyApplies(l.role) {
        secs, streak, invalid := model.ApplyRiskControl(account, "模型巡检收到风控响应 HTTP 405", now, s.RiskCoolingSteps())
        // 锁内不能同步写 stdout；量很小（仅 405），异步留审计线。
        go web.Warn("model-probe", fmt.Sprintf("risk run=%s target=%s role=%s account=%s penalty=account streak=%d cooling_s=%d invalid=%t",
            l.runID, l.routeID, l.role, account.ID, streak, secs, invalid))
    } else if kind == "risk_control" {
        go web.Warn("model-probe", fmt.Sprintf("risk run=%s target=%s role=%s account=%s penalty=skipped",
            l.runID, l.routeID, l.role, account.ID))
    }
    if kind == "success" && o.Complete {
        account.RiskControlStreak = 0
        if account.Status == model.StatusCooling {
            account.Status = model.StatusActive
        }
    }
}
```

同文件新增纯函数（便于单测，不依赖 Store）：

```go
// probeRiskPenaltyApplies 决定巡检 405 是否惩罚执行账号。
// target/screen 的 405 只是「目标线路嫌疑」，账号责任要等 reference 对照；
// 只有 reference 失败才构成账号侧证据。
func probeRiskPenaltyApplies(role string) bool {
    switch config.ProbeRiskPenalty {
    case "all":
        return true
    case "off":
        return false
    default: // reference（含非法值）
        return role == "reference"
    }
}
```

要点：

- `current` / `AcceptsRequestResult` 守卫**原样保留**（迟到响应、账号已换凭据/停用时仍不写）；
- 请求里已带上 `l.role`（screen/reference/target），不需要新增 lease 字段或 DB 迁移；
- 日志在锁内，必须 `go web.Warn`（`web.Warn` 是同步 stdout 写，阻塞会卡住 Store 锁——与
  `proxy_protection.go` 的既有约定一致）；
- `recordProbeAccountOutcomeLocked` 不动：probe 的 risk 条目本来就排除在 rate/tier 之外。

### 2.3 不改的东西（明确边界）

- `advanceModelProbeLocked` 的 screen→reference→target 状态机、2 轮确认、`PausedUntil` 退避；
- `probeAccountCorrelatedLocked` / `retireModelProbeLocked` / 严格退役与保护名单；
- `line405` 隔离、`ApplyLine405ProbeVerdict` 的 healthy/failed 回写；
- 网络巡检、软失败隔离、额度刷新（`quota_query_failed` 的写账号是另一条路径，不在本方案）；
- 主链路真实流量 405 的冷却 + 换线。

---

## 3. 测试计划

不本地跑测试（仓库规范：自动化验证只在 GitHub Actions）。新增文件：
`internal/modelprobe/probe_risk_penalty_test.go`（复用 `newFixture` / `f.drive` 与
`riskMessage`、`completeMessage`，见 `manager_test.go`）。

1. **TestProbeTargetRiskDoesNotTouchAccount**
   - target 恒 405、reference 恒 200 → 跑完 `drive`；
   - 断言 verdict=`route_correlated`、`DecisionID` 非空、线路被退役（现有
     `TestModelProbeHTTPComparableRouteControlRetires` 的判定仍成立）；
   - 断言两个账号 `RiskControlStreak==0`、`LastRiskControlAt==nil`、`LastErrorKind==nil`、
     `Status` 未被置 cooling/invalid；
   - 断言 run.Samples 里仍保留 `kind=risk_control` 证据（巡检观测不受影响）。

2. **TestProbeReferenceRiskCoolsOnlyFailingAccount**
   - 构造差分：target 恒 405；reference 仅对 `accounts[0]` 返回 405，对 `accounts[1]` 返回 200；
   - 断言 `accounts[0]` 有风控痕迹（`LastErrorKind=risk_control`、streak≥1、`LastRiskControlAt` 非空），
     `accounts[1]` 零改动；
   - 断言线路仍按 route_correlated 退役（归因链不受影响）。

3. **TestProbeRiskPenaltyEnvAllRestoresLegacy**
   - `config.ProbeRiskPenalty="all"` → target 405 也冷却账号（旧行为），`t.Cleanup` 恢复；
   - 直接改 config 全局，不用 `t.Setenv`（该变量 init 已求值），且本用例不 `t.Parallel`。

4. **TestProbeRiskPenaltyOff**
   - `config.ProbeRiskPenalty="off"` → reference 405 也不写账号。

5. **回归**：给现有 `TestModelProbeHTTPComparableRouteControlRetires` 追加「账号零风控痕迹」断言，
   作为 route_correlated 场景的长期守门；跑全量 `internal/modelprobe` + `internal/store` 确认无回归。

预计生产代码 diff ≈ 40 行，测试 ≈ 180 行。

---

## 4. 灰度与回滚

| 变量 | 默认 | 说明 |
|---|---|---|
| `ZCODE_MODEL_PROBE_RISK_PENALTY` | `reference` | `all` = 旧行为（全部角色都罚）；`off` = 巡检完全不写账号；非法值按 `reference` |

- 无需数据迁移；已有账号上的 `last_risk_control_at` 保留为历史观测，冷却按其时间自然结束，不回填。
- 回滚：设 `all` 重启即可恢复旧行为；只观察可设 `off`。
- 发布门禁遵循 AGENTS.md：分支 CI 全绿后才谈合并/发版；发版另需 `release.yml` 复用完整 CI。

---

## 5. 验收标准

1. **CI**：目标提交 SHA 的 `ci.yml` 全绿（test -race、frontend dist 一致性、vet、交叉编译等）；
2. **日志**：出现 `model-probe risk role=target account=... penalty=skipped` 与
   `model-probe risk role=reference ... penalty=account`，比例与预期一致（target/screen 占绝大多数）；
3. **功能**：脏线仍能走完「真实流量 405 → 隔离 → 模型巡检 2 轮对照 → 退役」，`line405-probe verdict=failed` 与 retire 日志照旧；
4. **账号侧**：`route_correlated` 退役后，执行巡检的账号不再新增
   `last_error="模型巡检收到风控响应 HTTP 405"`，不再因巡检进入 cooling；
5. **性能**：巡检单轮不再被 5 分钟冷却卡住（reference 对照立即执行），单位时间可完成更多诊断。

---

## 6. 风险与后续

| 风险 | 评估与缓解 |
|---|---|
| 真脏账号只在 target 阶段 405、reference 恰好成功 → 巡检不罚 | 业务真实流量 405 仍由主链路冷却 + 换线兜底；巡检只放弃「提前惩罚」，不放弃归因判定 |
| 参考线自身脏 → reference 405 仍可能误罚账号 | 现状不变；二期 A+ 用「另一账号在同一条 reference 成功」的差分门控 |
| 无 reference 可用时巡检完全不罚账号 | 结论仍 `inconclusive` + 30 分钟退避；账号风险交给业务链路 |
| 账号在巡检请求期间被业务冷却，probe success 又清掉该冷却（既有并发边界） | 本方案不改变该行为，列为独立观察项；出现频次高再单独处理 |

**二期（A+）触发条件**：上线后若日志出现 reference `penalty=account`、但该账号随后在业务流量上稳定成功
（说明参考线才是脏的一方），再给 reference 加「同参考线其他账号成功」的差分门控。

---

## 7. 实施步骤

1. 从 `go-rewrite` 拉 `codex/probe-risk-attribution`（不叠分支，避免与已推的 `proxy-protect-window` 相互阻塞）；
2. `internal/config/config.go`：新增 `ProbeRiskPenalty` 与注释；
3. `internal/store/model_probe_dispatch.go`：新增 `probeRiskPenaltyApplies` + 门控 + 异步风险日志 + 注释；
4. 新增/扩展 `internal/modelprobe/probe_risk_penalty_test.go` 与既有用例断言；
5. 文档：`docs/guide-proxy-model-health.md` 增一节「巡检 405 的账号处置边界」；
   `README.md` 与 `docs/releases/vX.Y.Z.md` 在发版时补；历史计划文档不改；
6. 本地只做 `gofmt -l`（无输出）与差异自查；**不跑** `go test/vet/build`、前端测试；
7. 推送 origin 分支，`gh run watch` 盯完整 CI；失败按远端日志修；
8. 合并/发版等用户明确指令后再做（本计划只到「分支 CI 绿 + 汇报 SHA」）。

---

## 8. 决策记录（2026-10-25 确认）

1. **保留 `ZCODE_MODEL_PROBE_RISK_PENALTY` 回滚开关**：已确认。默认 `reference`；
   `all` 为旧行为回退；`off` 供只观察场景。非法值按 `reference`。
2. **A+（reference 差分门控）延后，先看日志**：已确认。上线后按新增的
   `model-probe risk ... penalty=account` 日志评估，建议观察 1–3 天且累计 ≥20 条
   reference 惩罚样本。若满足以下任一条件再启动 A+：
   - 同一 reference 惩罚事件之后 24 小时内，该账号在业务请求上持续成功（说明参考线才脏）；
   - reference 惩罚占全部巡检 405 的比例 > 30%，且其中多数账号随后业务表现正常；
   - 运维反馈账号列表出现明显的「巡检导致的风控」误报。
3. 日志行已补 `run=` / `target=` 字段，用于按 run 关联 run samples 与后续账号业务表现，
   让上面的判定不需要额外加探针。

---

## 9. 发布与核对结果（v2.11.2-go，2026-10-25）

- **合并**：`codex/proxy-protect-window`（fa85855 → 61378e3）与 `codex/probe-risk-attribution`
  （d58af80 → e71545e）以 cherry-pick 线性合入 `go-rewrite`；发布提交 `8552428`
  （`chore: 发布 v2.11.2-go`）。
- **分支 CI**：`go-rewrite` 8552428 全绿（run 40203275763，12/12 jobs）。
- **发布**：annotated tag `v2.11.2-go` 指向 8552428；release 工作流 run 40203845063 全绿
  （validate 复用完整 ci.yml + build 5 平台 + image 双架构）。
- **Release 附件**（5 个，均上传成功）：linux/darwin × amd64/arm64、windows/amd64。
- **容器镜像**：`ghcr.io/fujunchao/zcode2api-plus:2.11.2-go` 与 `:latest`，
  manifest digest `sha256:c0c5fb8016a46062b8cb51ea199b523999ab6566c2149442d8cda5a45027ded0`，
  平台 linux/amd64 + linux/arm64。
- **Release 说明**：来自 `docs/releases/v2.11.2-go.md`，GitHub Release body 已核对。
- **远端状态**：origin/go-rewrite 已推送；tag 与 release 均已推送且可匿名拉取镜像。
