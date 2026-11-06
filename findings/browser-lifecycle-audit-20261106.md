# 浏览器生命周期审计：常驻三天 / 1.5GB RSS / 30% CPU 的代码归因

- 审计对象：zcode2api-plus（分支 go-rewrite，HEAD a35359e）
- 审计日期：2026-11-06
- 性质：**只读代码审计**（未改任何 Go 代码、未动服务器）
- 服务器事实（由 Lead 初查提供，本文直接引用，未独立复核）：容器 zcode2api，镜像 ghcr.io/fujunchao/zcode2api-plus:v2.11.12-go，Up 9 分钟（2026-11-06T04:12:40Z 启动），RestartCount=0；env 仅 ZCODE_CAPTCHA_BROWSER=true、CLOAKBROWSER_CACHE_DIR=/data/cloakbrowser，其余走代码默认
- 标注约定：**【已证实】**= 本仓库代码可直接读出；**【推断】**= 由代码推演但需运行期确认；**【未证实】**= 有意向但证据不足，不给结论

---

## 0. 结论摘要

| 问题 | 结论 | 关键证据 |
| --- | --- | --- |
| 常驻是设计吗？ | **是**。worker 一旦创建，只在「worker 死亡」「求解超时判死」「验证码配置键变更」「进程退出」四种情况下关闭重建；**没有任何空闲超时、没有次数/时长上限、没有 RSS 上限** | internal/captcha/pool.go:408-445（slotLoop 只有 factory → serveLoop → sleepRestart 三步） |
| 空闲时会停止求解吗？ | **不会**。idle 分支只把补货水位压到 1（不是 0），而 token TTL=95s，所以空闲稳态仍以**约 1 次求解 / 95 秒**（≈38 次/时，≈909 次/天）永续拉浏览器 | internal/captcha/token_pool.go:667-713、internal/config/config.go:104 |
| 空闲设计能解释 30% CPU 吗？ | **不能**。按 0.63 次/分推算，空闲态浏览器 CPU 应是个位数百分比以下（§4.5 算术），30% 必须另有放大器 | 同上 |
| 30% CPU 最可能机制（**已修正**） | 运行期证据（2026-11-06）否证了「跑飞」：3 天 0 条补解失败、0 条求解失败。0.63 次/分的事件率在算术上不可能烧掉 0.3 核（需每次约 28 CPU·秒，实际约 0.3s，差约 100 倍），故 30% 只能是稳态连续消耗（见 §4.6）。原文备份：**补货失败无退避导致的跑飞**：refillOnce 只看水位，完全不读 mintFailStreak；失败时目标水位仍是 1–6、并发仍为 2，于是每次失败都会立刻再排一次求解；失败还会置 pageDirty 触发整页重载 | token_pool.go:649-721（无失败退避）、token_pool.go:444-451（mintFailStreak 只用于失败等待者）、internal/captcha/solve.go:305-310 |
| 1.5GB RSS 最可能机制（**已收敛**） | 运行期证据：3 天 0 失败、0 重载、0 重建，故只可能是**同一页面内的累积**（成功路径永不重载页面，≈900 次/天 × 3 天 ≈ 2700 次 → 约 550KB/次）；「失败重载的 target 泄漏」与「孤儿双浏览器」两条本次现场**可排除**（§7 修正） | solve.go:305-310、§4.6、§7 |
| CPU governor 能救吗？ | **不能**。它只把 refill 的 target 与并发乘 0.5/0.25，target 下限写死 1、并发下限 1，**无法停浏览器**；且读的是 /proc/stat 全机器占用，容器语义不成立 | internal/captcha/cpu_governor.go:77-89、token_pool.go:690-701、cpu_governor.go:93-133 |
| 代码里缺哪些回收点 | ①worker 空闲回收 ②worker 次数/时长/RSS 上限 ③补货失败退避 ④浏览器池空闲 Stop ⑤页面 Close 的 ctx 正确性 ⑥launcher.Kill() 兜底 | §2.3、§6、§8 |
| ~~重大交叉疑点~~ | **已推翻**：今天 04:12 是 docker restart（保留容器日志、不改 RestartCount），那只跑了 3 天的 Chromium 就是重启前同一容器里的同一只；原归属疑点不成立 | §7 |

一句话：**「常驻」是设计（且设计里确实没有任何回收闸门）；但「1.5GB + 30% CPU」不是常驻设计的水位——代码里有两条能把它推到那个量级的放大路径（失败无退避的跑飞、页面永不重置的累积），另有若干可疑泄漏点需运行期证实。**

---

## 1. 阅读范围

本仓库 internal/captcha/：

- pool.go（515 行）槽位池、worker 生命周期、超时判死
- browser_solver.go（250 行）Solver 实现、池的惰性启动与按配置键重建、失败冷却
- solve.go（527 行）rod worker、启动参数、页面构建、求解 JS、判死探测
- token_pool.go（810 行）预解 token 池、refill 水位、idle 分支、governor 接线
- cpu_governor.go（145 行）CPU 采样与缩放因子
- captcha.go（359 行）Manager、配置缓存、令牌缓存、求解器编排
- AliyunCaptcha.js.txt（224,097 字节、单行压缩）内联 SDK
- internal/config/config.go:82-117 验证码默认值

调用方：cmd/zcode2api/main.go:113-120、internal/gateway/engine.go:397-424、internal/asyncpool/pool.go:530、internal/claim/claim.go:472、internal/adminapi/accounts.go:589-616、internal/adminapi/monitor.go:26-46、internal/adminapi/claim.go:298-389。

