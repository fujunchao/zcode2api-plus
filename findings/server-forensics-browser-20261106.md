# 服务端取证：zcode2api 容器内常驻 Chromium（2026-11-06）

> 取证人：server-forensics（task-2）
> 目标机：`root@154.12.85.101`（SSH 服务器名 `default`），hostname `yg6363567923`，Debian 13 / kernel 6.12.113，4 核 Intel Xeon Gold 6252
> 取证时间：2026-11-06 04:24Z ~ 04:36Z（12:24 ~ 12:36 CST）
> 约束遵守：全程只读。未执行 docker restart/stop/rm、未改 compose/.env、未改任何配置。唯一写操作是在服务器 `/tmp` 落了一个采样脚本（已结束）。所有数字均来自命令输出，取不到的标注「不可得 / 未证实」。

---

## 0. 结论速览

| 问题 | 结论 | 证据强度 |
|---|---|---|
| 04:12 这次是 restart / recreate / compose up？ | **手动 stop+start（等价 `docker restart`）**。容器未重建、镜像未变、无重启策略介入 | 强（3 条独立证据） |
| 11-03 起的旧日志还在不在？ | **完整存在，一行没丢**（json-file，未轮转，25886 行 / 7.7MB） | 强 |
| 现在（重启后 20 分钟）有没有拉起浏览器？ | **没有**。零 chrome 进程、容器 pids=12、memory.peak 仅 153MiB | 强（5 条独立证据） |
| 上次那个浏览器的真实寿命 | **2026-11-03 03:27:54Z → 2026-11-06 04:12:29Z，共 72h45m，一次性拉起、3 天从未回收** | 强（profile 时间戳 + systemd cgroup 账本） |
| 1.5GB RSS / 30% CPU 是设计还是泄漏？ | **「常驻」是设计，「线性涨到 1.5GB + CPU 涨 9 倍」是泄漏**。内存 174M→1509M 单调线性增长，无平台期 | 强（1Panel 每 5 分钟采样，72 小时曲线） |
| 再加一个浏览器会不会 OOM？ | **不会**。容器无内存上限，宿主 MemAvailable ≈1.8GiB，历史 3 天从未 OOM-kill | 强 |
| 109% CPU 里浏览器占多少？ | 观测时刻约 **31% 是浏览器主渲染进程**，**43% 是 zcode2api 自身**，其余是 chrome 子进程 | 中强 |

**最关键的一句**：这个容器里的 Chromium **不是进程启动就拉起的常驻进程**，而是**第一次真正需要验证码配置时懒启动**，然后**再也没有被关掉**。3 天里它既没有被 idle 回收，也没有 max-age / RSS 上限回收，于是渲染进程内存线性涨到 1.5GiB、CPU 从 3.4% 涨到 30.7%。

---

## 1. 容器历史：04:12 那次到底是什么

### 1.1 docker inspect 关键字段

```
$ docker inspect zcode2api --format '...'
ID=f23a35acfcd49fcbd59163aa3d49a3b5cdd70b06fe3099dd45ae8b5b63d7574a
Image=ghcr.io/fujunchao/zcode2api-plus:v2.11.12-go
ImageID=sha256:bba0cee423f2ae107cba022c499850ccc5c7c0e638900b321240295987b0d342
Created=2026-11-03T03:27:19.163757123Z      <-- 11-03 11:27 CST，没被重建
StartedAt=2026-11-06T04:12:40.942608465Z   <-- 11-06 12:12:40 CST
FinishedAt=2026-11-06T04:12:40.177281235Z  <-- 仅早 0.77 秒
RestartCount=0                             <-- 不是重启策略拉起来的
OOMKilled=false
ExitCode=0
RestartPolicy=unless-stopped:0
Memory=0  MemorySwap=0                     <-- 无 cgroup 内存上限
LogDriver=json-file  LogOpts=map[]
Pid=314022
```

三条独立证据合起来排除了另外两种可能：

1. **不是 recreate**：`Created` 仍是 11-03T03:27:19Z，`ImageID` 未变。若是 `docker compose up` 重建，Created 会变成 11-06。
2. **不是崩溃/自动重启**：`RestartCount=0`（Docker 只在重启策略自动拉起时递增该计数）、`OOMKilled=false`、`ExitCode=0`。
3. **是「先 stop 再 start」**：`FinishedAt` 比 `StartedAt` 早 0.77 秒，是 `docker restart` 的典型语义。

### 1.2 dockerd / systemd 侧旁证（journalctl，12:10–12:15 CST 窗口）

```
2026-11-06T12:12:29+08:00 dockerd: level=info  msg="stopping restart-manager" container=f23a35...
2026-11-06T12:12:39+08:00 dockerd: level=info  msg="Container failed to exit within 10s of signal 15 - using the force" container=f23a35...
2026-11-06T12:12:40+08:00 systemd: docker-f23a35....scope: Deactivated successfully.
2026-11-06T12:12:40+08:00 systemd: docker-f23a35....scope: Consumed 2d 6h 39min 37.394s CPU time,
                          1.9G memory peak, 322.9M memory swap peak, 4.1G read from disk, 17.2G written to disk.
2026-11-06T12:12:41+08:00 systemd: Started docker-f23a35....scope
```

**这一行 systemd 账本是本次取证最有价值的单条数据**：它给出了旧运行（3 天）的 cgroup 累计账：

- CPU 时间 **2d 6h 39m 37.394s = 55.66 CPU·小时**
- 内存峰值 **1.9G**（cgroup 口径，含 page cache；单进程 RSS 峰值见 §6，1.476GiB）
- swap 峰值 **322.9M**
- 3 天写盘 **17.2G**、读盘 **4.1G**

