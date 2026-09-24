# Golden 抓包比对：官方客户端实发请求 vs 网关出站（2026-09-24）

> 方法：用官方客户端自带的 CLI 以无头模式（`--prompt`）跑一次，把其端点与 provider 的
> baseUrl 指向本机**记录型透传代理**，代理原样转发到真实上游并落盘原始请求。
> 这是 `docs/plan-client-format-parity.md` 阶段 6 的执行结果。
> 全文不含凭据与实际设备指纹。

---

## 〇、结论摘要

### 已被 golden **证实为正确**的 7 项

| 项 | 证据 |
|---|---|
| 系统提示词块 0 | 42 字符，与官方**逐字节相等** |
| 系统提示词块 1（`# Harness`） | 1211 字符，与官方**逐字节相等**（阶段 3b 的文案改动完全命中） |
| `# Environment` 模板与标签顺序 | 与官方逐行一致，连"可选行"的位置都一致 |
| **头与 body 同源** | golden 的头 `x-os-version: 10.0.19044` 与其 body 的 `OS Version: win32 10.0.19044 x64` 取自同一处 ⇒ 我们那条派生关系是官方本来的做法 |
| `x-platform` / `x-os-category` / `x-release-channel` / `x-title` / `x-zcode-agent` / `x-zcode-session-type` | 取值全部一致 |
| **查询参数 `platform=` 的格式** | golden 实测 `?app_version=…&platform=windows-x86_64` ⇒ 阶段 4 的那个疑问由事实定案 |
| **不实现签名是对的** | 服务端开关返回 `codingPlanSignature.enable=true`，但客户端**仍发未签名请求** |

### 新发现 / 待处理 3 项

| # | 项 | 说明 |
|---|---|---|
| **G1** | **模型请求的 `User-Agent` 带 ai-sdk 后缀** | 官方为 `ZCode/<ver> ai-sdk/provider-utils/<v> runtime/node.js/<major>`；我方原先只有裸串。**已修** |
| **G2** | **`x-query-id` 恒在** | 官方每个轮次都会生成；我方原先只在客户端送来时才发。**已修** |
| **G3** | **模型请求不带 `x-device-mid`** | 实测确认官方模型请求**没有**这个头（只有配置/额度类接口带）。我方在推理路径发送 ⇒ **多发**。开关已就位，是否翻转默认值待定（见 §五） |

---

## 一、可复现步骤

1. 生成自签证书（客户端强制 https，无环境变量可放宽）。
2. 起一个**记录型透传代理**：`https://127.0.0.1:<port>` → 真实上游，落盘每次请求的
   方法、路径、全部请求头（凭据类取值掩码）与原始 body。
3. 以以下环境变量运行客户端 CLI 的**无头单次模式**：

| 变量 | 作用 |
|---|---|
| `ZCODE_BASE_URL` | 覆盖端点 origin（配置/额度类请求） |
| `ZCODE_BUILTIN_PROVIDER_CONFIG_FILE` | 内建 provider 配置的 active 路径（需可写副本） |
| `ZCODE_BUILTIN_PROVIDER_BUNDLED_CONFIG_FILE` | 内建 provider 配置的 bundled 路径 |
| `ZCODE_PERSONAL_PROVIDER_CONFIG_FILE` | 个人 provider 配置（**用副本**，把模型 baseUrl 指向代理） |
| `NODE_TLS_REJECT_UNAUTHORIZED=0` | 接受自签证书 |

4. 命令形如：`zcode -p "…" --no-color --cwd <空目录>`

> ⚠️ 两点必须注意：
> **① 个人 provider 配置只读、只改副本**，绝不改动用户真实配置。
> **② `--cwd` 要放在一个非 git 仓库的目录**，否则 body 里会注入该仓库的 git 快照
> （本次因 cwd 落在仓库内，golden 的 `Is a git repository` 为 `yes`，且带回了仓库的提交历史）。

---

## 二、抓到的请求

| # | 请求 | 结果 |
|---|---|---|
| 1 | `GET /api/v1/client/configs?app_version=…&platform=windows-x86_64` | 200 |
| 2 | `GET /api/v1/agent/configs` | 200，body 含 `{"codingPlanSignature":{"enable":true}}` |
| 3 | `POST /v1/messages`（模型请求） | 307 → `/cn/v1/messages` → 404（路径由 provider baseUrl 决定，非官方路径） |