依赖语义（仓库外，go.mod:14 锁定 github.com/go-rod/rod v0.116.2）：lib/launcher/launcher.go（Launch / Cleanup / Kill、leakless）、browser.go（Browser.Close）、page.go（Page.Close）、context.go（Page.Context）、lib/cdp/client.go（Call 的 ctx 语义）。

默认值（internal/config/config.go）：workers=1（:94）、startup=90s（:95）、request=45s（:96）、queue=60s（:97）、shutdown=10s（:98）、失败冷却 60s（:99）、TTL=95s（:104）、min=2（:105）、max=6（:106）、refill=1000ms（:107）、emptySolve=2（:108）、idleShrink=120s（:109）、idleStop=900s（:110）、governor=true（:113）、solveTimeout=40s（:90）。

---

## 2. 浏览器进程的完整生命周期

### 2.1 谁启动：四条触发路径，全部惰性

上游链路：Manager.getVerifyParam（captcha.go:290-343）→ token 池热取 pool.Take（captcha.go:324-326）或逐请求 solver.Solve（captcha.go:328）→ BrowserSolver.Solve（browser_solver.go:78-102）→ acquirePool 内 pool.Start()（browser_solver.go:155-156）→ Pool.Start 为每个 size 起一个 slotLoop（pool.go:201-204）→ p.factory = NewRodWorkerFactory（browser_solver.go:52）→ newRodWorker（solve.go:240-270）：launcher.New() → l.Launch()（solve.go:242）→ rod.New().ControlURL(url).Connect()（solve.go:246-249）→ loadSDKPage 建页面并加载 SDK（solve.go:275-301）。

触发条件只有四条：

1. JWT 模型请求：internal/gateway/engine.go:399（每次出站尝试前）
2. 异步池 JWT 请求：internal/asyncpool/pool.go:530
3. 套餐领取：internal/claim/claim.go:472（含换码重试，claim.go:471）
4. **后台验证码页读配置**：GET /admin/api/captcha/config 与 POST /admin/api/captcha/submit 都会调 FetchConfig（internal/adminapi/accounts.go:590、:610），成功后 Manager.FetchConfig 会 pool.SetConfig(cfg,true)（captcha.go:209-214）激活 token 池 → refill 开始 mint → 拉起浏览器。**即：不需要真实模型流量，管理员打开一次验证码页面就能把常驻 Chromium 启动起来。**

【已证实】池大小 = config.CaptchaBrowserWorkers（默认 1；browser_solver.go:52、config.go:94）。浏览器二进制每次 worker 创建都会重扫缓存目录（solve.go:230-237 → DiscoverBrowserBinary，solve.go:422-484）。

### 2.2 什么条件下关闭/重建：只有四种

| # | 条件 | 代码 | 语义 |
| --- | --- | --- | --- |
| 1 | 单次求解超过池的 requestTimeout（45s）→ 判死槽位 | pool.go:342-356（置 condemned）→ pool.go:477-483 closeWorker + return false | 重建 |
| 2 | worker 上报 ErrWorkerDead（浏览器退出/崩溃；classify 用 Browser.getVersion 3s 探测） | solve.go:353-371、pool.go:484-489 | 重建 |
| 3 | 验证码配置键（sceneId-region-prefix）变化 → acquirePool 走淘汰分支 | browser_solver.go:127-157、retireCurrentLocked browser_solver.go:213-226 | 换池 |
| 4 | 进程退出：cleanup → cm.Close()（main.go:166-180）→ BrowserSolver.Close（browser_solver.go:231-249）→ Pool.Stop/teardown（pool.go:240-279） | 停机 | 关闭 |

失败路径**不**重建：worker 明确回报求解失败（ErrSolverFailure）时池只记统计与日志，worker 保持健康（pool.go:491-496）；真实 worker 把页面标脏，下次求解前重载（solve.go:305-310，置脏点 solve.go:316、324、338、345）。

### 2.3 有没有「空闲多久就关浏览器 / 跑满 N 次或 N 小时就重建 / RSS 超限就重建」

**【已证实】全部没有。** 逐项核对：

- 空闲回收：internal/captcha 内 grep idle 的命中全部是 token 池水位与 pool.go 的 idle **通道名**（g.idle chan *slot，pool.go:116），**没有一处**按空闲时长关闭浏览器；serveLoop 入队后无限期阻塞在 case req = <-s.reqCh（pool.go:460-466），没有超时分支。
- 次数/时长上限：RodWorker 结构体只有 launcher/browser/page/cfg/solveTimeout/sdkLoadTTL/pageDirty（solve.go:215-227），没有计数或创建时间字段；serveLoop / slotLoop 也没有计数器。
- RSS 上限：全仓库没有读取 /proc/<pid>/status 或 VmRSS 的代码；rod 提供的 launcher.PID() 在项目里**从未被调用**。
- 整个 internal/captcha 只有一个 time.NewTicker：token 池 refill（token_pool.go:636）。没有任何后台协程检查浏览器健康/水位/内存。

结论：「一个 Chromium 常驻到进程退出」是**设计如此**，代码里没有任何回收闸门。docker-compose.yml:36 写的「空闲 15 分钟后 token 池会缩到 1 条并停止预解，CPU 自动回落」与代码不符：缩到 1 条是事实，「停止预解」不成立（§4.1、§4.2）。

---

## 3. 池里的页面复用语义

### 3.1 成功复用、失败/超时才 reload【已证实】

