# 上游同步评估（2026-09-18）

上游 `gakiyukr/zcode2api-plus` 在分叉后新增了 49 个提交，主力是 2026-09-15 的一次
「缺陷审查」（提交信息里记作 M11 / M12），方向与我们完全不同：**我们在做功能与
伪装（z.ai 探测、代理分配、领取链路），他们在做并发正确性与运维**。

本文是逐项对照的结论，供决定是否跟进使用。**所有行号都是本次实际读过代码后记录的；
来自调研但未亲自复核的条目单独标注「未亲自核实」。**

## 结论速览

上游这批更新**有实质性帮助**，但价值集中在少数几处，且都不是「新功能」。

- **依赖层面没有变化**：我们的 `go.mod` 与上游**逐行一致**（仅注释措辞不同），
  所以"更新"全部发生在代码里。
- **最值得跟的是 5 个正确性缺陷**，其中 2 项（设备指纹可被客户端覆盖、异步路径
  不认 `200 + 业务错误`）都有明确的真实后果，改动都不大。
- **超过一半的更新对我们不适用**：访客投稿、Cap 校验、`manage.sh` 裸机部署这三块
  占了上游近半的新增代码，但都与「自用后台 + Docker 部署」的定位不符。

| 上游主题 | 我们的现状 | 是否值得跟 |
| --- | --- | --- |
| 缺陷审查批次（并发正确性、状态机） | 部分重叠（`55e7f23` 与上游同名同内容），**多数缺失** | **跟** —— 见后文 5 项 |
| OpenAI 兼容层补全（Codex） | 事件序列**已比上游更全**；缺 3 处细节 | 跟（低优先） |
| 部署运维（优雅停机、超时） | **完全没有** | 跟 |
| 验证码子系统加固 | 多数缺陷**同样存在** | 跟（中高） |
| 访客投稿 + Cap 校验 | 无对应物 | 不跟 |
| `manage.sh` 裸机部署 / 内部重构 | 无对应物 | 不跟 |

## 基线与分叉距离

| 项 | 值 |
| --- | --- |
| 分叉点 | `232cc7ec`（2026-09-12，`fix: clamp admin success rate to non-negative`） |
| 上游 HEAD | `23903667`（2026-09-17 21:05 北京时间，`chore: bump version to 2.0.7-go`） |
| 我们 HEAD | `60f90b0`（`chore: release v2.0.12-go`） |
| 规模 | 上游 49 个提交 / 78 个文件 / `+8400 −1106`；我们 35 个提交；状态 `diverged` |
| 依赖 | `go.mod` 与上游逐行一致 → **依赖层面无更新** |
| 重叠 | 我们 `55e7f23` 与上游 `76753093` **同名**（`align async pool with gateway`），已验证基本等价 |

⚠️ **版本号空间撞车**：双方都在发 `2.0.x-go`——上游到 `2.0.7-go`，我们到
`2.0.12-go`。两个仓库的 tag 无法互相区分。将来若同时引用，需要带仓库前缀说明。

## 上游这三天在做什么

49 个提交按主题归并：

1. **部署与运维**（9 个）：新增 `deploy/`（`manage.sh` 1443 行、`README.md` 476 行、
   `zcode2api.service`）、`--host` 支持反代、安装时预下载验证码浏览器、
   `manage.sh` 的 adopt/migrate、release 只发 `linux/amd64` + `linux/arm64`。
2. **缺陷审查 M11 / M12**（18 个，**价值最高**）：账号指针数据竞争、断连不该冷却、
   异步路径走代理、`200 + 业务错误`、额度信号覆盖强状态、设备指纹头被覆盖、
   优雅停机、keep-alive 回收、`/v1/responses` 事件序列。
3. **验证码子系统**（5 个）：worker 清理有界化、不在持锁下启动池、浏览器下载与
   解包的边界加固。
4. **内部重构**（3 个）：抽出 `internal/util` 收敛重复助手函数、
   `internal/auth` 抽出 `ClientHost`。
5. **新功能：访客投稿**（7 个）：`internal/guest`（386 行 + 487 行测试）、
   邀请码、按 IP 配额、`internal/capverify`（Cap 自托管 PoW 校验）、前端投稿页。
6. **发版**（5 个）：`2.0.3-go` ~ `2.0.7-go`。

## 值得跟进的 5 项缺陷

### A1. 客户端可以覆盖每账号设备指纹 —— 已核实

**位置**：`internal/upstream/request.go:79-102`，过滤清单在同文件 `:40-52`。

固定头先建（含 `X-Device-Mid`，取 `acc.DeviceMidOr(config.DeviceMid())`），
**之后**才合并客户端透传头：

