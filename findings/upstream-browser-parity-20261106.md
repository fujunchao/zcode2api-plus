# 上游对照：常驻 Chromium 是"继承的既定设计"还是"Go 重写新引入的形态"

- 日期：2026-11-06
- 仓库 / 分支 / HEAD：`D:\augment-projects\zcode2api-plus` / `go-rewrite` / `a35359e`
- 取证方式：只读。仓库内文档 + `git` 历史 + 上游 Python 原文（在本仓库 git 历史里）；**未修改任何 Go 代码、未登录服务器、未改动容器**。
- 关联共享任务：task-3（横向对照）

---

## 0. 结论速览

| 问题 | 结论 | 关键证据 |
|------|------|----------|
| 常驻 Chromium 是上游继承还是 Go 新增？ | **继承**。上游 Python 的 worker 进程同样是"启动一次、常驻到 SHUTDOWN/崩溃" | `27f7a3c:app/captcha_browser.py:638,649-655` |
| 上游有 idle 超时自动退出吗？ | **没有**。无 idle 超时、无关机保温 | `captcha_browser.py` 全文无 idle/time 相关回收；`_idle` 只是空闲 worker 下标队列 |
| 上游每 N 次求解重启进程 / max_tasks？ | **都没有**。`max_tasks` 在 Python 与 Go 两个时代均无命中 | `git log -S 'max_tasks'` 空；全仓 grep 无 `MaxTasks` |
| 上游 token 预求解池默认开启？水位多少？ | **上游（Python）根本没有 token 预求解池**；浏览器 token 明确"不缓存" | `27f7a3c:app/captcha.py:85,132` |
| 我们的实现相对上游是等价/更强/偏离？ | 浏览器池**等价**（仅传输层不同，且已书面声明）；token 预求解池 + CPU governor 是**Go 侧新增**（借鉴第三方项目，非 Python 上游） | `internal/captcha/pool.go:1-13`、`docs/analysis-zcode-api-reference-20261028.md:556,580` |
| docs 里有没有"常驻是刻意选择"的书面依据？ | **有，且是明写的** | `docker-compose.yml:63`、`README.md:473-474`、`PLAN.md:198-211` |
| docs 里有没有记过"浏览器常驻导致内存/CPU 问题"的踩坑？ | **没有**。只有"资源预算建议"，没有故障复盘 | `docs/` 全目录 grep 无相关记录（见 §2.3） |

**一句话**：常驻浏览器是本项目**从第一版 Python 就有的既有设计**，Go 重写**忠实继承**并在注释里显式声明了差异；真正新增的是"后台持续预求解"（token 池），而**文档声称它在空闲 15 分钟后"停止预解"、代码实际是"维持 1 条滚动 token ⇒ 约每 95 秒仍解一次"**——这是本次唯一发现的"文档 vs 实现"偏离，也是"零流量时浏览器仍有周期性 CPU 工作"的唯一代码来源（见 §7）。

---

## 1. 先界定血缘：这里有三个"上游"

| 代号 | 是什么 | 位置 | 备注 |
|------|--------|------|------|
| **P** | Python 版 zcode2api（真正的原始上游） | 本仓库 git 历史 `27f7a3c`（Go 重写提交 `d01c740` 的父提交） | 根提交 `39ef045`；**GitHub 上已不可见**，见下 |
| **G** | Go 版 zcode2api-plus（本仓库，即"我们/Go 重写"） | `a35359e`（`go-rewrite`） | Python 作者 fujunchao 自己写的重写 |
| **R** | 第三方参考项目 `zcode-api`（Go/Bun 的另一实现） | 仅存在于分析文档中，非本仓库代码 | token 池 + CPU governor 的设计来源 |

**重要事实**：本仓库的 remote 与 `AGENTS.md` 的约定**方向相反**——

```
origin   https://github.com/fujunchao/zcode2api-plus.git   ← 上游作者本人的仓库
upstream https://github.com/gakiyukr/zcode2api-plus.git    ← 另一个 fork
```

也就是说 **Go 版本身就是 fujunchao 的作品**，不是"我们另起炉灶"。因此本文对"偏离"的判定标准是：**相对 Python 版 P 的行为差异**；而"是否有决策记录"以 G 自己的 PLAN/docs 为准。

**Python 原文为何只能从 git 历史取**：GitHub `fujunchao/Zcode2Api` 的 `main` 现在已是 Go 版（`GET /repos/fujunchao/Zcode2Api/contents/` 返回根目录为 `.env.example, .gitignore, README.md, README_RU.md, build.cmd, build.sh, cmd, go.mod, go.sum, internal`，无 `app/`），`commits?path=app/captcha_browser.py` 返回 0 条。
- 仓库主页：<https://github.com/fujunchao/Zcode2Api>
- Go 版主页：<https://github.com/fujunchao/zcode2api-plus>
- Python 原文复现命令：`git show 27f7a3c:app/captcha_browser.py`（下文引用均为此路径行号）