换算：旧运行墙钟 11-03T03:27:27Z → 11-06T04:12:29Z = **72.75 小时**，CPU 55.66/72.75 = **平均占用 0.765 个核**。

「signal 15 → 10 秒未退出 → 强制」说明用的是 `docker restart` 默认 10s 停止超时，而应用自己的优雅停机预算先耗尽了（见 1.4）。

### 1.3 旧日志是否还在：在，完整

```
$ ls -la /var/lib/docker/containers/f23a35.../
-rw-r----- 1 root root 7968059 Nov  6 12:24 f23a35...cdd70b06...-json.log
$ wc -l ...-json.log
25886 ...-json.log          # 无 .1 .2 轮转文件，单文件到底
$ head -c 200 ...-json.log
{"log":"  [+] captcha token 池已启用：ttl=1m35s min=2 max=6 empty_solve=2 refill=1s",...,"time":"2026-11-03T03:27:27.047537798Z"}
```

首行时间戳 **2026-11-03T03:27:27.047Z**，与容器创建时间吻合；末行实时增长。**旧运行的日志一行都没丢**，无需靠旁证推断。所以「旧日志不可恢复」这一分支不成立。

### 1.4 旧运行是怎么结束的（不是崩溃）

```
2026-11-06T04:12:29.363Z  [+] main 收到退出信号，开始优雅停机…
2026-11-06T04:12:39.366Z  [~] main 后台清理超过剩余停机预算，停止等待
2026-11-06T04:12:40.177Z  <容器 FinishedAt>
```

日志里「服务运行中」启动横幅只出现 **2 次**：

| 行号 | 时间 | 含义 |
|---|---|---|
| 9 | 2026-11-03T03:27:27.061Z | 旧运行启动 |
| 25775 | 2026-11-06T04:12:50.554Z | 新运行启动 |

即整个容器生命周期内进程只启动了 2 次，**11-03 → 11-06 之间没有任何中间重启**。旧运行占日志第 1~25774 行，新运行第 25775 行起。

> 旁证：`/opt/1panel/apps/local/zcode2api/data/` 下有 `accounts.db.bak_20261103_pre21112`（Nov 3 11:25），说明 11-03 11:25 刚做过 2.11.12 升级备份，2 分钟后（11:27）创建了当前容器，时间线自洽。

### 1.5 无 OOM 记录

```
$ journalctl -k | grep -icE 'oom|out of memory'
0
$ cat /sys/fs/cgroup/...zcode2api.scope/memory.events
low 0 / high 0 / max 0 / oom 0 / oom_kill 0 / oom_group_kill 0
```

内核从未 OOM-kill 过任何进程。**「浏览器把内存吃爆导致被杀」不成立**——它是被人工 restart 杀掉的。

---

## 2. 现在（重启后 20 分钟）到底有没有拉起浏览器

### 2.1 结论：没有。五条独立证据

**证据 A：宿主上零 chrome 进程**（按 `/proc/<pid>/exe` 真实可执行文件判定，避免 shell 自匹配）

```
$ for p in /proc/[0-9]*; do exe=$(readlink $p/exe); case "$(basename "$exe")" in chrome|chromium|headless_shell|chrome_crashpad_handler) echo "$p $exe";; esac; done
（唯一命中是无关的 node：/usr/local/bin/node /opt/app/sub-store.bundle.js）
chrome-ish procs = 0
```

**证据 B：容器进程数只有 12**（Chromium 一启动至少 20~40 个 pid）

```
$ docker top zcode2api
10001  314022  313995  0  12:12  ?  00:00:00  /sbin/docker-init -- /usr/local/bin/zcode2api serve
10001  314076  314022  48 12:12  ?  00:06:00  /usr/local/bin/zcode2api serve
$ cat ...zcode2api.scope/pids.current  -> 12
$ cat ...zcode2api.scope/pids.max      -> 4637
```

**证据 C：cgroup memory.peak 只有 153MiB**（重启后至今的历史峰值；一个 Chromium 起步就 150MB+，稳态 400MB~1.5GB）

```
$ cat ...zcode2api.scope/memory.peak     -> 160612352  (153.2 MiB)
$ cat ...zcode2api.scope/memory.current  -> 133533696  (127.3 MiB)
$ cat ...zcode2api.scope/memory.max      -> max
$ cat ...zcode2api.scope/memory.swap.current -> 0
```

**证据 D：日志里新运行没有一行 captcha**

```
旧运行（1~25774 行）：captcha_browser 池已启动：1 个 worker 全部就绪   @2026-11-03T03:27:54.975Z
                      captcha 真实浏览器验证码池已就绪（1 个 worker）   @2026-11-03T03:27:54.979Z
新运行（25775 行起，183 行）：含 "captcha" 的行数 = 0
```

**证据 E：容器内浏览器 profile 目录时间戳停在 11-03，没有重建**

容器可写层在 restart 后保留，所以 /tmp 残留是最硬的证据：

```
$ docker exec zcode2api ls -la /tmp
drwxrwxrwt 1 root  root  108 Nov  6 04:11 .                <-- /tmp 被 docker 重建于 04:11
drwxr-xr-x 2 zcode zcode  22 Nov  3 03:27 leakless-amd64-adb80298fa6a3af7ced8b1c9b5f18007
drwx------ 2 zcode zcode  52 Nov  3 03:27 org.chromium.Chromium.hJZsVh
drwx------ 3 zcode zcode  23 Nov  3 03:27 rod
```

