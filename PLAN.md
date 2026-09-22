# zcode2api Go 版重写计划

> 本仓库即 Go 重写仓库（原 `go/` 子目录已上提到仓库根，Python 版实验工作区内容已移除）。
> 计划与行为契约参照 Python 版主仓库的 `app/` + `main.py` 与 `HANDOFF.md`。
> 目标：用 Go 重写 Python 版的全部后端功能，
> 做到**与 Python 版行为对齐、数据互通（共用同一个 `data/accounts.db`）、前端零改动**。
> 本文档是唯一的计划与进度台账，每完成一项就勾选对应 `- [ ]`。

---

## 1. 背景与动机

- Python 版已稳定可用（FastAPI + httpx + SQLite + cloakbrowser），但部署形态依赖 Python 运行时 + Node 子进程。
- Go 版的收益：**单二进制部署**、goroutine 并发模型天然消除"事件循环阻塞 / 全局锁串行"两类问题、
  可借机**甩掉 Node 运行时**（jsdom 求解器已被上游 F001 风控判死，无移植价值）。
- 前端（`frontend/`，React SPA）**不重写**：构建产物 `frontend/dist` 直接 `go:embed` 进二进制。

## 2. 范围

**移植（与 Python 版 1:1 对齐）：**

- [x] `/v1/messages` 网关：多账号轮询、SSE/JSON 流式透传、错误分类与自动换号
- [x] `/v1/models`（Anthropic / OpenAI 双兼容超集形态，见 §5.7）
- [x] `/v1/chat/completions` **OpenAI（GPT）兼容层——Go 版增量功能**：请求/响应双向转换 +
  流式 SSE 重编码 + 工具调用，同步走网关引擎（详见 §5.7）
- [x] `/v1/responses`（OpenAI Responses API，服务 Codex CLI 生态；**排期在 completions 验收之后**，划界见 §5.8）
- [x] `/async/v1/messages`：ticket + SSE keepalive + 流中断终止语义（`_MidStreamError`）
- [x] 账号状态机（active/exhausted/cooling/invalid/disabled）+ 按模型可用性调度
- [x] 额度监控（`billing/balance` 解析、多订阅合并、15s 缓存 + 并发去重、后台周期刷新）
- [x] 调度 token 统计（UsageCollector：SSE `message_start`/`message_delta`、JSON 顶层 usage）
- [x] Admin API `/admin/api/*` 全部端点 + SPA 托管（`/admin/*` catch-all 回落 index.html）
- [x] 鉴权：后台密钥（含单 IP 失败限速 10 次/5 分钟）+ 网关密钥（fail-closed）
- [x] OAuth 登录（Z.AI 授权 → JWT 入池 → API Key 兑换链）
- [x] 账号级出站代理（http/https/socks4/socks5/socks5h）+ 命名代理管理 + 出口探测
- [x] 验证码：真实 Chromium 池（rod）+ 人工回填兜底
- [x] SQLite 持久化（accounts + meta，WAL）与 **Python 版数据库互通**
- [x] CLI 子命令（serve / login / add-account / accounts / remove-account / quota / status / set-admin-key / export / import）
- [x] Release CI（2026-09-11 定案：放弃 Docker 裸二进制交付；GitHub Actions 推 v* tag 构建 linux/darwin/windows × amd64/arm64 并上传 Releases）— `.github/workflows/release.yml`
- [x] **容器化交付（2026-09-15 决策翻转：Docker 重新纳入）**：多阶段 `Dockerfile`
  （golang:1.25-bookworm 构建 → debian:bookworm-slim 运行，非 root uid 10001）+ `docker-compose.yml`
  + `.dockerignore`；CI 每次 push 构建镜像当守门员，推 `v*` tag 时构建 linux/amd64 + linux/arm64
  多架构镜像并发布到 GHCR。翻转依据见 §9。
- [x] **套餐自动领取（Go 版增量，2026-09-10 后新增，Python 主仓已上线）**：billing/preview + billing/claim、
  激活事件上报、业务码翻译、3007 换码重试、入池自动领取（对照 Python 主仓 `app/claim.py` + `app/telemetry.py`，见 §5.9）；
  后续补齐领取状态落盘 / 冷却 / 定时（M13、M15）

**明确不移植：**

- `captcha_node/solver.js`（jsdom 无浏览器求解——已被上游 F001 风控识别，回退路径改为人工回填）
- 裸机 systemd 部署（Python 版已删除）
- Python 版曾有的 OpenAI 端点残留（残缺实现已从 Python 版删除；Go 版按 §5.7 完整契约重新实现，
  属增量功能而非移植项）
- `/v1/completions`（legacy）、`/v1/embeddings` 等其它 OpenAI 端点

## 3. 技术选型

| 领域 | 选择 | 理由 |
|------|------|------|
| Go | 1.22+ | 需要.ServeMux 的 method + wildcard 路由增强 |
| HTTP | 标准库 `net/http` | 不引框架；SSE 用 `http.Flusher` 手写透传 |
| SQLite | `modernc.org/sqlite` | 纯 Go 无 CGo → 可交叉编译单文件 |
| 浏览器自动化 | `github.com/go-rod/rod` | 验证码求解；复用 cloakbrowser 下载的 Chromium 二进制 |
| 前端嵌入 | `embed` | dist 打进二进制，单文件交付 |
| 配置 | 环境变量（沿用 `ZCODE_*` 命名）+ `.env`（`godotenv` 或自写 30 行） | 与 Python 版配置兼容 |
| 日志 | 自写包 `internal/web`（彩色终端 + writer 可注入） | 原计划用 `log/slog`，实际未采用：契约要求与 Python 版 `app/logs.py` 的彩色行形态逐字对齐（`>>>`/`<<<`/`<!>` 等），标准库结构化的收益抵不上重写成本。详见 §5.14 |
| 依赖原则 | 最少依赖：sqlite、rod、godotenv，其余标准库 | 便于审计与长期维护 |

## 4. 目标目录结构

```text
.（仓库根，即原 go/ 上提）
├── PLAN.md                      # 本文件（计划 + 进度台账）
├── go.mod
├── cmd/zcode2api/main.go        # CLI 入口（serve / login / accounts / ...）
└── internal/
    ├── config/config.go         # ← app/settings.py（环境变量、路径、上游端点、常量）
    ├── model/account.go         # ← app/models.py（Account、状态机、模型可用性、JSON 字段对齐）
    ├── store/store.go           # ← app/store.py（SQLite 单连接 + 轮询游标 + meta + 密钥引导）
    ├── gateway/
    │   ├── engine.go            # 选号重试循环 + 上游调用核心（/v1/messages 与 chat/completions 共用）
    │   ├── handler.go           # ← routes/gateway.py（/v1/messages、/v1/models 字节级透传）
    │   ├── classify.go          # 错误分类（鉴权/402/429 码族/3010/3007/F001/captcha 头）
    │   └── usage.go             # ← app/usage.py（UsageCollector）
    ├── openai/
    │   ├── convert.go           # OpenAI ↔ Anthropic 请求/响应/工具转换（契约见 §5.7）
    │   └── relay.go             # /v1/chat/completions 流式 SSE 重编码（content_block_delta → chunk）
    ├── upstream/request.go      # ← app/agent.py（build_request + zcode_system.json 注入）
    ├── quota/quota.go           # ← app/quota.py（fetch_quota、多订阅合并、monitor）
    ├── captcha/
    │   ├── manager.go           # ← app/captcha.py（缓存、人工回填、浏览器失败冷却回退）
    │   ├── pool.go              # ← app/captcha_browser.py（goroutine 池，语义对齐：超时杀页不误投）
    │   └── solve.go             # 页面 HTML + 求解 JS（从 Python 版逐字抄写）
    ├── oauth/oauth.go           # ← app/oauth.py
    ├── auth/auth.go             # ← app/auth_admin.py（Bearer 校验 + 失败限速）
    ├── adminapi/handlers.go     # ← routes/admin_api.py（账号/代理/设置/登录/导出导入/监控）
    ├── asyncpool/pool.go        # ← routes/async_pool.py（ticket + SSE + 泄漏防护）
    └── web/
        ├── embed.go             # go:embed frontend/dist + /admin catch-all
        └── logs.go              # ← app/logs.py（彩色终端）
```

主仓库 `app/zcode_system.json` 已复制为 `internal/upstream/zcode_system.json` 并 `embed`。

## 5. 行为契约（必须与 Python 版逐字对齐的部分）

重写时**先抄数字、再抄逻辑**。以下契约直接从 Python 版提取，Go 实现以此为验收依据：

### 5.1 常量

