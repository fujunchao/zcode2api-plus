# 风控失效账号「手动恢复」方案（v2.10，2026-10-12）

> 契约文档。实现落点与语义边界以此为准；PLAN.md §5.13「恢复出口」为摘要版。
> 分支 `go-rewrite`；涉及 `internal/model`、`internal/store`、`internal/adminapi`、
> `internal/quota`、前端账号页与 `frontend/dist`。

## 1. 背景与范围

风控连续命中超过阶梯档位数（默认第 4 次）后账号被置为 `Status=invalid` +
`LastErrorKind=risk_control`，是「待人工处理」的终态：

- 额度探测被 `quota.apply` 的 `protectInvalid` 挡住（不让旁路观测解封）；
- `MarkSuccess` 对 invalid 早退（成功流水不复活）；
- `applyEnabled` 明确不复活（启停不是解封后门）。

此前唯一的人工救济是**换凭据**（`EditAccount` 的 `SetSecret` 分支，需重新 OAuth 登录）。
本方案补上缺失的显式救济入口：**单条**与**批量**「手动恢复」端点 + 账号页 UI 按钮。

**范围**：仅「风控失效」（`invalid` 且 `last_error_kind=risk_control`）。
冷却中（会自然到期）、凭据失效（另有额度探测恢复路径）、额度耗尽（走刷新）、
已归档（先恢复到账号池）**不在本出口**。

**语义边界（必须在对管理员可见处说明）**：恢复只清**本地**状态，**不解除上游
仍生效的账号级标记**（实测 ≥14h）；若上游仍在拦截，恢复后账号会重新进入冷却
（`RiskControlStreak` 从 0 起算，重新走阶梯）或再次失效。

## 2. 判定与动作契约

| 项 | 决定 |
|---|---|
| 判定（单一来源） | `model.IsRiskControlInvalid(acc)`：`Status==invalid && LastErrorKind==risk_control`。`quota.apply` 的额度保护与恢复出口共用本函数（原 quota 私有实现已收敛，行为零变化）。 |
| 动作（单一实现） | `store.applyRecovered(acc)`：`Status→active`、`CoolingUntil→nil`、`RiskControlStreak→0`、`LastError/LastErrorKind/LastErrorAt→nil`。单条与批量结构性共用。 |
| 与 SetSecret 同语义 | 换凭据即重新可用；清 `RiskControlStreak` 的理由同：不清则下次命中=老计数+1，立刻又失效，等于人工修复无效。 |
| 不动 | `Enabled`（恢复与启停正交：停用中的账号恢复后仍不可调度，需显式启用）、`ArchivedAt`（归档正交）、`RateLimitStreak`/`Upstream503Streak`（与换凭据一致；503 连击刻意保留）。 |
| 拒绝集合 | 已归档 → `ErrAccountArchived`；其它一切状态 → `ErrAccountNotRecoverable`。均映射 400。 |
| 并发安全 | 单条：`Store.RecoverAccount` 专用持锁方法（形态对齐 `SetEnabled`），判定与写入同锁内，无 TOCTOU；批量：单锁 + 单 SQLite 事务，事务落「转移后的克隆快照」，commit 成功后才对 live 对象应用（失败回滚内存零改动）。 |
| 与其它机制 | 不经 `applyEnabled`（不是启停后门）；不触发额度刷新、不改 `ExhaustedModels`；与 ReplayGuard/重放防护无关（纯状态管理）。恢复后 `protectInvalid` 自然失效（status 已非 invalid），后续额度观测按既有语义更新状态。 |

## 3. 接口

### 3.1 单条

```
POST /admin/api/accounts/{account_id}/recover      （Admin Key 鉴权）
→ 200 {"ok": true}
→ 404 {"detail": "账号不存在"}
→ 400 {"detail": "账号非风控失效状态"}
→ 400 {"detail": "账号已归档，请先恢复到账号池再恢复"}
```

成功时记一条审计日志：`[OK] [account] 账号 <id> 已手动恢复（风控失效 → 正常，streak=<n>）`。