三个目录 mtime 全是 **Nov 3 03:27**，重启后 Chromium 根本没被再拉起过（否则会生成新的 `/tmp/rod` 与新的 `org.chromium.Chromium.*` 目录）。

### 2.2 12 分钟采样表（2026-11-06 04:26:21Z ~ 04:34:15Z，14 个采样点）

口径：每 30s 一次；chrome 计数按 `/proc/<pid>/exe` 基名匹配；`zc_*` 读 cgroup；PSI 读 `/proc/pressure/cpu`。

| 时刻(UTC) | chrome数 | chrome RSS | zc_pids | zc内存(MB) | zc内存峰值(MB) | zc CPU累计(s) | 可用内存(MB) | swap已用(MB) | PSI cpu some avg10 | load1 |
|---|---|---|---|---|---|---|---|---|---|---|
| 04:26:21 | 0 | 0 | 12 | 139.2 | 153.2 | 420.5 | 1908 | 2144 | 33.52 | 6.42 |
| 04:26:58 | 0 | 0 | 12 | 118.7 | 153.2 | 435.1 | 1939 | 2144 | 31.98 | 6.11 |
| 04:27:34 | 0 | 0 | 12 | 130.4 | 153.2 | 455.6 | 1875 | 2143 | 52.52 | 8.01 |
| 04:28:12 | 0 | 0 | 12 | 141.2 | 153.2 | 478.7 | 1876 | 2143 | 60.44 | 8.39 |
| 04:28:48 | 0 | 0 | 12 | 134.5 | 153.2 | 499.2 | 1807 | 2143 | 46.89 | 7.79 |
| 04:29:23 | 0 | 0 | 12 | 115.1 | 153.2 | 501.5 | 1887 | 2143 | 3.37 | 4.73 |
| 04:29:54 | 0 | 0 | 12 | 121.4 | 153.2 | 519.3 | 1852 | 2141 | 64.19 | 8.07 |
| 04:30:36 | 0 | 0 | 14 | 123.0 | 153.2 | 542.4 | 1857 | 2140 | 51.71 | 9.16 |
| 04:31:12 | 0 | 0 | 12 | 123.5 | 153.2 | 563.5 | 1888 | 2140 | 22.93 | 7.84 |
| 04:31:45 | 0 | 0 | 12 | 133.5 | 153.2 | 573.2 | 1880 | 2140 | 22.33 | 5.50 |
| 04:32:22 | 0 | 0 | 12 | 140.9 | 153.2 | 594.1 | 1874 | 2138 | 34.43 | 5.97 |
| 04:32:58 | 0 | 0 | 12 | 147.5 | 153.2 | 622.6 | 1859 | 2138 | 43.06 | 5.95 |
| 04:33:38 | 0 | 0 | 12 | 129.1 | 153.2 | 645.3 | 1841 | 2137 | 31.85 | 6.10 |
| 04:34:15 | 0 | 0 | 12 | 134.3 | 153.2 | 667.0 | 1822 | 2136 | 53.66 | 7.75 |

- chrome 计数**整段为 0**，RSS 与 CPU tick 恒为 0。
- zcode2api 容器 CPU：`(667.0-420.5) / 474s = 52.0%` —— **平均占 0.52 个核，而这 0.52 核里没有浏览器**。
- 同期 `docker stats` 两次快照：12:26 CST `cpu=48.20% mem=111.6MiB`；12:31 CST `cpu=156.87% mem=107.3MiB`。**同一进程 5 分钟内 48% → 157%**，说明瞬时采样在这台机器上极不可靠（见 §5.3）。

### 2.3 管理接口自证：captcha 池未激活、请求数为 0

`GET /admin/api/monitor`（只读，密钥不回显）：

```json
"captcha": {
  "enabled": false,          <-- 池未进入可服务状态
  "pool_size": 0, "take_total": 0, "empty_take_total": 0,
  "mint_total": 0, "mint_fail_total": 0, "inflight": 0,
  "target": 2, "idle_seconds": 1316.8, "cpu_percent": 70.72
},
"requests": { "scope": "process", "total": 0, "succeeded": 0, "failed": 0, "active": 0, "upstream_attempts": 0 },
"system": { "cpu_count": 4, "load_1m": 7.99, "memory": {"available_mb":1822.5,"total_mb":3921.3,"used_percent":53.5} }
```

`requests.total = 0` —— **重启后 20 分钟内，网关一条真实请求都没收到**。

> **顺带发现一个监控盲点**：`services[2]` 报 `{"name":"驗證瀏覽器","status":"online","detail":"Chromium"}`，但此时宿主上根本没有 Chromium 进程。原因在 `internal/adminapi/monitor.go:26-29`：状态只由配置开关 `config.CaptchaBrowserEnabled` 决定，与实际是否有 worker 在线无关。**控制台显示「浏览器在线」不能作为浏览器存活的证据。**

### 2.4 为什么这次没拉起：一条可验证的因果链

服务端观察到的事实：

1. 旧运行：池启用（03:27:27）→ 19 秒后打出 `验证码配置变化，已清空 token 池（scene=11xygtvd region=sgp prefix=no8xfe）`（03:27:47）→ 再 7 秒后 `captcha_browser 池已启动`（03:27:54）。
2. 新运行：183 行日志里 **0 行 captcha**，连 `验证码配置变化` 都没有。

代码侧对应关系（只读核对，细节留给 task-1）：

