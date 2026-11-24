# GLM-5.3 从 Start Plan 下架后的网关处理（2.15.1-go）

日期：2026-11-24（Asia/Shanghai）；分支 `go-rewrite`，基线提交 `e4130de`。
关联分析：[GLM-5.3 在 ZCode Start Plan 的可用性核查](analysis-zcode-glm53-availability-20261124.md)。

## 1. 事实

| 来源 | 观察 |
|---|---|
| 生产账号库（只读快照） | 80 个账号全部 `provider=zai` / `mode=jwt`（Start Plan）；额度快照只剩 `glm-5.3-flash`、`glm-5.4` 两项，**GLM-5.3 为 0 条**；无 exhausted / disabled 标记 |
| 官方生产目录（CDN revision 48） | Start Plan 预置 `GLM-5.4`、`GLM-5.3-Flash`、`GLM-5.2`、`GLM-5-Turbo`；Coding Plan / 错峰 / 高速卡目录仍列 `GLM-5.3` |
| 新-api 日志（24h） | 11-24 当天 0 次 `glm-5.3` 请求（11-23 权益到期前仍有成功记录） |

结论：这批 Start Plan 账号「GLM-5.3 无权益」是确定性事实，不是额度暂时不足；但仓库面向的是
通用部署，Coding Plan / API Key 账号的目录里 5.3 仍在，因此**不能全局删除**该模型。

## 2. 改动前的问题

1. `GET /v1/models` 用静态清单元数据列出 `GLM-5.3`，客户端会把它当可用模型。
2. 请求 `GLM-5.3` 落到 `503 no_available_account`：客户端与中转层按「临时没号」反复重试，
   new-api 还可能据此把渠道判成不健康——而实际是永久性下线。
3. 管理端「模型额度」对 GLM-5.3 画一张 0 账号 0 项目的空卡，并提示「请在账号池刷新额度」，
   刷多少次都不会回来。
4. CLI 单次探针默认模型、Pi 配置示例首项都指向 `glm-5.3`。

## 3. 改动

### 3.1 「支持清单」与「当前权益」分离

`AvailableModels` 仍是**支持清单与请求白名单**（`glm-5.3-flash`、`GLM-5.3`、`GLM-5.4`）：
解析、名称映射、effort 归一化的兼容能力不变，因此仍有 5.3 权益的部署照常可用。

新增 `internal/gateway/models.go`：

| 标识符 | 语义 |
|---|---|
| `ModelEntitlement{Model,Accounts,Available}` | 支持清单里每个模型在当前账号池的权益快照：持有该模型额度条目的非归档账号数、其中额度仍可用的账号数 |
| `ModelEntitlements(accounts)` | 按 `AvailableModels` 顺序产出上面的快照 |
| `EntitledModels(accounts)` | 只保留至少一个账号持有权益的模型；**一个都没有时退化为完整支持清单**，避免新装部署的 `/v1/models` 为空、客户端连一个模型都配不出来 |
| `DeterministicModelUnavailable(accounts, model)` | 确定性判定：池内**没有任何非归档账号持有该模型的额度条目**，且**至少有一个账号刷新过额度快照**（否则无从判断）。冷却/耗尽/停用的账号只要持有条目就一律不算确定性不可用——那是「此刻没号」，必须留 503 |
| `SuggestedModels(accounts, exclude)` | 当前池内有权益的替代模型，用于错误文案 |

### 3.2 `GET /v1/models` 按当前权益公布

`handleModels` 改为 `EntitledModels(h.Engine.Store.ListAccounts(""))`。响应形态不变（Anthropic + OpenAI
字段并存的超集），只是条目集合跟随账号池权益变化——官方恢复权益后无需发版即可自动重新出现。

### 3.3 确定性不可用 → 400 `model_not_available`