RodWorker.Solve 开头：if w.pageDirty { reload(); w.pageDirty = false }（solve.go:305-310）。pageDirty 只在四条路径置位：init JS 抛错（solve.go:316）、轮询 Evaluate 抛错（solve.go:324）、页面回报 error（solve.go:338）、求解超时（solve.go:345）。**成功路径不复位页面状态**，注释写明这是刻意设计：「成功路径复用同一页面，避免每次重新加载 224KB SDK（对齐 worker_main）」（solve.go:224-226）。

reload 的实现是关旧页 + loadSDKPage 建新页（solve.go:374-387），即失败会把整页（含 SDK）重建。

### 3.2 page 上常驻加载什么

buildCaptchaHTML（solve.go:146-152）生成最小页面，内联两段脚本：window.AliyunCaptchaConfig = {region,prefix} 与**整份 SDK 原文**（224,097 字节；solve.go:35-40 的 go:embed）。页面由 page.SetDocumentContent 写入（solve.go:281）；SDK 就绪判定是轮询 typeof window.initAliyunCaptcha === 'function'（solve.go:202、285-300），超时 20s（solve.go:205）。

每次求解注入的脚本见 solve.go:168-194：重置 window.__zcodeOutcome / __zcodeError，再调 window.initAliyunCaptcha({SceneId, mode:'popup', region, prefix, element:'#cap', button:'#btn', getInstance → startTracelessVerification, success/fail/onError})；随后每 250ms 轮询 {outcome,error}（solve.go:319-349）。

### 3.3 有常驻 JS 定时器吗（读 SDK 原文判定）

对 AliyunCaptcha.js.txt（单行压缩）做静态统计：

```
setInterval = 3                ← 全部是 core-js 的 polyfill 定义（module 504），不是真实用例
requestAnimationFrame = 0      requestIdleCallback = 0
visibilitychange = 0           toDataURL / getImageData = 0
MutationObserver = 2           addEventListener = 2          setTimeout = 5
appendChild = 7                removeChild = 3               iframe = 2
```

【已证实】内联 SDK 里**没有**常驻 setInterval / rAF 轮询者；它的工作是被调用时的网络 + JS 计算。因此「页面自己空转」解释不了 30% CPU；【推断】该页只有在 initAliyunCaptcha 被反复触发时才会持续吃 CPU——与 §4.4 的跑飞路径吻合。

【未证实】每次成功求解都在**同一页面**再调一次 initAliyunCaptcha，而 SDK 的 appendChild(7) 明显多于 removeChild(3)，存在「每次 init 往 #cap 里追加节点/隐藏 iframe 而从不回收」的可能，这会是长跑内存单调增长的直接原因。需 heap 快照才能定论，本审计不给结论。

---

## 4. token 池在「无真实流量」时的行为（算术）

### 4.1 refillOnce 的决策表【已证实】

refillOnce（token_pool.go:649-721）每 CaptchaPoolRefillMS=1000ms 跑一次（token_pool.go:634-646），顺序：更新 governor（654-656）→ 关闭/未激活直接返回（657-660）→ pruneLocked 清过期（661）→ 有等待者则 ensureMintsLocked 后返回（662-666）→ 算 idle/target（667-689）→ 乘 governor factor（690-697）→ need = target - size - inflight（702-704）→ deepIdle 且已有 1 条则 need=0（705-707）→ need>0 则 launchMintsLocked（708-713）→ 每分钟把 target 衰减 1、下限 min（714-719）。

关键三点：

1. **idle 分支把 target 压到 1，不是 0**（token_pool.go:671-683；idleStop 分支的日志文案就是「保底 1 条 token 唤醒」，token_pool.go:676）。
2. **TTL 只有 95s**（config.go:104），远小于 idleShrink=120s 与 idleStop=900s；桶里那 1 条 token 每 95s 必然过期被 pruneLocked 清掉（token_pool.go:606-625），随后 need = 1 - 0 - 0 = 1 → 再 mint 一次。
3. **target 下限是 1**（token_pool.go:695-697 的 if target < 1 { target = 1 }），所以 deepIdle 也永远 ≥1。

### 4.2 空闲稳态求解速率【推断，算式可验】

- t=0（首次 SetConfig 激活）：target=min=2，立刻补 2 条（token_pool.go:702-713，need=2 被 concurrency=ceil(2×1)=2 限住）。
- t=95s：两条同时过期 → 再补 2 条（此时 idle=95s<120s，target 仍 2）。
- t=190s 起：idle>120s → target=1 → 每 95s 补 1 条，**永久持续**（deepIdle 只把 need 归零当且仅当桶里已有 1 条；过期后照补）。
- 稳态：**≈1 次浏览器求解 / 95 秒 = 0.63 次/分 = 37.9 次/时 = 约 909 次/天**。

即：**「空闲也持续拉浏览器求解」是既定设计**，且因为 TTL < idleShrink，它在数学上不可能停。日志「token 池空闲停补（保留 1 条 token 唤醒）」（token_pool.go:676）是**误称**——停的是「加水位」，不是「补货」。

### 4.3 cron/探针/健康检查会不会刷新 lastTake

lastTake 只在 Take 里更新（token_pool.go:345，位于 closed/active 检查之后、取走 token 之前）。全仓库生产代码里 Take 的可达路径只有 3 条，全部来自 §2.1 的 getVerifyParam：

| 调用方 | 频率 | 是否刷新 lastTake |
| --- | --- | --- |
| internal/gateway/engine.go:399（JWT 模型请求） | 有流量才有 | 是 |
| internal/asyncpool/pool.go:530 | 有流量才有 | 是 |
| internal/claim/claim.go:472 | 每日定时领取，命中才调（internal/adminapi/claim.go:357-389，tick 30s 仅在 HH:MM 触发） | 是（零星） |

