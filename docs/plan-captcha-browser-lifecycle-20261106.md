# 验证码浏览器生命周期治理方案（2026-11-06）

- 适用仓库：zcode2api-plus（分支 go-rewrite，基线 a35359e）
- 触发问题：容器内 Chromium 常驻 3 天，renderer RSS 174M→1509M（+17.5MB/h）、CPU 3.4%→30.7%，而同期真实验证码消费 0 次
- 取证依据：`findings/browser-lifecycle-audit-20261106.md`、`findings/server-forensics-browser-20261106.md`、`findings/upstream-browser-parity-20261106.md`
- 本文性质：**方案，不含实现**。确认后按第 10 节顺序落地。

---

## 0. 先回答：P0–P3 都做吗

**做，但不是一个 PR 里的四个补丁。** 四件事性质不同，分成三批，依赖顺序明确：

| 批次 | 内容 | 性质 | 风险 | 能否独立回滚 |
|---|---|---|---|---|
| 第 1 批 | P0（idleStop 真停补）+ P3a（mint 汇总日志、monitor 真实状态） | 修 bug + 补观测，不改形态 | 极低 | 是（env 可关） |
| 第 2 批 | P1（worker 寿命闸门）+ 泄漏三处兜底（Close 加 Kill、Connect 失败路径、page Close 的 ctx） | 加保险，不改形态 | 低 | 是（阈值可关） |
| 第 3 批 | P2（池级空闲 Stop）+ P2b（claim 前预热）+ P3b（governor 改 cgroup 口径） | **形态变更**：常驻 → 按需 | 中（见 §9 指纹轮换） | 是（env 可关） |

**必须先做第 1 批的理由**：P2 的「空闲停池」现在根本生效不了——只要池还活着，每 95s 一次补货会把「最后使用时间」一直刷新，浏览器永远不算空闲。**P0 是 P2 的前置条件**，不是并列项。

**P1 即使做了 P2 也要保留**：P2 管「整池空闲」，P1 管「池活着时单个 worker 跑太久」。上游有套餐、claim 频繁的日子，池会长时间在线，P1 是那时的唯一上限。

**P3 分两半**：P3a（观测）跟着第 1 批走，因为后续两批的效果都要靠它验证；P3b（governor 口径）是独立小改，放最后。

---

## 1. 现状 → 目标

| 指标 | 现状（实测） | 第 1 批后 | 第 2 批后 | 第 3 批后 |
|---|---|---|---|---|
| 空闲期求解次数 | 909 次/天（≈每 95s 一次，永不停） | **0 次/天** | 0 | 0 |
| 空闲期浏览器 CPU | 30.7%（随天数上涨） | ≈0（页面不再被驱动） | ≈0 | 0（进程都不在） |
| 单 worker RSS 上限 | 无（3 天 1.5GB 线性） | 无（仍会长） | **≤0.8GB 触发重建** | ≤0.8GB |
| 空闲期常驻内存 | ~300MB（起步）～1.5GB | ~300MB | ~300MB | **0** |
| 浏览器换代频率 | 3 天 1 次（实测） | 不变 | ≤每 6h/5000 次一次 | 每次空闲停池一次 |
| 首次需要验证码的延迟 | 0（池热） | 0 | 0 | 8s 冷启动（可提前预热抵消） |
| 可观测性 | 成功 mint 无日志；控制台状态假 | mint 汇总 + 真实状态 | + 重建原因 | + CPU 口径正确 |

---

## 2. 不变式（任何一批都不许破坏）