---

## 2. 仓库内证据

### 2.1 为什么选"进程内 goroutine 持有常驻 rod 会话"而不是"每请求起一次浏览器"或"空闲关闭"

**（a）设计目标在 PLAN.md 里写得很直白：照抄 Python 的池语义，而不是重新设计。**

`PLAN.md:198`（§5.5 验证码标题）：
> `### 5.5 验证码（照抄 `captcha_browser.py` 的思路与字符串）`

`PLAN.md:204-206`：
> - **成功复用同一页面，失败重载页面清 SDK 内部状态**；token 只在内存。
> - **池语义对齐**：goroutine worker 槽位 = 并发上限；请求超时 → 关闭该页面/浏览器实例并替换，
>   迟到的 TOKEN 通过 context 取消保证**绝不误投**后续请求；…

`PLAN.md:91`（文件映射表）：
> `│   ├── pool.go            # ← app/captcha_browser.py（goroutine 池，语义对齐：超时杀页不误投）`

`PLAN.md:62`（技术选型）：
> `| 浏览器自动化 | `github.com/go-rod/rod` | 验证码求解；复用 cloakbrowser 下载的 Chromium 二进制 |`

**（b）"进程内 goroutine"这一形态差异是显式声明过的**，见 `internal/captcha/pool.go:1-13`（文件头注释，逐字）：
> ```
> // 有界浏览器 worker 槽位池：对应 Python 版 app/captcha_browser.py 的池职责。
> //
> // 与 Python 版的差异：Python 每个槽位是一个 cloakbrowser worker 子进程，经
> // READY/GET_TOKEN/TOKEN/FAILED/SHUTDOWN 行协议通信；Go 版 worker 是进程内
> // goroutine 持有的 rod 浏览器会话，没有行协议，池只保留其调度与生命周期语义：
> //   - 有界并发：size 即并发求解上限，同一 worker 上请求严格串行。
> //   - 启动：全部槽位在启动总超时内就绪，任一失败或超时整体失败并清场。
> //   - 超时隔离：请求超时即判死该槽位——放弃在途结果并丢弃 worker 重建；
> //     迟到的求解结果只会被丢弃，绝不投递给后续请求（杜绝响应错位）。
> //   - 失败分类：worker 明确报错视为求解失败（worker 仍健康，复用页面）；
> //     ErrWorkerDead（浏览器退出/崩溃）触发替换。
> ```

**（c）为什么不是"每请求起一次浏览器"**：上游（P）根本没这么设计——它同样是"每槽一个常驻 worker"（见 §4.1），所以 Go 版是在**保持同一形态**，不是做了取舍。附带理由（本文推论，非文档明言）：启动总超时 90 秒（`internal/config/config.go:95`，与上游 `app/settings.py:70` 同值），每请求起浏览器等于把这段冷启动代价放到 JWT 热路径上，与 P0-B 的"首字节 p95 < 200 ms"目标直接冲突（`docs/plan-server-start-plan-optimization-20261028.md:188`）。

**（d）为什么不是"空闲关闭"**：代码里**从来就没有**浏览器级 idle 关闭机制。`git log -S 'IdleStop'` 只命中一个提交 `447d185`，而该提交的 idle 逻辑落在 **token 池**（`internal/captcha/token_pool.go`），不在浏览器池。即：**从 M5 落地至今，浏览器池的回收路径只有三种——求解超时判死、worker 崩溃、池 Stop（配置键变更 / 进程关闭）**，见 `pool.go:433-498`（`slotLoop`/`serveLoop`）与 `browser_solver.go:213-226`（`retireCurrentLocked`）。

### 2.2 有没有明确写过"资源代价"与"回收策略"

**有，而且写得相当具体。**

| 内容 | 证据 |
|------|------|
| **"每个 worker 是一个常驻 Chromium，CPU/内存按 worker 数线性增长"** | `docker-compose.yml:63`（逐字） |
| 每个 worker 预留 **0.5–1 GB** | `docker-compose.yml:33`、`README.md:473-474`、`docs/handoff-m2-p0b-20261028.md:49` |
| 容器起步 **2 CPU / 2 GB** | `docker-compose.yml:33`、`README.md:473`、`docs/plan-server-start-plan-optimization-20261028.md:178` |
| 用 `init: true` 收尸，避免僵尸进程堆积 | `docker-compose.yml:30-31` |
| **回收策略书面记录**：空闲 120 s 缩到 1 条、900 s 停补（保留 1 条唤醒） | `docs/handoff-m2-p0b-20261028.md:40`、`internal/captcha/token_pool.go:10` |
| CPU governor：≥70% 减半、≥85% 降到 1/4，任何情况保留 1 路 | `internal/captcha/cpu_governor.go:75-89`、`docs/handoff-m2-p0b-20261028.md:39` |
| 风险登记里承认"验证码池 CPU/内存"为中等风险 | `docs/plan-server-start-plan-optimization-20261028.md:396` |
| 超时/崩溃替换后残留进程**只能靠 leakless 与进程退出兜底** | `internal/captcha/pool.go:540-541`（逐字："残留浏览器进程由 leakless/进程退出兜底"） |

