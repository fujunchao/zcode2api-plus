# 服务器 Docker 反代场景下的 start-plan 优化计划

- 日期：2026-10-28（Asia/Shanghai）
- 适用基线：zcode2api-plus go-rewrite（HEAD 0abb8a6，v2.11.2-go）
- 部署形态：服务器 Docker（单实例），前置 Nginx/Caddy 等反向代理，对外提供 OpenAI/Anthropic/Responses API
- 主要套餐：Start Plan（JWT）
- 依据：docs/analysis-zcode-api-reference-20261028.md、docs/analysis-zcode-api-start-plan-20261028.md，以及本项目既有的官方 3.14.5/3.15.3 wire 证据
- 性质：计划文档；只做方案与验收定义，不改代码、不跑本地测试。

---

## 0. 计划摘要

本计划围绕“**服务器反代 API + Start Plan 为主**”这一个前提，把可借鉴项收敛成 7 个工作流：

| 优先级 | 工作流 | 目标 | 预计 |
|---|---|---|---|
| **P0-A** | Start Plan system 组装对齐 | block 0 去 cache、补 dynamic 块、currentDate 合并、3 块封顶 | 4–5 天 |
| **P0-B** | 验证码热路径与韧性 | 预求解 token 池、配置 last-good、严格获取、CPU 治理 | 5–7 天 |
| **P0-C** | 会话血缘 + 翻译入口 cache 标记（先 observe） | 稳定 x-session-id、可解释的会话复用、cache 命中取数 | 5–7 天 |
| **P1-D** | Responses/Codex 兼容 | previous_response_id、custom/namespace/tool_search、流事件完整 | 7–10 天 |
| **P1-E** | credits 额度快照 | billing/preview + JWT age + 后台展示 | 3–4 天 |
| **P1-F** | 富模型目录 | /v1/models?client_version=pi | 1–2 天 |
| **P2-G** | 运维与安全 | 调试 dump、凭据加密、Docker/反代参数、soak | 3–5 天 |

**明确不做**：全局 OpenAI 官方默认 profile、高速卡、官方 off-peak、MCP 聚合、客户端签名、端点路由改写、TUI/Android、Web 聊天、多副本共享状态。原因见 §9。

建议节奏：**P0 两个迭代（约 2 周）先上线；P1 一个迭代（约 2–3 周）；P2 随发布补齐。** 单人开发约 6–8 周；两人并行可压到 3–4 周。

---

## 1. 前提、目标与边界

### 1.1 部署前提

- 单容器、单实例；数据目录固定 /data（accounts.db、device_mid.txt、Chromium 缓存）。
- 外层反向代理终止 TLS；容器端口只绑回环或内网。
- 下游客户端是 Claude Code、Codex、OpenAI SDK、各类 OpenAI/Anthropic 兼容工具，不是官方桌面 UI。
- 账号池以 JWT（Start Plan）为主；API Key 账号作为补充。
- 现有 Dockerfile 已非 root、已带 Chromium 运行库、已用 /meta 做 HEALTHCHECK；本计划在其上做增强，不重做交付形态。

### 1.2 目标

1. **Start Plan 上游外形更准**：system 3 块结构、block 0 缓存布局、dynamic 内容、currentDate 形态。
2. **验证码不拖慢首字节**：JWT 请求热路径取预解 token；配置故障不误开求解。
3. **会话连续性可测**：稳定 x-session-id；先观测，再决定账号黏性。
4. **Codex/Responses 可用**：状态化多轮与 custom 工具不再是硬 400。
5. **额度/模型信息可运营**：credits 快照、claimable plans、JWT age、Pi 富目录。
6. **Docker 反代可运维**：长流不被代理缓冲/超时截断；有调试与安全边界。

### 1.3 非目标（本计划不做）

- 不做全局 max_tokens=128000 / thinking enabled / effort max 的“官方桌面默认注入”。这是客户端生成参数，不是上游准入契约；详见 §9.1。
- 不做高速卡、官方 off-peak、MCP 聚合：它们不是 Start Plan 能力，且需要 coding-plan 双凭据；单独立项。
- 不做客户端签名 V4：Start Plan 明确不签名；只在错误分类里观察 VERIFY_*。
- 不做端点路由改写：Start Plan 固定 zcode-plan 端点；只对 API Key 账号另立观察项。
- 不做多副本共享会话/响应存储：SQLite 单写 + 进程锁；水平扩展需独立设计。

---

## 2. 现状基线与差距

