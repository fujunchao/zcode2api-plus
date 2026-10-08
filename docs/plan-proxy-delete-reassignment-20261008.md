# 删除代理后的账号重分配修复计划

日期：2026-10-08；目标版本：`v2.9.5-go`。

## 问题与复现

两条启用代理各绑定一个账号，手动删除其中一条。预期被影响账号共享剩余代理，实际账号的 `ProxyID` 和 `ProxyURL` 被清空，接口返回 `reassigned=0`、`direct_fallback=1`。

最小回归命令：

```powershell
go test ./internal/adminapi -run '^TestDeleteProxySharesOccupiedProxy$' -count=2 -v -timeout=30s
```

修复前连续两次失败；仅将单条删除的共享参数由 `false` 改为 `true` 后连续两次通过。

## 根因

排查假设依次为：单条删除仍沿用仅空闲策略、候选过滤/绑定数统计错误、持久化丢失绑定。

验证命中第一项：`DeleteProxyProfile` 调用 `removeProxiesLocked` 时传入 `allowShared=false`，因此所有已有绑定的候选都会被排除。批量清理传入 `true`；新增与风控换线也已经使用最少绑定选择器。不是账号统计或数据库写入异常。

`v2.9.0-go` 的方案曾明确保留手动删除的旧行为。本次按新要求统一该入口，删除不再因为“没有空闲代理”而回退直连。

## 行为契约

- 只重分配删除时仍绑定目标代理的账号；不改其他账号、现有主动直连或手工 URL。
- 候选仅为删除后仍存在的启用代理；空闲优先，否则共享绑定账号数最少的代理，并列按列表顺序。
- 按 Store 的账号顺序逐个分配，每次立即增加目标代理的绑定数，避免多个账号集中到同一条线上。
- 绑定数包含停用、冷却、失效及归档账号；手工 URL 不占用命名代理。
- 只有完全没有候选时才回退直连，同时清除旧代理 ID 和 URL。
- 删除线路、更新账号绑定仍在同一把锁和 SQLite 事务内完成；写入失败时全部回滚，不发布部分内存状态，不清除原线路断流计数。
- 批量清理仍先摘除整批代理；领取风控清理仍排除相同 URL；消息风控历史避让不变。
- 不修改数据库结构、API 字段和账号启停/冷却状态；不自动修改历史上已经回退直连的账号。

## 实施计划

- [x] 使用真实管理接口建立可重复失败的最小回归，并用单变量修改确认根因。
- [x] 移除删除路径的旧共享开关，复用统一最少绑定选择器。
- [x] 补齐存储、接口与前端测试，校验持久化、事务回滚及并发删除。
- [x] 更新确认框、结果提示、说明文档及内嵌前端产物。
- [x] 执行针对性重复回归、Go 全量测试/构建/静态检查和前端测试/构建。
- [x] 更新 `v2.9.5-go` 版本号及发版说明，准备按下述门禁流程发布。

## 发布流程

1. 仅提交本次修复、测试、文档和内嵌产物，保留工作区原有未跟踪文件，不操作运行中的服务或真实账号数据库。
2. 推送 `origin/go-rewrite`，等待 CI 的前端、Go 测试与竞态检测、SDK 兼容性、跨平台编译和容器构建全部通过。
3. 推送 `v2.9.5-go` 标签，由既有 release 工作流发布五平台二进制和双架构镜像。
4. 核对 Release 附件及镜像发布状态；实际远端验收以该提交和标签对应的 Actions 结果为准。

## 验收记录

- 删除、并发删除、批量清理、领取风控清理及代理事务失败回归连续运行 20 轮通过：
  `go test ./internal/store ./internal/adminapi -run 'TestDeleteProxy|TestConcurrentDeleteProxies|TestPurgeProxyProfiles|TestPurgeUnprovisioned|TestProxyMutationFailureIsAtomic' -count=20 -timeout=180s`。
- 前端 8 个测试文件、25 项测试通过；本次新增的 4 项删除提示回归已先验证旧文案失败，再验证修复后通过。
- `npm run lint -- src test` 无错误；9 项告警均位于本次未修改的文件。
- `npm run build` 通过，内嵌前端产物已更新；保留既有大于 500 kB 的分包提示。
- 注入与 CI 相同的旧产物扫描探针后重新构建，所有产物的 SHA256 与前一次完全一致。
- `go vet ./...`、`go build ./...`、`git diff --check` 通过。
- 首次全量运行遇到既有 Windows `TempDir RemoveAll cleanup` 偶发失败，涉及 `TestAdminUnauthorized` 和 `TestChatStreamReportsUpstreamFailure`；两者隔离复跑 20 轮全部通过。
- 第二次全量运行中，未改动的 `TestPoolStopWaitsInflight` 出现一次关闭时序失败；隔离复跑 50 轮通过，未修改验证码实现或放宽测试。
- Go 全量测试最终复跑通过：`go test -json -p 1 ./... -count=1 -timeout=180s`，1056 个通过事件（含子测试），4 项按既有条件跳过。
- 本机 `CGO_ENABLED=0` 且无 GCC，竞态检测交由 Linux CI 执行，CI 全部通过后才发布标签。