```go
for key, value := range incomingHeaders {
    lower := strings.ToLower(key)
    if dropHeaders[lower] || strings.HasPrefix(lower, "x-zcode") {
        continue
    }
    headers[key] = value
}
```

`dropHeaders` 里有 `host`、`authorization`、`user-agent`、`cookie` 等 12 项，
**但不含 `x-device-mid`**。后果有两个：

1. 客户端送 `X-Device-Mid: 任意值` 就能改掉我们为每个账号分配的隔离指纹——
   而这正是我们花力气做设备伪装的目的。
2. 大小写不同（固定的 `X-Device-Mid` 与客户端来的 `x-device-mid`）会在 map 里
   形成**两个键**，最终发出两个同名头，上游取哪个不确定。

**上游修法**：客户端头合并完成后，把固定头**重新写回**（固定头必胜），并把键
规范化为 `textproto` 形式。

**回移成本**：小。改 `BuildRequest` 一处，加断言测试即可。

### A2. 异步路径不识别 `200 + JSON 业务错误` —— 已核实

**位置**：`internal/asyncpool/pool.go:461-531`（状态码分类），`:529-530` 直接
`return p.forwardSSE(...)`。

`if resp.StatusCode != http.StatusOK { ... }` 之后就是 SSE 转发，**没有检查
200 的 body 是否其实是 JSON 业务错误**。而同步路径早就处理了：
`internal/gateway/engine.go:246-260` 的 `handleUpstreamJSON`、`:400` 的
`case code == "1005"`，还有回归测试 `TestBusinessCode1005ExhaustsDailyQuota`。

`pool.go:468-469` 的注释自己写着：

> 分类顺序与 engine.handleUpstreamError 一致（PLAN §5.2），
> 同一账号在两条路径下必须标出相同状态。

**触发与后果**：额度耗尽的账号收到 `200 + {"code":1005}` 时被当成成功流——
客户端拿到空流，账号**不被标记任何状态**，下一次选号还会选中它，反复白耗。

**上游修法**：把 `handleUpstreamJSON` 抽出来供异步路径复用。

**回移成本**：小到中。逻辑已存在，主要是提取与接线。

### A3. 账号状态被并发刷回可用 —— 已核实

**位置**：`internal/gateway/engine.go:492-520`（`MarkModelExhausted`），
`:481-489`（`MarkAccount` 同样无守卫）。

`MarkModelExhausted` 的 else 分支：

```go
if anyState && allExhausted {
    acc.Status = model.StatusExhausted
} else {
    acc.Status = model.StatusActive   // 无条件
}
acc.CoolingUntil = nil                // 无条件清空
```

**触发与后果**：并发请求下，A 请求刚把某账号判为 `invalid`（凭证失效）或
`cooling`，B 请求的 402 就会把状态刷成 `active`、并清掉冷却时间 → **失效账号
重新回到轮询**，被反复选中、反复失败。

**上游修法**：加 `isStrongStatus(invalid / cooling / disabled)` 守卫，命中则跳过写入。

**回移成本**：小。加一个判定函数 + 两处守卫。

**相关**：这一项与下面的「Store 指针收敛」同根——因为我们读到的是**内部指针**，
锁外改字段才会互相覆盖。加守卫能挡住症状，根治要等指针收敛。

### A4. 客户端断连也会冷却账号 —— 已核实

**位置**：`internal/gateway/engine.go:226-231` 与 `:285-288`。

```go
resp, err := e.clientFor(acc).Do(httpReq)
if err != nil {
    e.mark(acc, model.StatusCooling, "连接失败: "+err.Error())
    return attemptResult{switchAccount: true}
}
```

`Do` 返回的错误里包含**客户端主动取消**（`context.Canceled`——用户关掉客户端、
上游超时）；读错误体失败（`:285-288`）同理。这些都与账号健康无关，却会让一个
正常的账号进入 **300 秒**冷却。

**后果**：单次中断即可连锁冷却最多 5 个账号（每次切换都记一次），把整池可用性
打下去。

**上游修法**：`isCanceled(err)` 判断——命中则不冷却、不换号，直接结束。

**回移成本**：小。两处判断。

### B1. 零值 HTTP server：无超时、无优雅停机 —— 已核实

**位置**：`cmd/zcode2api/main.go:85-90`。

```go
addr := fmt.Sprintf("%s:%d", config.Host, config.Port)
if err := http.ListenAndServe(addr, mux); err != nil {
    web.Err("main", "服务退出: "+err.Error())
    os.Exit(1)
}
```

缺三样：`ReadHeaderTimeout`（慢速攻击面）、`IdleTimeout`（空闲连接堆积）、
**优雅停机**。另外 `os.Exit(1)` 会**跳过所有 defer**——包括 `:74-81` 注册的
`mon.Stop()`（额度监控）与 `sched.Stop()`（每日领取调度器）。