| 能力 | 现状 | 差距 | 对应工作流 |
|---|---|---|---|
| Start Plan system | 2 个静态块 + 可选 Environment；block 0 带 cache | block 0 应无 cache；缺 dynamic 块；缺 currentDate；electron 回退多一个 capability 块 | P0-A |
| 验证码 | 单 token；浏览器 token 不缓存；请求内同步求解；配置 10 分钟、失败回落 enabled=true | 无预求解池；无 last-good；无 CPU 治理；高并发首字节慢 | P0-B |
| 会话 | 只认 x-session-id / metadata；无血缘推断；诊断有 session 哈希 | 不主动发 x-session-id 的客户端每次新会话；无法做 cache/黏性实验 | P0-C |
| Prompt cache | 只标 system 静态块；翻译入口消息无标记 | OpenAI/Responses 长对话无缓存断点 | P0-C |
| Responses | previous_response_id 400；只支持 function 工具 | Codex custom/namespace/tool_search 与多轮不兼容 | P1-D |
| 额度 | 只查 billing/balance；后台只刷新 JWT 账号 | 无 claimable plans、JWT age；API Key 账号无 coding 窗口 | P1-E |
| 模型目录 | /v1/models 基础 data[] | Pi/DSH 拿不到 context/max/vision/reasoning | P1-F |
| 运维 | Docker healthcheck、日志、优雅停机已有 | 缺调试 dump、凭据字段加密、反代长流参数文档、soak 基线 | P2-G |

> M0 的详细清单、指标定义与探针脚本见 `docs/plan-server-start-plan-m0-baseline-20261028.md`，采集入口 `scripts/m0/collect.sh`。

### 2.1 基线采集（M0）
- M0 当前状态：清单/指标/采集脚本已定义；本机历史日志离线基线见 docs/analysis-m0-baseline-20261028.md；M1 上线后需复测。


在动手前先用现有诊断采集 3–7 天（或一轮压测）基线：

- captcha_wait_ms p50/p95、captcha 失败率、空池率；
- 各入口 cache_read_input_tokens / 输入 token；
- 405、5xx、3012、captcha 503 比例；
- 同 session 的账号切换次数；
- Docker CPU/内存、Chromium 进程数、SSE 中断；
- 反代长流是否被缓冲/超时截断。

这些数据决定后续收益验收，不允许用“感觉更快”代替。

---

## 3. 工作流详细设计

### 3.1 P0-A：Start Plan system 组装对齐

#### 目标

把“网关自行补 system”的路径从当前的 2 静态块 + 可选 Environment，收敛成官方 3 块结构；不改变“调用者自带官方 system 则保留”的既有策略。

#### 设计

1. **block 0 去 cache**
   - 文件：internal/upstream/request.go::ZcodeSystemBlocks。
   - 复制静态块时，对 index 0 删除 cache_control；block 1 保留 ephemeral；Environment 块保留 ephemeral。
   - 不再只对 desktop 分支删；所有档案一致。

2. **补 dynamic 块**
   - 文件：internal/upstream/zcode_system.json、internal/upstream/systemprompt.go。
   - 新增 dynamicSections：beforeEnvironment（# Communicating with the user）与 afterEnvironment（# Context management）；Environment 仍运行时生成。
   - 新增 BuildFallbackSystem(body, profile)：
     - block 0：cli_prefix；
     - block 1：stable（保留现有 2313 B Desktop Context）；
     - block 2：两个换行 + beforeEnvironment + 两个换行 + environment + 两个换行 + afterEnvironment；
     - 不再把 # Gateway Desktop Capabilities 作为第 4 块；如需保留，并入 block 2 末尾，或仅在“非官方前缀且无 caller system”时作为 block 2 的可选段。
   - 调用方：internal/gateway/body.go::NormalizeBody 改为调用 BuildFallbackSystem。

3. **currentDate 合并**
   - 新增 upstream.InjectContextPrefix(body, now, loc)：
     - 只在网关自行补 system 的路径调用；
     - 只对首条 user 消息：若 content 是 string，转成 [currentDateBlock, textBlock]；若是数组，在前面插入 currentDateBlock；
     - currentDateBlock 文本为 system-reminder 包裹的 currentDate 段，格式对齐官方；
     - 如果首条 user content 已含 # currentDate，跳过；
     - 时区取配置 ZCODE_CLIENT_TIMEZONE（默认 Asia/Shanghai）。
   - 不新增独立 user 消息；不改变 MCS / 尾部 system 消息。

4. **可选：Interactive views 分支**
   - 官方 3.15.3 block 1 为 2594 B，我们 2313 B；差 ### Interactive views 的无目录 fallback 两行。
   - 若决定对齐，新增静态变体 stableSectionsWithInteractiveViews，仅在 SourceTitle == "electron" 且无可视化目录时使用；否则保持 2313。
   - 该子项可放 P1，不阻塞 P0-A 上线。

5. **powered-by 行**
   - 暂不写死；若要加，必须按真实账号类型生成 account:zai-start-plan/<model>，不要照 zcode-api 的 zai-api/bigmodel-api。

#### 验收

