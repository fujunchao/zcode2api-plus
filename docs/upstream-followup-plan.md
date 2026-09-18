# 上游跟进实施计划（2026-09-18）

来源：`docs/upstream-sync-2026-09-18.md` 的评估结论。本文件把「建议跟进」的条目
拆成可执行任务，逐项落地。

**范围**：只跟上一次评估判定为「值得跟」的项，外加两处评估时未展开、但同属该批次
且改动很小的加固。评估判定为「不跟」的项（访客投稿、Cap 校验、`manage.sh`、
设置页拆 tab、抽 `internal/util`）不在本计划内。

## 任务总表

| 编号 | 任务 | 优先级 | 主要文件 | 状态 |
| --- | --- | --- | --- | --- |
| P1-1 | 固定请求头不得被客户端覆盖（含设备指纹） | 高 | `internal/upstream/request.go` | ✅ 已完成（`ad463ca`） |
| P1-2 | 异步路径识别 `200 + 业务错误` | 高 | `internal/asyncpool/pool.go`、`internal/gateway/engine.go` | ✅ 已完成（`5ace74e`） |
| P1-3 | 强账号状态不被额度信号刷回 | 高 | `internal/gateway/engine.go` | ✅ 已完成（`efc400d`） |
| P1-4 | 客户端断连不冷却账号 | 高 | `internal/gateway/engine.go` | ✅ 已完成（`efc400d`） |
| P2-1 | 优雅停机 + 请求头/空闲超时 | 中高 | `cmd/zcode2api/main.go`、`cli.go` | ✅ 已完成（`ce0c9a7`） |
| P3-1 | OpenAI 兼容三处补强 | 低 | `internal/openai/responses.go`、`stream.go`、`responses_stream.go` | ✅ 已完成（`a91185f`） |
| P4-1 | 验证码求解超时补下界 | 中 | `internal/config/config.go` | ✅ 已完成（`f065240`） |
| P4-2 | tar 硬链接补路径逃逸检查 | 中 | `internal/captcha/browserdl.go` | ✅ 已完成（`f065240`） |
| P4-3 | 浏览器池启动移出求解器锁 | 中高 | `internal/captcha/browser_solver.go` | ✅ 已完成（`ff014ff`） |
| P4-4 | Connect 失败后不对未连接浏览器调 Close | 中 | `internal/captcha/solve.go` | ✅ 已完成（`ff014ff`） |
| P5-1 | 部署文档：反代场景端口只映射回环 | 低 | `README.md`、`docker-compose.yml` | ✅ 已完成（`c1da509`） |
| — | Store 指针收敛（架构级） | 挂起 | — | 按决定只出方案 |

## P1-1 固定请求头不得被客户端覆盖

**目标**：客户端透传头**永远无法**改变网关固定头的取值，尤其不能改掉每账号的
设备指纹；同时消除大小写不同导致的重复头。

**现状**：`BuildRequest` 先建固定头（含 `X-Device-Mid`），再合并客户端透传头，
过滤表 `dropHeaders` 不含 `x-device-mid`。下游 `httpReq.Header.Set(k, v)` 会把
两个只差大小写的键归一到同一个，**map 迭代顺序随机 → 最终值不确定**。

**步骤**：
1. 客户端头**先**合并，键统一走 `textproto.CanonicalMIMEHeaderKey`；
2. 固定头（含条件性的验证码头）**最后写回**，固定头必胜；
3. 保留 `dropHeaders` 与 `x-zcode` 前缀过滤作为第一道防线。

**验证**：新增测试断言——传入 `X-Device-Mid` / `x-device-mid` 的客户端头后，
结果里该头只有**一个**、且等于账号指纹；`Authorization` 不能被覆盖；
普通透传头（如 `anthropic-beta`）仍能通过。

**产出**：`internal/upstream/request.go` 改造 + `request_test.go` 新用例。

## P1-2 异步路径识别 `200 + 业务错误`

**目标**：让异步（空闲池）路径与同步路径对同一账号标出**相同状态**——这正是
`pool.go` 注释自己承诺的性质。