- `internal/captcha/captcha.go:101-102` 注释明写：「池开启后仍需 SetConfig（由 FetchConfig 成功路径完成）才进入可服务状态」。
- `pool.SetConfig` 的唯一非测试调用点是 `captcha.go:210`；`FetchConfig` 只被 `getVerifyParam`（`captcha.go:296/311`，取验证码时）与两个管理端点（`adminapi/accounts.go:590/610`）调用，**没有启动预热**。
- 因此：新运行至今 0 请求 → 没走过 `getVerifyParam` → `FetchConfig` 未执行 → 池 `enabled=false`（已被 admin API 证实）→ 不 mint → 浏览器不启动。

**推论（重要，但不是服务端可直接证明）**：浏览器会在**第一条真正需要验证码的请求**（或 23:23 的 claim 定时任务 / 手工领取）到达后数秒内重新拉起，并再次进入「永不回收」的长跑。此条属代码路径推断，**服务端未证实**，需 task-1 静态确认。

### 2.5 /data 与 /data/cloakbrowser 残留

挂载：`bind | /opt/1panel/apps/local/zcode2api/data -> /data | rw=true`

```
$ du -sh /opt/1panel/apps/local/zcode2api/data/cloakbrowser
697M
$ find .../cloakbrowser -type d
.../cloakbrowser
.../cloakbrowser/chromium-146.0.7680.177.5
.../cloakbrowser/chromium-146.0.7680.177.5/locales        # 总共只有 3 个目录
$ docker exec zcode2api ls -la /data/cloakbrowser/chromium-146.0.7680.177.5/
-rwxr-xr-x 465371408 Oct 23 12:08 chrome        <-- 465MB Chromium 二进制，10-23 下载后再未变
-rwxr-xr-x    722392 Oct 23 12:08 chrome_crashpad_handler
-rwxr-xr-x  29125448 Oct 23 12:08 chromedriver
```

**`/data/cloakbrowser` 下没有任何 user-data-dir 残留，只有一份浏览器二进制。** user-data-dir 全部落在容器可写层的 `/tmp/rod/user-data/`：

```
$ docker exec zcode2api ls -la /tmp/rod/user-data
drwx------ 10 zcode zcode 4096 Nov  6 03:57 f0923dfa11aefbaa     <-- 只有 1 个，du 107M，369 个文件
$ docker exec zcode2api ls -la /tmp/rod/user-data/f0923dfa11aefbaa
lrwxrwxrwx SingletonLock -> f23a35acfcd4-30      <-- 容器 hostname + pid 30，启动即 11-03
-rw-r--r--            60 Nov  3 03:27 DevToolsActivePort
-rw-------       4194304 Nov  3 03:29 BrowserMetrics-spare.pma
drwx------            30 Nov  6 04:07 Default
```

**残留目录数量 = 1**，直接说明：这个容器整个生命周期里**只拉起过 1 代浏览器**，没有反复重启造成的目录堆积；同时也说明**停机时没有清理 profile**（107MB 脏目录留着，因为关机是 SIGKILL 强杀，清理没机会跑）。

---

## 3. 资源账本：会不会 OOM

### 3.1 宿主内存实数（04:34Z / 12:34 CST）

```
$ free -m
               total        used        free      shared  buff/cache   available
Mem:            3921        2065         529          14        1624        1855
Swap:           4095        2136        1959
$ grep -E 'MemTotal|MemAvailable|SwapTotal|SwapFree|Committed_AS' /proc/meminfo
MemTotal:        4015408 kB     (3.83 GiB)
MemAvailable:    1841268 kB     (1.76 GiB)
SwapTotal:       4194300 kB     (4.00 GiB)
SwapFree:        1999160 kB
Committed_AS:    8631608 kB     (8.23 GiB，已超物理内存 2.1 倍)
```

**「850MB 余量」与实测不符**：真实 `MemAvailable` 在 **1.76~1.95 GiB** 之间波动（采样区间 1807~1939 MB）。`free` 列的 529MB 是「完全空闲页」，不含可回收 page cache，不能当余量用。

**但 swap 已经吃紧**：4GiB swap 用了 **2.09 GiB**。按 `/proc/<pid>/status` 的 VmSwap 排序：

```
 1213796 KB  python    (pid 2333, proxy-checker 容器 "python server.py")   <-- 1.16 GiB
  320996 KB  hermes    (pid 777, 宿主侧 hermes agent)
  206888 KB  uvicorn   (pid 3019, chatgpt2api)
  102212 KB  beam.smp  (pid 2281, emqx)
   84860 KB  node      (pid 2270, sub-store)
```

**swap 主要不是浏览器造成的**：zcode2api 容器当前 `memory.swap.current = 0`，旧运行 systemd 记录的 swap 峰值也只有 322.9M。换页压力主要由 proxy-checker 的 python 进程（1.16 GiB swap）驱动。

### 3.2 cgroup 上限

```
HostConfig.Memory=0  MemorySwap=0  MemoryReservation=0  OomKillDisable=<nil>
cgroup: memory.max = max ,  memory.swap.max = max ,  pids.max = 4637 ,  pids.current = 12
```

**容器没有任何内存上限** ⇒ 容器级 OOM-kill 在原理上不可能发生；只有宿主级 OOM 才会杀进程，而 §1.5 证明 3 天里从未发生。

### 3.3 「再加一个浏览器就 OOM」是否成立：不成立

| 时点 | 浏览器渲染进程 RSS | 宿主状态 |
|---|---|---|
| 刚拉起（11-03 11 时均） | 174 MB | 可用 ~1.9GiB，无压力 |
| 第 1 天（11-04 11） | 573 MB | — |
| 第 2 天（11-05 11） | 936 MB | — |
| 第 3 天末（11-06 12:12） | **1508.6 MB** | 宿主 mem used 87.3%，**仍未 OOM** |