- golden 测试：3 块；block 0 无 cache；block 1/2 有 cache；block 2 含 # Communicating with the user 与 # Context management；总 block 数 ≤3。
- 调用者自带官方三块：原样保留，不重复注入。
- 第三方 system：合并后 ≤3 块；不出现 # Gateway Desktop Capabilities 第 4 块。
- 首条 user 消息 currentDate 只出现一次；已有 currentDate 不重复。
- 回归：现有 /v1/messages、/v1/chat/completions、/v1/responses、/async/v1/messages 全部通过；无 3012 新增（staging A/B 观测）。

#### 风险与回滚

- 风险：system 改动可能触发 3012；用 ZCODE_SYSTEM_PROFILE=legacy|3.16 开关，默认先 legacy 观察，再切 3.16。
- 回滚：切回 legacy；不需要数据迁移。

---

### 3.2 P0-B：验证码热路径与韧性

#### 目标

JWT 请求不再每个都同步等浏览器求解；池空时仍有界求解；配置故障不误开求解；Docker CPU/内存可控。

#### 设计

1. **Token 池（Manager 层）**
   - 文件：internal/captcha/captcha.go，新增 token_pool.go。
   - 结构：按 configKey 分桶，key = sceneId|region|prefix；token 记录 param/region/createdAt。
   - Take(ctx)：弹出未过期 token；池空时同步触发 1–2 个 solve（race），取最先成功；全失败按现有错误上抛。
   - 后台 refill：当池低于水位时补货；token 视为一次性，取走即删除；TTL 到期清理。
   - 配置变化：清空旧 key 的池；不跨 scene/region/prefix 复用。
   - Manager.Close 停止 refill goroutine；不持有进程。
2. **配置缓存**
   - CaptchaConfigCacheTTL 默认 10 分钟改为 1 小时（可 env 覆盖）。
   - 成功才写；保留 lastGoodConfig；失败不覆盖、不清空。
   - enabled=false / skip_model_request=true 语义不变。
3. **严格获取**
   - 保持“拿不到 token → 503，不裸发”；补测试固定。
   - 挑战重试仍按现有 3007/header/F001 逻辑；不无限循环。
4. **CPU governor（可选但建议）**
   - 读 /proc/stat（Linux）或 os.cpus() 累计 tick；按 CPU 水位调 refill 并发/池目标。
   - 默认上限 100%；Docker --cpus 限制下仍以容器可见 CPU 为准。
   - 空闲 120 秒缩容、900 秒停补；保留 1 个 token 供唤醒。
5. **Docker 资源**
   - 建议 ZCODE_CAPTCHA_BROWSER_WORKERS=2 起步（默认 1），按 pool 指标调。
   - 容器内存建议 ≥2 GB（Chromium + Go）；CPU ≥2 核；init: true 已有。
   - 保持 /data/cloakbrowser 卷持久化；首次下载 200MB 不重复。

#### 指标

- captcha_pool_size、captcha_take_total、captcha_empty_take_total、captcha_mint_total、captcha_mint_fail_total、captcha_wait_ms p50/p95。
- 日志字段：captcha_wait_ms、captcha_pool_size、captcha_empty_take、captcha_mint_fail；不打印 token。

#### 验收

- 池热时 p95 等待 < 200ms；冷启动有界（≤ solve timeout）。
- 并发 10 个 JWT 请求无重复 certifyId、无 503 裸发。
- 配置接口故障时 last-good 继续服务，不误开求解。
- 空闲 15 分钟后 CPU 回落，池缩到 1。

#### 风险与回滚

- 风险：预求解增加 CPU；token 一次性假设错误；池空时同步 solve 仍慢。
- 回滚：ZCODE_CAPTCHA_POOL_ENABLED=false，退回逐请求求解；配置 TTL 可回 10 分钟。
- 不改变 manual token 路径。

---

### 3.3 P0-C：会话血缘 + 翻译入口 cache 标记（先 observe）

#### 目标

让不主动发 x-session-id 的客户端也有稳定会话；为 prompt cache 与账号黏性实验提供正确输入；先只观察，不改调度。

#### 设计

1. **Resolver**
   - 新包 internal/session（或 internal/gateway/session）；纯内存，无持久化。
   - 显式头优先：x-opencode-session、x-claude-code-session-id、claude-code-session-id、x-vscode-sessionid、x-session-id、x-parent-session-id、helicone-session-id；body metadata sessionId/session_id/conversationId。
   - 无显式时血缘推断：
     - canonical = format/model/system/tools/tool_choice/messages；排除 max_tokens/temperature/stream/metadata/device 注入；
     - Merkle 链 hash；精确 key + 深度 2/3/4 父前缀；深度 1 不查；
     - 命中复用；未命中新建 UUID。
   - TTL 900s、maxSessions 4096（可 env）、每 session 32 node；LRU + 惰性清理。
   - 模式：off / observe / enforce；首版默认 observe。