| 项 | 值 |
|----|----|
| `MAX_CAPTCHA_RETRIES` / `MAX_ACCOUNT_ATTEMPTS` / `MAX_RATE_LIMIT_RETRIES` | 3 / 5 / 1 |
| 3010 并发准入重试延迟 | 1s、2s（第 3 次失败原样回传 429） |
| 瞬时限流原地重试 | 1 次，等待 1s（±20% 抖动）；用尽才冷却换号 |
| 对外模型白名单 | `glm-5.3-flash`、`GLM-5.3`（normalize 后比对） |
| `MODEL_NAME_MAP` | 仅 `{"glm-5.3": "GLM-5.3"}` |
| 429 额度上限码族 | 1113、1308-1311、1313、1316-1321 → 标记该模型 exhausted；其余 429（瞬时限流）→ 原地重试 1 次后按阶梯冷却 |
| 平台过载（529 / `code=1305`） | 判据 `IsUpstreamOverload`：状态码 529 **或**业务码 1305（官方表把它标成 429，只看 429 会整类漏掉）。原地退避 1s、3s（±20% 抖动），**不换号、不标状态、不计 fail_count**；用尽后原样透传上游响应 |
| 业务码语义 | `1005`(HTTP 200)=当日额度用完；`3007`=验证码失效；`3010`=并发准入；`1305`=**平台服务过载**（与账号无关）；`1302`=账户维度速率限制；F001=风控指纹拒绝 |
| 最近错误归类 | `last_error_kind` / `last_error_at` 两键记录「最近一次失败」；取值见 `model.ErrorKind*`（13 项，稳定字符串，**上线后不可改名**）。写入必须显式传 kind（编译期强制），且 `last_error_kind` 与 `last_error` 应描述同一次失败 |
| 风控冷却（405） | 判据 `model.IsRiskControlBody(text)` **必须看 body**：405 在本项目有风控拦截 / 计费重复查询 / 缺 system 注入三种含义，只看状态码会把第三种（我方缺陷）变成账号惩罚。风控命中即**整号冷却**，档位取 `RISK_COOLING_STEPS`（默认 `300,900,3600` 秒），**连续次数超过档位数则置 invalid**；成功调用后计数归零。契约见 §5.13 |
| 冷却 / 刷新 | `COOLING_SECONDS=300`（连接失败与 503 的固定值，同时是瞬时限流阶梯的封顶）、`QUOTA_REFRESH_INTERVAL=60`（0=关闭，运行中可改） |
| 瞬时限流冷却阶梯 | 同一账号连续被限流 30s → 60s → 120s → `COOLING_SECONDS`；任意一次成功调用后归零 |
| 领取冷却（仅自动路径） | 优先用上游 `data.plan.ends_at`；缺失时按成因分档：验证码类与 1005/1003 取 `CLAIM_CAPTCHA_COOLDOWN=3600`，其余取 `CLAIM_RETRY_COOLDOWN=600`（秒） |
| 「刷新资格」节流 | `CLAIM_PREVIEW_COOLDOWN=60`（秒，内存态；0=关闭） |
| 验证码缓存 | Node/人工令牌 45s；配置 600s；浏览器令牌**不缓存** |
| 验证码池 | workers=1、startup=90s、request=45s、queue=60s、shutdown=10s、失败冷却=60s |
| 后台限速 | 单 IP 滑动窗口 300s 内失败 10 次 → 一律 429，成功清零 |
| 额度缓存 | 结果 TTL 15s + inflight 去重 + 删除账号清理 |
| 端口 / 密钥 | 3000；admin_key 缺失随机生成（历史默认 `zcode` 强制轮换）；gateway_key `sk-` 前缀，fail-closed |

### 5.2 错误分类链（`/v1/messages`，顺序不可变）

1. 验证码挑战（响应头 `x-aliyun-captcha-*` / body `code=3007` / `F001` 文本仅限 400/403）→ 刷新令牌同账号重试
2. 401/403 → 账号 `invalid`，换号（JWT 上游为**裸 401 空 body**；api.z.ai 为 `error.type=1000/1001/1003`）
3. 402 → 该模型 exhausted，换号 + 触发额度刷新
4. 429 且 code∈3010 → 等待重试（账号状态不变）
5. **529 或 code=1305（平台服务过载）→ 原地退避重试（1s、3s），用尽后原样透传**；不换号、不标状态、不计 fail_count
6. 429 且 code∈额度上限码族 → 该模型 exhausted 换号；其余 429（1302 等瞬时限流）→ **原地重试 1 次**，用尽才按递进阶梯冷却换号
7. 503 → cooling 换号（固定 `COOLING_SECONDS`，不走阶梯）
8. 其余 → **不做状态推断，原样透传上游响应**

**为什么平台过载必须排在 429 之前**：官方错误码表把 1305 标成 429，若先过 429 分支，一次平台过载就会被当成「该账号被限速」而挨上 30→60→120→300s 的递进冷却，整池健康账号会被逐个踢出调度。1305 的定义是「平台服务过载，与单一账户的调用行为无直接关系」（1302 才是账户维度），换号救不了、冷却只会缩小可用池，**官方对它的处置建议就是「增加重试间隔、避免立即高频重试」**。
一线实测（2026-09-20 日志包）：z.ai 在 Anthropic 兼容面用 **HTTP 529** 承载 1305。分类链只认 429 时，30 次过载全部落到第 8 条被原样透传，网关一次都不重试，上层中转以 80ms 间隔连冲 26 次后放弃——这正是官方点名要避免的做法。

同账号重试有三类，**预算各自独立**（`internal/gateway` 的 `attemptBudget`）：验证码刷新
（`MAX_CAPTCHA_RETRIES`，含首次尝试）、3010 并发准入等待（`len(BusyRetryDelays)`）、瞬时限流原地重试
（`MAX_RATE_LIMIT_RETRIES`）。三者曾共用同一个循环变量：一次验证码重试就会吃掉 3010 的等待预算，
且延迟会按验证码次数取到错误下标，到第三次迭代时 3010 直接把 429 回传客户端。
`TestCaptchaRetryDoesNotConsumeBusyBudget` 为这条不变式做回归。

**为什么瞬时限流只重试一次**：池子里通常还有别的账号，换号成本是百毫秒级；原地等待是确定的秒级
延迟，且被限流后立刻重试大概率仍然失败。一次对冲只用来捞「限流窗口秒级复位」这种情况。
**为什么冷却仍要设**：原地重试用尽后若不给调度器门禁，下一个请求会再次选中同一账号并重复失败，
形成热循环，比冷却更糟——该改的是时长（递进阶梯）而不是有无。冷却到期由 `IsSelectable`
自动放行（`EffectiveStatus` 同时返回 active），任意一次成功调用也会立刻清零阶梯计数。

### 5.3 上游请求

- JWT 账号 → `https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages`，`Authorization: Bearer <jwt>`，
  **必须注入 `zcode_system.json` 到顶层 `system`**（否则上游 405），并携带验证码头 + `X-Device-Mid`。
- API Key 账号 → `https://api.z.ai/api/anthropic/v1/messages`，`x-api-key` 头，无需验证码。
- 固定头：`anthropic-version: 2023-06-01`、`User-Agent: ZCode/3.11.2`、`X-ZCode-App-Version: 3.11.2`、
  `X-ZCode-Agent: glm`、`HTTP-Referer: https://zcode.z.ai/`、`X-Device-Mid: <uuid4 持久化于 data/device_mid.txt>`。
- 透传剔除：host/content-length/x-api-key/authorization/user-agent/http-referer/accept-encoding/connection/cookie/验证码头；
  `x-zcode*` 开头一律剔除。

### 5.4 数据互通（最高优先级契约）

Go 版**直接打开 Python 版的 `data/accounts.db`**，schema 完全一致：

```sql
accounts(id TEXT PK, provider, name, mode, status, enabled INT, created_at REAL, data TEXT/*JSON*/)
meta(key TEXT PK, value TEXT)
```

- `accounts.data` 是 `Account` 全量 JSON。Go 结构体的 json tag 必须**逐一对应 Python dataclass 字段**
  （snake_case）：`id,name,provider,mode,email,jwt_token,api_key,enabled,status,quota,exhausted_models,
  disabled_models,plan,plans,usage,use_count,fail_count,total_input_tokens,total_output_tokens,
  total_cache_creation_tokens,total_cache_read_tokens,last_used_at,last_checked_at,cooling_until,
  last_error,proxy_url,proxy_id,created_at`；未知字段忽略（对齐 `Account.from_dict`）。
- **Go 增量键**（原约定为"不新增键"，2026-09-16 重新评估：Python 侧已退休，
  多余键只会被 `json.loads` 原样保留、旧版读 Go 数据也不受影响，故该约定已放开为"只增不减"）：
  `archived_at`（归档）、`user_id`（身份判据）、`virtual_device_mid`（每账号设备指纹）、
  `claim`（领取状态，见 §5.9.1 / §5.10）。键集由 `TestJSONContractWithPython` 逐个钉死
  （现 32 键，并额外断言 `claim` 的嵌套键名）。纯运行期退避状态（如 `RateLimitStreak`）
  仍用 `json:"-"`，不进该集合。
- 导出/导入格式（`version:1` + `providers.{name,mode,secret,disabled_models}`）保持一致——
  刻意不含 Go 增量键：设备指纹是机器绑定的，跨机搬运反而有害（导入后重新分配更合理），
  领取状态则是瞬时状态。
- 两个版本可交替打开同一个 db（不做 schema 迁移）。

### 5.5 验证码（照抄 `captcha_browser.py` 的思路与字符串）

- **HTML 与求解 JS 逐字抄写**：`AliyunCaptchaConfig` 先于内联 SDK（SDK 取自 `captcha_node/AliyunCaptcha.js.txt`，
  `</script>` 替换为 `<\/script>`）；`initAliyunCaptcha` 配置（mode=popup、language=en、`#cap`/`#btn`）、
  `getInstance` 内 `startTracelessVerification ?? show`、`success` 写 `window.__zcodeOutcome`、
  轮询间隔 250ms、SDK 加载 20s / 单次求解 40s。
- **成功复用同一页面，失败重载页面清 SDK 内部状态**；token 只在内存。
- **池语义对齐**：goroutine worker 槽位 = 并发上限；请求超时 → 关闭该页面/浏览器实例并替换，
  迟到的 TOKEN 通过 context 取消保证**绝不误投**后续请求；启动阶段失败 → 60s 冷却回退人工回填提示。
- **浏览器二进制复用**：rod 通过 `launcher.Bin(...)` 启动 cloakbrowser 下载到 `CLOAKBROWSER_CACHE_DIR`
  的同一 Chromium + 相同 `_BROWSER_ARGS`（`--disable-dev-shm-usage` 等 5 项），驱动差异极小化。
  构建期仍执行 `python -m cloakbrowser install` 预下载（仅构建期需要 Python）。
- 回退链：浏览器池不可用/失败冷却 → 返回 503 `captcha_required`（提示后台 `/admin/captcha` 人工回填），
  人工令牌缓存 45s。

### 5.6 async ticket 语义

- SSE 事件：`ticket`(pending) → `ready` → `data:` chunk… → `done`/`error`；每 10s `: keepalive`；
  总超时**默认 300s，可用 `ZCODE_ASYNC_TICKET_TIMEOUT` 调（下限 30，无上限）**。
- ⚠️ 该超时是**整票的墙钟寿命**（`deadline = tk.createdAt + Timeout`，见 `streamTicket`），**不是空闲
  超时**——thinking 档拉满的长请求在 sync `/v1/messages` 上能跑满 8 分钟（无总超时），但在 async 上
  到点必被截断，客户端收到 `type: ticket_timeout` 的错误事件。凡是要跑长流的工作流走 async，
  必须先把这档调高，否则故障是**设计使然**而非上游故障。