**关键落格**：`docs/plan-server-start-plan-optimization-20261028.md:175`（P0-B 设计第 4 条）：
> `- 空闲 120 秒缩容、900 秒停补；保留 1 个 token 供唤醒。`

以及验收口径 `同文档:191`：
> `- 空闲 15 分钟后 CPU 回落，池缩到 1。`

注意这里的措辞精度：**验收写的是"CPU 回落"，不是"浏览器关闭"**。所以"浏览器常驻不关"在项目里**从未被当成缺陷**，反而 `docker-compose.yml:63` 把它当成需要预留资源的既定事实。

### 2.3 有没有反过来记录过"浏览器常驻导致内存/CPU 问题"的踩坑

**没有。** 检索范围：`PLAN.md`（含 §8 风险与对策表、§6 决策记录）、`README.md`、`docs/` 全 200+ 篇、`HANDOFF.md`。

- `PLAN.md` §8 风险表里与浏览器相关的只有两条，都不涉及内存/CPU：`PLAN.md:907`（rod 过不了风控）、`PLAN.md:912`（rod 版本 API 变动）。
- `docs/` 中 `常驻` 的 21 处命中全部与账号池 / async 票 / 后台面板 / 官方 Source Server 相关，**无一处针对验证码浏览器**。
- 与 captcha 浏览器池相关的所有历史文档，记的都是"**功能/并发/超时**"类修复（见 §3），没有一条是"内存涨到 X GB / CPU 占用 X%"的复盘。
- 唯一接近"资源"字样的记录是上面 §2.2 的**预算建议**与"空闲 CPU 回落"的**验收承诺**，不是故障记录。

**推论（未证实）**：这条"3 天 1.5 GB / 30% CPU"的现场，在本项目里**没有先例文档可对照**，因此不能靠"历史上就爱出这问题"来解释，也不能靠"文档说过会回落"来免责——需要按 §7 的机制逐项核。

---

## 3. git 历史：回收/保温相关提交

`git log --oneline -- internal/captcha` 共 **23 条**。与"生命周期/回收"直接相关的关键提交如下（按时间正序）。

| # | commit | 日期 | 意图（作者原文摘要） | 与"常驻/回收"的关系 |
|---|--------|------|---------------------|-------------------|
| 1 | `29af676` | 2026-09-11 | `feat: add rod browser captcha pool (M5)` | **常驻形态的出生提交**。原文："bounded worker slot pool with Python-aligned semantics (workers=1, startup 90s / request 45s / queue 60s / shutdown 10s / failure cooldown 60s); request timeout condemns the slot and late results are never delivered to later requests"。→ **建池即常驻，没有任何 idle 回收，与 Python 一致**。 |
| 2 | `53782f8` | 2026-09-11 | `feat: auto-download patched Chromium…` | 让容器能自举二进制；不涉及生命周期。 |
| 3 | `f065240` | 2026-09-18 | `fix: reject escaping tar hardlinks and bound the captcha solve timeout` | 给求解超时设上界（`CaptchaSolveTimeout`）。 |
| 4 | `ff014ff` | 2026-09-18 | `fix: start and stop browser pools outside the solver lock` | **直接触碰"Stop"路径**：`pool.Start()` 最长 90 s 持锁会把 Close 一起钉住，容器等不到优雅退出被强杀（对应 `docs/upstream-sync-2026-09-18.md:183`："容器只能等约 10 秒被 SIGKILL，SQLite 与浏览器池都不会收尾"）。→ 说明"池的关闭"一直被当作**进程退出/配置变更**事件，不是空闲事件。 |
| 5 | `115c103` | 2026-09-19 | `fix: return the captcha slot when a cancelled request is abandoned` | 槽位泄漏修复："a slot that never receives its request is also never re-enqueued… with the default size of 1 that retires the whole captcha pool while IsStarted still reports true"。→ **单位置池的容量泄漏**曾是真实缺陷（已修）。 |
| 6 | `0aae845` | 2026-09-19 | `fix: give the captcha liveness probe a deadline` | 假死浏览器会让槽位永不归队："a hung browser (CDP socket connected but silent) blocked classify forever… the whole captcha pool stays dead while IsStarted still reports true"。→ 这是**"浏览器活着但卡住"**这一类问题的唯一记录，用的是 3 s CDP 探针（`solve.go:207-211,353-371`）。 |
| 7 | `447d185` | 2026-10-28 | `feat(captcha): add pre-solved token pool and last-good config`（作者 fujunchao） | **新增后台持续预求解 + idle 缩容/停补 + CPU governor**。这是"浏览器零流量时仍被唤醒"的**唯一来源**。 |
| 8 | `2fb7e97` | 2026-10-28 | `fix(captcha): deliver pool tokens through waiter queue` | 重写 token 池交付路径（+165/−106），不改 idle 语义。 |