**没有**健康检查/探针/cron 周期性 Take：Dockerfile 的 HEALTHCHECK 打 /meta（Dockerfile:74-75）；/admin/api/monitor 只读 TokenPoolStats()（internal/adminapi/monitor.go:31-33；token_pool.go:724-757 不写 lastTake）；model probe 走自己的 request 层，只把 403 归类成 captcha_required（internal/modelprobe/request.go:111-112），从不取 token。

【已证实】所以 idle 分支**可达**，不存在「被探针永久钉住」的问题。反过来也说明：**只要有过一次模型流量或一次领取，lastTake 更新后 120s 内 target 会回到 2（空池时还会涨到 max=6，token_pool.go:353、627-631），空闲后每 95s 的补货量随之从 1 变成 2~6。**

### 4.4 真正的放大器：补货失败没有退避 → 跑飞【已证实；触发条件待服务器证实】

refillOnce 里**没有任何失败退避**：mintFailStreak 只在 ensureMintsLocked 里用于「给等待者发错误」（token_pool.go:444-451），水位计算完全忽略它（token_pool.go:667-713 无一处读 mintFailStreak / lastErr）。

只要 mint 持续失败（阿里云侧拒绝、SDK 报错、网络不通、上游风控等），稳态变成：

- 每个 refill tick（1s）都看到 size=0、inflight=0 → 再 launchMintsLocked(2)（token_pool.go:708-713）；
- 池只有 1 个 worker（config.go:94），两次 mint 串行 → 实际是「一次求解结束立刻接上下一次」，形成**连续求解**；失败越快频率越高；失败还会置 pageDirty → 每次都整页重载 224KB SDK（solve.go:305-310、374-387）；
- 每次失败打一条「token 补解失败 captcha_mint_fail=N」Warn（token_pool.go:491、496、510），无去重/限流（internal/web/logs.go:100-102），日志会以分钟级刷屏。

这条路径能同时解释「持续 30% CPU」与「内存单调增长」。**是否真在跑飞取决于服务器日志里 captcha_mint_fail 的增长速率与 monitor 的 captcha.mint_fail_total。**

> **【2026-11-06 运行期复核：本假设已被否证】** Lead 核实服务器日志：3 天全量日志里「补解失败」0 条、「验证码求解失败」0 条、「求解超时/提前退出/关闭超时」0 条；123 条请求日志的 captcha_wait_ms / captcha_pool_size / captcha_mint_fail 全为 0（真实请求从未从池里取 token）；浏览器只在 03:27:54 启动过一次，03:29:48 唯一一条「token 池空闲缩容至 1」之后再无 captcha 子系统日志（成功 mint 不打日志，token_pool.go:476-529）。**结论：没有跑飞；补货始终是 1 次/95s 且全部成功。**

### 4.6 30% CPU 的重新归因（运行期证据之后）

先做算术：0.3 核持续 3 天 = 每 95s 消耗约 28.5 CPU·秒。而一次成功求解的 CPU 成本量级是 0.1–0.5s（§4.5）。**要由「每 95s 一次求解」凑出 30%，每次求解必须占用约 28 CPU·秒——实际不可能**。所以 30% 必然来自**与求解事件率无关的稳态连续消耗**；这与「0 失败、0 重建」也自洽（CPU 是在两次求解之间的空当里烧掉的）。

按这个约束重排候选：

1. **累积 DOM 把某个逐帧开销放大（首选）**：单次 initAliyunCaptcha 会往 #cap 里 appendChild（SDK 内 appendChild 7 次 vs removeChild 3 次）并可能留下隐藏 iframe；2700 次后，只要页面里存在任何**持续产生帧**的东西（SDK 组件自带的 CSS 动画/transition 就够——CSS 动画不需要 JS，因此与「内联包没有 setInterval/rAF」不矛盾），每帧的样式重算/布局/软件光栅成本会随 DOM 规模**线性上升** → CPU 随时间单调上涨。放大因素：rod 默认带 --disable-background-timer-throttling / --disable-renderer-backgrounding / --disable-backgrounding-occluded-windows（rod v0.116.2 launcher.New 的 defaultFlags），隐藏页不降帧；--headless 无 GPU，走软件合成。
2. **1.5GB 堆带来的 GC 压力（次选）**：堆越大，标记/清扫的绝对成本越高，空闲期增量标记会持续吃 CPU；典型表现是 CPU 呈锯齿（GC 峰值）而不是平稳。
3. **可排除**：阿里云往返慢 / 轮询窗口长。求解轮询是 250ms 一次 CDP Evaluate（solve.go:348），即使一次 40s 的求解也只有 160 次往返，CPU 量级远不够；且现场 0 失败，求解都在 solveTimeout 内结束。
4. **待查**：cloakbrowser 补丁二进制的 --fingerprint 钩子（solve.go:113）是否逐帧生效。若是，则与页面内容无关，会表现为「换新页也一样烧」。

### 4.5 每次求解的成本量级【推断】

一次求解 = init JS（SDK 初始化 + 一次阿里云网络往返）+ 250ms 间隔轮询（solve.go:348），典型 0.5–3s 墙钟、CPU 时间约 0.1–0.5s（无 GPU 的无头 Chromium 做 JS 初始化 + 少量 canvas/DOM 工作）。

