# 计划：账号池批量管理（批量新增 / 批量删除 / 批量启用禁用 / 条件查询）

> 状态：**已实施**（2026-09-24）。实施落点：
> `internal/store/batch.go`、`internal/adminapi/accounts_batch.go`、
> `internal/store/batch_test.go`、`internal/adminapi/accounts_batch_test.go`，
> 路由注册与 `accountSnapshot` 拆分见 `adminapi.go`，列表条件化见 `accounts.go`。
> 实施时的一处修正：`limit` **不传 = 不分页**（保持旧版全量行为），传则 1–500——
> 原稿「默认 100」会改变无参数调用方看到的结果集，与向后兼容矛盾。

## 0. 现状与差距

| 能力 | 现状（internal/adminapi/accounts.go） | 差距 |
|---|---|---|
| 批量新增 | `POST /admin/api/accounts` 接受多 token（字符串按行 / 数组） | 中途失败直接 500 返回：**已成功的项不回滚**、无逐条明细；无数量上限 |
| 批量删除 | `DELETE /admin/api/accounts` 接受 ID 数组 | 逐条删、not-found 静默跳过、**非原子**；仅落库失败回 `failed` 数组 |
| 批量启用/禁用 | 仅单个 `POST /admin/api/accounts/{id}/enabled` | 缺批量端点 |
| 条件查询 | `GET /admin/api/accounts` 全量返回 | 无过滤、无分页、无排序 |

说明：`m11_batch_test.go` 是上游 M11 段的回归守卫（刷新/领取/编辑），与本功能无关；新测试文件命名避开它（用 `batch_test.go` / `accounts_batch_test.go`）。

## 1. 接口设计（全部为新端点，旧端点零改动）

统一响应明细结构（每条操作的成败逐项回报）：

```json
{
  "total": 3, "succeeded": 1, "duplicated": 1, "failed": 0, "not_found": 1,
  "ids": ["新建/命中的账号 ID 列表"],
  "items": [
    {"index": 0, "id": "…", "name": "…", "status": "ok", "message": ""},
    {"index": 1, "status": "duplicate", "message": "同 user_id 已存在"},
    {"index": 2, "status": "not_found", "message": "账号不存在"}
  ]
}
```

`status` 取值：`ok` / `duplicate`（仅新增）/ `not_found`（删除、启停）/ `error`。

### 1.1 批量新增 `POST /admin/api/accounts/batch/add`

```json
{
  "provider": "zai",
  "items": [{"name": "可选", "secret": "必填", "email": "可选"}],   // ≤500 项，去重保序
  "proxy_id": "__auto__"          // 三态语义与现有端点一致：__auto__/null=自动、__direct__/" "=直连、其余=profile
}
```

- **预校验先行**：任一项 secret 为空 / 超长 / provider 非法 → 400 + 全部坏项明细，**零副作用**。
- 判重沿用 `duplicateLocked` 三轮语义（user_id → email → 凭据）；命中既有记录记 `duplicate`，**不算失败**（与现有 `AddAccountWithIdentity` 幂等行为一致）。
- 持久化阶段整体走单事务：任一条落库失败 → **整批回滚**，500 + 明细。
- 成功后沿用现有链路：`AutoAssignProxies(新 ID)` → jwt 账号额度刷新 → `scheduleAutoClaim`（与 `handleAddAccounts` 尾段同构，提取共享 helper）。

### 1.2 批量删除 `POST /admin/api/accounts/batch/delete`

```json
{ "ids": ["…"], "missing_ok": false }
```

- `missing_ok=false`（默认，严格）：任一 ID 不存在 → 400 + `not_found` 明细，**一个都不删**。
- `missing_ok=true`：跳过不存在项并逐条标注（兼容现有 `DELETE` 的宽松语义）。
- 旧 `DELETE /admin/api/accounts`（数组体）保持原行为不变，新端点是推荐路径。

### 1.3 批量启用/禁用 `POST /admin/api/accounts/batch/enable`、`POST /admin/api/accounts/batch/disab`

```json
{ "ids": ["…"] }
```