2. **接入点**
   - gateway.Engine.RunMessages 计算 attr 之前；同步、async、openai handler 共用。
   - 原生 /v1/messages：用入口 NormalizeBody 后的 body；OpenAI/Responses：用转换后的 Anthropic body。
   - 只生成一个 ID：X-Session-Id 与 metadata.user_id.session_id 同值；不要把 header/body 分成两个 ID。
3. **cache 标记**
   - 新增 upstream.ApplyTranslatedCacheControl(body)：只对翻译入口（Chat/Responses）的 Anthropic body：
     - 清除非 system 消息旧标记；
     - 最后一条非 system 消息的最后一个 content block 打 ephemeral；字符串转 block 数组。
   - 原生 /v1/messages 不动；不改变字节透传。
   - 在内容指纹 ContentKey 计算之后执行，避免重放指纹被缓存标记影响。
4. **诊断**
   - 新增 session_source=explicit|lineage|none、session_reuse=1、session_confidence；保留现有 session= 哈希。
   - cache_read_input_tokens 已有；增加 cache_hit_ratio 计算（读/总输入）仅日志。
5. **账号黏性（P1，后置）**
   - 先 observe 1–2 周，按 session 统计 cache_r 与账号切换；
   - 再实现 session→account 映射：TTL、max share、冷却/停用/线路不可用即回落；不钉探测账号/初始化账号；
   - 只在同模型、同 provider 内生效。

#### 验收

- 显式头 100% 识别；线性续轮复用；fork 不误并；TTL 后新建。
- 同一会话的 cache_read_input_tokens 有可测提升（目标 ≥10% 相对基线；按实际数据定）。
- 原生 Anthropic 请求体字节不变（除已有逻辑）。
- 观察模式不改变选号与上游请求。

#### 风险与回滚

- 误合并 → 深度/TTL/显式优先；observe 先跑。
- 账号黏性导致集中 → TTL + max share + 冷却回落；先只观察。
- cache 标记改变内容指纹 → 放在 ContentKey 之后。

---

### 3.4 P1-D：Responses/Codex 兼容

#### 目标

Codex 多轮不再 400；custom/namespace/tool_search 工具可转换与还原；流事件完整。

#### 设计

1. **ResponseStore**
   - 新文件 internal/openai/response_store.go：LRU + TTL（默认 1000 条 / 24h；env 可配）。
   - 保存 input/output/instructions/model/usage/status；store:false 不落库；重启清空并文档化。
2. **previous_response_id**
   - handleResponses 解析；找不到 404 response_not_found；拼接历史到 input；响应回显 previous_response_id。
3. **工具转换**
   - function 原样；
   - custom → 同名 function，schema 为 input 字符串，响应还原 custom_tool_call；
   - namespace → ns__name，记录映射，响应还原；冲突拒绝；
   - tool_search → 同名 function 代理，标记 execution:client；
   - hosted 工具（web_search/file_search/code_interpreter/computer_use/image_generation/mcp）静默剥离；全部剥离时删 tool_choice/parallel_tool_calls。
4. **流事件**
   - 补齐 output_item.added/done、content_part/reasoning_summary、function_call_arguments.done、custom_tool_call_input.done、response.completed/failed；用 Codex 夹具验证顺序。
5. **多实例边界**
   - 内存 store；单实例 Docker 假设；若多副本，需 sticky 或共享存储（不在本计划）。

#### 验收

- Codex CLI 连续 3 轮不 400；custom exec/apply_patch 可调用；工具历史配对不被上游拒；流事件顺序通过夹具；store TTL/LRU 正常。

#### 风险

- 历史拼接放大 body → 最大条数/字节上限；
- custom 工具 schema 与 Codex 版本漂移 → 以目标版本夹具为准。

---

### 3.5 P1-E：credits 额度快照

#### 目标

后台能统一看到 Start Plan 余额、可领套餐、JWT 年龄；API Key 账号也能看到 coding 窗口（如适用）。

#### 设计

- Account 新增独立字段 CreditsSnapshot（json 只增不减）：
  - balances[]、claimable_plans[]、jwt_issued_at、jwt_age_hours、checked_at、error。
- quota.Service 对 JWT 账号并行查 billing/balance + billing/preview；preview 频率低于 balance（建议 15 分钟，可配）；失败只降级。
- UI：账号详情/额度页增加“可领套餐 / JWT 年龄”，不混入按模型 Quota。
- platform 查询用我们的 windows-x86_64（官方 preview 格式），不要照 zcode-api 的 win32-x64。
- API Key 账号的 monitor 窗口另立字段，不在本阶段与 start-plan credits 混用；若要加，先探针。

#### 验收

- JWT 账号显示 balances + claimable + age；preview 失败不影响 balance；不触发额度误清空；UI 无数据迁移。

---

### 3.6 P1-F：富模型目录