- **重新拉起那一刻**只多占 150~250MB，离 OOM 很远。
- **跑到稳态（约 3 天）**会吃掉 ~1.45GiB，把 MemAvailable 从 1.8GiB 压到 ~0.4GiB；但历史上它就是这样跑满 3 天且没有任何 OOM-kill。
- 真正被牺牲的是 **swap 与 CPU 时间片**：swap 已 2.09GiB 在用；PSI `cpu some avg10` 在采样期多次冲到 40%~64%（即有任务因等 CPU 而停顿的时间占比最高达六成）。

> 精确表述：**「再加一个浏览器就会 OOM」= 未证实，且现有证据倾向于否定。** 「再加一个浏览器会让这台 4 核 3.8G 机器进一步陷入 CPU 争抢、并可能把 swap 推向 2.5GiB+」= 有证据支持。

---

## 4. 触发频率：验证码到底多久被真实需要一次

### 4.1 真实请求里的 captcha 字段：123 条，全是 0

```
全日志含 captcha 的行 = 129 条，其中：
  123 条是请求完成行里的字段  captcha_wait_ms / captcha_pool_size / captcha_empty_take / captcha_mint_fail
    2 条是启动横幅          "captcha token 池已启用"
    1 条是配置变更          "captcha 验证码配置变化，已清空 token 池（scene=11xygtvd region=sgp prefix=no8xfe）"
    1 条是池空闲缩容        "captcha token 池空闲缩容至 1"
```

**123 条请求的 captcha 字段无一例外全为 0**：

```
2026-11-03T03:27:52Z {"captcha_wait_ms":"0","captcha_pool_size":"0","captcha_empty_take":"0","captcha_mint_fail":"0"}
...
2026-11-06T02:08:05Z {"captcha_wait_ms":"0","captcha_pool_size":"0","captcha_empty_take":"0","captcha_mint_fail":"0"}
```

`captcha_wait_ms=0` 说明**这 123 条请求没有一条等过验证码**，即**从未真正消费过一个 token**（`skip_model_request=true` 或该路径不需要验证码）；`empty_take=0` 说明也没出现「池空被迫等待」。

请求频率（旧运行全程，逐行 json 解析统计，避开 ANSI 干扰）：

```
旧运行: 行数=25774  ingress_start=123  请求完成=123  attempt=533
        首请求=2026-11-03T03:27:52.632Z   末请求=2026-11-06T02:08:05.827Z
新运行: 行数=183    ingress_start=0    请求完成=0    attempt=0
```

- 3 天 **123 条请求**，平均 **1.7 条/小时**，且高度聚集（11-06 01:00–02:08 一小时内占大头，之后到停机 2 小时零请求）。
- 最后一条真实请求 **2026-11-06T02:08:05Z**；容器停机 **04:12:29Z**。
- **真实业务需要验证码的频率 ≈ 0，3 天里 0 次 token 消费。**

### 4.2 token 池空闲日志：只有 1 条，且暴露一个日志盲点

```
$ grep -E '缩容|补货' ...-json.log
  [+] captcha token 池空闲缩容至 1      @2026-11-03T03:29:48.048386363Z     <-- 全文仅此 1 条
（"恢复补货" 0 条）
```

按 `internal/captcha/token_pool.go:670-689` 的语义：

- `idle > idleShrink(120s)` → 打「空闲缩容至 1」，置 `p.idleLog=true`
- `idle > idleStop(900s)` → 打「空闲停补（保留 1 条 token 唤醒）」，**但被同一个 `p.idleLog` 锁挡住**

所以：**「空闲停补」这条日志一旦前面打过「空闲缩容」就永远打不出来**（日志缺陷，归 task-1）。这解释了为什么 3 天里只有 1 条空闲日志——**不能据此认为池一直没进入深空闲**。

服务端能确证的只有：**启动 2 分 21 秒后池就进入空闲状态并缩容到 1 条，此后 3 天再没有「恢复补货」迹象。**

### 4.3 空闲期是否仍在驱动浏览器：有直接证据，但无法精确到 mint 次数

**硬证据**是浏览器 profile 的最后写入时间：

```
$ docker exec zcode2api find /tmp/rod -type f -printf '%TY-%Tm-%Td %TH:%TM:%TS %s %p' | sort -r | head
2026-11-06 04:11:48.93   4920  .../Default/Code Cache/js/index-dir/the-real-index
2026-11-06 04:11:28.91    227  .../Default/Code Cache/js/8c8e8d533a6434b0_0
2026-11-06 04:10:48.63 270336  .../GraphiteDawnCache/data_1
2026-11-06 04:10:48.62 270336  .../GrShaderCache/data_1
2026-11-06 04:09:23.09    227  .../Default/Code Cache/js/d2f9360cc7706457_0
2026-11-06 04:07:45.57     22  .../Default/TransportSecurity
2026-11-06 04:07:24.17    227  .../Default/Code Cache/js/f0c9fbf838b935b8_0
2026-11-06 04:05:21.13    267  .../Default/Code Cache/js/6f6bac084d2ae62a_0
2026-11-06 04:01:32.21    227  .../Default/Code Cache/js/e28e7dac8e1e649e_0
```

最后一条真实请求是 02:08:05，但浏览器在 **04:01、04:05、04:07、04:09、04:10、04:11** 仍在写 `Code Cache/js` 与 `TransportSecurity` —— 这些是**页面级导航/JS 执行**才会产生的写入，不是 Chromium 后台维护任务。**最后写入 04:11:48，距 SIGTERM（04:12:29）仅 41 秒。**

按小时统计 `Code Cache/js/*_0` 文件数（页面活动代理指标）：

