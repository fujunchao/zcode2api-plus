# 风控静默期隔离与恢复护栏（历史设计）

> v2.9.19（2026-10-13）起默认重新启用本文设计的静默隔离（24h）：当日观测确认上游标记在 12 小时闲置期后仍未解除；v2.9.18 曾短暂改为默认纯阶梯。现行方案见 [恢复阶梯与诊断](plan-risk-ladder-diagnostics-20261012.md)。

2026-10-12 晚事故（`docs/analysis-riskcontrol-hotaccount-20261012pm.md`，按现行约定未入库）
的三项 P0 修复：**选号硬闸门 / 恢复护栏 / 请求级判定收窄**。本文是行为规格与边界说明。

## 背景（一句话）

上游对触发风控的账号打**服务端标记**，标记期内该账号对任何请求都回 405（实测 ≥14h）；
本地 ladder 冷却只有分钟级——「冷却到期 → 再入池 → 再 405」循环消耗池子，且污染池里
「连抽两个热账号」会被误判成请求级风控、错误地停止换号并封锁同内容（本次 33 分钟
全池风暴的直接机制）。

## 设计

### 1. 新字段：LastRiskControlAt（持久化）

- `Account.LastRiskControlAt *float64`，JSON 键 `last_risk_control_at`（键集只增不减，
  `model/account_test.go` 的 `pythonAccountKeys` 同步）。写入点唯一：
  `gateway.MarkRiskControl`（sync / async 共用），每次风控命中即盖章。
- **刻意持久化**（对比三个 streak 的 `json:"-"`）：它衡量的是上游侧、跨重启存续的
  状态，发版重启不得把静默期内的账号放回轮转。
- **升级迁移**：旧数据无此键时，`normalize()` 用 `last_error_at` 回填（仅当
  `last_error_kind == risk_control`）——覆盖事故当晚已被标记、但字段尚不存在的账号。
- **恢复不清除、换凭据清除**：手动恢复不解除上游标记，证据保留（恢复≠解除）；
  换凭据=换身份（与 streak 同语义），不清的话「换凭据救回」会被闸门拦住 24h。
  `EditAccount(SetSecret)` / `RenewJWT` / OAuth 登录三处同步清除。

### 2. 静默窗口开关

- `config.RiskSilentHours`（env `ZCODE_RISK_SILENT_HOURS`，默认 24）；
  `config.RiskSilentWindow()` 是唯一换算点。
- 取 24h：观测下界 ≥14h 之上取整到整天，并覆盖「命中会续期标记」的最坏假设——
  静默期若短于标记时长，探测命中可能把标记续上，宁可晚恢复也不主动踩雷。
- `0` = 关闭（应急回退；闸门与恢复护栏同源，两处一起失效）。

### 3. 选号硬闸门（`store.Select` 第一层）

- `!a.InRiskControlSilentWindow(now, window)` 加入 base 过滤。**硬过滤而非软回避**：
  池内全在静默期时宁走 `no_available_account` 出口，也不把必然 405 的账号喂给请求
  （软回避会在池子退化时把热账号当兜底，等于没修）。
- 池状态分桶新增 `risk_silent`；`no_available_account` 文案/结构新增
  「N 帳號風控靜默期中（上游標記未解除）」/ `details.risk_silent`。

### 4. 恢复护栏（`store.applyRecovered`，单账号与批量共用）

- 静默期未过 → `Status=cooling`、`CoolingUntil=静默期终点`（与闸门放行时刻重合）、
  `last_error_kind` 保持 `risk_control`、`last_error` 写「已手动恢复；上游风控静默期
  未过，保护冷却中，期满自动重新参与调度」；`RiskControlStreak` 照旧清零。
- 静默期已过 → 旧语义（直接回 `active`）。
- 响应面：单账号 message 带「距上次风控 N 小时（静默期内）…保护冷却至 XX」；
  批量逐条 message 带「保护冷却至 XX」；前端 toast 采用 message，批量摘要附
  「（N 個帳號進入風控保護冷卻）」——16:31 事故里响应只写 ok，管理员无从得知
  账号实际没有回到调度。

### 5. 请求级判定收窄（`gateway.RiskScope.Verdict`）

- 只统计「命中时不在静默期」的账号；热账号的命中只做账号级处置（冷却 + 盖章），
  不计入阈值。阈值仍为 2，`RiskControlRequestLevelThreshold` 语义不变。
- 推论：单干净账号的 405 不构成请求级 → **不登记重放封锁**（内容未被证伪，
  封锁会挡住本可在池子恢复后成功的请求）；池子退化时以 no_available_account 结束。

## 边界与不变式

- 冷却到期 ≠ 入池：`IsSelectable`（状态机）与静默期闸门（上游标记证据）是两个概念。
- 账号级处置完全不改：冷却阶梯、超档置失效、换代理、last_error 证据章保持原样。
- quota 刷新、启停、归档与闸门正交：闸门只影响 Select 与池状态分桶，不写状态机。
- sync 与 async 两条路径共用同一判定函数与同一字段（项目硬不变式）。

## 测试矩阵（CI 验证）

- model：判定边界（窗口内/外、恰好终点、window=0、nil）、`RiskControlSilentUntil`
  换算、normalize 回填三分支、Clone 复制。
- store：Select 跳过热账号 / 全静默返回 nil / 窗口过后恢复轮转 / 开关关闭回退；
  恢复保护冷却与终点换算；批量逐条 message；PoolStats 分桶；换凭据清证据后
  立即可选中。
- gateway：热账号命中不计入请求级 + 混合场景 + 开关回退；MarkRiskControl 盖章；
  事故形状端到端（污染池跳过热账号、干净双命中请求级、单干净不误判不封锁）。
- asyncpool：镜像端到端（热账号跳过、no_account 带静默期文案、不登记重放防护）。
- frontend：恢复对话框文案与 message 透传（保持既有断言通过）。