- handleModels 增加 client_version=pi 分支，返回 models[]：slug/display_name/context_window/max_context_window/max_tokens/input_modalities/supported_reasoning_levels/visibility。
- 模型规格表放 internal/model 或 config 只读表；先覆盖 glm-5.3 / glm-5.3-flash；与 examples/pi-models.json 对齐。
- 验收：Pi 同步器解析成功；普通客户端仍拿 data[]。

### 3.7 P2-G：运维与安全

1. **调试 dump**
   - 环境变量 ZCODE_DUMP_UPSTREAM 指向 /data/dumps；JSONL phases client_in/upstream_out/upstream_in；脱敏 Authorization/x-api-key/captcha/cookie；body 上限 256KB；opt-in；不改变请求。
2. **凭据加密**
   - 评估对 accounts.db 中 jwt_token/api_key/proxy 密码做应用层 AES-GCM 字段加密，key 来自 Docker secret ZCODE_DATA_KEY；双读迁移；备份/恢复文档。
   - 若成本过高，至少保证 /data 权限 0700、卷不共享、备份加密；列为 P2。
3. **Docker/反代清单**
   - Nginx：proxy_http_version 1.1、proxy_set_header Connection ""、proxy_buffering off、proxy_request_buffering off、proxy_read_timeout 600s、proxy_send_timeout 600s、client_max_body_size 16m。
   - Caddy：flush_interval -1 + read/write timeout ≥600s。
   - 端口只绑 127.0.0.1 或内网；TLS 在反代；后台路径加 IP allowlist/额外 auth。
   - stop_grace_period: 30s；与 ZCODE_REQUEST_DEADLINE_SECONDS / async 票期对齐。
   - healthcheck 继续用 /meta；增加启动后自检日志（DB、captcha、proxy 配置）。
   - 资源建议：2 CPU / 2 GB 起；Chromium 并发按 CAPTCHA_BROWSER_WORKERS 调。
4. **soak**
   - staging 24h：混合 sync/async/stream；观察 5xx、405、captcha 503、内存、goroutine、SSE 中断。

---

## 4. 里程碑

| 里程碑 | 范围 | 退出标准 |
|---|---|---|
| M0（第 0 周，2–3 天） | 基线与探针 | 3.16.3 golden 采样完成；captcha_wait/cache_r/5xx/405/账号切换基线入库；Docker 反代长流基线 |
| M1（第 1 周） | P0-A system + P0-B 验证码池骨架 | golden 3 块通过；验证码池热路径 p95 < 200ms；可回滚 |
| M2（第 2 周） | P0-B 完成 + P0-C observe | 池/配置 last-good 稳定；session_source 可观测；无行为变化 |
| M3（第 3–4 周） | P1-D Responses + P1-F 富目录 | Codex 多轮/custom 工具通过；Pi 同步通过 |
| M4（第 5 周） | P1-E credits + P0-C enforce/黏性实验 | credits 展示；同会话 cache 提升；黏性有数据支持 |
| M5（第 6 周） | P2-G 运维/安全 + soak | 24h soak 无回归；反代长流通过；发布候选 |

具体日期按团队资源调整；每个里程碑单独分支、单独 CI、失败不前进。

---

## 5. 测试与验收策略

### 5.1 测试层次

- **单元**：system 组装、session resolver、captcha pool（fake solver）、response store、tool 转换、quota parse。
- **集成**：httptest 假上游验证头部/body/captcha/流；同步/async/OpenAI/Responses 四入口。
- **golden**：脱敏的 3.16.3 start-plan 请求 fixture；比较 block 数、长度、cache 布局、关键头；不保存真实凭据/正文。
- **SDK 兼容**：现有 sdk-compat 扩展 Codex/Pi 场景。
- **Docker**：CI docker job 构建；本地/远端 smoke：启动容器、/meta、模拟长 SSE 经 Nginx。
- **soak**：staging 24h；不读取生产 data/。

### 5.2 CI 接线（沿用现有 ci.yml，不另起体系）

- test job 增加定向重复用例：system 组装、session resolver、captcha pool、response store、tools、quota parse。
- sdk-compat 增加 Codex/Pi 场景（目标客户端版本固定）。
- docker job 继续构建 linux/amd64；新增一个 smoke step：启动容器、GET /meta、经 Nginx 跑 5 分钟 SSE（无真实账号，使用本地假上游）。
- -race 覆盖新增包；不降低现有覆盖率。

### 5.3 纪律

- 不在开发机跑自动化测试；推送分支后由 GitHub Actions Ubuntu 执行。
- 每项修复补回归测试；不删除断言、不降低覆盖。
- 以当前提交 SHA 的完整 CI 为验收；不以本地构建或旧 CI 结果代替。
- 真机/官方样本只用于设计验证，不写入 CI，不使用真实账号/密钥。

---

## 6. 观测与指标