**现状**：`attemptUpstreamOnce` 对非 200 做完整分类；**200 直接 `forwardSSE`**。
额度耗尽账号收到 `200 + {"code":1005}` 被当成成功流：客户端拿空流、账号不标状态、
下次继续被选中。

**步骤**：
1. 把 `gateway.messageFromJSON` 导出为 `MessageFromJSON`（供异步路径复用），
   更新 `engine.go` 内唯一调用点；
2. `attemptUpstreamOnce` 在 200 之后先看 `Content-Type`：含 `application/json`
   即按业务错误分类，否则才进 `forwardSSE`；
3. 分支与同步路径逐条对齐：
   - `1005` → `MarkModelExhausted` + 换号（返回 `errNetwork`）；
   - `3007` → `Captcha.Invalidate()` + `errCaptchaRejected`（同账号换令牌重试）；
   - 其他非空非 `0` → `FailCount++` + 投递 error 事件 + `errDelivered`；
   - 空 / `0` → 视为「上游未返回有效 SSE」，与同步 `case stream` 同语义。

**验证**：新增 async 用例——上游回 `200 + {"code":1005}` 时，该模型被标 exhausted
且换号（不是静默成功）。

**产出**：`internal/asyncpool/pool.go`、`internal/gateway/engine.go` + 测试。

## P1-3 强账号状态不被额度信号刷回

**目标**：`invalid` / `cooling` / `disabled` 三种与「额度用没用完」无关的状态，
不得被额度信号改写。

**现状**：`MarkModelExhausted` 的 else 分支无条件 `Status = active` 且
`CoolingUntil = nil`；并发下能把刚判 invalid 的号刷回可用，失效号重回轮询。

**步骤**：
1. 在 `gateway` 内加 `isStrongStatus(status)`（`invalid` / `cooling` / `disabled`）；
2. `MarkModelExhausted` 的两个落状态分支都先判强状态，命中则跳过状态与冷却写入；
3. `MarkAccount` **不改**——它是显式设置状态的入口，被守卫反而会失效。

**验证**：新增用例——账号先被标 `invalid`，再来一次 402，断言状态仍是 `invalid`。

**产出**：`internal/gateway/engine.go` + 测试。

## P1-4 客户端断连不冷却账号

**目标**：与账号健康无关的取消（客户端断开、服务关停）不得让账号进入 300 秒冷却。

**现状**：`e.clientFor(acc).Do(httpReq)` 出错即 `mark(cooling)`；读错误体失败
（`handleUpstreamError`）同样无条件冷却。用户关掉客户端就会冷掉一个健康账号，
一次中断连锁最多 5 个。

**步骤**：
1. 加 `isCanceled(err)`：`errors.Is(err, context.Canceled)` 或 `DeadlineExceeded`；
2. `Do` 出错时若为取消 → 不标记、不换号，直接返回结束结果；
3. `handleUpstreamError` 的读错误体失败分支同样处理。

**验证**：新增用例——请求 context 预先取消，断言账号状态**未被**改为 cooling。

**产出**：`internal/gateway/engine.go` + 测试。

## P2-1 优雅停机 + 请求头/空闲超时

**目标**：容器收到 SIGTERM 能有序退出，defer 链（存储、验证码池、额度监控、
领取调度器）全部收尾；补上慢速攻击与空闲连接的超时。

**现状**：`http.ListenAndServe(addr, mux)` 零值 Server；错误路径 `os.Exit(1)`
跳过所有 defer。

**步骤**：
1. `serve()` 返回退出码（`func() int`），`cli.go` 的 `serveFn` 类型同步调整，
   `main()` 在 `serve()` 返回后才 `os.Exit`，让 defer 生效；
2. 显式构造 `http.Server`：`ReadHeaderTimeout 30s`、`IdleTimeout 120s`，
   **不设 `WriteTimeout`**（SSE 需要长连接）；
3. 监听 `os.Interrupt` 与 `SIGTERM`，收到后 `Shutdown`（10s 上限），
   超时只告警不强杀；
4. `ListenAndServe` 返回 `http.ErrServerClosed` 视为正常退出。