- ✅ **async 出站线路已对齐网关**（v2.4.0-go 修复）。历史成因**不是「移植时丢参数」**，而是上游
  2026-09-15 的 `0d370e5 fix: route async requests through the account proxy` 我们没挑
  ——该提交是 v2.0.9-go 的祖先，我们却没跟（血缘与逐笔对照见
  `docs/upstream-async-audit-2026-09-22.md`）。修复形态：`p.client()` → `p.clientFor(acc)`，
  走 `proxy.TransportForTimeout(raw, 180s)`；`TransportFor` 拆出 `TransportForTimeout`
  且**缓存键纳入超时值**（网关 120s 与 async 180s 各持一份 Transport，否则先到者污染另一个用途）。
  ⚠️ **不要用 `proxy.ClientFor`**：它设的是 `http.Client.Timeout`（整体超时），对 SSE 等于给流设上限。
  同批另三处缺口一并回移：跳过 apiKey 账号（`e86c5bc`）、成功时记用量并复位状态（`5105b5d`）、
  入口模型归一化（`6da8df6`，另含 M11 的 5 个伴生项）。
- 归因对照改由**显式开关**提供：`async_force_direct`（后台「系統設定 → Async 強制直連」，
  env 默认值 `ZCODE_ASYNC_FORCE_DIRECT`，默认关）。开启后 async 忽略账号代理恒直连，
  诊断行 `route` 如实报 `direct`。这样「线路 vs 直连」两个方向都可复现，不再依赖实现缺陷。
- 泄漏防护三件套照搬：SSE 退出 finally 释放 + 中止后台任务；孤儿 ticket 建票时清扫（生命周期 + 60s 宽限）。
- 流中断：**已发出 chunk → 终止票务（upstream_stream_interrupted）不重试**；零 chunk → 换号重试（最多 3 次，指数退避 2^n）。

### 5.7 OpenAI（GPT）兼容端点契约（Go 版增量）

- 路由：`POST /v1/chat/completions`，**同步**走网关引擎（选号/验证码/错误分类全复用），不经 async ticket 池；
  鉴权与 `/v1/messages` 相同（网关密钥 fail-closed）。
- `/v1/models` 返回双兼容超集：每项同时带 Anthropic 侧 `id/display_name/type:"model"` 与
  OpenAI 侧 `object:"model"/created/owned_by:"zcode2api"`，顶层 `object:"list"`。

**请求转换（OpenAI → Anthropic Messages）：**

| OpenAI 字段 | 处理 |
|-------------|------|
| `messages[].role = system/developer` | 全部归并到顶层 `system`，按顺序拼接，且排在 zcode_system 块之后 |
| `role = user/assistant` | 原样映射；content 字符串 → `[{type:"text"}]` |
| content part `text` | text block |
| content part `image_url`（data URL） | image block（base64，media_type 从 data URL 解析） |
| content part `image_url`（http[s]） | image block（`source.type="url"`）；上游支持度未知，失败按上游错误原样透传 |
| assistant 消息含 `tool_calls` | assistant 消息 + `tool_use` block（`arguments` JSON 字符串解析为 `input`） |
| `role = tool` | user 消息 + `tool_result` block（`tool_use_id` 绑定） |
| `model` | 白名单校验 + `MODEL_NAME_MAP`（与 /v1/messages 一致，白名单外 400） |
| `max_tokens` / `max_completion_tokens` | → `max_tokens`；两者皆缺省 8192 |
| `temperature` / `top_p` | 透传 |
| `stop`（string 或 array） | → `stop_sequences` |
| `stream` | true → OpenAI chunk 流；false → JSON |
| `tools` / `tool_choice` | 仅支持 function 工具，`parameters` → `input_schema`；`auto/none` 保留语义、`required` → `any`、named → `{type:"tool",name}`；强制的函数必须已声明，重复名称/不支持的类型/`strict:true` 明确 400 |
| `parallel_tool_calls` | 转换为 `tool_choice.disable_parallel_tool_use` 的反值；未指定 choice 时补 auto，none 时不附并行开关 |
| `n` | 仅支持 1，>1 返回 400 |
| `reasoning_effort`（顶层） | GLM-5.3 系列原生 low/high/max；Coding Plan 别名 none/minimal/low→low、medium/high→high、xhigh/max→max，转为 Anthropic output_config.effort；不虚构预算、不改变输出上限 |
| `thinking`（客户端自带） | 模型强制思考，disabled 返回 400；Pi 无预算 enabled 开关沿用上游默认思考，不再补猜测预算。显式 budget_tokens 校验后独立保留，不覆盖 effort；不透传 clear_thinking |
| `presence_penalty` / `frequency_penalty` / `logprobs` / `user` 等 | 静默忽略（README 声明） |

**响应转换（Anthropic → OpenAI）：**

- 非流式：`choices[0].message` 中 text blocks 拼接为 `content`；`tool_use` →
  `tool_calls[{id, type:"function", function:{name, arguments(JSON 字符串)}}]`；
  `stop_reason` 映射：`end_turn/stop_sequence→stop`、`max_tokens→length`、`tool_use→tool_calls`、
  `refusal→content_filter`；usage：`prompt_tokens = input + cache_read + cache_creation`、
  `completion_tokens = output`（cache 细节放 `prompt_tokens_details.cached_tokens`）；`id = "chatcmpl-<上游 id>"`。
- 流式（**SSE 重编码，不是透传**）：`message_start` → 首个 chunk（`delta:{role:"assistant"}`）；
  `content_block_delta`(text_delta) → `delta.content` 增量；`content_block_start`(tool_use) →
  `tool_calls` delta（id/name）+ `input_json_delta` → `arguments` 增量；`message_delta` →
  `finish_reason` 终止 chunk；`message_stop` → `data: [DONE]`；`ping` 事件丢弃；
  `stream_options.include_usage` 时在终止前附 usage chunk。
- 没有参数增量的工具在块结束时补发初始 input；同角色的相邻消息合并，确保同轮多个工具及回传结果成组送达上游。
- 上游 error、非法 JSON 或缺少 message_stop 的断流必须发送 `upstream_stream_error`，不发送正常 `[DONE]`，也不累计为完整交付。
- **思维链（Go 版增量）**：`thinking` block → `message.reasoning_content`
  （DeepSeek / GLM 系 OpenAI 兼容端点的惯例字段；无思考块时**不写该键**）；
  流式 `thinking_delta` → 增量 chunk 的 `delta.reasoning_content`（只带该字段、不带 `content`），
  `signature_delta` 与 `redacted_thinking` 不外泄（签名属内部凭据）。
  多轮历史中的 `reasoning_content` **不回灌**为 thinking 块（缺签名，上游会拒），见 §8 风险表。
- UsageCollector 在重编码旁路照常解析 Anthropic 事件——账号调度统计不受转换影响。
- **账号用量的计入判据是「上游是否交出终值」，不是「客户端有没有把流读完」**：
  收到带非空 `stop_reason` 且含 `output_tokens` 的 `message_delta`（或非流式 JSON 解析出
  `usage`）即认定为终值；此后交付即便因客户端提前断开而失败（编辑器类客户端收到
  `finish_reason` 就立刻关流，是常态而非故障），这笔用量仍要累计。没有终值的半截流
  （缺少 `message_stop` 且未见终值）仍旧不计入——这正是上一行「不累计为完整交付」的本意。
  判据实现见 `gateway.UsageCollector.UsageComplete`，同步与 async 两条路径同口径。

### 5.8 `/v1/responses`（无状态兼容层）

- 定位：服务 Codex CLI 等 Responses 生态客户端；复用 §5.7 的引擎与转换基建，增量约 300-500 行。
- v1 范围：`instructions` → system；`input`（字符串 / 类型化 item 数组：message、function_call、
  function_call_output）→ messages；扁平 `tools` → Anthropic tools；`max_output_tokens` → `max_tokens`；
  `reasoning.effort` 按 §5.7 模型原生档位归一化后转为上游 `output_config.effort`，
  不再伪造 thinking 预算；未指定参数时保留上游强制思考与默认 max。输出端 text → `output_text`、
  tool_use → `function_call`、thinking → `reasoning` item（`summary[].summary_text` 承载思考内容，
  流中按上游内容块首次声明顺序分配连续 `output_index`，不插入虚构空 message）；流式重编码为 `response.*` 事件序列
  （`response.output_text.delta`、`response.reasoning_summary_text.delta`、`response.output_item.added` 等）。
- **状态化划界**：无状态用法全支持（`store:false` + 每轮完整历史，Codex 默认即此）；
  带 `previous_response_id` 的请求 v1 返回明确 400；内存 LRU 回放列为后续可选增强，不阻塞。
- 工具控制与 §5.7 共用实现，Responses 的 named choice 使用扁平 `name`；响应反映调用方的 parallel_tool_calls。
- SSE 补齐 created/in_progress、output_item added/done、content_part added/done、文本/思考摘要/工具参数的 delta/done，所有事件携带连续 sequence_number，内容带 content_index 或 summary_index。
- 工具参数按上游 block index 分别累积，支持交错增量；最后一个项目的 done 位于终止 response 之前。
- 正常结束为 completed；max_tokens 截断为 incomplete + reason=max_output_tokens；上游错误/非法 JSON/断流为 failed，不伪装完成。

### 5.9 套餐自动领取（Go 版增量，2026-09-10 新增）

- 定位：Python 主仓 2026-09-10 上线的活动套餐自动领取（`app/claim.py` + `app/telemetry.py`），
  Go 版对齐移植，排期 M8（M6 交付后）。
- 链路：`GET {BILLING_BASE}/billing/preview?app_version=&platform=` → 解析
  `data.plans[]`（plan_id/name/priority + model_usage token grants）→
  `POST {BILLING_BASE}/billing/claim` body `{"plan_id"}`，需验证码头
  `X-Aliyun-Captcha-Verify-Param`（+ 可选 `X-Aliyun-Captcha-Verify-Region`）。
- 业务码映射：1001 套餐不存在 / 1002 活动结束 / 1003 已领取过 / 1004 不符合条件 /
  1005 今日名额用完 / 3001 参数错误 / 3007 验证码失败（换码重试一次）/ 401 未登录。
- 激活上报：preview 前对 `https://zcode.z.ai/api/v1/event/report` 发 `app_launch` +
  `app_daily_active` 两事件（16 字段体，无 Authorization；疑似活动投放资格信号；
  失败仅记日志不阻断）。device_mid 取**账号自己的指纹**（见 §5.10）。