- 空闲设计（0.63 次/分）：0.63 × 约 0.3s ≈ 0.2 CPU·秒/分 ≈ **0.3% 单核**；即使按每次 2s CPU 也才约 2%。
- 跑飞（每次失败约 1s、连续求解）：接近 **100% 单核**，与观测到的「容器 109%、其中 Chromium 30%」同量级（其余可由 Go 进程与多个 chrome 子进程分别计入）。

结论：**空闲常驻本身只能解释个位数百分比 CPU；30% 必须另有放大器（跑飞或真实流量）。**

---

## 5. CPU governor 的真实边界

### 5.1 它只能缩放 refill 的 target 与并发【已证实】

- 采样：refillOnce 每次 tick 调 p.governor.Update()（token_pool.go:654-656 → cpu_governor.go:42-65，两次采样求差分）。
- 作用点只有两处：target = round(target × factor)（token_pool.go:690-697）与 concurrency = ceil(emptySolve × factor)（token_pool.go:698-701）。
- Factor() 只有三档：≥85% → 0.25；≥70% → 0.5；否则 1.0（cpu_governor.go:77-89）。
- **没有任何路径能让它停止浏览器、关闭池或中止在途求解**；target 被夹到 ≥1（token_pool.go:695-697），concurrency 被夹到 ≥1（token_pool.go:699-701）。只有 active=false 或 closed 时才完全不 mint（token_pool.go:657-660）。

### 5.2 109% 容器 CPU 时会落到什么行为

【推断】按代码，factor 取决于 governor 读到的**机器级**占用率：

- 若 /proc/stat 反映整机（4 核）→ 109% / 400% ≈ 27% → **factor=1.0，完全不限速**，target 与并发照旧；
- 只有整机忙到 70%/85% 才降 0.5/0.25；而 target 最低仍 1、并发最低仍 1 → **即使 100% 满载，也仍每 95s 补一次货**。

所以 governor 的作用只是「满载时把补货水位从最多 6 降到 1、把空池竞速从 2 路降到 1 路」，**对「浏览器常驻」零作用**，对「跑飞」也只是把并发 2 降到 1（单 worker 时本来无差别）。

### 5.3 /proc/stat 是全机器口径，容器语义不成立【明确判断】

readCPUSample 在 Linux 优先 readProcStatSample（cpu_governor.go:93-100），后者读 /proc/stat 的**聚合 cpu 行**并对 8 个字段求和（cpu_governor.go:107-133）。

判断：**这是整机（宿主机全部核心）口径，不是容器 cgroup 口径。** 理由：

1. /proc/stat 的 cpu 聚合行是内核全局计数；容器默认不虚拟化它（除非宿主挂 lxcfs），--cpus / cpu.max 之类的 cgroup 配额不体现在这里；
2. 分母是全部核心的累计 tick 和（cpu_governor.go:126），所以「109% 的容器」在 4 核机器上最多只贡献 27 个百分点；
3. 因此 70%/85% 阈值实际是「**整机**忙 70%/85%」，与其他容器、宿主上任何负载共享同一口径——在共享宿主机上既可能误判（别人的负载导致我方降级），更常见的是**判不出自己跑飞**（我方 100% 单核但整机仍空闲 → factor 恒为 1）。

**结论：这个 governor 在容器部署下语义不正确，不能当「CPU 保护」依赖；它对本次现象的归因价值≈0。** 想真正自适应应读 cgroup v2 的 cpu.stat / usage_usec（或 cpu.max 配额），而不是 /proc/stat。

---

## 6. 是否存在真实的泄漏路径（逐条，标注证实度）

### 6.1 closeWorker 看门狗放弃等待 → 旧浏览器可能变成进程内孤儿【可疑；机制已证实，后果未证实】

- closeWorker 起 goroutine 调 w.Close()，超过 closeWatchdog=5s（pool.go:162）只打印「worker 关闭超时，放弃等待」（pool.go:542-557），**不 Kill、不重试、不留句柄**。
- RodWorker.Close 的顺序是 page.Close → browser.Close() → launcher.Cleanup()（solve.go:390-402）。
- rod v0.116.2 的 Launcher.Cleanup() 实现是**先 <-l.exit、再删临时目录**（lib/launcher/launcher.go：func (l *Launcher) Cleanup(){ <-l.exit; ... RemoveAll(dir) }），即**一直阻塞到浏览器进程真的退出**，本身不杀进程；真正杀进程的是 Kill()（killGroup），而**本仓库从未调用 l.Kill()**（grep 无命中）。
- 于是：浏览器假死（CDP 不回话）→ browser.Close() 里的 proto.BrowserClose{}.Call(b) 走 b.ctx（无 deadline，见 rod browser.go 的 func (b *Browser) Close()）→ 可能长时间/永久阻塞；即便它返回错误，Cleanup() 也会因进程没退而永久阻塞 → 这个 Close goroutine 永久泄漏，**那只浏览器在进程内永远杀不掉**，而池已在 1s 后（restartDelay，pool.go:161）拉起新浏览器 → **双浏览器常驻**，内存翻倍。
- 【推断】这正好是「1.5GB ≈ 两只各 ~750MB」的一种可能形态；也可能是「一只 1.5GB」。区分方法见 §10（数 chrome 进程、看各自 RSS）。
- 唯一兜底是 leakless：rod launcher.New() 默认带 --leakless（lib/launcher/launcher.go 的 New() 注释与 flags.Leakless: nil；Launch() 里 if l.Has(flags.Leakless) && leakless.Support()），它只在**父进程（Go 进程）死亡**时杀浏览器——**对进程内孤儿无效**。

