# zcode2api Go 版重写计划

> 本仓库 `C:\Projects\zcode2api-go` 即 Go 重写仓库（原 `go/` 子目录已上提到仓库根，
> Python 版实验工作区内容已移除）。计划与行为契约参照主仓库
> `C:\Projects\zcode2api` 的 Python 版（`app/` + `main.py`）与 `HANDOFF.md`。
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

- [ ] `/v1/messages` 网关：多账号轮询、SSE/JSON 流式透传、错误分类与自动换号
- [ ] `/v1/models`（Anthropic / OpenAI 双兼容超集形态，见 §5.7）
- [ ] `/v1/chat/completions` **OpenAI（GPT）兼容层——Go 版增量功能**：请求/响应双向转换 +
  流式 SSE 重编码 + 工具调用，同步走网关引擎（详见 §5.7）
- [ ] `/v1/responses`（OpenAI Responses API，服务 Codex CLI 生态；**排期在 completions 验收之后**，划界见 §5.8）
- [ ] `/async/v1/messages`：ticket + SSE keepalive + 流中断终止语义（`_MidStreamError`）
- [ ] 账号状态机（active/exhausted/cooling/invalid/disabled）+ 按模型可用性调度
- [ ] 额度监控（`billing/balance` 解析、多订阅合并、15s 缓存 + 并发去重、后台周期刷新）
- [ ] 调度 token 统计（UsageCollector：SSE `message_start`/`message_delta`、JSON 顶层 usage）
- [ ] Admin API `/admin/api/*` 全部端点 + SPA 托管（`/admin/*` catch-all 回落 index.html）
- [ ] 鉴权：后台密钥（含单 IP 失败限速 10 次/5 分钟）+ 网关密钥（fail-closed）
- [ ] OAuth 登录（Z.AI 授权 → JWT 入池 → API Key 兑换链）
- [ ] 账号级出站代理（http/https/socks4/socks5/socks5h）+ 命名代理管理 + 出口探测
- [ ] 验证码：真实 Chromium 池（rod）+ 人工回填兜底
- [ ] SQLite 持久化（accounts + meta，WAL）与 **Python 版数据库互通**
- [ ] CLI 子命令（serve / login / add-account / accounts / remove-account / quota / status / set-admin-key / export / import）
- [ ] Dockerfile（多阶段：node 构建前端 → go 构建二进制 → 运行镜像仅含 Chromium 运行库）
- [ ] **套餐自动领取（Go 版增量，2026-09-10 后新增，Python 主仓已上线）**：billing/preview + billing/claim、
  激活事件上报、业务码翻译、3007 换码重试、入池自动领取（对照 Python 主仓 `app/claim.py` + `app/telemetry.py`，见 §5.9）

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
| 日志 | `log/slog` | 标准库；彩色终端输出按需自写 |
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
| `MAX_CAPTCHA_RETRIES` / `MAX_ACCOUNT_ATTEMPTS` | 3 / 5 |
| 3010 并发准入重试延迟 | 1s、2s（第 3 次失败原样回传 429） |
| 对外模型白名单 | `glm-5.3-flash`、`GLM-5.3`（normalize 后比对） |
| `MODEL_NAME_MAP` | 仅 `{"glm-5.3": "GLM-5.3"}` |
| 429 额度上限码族 | 1113、1308-1311、1313、1316-1321 → 标记该模型 exhausted；其余 429 → cooling |
| 业务码语义 | `1005`(HTTP 200)=当日额度用完；`3007`=验证码失效；`3010`=并发准入；F001=风控指纹拒绝 |
| 冷却 / 刷新 | `COOLING_SECONDS=300`、`QUOTA_REFRESH_INTERVAL=60`（0=关闭，运行中可改） |
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
5. 429 且 code∈额度上限码族 → 该模型 exhausted 换号；其余 429 → cooling 换号
6. 503 → cooling 换号
7. 其余 → **不做状态推断，原样透传上游响应**

### 5.3 上游请求

- JWT 账号 → `https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages`，`Authorization: Bearer <jwt>`，
  **必须注入 `zcode_system.json` 到顶层 `system`**（否则上游 405），并携带验证码头 + `X-Device-Mid`。
- API Key 账号 → `https://api.z.ai/api/anthropic/v1/messages`，`x-api-key` 头，无需验证码。
- 固定头：`anthropic-version: 2023-06-01`、`User-Agent: ZCode/3.7.7`、`X-ZCode-App-Version: 3.7.7`、
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
- 导出/导入格式（`version:1` + `providers.{name,mode,secret,disabled_models}`）保持一致。
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

- SSE 事件：`ticket`(pending) → `ready` → `data:` chunk… → `done`/`error`；每 10s `: keepalive`；总超时 300s。
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
| `tools` / `tool_choice` | tools → Anthropic tools（`parameters` → `input_schema`）；choice `auto/none` 透传语义、named → `{type:"tool",name}` |
| `n` | 仅支持 1，>1 返回 400 |
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
- UsageCollector 在重编码旁路照常解析 Anthropic 事件——账号调度统计不受转换影响。

