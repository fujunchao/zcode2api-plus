# 实施计划：账号池「无额度」派生分组（v2.10.30-go）

> 基线：`origin/go-rewrite` @ `7184e53`（v2.10.29-go）。状态：**已实施，随 v2.10.30-go 发布**。
> 外部评审：GLM-5.3 独立评审（0 严重 / 5 一般 / 6 建议），评审意见已全部落入本计划。

## 1. 需求与口径

用户希望把「体验套餐到期后额度快照为空」的账号单独分组，便于批量处理。确认口径：

- **派生筛选 tab**（与 v2.10.13「异常」组同型），不建手工分组、不新增持久化字段、不改列表 API；
- 命中：非归档、JWT、非初始化阻塞、`status ∈ {active, cooling}`、`quota` 为空；
- 排除：初始化中/待固定代理、用完、停用、失效、本地风控隔离、探测专用、apiKey；
- 不区分新老账号；尚未取额但无初始化任务的直连账号会计入（已知边界）；
- 需要批量删除——复用现有批量删除（确认弹窗 + `missing_ok` + 只提交可见选中项），不新增接口；
- 只改前端 + 测试 + 重建 `frontend/dist`，不发版动作之外的版本升级按发布流程执行。

## 2. 实现落点（frontend/src/pages/accounts.tsx）

- `FilterKey` 增加 `'no_quota'`；
- 组件外纯谓词 `hasEmptyQuota(a)`：enabled、探测专用、mode、本地隔离、初始化阻塞、
  active/cooling、`Object.keys(a.quota ?? {}).length === 0`；
- `counts.no_quota` 在 `last_error_kind` 早退**之前**累加（典型无额度账号没有 last_error_kind）；
- `filtered` 对 `no_quota` 短路判定，不追加第三个 `.filter()`（避免 AND 语义空列表）；
- tab 数组在「用完」后插入「无额度」，加口径 tooltip；
- 空列表文案增加无额度专属分支。

## 3. 测试

新增 `frontend/test/accounts-no-quota-filter.test.tsx`（不整抄异常组夹具，显式构造命中/不命中矩阵）：
计数与命中矩阵、quota null 兜底、零数量入口与空态、错误类型交集、切换清空勾选、
批量删除只提交可见集合且需确认、额度恢复后退出分组。

## 4. 评审记录（GLM-5.3）

0 严重 / 5 一般 / 6 建议，均已落实：

1. 本地隔离 `risk_isolation` 显式排除并加用例；
2. 探测专用 `model_probe_only` 显式排除并加用例；
3. counts 累加位置写在 last_error_kind 早退之前；
4. filtered 分支短路，不追加 filter 阶段；
5. 测试夹具不整抄（模板默认 `quota:{}` 会让多个非目标账号命中），补 quota null 用例；
6. 建议项：enabled 防御、tooltip 口径、待固定代理用例、dist 用 CI 同版本 Node 构建、
   与未来持久化到期事实前向兼容、批量栏按钮描述修正。

## 5. 交付与验证

- 先推失败测试（CI 仅 frontend 失败，6 条新用例按预期找不到「无额度」入口）；
- 再提交实现 + `frontend/dist`，等待当前提交完整 CI 通过；
- 版本号 + 发布说明 + README 后打 tag `v2.10.30-go`，由 release 工作流复用完整 CI 并发布附件与镜像；
- 不发版之外不新增其它改动；无数据迁移，回滚只需撤销前端。