### 6.2 newRodWorker 的 Connect 失败路径会永久阻塞并泄漏【可疑；机制已证实】

solve.go:249-255：b.Connect() 失败时注释明确「不对这个浏览器对象调 Close()」，只调 l.Cleanup()。而 Cleanup() 等的是 l.exit（进程退出）——此时浏览器进程**还活着**（Launch 已成功），所以 Cleanup() 会**永久阻塞**，factory 永不返回 → slotLoop 卡死 → Pool.Start 90s 超时 → ErrStartup → BrowserSolver 进 60s 冷却（browser_solver.go:161-168）→ 冷却后重试再拉一只新的。**每次重试都留一只活浏览器 + 一个卡住的 goroutine。**

同类风险：loadSDKPage 失败路径（solve.go:256-261）先 b.Close() 再 l.Cleanup()，若 browser.Close() 阻塞（见 6.1）同样卡住。

### 6.3 页面的 Close 用的是**已被取消的启动 ctx**【可疑，但机制清晰】

- 池给 factory 的 ctx 是 startupTimeout(90s) 的派生，**factory 返回后立刻 fcancel()**（pool.go:418-420）。
- loadSDKPage 把页面绑在这个 ctx 上：page = page.Context(ctx)（solve.go:280）。rod 的 Page.Context 是**浅拷贝 + 换 ctx**（context.go：func (p *Page) Context(ctx context.Context) *Page）。
- RodWorker 保存的 w.page 就是这个绑定已取消 ctx 的页面（solve.go:256、262-269）；Solve 里每次都用 w.page.Context(reqCtx) 派生新副本（solve.go:311），所以**求解本身没问题**。
- 但 Close() 与 reload() 用的是字段本身：_ = w.page.Close()（solve.go:376、392）。rod 的 Page.Close() 第一条就是 context.WithCancel(p.ctx)，并用 proto.PageClose{}.Call(p)（page.go），而 cdp 的 Call 在 ctx 已结束时**直接返回 ctx.Err()**（lib/cdp/client.go：select { case <-ctx.Done(): return nil, ctx.Err() ... }），于是 Page.Close 拿到错误提前返回，旧 target 是否真被销毁**取决于那条是否已经发出**（cdp.Call 在 select 之前已 ws.Send），rod 侧还会把页状态当成已清理（cleanupStates）。
- 【未证实】因此每次失败重载**可能**留下未被追踪的旧 target/渲染进程。需运行期证据：Target.getTargets 列表长度是否随重载次数单调增长（§10）。

### 6.4 判死重建的「瞬时双浏览器」窗口【已证实是设计；影响量级取决于 Close 速度】

判死路径（pool.go:477-483）先 closeWorker（异步）再 return false → slotLoop 立刻 sleepRestart(1s) 后 factory **再拉一只**（pool.go:437-443）。所以**必然存在 ≥1s 的双浏览器重叠**；若旧的那只 Close 阻塞或慢，重叠会延长到永久（6.1）。这是 OOM 的直接触发器形态（2×~700MB 在 3.9GiB 机器上还能活，再多就危险）。

### 6.5 retired 池的回收依赖 inFlight 归零【已证实语义，风险有限】

acquirePool 淘汰旧池时，若 inFlight>0 就把池挂到 retired，由最后一个 release 去 Stop()（browser_solver.go:190-226）。若某次求解永不返回，inFlight 永不归零 → retired 池永不关闭 → 旧浏览器常驻。下一轮淘汰会立刻 Stop 更早的 retired（browser_solver.go:217-219），算是有个粗糙上限。

### 6.6 配置刷新不会回收浏览器，配置禁用也不会【已证实】

SetConfig 只清 token 桶、置 active 标志（token_pool.go:272-311）；active=false 只让 refill 提前返回（token_pool.go:657-660），**浏览器池照旧常驻**（只有 sceneId-region-prefix **变化**才换池，browser_solver.go:127-157）。所以「上游把 captcha 关掉」这类场景下，浏览器仍以 ~1.5GB 常驻且一分钟都不会被用到。

### 6.7 已证实「不会泄漏」的几处（澄清）

- Pool.teardown 放弃等待（pool.go:267-271）后，残留 slotLoop 仍会在 factory 返回时走 markReady 失败分支调 closeWorker（pool.go:433-436、518-526）——这条是干净的。
- tokenPool.Close 等 5s 后放弃在途 mint（token_pool.go:778-787），mint 的 ctx 由 mintCancel 取消（token_pool.go:775），不会永久挂住。
- 启动阶段任一 worker 失败会整体清场（pool.go:214-219、229-233）。

---

## 7. 归属疑点：**已推翻**（2026-11-06 运行期复核）

原假设是「容器 Up 9 分钟 ⇒ 那只跑了 3 天的 Chromium 不可能属于本容器（疑似宿主孤儿/别的容器）」。**该假设不成立。**

推翻依据（Lead 从服务器日志核实）：

- 容器 Created=2026-11-03T03:27:0x（日志首行 03:27:27），今天 04:12 是 docker restart。
- docker restart 的语义是停止后用同一容器配置重新启动：**保留容器日志、不增加 RestartCount**，只重置「Up 时长」。
- 因此「Up 9 分钟」只反映本次启动时刻；那只跑了 3 天的 Chromium 正是**重启前同一容器内**的同一只进程。