- 触发点：入池后（批量添加 / OAuth 完成 / CLI login）后台自动全量领取 +
  Admin API `GET /claim/preview`、`POST /claim`（account_ids 可选，冷却账号跳过）+
  前端账单页按钮（工具栏全量 + JWT 账号行内单账号）。
- 复用项：鉴权头与 quota 同源（`X-ZCode-App-Version`/`X-Platform`/`X-Device-Mid`）；
  请求走账号代理（与 Python 版 make_async_client 语义一致）；
  验证码经 M5 的 captcha manager 求解/人工回填。

#### 5.9.1 领取状态、冷却与并发（2026-09-16 新增，v2.0.6 排期）

对齐 zcode-switch 的 autoClaim 设计补齐三处（此前只有"入池领一次"）：

- **上游给的 `next_at` 必须留住**：上游在成功响应与 1005（当日名额用完）里都会给出
  `data.plan.ends_at` ——那是它自己算好的下次可领时间，此前只取了错误文案把时间丢掉。
  现落到 `Account.claim.next_at`（兼容秒/毫秒时间戳），后端与前端据此倒计时。
- **冷却只作用于自动路径**：手动点按钮（`POST /claim`）始终可强制触发——用户点了
  没反应是最糟的交互，冷却的目的是避免自动路径白打上游，而不是拦用户。
  自动路径在 `claim.next_at` 未到期时直接跳过。
- **单槽串行闸门**：`claimGate`（容量 1，非阻塞抢占）。上游按账号维度做验证码与领取
  风控，并发领取只会互相挤兑 captcha 池；批量导入 20 个账号时尤其明显。
  抢不到即跳过该账号（**不排队**——把 goroutine 堆着等与并发并无区别）。
- **不做后台周期重试**：网关是长驻服务，定时领取会产生持续的上游流量；
  保持"入池一次 + 手动"两个触发点。
- 业务码与 `next_at` 经 `claim.FailureOutcome` 回传，`adminapi` 据此落盘，不解析错误文案。
- **`Claim` 的并发访问统一走锁**：领取在后台 goroutine 写、后台快照与落库序列化在读
  （CI 的 `-race` 实测抓到过竞态，run 35091374551）。`Account` 内嵌 `claimMu`，
  读写走 `SetClaimState`/`ClaimView`，序列化由 `MarshalJSON` 在锁内完成；
  已发布的 `ClaimState` 视为不可变（改动一律生成新快照整体替换）。
  回归 `TestClaimStateConcurrentAccess`（-race 下压该不变量）。

#### 5.9.2 领取设定与每日定时（2026-09-16 新增）

领取设定进后台「系統設定」页，**即时生效**（冷却与调度器每轮重新读取，无需重启）；
环境变量 `ZCODE_CLAIM_*` 降级为**默认值**（`store` 访问器在键缺失/非法时回退 config）：

| 设置键 | 默认 | 说明 |
|---|---|---|
| `claim_auto_enabled` | `true` | 入池自动领取开关（只约束自动路径，手动按钮不受影响） |
| `claim_schedule_enabled` | **`false`** | 每日定时领取开关 |
| `claim_schedule_time` | `23:00` | 定时点，本地时区 `HH:MM` |
| `claim_captcha_cooldown` | `3600` | 验证码类/已领过但无 ends_at 的冷却（秒，≥60） |
| `claim_retry_cooldown` | `600` | 其他失败的冷却（秒，≥30） |
| `claim_preview_cooldown` | `60` | 「刷新资格」节流（秒，≥0） |

**每日定时调度器**（`ClaimScheduler`，对齐 `quota.Monitor` 的 Start/Stop 形态）：

- 每 30s tick 一次并实时读设置；本地 `HH:MM` 等于目标且当天未触发过才执行
  （内存记录已触发日期，防同一天重复）。**错过不补跑**——23:00 时进程没在跑就等下一天，
  避免用户无感知的补打上游。
- 批量对池内全部 JWT 账号（非归档）顺序领取，**尊重 `claim.next_at`**（上游自己给的节奏，
  刚领过的账号跳过）；抢串行闸门限时等待而非丢账号（一天一次的批量，慢点没关系）；
  账号间隔 1s；结束记汇总日志（成功/失败/冷却跳过/闸门占用跳过）。
- **定时默认关闭**：升级/新装不该在用户无感知时自发产生每日上游流量，由管理员在设置页打开。

### 5.10 账户身份与设备指纹（Go 版增量，2026-09-16 新增）

- **身份与凭据解耦**：`accounts.data` 新增 `user_id`（JWT payload 的 `user_id`，`sub` 兜底）。
  同一个号的 token 每次登录都会变，只比凭据字节会让它变成"另一条记录"。
  入池判重按 **user_id → email → 凭据** 三轮优先级遍历（`Store.duplicateLocked`）：
  必须分遍而不能单遍取首个命中——单遍会让列表里靠前的低优先级判据抢先于靠后的高优先级判据。
  `user_id` 一律由 `secret` 派生，故手动添加 / 导入 / CLI 路径自动获得身份判定；
  OAuth 路径额外传入 email（该类 token 形态当场拿不到邮箱）。
- **每账号设备指纹**：新增 `virtual_device_mid`。此前全仓只有一份全局 `device_mid.txt`
  （`config.DeviceMid()`），同一台机器上的 N 个账号共用一个设备标识，等于主动给上游递关联线索。
  取值走 `Account.DeviceMidOr(fallback)`：有账号指纹用账号的，缺失才回退全局值。
  回退值由调用方传入，`model` 包不因此依赖 `config`。
- **存量为一次性迁移**（`Store.migrateAccountIdentity`，`load()` 末尾，幂等）：
  回填缺失的 `virtual_device_mid` 与 `user_id`，仅在有改动时落库。
  **不删除、不合并任何存量账号**——判重只作用于新增。
- **导出格式刻意不加新字段**：`ExportPayload` 保持 version 1。设备指纹是机器绑定的，
  跨机搬运反而有害（导入后重新分配更合理）；领取状态是瞬时状态。
- ⚠️ 迁移会改变存量账号的 `X-Device-Mid`，上游可能识别为「换了台机器」，需在线观察。

### 5.11 登录与出站线路（Go 版增量，2026-09-16 新增）

- **代理必须在建立登录会话时选定**（`POST /login/start` 可带 `proxy_id`，或直接给
  `proxy_url`）。理由是登录、API Key 兑换、随后的额度刷新与自动领取属于**同一条出站链路**：
  只在登录本身走代理、后续直连，等于拿真实 IP 去打上游——那恰恰是需要代理的人最不想要的。
  所以会话一建立，出口就锁定（前端在选择器上置灰并说明原因）。
- 线路 ID 不存在、或地址协议不在 `proxy.AllowedSchemes`（http/https/socks4/socks5/socks5h）
  内，一律**提前 400**，不要把坏代理带进会话等兑换时才炸。
- `oauth.ExchangeCode` / `ExchangeAPIKey` 及其内部的 `postJSON` / `getJSON` / `postJSONAuth`
  全部接受 `proxyURL`；空值时用零值 `http.Client`（**保留环境变量 `HTTP_PROXY` 的既有语义**，
  不要改成 `proxy.ClientFor("")`，那会显式关掉环境代理）。
- 网络错误文案经代理时附带**已脱敏**的代理地址（`proxy.MaskURL` 把密码换成 `***`），
  使使用者能区分"上游挂了"和"代理不通"；代理密码不得出现在响应体与日志里。
- 登录成功后把线路写到账号（`proxy_id` → `AssignProxyProfile`；裸地址 → `account.ProxyURL`
  并解除线路指派）。**未指定时保持账号原有指派不动**——重新登录不该把已有线路清掉。
- 后端 `test` 覆盖：真发 CONNECT 到假代理（证明请求确实经代理）、非法代理提前报错、
  脱敏不泄密码、`resolveLoginProxy` 三条分支。

### 5.12 账号选择：额度优先调度（Go 版增量，2026-09-20 新增）

`store.Select(provider, skipIDs, modelName)` 是唯一的选号入口（网关与 async 池共用），
分五层，**前一层筛出的池子决定后一层的作用范围**（层与层之间是「替换」而不是「加权」）：

| 层 | 规则 | 被排除者 |
|---|---|---|
| 1 可选 | `IsSelectable(now)` 且不在 `skipIDs` | 停用/归档/冷却中，以及本次请求已试过的 |
| 2 模型分档 | 该模型 `available` 者优先；无 available 时用 `unknown` | `absent`（快照里没这个模型）、`exhausted`、`disabled` |
| 3 优惠优先 | 持有未耗尽的一次性额度（`period` 含 `one_time` 且 `remaining>0`）者优先 | —（不排除，仅降级） |
| 4 **额度优先** | 该模型**剩余可用额度最多**者优先 | —（仅降级） |
| 5 轮询 | 在第 4 层并列的账号之间按 `rotation[provider:model]` 游标轮转 | — |

**「剩余可用额度」的定义**（`model.Account.UsableQuotaForModel`）：

- 逐额度列**优先取 `available`**（上游的 `available_units`，「现在还能用多少」），缺省回落
  `remaining`（`total − used`）。逐列回落而不是整体二选一——老快照往往只有 `remaining`。
- 同一模型出现在多个订阅时**各列相加**：它们是同一个模型的不同额度池，本次请求可能落到
  其中任意一个，对调度而言可用量就是它们的和。
- 返回 `(0, false)` 表示「没有可用数值」（尚无快照 / 额度列无数值），调用方必须把它与
  **真正的 0 区分开**：0 的含义是「已用完」，而「没有数值」只是「还不知道」。排序里前者
  记 `-1`，落在任何有数值者之后，但彼此之间照样并列轮转。

**为什么是「最大值并列组 + 组内轮转」，而不是「永远取最大的那一个」**：

额度快照是**按账号缓存**的（`quota.QuotaCacheTTL = 15s`），且只在账号被使用时才异步刷新
（`Engine.success → fireRefresh`）。因此同一份数值会在一段时间里保持不变，若严格取唯一最大值，
这段时间内全部流量都会压在那一个账号上。取「最大值并列组 + 组内轮转」保住了「额度多的先用」
的方向，又不会把流量压在一个号上；**同规格账号（同一套餐、剩余量相同）会整体并列，行为与
原来的纯轮询完全一致**——这也是最常见的生产形态，所以这次改动对它的净值是零。