### 5.8 `/v1/responses`（已规划，延后实现——决策：先 completions，后 Responses）

- 定位：服务 Codex CLI 等 Responses 生态客户端；复用 §5.7 的引擎与转换基建，增量约 300-500 行。
- v1 范围：`instructions` → system；`input`（字符串 / 类型化 item 数组：message、function_call、
  function_call_output）→ messages；扁平 `tools` → Anthropic tools；`max_output_tokens` → `max_tokens`；
  `reasoning.effort` → 上游 `output_config.effort`；输出端 text → `output_text`、tool_use → `function_call`
  item；流式重编码为 `response.*` 事件序列（`response.output_text.delta` 等）。
- **状态化划界**：无状态用法全支持（`store:false` + 每轮完整历史，Codex 默认即此）；
  带 `previous_response_id` 的请求 v1 返回明确 400；内存 LRU 回放列为后续可选增强，不阻塞。
- 排期：M4 的 completions 验收通过后启动，避免两个转换层并行开发。

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
  失败仅记日志不阻断）。device_mid 沿用本机持久化标识。
- 触发点：入池后（批量添加 / OAuth 完成 / CLI login）后台自动全量领取 +
  Admin API `GET /claim/preview`、`POST /claim`（account_ids 可选，冷却账号跳过）+
  前端账单页按钮（工具栏全量 + JWT 账号行内单账号）。
- 复用项：鉴权头与 quota 同源（`X-ZCode-App-Version`/`X-Platform`/`X-Device-Mid`）；
  请求走账号代理（与 Python 版 make_async_client 语义一致）；
  验证码经 M5 的 captcha manager 求解/人工回填。

## 6. 里程碑

### M0 骨架 + 数据层
- [x] go.mod / 目录骨架 / config（全部 `ZCODE_*` 环境变量）— `internal/config/config.go`
- [x] model.Account + 状态机 + 模型可用性 + JSON 契约单测 — `internal/model/`
- [x] store：SQLite 单连接 + 密钥引导（随机生成、`zcode` 轮换）+ 轮询游标 + 代理 + 导入导出 + 单测 — `internal/store/`
- [ ] **互通验收**：用 Python 版生成的真实 `data/accounts.db` 打开 → 账号/设置完整可读，Go 写回后 Python 版也能读
  （代码就绪；需在有 Go 工具链的设备执行 `go test ./...` 实测）
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
- [ ] auth（Bearer + 限速）、admin_api 全部端点、embed dist + catch-all
- [ ] **验收**：浏览器完整走一遍后台 UI（登录/账号/代理/设置/验证码页）；限速单测
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
  Dockerfile（多阶段：node 前端 → go 二进制 → bookworm-slim + Debian chromium）+ compose + README
- [ ] **验收**：`docker compose up -d --build` 一键起；`-race` 下全测试通过；两版本交替使用同一 db 无异常
### M7 `/v1/responses` 端点（代码完成，待真机验收）
- [x] 请求/响应/流式转换 + 状态化划界（`previous_response_id` v1 先 400）—
  `internal/openai/{responses,responses_stream}.go`（8 组单测 + 2 组 e2e）
- [ ] **验收**：Codex CLI 指向网关完成一次完整会话（无状态模式）
### M8 套餐自动领取（代码完成，待真机验收）
- [x] claim 核心链（preview 解析 / claim 业务码 / 3007 换码重试）+ 激活事件上报 —
  `internal/claim/`（8 组单测对照 Python tests/test_claim.py）
- [x] Admin API `/claim/preview` + `/claim` + 入池自动领取触发点（批量添加 / OAuth / CLI login）
  + 前端按钮（工具栏全量 + JWT 账号行内单账号）
- [ ] **验收**：单测覆盖业务码映射与重试语义（已完成）；真机领取一次成功（待真实账号环境）

## 7. 测试策略

- 单测**逐个移植** Python 版 `tests/`（错误分类、池协议、路由白名单、quota 合并、oauth、usage、鉴权引导），
  保持同名用例语义，便于两边对照。
- OpenAI 转换层：§5.7 每条映射一行单测；流式重编码按事件序列断言输出 chunk 序列；
  最终用 openai 官方客户端（python）指向网关做真客户端回归。
- httptest 起完整服务打 mock 上游做端到端；SSE 用 `curl -N` 与 Python 版逐字节对比分块行为。
- 全部测试在 `-race` 下通过。
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

## 9. 交付形态

- `go build ./cmd/zcode2api` → 单二进制（前端已 embed），仅 Chromium 运行库为外部依赖。
- Docker 镜像：`node:20-alpine` 构建前端 → `golang` 镜像构建二进制 → 运行层 `python:3.11-slim` 换成
  `debian:bookworm-slim` + Chromium 运行库 + cloakbrowser 预下载（构建期一行 Python）→ 无 Python/Node 运行时。
- Python 版保留在仓库中直至 Go 版 M6 验收通过，届时再决定去留（不在本计划范围内）。