**Docker 场景下的实际表现**：`docker compose down` / 重启时发的 SIGTERM 无人接管，
容器只能等约 10 秒被 SIGKILL，SQLite 与浏览器池都不会收尾。

**上游修法**：signal 通知 + `Shutdown`（10s 上限）+ `ReadHeaderTimeout 30s` +
`IdleTimeout 120s`（`WriteTimeout` 保持 0，SSE 需要长连接）。

**回移成本**：小，约 30 行。

## 值得借鉴但非缺陷

### 部署建议：反代后端口只映射到回环

上游在 `manage.sh` 与文档里记录了一个我们也会踩的坑：**经反向代理部署时，
所有请求的 `RemoteAddr` 都是代理地址**，于是后台登录的「单 IP 10 次失败」限速
会共用一个桶——**任何人的 10 次失败都会锁死整个后台**。

我们目前没做反代，风险未显现。建议在部署文档里写明：compose 端口映射用
`127.0.0.1:3000:3000` 而不是 `3000:3000`，让反代与后台同机。

### OpenAI 兼容层三处补强 —— 未亲自核实

调研结论：我们的 `internal/openai` 已被自己重写得**比上游更全**（blocks map、
`sequence_number`、content_part / reasoning / failed 事件都有），上游对该包的流式
改动大半已覆盖，只有三处细节缺失：

- `internal/openai/responses.go:119-124`：`function_call_output` 是硬 `.(string)`
  断言，收到数组时静默变成空串——工具输出丢失（Codex 场景常见）。
- `internal/openai/responses.go:300-306`：`responsesUsage` 只映射
  `input_tokens` / `output_tokens`，无缓存 token 与
  `input_tokens_details`——Codex 依 `cached_tokens` 判断上下文压缩时机，恒为 0
  会导致压缩时机错误。
- `usage` 在编码器层是覆盖式而非取 max，多段响应可能把 usage 归零。

**建议**：低优先，但都是小改动。

### 验证码子系统五处 —— 部分已核实

已核实的三处：

| 位置 | 问题 |
| --- | --- |
| `internal/captcha/solve.go:243-247` | `b.Connect()` 失败后对**未连接成功**的浏览器调 `b.Close()`（上游认定此处会 nil panic；实际是否必然 panic 取决于 rod 内部实现，本次未实机验证） |
| `internal/captcha/solve.go:350`、`:373-385` | `BrowserGetVersion` 与 `RodWorker.Close` 走 rod 的 background ctx，**不可取消**；浏览器挂死时会永久占住池槽位 |
| `internal/captcha/browser_solver.go:102-124` + `:127-159` | `acquirePool` **持锁**调用 `ensurePoolLocked` → `pool.Start()`，而 `Pool.Start()`（`pool.go:186-234`）会阻塞等待全部 worker 就绪 → 期间其他求解者全部阻塞且无法取消 |

未亲自核实的另两处（调研所得，写作时未逐一复核）：

- `internal/captcha/browserdl.go:47-48` + `:173-176`：`downloadMutex` 是普通
  `sync.Mutex`，`ensureVersion` 全程持锁 → 下载最长 10 分钟期间**不可取消**。
- `internal/captcha/browserdl.go:329-341`：符号链接分支有逃逸检查（`:321-324`），
  **硬链接分支没有**——直接 `filepath.Join(dest, hdr.Linkname)` 后 `ReadFile`，
  恶意压缩包可读取解包目录外的文件。⚠️ 但压缩包有 SHA256SUMS + Ed25519 签名
  校验链兜底，属深度防御。
- `internal/config/config.go:86`：`CaptchaSolveTimeout = envInt("ZCODE_CAPTCHA_TIMEOUT", 40)`
  **没有 `max()` 下界**（相邻的 `:90`、`:95` 都有），设 0 或负数时行为未定义。

**是否会触发**：代码默认是 `false`（`config.go:89`），但**我们的
`docker-compose.yml:41` 写的是 `${ZCODE_CAPTCHA_BROWSER:-true}`** —— 也就是说
我们实际的 Docker 部署下浏览器路径是**开启**的，JWT 账号首次请求即走该路径。
所以这一块优先级**中高**，不能按「默认关闭」忽略。

## 单独立项：Store 指针收敛（本次只出方案）

**问题**：`internal/store/store.go` 的读取函数把**内部 `*Account` 指针**交给调用方，
`Select` 直接返回指针，`engine` / `quota` / `asyncpool` 在**锁外**修改字段，再
`UpdateAccount` 写回。两条并发路径会互相覆盖 → A3 的状态被刷回、字段撕裂读。

**上游方案**：