**已知取舍（未采纳的方案）**：按剩余量加权的平滑加权轮询（weight = remaining）能让大号按比例
多服务、小号永不饿死，且对快照陈旧不敏感；但它不满足「优先使用剩余更多的账号」这一字面要求
（小号会在有大号可用时仍被选中）。当前按用户要求实现额度优先，若在线观察到「同一个号被连打」
再评估切换。

**不参与排序的因素**（有意为之）：额度过期时间、线路/代理、`RateLimitStreak`、最近使用时间。
其中**过期时间**值得注意——`ModelAvailability` 与本次排序都不看 `expires_at`/`period_end`，
所以一个「计划已过期但快照仍显示有余额」的账号会被正常选中（是否要改是独立议题，不在本次范围）。

### 5.13 风控 405 冷却契约（Go 版增量，2026-09-20 新增）

上游用 **HTTP 405 + `unusual activity`** 表达风控拦截。它看的是**身份维度**——JWT 账号、
`X-Device-Mid` 设备指纹、出口 IP、请求头与 UA——**模型只是 body 里的一个字段**。因此
处置是**停整个账号**：换个模型照样被拦，按模型冷却只会把失败摊到别的模型上、把暴露时间拖长。

**触发判定（必须看 body，不能只看状态码）**

| 405 形态 | 含义 | 处置 |
|---|---|---|
| body 含 `unusual activity` / `blocked` / `risk` | 上游风控拦截 | **整号冷却**（本节） |
| 计费接口的重复查询（无风控文案） | 幂等，可安全忽略 | 忽略（`quota.go` 的幂等分支） |
| body 为空 / 无风控文案 | JWT 账号缺顶层 `system` 注入时上游就回 405（见 `body.go`、`upstream/request.go`）——**我方构造请求的缺陷** | 落「其余错误」兜底，**不冷却**（每个账号都会一样地失败） |

判定收口在 `model.IsRiskControlBody`（唯一权威实现，网关请求路径与 quota 计费路径共用）。

**冷却阶梯与升级点**

- 档位取自在线设定 `risk_cooling_steps`（逗号分隔秒数，默认 `300,900,3600`，环境变量
  `ZCODE_RISK_COOLING_STEPS` 只是默认值）。**第 N 次连续命中取第 N 档**。
- **连续次数超过档位数 → 置 `StatusInvalid` 且清空 `CoolingUntil`**：失效是「需人工介入」
  的终态，没有等待窗口。也就是说**档位数本身就是升级点**——默认三档 ⇒ 前三次冷却、第四次失效；
  想多给账号一次自证机会就多加一档，想更早封禁就减一档。
- **首次命中即冷却**，不要求连续两次确认：风控是明确的策略拒绝，且误判一次的代价（一个号闲
  几分钟）远低于漏判的代价（被标记的身份持续打上游，可能把拦截级别升上去，而共享这条 IP 的
  其它账号会一起遭殃）。
- 阶梯与限流阶梯**完全独立**（`RiskControlStreak` vs `RateLimitStreak`）：两者失败模式不同
  （限流等一会儿真的会好，风控往往要换身份），共用计数会互相清零干扰。字段用 `json:"-"`，
  不新增 `accounts.data` 的键。

**生效范围**：`IsSelectable` 为 false（全部模型不可选）+ 领取调度自动跳过（既有行为）。
不覆盖手动 `disabled`（管理员意图优先）。

**恢复出口（三条，缺一不可）**

1. **到期自动可再选**：`CoolingUntil` 过去后 `IsSelectable` 直接为真，无需显式清除。
2. **成功即归零**：冷却到期后成功调用一次 ⇒ `Status → active` 且 `RiskControlStreak → 0`
   （回到最低档）。所以只有「冷却一到期立刻又被拦」才会继续升级。
3. **人工恢复**：换凭据（`store.EditAccount` 的 `SetSecret` 分支）会清 `Status`/`LastError*`
   **并清零 `RiskControlStreak`**——不清的话，救回来的账号下一次命中就是老计数 + 1，会立刻
   又判失效，等于人工修复无效。失效后真正要做的是换线路 / 换设备指纹 / 换凭据。

**⚠️ 额度探测不得撤销风控封禁**：`quota.handleBillingResponse` 在探测成功时会把
`exhausted`/`invalid` 刷回 `active`（既有语义）。但额度接口与消息接口是**不同端点**，风控未必
同时命中——只探测通了额度就复活账号，一次轮询（15–60s）就把刚升上去的封禁悄悄抹掉。因此该处
加了窄守卫：`invalid` 且 `last_error_kind == risk_control` 时**早退、不复活**；其它成因的
`invalid`（如凭据失效）保持「额度通了即视为恢复」的既有行为。这条守卫**不让计费路径驱动阶梯**
（不递增计数、不冷却），只阻止它**覆盖**请求路径的判决。

**不做模型级冷却**：已论证无效（见本节开头）。**不让额度轮询驱动阶梯**：计费路径只记归类。

### 5.14 观测契约：诊断行、出口标签与截断计数（Go 版增量，2026-09-21 新增）

立项背景：2026-09-21 出现「上游流在约 300s 处被掐断」的故障（15 次 `unexpected EOF`，全部落在
301–308s），当时**无法只靠日志定论**——不知道是哪个账号、走了哪条出口线路、数据是在流还是静默。
根因分析见 `docs/analysis-flash-5min-stream-cut-20260921.md` §5（两个观测盲区）。

**诊断行（`[#]`）**：每请求恰好一条，收尾期输出，由 `web.Diag(reqID, msg)` 打印。

- 承载 `>>>`/`<<<`/`<!>` 三类行**结构上装不下**的字段：账号名、出口线路、请求体字节数、
  首字节延迟、最大 chunk 间隔、已收 usage、是否完整、总耗时、上游状态码、错误原文。
- 自带 reqID，可与同一请求的既有行 join；async 同时输出 `ticket=<UUID>`，两种检索习惯都保留。
- sync 落点：`internal/gateway/diagnostics.go`（`ReqDiag`）+ `engine.go` 的
  `RunMessages`/`tryAccount`/`deliverStream`/`finishDelivery`/`teeReader`。
- async 落点：`internal/asyncpool/pool.go` 的 `processTicket`/`attemptUpstream`/`attemptUpstreamOnce`/`forwardSSE`；
  **async 从此有了起始/完成与诊断行**（此前只有 `Warn` 与一条 `ReqErr`），并新增 6 位十六进制
  `shortID` 与 sync 的 reqID 同构。
- 首字节/最大间隔的采集点：sync 在 `teeReader.Read`，async 在 `forwardSSE` 的 `bufio.Scanner` 循环
  ——**两条路径都必须单独采集**（async 不走 teeReader）。非流式缓冲交付不经此路径，打印为 `-`。

**冻结既有行**：`>>>` / `<<<` / `<!>` 的格式串与文案**不得改动**（外部日志分析脚本按它们配对请求）。
`internal/web/logs_test.go` 的 `TestLegacyLineFormatsFrozen` 是这条约定的守卫。

**`web.SetOut`**：日志 writer 可注入（`atomic.Value` + 定长结构体持有 `io.Writer`；**不要直接把
`io.Writer` 存进 `atomic.Value`**，具体类型不一致会 panic）。生产路径不调用它，行为与直接写 stdout 等价。
⚠️ 禁止在 store 持锁路径里同步写日志（stdout 阻塞会锁死 Store，参见 `store.logPersistFailure` 的脱锁处理）。

**出口标签 `Store.ProxyLabel(acc)`**：`ProxyID` 命中线路 → 线路名；仅手工 `ProxyURL` → `proxy:` + 掩码；
皆空 → `direct`；线路已删 → `proxy-id:<id>`。每次选号调用一次，不在逐 chunk 热路径。

**两条路径同口径**（v2.4.0-go 起）：async 的 `route` 取自 `Pool.clientFor(acc)` 的第二个返回值，
与 sync 的 `ProxyLabel` 语义一致——账号绑线路则报线路名，无代理/代理无效回退/开关强制直连则报
`direct`。标签由 `clientFor` 一手交出，保证读数与实际出口不可能不一致（不另起判据）。
守卫用例 `TestAsyncDiagRouteReportsAccountProxyLine`（绑了线路必须报线路名）与
`TestAsyncForceDirectIgnoresAccountProxyLine`（开关打开必须报 `direct` 且不出现线路名）。

要复现「sync 走线路 vs async 直连」这个归因对照时，用后台「Async 強制直連」开关把 async 臂
显式关掉即可（`async_force_direct`，见 §5.6）；两个方向都可复现，`route` 读数在两个方向上都如实。

**`Account.StreamTruncateCount`（`json:"-"`）**：累计被上游中途掐断的次数。

- **只增不清**：成功不归零、换凭据不归零。「断流是否集中在某账号/某线路」要用累计值比较。
  ⚠️ 不要改成「连续次数 + 成功归零」：sync 的 `e.success(acc)` 在 `deliverStream` 读流**之前**调用，
  会先把计数清掉。
- **不参与任何状态机**：不冷却、不换号、不改 `Status`、不参与 `Select`、不进 `PublicView`、
  **不新增 `ErrorKind`**、不动 `accounts.data` 的 34 键契约（`TestJSONContractWithPython` 守）。
- 只统计**上游侧**掐断：判据是 `isClientGone(ctx)` 为假（客户端收到 finish_reason 就关流是常态，
  把它算进去会让计数失真）。
- 写入入口仅 `gateway.RecordStreamTruncate`；async 侧复用同一函数。

**业务码分支的 body 预览**：401/403、402、3010、529/1305、1005、3007 等分支的 `[~]` 行统一追加
`gateway.ErrorDetail(body)`（空 body 不留孤立冒号）。此前只有通用分支带预览，业务码在日志里不可见。

### 5.15 线路断流熔断、账号短回避与短流探测（Go 版增量，2026-09-22 新增）