**反向检索结果（证明"没有"什么）**：
- `git log -S 'max_tasks'` → **空**。两个时代都没有"每 N 次重启"。
- `git log -S 'IdleStop'` / `-S 'IdleShrink'` → 仅 `447d185`（token 池，非浏览器）。
- `git log -S 'closeWorker'` → 仅 `29af676`（首次引入时就有，且只挂在超时/崩溃/Stop 上）。
- 全仓 grep `MaxTasks|MaxAge|max_tasks` 在 `internal/captcha` 下 **0 命中**（其余命中属 proxysub 的结果有效期，无关）。

---

## 4. 上游 Python 原文对照（逐条回答必查项）

> 复现：`git show 27f7a3c:app/captcha_browser.py` / `git show 27f7a3c:app/captcha.py` / `git show 27f7a3c:app/settings.py`

### 4.1 worker 进程是否常驻？—— 是，常驻到 SHUTDOWN 或崩溃

`app/captcha_browser.py:629-655`（逐字）：
```python
def worker_main(argv: list[str]) -> int:
    ...
    browser = launch(headless=True, args=list(_BROWSER_ARGS))   # 638：只启动一次
    ...
        page = _load_page(browser, args)                        # 645：页面只加载一次
        ...
        print(READY, flush=True)                                # 649：报告就绪
        for raw in sys.stdin:                                   # 650：**阻塞等指令，不退出**
            line = raw.strip()
            if line == SHUTDOWN:                                # 652-653：唯一正常退出条件
                break
            if line != GET_TOKEN:
                continue
```

同一文件 `661-669`（逐字）：
> ```python
>                 # 每次求解失败都重载页面，清掉 SDK 内部状态；成功路径
>                 # 则继续复用同一页面，避免每次重新加载 224KB SDK。
> ```

→ **成功复用页面、失败重载页面**的语义，与我们的 `RodWorker.pageDirty` 完全一致（`internal/captcha/solve.go:224-226,305-310,373-387`）。

### 4.2 是否有 idle 超时自动退出？是否每 N 次求解重启进程？是否有 max_tasks？

**三项全部为否。** 证据：

1. `app/captcha_browser.py` 全文 `_idle` 共 7 处，全部是 `self._idle: deque[int]`（空闲 **worker 下标**队列）的声明/清空/出入队：`:147` 声明、`:211`/`:268` clear、`:316-317` popleft、`:338`/`:474` append。**没有一处承载"空闲时长"或计时语义。**
2. 全文 `monotonic` 只有 2 处（`:615-616`），都在 `_solve_once` 内部，是**单次求解**的超时循环；`time.sleep` 也只有 `:625` 的 0.25 s 轮询间隔。**没有一处用于 worker 寿命。**
3. 全文无 `max_tasks` / `restart_after` / `idle_timeout` 之类标识符；`git log -S 'max_tasks'` 在整个仓库（两代代码）**零命中**。
4. worker 重建只由三件事触发：**请求超时**（`_request` → `_mark_broken`，`captcha_browser.py:341,505`）、**EOF/非零退出**（`_on_eof`）、**spawn OSError**；池关闭（`stop()`）另算。与我们的 `serveLoop` 三分支一一对应（`pool.go:477-496`）。

### 4.3 池参数是否等价？—— 6 项全等价

| 参数 | Python 默认（`app/settings.py`） | Go 默认（`internal/config/config.go`） | 判定 |
|------|-----------------------------------|----------------------------------------|------|
| 启用开关 | `ZCODE_CAPTCHA_BROWSER` = **false**（:66） | `ZCODE_CAPTCHA_BROWSER` = false（:93） | 等价 |
| workers | `ZCODE_CAPTCHA_BROWSER_WORKERS` = 1（:69） | = 1（:94） | 等价 |
| 启动总超时 | 90（:70） | 90（:95） | 等价 |
| 单次请求超时 | 45（:71） | 45（:96） | 等价 |
| 队列等待 | 60（:72） | 60（:97） | 等价 |
| 优雅停机 | 10（:73） | 10（:98） | 等价 |
| 启动失败冷却 | 60（:74） | 60（:99） | 等价 |

（附注：`BrowserWorkerPool` 类自身的 `request_timeout` 形参默认 75.0 见 `captcha_browser.py:122`，但 `captcha.py:148` 显式传入 settings 的 45，**实际生效值 45**。）

### 4.4 token 预求解池在上游是否默认开启、水位是多少？—— **上游没有这个池**