1. **对外语义不变**：Take 命中/未命中、`ErrUnavailable`/`ErrSolverFailure`/`ErrSolveTimeout` 的分类、503 `captcha_required` 兜底路径（[gateway/engine.go:1269-1274](internal/gateway/engine.go#L1269-L1274)）全部保持。
2. **不打断在途求解**：任何回收只在两次求解之间发生。
3. **不改变浏览器启动参数与指纹生成方式**（除 §9 单独立项讨论）。
4. **诊断字段不减少**：`captcha_wait_ms`/`pool_size`/`empty_take`/`mint_fail` 继续输出，只增不减。
5. **默认行为可由 env 一键退回**：每批都留 `...=0/false` 的关闸开关。

---

## 3. P0：idleStop 真停补

### 3.1 问题

- [token_pool.go:670-689](internal/captcha/token_pool.go#L670-L689)：深空闲只把 `target` 压到 1；
- [token_pool.go:705-707](internal/captcha/token_pool.go#L705-L707)：`deepIdle && size >= 1` 才 `need=0`，而 `CaptchaPoolTTL=95s`（[config.go:104](internal/config/config.go#L104)）让桶里那条必然过期 → 过期后照补；
- 结果与 [docker-compose.yml:36](docker-compose.yml#L36)、[README.md:473-476](README.md#L473-L476) 的「空闲 15 分钟停止预解」直接矛盾。

### 3.2 改法

`refillOnce` 的深空闲分支改为**无条件不补货**，与桶内是否还有 token 无关：

```text
if idle > idleStop:
    target = 1（仅用于展示/统计）
    deepIdle = true
...
need = target - size - inflight
if deepIdle:
    need = 0            // 不再依赖 size>=1
```

**唤醒路径必须保持可用**（这是本改动的关键验证点）：真实请求走 `Take` → 池空 → 注册 waiter → `ensureMintsLocked`（[token_pool.go:355-361](internal/captcha/token_pool.go#L355-L361)）→ 照常并发求解。这条路径与 `refillOnce` 无关，因此**深空闲只是停掉"主动补货"，不影响"按需求解"**。

### 3.3 顺带修的两个观测缺陷

- **「空闲停补」日志永远打不出来**：`p.idleLog` 在缩容时置 true，深空闲分支被同一个标志挡住（[token_pool.go:670-689](internal/captcha/token_pool.go#L670-L689)）。改为两个独立标志（`shrinkLogged`/`stopLogged`），并把文案改准：「空闲停补（不再主动解）」。
- **成功 mint 静默**：见 P3a。

### 3.4 测试

- `TestTokenPoolDeepIdleStopsRefill`：`ttl=95s`、`idleStop=900s`，跑到深空闲后断言 `waitN(2*ttl)` 内 `mintTotal` 不增长；
- `TestTokenPoolDeepIdleStillServesTake`：深空闲下 `Take` 仍能触发一次求解并拿到 token；
- **修改现有** `TestTokenPoolIdleShrinkAndStop`（[token_pool_test.go:284](internal/captcha/token_pool_test.go#L284)）：把 `ttl` 从 `time.Hour` 改成生产默认 95s —— 这正是当初漏掉本缺陷的原因。

---

## 4. P1：worker 寿命闸门 + 泄漏兜底

### 4.1 闸门（新增，不改 Worker 接口）

新增可选接口，不破坏现有 fake：

```go
// WorkerRecycler 可选能力：是否需要回收重建（两次求解之间判定）。
type WorkerRecycler interface {
    ShouldRecycle(now time.Time) (bool, string) // reason 进日志
}
```

- 判定点：[pool.go:468-497](internal/captcha/pool.go#L468-L497) 每次 `Solve` 返回后、投递结果之后；
- 命中 → `countRestart()` + `closeWorker(w)` + `return false`（复用现有重建路径，slotLoop 会延迟 1s 重建）；
- `RodWorker` 实现三个阈值（任一命中）：**存活时长 / 求解次数 / RSS**。

RSS 读取：`launcher.PID()`（rod v0.116.2 `lib/launcher/launcher.go:529`）→ `/proc/<pid>/status VmRSS`（Linux）/ `ps -o rss=`（darwin）/ 跳过（Windows）。读不到就不判，只靠次数与时长。

**注意**：`--type=renderer` 是子进程，父进程 RSS 不能代表渲染进程。首版用「父进程 RSS + 时长 + 次数」三合一，其中时长/次数是主判据，RSS 只做兜底（阈值给宽，如 1.5GB）。

### 4.2 泄漏兜底（三处，均为「机制已证实、现场未触发」的防御性修复）

| # | 位置 | 现状 | 改法 |
|---|---|---|---|
| 1 | [pool.go:542-557](internal/captcha/pool.go#L542-L557) `closeWorker` | 5s 看门狗超时后只打日志，**不 Kill**，旧浏览器可能永生 | 超时后调用 `launcher.Kill()`（杀进程组，`launcher.go:534-548`），再走 Cleanup |
| 2 | [solve.go:249-255](internal/captcha/solve.go#L249-L255) Connect 失败 | 只 `l.Cleanup()`，而 Cleanup 阻塞在 `<-l.exit` → 永久卡住 | 改 `l.Kill()` 再 Cleanup |
| 3 | [solve.go:374-387](internal/captcha/solve.go#L374-L387) / :390-402 Close | page 绑的是**启动期 ctx**（[pool.go:418-420](internal/captcha/pool.go#L418-L420) 已 cancel），Close 可能直接返回 ctx.Err() | 页面改用 `context.Background()` 派生（或 `page.Context(context.WithoutCancel(ctx))`，Go 1.21+），保证 Close 真的发出 |

### 4.3 新增配置（默认值，全部可关/可调）

```ini
ZCODE_CAPTCHA_BROWSER_MAX_AGE_SECONDS=21600   # 6h；0=不限
ZCODE_CAPTCHA_BROWSER_MAX_SOLVES=5000         # 次；0=不限
ZCODE_CAPTCHA_BROWSER_MAX_RSS_MB=1500         # 父进程兜底；0=不判
```

### 4.4 测试

- `TestPoolRecyclesWorkerOnMaxSolves` / `...OnMaxAge`：假 worker 自增计数，断言到点后 Close 被调用且新 worker 上线、**在途请求不中断**；
- `TestPoolCloseKillsHungWorker`：假 worker `Close()` 永久阻塞，断言看门狗超时后 `Kill` 被调用、`serveLoop` 不泄漏；
- `TestRodWorkerRecycleReason`：三个阈值分别命中。

---

## 5. P2：池级空闲 Stop（形态变更）

### 5.1 语义

`BrowserSolver` 在「无在途求解 + 最后一次使用超过 N 分钟」时关闭整个池；下次需要时由现有 `acquirePool` 惰性重启（冷启动实测 7.6s，[日志 03:27:47→03:27:54]）。

### 5.2 实现要点

1. `Pool` 增加 `lastUse`（`Solve` 进入与结束时更新，持锁）；
2. `BrowserSolver` 起一个 60s 巡检（`Start` 时启动、`Close` 时停止），条件：`pool != nil && inFlight == 0 && now-lastUse > idle` → 走**现有** `retireCurrentLocked` + 锁外 `Stop()`（复用 inFlight/retired 机制，[browser_solver.go:213-226](internal/captcha/browser_solver.go#L213-L226)）；
3. **竞态**：Solve 刚拿到池指针、巡检就把池停了 → 该次 Solve 拿到 `ErrPoolNotStarted`。处理：`BrowserSolver.Solve` 对 `ErrPoolNotStarted/ErrPoolClosed` **重试一次**（重新 acquirePool，等价冷启动）；
4. **防抖动**：两次 Stop 之间设最小间隔（默认 30 分钟），避免「流量每 14 分钟来一次 → 反复冷启动」；
5. `Pool.Stop` 保持幂等（现有 `closeOnce`）。

### 5.3 新增配置

```ini
ZCODE_CAPTCHA_BROWSER_IDLE_STOP_SECONDS=1800   # 30min；0=关闭（退回完全常驻）
ZCODE_CAPTCHA_BROWSER_MIN_STOP_INTERVAL_SECONDS=1800
```

> 默认取 30 分钟而不是 15 分钟，原因是 §9 的指纹轮换风险；两者独立可调。

### 5.4 测试

- `TestBrowserSolverStopsIdlePool`：假池 + 注入时钟，空闲超时后 Stop 被调用；
- `TestBrowserSolverRestartsAfterIdleStop`：Stop 后下一次 Solve 触发 Start 且成功；
- `TestBrowserSolverIdleStopHysteresis`：每 10 分钟一次求求解 → 不 Stop；
- `TestBrowserSolverIdleStopRaceWithSolve`：并发 Solve 与 Stop，断言最多一次重试、不返回 5xx 语义错误。

---

## 6. P2b：事件驱动预热（可选增量）

- `Manager` 暴露 `EnsureWarm(ctx)`（幂等，直接 `acquirePool` + 触发一次 mint）；
- `ClaimScheduler` 在定时批次开始前 `prestart` 秒调用（默认 120s，覆盖 7.6s 冷启动 + 首条 token 失败重试）；
- 用 preview 结果做闸门：若上一步 preview 已知「无投放套餐」，跳过预热，连 8s 都不花。

配置：`ZCODE_CAPTCHA_BROWSER_PRESTART_SECONDS=120`（0=关闭）。

> 该增量只在 P2 生效后才有意义（P0/P1 阶段池本来就是热的）。

---

## 7. P3：观测

### 7.1 P3a（跟第 1 批）

| 项 | 现状 | 改法 |
|---|---|---|
| mint 成功无日志 | 3 天静默烧 CPU | 每 10 分钟一条汇总：`captcha 补解汇总 mint=+N 总=M 池=K 目标=T 空闲=Ns`；0 增量不打 |
| 控制台「驗證瀏覽器 online」 | 只看配置开关（[monitor.go:26-29](internal/adminapi/monitor.go#L26-L29)） | 改为读实际状态：`online/starting/idle_stopped/disabled`，并加 `worker_started`/`worker_solves`/`worker_rss_mb` |
| 重建原因不可见 | 只有「超时已替换」 | P1 的 `reason` 进日志；monitor 增 `worker_restarts_by_reason` |

前端只消费字符串，无需重建 dist。

### 7.2 P3b（最后一批）

`cpuGovernor` 的采样改为 **cgroup v2 `/sys/fs/cgroup/cpu.stat`**（容器内已确认可读：`usage_usec`），按 `usage_usec/wall` 得「用了几个核」，再对 `cpu.max` 配额算百分比；读不到 cgroup 时退回 `/proc/stat`。

> 理由：现在读的是**整机**口径，109% 的容器在 4 核机上只算 27% → factor 恒为 1，等于没生效（[cpu_governor.go:93-133](internal/captcha/cpu_governor.go#L93-L133)）。

---

## 8. 文档一致性修正（随第 1 批）

- [docker-compose.yml:36](docker-compose.yml#L36)、[README.md:473-476](README.md#L473-L476)：把「空闲 15 分钟停止预解」改为**如实描述**最终形态（第 1 批：空闲停止主动补货；第 3 批后：空闲停池）；
- 补一节「资源与回收」：单 worker 预算、重建阈值、冷启动代价。

---

## 9. 风险：指纹轮换（P2 的前置决策）

现实现每次启动浏览器都会**随机指纹种子**（[solve.go:113](internal/captcha/solve.go#L113) `fingerprint=10000+rand`）+ **每次新建临时 user-data-dir**（rod launcher 默认）。也就是说：

- 今天：换代频率 = 3 天 1 次（实测），设备身份几乎不动；
- 第 3 批后：每次空闲停池再启动 = 一次身份轮换；按 30 分钟阈值，最坏 48 次/天。

两条可选路线（需要你选）：

| 路线 | 做法 | 优点 | 缺点 |
|---|---|---|---|
| A. 保持轮换 | 不动指纹逻辑，只把 idle-stop 默认调到 30–60 分钟 | 实现简单，身份多样反而利于 stealth | 上游若按设备做风控，可能更容易触发验证码/限流 |
| B. 身份持久化 | 固定 `--fingerprint` 种子（配置或持久化文件）+ 持久 user-data-dir | 身份稳定，验证码通过率可预期 | 削弱 stealth；与「device_mid 必须稳定」（PLAN.md:929）方向一致但需重做启动参数 |

**建议**：先按 A 上线第 3 批，同时做一次小实验（连续 20 次停/起后跑一次真实验证码，统计通过率）；若通过率下降，再评估 B。**在此之前，第 1、2 批不受影响，可以先合并。**

---

## 10. 交付顺序与工作量

| 批次 | 内容 | 预估 | 分支/PR |
|---|---|---|---|
| 1 | P0 + P3a + 文档修正 | 0.75 人日 | `codex/captcha-idle-stop`，1 个 PR |
| 2 | P1 + 泄漏三处兜底 | 1.0 人日 | `codex/captcha-worker-recycle`，1 个 PR |
| 3 | P2 + P2b + P3b | 1.5 人日 | `codex/captcha-browser-on-demand`，1 个 PR |

每批独立过 CI（Ubuntu 全量测试）+ 独立回滚开关；第 3 批上线后观察 24 小时再看是否调阈值。

---

## 11. 待你确认

1. **第 3 批做不做**：做完是「空闲 30 分钟后浏览器彻底退出、下次冷启动 8s」；不做则停在「常驻但空闲不跑求解、内存靠 6h 重建压住」。
2. **指纹风险选 A 还是 B**（§9）。
3. **阈值**：worker 上限 6h/5000 次、池空闲 30 分钟、预热提前 120s —— 是否按这套默认走。

确认后我从第 1 批开始，先给 diff 说明再落代码。