### 3.2 批量

```
POST /admin/api/accounts/batch/recover             （Admin Key 鉴权）
请求体：{"ids": [...], "missing_ok"?: bool}
→ 200 批量汇总（BatchResult）：逐条 ok / not_found / error("账号非风控失效状态"
     或"账号已归档，请先恢复到账号池")
→ 400 严格模式（缺省 missing_ok=false）含缺失 ID：整批拒绝、零改动
```

语义与其它批量端点一致：`missing_ok` 只管 `not_found`；不可恢复项**逐条 error**
（目标集合由管理员显式选择，常见「筛选后全选」，混入不可恢复项是预期内的，
逐条回报比整批拒绝有用）；去重保序；上限 `MaxBatchSize=500`。

## 4. 前端（账号页）

- **行内按钮**：`isRiskControlInvalid(a)` 时在「重置 Token 統計」与启停之间渲染
  `HeartPulse` 图标按钮，title `手動恢復（風控失效）`。仅异常行渲染，常态行队形不变。
- **确认框**（useConfirm，非破坏色）：标题「手動恢復帳號」，确认文案「確認恢復」；
  描述说明「只清本地標記」、「不會解除上游仍生效的風控標記（實測可持續 ≥14 小時）」，
  并显示最近风控时间（`relativeTime(last_error_at)`）。
- **批量栏**：新增「批量恢復」按钮（在批量停用与批量删除之间），确认后
  `POST /accounts/batch/recover {ids, missing_ok:true}`，走既有 `batchResultToast`
  逐条反馈；不做前端预过滤（全量发送、服务端逐条判定，与启停/删除同一语义）。
- 文案繁体、代码注释简体；`frontend/dist` 必须重建并提交（CI 有一致性门禁）。

## 5. 测试

- `internal/store/recover_test.go`：正例字段转移、Enabled/Archived 正交不变量、
  反向表驱动（active/冷却/凭据失效/耗尽/停用 → 零变化拒绝）、归档拒绝、不存在、
  跨重启持久化、批量混合（ok/not_found/error 计数与明细）、严格模式零副作用、
  重复 ID 去重、**启停契约回归**（未恢复的风控失效号启停往返后仍 invalid）。
- `internal/adminapi/accounts_recover_test.go`：单条 200/404/400（非风控失效、已归档）、
  批量 200 混合明细与严格模式 400、`batch/recover` 命中批量 handler 不被通配吞、
  缺 Admin Key → 401。
- `frontend/test/accounts-recover.test.tsx`：按钮仅风控失效行渲染（凭据异常/冷却行不渲染）、
  单条确认框文案与端点调用、批量勾选后端点与 toast 汇总。

## 6. 已知取舍（记录决议）

1. **归档账号拒绝恢复**：归档=停止调用；先在归档区「恢復到帳號池」再操作，
   保持归档区操作集合固定（归档区 UI 不加恢复入口）。
2. **quota 判据收敛**：`quota.isRiskControlInvalid` 已删除，调用点直用
   `model.IsRiskControlInvalid`——零行为变化，消除双份判据；可选独立回退。
3. **streak 清零语义**：恢复=「人工确认该身份可再试」，连击从零起算；
   不清则恢复几乎无效（下次命中即老计数+1 → 立刻再 invalid）。
4. **速率限制/503 连击不清**：与换凭据路径一致，避免扩大本出口的语义。
5. **批量不做前端预过滤**：保留「服务端唯一真相」，非可恢复项的失败回报是
   给管理员的有用反馈（说明这些账号不在此出口范围内）。

## 7. 验证

- 本地：gofmt 合规（LF 规范化副本）、`git diff` 复核、前端 `npm run build` 重建 dist。
- CI（Ubuntu）：`go test -race` 全量 + vitest + dist 一致性门禁，全绿为准。
- 手工验收：账号页对风控失效行出现恢复按钮 → 确认后状态变「正常」并可被调度；
  对正常/冷却/凭据异常账号调 API → 400。
