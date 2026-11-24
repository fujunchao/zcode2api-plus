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
| `ModelEntitlement{Model,Accounts,Available,Unknown,Expired}` | 每个模型在池里的权益分布（一个非归档账号只落一个桶）：持有该模型额度条目的账号数、其中额度仍可用的账号数、**权益未知**（没有快照 / 旧快照模型身份不明）、**已到期**（额度列全是「不会自动刷新且已过期」的作废额度） |
| `ModelEntitlements(accounts, now)` | 按 `AvailableModels` 顺序产出上面的分布 |
| `EntitledModels(accounts, now)` | 收录 `Accounts>0` **或** `Unknown>0` 的模型（混合池里从不刷额度的 API Key 账号会为所有模型保留公布资格）；已到期的不收录；**一个都没有时退化为完整支持清单**，避免新装部署的 `/v1/models` 为空、客户端连一个模型都配不出来 |
| `DeterministicModelUnavailable(accounts, model, now)` | 确定性判定：**每个非归档账号**对该模型的权益都已确定、且都是「不含」（absent，或额度列已到期）。任何一个账号是 available / exhausted / disabled / unknown 都不判——那是「此刻没号」或「还说不准」，必须留 503 |
| `UnavailableAccountCount(accounts, model, now)` | 与上面同口径的账号数（absent + 已到期），供错误文案与 details 的 N |
| `SuggestedModels(accounts, exclude, now)` | 当前池内确有权益的替代模型，用于错误文案 |

### 3.2 `GET /v1/models` 按当前权益公布

`handleModels` 改为 `EntitledModels(h.Engine.Store.ListAccounts(""), time.Now())`。响应形态不变
（Anthropic + OpenAI 字段并存的超集），只是条目集合跟随账号池权益变化——官方恢复权益后无需发版即可
自动重新出现。

### 3.3 确定性不可用 → 400 `model_not_available`

`RunMessages` 的 `no_available_account` 出口先判断 `DeterministicModelUnavailable`：成立时返回
**400**（而不是 503），错误体 `type=model_not_available`，message 点明
「目前 N 個帳號的套餐均不含此模型（該模型可能已下架、未開通或已到期），請改用 …」
（N = 确定不可用的非归档账号数：快照不含 + 额度列已到期），
`error.details` 带 `model`、`total_accounts`、`active_accounts`、`unavailable_accounts`、
`snapshot_accounts`、`model_unavailable{absent:N}`、`suggested_models`。其余「暂时没号」（冷却、风控静默、
权益未知、单模型耗尽、以及任何账号仍持有该模型条目但暂时不可调度）仍返回原 503，
401/429/1005 等既有分类不变。

判定刻意做到「宁可 503，不可错报 400」，两道防线：
- **不看 `PoolStat.ModelStat`**：它只统计通过健康门槛的账号，一个正在冷却、但确实持有该模型的账号会被
  算漏，于是「其余健康账号都不含」被误判成永久不可用，客户端收到 400 从此不再重试——比 503 更糟；
- **权益未知不算「不含」**：没有额度快照、或旧快照模型身份不明的账号（例如从不刷额度的 API Key 账号、
  刚入池还没刷新过的新账号）都可能持有该模型，只要存在这样的账号就不判 400。

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

### 3.6 评审修复（A–D，同日）

初版提交 `e2e329a` 经独立评审提出四条，逐条定性后全部修复：

| 编号 | 定性 | 问题 | 修复 |
|---|---|---|---|
| A | 真 bug | 「未知权益」被当成「确定缺失」：79 个已刷新账号不含 + 1 个从未刷新/身份未知的账号也会判 400，等于替客户端放弃一个可能可用的模型 | 判定改为「每个非归档账号的权益都已确定且都是 absent」，任一账号 available/exhausted/disabled/unknown 即不判 |
| B | 真 bug | `/v1/models` 只数「有额度条目」的账号，混合池里从不刷额度的 API Key 账号能用的模型会被藏掉 | `ModelEntitlement` 增 `Unknown`，收录条件改为 `Accounts>0 \|\| Unknown>0` |
| C | 既有缺口被放大 | `ModelAvailability` 只看剩余额度、不看到期时间：2026-11-23 的 GLM-5.3 正是「remaining>0 + expires_at 已过」，权益层仍把它当在售 | 新增 `(*Account).ModelEntitlementExpired`（只认不会自动刷新、且已过期的额度列），权益层把这类账号记为 `Expired`：不公布、按「确定不可用」参与判定；**调度语义不改** |
| D | 真 bug（测试卫生） | 网关测试夹具只隔离了 `config.DBPath`，引擎出站调用 `config.DeviceMid()` 会把 `device_mid.txt` 写进仓库 `data/` | `openStore` 一并隔离 `config.DataDir`（对齐 `internal/adminapi` 既有做法），并补隔离断言用例 |