**验证**：新增测试断言 Server 的两个超时已设置、`WriteTimeout` 为 0。

**产出**：`cmd/zcode2api/main.go`、`cli.go` + 测试。

## P3-1 OpenAI 兼容三处补强

**目标**：Codex 等客户端的工具轮次与用量统计不出错。

**现状与步骤**：
1. `responses.go` 的 `function_call_output` 是硬 `.(string)` 断言 → 收到数组时
   工具输出静默变空。改为同时接受字符串与内容块数组；
2. `responsesUsage` 只映射 `input_tokens` / `output_tokens` → 补缓存 token 与
   `input_tokens_details`（键名与 chat 路径 `mapUsage` 保持一致）；
3. 流式 usage 目前是覆盖式 → 改为取 **max**，避免上游补发的 `input_tokens: 0`
   把已有统计归零。

**验证**：三处各补一个断言用例。

**产出**：`internal/openai/*` + 测试。

## P4-1 验证码求解超时补下界

**目标**：`ZCODE_CAPTCHA_TIMEOUT` 设 0 或负数时行为可预期。

**步骤**：`config.go` 的 `CaptchaSolveTimeout` 加 `max(1, …)`，与相邻两项一致。

**验证**：随既有 config 用例覆盖（或直接看代码常量）。

**产出**：`internal/config/config.go`。

## P4-2 tar 硬链接补路径逃逸检查

**目标**：与符号链接分支同级防护——解包不得读取解包目录之外的文件。

**现状**：symlink 分支有 `IsAbs` / `..` 检查，**硬链接分支没有**，直接
`filepath.Join(dest, hdr.Linkname)` 后 `ReadFile`。

**步骤**：硬链接分支加同样的校验，并且**校验清理后仍在 dest 内**（仅挡 `..` 前缀
不足以覆盖 `a/../../x` 这类形态）。

**验证**：构造含逃逸硬链接的 tar，断言解包返回错误。

**产出**：`internal/captcha/browserdl.go` + 测试。

## P4-3 浏览器池启动移出求解器锁

**目标**：启动浏览器（最长 90s）期间不阻塞其它求解者，且取消能生效。

**现状**：`acquirePool` 持 `s.mu` 调 `ensurePoolLocked` → `pool.Start()`，
而 `Start()` 阻塞等全部 worker 就绪。

**步骤**：把「构造 + Start」挪到锁外执行，锁内只做检查与状态登记；启动成功后
再回到锁内落 `pool` / `poolKey`，并处理「启动期间配置又变了」的竞态。

**验证**：现有假池工厂用例覆盖启动路径；新增用例断言启动期间另一调用者不被阻塞。

**产出**：`internal/captcha/browser_solver.go` + 测试。

## P4-4 Connect 失败后不对未连接浏览器调 Close

**目标**：避免对未连接成功的浏览器对象调 `Close()`（上游认定此处会 nil panic）。

**现状**：`solve.go` 的 `newRodWorker` 在 `b.Connect()` 失败后调 `b.Close()`，
随后 `l.Cleanup()`。`Cleanup()` 自身已经会杀掉进程并清理临时目录。

**步骤**：Connect 失败分支只保留 `l.Cleanup()`。

**验证**：本机无法跑真实浏览器，靠代码审查 + 编译；现有 captcha 用例不回归。

**产出**：`internal/captcha/solve.go`。

## P5-1 部署文档：反代场景端口只映射回环

**目标**：避免反向代理部署时所有请求的 `RemoteAddr` 都是代理地址，导致后台登录
的「单 IP 10 次失败」限速共用一个桶——任何人的 10 次失败锁死整个后台。

**步骤**：在 README 的代理/部署段落与 `docker-compose.yml` 注释里写明：
经反代时应把端口映射为 `127.0.0.1:3000:3000`。

**验证**：文档改动，人工核对表述。

**产出**：`README.md`、`docker-compose.yml` 注释。

## 挂起项：Store 指针收敛