`/api/v1/agent/configs` 请求头（非模型接口的那一套）：

```
user-agent: ZCode/<ver>                     ← 裸串，无 ai-sdk 后缀
http-referer: <端点 origin>
x-api-key: <凭据>
x-device-mid: <设备指纹>                     ← 非模型接口**带**设备指纹
x-platform: win32-x64
x-os-category: windows
x-os-version: 10.0.19044
x-title: Z Code@cli
x-release-channel: production
x-client-language: zh-CN
x-client-timezone: Asia/Shanghai
x-zcode-app-version: <ver>
```

> 这印证了记忆里那条结构：**头是两套集合** —— 模型请求不带设备指纹，非模型接口带。

---

## 三、模型请求：逐字段比对

golden 27 个头 vs 我方 19 个头。下表只列非传输层、且**判定过**的行。

| 头 | golden | 我方 | 判定 |
|---|---|---|---|
| `anthropic-version` | `2023-06-01` | `2023-06-01` | ✅ |
| `content-type` | `application/json` | `application/json` | ✅ |
| `x-platform` | `win32-x64` | `win32-x64` | ✅ |
| `x-os-category` | `windows` | `windows` | ✅ |
| `x-client-language` | `zh-CN` | `zh-CN` | ✅ |
| `x-client-timezone` | `Asia/Shanghai` | `Asia/Shanghai` | ✅ |
| `x-release-channel` | `production` | `production` | ✅ |
| `x-zcode-agent` | `glm` | `glm` | ✅ |
| `x-zcode-session-type` | `main` | `main` | ✅ |
| `user-agent` | `ZCode/<ver> ai-sdk/provider-utils/4.0.27 runtime/node.js/22` | 同形态（已修） | ✅ 形态一致 |
| `x-query-id` | `<uuid>` | 恒发（已修） | ✅ |
| `x-request-id` / `x-session-id` / `x-zcode-trace-id` | `<uuid>` | `<uuid>` | ≈ 每请求随机，属正常 |
| `authorization` | `Bearer <凭据>` | `Bearer <凭据>` | ≈ 凭据 |
| `x-device-mid` | **无** | 有 | ❌ **多发**（G3） |
| `x-api-key` | 有（与 `authorization` 同时出现） | 无（JWT 账号只发 `Authorization`） | ⚠️ provider 形态差异，见 §五 |
| `x-os-version` | `10.0.19044`（真实机器） | `10.0.26100`（伪装值） | — 伪装值差异，**不算缺陷** |
| `x-title` | `Z Code@cli` | `Z Code@electron` | — 取决于伪装成 CLI 还是桌面端，**可配置** |
| `http-referer` | `<端点 origin>` | `https://zcode.z.ai` | — 抓包环境产物（我把 origin 指向了代理） |
| `accept` / `accept-language` / `sec-fetch-mode` | `*/*` / `*` / `cors` | 无 | ⚠️ 传输层指纹，见 §五 |

---

## 四、模型请求体比对

golden 的顶层字段顺序：

```
model, max_tokens, metadata, system, messages, tools, tool_choice, stream, thinking, output_config
```

| 字段 | golden 取值 |
|---|---|
| `model` | `glm-5.3-flash` |
| `max_tokens` | `128000` |
| `metadata.user_id` | **JSON 字符串**：`{"device_id":"<uuid>","account_uuid":"","session_id":"<uuid>"}` |
| `system` | 3 段 |
| `messages` | 1 条 |
| `tools` | **52** 个 |
| `tool_choice` | `{"type":"auto"}` |
| `stream` | `true` |
| `thinking` | `{"type":"enabled"}` |
| `output_config` | `{"effort":"max"}` |

### 4.1 系统提示词三段

| 段 | 长度 | 比对结果 |
|---|---|---|
| `[0]` | 42 | **与官方逐字节相等** |
| `[1]` | 1211 | **与官方逐字节相等**（含 `# Harness` 三条 bullet 的最新措辞） |
| `[2]` | 9026 | 我方只有其中的 `# Environment` 子段 |