C 的边界刻意选保守：日额度（周期跨度 24h）即使快照过期也不算作废——次日就刷新，把它判成过期会让
`/v1/models` 每天抖动一次；只有 one_time 活动额度或跨度 > 36h 的套餐周期才走到期判定。

## 4. 契约（对外）

| 位置 | 变化 |
|---|---|
| `GET /v1/models` | 条目集合改为当前有权益的模型（持有条目或权益未知；已到期不公布）；字段结构不变 |
| `POST /v1/messages`（及 chat/responses 转换层） | 确定性不可用时 `400` + `error.type=model_not_available` + `error.details`（含 `unavailable_accounts`、`snapshot_accounts`、`suggested_models`） |
| `POST /async/v1/messages` | 事件 `type=no_account` 不变；message/details 增加下架口径与替代模型 |
| `GET /admin/api/accounts`、`GET /admin/api/status` | `model_quotas[]` 每项新增 `snapshot_accounts` |

## 5. 验证

1. 远端 CI（`ci.yml`）：Go 静态检查与测试、竞态、前端 tsc/oxlint/vitest、交叉编译、镜像构建。
2. 部署后真机复核（Start Plan 池）：
   - `GET /v1/models` 只应返回 `glm-5.4` 与 `glm-5.3-flash`；
   - `POST /v1/messages` 指定 `GLM-5.3` → 400 `model_not_available`，message 给出替代模型；
   - 管理端「模型额度」不再出现空卡误导文案；
   - `GLM-5.4` / `glm-5.3-flash` 正常 200，思考档位与既有行为一致。

## 5.1 发布与部署结果（2026-11-24）

| 项 | 结果 |
|---|---|
| 分支 CI | run `45326377243`（`codex/zcode-2.15.1-glm53-retire`，提交 `7329962`）全 job 通过 |
| 发布 | tag `v2.15.1-go` → release run `45326929686` 全绿；附件 5 个（linux/darwin × amd64/arm64、windows amd64）；镜像 `ghcr.io/fujunchao/zcode2api-plus:2.15.1-go` 与 `latest` |
| 部署 | 服务器 `154.12.85.101` 的 compose 镜像标签 2.15.0-go → 2.15.1-go，备份 `docker-compose.yml.bak_pre2151_*`；容器 `zcode2api` healthy、restarts=0、`/meta` 返回 `2.15.1-go` |
| `GET /v1/models` | 仅 `glm-5.3-flash`、`GLM-5.4` |
| `POST /v1/messages model=GLM-5.3` | **400** `model_not_available`，35 ms，`upstream_attempts=0`；message 含「可能已下架」与替代模型，details `unavailable_accounts=80`、`snapshot_accounts=80`、`suggested_models=[glm-5.3-flash,GLM-5.4]` |
| `POST /v1/messages model=GLM-5.4` | **200**，3.9 s，正常返回 |
| 管理端 `/admin/api/status` | `model_quotas` 三项均带 `snapshot_accounts:80`；GLM-5.3 `accounts=0`（前端据此显示「已下架」而非「请刷新」） |
| 内嵌前端 | `/assets/index-BW77Je-S.js` 含新文案；`new-api` 容器内访问 `http://zcode2api:3000/meta` 正常 |

部署期间一次 `docker compose up -d` 因命令超时被打断（旧容器已移除、新容器未创建，约 1 分钟不可用），
随即用后台任务重跑 `docker compose up -d` 并 `--force-recreate` 恢复，容器名与网络别名均回到 `zcode2api`，
`new-api` 到网关的链路不受影响。

## 6. 回滚

- 若官方重新给 Start Plan 开放 5.3 权益：额度刷新后账号重新持有该模型条目，
  `/v1/models` 自动恢复列出、请求自动恢复 200，**不需要改代码或发版**。
- 若需整体回退：重部 2.15.0-go 镜像即可；本轮无数据库结构变更（`snapshot_accounts` 是运行时汇总字段）。