立项依据：2026-09-22 事故定论（`docs/analysis-flash-5min-stream-cut-20260921.md` 09-22 附录）——
DSH 会话 3 连切全部落在 mihomo-zai-024 **同一账号同一线路**（诊断行 `trunc_total` 1→2→3 铁证），
成因是「中途断流不标账号」+「额度优先黏住最富账号」；当天其余 9 条线路 10 次 >300s 长流全部
成功，墙是单线路的 ~300s 连接时长上限。

**线路级熔断**（`gateway.RecordUpstreamTruncate`，sync/async 两路唯一入口）：

- 计数按**线路**（proxy profile ID）聚合，不按账号：多账号可共享一条线路，责任也在线路——
  按账号聚既命不中真凶，又会把上游的墙变成对账号的惩罚（与 405/503 的教训同源）。
  内存态（`store.BumpLineTruncate/ResetLineTruncate/LineTruncateStats`），重启归零无害。
- **「连续」的复位点是完整成功交付**（`finishDelivery` 成功分支 / async `forwardSSE` 成功
  路径调 `gateway.ResetLineTruncate`），**不能**放进 `MarkSuccess`——engine 的 success 在流
  开始读取之前调用，「每次正常开头、~300s 被切」的链会刚复位又被计入，永远凑不满连击。
- 达到在线设定 `line_truncate_strikes`（默认 3，0=关闭）→ `PurgeProxyProfiles` 移除线路并
  改派绑定账号（与 proxy-health 同一套原子路径），日志 `[~] line-guard ...`；线路删除时
  计数条目一并清理。
- 账号级 `StreamTruncateCount` 语义不变：纯观测、只增不清、不驱动状态机（§5.14 契约保持）。

**账号短回避**（仅选号层软过滤，在线设定 `line_truncate_avoid_seconds`，默认 60，0=关闭）：

- 断流时写 `Account.TruncateAvoidUntil`（`json:"-"`，Unix 秒；Clone 复制；不进 34 键契约）。
- `Select` 第 1 层后软过滤：被回避账号只在**池内还有别的可选账号**时被剔除；全部被回避则
  不过滤——软过滤永远不能让 Select 选不出号。它不是冷却：不写 Status/CoolingUntil/
  last_error、不进面板、到期自动失效、成功不延长。
- 动机：客户端 TRANSPORT 重试 ~2s 后原样重放，额度优先会再次选中同一「最富」账号；回避让
  下一次重试自然换线，比 N 连击熔断更早止血。

**断流即额度刷新**：断流分支调 `fireRefresh(acc)`（async 池新增 `OnQuotaRefresh` 钩子，
main 接线 `qs.FetchQuota`）——断流账号的额度读数停在旧值，会以「幽灵最富」持续黏住选号。
副作用已知且可接受：刷新失败记 `last_error=quota_query_failed`；计费面 401/403 会正确判
invalid（本就是期望行为）。

**手动短流探测**（`POST /admin/api/proxies/{id}/stream-test`，后台线路页按钮）：

- 借该线路绑定的启用 JWT 账号发一条最小流式请求（flash、max_tokens=16、effort=low、
  zcode_system 注入齐全），总预算 30s，出站走 `proxy.TransportForTimeout(url, 20s)`。
- 判定：不可达 / 预算内无数据行（疑似缓冲）/ **流被中途掐断** / 缺 message_stop /
  完整通过（附首数据延迟与耗时）；**任何完整业务响应（401/429/3007 等）判连通性通过**——
  它证明线路能完整承载请求+响应，验证码挑战不消耗求解。
- ⚠️ 能力边界：探测预算 30s，**测不出 300s 量级的连接时长上限**——那由熔断用真实流量兜底。
  探测是手动运维工具（每次都是真实上游请求），不进周期巡检；**只读**：不写账号状态、
  不记断流计数、不入用量账。
- async 路径断流记录修掉一个缺陷：此前 `diag==nil` 时整段漏记（诊断行只是展示，账号计数
  与线路熔断必须照常发生）。

## 6. 里程碑

### M0 骨架 + 数据层
- [x] go.mod / 目录骨架 / config（全部 `ZCODE_*` 环境变量）— `internal/config/config.go`
- [x] model.Account + 状态机 + 模型可用性 + JSON 契约单测 — `internal/model/`
- [x] store：SQLite 单连接 + 密钥引导（随机生成、`zcode` 轮换）+ 轮询游标 + 代理 + 导入导出 + 单测 — `internal/store/`
- [ ] **既有库兼容验收**：用真实 `data/accounts.db`（含存量账号）打开 → 账号/设置完整可读、迁移幂等、写回后再次读取一致
  （原「与 Python 版互读」目标已随 Python 侧退休改写；`-race` 与全量测试已在 CI 覆盖，真实库的打开仍待真机验证）
### M1 网关核心
- [x] build_request（头 + zcode_system 注入 + 剔除表 + 客户端头过滤）— `internal/upstream/`
- [x] 请求整形 + 错误分类链 + 模型白名单 + UsageCollector（含单测）— `internal/gateway/{body,classify,usage}.go`
- [x] captcha 管理器（配置缓存 10min / 人工回填 45s / Solver 接口；浏览器池留 M4）— `internal/captcha/`
- [x] 鉴权：网关 fail-closed + 后台限速（10 次/5min）— `internal/auth/`；终端日志 — `internal/web/`
- [x] /v1/messages 选号循环（engine.go）+ JSON/SSE 字节级透传（handler.go）
- [x] /v1/models（先按 Python 版形态，M4 再扩展双兼容超集）
- [x] httptest e2e：鉴权、透传、白名单、401/402/429 码族/3010/500 透传、1005、JWT 注入、验证码 503（13 组用例）
- [ ] **验收**：真实账号非流式 + 流式各打通一次（需验证码：待 M4 浏览器池或 M2 人工回填端点）
### M2 Admin API + 鉴权 + SPA
- [x] auth（Bearer + 限速）、admin_api 全部端点、embed dist + catch-all
- [ ] **验收**：浏览器完整走一遍后台 UI（登录/账号/代理/设置/验证码页）
  （限速单测已由 `TestVerifyAdminKeyLimitsFailures` / `TestVerifyAdminKeySuccessResetsFailures` 覆盖）
### M3 额度监控 + async
- [x] fetch_quota（解析 + 多订阅合并 + 15s 缓存 + inflight 去重 + 清理）+ 后台 monitor
- [x] /async/v1/messages（ticket 全语义）
- [x] **验收**：移植 test_quota / test_usage / test_async_pool 全部用例
### M4 OpenAI 兼容层（Go 版增量，见 §5.7）
- [x] 请求转换：system 归并、content blocks、图片、tools/tool_calls/tool_result、stop_sequences
- [x] 响应转换：非流式 JSON + 流式 SSE 重编码 + stop_reason/usage 映射
- [x] /v1/models 扩展为双兼容超集
- [x] **验收**：§5.7 每条映射至少一个单测（convert 10 + respond/stream 9 + e2e 5）；
  httptest 全链路覆盖非流式、流式、一次工具调用三种场景（openai 官方客户端真机
  跑通待真实账号环境，与 M0/M1 验收合并执行）
### M5 验证码（高风险，单独攻坚）
- [x] rod 池（复用 cloakbrowser 二进制）+ manager（缓存/人工回填/冷却）— `internal/captcha/{pool,browser_solver,solve}.go`
- [ ] **验收**：真实账号连续 20 次 JWT 请求全部自动通过（无 F001）；
  失败注入测试（超时/崩溃替换/迟到 token 不误投）**已由单测覆盖**（pool_test.go 12 组）
### M6 OAuth + 代理 + CLI + 交付（代码完成，待真机验收）
- [x] OAuth 登录链（internal/oauth + adminapi login 端点 + CLI login）、账号代理出口
  （internal/proxy：http/https CONNECT + 手写 socks4/4a/5/5h，Transport 缓存；引擎与 quota 已接线）、
  CLI 子命令（serve/login/add-account/accounts/remove-account/quota/status/set-admin-key/export/import）、
  Release CI（原 Dockerfile/compose 方案 2026-09-11 放弃，见范围一节）
- [ ] **验收**：Release CI 产物可运行；`-race` 下全测试通过；两版本交替使用同一 db 无异常
### M7 `/v1/responses` 端点（代码完成，待真机验收）
- [x] 请求/响应/流式转换 + 状态化划界（`previous_response_id` v1 先 400）—
  `internal/openai/{responses,responses_stream}.go`（8 组单测 + 2 组 e2e）
- [ ] **验收**：Codex CLI 指向网关完成一次完整会话（无状态模式）
### M8 套餐自动领取（代码完成，待真机验收）
- [x] claim 核心链（preview 解析 / claim 业务码 / 3007 换码重试）+ 激活事件上报 —
  `internal/claim/`（8 组单测对照 Python tests/test_claim.py）
- [x] Admin API `/claim/preview` + `/claim` + 入池自动领取触发点（批量添加 / OAuth / CLI login）
  + 前端按钮（工具栏全量 + JWT 账号行内单账号）
- [x] **验收**：单测覆盖业务码映射与 3007 换码重试语义（claim_test.go 8 组）
- [ ] **验收**：真机领取一次成功（billing/preview + claim + 激活上报全链路）

### M9 已知缺陷修复（2026-09-15 完成，仅剩 1 项待评估）
以下为代码审查确认的行为问题，**已全部修复**（每项附回归测试）：

- [x] `asyncpool` 错误分类与 engine 分歧 → 抽出 `gateway.MarkAccount` / `MarkModelExhausted` /
  `IsQuotaExhaustedCode` 共用，asyncpool 现按同一顺序分类 401/402/3010/429 码族/503；
  新增 `TestQuotaExhaustedCodeMarksModelNotCooling`、`TestUnauthorizedMarksInvalid`、
  `TestConcurrencyLimitKeepsAccountState`
- [x] `BrowserSolver.Solve` 持锁跨 `pool.Solve` → 锁只保护池的选取与重建，求解在锁外执行；
  配置变更时以 `retired` 标记延迟关闭旧池，避免中止在途求解；
  新增 `TestBrowserSolverConcurrentSolvesDoNotSerialize`（验证 n 路并发）、
  `TestBrowserSolverConfigChangeDoesNotAbortInFlight`
- [x] async ticket 逾时不投递终止事件 → 补发 `ticket_timeout` 错误事件；
  新增 `TestTicketTimeoutEmitsErrorEvent`
