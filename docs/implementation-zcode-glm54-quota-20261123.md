# 2.15.0：GLM-5.4 接入与额度模型身份修复

记录日期：2026-11-23，Asia/Shanghai。分支 `go-rewrite`，基线 `a9a195e`（2.14.6-go）。
本记录对应 2.15.0-go。

## 1. 背景与证据

上游在 2026-11-23 把额度余额的模型身份从 `show_name`/`model` 改为
`capabilities: ["model:<id>"]`，并同时下发 GLM-5.4（云端运行时目录 revision 48）。

独立核查（只读）：

- 生产库副本（23:19，含 WAL）：80 个账号，额度键为 `GLM-5.3-Flash` ×80、
  `model` ×80、`GLM-5.4` ×69，**没有 `GLM-5.3`**；金额与后台快照一致。
- 代码链路：`quota.go` 旧解析回落 `"model"` → `QuotaEntriesForModel` 查不到 →
  `ModelAvailability` 判 `absent` → `Select` 只保留 `available`/`unknown` →
  5.3 请求会返回 `503 no_available_account`。
- 上游 5.3 仍可用：`probe --model glm-5.3` 返回 200；5.4 `probe --model glm-5.4`
  22:55 返回 200（23:23 复测为 529/1305 平台过载，非模型名问题）。
- 运行时目录：4.0.0 revision 48（sha256 `60684A80…`）与 3.19.1 revision 47 均含
  `GLM-5.4`；随包 revision 44 不含。官方标准 ID 为大写 `GLM-5.4`。

## 2. 代码改动

- `internal/quota/quota.go`：新增 `balanceModelIDs`，按官方
  `resolveZaiStartPlanBalanceModelIds` 口径从 `capabilities` 的 `model:` 项取模型 ID；
  无合法项时回落 `show_name` / `model`；共享额度桶按模型各记一列。
- `internal/quota/initialization.go`：初始化观察使用同一解析，避免新号余额被判 unknown。
- `internal/model/account.go`：旧快照的未知 `model` 桶在 `ModelAvailability` 中返回
  `unknown`（不再 `absent`），作为额度刷新前的过渡兼容。
- `internal/gateway/classify.go`：`AvailableModels` 增加 `GLM-5.4`；
  `ModelNameMap` 增加 `glm-5.4 → GLM-5.4`。
- `internal/gateway/outbound_diagnostics.go`：诊断标签识别 `glm-5.4`。
- `internal/openai/reasoning.go`：GLM-5.4 沿用 `low/high/max` 三档，不能关闭思考。
- `cmd/zcode2api/probe.go`：新增 `--effort`，用于单账号验证三个思考档位。

## 3. 回归

- `internal/quota/capability_parse_test.go`：capabilities 解析、非 model capability
  回落、无字段回落、共享桶、旧快照 unknown、调度可用性。
- `internal/model/account_test.go`：未知 `model` 桶不判 absent。
- `internal/gateway/*_test.go`：白名单含 5.4、大小写映射、`/v1/models` 三条。
- `internal/openai/*_test.go`：GLM-5.4 的 low/high/max 与禁止关闭思考；Chat/Responses
  的思考档位循环加入 5.4。

## 4. 部署后验证（待填）

1. 容器 `2.15.0-go` healthy、`/meta` 版本正确。
2. 后台额度刷新后，5.3 汇总从 0 恢复；额度键出现 `GLM-5.3`。
3. `probe --model GLM-5.4 --effort low|high|max` 三条均 200。
4. 四入口（Messages / Chat / Responses / Async）对 GLM-5.4 放行。

## 5. 回退

- 镜像回退：`2.14.6-go`。
- 仅回退客户端身份：`ZCODE_CLIENT_VERSION=3.19.1`。
- 额度快照会在部署后后台刷新周期内自动重建，无需数据库迁移。