`RunMessages` 的 `no_available_account` 出口先判断 `DeterministicModelUnavailable`：成立时返回
**400**（而不是 503），错误体 `type=model_not_available`，message 点明
「目前 N 個帳號的套餐均不含此模型（該模型可能已下架或未開通），請改用 …」（N = 已刷新额度的非归档账号数），
`error.details` 带 `model`、`total_accounts`、`active_accounts`、`snapshot_accounts`、
`model_unavailable{absent:N}`、`suggested_models`。其余「暂时没号」（冷却、风控静默、未知快照、
单模型耗尽、以及任何账号仍持有该模型条目但暂时不可调度）仍返回原 503，401/429/1005 等既有分类不变。

判定刻意做到「宁可 503，不可错报 400」：`PoolStat.ModelStat` 只统计通过健康门槛的账号，若拿它做判据，
一个正在冷却、但确实持有该模型的账号会被算成「不含该模型」，于是客户端收到 400 不再重试——比 503 更糟。
因此判据回到账号数据本身：只要还有非归档账号持有该模型条目，就不是永久缺失。

选择 400 而不是 503 的理由：4xx 表达「重试不会变好」，客户端与中转层不会把永久下线当成瞬时故障，
也不会让 new-api 依据 5xx 给渠道降权。

异步入口 `/async/v1/messages` 已先返回 200 + SSE，无法改 HTTP 状态；同一判定下只把
`no_account` 事件的 message 与 details 对齐（含 `suggested_models`），事件类型保持不变。

### 3.4 管理端「模型额度」区分「已下架」与「未刷新」

`ModelQuotaSummary` 新增 `snapshot_accounts`（池内已有额度快照的非归档账号数，池级数值）。
前端据此把 `accounts === 0` 的模型卡从「三列 -- + 空进度条 + 请刷新额度」改为一行说明：

- `snapshot_accounts > 0` →「当前账号池的 N 个账号套餐均不含该模型（可能已下架），请求会直接失败」；
- `snapshot_accounts === 0` →「尚未获取该模型额度，请在账号池刷新额度。」（保留原文案）。

有权益的卡片（数字、进度条、部分缺失提示）行为完全不变。

### 3.5 默认值

- `cmd/zcode2api/probe.go`：单次探针默认 `--model` 由 `glm-5.3` 改为 `glm-5.3-flash`。
- `examples/pi-models.json`：`GLM-5.4` 置首（含视觉输入），`GLM-5.3` 保留在末位并注明
  「僅在賬號仍有該模型權益時可用」。
- README 与 `docs/client-compatibility.md` 同步说明「模型能力 ≠ 当前权益」。

## 4. 契约（对外）

| 位置 | 变化 |
|---|---|
| `GET /v1/models` | 条目集合改为当前有权益的模型；字段结构不变 |
| `POST /v1/messages`（及 chat/responses 转换层） | 确定性不可用时 `400` + `error.type=model_not_available` + `error.details` |
| `POST /async/v1/messages` | 事件 `type=no_account` 不变；message/details 增加下架口径与替代模型 |
| `GET /admin/api/accounts`、`GET /admin/api/status` | `model_quotas[]` 每项新增 `snapshot_accounts` |

## 5. 验证

1. 远端 CI（`ci.yml`）：Go 静态检查与测试、竞态、前端 tsc/oxlint/vitest、交叉编译、镜像构建。
2. 部署后真机复核（Start Plan 池）：
   - `GET /v1/models` 只应返回 `glm-5.4` 与 `glm-5.3-flash`；
   - `POST /v1/messages` 指定 `GLM-5.3` → 400 `model_not_available`，message 给出替代模型；
   - 管理端「模型额度」不再出现空卡误导文案；
   - `GLM-5.4` / `glm-5.3-flash` 正常 200，思考档位与既有行为一致。

## 6. 回滚

- 若官方重新给 Start Plan 开放 5.3 权益：额度刷新后账号重新持有该模型条目，
  `/v1/models` 自动恢复列出、请求自动恢复 200，**不需要改代码或发版**。
- 若需整体回退：重部 2.15.0-go 镜像即可；本轮无数据库结构变更（`snapshot_accounts` 是运行时汇总字段）。