1. 读返**深拷贝**（`Clone`），调用方拿到的是副本；
2. 写走新增的 `Store.Update(provider, id, fn)`，在**锁内**应用修改函数；
3. 删掉 `UpdateAccount`；
4. `MarkAccount` / `MarkModelExhausted` 改成接收 `(provider, id)` 而非指针。

**影响面**：约 **31 个调用点**，跨 `store` / `gateway` / `quota` / `asyncpool` /
`adminapi`。

**为什么不当场做**：

- 它是 A3 的**公共根因**，但 A3 的守卫可以先挡住症状，收益/风险比更好；
- 我们已在 **claim 路径**用 `claimMu` 独立解决了同类问题（回归测试
  `TestClaimStateConcurrentAccess`），指针收敛会与这套设计交叉；
- 会与代理分配逻辑（`AutoAssignProxies`、`DeleteProxyProfile` 的改派）冲突——
  这两处都依赖「在同一把锁内改多个账号对象」；
- 属于**架构级改动**，必须重跑 `-race`，而本机跑不了（无 C 编译器），只能靠 CI。

**建议**：单独立项。做的时候先设计 `Clone` 的字段清单（注意 `Account.Claim`
的不可变整体替换约定与 `json:"-"` 的运行期字段），再一次性替换调用点。

## 不建议跟进

| 项 | 理由 |
| --- | --- |
| **访客投稿**（`internal/guest` + 前端 `guest.tsx`） | 引入**公开写入口**：访客可提交自己的账号进池。我们的场景是自用后台批量加号，不对外开放；多一个公开面就多一份风险。 |
| **Cap 校验**（`internal/capverify`） | 只服务访客入口。它不是自研验证码，而是 **Cap（自托管 PoW）的客户端**，需要管理员自建实例。我们不做访客投稿则完全无用。 |
| **`manage.sh` 裸机部署** | 我们用 Docker + GHCR 镜像，已有完整的构建发布链。它的备份策略（只备份二进制、成功即删）反而**弱于**我们的卷持久化。 |
| **`zcode2api.service`** | 我们用容器，`restart` 语义已由 compose 的 `restart: unless-stopped` 覆盖。 |
| **release 只发两个平台** | 我们仍分发裸机二进制，`darwin-amd64` 等平台还有用（虽然 Docker 镜像已经是两个平台）。 |
| **设置页拆 tab + 两列栅格** | 纯 UI 重构。我们的设置页已分「鉴权 / 套餐自动领取 / 使用说明」三卡且不臃肿。 |
| **抽出 `internal/util`** | 收益是 DRY，但会动到大量文件的 import，与我们在这些文件里的既有改动冲突。 |

## 长期机制建议

本次比对用三步就能复用，建议**每有上游大动作时重跑一次**：

1. 求分叉点：GitHub compare API 跨 fork 比较，取 `merge_base_commit`；
2. 拉两边分叉后的提交清单（本次：上游 49 / 我们 35）；
3. 对上游每条 fix 类提交，读 patch 后**在本地代码里逐项核对**——不要凭提交标题
   判断我们是否受影响（本次就有两条同名提交实为同一修复）。

⚠️ **不要直接 merge 或在 GitHub 上点同步**：上游的 `internal/openai/*`、`Dockerfile`、
`docker-compose.yml`、`.dockerignore`、`HANDOFF.md` 都与我们同名但内容不同
（Docker 三件套是我们独立加的，上游那份是另一套实现；上游删掉了 `HANDOFF.md`）。
必须逐文件人工比对。

## 附录：证据

比对命令（`gh` 与远端仓库均为只读访问）：

```bash
GH="${GH:-gh}"   # gh CLI，需已在 PATH 中

# 跨 fork 比较，取分叉点与领先/落后数
"$GH" api "repos/gakiyukr/zcode2api-plus/compare/<上游HEAD>...fujunchao:<我们HEAD>" \
  --jq '{status, ahead:.ahead_by, behind:.behind_by, merge_base:.merge_base_commit.sha}'

# 上游分叉后的提交清单
"$GH" api "repos/gakiyukr/zcode2api-plus/compare/<分叉点>...<上游HEAD>" \
  --jq '.commits[] | "\(.sha[0:8])  \(.commit.message | split("\n")[0])"'

# 单个提交的补丁
"$GH" api repos/gakiyukr/zcode2api-plus/commits/<SHA> \
  --jq '.files[] | "=== \(.filename) ===\n\(.patch)"'
```

关键 sha：

| 用途 | sha |
| --- | --- |
| 分叉点 | `232cc7ec0e9b53d30ec144051d2908874adc7a29` |
| 上游 HEAD | `239036679a45ddef3023e4e805b9112e1e11fd2c` |
| 我们 HEAD | `60f90b03d0a4f4e5462cc0f1b029f205aa29a83c` |
| 双方同名的异步池修复 | 上游 `76753093` / 我们 `55e7f23` |