- 字段转移逻辑与 `store.SetEnabled` 逐字一致：禁用 → `Enabled=false` + `Status=disabled`；启用 → `Enabled=true`，且仅当原状态为 `disabled` 时回 `active`（**不动 invalid/cooling/exhausted 的状态机归属**，红线 3：归档 ≠ 停用 ≠ invalid）。
- 未知 ID 默认按 `not_found` 明细返回并整体拒绝（与 1.2 严格语义对齐）；后续如需宽松可加 `missing_ok`。

### 1.4 条件查询 `GET /admin/api/accounts`（向后兼容扩展）

| 参数 | 取值 | 说明 |
|---|---|---|
| `keyword` | 子串 | 匹配 name / email / id / user_id |
| `status` | 逗号分隔 | 白名单：active,exhausted,cooling,invalid,disabled |
| `enabled` | true/false | — |
| `mode` | jwt/apiKey | — |
| `model` | 模型名 | 经 `NormalizeModelName` 归一后比对 `ModelAvailability` |
| `proxy_id` | profile ID / `__direct__` / `__none__` | 按线路筛选 / 仅直连 / 仅手工 URL |
| `archived` | all/false/true | 默认 **all**（保持现状可见性），可筛仅归档/排除归档 |
| `last_error_kind` | 错误类别 | 与前端 `ERROR_KIND_ALL` 同源 |
| `created_after` / `created_before` | unix 秒 | — |
| `sort` | created_at_asc（默认）/ created_at_desc / name / use_count_desc | — |
| `limit` / `offset` | 1–500 / ≥0 | **不传 limit = 不分页**（旧版全量行为）；传则按页截取，`total` 始终为过滤后总数 |

响应在现有键之外**追加** `total` / `offset` / `limit`（纯 additive）；无任何过滤参数时返回内容与现状一致。

## 2. Store 层设计（新文件 `internal/store/batch.go`）

```go
const MaxBatchSize = 500

type BatchAddItem struct{ Name, Secret, Email string }
type BatchItemStatus string // "ok" | "duplicate" | "not_found" | "error"
type BatchItemResult struct{ Index int; ID, Name string; Status BatchItemStatus; Message string }
type BatchResult struct{ Total, Succeeded, Duplicated, Failed, NotFound int; IDs []string; Items []BatchItemResult }
type AccountQuery struct{ Keyword string; Statuses []string; Enabled *bool; Mode, Model, ProxyID, LastErrorKind string; Archived string /*all|false|true*/; CreatedAfter, CreatedBefore *float64; Sort string; Limit, Offset int }
```

方法（跨 provider 用 `findAnyLocked` 解析，与现有删除端点一致）：

- `BatchAddAccounts(provider string, items []BatchAddItem) (BatchResult, error)`
- `BatchRemoveAccounts(ids []string, missingOK bool) (BatchResult, error)`
- `BatchSetEnabled(ids []string, enabled bool) (BatchResult, error)`
- `QueryAccounts(q AccountQuery) ([]*model.Account, int)` — 返回 Clone 副本 + 过滤后总数（分页用）

### 事务一致性实现（核心）

1. 取 `s.mu`（单锁 ⇒ 相对其它 store 操作原子；批量上限 500 ⇒ 持锁时间有界）。
2. **预校验 + 生成 plan**：新增对象 / 待删 ID / 待改对象；判重复用 `duplicateLocked`。
3. `s.db.BeginTx` → 逐条 `tx.Exec`（新增 `INSERT OR REPLACE`、删除 `DELETE`、启停 `UPDATE`）。
4. `tx.Commit()` 成功后**才**应用内存变更（append / 移除 / 字段转移）。
5. 任一步失败 → `tx.Rollback`、内存零改动、返回 error + 已知逐条明细。

- **崩溃一致性**：commit 与内存应用之间进程崩溃 → 重启从 DB 载入快照，自然一致（store 本就以 DB 为真相源）。
- **约束**：事务期间只经 `tx.Exec` 写库，不得再走 `s.db`（`SetMaxOpenConns(1)`，混用会自占连接）；不复用 `persistAccountLocked`（它绑 `s.db`），新增 `persistAccountOnTx(tx, acc)` 镜像同一 SQL。
- **兼容性**：不新增 Account 字段 ⇒ 不触发「五处同步」契约；`AddAccount*` / `Update` / `SetEnabled` 等既有方法签名与行为零改动；JSON 序列化沿用 `marshalJSON`（`SetEscapeHTML(false)` + 去尾换行）。