三段都带 `cache_control: {"type":"ephemeral"}` —— 与我方一致。

golden 的 `[2]` 是一个**多章节合并块**，章节顺序为：

```
# Communicating with the user      ← 输出风格类
# Session-specific guidance        ← 会话指引
# Memory                           ← 记忆
# Environment                      ← 环境（我方对应这一段）
# Context management               ← 上下文管理
```

其中 `# Environment` 原文：

```
# Environment
You have been invoked in the following environment:
- Primary working directory: <cwd>
- Is a git repository: yes
- Platform: win32
- Shell: Git Bash
- OS Version: win32 10.0.19044 x64
- You are powered by the model named <providerId>/<modelId>.
```

⇒ 标签集与顺序与我方实现**完全一致**；我方刻意省略了最后那条可选行（理由见
`systemprompt.go` 注释：我们的 provider/model 与官方不一致，注入了反而是新矛盾）。

### 4.2 `metadata.user_id`

**我方完全不发 `metadata`。** 官方恒发，且 `device_id` 就是 `x-device-mid` 的取值。

这是本次新发现里最"重"的一条：它是**设备身份在 body 侧的投影**，与请求头
`x-device-mid` 呼应。上游若在 body 里找它，我方就是"缺字段"。

> 注意与我方既有设计的关系：我方把每账号指纹放在 `x-device-mid` **头**上用于账号隔离。
> golden 显示官方在**模型请求**里既不发该头、又把它写进 `metadata.user_id` —— 两种放置
> 方式等价但位置不同。**这条需要单独决策**（见 §五 G3）。

---

## 五、剩余决策（需要你拍板，已不在"格式对齐"范畴内）

### G3-a `x-device-mid`：翻转默认值？

- golden 事实：官方模型请求**不带**该头。
- 我方考量：每账号独立指纹是**账号隔离**的基础（见 `request.go` 注释）；去掉后所有账号
  在模型请求上就没有设备身份了，可能反而更不像真实客户端。
- 开关已就位：`ZCODE_UPSTREAM_SEND_DEVICE_MID`，默认 `true`。
- **建议**：与 G4（`metadata.user_id`）**一起决策** —— 若要把设备身份迁到 body 里，
  应该"头去掉、body 加上"，而不是两头都去掉。

### G4 `metadata.user_id`：是否注入？

- 官方恒发，且 `device_id` = 该次请求的设备指纹。
- 我方目前不发。补上即可与官方 body 结构对齐，且**顺带把设备身份放回官方位置**。
- 风险：`metadata` 也可能被下游客户端使用（Anthropic 官方用它做用户标识/滥用追踪），
  需要确认不会与下游的值冲突（我方下游多为 Claude Code 类客户端，通常不带 metadata）。

### G5 传输层三头（`accept: */*`、`accept-language: *`、`sec-fetch-mode: cors`）

- 这三个是 Node fetch / undici 栈的产物，不是 ZCode 自己的头；所有 Node 客户端都带。
- 我方是 Go，缺失它们等于"暴露 HTTP 客户端栈"。
- 成本极低（加三行），但属于"模仿另一个语言的运行时"，是否值得由你判断。

### G6 provider 形态差异：`authorization` 与 `x-api-key` 同时出现

- golden 的 provider 是「自带 apiKey 的自定义 provider」，客户端把同一个 key 同时放进
  两个头。
- 官方 zcode-plan（OAuth）路径只会是 `Authorization: Bearer <jwt>`。
- 我方按账号模式二选一，对每种模式都是正确的。**建议保持**，仅在文档里记录这个差异来源。

### G7 `# Communicating with the user` 等 4 个章节

- 官方 `[2]` 块里还有 4 个"行为指引"章节，我方不注入。
- 它们影响的是**模型行为**而不只是身份，注入会改变所有下游客户端的表现。
- **建议不做**（除非有证据表明上游按这些章节判定客户端真伪）。

---

## 六、安全与卫生

- 全程**未修改**用户的真实配置：个人 provider 配置只读、仅改副本；客户端安装目录只读。
- 抓包文件（含掩码后的凭据与设备指纹原文）落在本机临时目录，该目录整体已 gitignore，
  **不入库**。
- 本文档不含凭据、设备指纹、本机绝对路径。