- [x] `include_usage` 外泄到上游 → 不再写入上游请求体，handler 改从原始 OpenAI 请求读取；
  测试改为 `TestStreamOptionsIncludeUsageNotForwarded`
- [x] `responses_stream` 的 `item_id` 不一致 → 统一取 function_call item 的 id；
  `TestResponsesStreamEvents` 增加 id 一致性断言
- [x] socks5 IPv6 回退 → 本地解析优先 IPv4，仅有 IPv6 时以 ATYP=0x04 发送；
  新增 `TestSocks5LocalResolveFallsBackToIPv6`
- [x] `browserdl.extractTarGz` 无 symlink 分支 → 支持 `TypeSymlink`（限制链接目标在解包目录内）
  与 `TypeLink`，未知类型记日志而非静默丢弃；新增 `TestExtractTarGzPreservesSymlink`、
  `TestExtractTarGzRejectsEscapingSymlink`
- [x] `quota` 代理回退无日志 → 补 `web.Warn`
- [x] `go.mod` 将 `go-rod/rod` 标为 indirect → `go mod tidy` 修正，并补齐 go.sum 缺失条目
- [x] `gofmt` 未覆盖 → 全部 62 个 Go 档已格式化

**待评估（未修改）**：
- [ ] 后台限速以 `RemoteAddr` 为键、不信任 `X-Forwarded-For`（`auth.go:129-135`）。
  这是**刻意的安全取舍**（信任 `X-Forwarded-For` 会让攻击者伪造头绕过限速），
  本次仅吸收上游核心修复，不引入其部署脚本；后台限制内网访问的部署说明另行维护。
  若确需反代支持，应改为显式配置可信代理列表，而非无条件信任该头。

### M10 工具、思考与真实客户端兼容（2026-09-15）
- [x] 合入上游 7675309 核心修复，保留本仓库 Docker/GHCR/数据卷及思考功能。
- [x] 工具控制参数共用转换与校验；初版思考预算映射在 M11 按模型原生能力纠正。
- [x] Responses 完整项目生命周期、交错工具流与异常终止；Chat 初始工具 input 与断流错误。
- [x] HTTP 回归覆盖工具结果闭环、思考档位、错误参数、流式项目关联。
- [x] 官方 OpenAI Python SDK 2.30.0 与 Pi 0.85.1 适配器通过本地 mock 上游闭环，测试见 client_sdk_test.go。
- [ ] 真实 Z.AI 账号与实际 Pi 会话在线验收（离线 SDK 回归不代替上游能力验证）。

### M11 GLM-5.3 原生 max 热修复（v2.0.4-go）
- [x] 用 HTTP 回归复现 Flash + max 被本地白名单错误拒绝。
- [x] 对照 Z.AI 官方模型及深度思考文档，按 low/high/max 和 Coding Plan 别名实现转换。
- [x] 移除自定义固定预算与 minimum max_tokens=2048 限制，保留显式预算与输出上限校验。
- [x] Pi 示例启用两种模型的原生 max，并隐藏不支持的关闭思考选项。
- [x] Python/Pi SDK 回归覆盖两种模型、high/max 与两种请求格式，断言真正上行的 effort。

### M12 限流重试与递进冷却（v2.0.5-go）
- [x] 三类同账号重试预算拆成独立计数（`attemptBudget`），3010 不再被验证码重试挤掉等待次数，
  延迟也不再取错档位；`TestCaptchaRetryDoesNotConsumeBusyBudget` 钉住该不变式。
- [x] 瞬时限流改为**先原地重试 1 次**（1s ±20% 抖动，`JitteredDelay`）再用尽冷却换号；
  等待期间 ctx 取消即放弃（`sleepCtx`）。
- [x] 冷却按连续被限流次数递进 30s → 60s → 120s → `COOLING_SECONDS`；
  `MarkRateLimited` / `ResetRateLimitStreak` 导出，sync 与 async 两条路径共用。
- [x] 连续计数以 `json:"-"` 挂在 Account 上（当时为避免破坏 Python 互读契约而不新增键；
  该理由已在 M13 重新评估——Python 侧已退休，`accounts.data` 开始接纳 Go 增量键）。
- [x] 回归：`TestTransientRateLimitRetrySucceedsInPlace`、`TestTransientRateLimitCoolingEscalates`
  及 asyncpool 同名两例。
- [x] 1302／1305 的窗口量级在线验证 —— 2026-09-20 定论：**两者根本不同类，不该共用一条路**。
  1302 是账户维度速率限制（API 侧），继续走递进冷却；1305 是**平台服务过载**（API 侧，
  「与单一账户的调用行为无直接关系」），已拆出独立分支：原地退避 1s/3s，用尽后原样透传，
  既不换号也不冷却账号。实测 z.ai 在 Anthropic 兼容面用 HTTP 529 承载 1305，
  因此判据是 `IsUpstreamOverload`（状态码 529 **或**业务码 1305），不是 `== 429`。

### M13 账户身份、设备指纹与领取状态（v2.0.6-go）
- [x] 借鉴 zcode-switch（`pjpv/zcode-switch`，Tauri 桌面多账号切换器）的 autoClaim 设计，
  补齐领取状态落盘、冷却分档与单槽串行闸门；契约见 §5.9.1。
- [x] 保住上游在成功与 1005 响应里给出的 `data.plan.ends_at`（此前只取了错误文案），
  落到 `Account.claim.next_at`；`ClaimError` 携带 code/nextAt 供调用方使用。
- [x] 领取状态落盘（`Account.claim`）：长驻服务重启后不再把刚领过的账号重领一遍。
- [x] 身份与凭据解耦：新增 `user_id`，入池判重按 user_id → email → 凭据 三轮优先级遍历；
  `saveOAuthAccount` 提前传入 email（否则对"重新登录"这个主场景无效）。契约见 §5.10。
- [x] 每账号设备指纹 `virtual_device_mid`（此前全局一份），四处注入点改走
  `Account.DeviceMidOr(config.DeviceMid())`；`model` 不依赖 `config`。
- [x] 存量一次性迁移（幂等）：回填 mid 与 user_id，不删除/合并任何存量账号。
- [x] `accounts.data` 契约测试同步（29 → 32 键），并钉住 `claim` 的嵌套键名。
- [x] 回归：`TestAddAccountDedupByUserIDAcrossTokenRefresh`、
  `TestAddAccountDedupPrefersUserIDOverEmail`、`TestMigrationBackfillsIdentityAndIsIdempotent`、
  `TestClaimSurfacesUpstreamNextAt`、`TestApplyClaimOutcomePersists`、`TestClaimGateIsExclusive` 等。
- [ ] 存量迁移后在线观察：上游是否把 `X-Device-Mid` 变更识别为「换了台机器」。
- [ ] 真实账号领取一次，确认 `claim.next_at` 与上游 `ends_at` 一致。

### M14 登录即可选定出站线路（v2.0.6-go）
- [x] `POST /login/start` 接受 `proxy_id`（线路）或 `proxy_url`（裸地址），
  非法/未知一律提前 400；契约见 §5.11。
- [x] `oauth.ExchangeCode` / `ExchangeAPIKey` 及其内部请求全部按会话代理出站；
  未指定时保留环境变量 `HTTP_PROXY` 语义。
- [x] 代理地址脱敏（`proxy.MaskURL`）后才进错误文案与后台回显，密码不外泄。
- [x] 登录成功后线路写入账号，且**发生在** API Key 兑换 / 额度刷新 / 自动领取之前。
- [x] 回归：真发 CONNECT 到假代理、非法代理提前报错、脱敏、`resolveLoginProxy` 分支、
  `/admin/api/login/start` 回显与未知线路 400。
- [ ] 真实线路（http / socks5）在线验收：经代理完成一次完整 OAuth 登录。

### M15 领取设定与每日定时（v2.0.7-go）
- [x] 领取设定进后台「系統設定」页：入池自动领取开关、三个冷却时长在线改，
  环境变量降级为默认值；契约见 §5.9.2。
- [x] 每日定时领取（`ClaimScheduler`）：默认关闭、时间可配（默认 23:00、本地时区），
  尊重 `claim.next_at`、错过不补跑、批量尊重串行闸门。
- [x] 回归：store 访问器回退与钳制、设置 API 往返与非法值、`shouldFireClaim` 判定、
  开关关闭零动作、定时批量跳过冷却账号。
- [x] 修掉 CI `-race` 抓到的领取状态竞态（`Claim` 的读写与序列化统一走 `claimMu`，
  见 §5.9.1）；回归 `TestClaimStateConcurrentAccess`。
- [ ] 在线观察定时批量的上游节奏（账号较多时 1s 间隔是否合适）。

### M16 平台过载分支与账号错误归类（v2.2.0-go）
- [x] 529 / `code=1305` 平台过载分支：排在 429 之前（否则官方表里的 429+1305 会误入限流冷却），
  退避档位 `OverloadRetryDelays`（1s/3s，±20% 抖动）、独立预算 `attemptBudget.overload`。
- [x] 账号「最近错误」归类：`last_error_kind` / `last_error_at`（`model.ErrorKind*` 共 12 项），
  `MarkAccount` / `MarkModelExhausted` / `bumpFail` 全部强制传 kind，漏传编译失败。
- [x] 上游错误日志补业务码与截断预览（`gateway.ErrorPreview`，200 字符、按 rune 截断）——
  此前只记状态码，1305 与风控 405 这类「关键信息在 body 里」的失败在日志里完全不可见。
- [x] 后台账号页新增「最近錯誤」列与错误类型筛選（前端本地过滤，含「帳號故障」聚合项）。
- [x] 顺手修 `quota.go` 的 405 判定：原来只看状态码，风控 405 被当成「重复查询」静默吞掉。

### M17 额度优先调度（未发版）
- [x] `model.Account.UsableQuotaForModel`：汇总请求模型当前可用额度（逐列优先取 `available`，
  回落 `remaining`；同模型多订阅相加），并把「没有数值」与「额度为 0」严格区分开。
- [x] `store.Select` 增至五层：可选 → 模型分档 → 优惠额度优先 → **额度优先** → 并列组内轮询；
  契约见 §5.12。