### 日志

- store 层持久化失败：复用 `logPersistFailure`（已节流，`go web.Warn` 锁外输出）。
- adminapi 层：每次批量操作记一条汇总（`web.Ok` / `web.Err`，module=`batch`）：操作类型、总数、成功/重复/失败数、耗时。逐条明细只进响应体，不逐条打日志（防刷屏）。

## 3. Admin API 层（新文件 `internal/adminapi/accounts_batch.go` + `adminapi.go` 追加 4 条路由）

```
POST /admin/api/accounts/batch/add
POST /admin/api/accounts/batch/delete
POST /admin/api/accounts/batch/enable
POST /admin/api/accounts/batch/disable
```

- 与现有路由无冲突：Go 1.22 mux 字面量段优先，且现有通配段 `{account_id}` 后必须紧跟 `enabled/archived/refresh/reset-stats` 字面量，`/batch/add` 等不会被吞。
- 校验：items ≤500、secret 非空 ≤8192、ids 去重非空、enabled 类参数必须布尔、query 参数白名单校验（非法一律 400，不做宽容解析——与风控阶梯同 philosophy）。
- 错误响应统一走现有 `writeAPIError` / `writeError500` / `errBadRequest` 体系。
- proxy 三态解析、`AutoAssignProxies`、jwt 刷新、`scheduleAutoClaim` 从 `handleAddAccounts` 提取共享 helper 复用（不改其行为）。

## 4. 测试计划（`internal/store/batch_test.go` + `internal/adminapi/accounts_batch_test.go`）

1. **批量新增**：混合合法/重复/非法项 → 预校验失败零副作用（内存 + 重开 DB 双查）；重复项报 `duplicate` 且不新建；成功项 ID 回传；重启（重开 Store）后数量一致。
2. **事务回滚**：注入持久化失败（关闭底层 db 后调用）→ 返回错误且内存不变。
3. **批量删除**：严格模式缺 ID 一个不删 + 明细；`missing_ok` 跳过并标注；旧 `DELETE` 数组体回归不变。
4. **批量启停**：`disabled → enable → active` 转移正确；`invalid/cooling/exhausted` 状态不被启用操作改写；archived 账号 `Enabled` 置位但 `IsSelectable` 仍 false（归档优先，语义与单账号端点一致）。
5. **条件查询**：各过滤器组合、`keyword` 四字段命中、分页边界（offset 超界返空 + total 正确）、无参数 = 现状全量、非法参数 400。
6. **兼容回归**：现有 adminapi 全量测试不红（尤其 `m11_batch_test.go` 与 `TestJSONContractWithPython`）。
7. **全量验证**：本机 `go test ./... -count=1` → `node .workbuddy/gofmt-check.js` → `redact-scan-all.js` → 推分支等 CI `-race` 绿。反向验证：先写测试（红）→ 实现（绿），关键分支（回滚、严格缺失）各做一次回退验证。

## 5. 实施顺序

1. `internal/store/batch.go` + `batch_test.go`（含反向验证）
2. `internal/adminapi/accounts_batch.go` + `adminapi.go` 路由 + handler 测试
3. `GET /admin/api/accounts` 条件查询扩展
4. 文档收尾（PLAN.md 勾选项 / 本文档状态更新）
5. 提交：按「后端 / docs」分笔（无前端改动不拆 dist）；如需发版走既有 v2.7.1-go 流程（可选，另确认）

前端批量 UI（表格多选 + 批量按钮）列为可选后续阶段，涉及 dist 重建，默认不在本次范围。

## 6. 风险与边界

- **持锁时长**：500 条上限内单事务耗时为毫秒级；期间所有账号操作排队，与现有 `PurgeProxyProfiles` 批量改派同量级，可接受。
- **幂等性**：新增的 `duplicate` 语义、启停的重复执行均为幂等；删除的严格模式刻意非幂等（防误删），`missing_ok` 提供宽松出口。
- **不修旧账**：现有 `handleAddAccounts` 半途失败不回滚的行为保持原样（兼容），文档标注新端点为推荐路径。
