# 项目代码审查报告（2026-09-30）

> 后续进度：第三部分 11 项已完成代码修复，方案、正式回归与验证限制见
> [修复方案及验收记录](plan-audit-fixes-20260930.md)。
> 本文保留原提交的审查证据，代码行号对应修复前基线。

## 结论

项目存在明确的正确性、状态一致性和运维问题，但不是“无法编译或主流程整体不可用”。

本次以主仓库提交 `a922ee5`（应用版本 `2.8.5-go`）为基线，完成模块与调用链梳理、源码审查、全量现有 Go 测试、前端构建，以及隔离故障复现。**11 项问题通过新增诊断测试复现**；另外列出静态确认的问题、既有部署风险和设计限制，不把它们混成同一种证据。

没有修改业务源码、没有使用真实账号调用上游、没有读写真实账号数据库，也没有改动原有未提交文件。

## 一、审查范围与项目结构

主仓库包含 57 个生产 Go 文件、74 个 Go 测试文件（496 个顶层 Test 函数）、44 个前端源文件。阅读与验证以自主源码为边界：

- 入口和交付：CLI、HTTP 生命周期、环境配置、Docker、Compose、CI、内嵌前端。
- 数据和调度：账号状态机、SQLite、设置、账号/代理 CRUD、批量事务、额度优先选号。
- 请求链路：Anthropic 同步透传、异步票务、Chat/Responses 协议转换、上游头和 system/metadata 注入。
- 后台任务：额度刷新、套餐领取、验证码管理与浏览器池、代理健康巡检。
- 管理后台：登录、账号、代理、设置、验证码、仪表板、共用组件与 API 封装。
- 测试：全量执行，并重点核对本次问题涉及的状态、存储、协议和故障注入测试；这不等同于对每条测试断言逐行穷尽证明。

边界说明：

- `zcode-switch/` 是被主仓库忽略的独立第三方参考仓库，不是本服务构建输入，本次不对该独立项目作完整审计。
- `node_modules`、内嵌第三方阿里云 SDK 不作为自主源码逐行审计；前端依赖另做 npm audit。
- 已验证内嵌前端的 HTML、JS、CSS 与本次重建结果 SHA-256 完全一致。
- 真实上游、真实 OAuth、真实验证码、实际容器停机、多平台运行未做在线验收。

### 主要调用关系

1. `serve` 创建 Store、鉴权服务、Captcha Manager、Quota Service。
2. 原生 Messages 和两个 OpenAI 兼容入口共用 Gateway Engine；OpenAI 入口先转请求，交付时再转响应。
3. Async Pool 自己执行票务与上游循环，但复用 Store 选号、部分错误分类和状态操作。
4. Store 以 SQLite 持久化，同时在进程内保存账号与设置快照；这正是跨进程一致性问题的关键。
5. 三种后台调度器分别负责额度、领取和线路巡检；部分操作尚未纳入统一取消和生命周期。
6. React 后台通过 Bearer 管理密钥访问 Admin API，Go 二进制直接嵌入已提交的 dist。

## 二、验证结果

| 检查 | 结果 |
|---|---|
| `go build ./...` | 通过 |
| `go vet ./...` | 通过 |
| `go test ./... -count=1 -timeout=180s` | 通过 |
| 全量测试附带 `-cover` | 通过 |
| 前端 `tsc -b` | 通过 |
| Vite 生产构建（输出到临时目录） | 通过，未覆盖仓库 dist |
| `npm run lint` | 返回 0，但有 1,405 条警告，其中 10 条来自 src，其余主要来自误扫 dist |
| `npm audit --json` | 当次报告 0 个已知漏洞 |
| `go test -race ./...` | 未执行成功：环境 `CGO_ENABLED=0`，工具提示需要启用 CGO |
| 本次新增诊断测试 | 12 个顶层用例：11 个暴露问题，1 个验证“忙时跳过”符合现行契约 |