| 指标 | 来源 | 用途 |
|---|---|---|
| captcha_wait_ms p50/p95 | 请求诊断 | 验证池收益 |
| captcha_pool_size / empty_take / mint_fail | captcha 日志 | 调池/worker |
| session_source / session_reuse / session_confidence | 诊断行 | 验证推断质量 |
| cache_read_input_tokens / cache_hit_ratio | usage | 验证缓存收益 |
| same_session_account_switch | session 哈希 + 账号 | 黏性实验 |
| response_store_hit/miss/evict | Responses 日志 | 容量/TTL |
| quota_preview_fail | quota 日志 | 降级频率 |
| upstream_5xx/405/captcha | 现有 requeststats | 回归 |

日志只输出哈希/短引用，不输出原始 session、JWT、token、请求正文。

---

## 7. 风险登记

| 风险 | 概率 | 影响 | 缓解 |
|---|---|---|---|
| system 改动触发 3012 | 中 | 高 | legacy/3.16 开关；golden + staging A/B；先小流量 |
| 验证码池 CPU/内存 | 中 | 中 | CPU governor；worker 上限；Docker 资源；idle 缩容 |
| token 一次性假设错误 | 低 | 中 | 取走即删；不重复使用；指标 empty_take |
| session 误合并 | 中 | 中 | 深度 2–4、TTL、显式优先、observe 先跑 |
| 账号黏性集中 | 中 | 中 | TTL、max share、冷却回落、只 observe 后 enforce |
| Responses 内存增长 | 中 | 中 | LRU/TTL；max entries；单实例 |
| 凭据加密迁移 | 中 | 高 | 备份、双读、灰度；P2 |
| 反代缓冲/超时截断 | 中 | 高 | Nginx/Caddy 参数；staging 长流测试 |
| 多副本不一致 | 低（当前单实例） | 高 | 文档禁止多副本；共享存储另立项 |

---

## 8. 发布与回滚

- 每个工作流独立 feature flag；默认值：
  - ZCODE_SYSTEM_PROFILE=legacy 起步，验证后 3.16；
  - ZCODE_CAPTCHA_POOL_ENABLED=true（仅 browser 启用时生效）；
  - ZCODE_SESSION_MODE=observe；enforce 单独开关；
  - ZCODE_PROMPT_CACHE_MARKERS=translated（默认只翻译入口）；
  - ZCODE_RESPONSES_STORE_MAX_ENTRIES=1000 / TTL 24h；
  - ZCODE_DUMP_UPSTREAM 空（关闭）。
- 数据变更只增字段，不删不改；升级前备份 /data/accounts.db。
- 回滚：镜像回退 + flag 回退；session/response 内存状态重启清空；captcha 池关闭即回退。
- 发布门禁：分支 CI 通过 → 版本标签 → release.yml 复用 ci.yml → 镜像/附件验证；不在本计划内打标签。

---

## 9. 明确不做与原因

### 9.1 全局 OpenAI 官方默认 profile

- 128000/thinking/effort 是官方桌面 UI 的客户端默认，不是 Start Plan 上游准入契约。
- 服务器反代下游客户端多样；全局默认会改变生成语义、放大成本、破坏“省略参数”的预期。
- 正确做法：保留显式参数透传（max_tokens/max_completion_tokens/max_output_tokens/thinking/reasoning_effort/reasoning.effort），文档给示例；如确有单客户端需求，做按 key 的显式 profile，不做全局 env。

### 9.2 高速卡 / 官方 off-peak / MCP

- 都不是 Start Plan 能力：zcode-api 对 start-plan 明确 400/不触发；需要 coding-plan 双凭据。
- 单独立项，先做单账号探针，不在本计划范围。

### 9.3 客户端签名

- Start Plan 明确 unsigned；签名是 coding-plan/API Key 路径。只在上游错误分类里观察 VERIFY_*，不实现。

### 9.4 端点路由

- Start Plan 固定 zcode-plan 端点；映射主要面向 API Key coding-plan。只作为 API Key 账号的独立观察项。

### 9.5 TUI / Android / Web 聊天 / 多副本

- 与服务器反代 API 形态无关；多副本需要共享会话/响应存储与 SQLite 替代，另立架构项目。

---

## 10. 开放问题（先探针再动）

1. 3.16.3 官方 start-plan 的 block 1 是否 2594 B（Interactive views）、block 2 是否 5521 B？需一次 3.16.3 wire 采样。
2. block 0 无 cache、dynamic 文本、currentDate 合并是否在 3.16.3 仍成立？用 golden 复核。
3. 我们的 JWT 账号里有多少同时具备 APIKey、属于 coding-plan/start-plan？决定 credits monitor 与战略平面。
4. skip_model_request=true 是否也跳过 claim captcha？zcode-api 认为会，我们 README 认为不会；做 preview/claim 探针。
5. 验证码 token 是否真一次性、TTL 是否约 95 秒；小流量实测。
6. Codex 当前版本是否真的发送 previous_response_id / custom tools？用目标客户端抓包确认。
7. 翻译入口加 cache_control 是否提高 cache_read_input_tokens、是否影响 405/3012？A/B。
8. 反代当前是否有缓冲/超时截断长流？staging 用 5 分钟以上 SSE 验证。