`app/captcha.py` 的取码路径（`get_verify_param`）是**逐请求同步求解**，且**浏览器 token 明确不缓存**：

`app/captcha.py:83-91`（逐字）：
```python
            browser_token = await self._solve_browser(config)
            if browser_token is not None:
                # 上游验证码参数可能是一次性的，不能像人工回填一样缓存 45 秒。
                return browser_token

            token = await self._solve(config)
            self._cached = token
            self._cached_at = time.time() * 1000
            self._cached_ttl = settings.CAPTCHA_CACHE_TTL
```

→ 浏览器路径**直接 return，连 45 秒缓存都不给**；只有 Node/人工令牌才进 `_cached`。
→ 全文无后台 refill / 分桶 / 水位 / TTL 池 / CPU governor；`app/` 下也没有 `token_pool.py` 之类文件。
→ 上游 `.env.example` 里也没有任何 `CAPTCHA_POOL_*` 变量（只有 `ZCODE_CAPTCHA_TIMEOUT/RETRIES/CAPTCHA_CACHE_TTL/CAPTCHA_CONFIG_CACHE_TTL`）。

**所以"token 池默认开启、水位 min=2/max=6"是 Go 版独有，不是从 Python 上游继承的。**

它的设计来源，项目里写得很清楚——**借鉴第三方项目 zcode-api**：

`docs/analysis-zcode-api-reference-20261028.md:556`（能力对照矩阵）：
> `| 验证码 | happy-dom + 令牌池 + CPU governor | rod 浏览器池 + 单 token | **借鉴池设计** |`

`同文档:580`（§5 不建议照搬的点，第 7 条）：
> `7. **不要移植 happy-dom solver**：它是 JS 运行时内的 DOM 模拟；我们的 rod 浏览器池是另一条路线。借"预求解池 + CPU 治理"的设计即可。`

以及 `docs/plan-server-start-plan-optimization-20261028.md:66`（基线差距表）：
> `| 验证码 | 单 token；浏览器 token 不缓存；请求内同步求解；配置 10 分钟… | 无预求解池；无 last-good；无 CPU 治理；高并发首字节慢 | P0-B |`

→ 这三处构成**"token 池是刻意引入、动机是首字节延迟，而非上游抄来"**的完整书面证据链。

---

## 5. 对照矩阵：等价 / 更强 / 偏离

| 维度 | 上游 Python (P) | 我们 Go (G) | 判定 |
|------|-----------------|-------------|------|
| worker 是否常驻 | 是（子进程，`for raw in sys.stdin`） | 是（进程内 goroutine + rod） | **等价** |
| idle 自动退出 | 无 | 无 | **等价** |
| 每 N 次求解重启 / max_tasks | 无 | 无 | **等价** |
| 成功复用页面 / 失败重载 | 是 | 是 | **等价** |
| 超时判死 + 迟到结果不误投 | 是（杀进程） | 是（condemn 槽位 + abandoned 计数） | **等价** |
| 崩溃检测 | 进程 EOF/退出码 | CDP `Browser.getVersion` 3 s 探针（`solve.go:367`） | **更强**（能识别"socket 还连着但不回应"的假死） |
| 池 6 项超时参数 | 见 §4.3 | 同值 | **等价** |
| 失效兜底 | 回退 Node（jsdom）→ 人工 | 直接 503 `captcha_required` → 人工 | **偏离（有意）**：jsdom 已被 F001 判死，不移植（`PLAN.md:911`、`browser_solver.go:71-73`） |
| token 预求解池 | **无**（逐请求求解） | 有：分桶 + TTL 95 s + min2/max6 + 1 s refill + 空池 2 路竞速 | **偏离（新增）** |
| 零流量时的后台求解 | 无 | **有**：空闲 120 s 缩到 1；900 s 后仍维持 1 条滚动 token | **偏离（新增）** ← 见 §7 |
| CPU 自适应 | 无 | governor：≥70% 半速、≥85% 1/4 | **偏离（新增）** |
| 单浏览器内存预算 | 未记录 | 0.5–1 GB / worker（写进 compose/README） | **更强（有预算）** |
| 残留进程兜底 | `proc.kill()` 保证整棵进程树死 | `browser.Close()` + `launcher.Cleanup()`，5 s 看门狗超时**放弃等待**，残留靠 leakless | **偏离（弱化）** ← 见 §6.5 |
| 验证码配置缓存 TTL | 600 000 ms（10 min，`app/settings.py` 的 `CAPTCHA_CONFIG_CACHE_TTL`） | 3 600 000 ms（1 h，`config.go:85`） | **偏离（有意，P0-B 的 last-good 韧性）** |

---

## 6. 偏离点明细（每一处附证据 + 后果）