部分覆盖率：入口/CLI 5.9%、OAuth 38.1%、Captcha 59.7%、Admin API 69.9%、Store 86.7%、Gateway 87.5%、OpenAI 83.3%。覆盖率高不代表状态组合已经覆盖。

本地跳过的用例包括官方 Python/Pi SDK 兼容测试、真实验证码 SDK 页面测试和 Windows 下的符号链接测试。本次没有运行 govulncheck，不能把 npm audit 的结果外推为全部依赖安全。

## 三、已复现的问题

优先级定义：P1 应优先修复，可能改变账号调度、出口或持久化结果；P2 是明确的功能/运维问题，应进入近期修复计划。

### 1. [P1] CLI 与运行中服务同时操作同库，删除账号可能被“复活”

**位置：** [store.go:202](../internal/store/store.go#L202)、[store.go:298](../internal/store/store.go#L298)、[store.go:1416](../internal/store/store.go#L1416)。

Store 只在初始化时读取数据库，随后依靠本进程的账号/设置快照。CLI 会另外打开一个 Store，而服务不会接收 CLI 的修改。账号更新又采用 `INSERT OR REPLACE`。

**复现：** 服务 Store 加入账号 → 第二个 Store 模拟 CLI 删除账号 → 服务对原有账号累计一次用量 → 重新打开数据库，账号重新出现。

同一机制也意味着 CLI 改密码后，运行中的服务仍可能验证旧内存密钥；CLI 新增账号也不会自动进入服务内存池。该密码现象为同一机制的静态推论，独立复现测试验证的是账号删除被回写。

**建议：** 优先建立单写进程边界：服务运行时 CLI 经管理 API 操作，或明确阻止并发离线写库。若要支持多进程，应增加数据库版本/失效同步机制，并避免用 upsert 更新已被删除的记录。

**证据：** `TestAuditConcurrentStoreMustNotResurrectDeletedAccount`。

### 2. [P1] SOCKS4 在真实请求中受支持，却会被巡检判为无效并可能删除

**位置：** [proxies.go:391](../internal/adminapi/proxies.go#L391)、[proxyhealth.go:201](../internal/adminapi/proxyhealth.go#L201)。

网关代理实现支持 `socks4://`，但巡检的 `newProbeClient` 只接受 HTTP、HTTPS、SOCKS5、SOCKS5H。任何 SOCKS4 线路都会在连接前收到“不支持的代理协议”。

启用默认自动巡检后，只要存在其他可用线路，或全失败时直连检查可达，该线路就会进入删除/账号改派路径。也就是说，即使线路本身健康，也无法通过这个探测。

**建议：** 巡检与实际业务共用代理 Transport 工厂；区分“探测能力不支持”和“线路故障”，前者不能触发删除。

**证据：** `TestAuditProbeMustSupportGatewaySOCKS4`。

### 3. [P1] 断流短回避可能排除唯一支持目标模型的账号

**位置：** [store.go:1646](../internal/store/store.go#L1646)。

短回避在模型可用性筛选之前执行。只要基础池还剩其他账号，就会排除被回避者；但剩下的账号不一定支持本次模型。

**复现：** A 支持 Flash 且刚断流，B 停用了 Flash。A 先被回避删除，B 再被模型筛选删除，最后 `Select` 返回 nil。实际 A 仍可用，却向用户报无可用账号。

这违反 PLAN 中“软过滤永远不能让 Select 选不出号”的约定，默认可能影响断流后的 60 秒窗口。

**建议：** 先形成目标模型的可用候选集，再执行带兜底的软回避。

**证据：** `TestAuditSelectAvoidDoesNotHideOnlyUsableModel`。

### 4. [P1] 自动领取没有重新检查冷却、停用、失效状态

**位置：** [claim.go:55](../internal/adminapi/claim.go#L55)、[claim.go:221](../internal/adminapi/claim.go#L221)、[claim.go:339](../internal/adminapi/claim.go#L339)。

定时领取的候选过滤主要检查 JWT 和归档；循环只检查领取时间冷却 `claim.next_at`。真正执行的 `claimUnderGate` 重新取快照后，也不检查账号健康状态。

**复现：** 对 cooling、disabled、invalid 三种账号执行自动领取入口，三种都访问了本地模拟上游的套餐接口。手动领取路径中的健康检查没有保护自动路径。

**影响：** 管理员停用的账号仍被自动使用，风控冷却不能完整停止该身份的上游请求。

**建议：** 在取得领取槽位并获取最新快照后，统一检查归档、enabled、invalid 和未到期 cooling；正常额度耗尽但允许领新套餐的账号应单独允许，不能简单套用全部 `IsSelectable` 条件。

**证据：** `TestAuditAutoClaimMustHonorAccountHealth`。

### 5. [P1] 额度先归零再恢复，会提前解除尚未到期的风控冷却

**位置：** [quota.go:219](../internal/quota/quota.go#L219)、[quota.go:473](../internal/quota/quota.go#L473)。

额度回写只专门保护了风控 invalid，没有同样保护未到期 cooling。额度全零时无条件把状态改成 exhausted，随后额度恢复时又把 exhausted 改成 active 并清除冷却截止时间。

**复现：** 建立还有一小时才到期的风控冷却 → 额度返回 0 → 下一次返回 100 → 账号已提前 active。

**建议：** 将健康状态/冷却与额度状态分开，或在统一回写边界保留未到期的冷却状态，再更新额度快照。

**证据：** `TestAuditQuotaMustNotBypassRiskCooling`。

### 6. [P2] Responses 的 system/developer 输入被降为 user

**位置：** [responses.go:94](../internal/openai/responses.go#L94)。

`convertResponsesInput` 把所有非 assistant 的 message 都改成 user；顶层 `instructions` 的正确映射没有覆盖输入数组中的 system/developer 消息。

**复现：** 输入一条 system/developer 指令和一条 user 消息，结果没有顶层 system，二者被合并成同一条 user 消息。

**建议：** 与 Chat 转换一样，保留输入指令的优先级并按顺序归并到顶层 system；未知角色应明确拒绝，而非静默降级。

**证据：** `TestAuditResponsesMustPreserveInstructionRoles`，覆盖两个角色。

### 7. [P2] OAuth 明确选择直连，保存账号后仍会自动绑定代理

**位置：** [login.go:77](../internal/adminapi/login.go#L77)、[login.go:221](../internal/adminapi/login.go#L221)。

`__direct__` 被解析为空代理；保存新账号时，只要 ProxyURL 为空，就无条件调用 AutoAssignProxies，没有区分“未指定”和“明确选择直连”。

**复现：** 存在一条空闲代理，OAuth 会话明确选择直连，新账号最终仍绑定了该代理。登录兑换与后续额度/领取可能因此使用不同出口。

**建议：** 会话中保留显式出口模式（自动、直连、指定线路、自定义），仅自动模式自动分配；明确直连应清除既有指派。

**证据：** `TestAuditOAuthExplicitDirectMustRemainDirect`。

### 8. [P2] 代理配置写库失败，运行时配置却已经改变

**位置：** [store.go:719](../internal/store/store.go#L719)。

`saveProxyProfilesLocked` 先写内存并发布快照，再写数据库。与已修复的普通设置事务不同，代理配置没有相同的失败原子性。

**复现：** 创建代理 → 关闭测试数据库模拟持久化失败 → 更新代理收到错误 → ListProxyProfiles 却已返回新名称/地址。

**影响：** 管理端显示失败，但实际出口可能已改变；重启又回到旧配置。线路删除及账号改派的多步持久化也需要一并审视。

**建议：** 先在事务中提交线路与关联账号，再发布内存状态；失败保持原值。

**证据：** `TestAuditFailedProxyWriteMustNotChangeRuntime`。

### 9. [P2] Chat 流式转换丢弃 content_block_start 的初始正文

**位置：** [stream.go:132](../internal/openai/stream.go#L132)。

Chat 编码器只处理 tool_use 的 content_block_start，忽略 text/thinking 初始值；Responses 编码器则处理了这两种初始值。

**复现：** 上游把正文放在 text 类型的 content_block_start 中，随后正常结束，客户端收到 [DONE]，却没有该正文。

**建议：** 转发初始 text/thinking，再处理后续 delta；增加纯初始值和初始值加增量两种回归。

**证据：** `TestAuditChatMustKeepInitialBlockText`。本次证明的是协议边界问题，没有证明真实 Z.AI 当前会频繁产生这种帧。

### 10. [P2] HTTP 停机预算没有约束全部后台任务

**位置：** [quota.go:196](../internal/quota/quota.go#L196)、[quota.go:681](../internal/quota/quota.go#L681)、[claim.go:279](../internal/adminapi/claim.go#L279)。

HTTP Shutdown 已修复，但额度请求使用 Background context；Monitor.Stop 只关闭通知并等待，不能取消正在执行的刷新。定时领取的一整批任务也没有消费停机取消信号。

**复现：** 模拟额度请求阻塞，调用 Monitor.Stop 后不会取消该请求；只有人为释放模拟上游才返回。

**影响：** 整个进程的退出时间并不保证在 HTTP 的 10 秒预算内；容器可能在后台收尾完成前被强杀。一次额度请求已有 20 秒总超时，多批账号或领取操作可能更久。

**建议：** 使用统一应用 context；后台请求、并发槽位等待、账号间隔和批量循环都响应取消，并共享有限的关闭预算。

**证据：** `TestAuditMonitorStopCancelsInflightWork`。没有执行真实 Docker 强杀实验。

### 11. [P2] CLI quota 更新了数据库，却打印刷新前的旧副本

**位置：** [cli.go:337](../cmd/zcode2api/cli.go#L337)。

ListAccounts 返回克隆；FetchQuota 通过 Store.Update 更新库内对象，不更新传入的克隆。CLI 仍使用旧变量 a 显示结果。

**复现：** 模拟上游返回剩余额度 750，重新开库确认写入成功，但 CLI 输出“无额度数据”。

**建议：** 刷新后重新 Find/SnapshotAccount，再打印；同时处理刷新错误，避免用旧数据显示“实时额度”。

**证据：** `TestAuditCLIQuotaMustPrintFreshSnapshot`。

## 四、其他静态确认问题

这些问题有直接源码依据，但本次未补独立动态复现或对应平台运行测试。

| 优先级 | 问题与触发条件 | 位置及建议 |
|---|---|---|
| P2 | Intel macOS 的 platformTag 返回 darwin-amd64，但版本选择只接受 darwin-x64，落入 Windows 版本默认分支，并构造错误的平台包名。无现成缓存时自动下载链存在确定映射错误。 | [browserdl.go:74](../internal/captcha/browserdl.go#L74)，统一 amd64→x64，增加平台表驱动测试。 |
| P2 | CLI OAuth 重登命中旧账号时没有 RenewJWT；后台 OAuth 已补此修复，CLI 尚未同步。旧令牌失效时，重新授权仍可能继续使用旧令牌。 | [cli.go:156](../cmd/zcode2api/cli.go#L156)，复用后台的凭据更新服务。 |
| P2 | 设置页任意一个设置分组保存后刷新整个 settings；useEffect 无条件重置所有表单，会覆盖其他分组尚未保存的编辑。注释“仅在尚未编辑时同步”没有实现。 | [settings.tsx:60](../frontend/src/pages/settings.tsx#L60)，维护分组 dirty 状态或只同步对应分组。 |
| P2 | 单账号额度刷新忽略响应中的 ok=false，只要 HTTP 200 就提示刷新成功；后端恰好用 200+ok=false 表达业务失败。 | [accounts.tsx:526](../frontend/src/pages/accounts.tsx#L526)，检查业务结果后显示成功或失败。 |
| P2 | 成功率把 UseCount 当总尝试数，但其主要在成功响应处递增；失败通过其他路径累计。1 次成功+1 次计数失败可显示为 0%，而非 50%。部分错误还完全不计 FailCount。 | [monitor.go:26](../internal/adminapi/monitor.go#L26)、[dashboard.tsx:66](../frontend/src/pages/dashboard.tsx#L66)，独立记录请求总数、成功、失败、重试，明确粒度。 |

## 五、既有部署风险、设计限制与工程缺口

### 1. 反向代理后的后台限速会退化成共享锁定

[auth.go:65](../internal/auth/auth.go#L65) 按 RemoteAddr 限速，达到 10 次失败后在验证正确密码前直接返回 429。多个访问者经同一反向代理时共用一个 IP 桶，某个访问者可导致其他管理员被锁约 5 分钟。

这是 PLAN 已登记但尚未解决的部署风险，并非本次首次发现。README/Compose 中“绑定回环即可让该桶不再对外部流量生效”的解释不成立：绑定地址不会改变处理器看到的代理 RemoteAddr。

建议只对配置白名单内的可信反向代理解析真实来源，并由反向代理承担相应限速；不能直接无条件信任任意 X-Forwarded-For。

### 2. 入池自动领取忙时跳过是现行设计，不计作实现缺陷

本次确实复现了抢不到 claimSlot 的任务立即退出，但进一步核对 [PLAN.md:351](../PLAN.md#L351) 后确认明确规定“非阻塞抢占、抢不到即跳过、不排队”。

因此将对应诊断用例改为验证该既定行为，不列入上述 11 项缺陷。其产品限制仍然是：批量新增不保证每个账号都自动领取成功；默认关闭每日定时领取时，跳过的账号需要手动补领。若希望保证逐个处理，应修改契约并使用有界队列，而不是只改代码。

### 3. 不应当作新 bug 的既有边界

- async 默认 300 秒是整票总寿命，不是空闲超时；长请求超时属于已声明配置边界。
- Responses previous_response_id 被明确拒绝，当前为无状态兼容层。
- 导出不包含设备指纹和领取状态是现行格式设计，不据此判定导出丢字段。
- 上游 error 事件不计线路断流、16 MiB 非流式响应预算、普通设置原子提交、后台 OAuth RenewJWT 等近期修复已存在，不能重复报旧问题。

### 4. 测试与可维护性

- 现有测试偏向独立分支，缺少“两个状态操作接连发生”的组合回归。本次暴露的选号+回避、额度+冷却、CLI+服务并发写库都属于此类。
- CLI 覆盖率仅 5.9%，后台修复很容易漏同步到 CLI。
- 前端没有独立交互测试；CI 的 Go 编译只嵌入 dist，不自动证明源码与 dist 一致。此次一致，不代表后续提交有守门机制。
- lint 扫入压缩 dist，1,395 条生成代码警告掩盖了 10 条源码警告，应排除 dist。
- HANDOFF、PLAN、环境示例中仍有旧 Go 版本、Python/FastAPI、jsdom 等历史描述；部分同文件旧段落与新段落冲突，应整理有效契约。
- 并发检测、真实 SDK、真实上游和跨平台运行仍需独立验收；本地普通测试全绿不能替代这些验证。

## 六、建议修复顺序

1. 先修复会改变实际账号/出口的 P1：跨进程写库、SOCKS4 巡检误删、选号回避、自动领取健康检查、额度解除冷却。
2. 再修协议和用户操作：Responses 角色、OAuth 显式直连、代理事务、Chat 初始正文。
3. 补生命周期与 CLI：后台取消、实时额度输出、CLI 重登更新、macOS 映射。
4. 最后补前端交互、正确的监控指标、可信代理限速与 CI 守门。

重点应是收紧几个共享边界，而不是继续在每个入口添加局部判断：统一健康状态转换、统一账号凭据更新、统一代理构造、单写持久化、统一后台任务取消。

## 七、诊断测试与后续回归

审查阶段使用临时 Go overlay 和隔离数据库复现，未改动业务源码。
后续修复已将正式回归纳入仓库；文件清单与验收结果见
[修复方案及验收记录](plan-audit-fixes-20260930.md)。

本报告保留修复前证据，不将仅存在于开发机的临时文件路径作为公开依赖。