```
11-03: 3 2 5 - 1 3 1 1 5 4 5 3 2 4 4 4 2 1 2 2
11-04: 4 4 2 1 - 3 2 1 2 2 3 2 1 1 2 2 3 1 1 2 1 1 2 3
11-05: 1 2 1 2 3 3 - 4 1 2 2 2 3 3 2 1 4 2 3 - 1 2
11-06: 2 8 12 24 6                                  <-- 最后 4 小时明显加速
```

零请求的小时里也有 1~5 次写入，末段（01:00–04:00）升到 8/12/24/6。

> **诚实边界**：代码里没有「一次 mint 打一行日志」，因此**从服务端无法把 mint 频率精确到次数/秒**。能确证的只有：① 无请求时段浏览器仍在被持续驱动；② 这个驱动只可能来自池自身补货（因为没有任何外部请求）。是否即「每 95s 一次 mint（TTL 到期补 1 条）」需代码侧确认，**服务端未证实**。

---

## 5. 这台机器上还有谁在抢 CPU

### 5.1 同期容器快照（docker stats --no-stream）

```
12:26 CST
workbuddy2api    cpu=191.23%  mem=38.75MiB
new-api          cpu= 65.75%  mem=120.6MiB
zcode2api        cpu= 48.20%  mem=111.6MiB
proxy-checker    cpu= 39.61%  mem=406.8MiB   (net=10.4GB/10.6GB, blk=3.69GB/132GB)
1Panel-openresty cpu= 10.81%  mem=16.23MiB
1Panel-emqx      cpu=  6.34%  mem=134.8MiB
chatgpt2api      cpu=  2.86%  mem=131.3MiB
合计约 3.7 个核（机器只有 4 核）

12:31 CST（同一批容器，5 分钟后）
zcode2api        cpu=156.87%  mem=107.3MiB     <-- 无浏览器，仍是 157%
workbuddy2api    cpu= 24.71%
proxy-checker    cpu= 14.08%
```

**这组数字非常重要**：zcode2api **在没有浏览器的状态下**能从 48% 跳到 157%。它自己的 proxy 健康巡检（`proxy-deep` / `proxy-health` 每轮几十条出站探测，日志里刷屏）本身就是重 CPU 事项。因此「109% CPU」不能简单归因给浏览器。

### 5.2 逐进程 CPU 历史（1Panel 5 分钟采样，单位＝1 核＝100%，标定见 5.3）

| 小时 | chrome | zcode2api | python(proxy-checker) | beam.smp(emqx) | 宿主总CPU |
|---|---|---|---|---|---|
| 11-03 11 | 14.1% | 22.2% | 25.3% | 0.0% | 42.6% |
| 11-03 12 | 3.4% | 38.3% | 26.7% | 46.0% | 56.7% |
| 11-04 00 | 5.0% | 35.7% | 30.8% | 50.5% | 59.6% |
| 11-04 12 | 8.8% | 37.3% | 28.7% | 0.0% | 55.2% |
| 11-05 00 | 13.8% | 37.0% | 24.4% | 0.0% | 41.9% |
| 11-05 12 | 18.7% | 41.8% | 26.1% | 0.0% | 68.3% |
| 11-06 00 | 26.6% | 43.3% | 28.5% | 55.3% | 60.3% |
| 11-06 12 | 30.7% | 46.3% | 23.4% | 59.1% | 95.9% |

chrome 全程均值 **14.83%**（中位 13.8%，最大 56.9%），zcode2api 主进程全程均值 **39.26%**。

### 5.3 CPU% 单位标定（避免误读）

1Panel `top_cpu` 里 zcode2api 记 47.38%~51.64%，同时 `docker stats` 记 48.20% —— 两者同量纲，即 **1Panel 的进程 CPU% 是「占 1 个核的百分比」**（100% = 1 核，上限 400%）。而 `top_mem` 的 percent 是「占物理内存百分比」（145.3MB → 3.70% × 3921MB ✓）。这决定了下面 109% 的拆解口径。

### 5.4 109% 的拆解（停机前最后一个采样 2026-11-06 12:12:10 CST，距 SIGTERM 19 秒）

```
样本 2026-11-06 12:12:10   宿主cpu=93.6%  宿主mem=87.3%
 top_cpu:
   zcode2api   42.99%   /usr/local/bin/zcode2api serve
   chrome      30.75%   /data/cloakbrowser/chromium-146.0.7680.177.5/chrome --type=renderer ...
   python      26.97%   python server.py
   wb2api       4.88%   /app/wb2api -config /app/config.json
   uvicorn      2.34%   /app/.venv/bin/python -m uvicorn main:app
 top_mem:
   chrome     mem= 1508.6MB  38.47%   chrome --type=renderer ...
   python     mem=  393.0MB  10.02%
   hermes     mem=  157.2MB   4.01%
   zcode2api  mem=  145.3MB   3.70%
   beam.smp   mem=  117.9MB   3.01%
```

- 用户观测的 **1.379 GiB** 与此处 **1508.6MB（1.473 GiB）** 同量级，是同一个 `--type=renderer` 进程在最后 2 小时 1.30~1.51GiB 间波动时的取值。
- 用户观测的 **109% CPU** ≈ 1.09 核 ≈ chrome 主渲染 0.31 核 + zcode2api 0.43 核 + chrome 的 gpu/zygote/utility/crashpad 等未进 top5 的子进程 ≈ 0.35 核。
- 用户观测的「单进程 RSS 1.5GB、30% CPU」与 12:12:10 采样**逐项吻合**，可判定用户看到的就是这个 renderer 进程。
- 该 renderer 完整命令行（与用户看到的 `--headless --remote-debugging-port=0` 一致）：