仍然成立的前提（不需要修改）：容器停止时 PID 命名空间销毁，**ns 内所有进程必然被杀**（init: true，docker-compose.yml:31；无 pid: host）。所以 04:12 的 restart 确实结束了那只浏览器；又因为浏览器池是惰性启动（§2.1），重启后无 JWT 流量就不会再拉起 —— 与「当前宿主无任何 chrome 进程」完全一致（Lead 核实）。

**修正后的推论（比原假设更重要）**：既然 3 天内 0 重建、0 重载、0 失败（日志核实），那 1.5GB RSS 只能来自**同一页面内的累积**，而不可能是 target 泄漏或孤儿双浏览器——§6.1（closeWorker 放弃等待）、6.3（页面 Close 死 ctx）、6.4（判死重建双浏览器）三条在本次现场**均可排除**：它们只由失败/超时路径触发，而本次一条都没触发。

---

## 8. 若要在不改语义的前提下加回收：挂载点与代价

前提：先修 §6 的可疑泄漏（否则回收只是把泄漏变成「定期重建 + 泄漏依旧」）。以下只谈设计。

| 方案 | 挂载点 | 能解决什么 | 影响的语义 / 代价 | 优先级 |
| --- | --- | --- | --- | --- |
| A. 失败退避 | tokenPool.refillOnce（token_pool.go:649-721） | 掐断「补货失败 → 立刻再求解」的跑飞，直接压 30% CPU | 不改对外语义；在 need>0 前读 mintFailStreak / lastErr 做指数退避（如 ≥3 次失败后按 min(60s, 2^n) 跳过 mint），并保留「有等待者时仍立刻解」（ensureMintsLocked 路径不动）。代价：真故障恢复最多延迟一个退避窗口 | **P0** |
| B. worker 级重建阈值 | pool.serveLoop 每次 Solve 结束后（pool.go:468-497） | 解掉「一次页面跑 3 天」的累积（内存/CPU 慢涨） | 在两次求解之间判条件（次数 / 存活时长 / RSS），命中就 closeWorker + return false 让 slotLoop 重建。**不打断在途求解**；token 池水位不变（池一直 IsStarted）；代价 = 每次重建一次冷启动（实测约 1–3s；最坏 90s 启动超时 + 60s 失败冷却）。需要 Worker 接口扩一个判定方法（RodWorker 可读 launcher.PID() 的 VmRSS 或自计数） | **P1** |
| C. 空闲回收（serveLoop 版） | pool.serveLoop 的等待点（pool.go:460-466，现在无限期阻塞） | 空闲时彻底释放 ~1.5GB | 给 case req = <-s.reqCh 加 idle 定时分支，命中则 closeWorker 并退出内层循环。语义代价：池容量从「常驻 size 个 worker」变成「按需」，第一个请求吃冷启动；且必须处理「槽位协程退出后 IsStarted 仍 true、g.idle 里没有槽位」的状态（pool.go:301-317 会等到 queueTimeout 60s 报 ErrQueueTimeout）——所以正确做法是「重建 worker 但不退出协程」（等同 B 的空闲特例） | P1（与 B 合并） |
| D. 整池空闲 Stop | BrowserSolver.acquirePool（browser_solver.go:111-188），复用其 inFlight/retired 机制 | 最干净地停掉整个浏览器池（释放内存、结束 leakless 子进程） | 需要「最后一次使用时间」+ 后台巡检（可借 tokenPool 的 ticker 回调，或在池空闲时反向通知 solver）。要求 inFlight==0 才 Stop（现成机制）；Stop 后下次 mint 会自动 Start（acquirePool 冷启动路径），语义自洽；代价 = 空闲后第一个请求吃冷启动 + Start 的 90s 超时预算 | P2 |
| E. 依赖 governor 限速 | — | 不可行（§5） | 阈值是整机口径，且 target 下限 1、不能停浏览器 | 不做 |

**推荐组合**：A（马上做，治 CPU）+ B（治内存长跑，空闲触发最保守）+ 修 §6.1 / 6.2 / 6.3；C / D 属于「要不要把常驻降级为按需」的产品决策，需单独立项——一旦做成按需，第一个请求就要吃冷启动，且 ZCODE_CAPTCHA_POOL_ENABLED=true 的「热取」承诺会打折（池空时回到逐请求求解路径 captcha.go:327-339，语义仍在但延迟变长）。

另注：若目标只是「空闲时内存能降下来」，最省事且语义最稳的是 B 的时长/RSS 阈值（把「3 天」压缩成「N 小时后重建」），而不是引入全新的按需模型。

---

## 9. 待证实清单（明说「未证实」）

1. ~~服务器上是否真的在「补货跑飞」~~ **已否证**（3 天 0 条 mint 失败，§4.4 复核段）。
2. 当前是**一只** 1.5GB 浏览器，还是**两只**各约 700MB（§6.1/6.4）：需数 chrome 进程。
3. ~~3 天寿命的那只 Chromium 是否属于当前容器~~ **已核实属于同一容器**（docker restart 语义，§7）。
4. 成功路径复用页面导致的 in-page 累积（§3.3）：需 heap 快照或 Target.getTargets 数量趋势。
5. 页面 Close 使用死 ctx 是否真的留下未销毁 target（§6.3）：需对比重载次数与 target 数。
6. 每次求解的实际 CPU 时间（§4.5）：需 docker stats 前后对比或 /proc/<pid>/stat 增量。

---

## 10. 服务器侧验证配方（只读，供 Lead / 运维执行）

日志关键字（全部由代码生成，可直接 grep 容器日志）：