- [x] 回归：`TestUsableQuotaForModel`、`TestSelectPrefersMostRemainingQuota`、
  `TestSelectEqualQuotaStillRotates`、`TestSelectQuotaNumbersRankAboveUnknown`、
  `TestSelectWithoutModelRotates`；原有 `TestSelectRotationAndModelFilter`、
  `TestSelectPromoAccountsFirst` 不改而动（它们本来就是额度并列）。
- [ ] 在线观察：额度快照 15s TTL 下的实际分摊是否均匀。若出现「同一个号被连打到下一次
  刷新」，改评估按剩余量加权的平滑加权轮询（见 §5.12 的取舍说明）。

### M18 风控 405 递进冷却（未发版）
- [x] 判定收口：新建 `model.IsRiskControlBody`（唯一权威实现），`quota` 私有的
  `isRiskControlBody` 改为调用它；契约见 §5.13。
- [x] 同步与异步两条请求路径各加一条 405 风控分支（插在 503 与「其余错误」之间，405 与
  所有既有分支条件互斥）：`MarkRiskControl` 递进冷却，超限置 invalid。
- [x] 阶梯做成在线设定 `risk_cooling_steps`（逗号分隔秒数）+ 后台「風控冷卻」卡片；
  非法值 PUT 层 400、超大值钳到 7 天、存储损坏回退默认。
- [x] ⚠️ 修掉一个会让本特性失效的漏洞：`quota` 的额度探测成功会把 `invalid` 刷回 `active`，
  一次轮询就能悄悄撤销风控封禁。加窄守卫（只对 `last_error_kind=risk_control` 的失效早退）。
- [x] 新错误归类 `risk_control`（第 13 项，算账号故障）；`RiskControlStreak` 用 `json:"-"`，
  不新增 `accounts.data` 的键；换凭据时一并清零。
- [x] 回归：`TestIsRiskControlBody`、`TestMarkRiskControlLadder`、
  `TestRiskCoolingStepsDriveEscalationPoint`、`TestRiskCoolingStepsFallback`、
  `Test405RiskControlCoolsAndSwitches`、`Test405WithoutRiskBodyDoesNotCool`、
  `Test405RiskControlInAsyncPool`、`Test405WithoutRiskBodyInAsyncPool`、
  `TestRiskCoolingStepsSetting`、`TestEditSecretResetsRiskControlStreak`、
  `TestBillingSuccessKeepsRiskControlInvalid`、`TestRiskCoolingStepsAPI`；
  扩 `TestErrorKindRecordedPerBranch` 两行（405 风控 / 405 非风控）。
- [ ] 在线观察：风控文案是否只有 `unusual activity` 一族；升级阈值（= 档位数）默认三档是否合适。

### M19 观测数据补齐（2026-09-21）
- [x] `internal/web`：writer 可注入 + `[#]` 诊断原语 — `logs.go`（`SetOut`/`Diag`/`writerHolder`）
- [x] `ReqDiag` 容器与格式化，贯穿 sync 全链路（请求体画像 / 出口身份 / 首字节 / 最大间隔 / usage / 耗时）
- [x] `Store.ProxyLabel`：账号 → 线路名/掩码/直连，三态 + 线路已删回落
- [x] `Account.StreamTruncateCount`（`json:"-"`，累计、不驱动状态机）+ `gateway.RecordStreamTruncate`
- [x] async 对称：`shortID`、诊断行、scanner 侧流形采集、上游侧掐断计数
- [x] 业务码分支补 `ErrorDetail`（sync + async 同口径）
- [x] 守卫用例：既有三类行格式冻结、诊断行字段、首字节/间隔、累计语义、线路标签、Clone 齐全性
- [ ] **验收**：下一次 flash 长流场景只用日志即可判定 300s 墙归属（判定矩阵见分析报告 §4.1）

### M20 线路断流熔断、账号短回避与短流探测（2026-09-22，v2.5.0-go）
- [x] store：线路级断流计数（Bump/Reset/Stats，内存态；线路删除时清理条目）+
  在线设定访问器（`line_truncate_strikes` / `line_truncate_avoid_seconds`，env 只是默认值）。
- [x] gateway 共享入口 `RecordUpstreamTruncate`：账号计数（语义不变）+ 回避写入 +
  线路连击与熔断（PurgeProxyProfiles 原子移除+改派，`[~] line-guard` 日志）；
  sync（finishDelivery）与 async（forwardSSE）两路统一走它，并修掉 async
  `diag==nil` 漏记缺陷。契约见 §5.15。
- [x] 成功复位在「完整成功交付」处（engine 成功分支 / async 成功路径），不进 MarkSuccess。
- [x] `Select` 第 1 层后加断流短回避软过滤（全池被回避不过滤）；`TruncateAvoidUntil`
  `json:"-"` + Clone 复制 + 守卫用例。
- [x] 断流即额度刷新：engine `fireRefresh` + async 新增 `OnQuotaRefresh` 钩子（main 接线）。
- [x] 可观测性：PublicView 加 `stream_truncate_count`；`GET /admin/api/proxies` 合并
  `truncate_streak`/`truncate_total`；前端线路页显示断流徽标。
- [x] 手动短流探测端点 + 线路页「流式探测」按钮（判定与能力边界见 §5.15）。
- [x] 回归：`TestLineTruncateStrikesPurgeLineAndReassign`、`TestLineTruncateStrikesZeroDisables`、
  `TestLineTruncateSuccessResetsStreak`、`TestTruncateAvoidsAccountInSelection`（用额度差构造
  黏性，证明回避而非轮询在换号）、`TestTruncateFiresQuotaRefresh`、async 同名三例 +
  `TestAsyncForwardSSENilDiagStillRecords`、store 计数与软过滤两例、adminapi 设定往返与
  探测四分支、`TestCloneCopiesTruncateAvoidUntil`。
- [ ] 在线观察：熔断误杀率（好线路被 3 次偶发断流移除的频率）；阈值 3 / 回避 60s 是否合适。

## 7. 测试策略

- 单测**逐个移植** Python 版 `tests/`（错误分类、池协议、路由白名单、quota 合并、oauth、usage、鉴权引导），
  保持同名用例语义，便于两边对照。
- OpenAI 转换层：§5.7 每条映射一行单测；流式重编码按事件序列断言输出 chunk 序列；
  最终用 openai 官方客户端（python）指向网关做真客户端回归。
- httptest 起完整服务打 mock 上游做端到端；SSE 用 `curl -N` 与 Python 版逐字节对比分块行为。
- 日志行可断言的：`web.SetOut(w)` 注入 writer（默认 stdout），测试用管道/缓冲捕获后断言行内容。
  `TestLegacyLineFormatsFrozen` 冻结 `>>>`/`<<<`/`<!>` 的形态；诊断行 `[#]` 的字段、
  截断计数语义、首字节与最大间隔、`ProxyLabel` 三态各有专门用例（§5.14）。
- 全部测试在 `-race` 下通过（本机无 gcc，`-race` 只能由 CI 跑）。
- 数据互通夹具：把一份脱敏 `accounts.db` 提交到 `testdata/` 作为固定夹具。

## 8. 风险与对策

| 风险 | 对策 |
|------|------|
| rod 驱动 cloakbrowser 二进制过不了风控 | 启动参数逐项对齐；不行则退 `--headless=new` / go-rod/stealth；最终兜底 = 人工回填（功能不中断） |
| OpenAI↔Anthropic 转换长尾（工具调用分片、多 system、图片 URL） | §5.7 映射表逐行单测；不支持的行为（n>1 等）显式 400 并在 README 声明 |
| SSE 分块/flush 行为与 Python 不一致 | httptest + curl -N 字节级对照；flush 每个 chunk |
| Account JSON 字段错漏导致 db 互读失败 | M0 就做互通验收；结构体 tag 对照 dataclass 逐一 review |
| Go 无 jsdom 兜底 | 接受——jsdom 本已被风控判死；人工回填为最终兜底 |
| rod 版本 API 变动 | go.mod 锁定 minor 版本 |
| 开启 thinking 后多轮会话历史缺 thinking 块 | Anthropic 语义下续聊需回灌上一轮 thinking（含签名）；OpenAI 形态客户端只回传正文与 `reasoning_content`，缺签名无法合规回灌。当前策略：历史不回灌、按上游实际行为验收；若上游强制要求，则改为仅在客户端显式传 Anthropic `thinking` 时开启，或增加开关 |
| 把兼容层预算映射误当成模型能力 | 以模型官方文档为依据；GLM-5.3 仅原生 low/high/max 且强制思考，effort 与输出上限独立。显式 Anthropic 预算仍单独校验，但不能据此限制模型 effort |

## 9. 交付形态

- `go build ./cmd/zcode2api` → 单二进制（前端已 embed），仅 Chromium 运行库为外部依赖。
- Release CI：推 `v*` tag → GitHub Actions 交叉编译 linux/darwin/windows × amd64/arm64
  （CGO_ENABLED=0，`-trimpath -ldflags="-s -w"`）→ 上传 GitHub Releases。
  （2026-09-11 曾定案放弃 Docker 镜像方案：裸二进制 + systemd 更简单，Chromium 由部署机自备。
  **2026-09-15 翻回**：① 补丁 Chromium 的自动下载已是纯 Go 实现（`internal/captcha/browserdl.go`，
  net/http + archive/tar，无 curl/tar 等外部工具依赖），容器化不再需要把 Python 构建期依赖带进来；
  ② rod 启动参数已内置 `--no-sandbox`（`captcha/solve.go`），容器内无需额外旗标；
  ③ cloakbrowser 对 linux-x64 与 linux-arm64 都有预构建二进制，多架构镜像可行。
  保留 systemd 作为无 Docker 环境的替代方案。）
- 容器镜像（`Dockerfile` + `docker-compose.yml`）：debian:bookworm-slim 运行阶段，
  非 root uid 10001；`/data` 卷承载 `accounts.db`、`device_mid.txt` 与
  `CLOAKBROWSER_CACHE_DIR=/data/cloakbrowser`（device_mid 必须稳定，否则被上游当新设备）。
  **不可用 Alpine**：补丁 Chromium 是 glibc 构建，musl 下跑不起来（表现是 JWT 账号恒 503）。
- Python 版保留在仓库中直至 Go 版 M6 验收通过，届时再决定去留（不在本计划范围内）。