### 6.1 传输层：子进程 + 行协议 → 进程内 goroutine + rod
- 证据：`internal/captcha/pool.go:1-13`（已在上文逐字引用）；`PLAN.md:91,198-211`。
- 后果（对本次问题的意义）：Chromium 与 Go 主进程同属一棵进程树，**浏览器寿命 ≈ 服务进程寿命**。这解释了"Chromium 从 11-03 跑到今天"——只要服务进程没重启，浏览器就没有任何理由重启。
- 判定：**已声明的等价重写**，不是缺陷。

### 6.2 新增 token 预求解池（背景持续求解）
- 证据：`internal/captcha/token_pool.go:1-11` 文件头：
  > `//   - 后台 refill 在低于目标水位时补货；池空时同步发起 1–2 路 solve 竞速，`
  > `//   - CPU governor 按占用率缩放目标与并发；空闲 120s 缩到 1，900s 停补。`
- 证据：`internal/config/config.go:103-113`（`CaptchaPoolEnabled=true`、TTL 95 s、min 2 / max 6、refill 1 s、emptySolve 2、idleShrink 120 s、idleStop 900 s、CPUGovernor=true 全部默认开启）。
- 后果：**上游只在有请求时求解；我们从进程启动起就按 1 s 节拍检查水位并主动求解**。这是"零流量时浏览器仍有工作"的直接原因。
- 判定：**刻意引入的新增能力**（`docs/handoff-m2-p0b-20261028.md:10-21`、`docs/plan-server-start-plan-optimization-20261028.md:150-191`），动机是首字节延迟（p95 < 200 ms）。

### 6.3 新增 CPU governor
- 证据：`internal/captcha/cpu_governor.go:1-8`（"预求解会在空闲时也持续拉起浏览器求解，若无约束会长期占用 CPU"）+ `:75-89`。
- 注意：**这段注释本身承认了 6.2 的副作用**，是最接近"我们早就知道会占 CPU"的书面记录。但它只降速，不关浏览器。

### 6.4 配置缓存 10 min → 1 h
- 证据：上游 `app/settings.py`：`CAPTCHA_CONFIG_CACHE_TTL = _int("CAPTCHA_CONFIG_CACHE_TTL", 600_000)`；我们 `internal/config/config.go:85` = `3_600_000`。
- 后果：与浏览器常驻无直接关系，但拉长了"配置变化后旧池仍被使用"的时间窗——而旧池的回收只由配置键变化触发（`browser_solver.go:213-226`）。属于需要留意的耦合。

### 6.5 残留进程兜底弱化（**最值得服务器侧核实的一条**）
- 证据：`internal/captcha/pool.go:540-557`：
  > ```
  > // closeWorker 异步关闭 worker；Close 阻塞超过看门狗时限即放弃等待
  > // （残留浏览器进程由 leakless/进程退出兜底）。
  > ```
  > 代码：`select { case <-done: case <-time.After(p.closeWatchdog /* 5s */): web.Warn(... "worker 关闭超时，放弃等待") }`
- 对比上游：`captcha_browser.py` 的 `_kill` 直接 `proc.kill()` 并对整个子进程树生效，**不存在"放弃等待"分支**。
- 后果：若某次替换 worker 时 `browser.Close()` 卡住 >5 s，则**该 Chromium 再也没有任何代码路径去关它**（无 reaper、无登记、无重试）；只有 Go 进程整体退出（含 leakless 触发）才会清掉。
- **注意**：本次现场（容器 Up 9 min、Chromium 3 天）**不能**用这条解释——容器重启会带走整个 PID 命名空间，3 天的 Chromium 必然属于**上一个容器运行期**的进程，即"服务进程活了 3 天"。这条只是"未来可能出现孤儿 Chromium"的**代码级风险**，不是本次现象的解释。**未证实本次有孤儿。**

---

## 7. 关键发现：文档承诺"停止预解"，代码实际"约每 95 秒仍解一次"

这是本次横向对照里**唯一一处真正的"文档 vs 实现"偏离**，且正好落在"零流量时浏览器为什么还在动"这个点上。

### 7.1 文档怎么说

`docker-compose.yml:36`（逐字）：
> `# 空闲 15 分钟后 token 池会缩到 1 条并停止预解，CPU 自动回落。`

`README.md:475-476`（逐字）：
> `> `captcha_pool_size` / `empty_take` / CPU 指標調整；空閒 15 分鐘後 token 池會`
> `> 縮到 1 條並停止預解。`

`docs/handoff-m2-p0b-20261028.md:40`（逐字）：
> `- CPU ≥70% 降半速、≥85% 降到 1/4；空闲 120s 缩到 1 条，900s 停补（保留 1 条唤醒）。`

### 7.2 代码实际怎么跑（`internal/captcha/token_pool.go:649-721` `refillOnce`）