| 关键字 | 出处 | 说明 |
| --- | --- | --- |
| 池已启动：N 个 worker 全部就绪 | pool.go:224 | 浏览器池启动时刻（一个进程生命周期内应只出现一次） |
| 真实浏览器验证码池已就绪 | browser_solver.go:185 | 同上（solver 层） |
| token 池已启用 | token_pool.go:234-236 | 池参数回显，可与 config 默认核对 |
| token 池空闲缩容至 1 | token_pool.go:681 | 空闲分支确实触发 |
| token 池空闲停补（保留 1 条 token 唤醒） | token_pool.go:675-676 | deep idle 触发（注意：并不真的停补） |
| token 补解失败 captcha_mint_fail= | token_pool.go:491、496、510 | 跑飞的直接证据（看条数/分钟） |
| worker N 求解超时，已替换 / worker N 提前退出 | pool.go:480、487 | 判死重建次数 |
| worker 关闭超时，放弃等待 | pool.go:554 | §6.1 的孤儿浏览器信号（出现即高度可疑） |
| 补丁 Chromium 就绪 / 开始自动下载 | browserdl.go:199、242 | 区分每次重启是否重下 |

指标（/admin/api/monitor 的 captcha 字段；internal/adminapi/monitor.go:31-33、token_pool.go:742-756）：take_total、empty_take_total、mint_total、mint_fail_total、pool_size、target、idle_seconds、cpu_percent。

判读法（间隔 10 分钟取两次快照）：

- take_total 不动而 mint_total 按约 0.63/分 增长 → **空闲设计下的正常补货**（§4.2），CPU 应在低位；
- mint_total 增速远高于 1/95s、且 mint_fail_total 同步上涨 → **跑飞**（§4.4），即 CPU 的直接原因；
- idle_seconds 一直很小（<120）而 take_total 在涨 → 有真实流量在持续取 token（此时浏览器本来就该忙）。

进程 / cgroup 取证（区分 §7 与 §6.1）：

```
docker top zcode2api -eo pid,ppid,etimes,rss,args | grep -i chrome
docker inspect -f '{{.State.StartedAt}} restarts={{.RestartCount}} pidmode={{.HostConfig.PidMode}}' zcode2api
# 若某 chrome 的 etimes > 容器 uptime，说明它不属于当前容器：
docker exec zcode2api sh -c 'cat /proc/PID/cgroup; readlink /proc/PID/ns/pid'
ps -eo pid,ppid,etimes,rss,args | grep -i '[c]hrome'   # 宿主机全量，可能看到别的容器/孤儿
```

---

## 附：与观测数值的对照小结

| 观测 | 最可能的代码机制 | 置信度 |
| --- | --- | --- |
| Chromium 长期不退（3 天） | 设计：无任何空闲/次数/RSS 回收（§2.3）；3 天这个具体时长另有容器归属疑点（§7） | 设计部分【已证实】；3 天归属【未证实】 |
| 1.5GB RSS | 页面永不重置的 in-page 累积（§3.3）+ 失败重载的 target 疑似泄漏（§6.3）+ 可能的孤儿双浏览器（§6.1/6.4） | 【推断】，需 §10 取证 |
| 30% CPU（Chromium） | **已修正**：跑飞假设被日志否证（§4.4/§4.6），改为「累积 DOM 放大逐帧开销」的稳态消耗估计；空闲设计本身只能解释个位数百分比（§4.5） | 【推断】，见 §4.6 判别式 |
| 容器 109% CPU | 上述浏览器 CPU + Go 进程/多个 chrome 子进程分别计入；governor 因整机口径不会限速（§5.3） | 【推断】 |

---

## 11. 与 task-3（上游对照）的交叉印证

读完 findings/upstream-browser-parity-20261106.md 后，两份审计在三点上独立吻合，可作为互相佐证：

1. **常驻是继承来的既定设计，不是 Go 重写新引入的形态**：上游 Python 的 worker 同样是「启动一次、常驻到 SHUTDOWN/崩溃」，没有 idle 超时、没有 max_tasks（该文件 §0 与 §4.1，取证于本仓库 git 历史 `27f7a3c:app/captcha_browser.py`）。本文 §2.3 的「无任何回收闸门」结论与之一致。
2. **「空闲停补」是文档 vs 实现的唯一实质偏离**：该文件 §0/§7 的结论与本文 §4.2 的算术完全对上——代码只维持 1 条滚动 token，而 TTL=95s，净效果是约每 95 秒仍求解一次；`docker-compose.yml:36` 的「停止预解，CPU 自动回落」不成立。
3. **token 预求解池与 CPU governor 是 Go 侧新增（借鉴第三方项目），Python 上游没有**：这解释了为什么「空闲期持续 mint」这条路径在上游不存在、也就从未被上游的踩坑记录覆盖；本文 §4.4 的「失败无退避跑飞」同样属于 Go 侧新增路径。

另外该文件给出的两条补充证据可直接引用：

- `internal/captcha/pool.go:540-541` 的注释逐字承认「残留浏览器进程由 leakless/进程退出兜底」——即本文 §6.1 的孤儿风险是**代码里明写的已知边界**，不是审计臆测。
- 项目风险登记把「验证码池 CPU/内存」列为中等风险（`docs/plan-server-start-plan-optimization-20261028.md:396`），预算口径是「每 worker 0.5–1 GB、容器 2 CPU / 2 GB 起步」（`docker-compose.yml:33`），而现场是 3.9GiB 机器上单容器吃到 1.379GiB——处于预算内但贴近上限，任何叠加（孤儿双浏览器 / 页面累积）都会越界。