---

## 11. 任务清单（可排期）

| ID | 任务 | 依赖 | 估算 |
|---|---|---|---|
| A1 | block 0 去 cache | 无 | 0.5d |
| A2 | dynamic 文本 + BuildFallbackSystem | A1 | 2d |
| A3 | currentDate 合并 | A2 | 1d |
| A4 | system golden/回归 | A3 | 1d |
| B1 | token 池 + refill | 无 | 3d |
| B2 | config last-good + TTL | B1 | 1d |
| B3 | CPU governor + 指标 | B1 | 1.5d |
| B4 | Docker worker/资源文档 | B3 | 0.5d |
| C1 | session resolver | 无 | 3d |
| C2 | 同步/async/OpenAI 接入 | C1 | 2d |
| C3 | 翻译入口 cache 标记 | C1 | 1d |
| C4 | session 诊断/指标 | C2 | 1d |
| D1 | ResponseStore + previous_response_id | 无 | 2d |
| D2 | custom/namespace/tool_search | D1 | 3d |
| D3 | 流事件 + Codex 夹具 | D2 | 2d |
| E1 | credits snapshot 后端 | 无 | 2d |
| E2 | 额度 UI | E1 | 1d |
| F1 | 富模型目录 | 无 | 1d |
| G1 | session→account 黏性（observe→enforce） | C4 | 3d |
| H1 | 调试 dump | 无 | 1d |
| H2 | 凭据加密评估/实现 | 无 | 3d |
| I1 | Docker/反代参数 + soak | 全部 | 1d |

总计约 35–40 人日；按 1 人约 7–8 周，2 人并行约 4 周。

---

## 12. 一句话

**先修 Start Plan 的 system 外形与验证码热路径，再让会话连续性和 Responses 兼容变得可测；额度与运维补齐；高速卡/off-peak/MCP/签名都不在 Start Plan 的服务器反代计划内。**


---

## 13. 执行记录

### M1 / P0-A（2026-10-28）