```
/data/cloakbrowser/chromium-146.0.7680.177.5/chrome --type=renderer
  --crashpad-handler-pid=32 --enable-crash-reporter=,
  --noerrdialogs --user-data-dir=/tmp/rod/user-data/f0923dfa11aefbaa
  --change-stack-guard-on-fork=enable --no-sandbox --disable-dev-shm-usage
  --disable-back-forward-cache --disable-background-timer-throttling
  --disable-breakpad --fingerprint=34276 --fingerprint-platform=windows
  --force-color-profile=srgb --remote-debugging-port=0 --allow-pre-commit-input
  --blink-settings=primaryHoverType=2,availableHoverTypes=2,primaryPointerType=4,availablePointerTypes=4
  --ozone-platform=headless
```

（`--user-data-dir=/tmp/rod/user-data/f0923dfa11aefbaa` 与 §2.5 里残留的唯一那个目录同名，闭环。）

### 5.5 load average 与 PSI

```
$ uptime                  -> load average: 8.05, 5.40, 7.08      (04:24Z)
$ cat /proc/pressure/cpu  -> some avg10=60.44 avg60=41.87 avg300=29.97 total=36197576286
                             full avg10=0.00 avg60=0.00 avg300=0.00 total=0
$ cat /proc/pressure/memory -> some avg10=0.00 avg60=0.00 avg300=0.23   (几乎无内存压力停顿)
$ cat /proc/pressure/io     -> some avg10=0.58 avg60=0.44 avg300=1.31
```

1Panel 记录的历史 load1（停机前最紧张的一段）：

```
11:32 6.18  11:37 6.30  11:42 9.45  11:47 5.27  11:52 7.50
11:57 6.23  12:02 15.58 12:07 16.34 12:12 10.72  (停机)
12:17 4.40  12:22 2.74  12:27 6.11   (重启后回落)
```

- **7 天宿主级 CPU 历史**（1Panel `monitor.db`，5 分钟一点，共 2018 点，10-30 12:28 → 11-06 12:27）：CPU 长期 40~80%，11-05 起抬到 60~80%，11-06 11:00–12:00 达 79~92%。
- **PSI memory `some avg300 = 0.23`，几乎为零** —— 再次说明这台机器当前瓶颈是 **CPU 争抢，不是内存**，进一步削弱「再加浏览器就 OOM」的说法。
- `sar` 不可得：`which sar sadf` 无输出，未安装 sysstat；宿主 load 历史改用 1Panel `monitor.db`（仅 7 天窗口）替代。

---

## 6. 泄漏 vs 设计：1Panel 全量进程采样给出的曲线

1Panel 的 `monitor_bases.top_mem`/`top_cpu` 保存每 5 分钟 top-N 进程的完整命令行与 RSS。把 7 天里所有 `/cloakbrowser/` 进程按 `--user-data-dir`（= 一代浏览器）归并：

| user-data-dir | 首次出现 | 最后出现 | 存活时长 | 采样命中 | 单进程内存峰值 |
|---|---|---|---|---|---|
| fd8fe30585b581b7 | 10-30 12:28 | 10-31 21:08 | 32.7 h | 673 | 0.582 GiB |
| e439d971f76a8967 | 10-31 21:53 | 10-31 21:58 | 0.1 h | 8 | 0.177 GiB |
| f2cf12a3bd103b19 | 10-31 22:18 | 11-01 10:28 | 12.2 h | 360 | 0.417 GiB |
| 0b2f66a0c1bb5eef | 11-01 16:03 | 11-01 17:23 | 1.3 h | 42 | 0.236 GiB |
| 4887803ea7da4f0c | 11-01 17:33 | 11-01 18:13 | 0.7 h | 22 | 0.196 GiB |
| e36546dc8713c200 | 11-01 18:18 | 11-03 11:23 | 41.1 h | 969 | 0.918 GiB |
| **f0923dfa11aefbaa** | **11-03 11:28** | **11-06 12:12** | **72.7 h（3.03 天）** | **1838** | **1.476 GiB** |

存活越久峰值越高：**32.7h→0.58GiB，41.1h→0.92GiB，72.7h→1.48GiB**。

最后一代逐小时曲线（RSS 为该 renderer 均值/峰值，CPU 为 1Panel 记录的该进程 %）：

| 时间 | RSS 均值 | RSS 峰值 | 该进程 CPU |
|---|---|---|---|
| 11-03 11 | 174 M | 182 M | 15.2%（启动） |
| 11-03 12 | 189 M | 222 M | 3.4% |
| 11-03 20 | 365 M | 395 M | 4.2% |
| 11-04 00 | 408 M | 443 M | 5.0% |
| 11-04 12 | 582 M | 589 M | 8.8% |
| 11-05 00 | 762 M | 878 M | 13.8% |
| 11-05 12 | 959 M | 1109 M | 18.7% |
| 11-06 00 | 1119 M | 1304 M | 26.6% |
| 11-06 08 | 1264 M | 1455 M | 28.8% |
| 11-06 12 | **1433 M** | **1512 M** | **30.7%** |

- 内存：**单调线性，约 +17.5 MB/小时**，72.7 小时 174M → 1508M（8.7 倍），**从未回落、没有平台期**。
- CPU：**从 3.4% 单调涨到 30.7%（9 倍）**，同期请求量趋近于 0。
- 对照：`e36546dc8713c200` 只活 41 小时峰值 0.918 GiB；`f0923dfa11aefbaa` 活 72.7 小时峰值 1.476 GiB。**峰值与存活时长正相关，与业务量无关**（业务量本就 ≈0）。

**判定**：