```go
case idle > p.opts.idleStop:   // 900s
    target = 1
    deepIdle = true
...
size := len(p.buckets[p.key])          // 702：**先 pruneLocked 掉了过期 token**（:661）
need := target - size - inflight       // 704
if deepIdle && size >= 1 { need = 0 }  // 705-707
if need > 0 { ... launchMintsLocked(...) }
```

推演（默认参数：TTL 95 s、min 2、idleShrink 120 s、idleStop 900 s、refill 1 s）：

| 时刻 | bucket 内有效 token | target | need | 行为 |
|------|--------------------|--------|------|------|
| 深度空闲刚达成 | 1 | 1 | 0（`deepIdle && size>=1`） | 不求解 ✓ |
| 约 95 s 后（TTL 到期，`:606-625` prune 掉） | **0** | 1 | **1** | **求解一次** |
| 再过约 95 s | 0 | 1 | 1 | **再求解一次** |
| … | | | | **无限循环** |

→ "停补"= 不再补到 min=2，**不等于停止求解**。真实稳态是**"永远保留 1 条滚动 token，因此约每 95 秒必然求解一次，直到进程退出"**。
→ 也就是说：**只要 token 池开着且验证码配置可用，浏览器就永远不会真正空闲**；`idleStop` 只能把"1 秒节拍连续求解"降级为"每 95 秒一次"。

### 7.3 为什么单测没抓到

`internal/captcha/token_pool_test.go:278-307` `TestTokenPoolIdleShrinkAndStop`：

```go
opts.ttl = time.Hour          // :284 ← **把 TTL 设成 1 小时**
opts.idleShrink = 120 * time.Second
opts.idleStop = 900 * time.Second
...
now = now.Add(20 * time.Minute); p.refillOnce(now)
if callsAfter != callsBefore { t.Fatalf("深度空闲不应继续补货：%d -> %d", ...) }   // :304-306
```

测试把 `ttl` 设成 1 小时（远大于 900 s 空闲窗），token 在断言窗口内**不会过期**，所以 `size >= 1` 一直成立、`need = 0`、断言通过。
**换成生产默认 `ttl = 95s`，同一断言会失败。** 这是"契约只在 TTL > idleStop 时成立"的隐含前提，生产参数恰好违反它。

### 7.4 这条会不会解释服务器上的 30% CPU？

- **机制上它是唯一的候选**：`internal/captcha` 里能让浏览器在零流量下动起来的代码路径只有这一条（`git log -- internal/captcha` 23 条提交中，只有 `447d185` 引入了后台求解）。
- **但不能就此定论**：约每 95 秒一次求解，加上 SDK 页面自身定时器，是否足以构成观测到的 30% CPU / 1.5 GB RSS，**仓库内无法证明**。需要服务器侧的 `captcha_mint_total` 指标增速、日志节拍、或是把 `ZCODE_CAPTCHA_POOL_ENABLED=false` 做对照（属 server-forensics 的取证范围）。
- 判定：**机制已证实（代码可读），量级未证实**。

---

## 8. 未证实 / 需服务器侧取证（本文不下结论）

| 项 | 现状 | 需要什么才能定 |
|----|------|----------------|
| 1.5 GB RSS 是"正常长跑水位"还是"泄漏" | 文档预算 0.5–1 GB/worker（`docker-compose.yml:33`），**现场 1.5 GB 已超上限**；但 3 天寿命的基线值未知 | 需要一个刚启动 10 分钟的对照采样（同版本同参数），比较 RSS 斜率 |
| 30% CPU 的构成（背景求解 vs SDK 定时器 vs 渲染器空转） | 代码上只有 6.2 一条背景路径 | `captcha_mint_total` 增量 / 日志中 `token 池` 与 `真实浏览器验证码池已就绪` 的出现节奏 |
| 那个 Chromium 是否 = 当前池的活 worker | 命令行含 `--headless --remote-debugging-port=0`，与 `solve.go:86` 记录的 rod 连接方式一致 → **形态吻合** | `ls /proc/<pid>/task` 父进程是否是 zcode2api 主进程（而非 init/1 的孤儿） |
| 是否存在 6.5 的孤儿浏览器 | 代码有该风险路径 | `ps -ef` 里 Chromium 进程数 vs `ZCODE_CAPTCHA_BROWSER_WORKERS`（现场 env 未设 → 应为 1） |

---

## 9. 对核心问题的回答（任务书口径）

**问：常驻 Chromium 三天 + 1.5 GB RSS + 30% CPU，是设计如此还是设计缺陷/泄漏？**

按本次横向对照能确定的部分：