- 分支：`codex/server-start-plan-m1-p0a`
- 最终代码提交：`51c774fb846440c8b8869cc63f6b134e7ba1dba9`
- 代码验证 CI：GitHub Actions `ci` run [40623092008](https://github.com/fujunchao/zcode2api-plus/actions/runs/40623092008)，conclusion = **success**
- CI 覆盖：workflows、frontend、test（含 go build / vet / 全量测试 / 各重复回归 / `-race`）、sdk-compat、newapi-compat、windows-proxy-journal、cross-build 五平台矩阵、docker 镜像构建；全部 job 通过。
- 说明：执行过程中先暴露了硬编码时区、fallback 合并幂等和测试夹具文本三处回归，均通过后续提交修复；最终以 `51c774f` 的完整 CI 为准。本文档提交为纯文档提交，不改变上述代码验证 SHA。

### 本批完成项

- A1：`ZcodeSystemBlocks` 的 block 0 对所有档案删除 `cache_control`；block 1 保留 `ephemeral`。
- A2：`zcode_system.json` 增加 `dynamicSections.beforeEnvironment/afterEnvironment`；新增 `BuildFallbackSystem(body, profile)`，默认三块：
  - block 0：CLI Prefix；
  - block 1：现有 stable（2313 字符，含 Desktop Context）；
  - block 2：`\n\n` + `# Communicating with the user` + `\n\n` + 运行时 Environment + `\n\n` + `# Context management`。
- 第三方 system 文本并入 block 2，不再追加第 4 块；Gateway Desktop Capabilities 第 4 块已移除。
- 调用者自带官方前缀 / 显式空值 / MCS 路径保持原样保留，不重复注入。
- A3：新增 `InjectContextPrefix(body, now, loc)`；只对 fallback 路径的首条 user 消息合并 currentDate system-reminder（string → 两个 text block，数组 → 前置一个 block），已含 `# currentDate` 时幂等跳过；时区取 `ZCODE_CLIENT_TIMEZONE`，默认 `Asia/Shanghai`，并嵌入 `time/tzdata` 以适配 Docker 运行镜像。
- A4：新增/更新 system 组装、三块 cache 布局、官方 system 保留、第三方合并、currentDate 不重复和首条 user 合并形态的回归测试；新增脱敏 golden fixture `internal/upstream/testdata/start_plan_system_shape.json`。

### 脱敏 M0 形状对照

按默认 profile 的 `BuildFallbackSystem` fixture（不触网、不含真实凭据/正文）：

| block | 字符数 | 字节数 | cache_control | 关键内容 |
|---|---:|---:|---|---|
| 0 | 42 | 42 | 无 | `You are ZCode, an interactive coding agent` |
| 1 | 2313 | 2317 | ephemeral | 现有 Agent Identity / Desktop Context |
| 2 | 5193 | 5223 | ephemeral | 以两个换行 + `# Communicating with the user` 开头，含 `# Environment`、`# Context management` |

- 回退 system 块数：3；无 `# Gateway Desktop Capabilities` 第 4 块。
- currentDate 块（日期 2026-09-24）字符数：305；格式为 `<system-reminder>... # currentDate ... </system-reminder>\n`。
- 本机未运行 `go run -tags m0probe ./scripts/m0/system_shape.go` 或任何自动化测试；按协作规范只做 `gofmt` 与差异检查，形状断言由上述 fixture 和远端 CI 覆盖。

### 未完成 / 后续

- 未做官方 3.16.3 实机 wire 采样；block 1 是否含 `### Interactive views`（2594 B 变体）仍未确认，该可选项未实现。
- 未做 staging A/B 的 3012 / 405 观测；本批只完成代码与 CI 回归，不能据此宣称生产上游无新增 3012。
- 未实现计划中提到的 `ZCODE_SYSTEM_PROFILE=legacy|3.16` 回滚开关（handoff 未将其列入 P0-A 必需项；当前只能通过回退镜像/版本回滚）。
- 未执行 M0 的 24h 真实流量采集；本次 golden fixture 仅覆盖脱敏结构、长度和标题，不包含真实账号数据或请求正文。

### M2 / P0-B（2026-10-28）

- 分支：`codex/server-start-plan-m2-p0b`
- 范围：验证码 token 池、配置 last-good、严格获取回归、CPU governor、指标与 Docker 资源建议。
- 最终代码提交：`2fb7e97b600e47745a3db574f8e110ea362fd4d3`（`fix(captcha): deliver pool tokens through waiter queue` 为最后代码提交；此前 `447d185`/`2052a3b` 为同一分支修复历史）。
- 代码验证 CI：GitHub Actions `ci` run [40628849663](https://github.com/fujunchao/zcode2api-plus/actions/runs/40628849663)，conclusion = **success**。
- CI 覆盖：workflows、frontend、test（go build / vet / 全量测试 / 重复回归 / `-race`）、sdk-compat、newapi-compat、windows-proxy-journal、cross-build 五平台矩阵、docker 镜像构建；全部 job 通过。

#### 本次完成项

- 新增 `internal/captcha/token_pool.go`：按 `sceneId|region|prefix` 分桶；token 一次性、TTL 默认 95s；后台 refill（min=2 / max=6，1s 节拍）；池空最多 2 路并发 solve 竞速并回填多余结果；CertifyIdRegistry 按 certifyId（或 token 摘要）去重；配置键变化清空旧桶；`Manager.Close` 停止 refill 与在途 mint。
- 新增 `internal/captcha/cpu_governor.go`：Linux `/proc/stat`、其他平台 `runtime/metrics` 进程 CPU 折算；CPU ≥70% 降半速、≥85% 降到 1/4；空闲 120s 缩到 1、900s 停补（保留 1 条唤醒）。
- 配置缓存：`CaptchaConfigCacheTTL` 默认 1 小时（`CAPTCHA_CONFIG_CACHE_TTL` 可覆盖）；成功才写、失败保留 last-good 且 30s 退避；`enabled` / `skip_model_request` 语义不变。
- 严格获取：拿不到 token 仍 503 `captcha_required`；3007/header/F001 有限重试逻辑未改；新增池空失败与并发 JWT 的唯一性回归。
- 指标：`/admin/api/monitor` 增加 `captcha`（pool_size、take_total、empty_take_total、mint_total、mint_fail_total、wait_p50_ms、wait_p95_ms）；`[#]` 诊断行尾部增加 `captcha_wait_ms / captcha_pool_size / captcha_empty_take / captcha_mint_fail`；不打印 token。
- Docker：compose 建议 `ZCODE_CAPTCHA_BROWSER_WORKERS=2` 起步、2 CPU / 2 GB 内存；README/PLAN 同步。

#### 验证与未完成

- 新增单测覆盖：池热一次性、空池 2 路竞速、并发唯一 token、TTL、配置切换清池、重复 certifyId、Manager.Close、空闲缩容/停补、CPU governor、last-good 与 env 覆盖；网关侧覆盖池空不裸发与并发 JWT 唯一 token。
- 本地未运行自动化测试（按 AGENTS 约定）；以当前提交 SHA 的 GitHub Actions 完整 CI 为验收。staging 的池热 p95 与 24h soak 需在部署环境用 `captcha_wait_ms` / `captcha_pool_size` 复测，不在本机压测。
- M3 可直接在本分支基础上做 session observe；验证码 Manager 的公开方法保持兼容。