- 「进程启动后长期驻留」→ **设计使然**（懒启动 + 无 idle 回收，见 §2.4 与 §4.2）。
- 「RSS 线性涨到 1.5GB、CPU 涨 9 倍、页面永不重载」→ **不是设计意图，是回收缺口/泄漏**。服务端 3 天日志里 0 条浏览器重启、0 条关闭；`/tmp/rod/user-data` 下只有 1 个目录说明从未换过 profile。

---

## 7. 证据强度与未证实项（请勿越界引用）

**已证实（服务端可直接复现）**

1. 04:12 是手动 stop+start，容器未重建、镜像未变、RestartCount=0、OOMKilled=false。
2. 旧运行日志完整存在（25886 行，首行 2026-11-03T03:27:27.047Z，无轮转）。
3. 旧运行以「收到退出信号 → 优雅停机预算耗尽 → 被强杀」结束，非崩溃。
4. 旧运行 systemd cgroup 账：CPU 2d6h39m37s、内存峰值 1.9G、swap 峰值 322.9M、3 天写盘 17.2G。
5. 浏览器只在 2026-11-03T03:27:54Z 拉起过 1 次，user-data-dir 唯一（f0923dfa11aefbaa），最后写入 04:11:48Z，距停机 41 秒。
6. 重启后至今**无浏览器**：0 个 chrome 进程 / pids=12 / memory.peak=153MiB / 新运行 0 行 captcha。
7. 重启后**0 条真实请求**（admin API `requests.total=0` + 日志 0 行 ingress）。
8. 3 天 123 条请求，captcha 字段全 0，**0 次 token 真实消费**。
9. renderer RSS 174M→1509M 线性增长、CPU 3.4%→30.7%，无回落。
10. 容器无内存上限；宿主从未 OOM-kill；当前 MemAvailable ≈1.8GiB；swap 已用 2.09GiB，主要由 proxy-checker 的 python 贡献（1.16GiB）。
11. 控制台「驗證瀏覽器 online」是假信号（只看配置开关，`internal/adminapi/monitor.go:26-29`）。

**未证实 / 证据不足（不要当结论用）**

- 「每 95s 补一条 token 导致浏览器持续被驱动」——服务端无 mint 计数日志，只有 profile 写入时间作间接证据，需 task-1 从代码确证。
- 「浏览器占 109% 里的 55%~60%」——chrome 子进程（gpu/zygote/utility）不在 top-5 采样内，只能给出主渲染进程 30.75% 这个硬数。
- 「重启后第一条验证码请求会让浏览器重新拉起」——依据代码路径（`FetchConfig` 只在 `getVerifyParam` 里调用）推断，未经实测触发（不允许改配置/发请求）。
- 「1Panel monitor.db 只覆盖 7 天」——10-30 之前的历史未知，故无法判断 10-23~10-30 那几代的寿命。
- 11-03 03:27:47 那次 `FetchConfig` 的确切触发者（首条模型请求 or 管理端操作）——日志只记结果，未记触发来源。
- `sar` 历史不可得（未安装 sysstat）。

---

## 8. 复现命令清单（全部只读）

```bash
# 1) 容器身份与这次重启的性质
docker inspect zcode2api --format 'Created={{.Created}} StartedAt={{.State.StartedAt}} FinishedAt={{.State.FinishedAt}} RestartCount={{.RestartCount}} OOMKilled={{.State.OOMKilled}} ExitCode={{.State.ExitCode}}'
journalctl --since "2026-11-06 12:10:00" --until "2026-11-06 12:15:00" -o short-iso | grep -iE 'f23a35|signal 15|Consumed'

# 2) 旧日志时间跨度
CID=f23a35acfcd49fcbd59163aa3d49a3b5cdd70b06fe3099dd45ae8b5b63d7574a
LOG=/var/lib/docker/containers/$CID/$CID-json.log
wc -l $LOG; head -1 $LOG; tail -1 $LOG

# 3) 浏览器在不在（按 exe 基名判定，避免匹配到 shell 自己）
for p in /proc/[0-9]*; do e=$(readlink $p/exe 2>/dev/null); b=$(basename "$e" 2>/dev/null); case "$b" in chrome|chromium|headless_shell) echo "$p $e";; esac; done
docker top zcode2api
S=/sys/fs/cgroup/system.slice/docker-$CID.scope
cat $S/pids.current $S/memory.peak $S/memory.max $S/memory.swap.current

# 4) 浏览器残留与寿命（容器可写层在 restart 后保留）
docker exec zcode2api sh -c 'ls -la /tmp; ls -la /tmp/rod/user-data; find /tmp/rod -type f -printf "%TY-%Tm-%Td %TH:%TM:%TS %p\n" | sort -r | head'

# 5) 请求与 captcha 频率：逐行 json.loads 后统计 ingress / 请求完成行 / captcha_* 字段（避开 ANSI 干扰，grep 会被转义序列坑到）
python3 -c "import json;..."

# 6) 历史曲线（1Panel，5 分钟一点，仅 7 天窗口）
python3 -c "import sqlite3;c=sqlite3.connect('file:/opt/1panel/db/monitor.db?mode=ro',uri=True);..."

# 7) 管理接口池指标（密钥从 accounts.db 的 meta 表读，不回显）
KEY=$(python3 -c "import sqlite3;c=sqlite3.connect('file:/opt/1panel/apps/local/zcode2api/data/accounts.db?mode=ro',uri=True);print(c.execute(\"select value from meta where key='admin_key'\").fetchone()[0])")
curl -s -H "Authorization: Bearer $KEY" http://127.0.0.1:3000/admin/api/monitor
```