按上次的决定**只出方案，本次不动**。方案要点已写在
`docs/upstream-sync-2026-09-18.md` 的对应章节（读返深拷贝 + 新增
`Store.Update(provider, id, fn)` 锁内写、删 `UpdateAccount`，约 31 个调用点）。

它是 P1-3 的公共根因：守卫能挡住症状，根治仍要靠指针收敛。

## 执行顺序与提交切分

按依赖与风险从低到高推进，每项独立提交、逐项验证：

1. P4-1、P4-2（自包含、可离线测试）
2. P1-1、P1-3、P1-4（`upstream` 与 `gateway` 的小改，含测试）
3. P1-2（跨 `gateway` + `asyncpool`）
4. P2-1（`cmd` 层）
5. P4-3、P4-4（验证码，本机无法实机验证，改动面最小化）
6. P3-1（openai 兼容层）
7. P5-1（文档）

全部完成后统一跑：`go build ./...`、`go vet ./...`、全量测试
（`-count=1`，跳过本机必失败的符号链接用例）、`gofmt-check.js`；
前端本次无改动，故未重建 `dist`。

## 实施结果（2026-09-18 完成）

11 项全部落地，逐项独立提交（**已本地提交，未推送、未发版**——比原计划的
「不提交」多做了一步，因为仓库惯例是一项改动一个提交，且本地提交可随时 `reset` 回退）：

```
c4f869f style: add the space gofmt expects after a comment marker
c1da509 docs: warn about the shared rate-limit bucket behind a reverse proxy
a91185f fix: accept array tool output and report cache tokens in the responses api
ff014ff fix: start and stop browser pools outside the solver lock
ce0c9a7 fix: shut down gracefully and bound the http server timeouts
5ace74e fix: classify 200 responses carrying a business error in the async path
efc400d fix: keep quota signals and client cancels from corrupting account state
ad463ca fix: keep client headers from overriding the gateway fixed headers
f065240 fix: reject escaping tar hardlinks and bound the captcha solve timeout
```

验证：`go build ./...` 与 `go vet ./...` 通过；全量 14 个包测试通过
（`-count=1`）；`gofmt-check.js` 74 文件 0 不合规；`windows/amd64`、`darwin/arm64`
交叉编译通过（`syscall.SIGTERM` 在 Windows 可用）。

### 与原计划的偏差（如实记录）

1. **P4-3 多了一项「启动中」闩**。第一版只把 `Start` 移到锁外，结果并发冷启动时
   日志显示**启动了 3 个池**（改动前由锁串行化，只启 1 个）——每个池背后都是真实
   浏览器进程。补了 `starting` 通道闩后恢复为一个。这一条是原计划没预料到的，
   已补测试 `TestBrowserSolverConcurrentColdStartBuildsOnePool` 钉住。
2. **P4-2 顺带修了一处平台差异**：`filepath.IsAbs("/etc/passwd")` 在 Windows 上返回
   false（缺盘符），会让校验结果随平台漂移；改为显式挡前导分隔符。由新增的
   `TestSafeArchiveName` 暴露出来。
3. **P2-1 改了 `serve()` 的签名**（`func()` → `func() int`），因此 `cli.go` 的
   `serveFn` 类型同步调整。原计划只说「返回退出码」，没写清这处连带改动。
4. **P4-4 无法实机验证**：本机没有可用的浏览器环境跑通整条浏览器求解链路，
   该改动（Connect 失败分支不再调 `Close()`）只做了代码审查 + 编译 +
   既有 captcha 用例不回归。
5. **P4-3 的并发正确性只在本机验证了一半**：`-race` 本机跑不了（无 C 编译器），
   需靠 CI 的 `go test -race` 才能最终确认。这是本次最需要 CI 把关的一项。

### 已知未覆盖

- `internal/captcha/browserdl.go` 解包用 `string(archive)` 会多复制一份内存
  （上游顺手改了，本次未动，属性能而非正确性）。
- `internal/captcha/solve.go` 的 CDP 调用仍走 rod 的 background ctx（不可取消，
  浏览器挂死时会占住槽位）——修它需要改 rod 调用方式，风险高于本次范围。
- Store 指针收敛（见上）仍挂起。