1. **"常驻浏览器"本身 = 设计，且是从 Python 上游继承的既定设计**，不是 Go 重写引入的形态。
   - 上游原文：`app/captcha_browser.py:638,649-655`（启动一次、`for raw in sys.stdin` 常驻等指令、只有 SHUTDOWN/崩溃才退出）。
   - 我们原文：`internal/captcha/pool.go:1-13` + `internal/captcha/solve.go:240-270`（每槽一个 rod 会话，池只在超时/崩溃/Stop 时替换）。
   - 项目里对"常驻"的**书面确认**：`docker-compose.yml:63`（"每个 worker 是一个常驻 Chromium，CPU/内存按 worker 数线性增长"）、`docker-compose.yml:33`、`README.md:473-474`、`PLAN.md:198-211`。
   - **未发现任何"浏览器常驻导致内存/CPU 问题"的历史踩坑记录**（`docs/` 全目录）。

2. **"三天不重启浏览器"= 设计使然**：两代实现都没有 idle 关闭、没有 max_tasks、没有 max-age。浏览器寿命 = 服务进程寿命。要它重启，只有服务重启或触发超时/崩溃替换。

3. **相对上游，我们有两处新增（偏离）**，都在 `447d185`：
   - 后台 token 预求解池（上游没有）；
   - CPU governor（上游没有）。
   **这两处把"常驻但只在有流量时动"变成了"常驻且恒有周期性工作"。**

4. **一处"文档 vs 实现"偏离（本次新发现，建议单独立项）**：
   `docker-compose.yml:36` / `README.md:475-476` / `docs/handoff-m2-p0b-20261028.md:40` 声称空闲 15 分钟后"**停止预解**"；
   代码实际是"**维持 1 条滚动 token，TTL 95 s ⇒ 约每 95 秒仍求解一次，永不停止**"（`token_pool.go:649-721`）；
   单测因把 `ttl` 设为 1 小时而未能识别（`token_pool_test.go:284`）。

5. **1.5 GB RSS 是否超设计**：按文档预算（0.5–1 GB/worker）**已超**；但"超预算 = 泄漏"这一步**未证实**，需要基线对照（§8）。

**建议（仅供决策，未改任何代码/配置）**：
- 若要"真正的空闲停机"，需要**浏览器池级**的 idle 回收（当前不存在），而不是调 `idleStop`——`idleStop` 只影响 token 水位。
- 若要"验证文档承诺"，最小改动是把 `idleStop` 语义改成"允许 bucket 为空"（即 `deepIdle` 且 `size==0` 时仍不求解），或给 token 池加"深度空闲后不再 mint"的开关；两者都必须同步改 `token_pool_test.go` 的 `ttl`（否则仍测不到）。
- 若要定位内存，先补 §8 的对照采样，再决定是否加 max-age/max-tasks（两代都没有，加了就是**第二个偏离**，需要单独评估风控影响——每次重启浏览器会换 `--fingerprint` 随机种子，见 `solve.go:113`）。

---

## 附录 A：证据复现命令（只读）

```bash
# Python 上游原文（Go 重写提交 d01c740 的父提交）
git show 27f7a3c:app/captcha_browser.py
git show 27f7a3c:app/captcha.py
git show 27f7a3c:app/settings.py

# 常驻形态的出生提交
git show --stat 29af676 && git show 29af676:internal/captcha/pool.go | head -20

# token 池 / idle 语义的出生提交
git show --stat 447d185
git show 447d185:internal/captcha/token_pool.go | sed -n '1,20p'

# 反向检索（证明"没有"回收机制）
git log --oneline -S 'max_tasks'                       # 空
git log --oneline -S 'IdleStop' -- internal PLAN.md PLAN.md README.md   # 仅 447d185
git log --oneline -S 'closeWorker' -- internal         # 仅 29af676
```

## 附录 B：commit 全 hash

| 短 hash | 全 hash | 一句话 |
|---------|---------|--------|
| `39ef045` | `39ef045fe7c45d0c8c7c00c051921022b822b6ca` | 根提交（Python 版起点） |
| `27f7a3c` | — | Go 重写前最后一个 Python 提交（`d01c740^`） |
| `d01c740` | — | `feat: rewrite the service in Go, drop the Python codebase` |
| `29af676` | `29af6765ce1300ad01481719d35bd0fed00af58c` | `feat: add rod browser captcha pool (M5)` |
| `f065240` | `f065240052bf100a732f76c7f75b4e6398721e0a` | 收紧求解超时上界 |
| `ff014ff` | `ff014ff6e48dd04800ebe62bc838daeca8e44fca` | 池 Start/Stop 移出求解器锁 |
| `115c103` | `115c103d086226e0d0b07a1e530bc1e2ef830ad1` | 修复取消请求导致的槽位泄漏 |
| `0aae845` | `0aae845698e33aa4ccab35c2abdf9d57275bc08a` | 给存活探针加 deadline（防假死卡死池） |
| `447d185` | `447d1855aa1bee230f39b6b73671cd5e17315d54` | `feat(captcha): add pre-solved token pool and last-good config` |
| `2fb7e97` | `2fb7e97b600e47745a3db574f8e110ea362fd4d3` | token 竞速改为 waiter 队列 |
