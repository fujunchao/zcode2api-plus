# ZCode 3.17.1 升级事故：上游 405 根因分析

> 日期：2026-11-01（北京时间）。触发：用户报告「升级 v2.11.4-go 后无法正常工作」，附日志包 `E:\qq\zcode_logs_20261031.zip`（解包副本：`temp/incident-20261031/zcode_logs/`）。
> 结论依据：日志时间线 + 3.17.1 客户端静态代码 + 本项目代码；**未执行客户端、未用真实账号发起请求**。

---

## 0. 一句话结论

**3.17.1 改了官方 system 提示词的「首段」文本（`You are ZCode, an interactive coding agent` → `You are ZCode, an interactive agent`，并同步改了身份段首行措辞）。上游按「声明的 `app_version` 应发哪一版首段」做逐字节校验，而我们把版本号升到 3.17.1、文案仍是 3.16.3 时代那一套 → 每请求被判 `3012 unusual activity` → 网关按阶梯冷却账号 → 池级 503。回退到 3.16.3（版本+文案重新配对）即恢复。**

这不是网络、并发或代理问题，是**「版本号必须与官方文案成对出现」**这条此前被忽略的耦合。

---

## 1. 现象与时间线（日志事实）

### 1.1 失败段（v2.11.4-go，伪装 3.17.1）

`01_new_api_v2.11.4_errors.log`：**16 行、8 个不同请求**，窗口 `21:50:35 – 21:56:22`。

| 状态码 | 次数 | 原文 | 归因 |
|---|---:|---|---|
| 405 | 5 | `request has been blocked due to unusual activity.` | **上游请求级风控（业务码 3012）** |
| 429 | 3 | `model concurrency limit exceeded` | 上游并发超限（次生：405 后重试/并发叠加） |
| 503 | 8 | `相同请求内容刚刚被上游风控拦截，请稍后重试或修改请求内容` | **本网关自己的重放防护**（`internal/asyncpool/pool.go:452`） |

请求 id 观察：同一 id 在 4 秒内出现两次 405（`…Jha9Yyou` 21:51:10 / 21:51:14），说明失败后仍在重试；503 成对出现（21:51:46/48/51/59 各 2 条）说明两个下游客户端同时在打同一内容。

### 1.2 恢复段（v2.11.3-go，伪装 3.16.3）

`04_new_api_v2.11.3_success.log`：**87 条全部成功**（`status=ok / end=eof / response=completed`），窗口 `22:14:37 – 22:45:14`（30.6 分钟，≈2.84 请求/分钟），模型均为 `glm-5.3`。

### 1.3 站点侧（openresty）

`02_openresty_v2.11.4_access.log` 87 行**全是后台管理页/接口**（81×200、3×502、2×307、1×404），与模型链路无关；3 个 502 与 `03_openresty_error.log` 的 `22:58:33` 断连对应**回退时的容器重启**。⇒ 反向代理不是本次故障点。

---

## 2. 关键发现：3.17.1 改了官方 system 首段

### 2.1 静态证据（3.15.3 vs 3.17.1 客户端 CLI）

模型请求的 system 由随包 CLI（`resources/glm/zcode.cjs`）构造；两版对照：

| 片段 | 3.15.3 CLI | 3.17.1 CLI |
|---|---|---|
| block 0（CLI Prefix，`buildCliPrefixSection`） | `You are ZCode, an interactive coding agent`（42 字符） | `You are ZCode, an interactive agent`（**35 字符**，去掉 `coding`） |
| 身份段首行（`pBa`，interactive 模式） | `You are an interactive ZCode agent that helps users with software engineering tasks.` | `You are an interactive agent that helps users according to their "Output Style", which describes how you should respond to user queries.` |
| Harness 项目符号 | `- …`（无前导空格） | ` - …`（**有前导空格**） |
| 安全检查段 `nBo` | 相同 | **字节相同**（459 字符，两边一致） |

3.17.1 侧原文（可复核）：
```
uBa="You are ZCode, an interactive agent";            // buildCliPrefixSection
pBa='You are an interactive agent that helps users according to their "Output Style", …'
SDn(e=!0){return["# Harness"," - Text you output outside of tool use …", …]}
```

### 2.2 我方现状

- `internal/upstream/zcode_system.json` 的 `blocks[0].text` = `You are ZCode, an interactive coding agent`（**3.15.3 时代文案**）。
- `blocks[1]` 共 **2313 字符**，分段（已精确测量）：

| 段 | 字符数 | 内容 |
|---|---:|---|
| 身份段首行 `pBa` | 84 | `You are an interactive ZCode agent that helps users with software engineering tasks.` |
| 安全检查 `nBo` | 459 | `IMPORTANT: Assist with authorized security testing…` |
| Harness 段 | 663 | `# Harness` + 5 条 `- …`（**无前导空格**） |
| 桌面上下文 | 1100 | `# ZCode Desktop Context` + `### Interactive views` / `### Files & URLs` / `### Inline Code Comments` |

- **快照与 3.15.3 CLI 的同一段逐字符串一致**：`old_pba`、`no_space_bullet`、`# Harness`、`# ZCode Desktop Context` 在 3.15.3 CLI 中各命中 1 次、在 3.17.1 CLI 中 `old_pba`/`no_space_bullet` 均 0 次。
- 3.17.1 的桌面上下文构造器（`WDn` ← 3.15.3 的 `ybn`）**抽样一致**（`# ZCode Desktop Context`/`### Interactive views` 起始相同）；动态部分由服务端下发，本次无法静态验证。
- 3.17.1 重建的等价身份段为 1355 字符（pBa 新措辞 + nBo + 新 Harness），与我们 `block1` 前 1355 字符**从第 24 个字符起就分叉**。
- 版本号与文案的绑定点是 `internal/upstream/capabilities.go:66-76` `OfficialSystemPrefix()`（读 `blocks[0]`）与 `internal/upstream/request.go:60` 的块 0 原样透传——**版本变了，这里不会变**。

### 2.3 为什么这就等于 405

本项目已用受控实验证明过这条机制（`docs/analysis-riskcontrol-dsh-405-root-cause-20261012.md`）：**同一条用户消息，system 首段与官方不一致时必然 405/3012，一致时 200；校验点在 system 首段，逐字节比较，加头/改内容都绕不过。**

把它和本次结合：

1. 上游以 `app_version` 识别「你是哪个客户端版本」，并按该版本期望的首段做比对；
2. 我们把 `app_version/UA/X-ZCode-App-Version` 升到 3.17.1（`internal/config/config.go:483`），首段仍是 `…interactive coding agent`；
3. 3.17.1 期望 `…interactive agent` → 首段不匹配 → 405 `3012`；
4. 网关把 405 记为「上游风控」（`internal/gateway/classify.go:140+` `MarkRiskControl`），按 `ZCODE_RISK_COOLING_STEPS=300,900,3600` 阶梯冷却账号；账号被冷却后请求无号可用 + 同内容重放防护 → 返 503；
5. 期间出现的 429 `model concurrency limit exceeded` 是**次生**：405 后重试撞上游并发闸门，不是根因。

---

## 3. 竞争假设与判别实验

| 假设 | 机制 | 支持证据 | 反对/缺口 | 置信度 |
|---|---|---|---|---|
| **H1 文案-版本耦合** | 上游按版本校验 system 首段 | 首段确实变了；该机制已被本项目受控实验证明；改版本后立刻 405、回退后恢复 | 恢复发生在最后一次 405 之后 18 分钟，与阶梯冷却到期存在时间混淆（见 §3.1） | **高** |
| H1b 新头缺失 | 3.17.1 新增 `x-zcode-session-features`（模型请求上下文头），缺失即指纹不符 | 该头确为 3.17.1 新增，且属于「context-owned model headers」 | 无任何证据表明缺它触发 3012；本项目从未发送过它 | 中 |
| H2 版本白名单/灰度 | 上游尚未放行 3.17.1 | 3.17.1 发布仅 2 天 | 白名单拒绝通常是 403/426，而非 405 `unusual activity` | 低 |
| H3 巧合风控 + 冷却到期 | 与版本无关的内容级风控恰好发生 | 历史确有偶发 405；冷却阶梯与 18 分钟间隔吻合 | 8 个不同请求、6 分钟内持续被拒，密度异常；且回退后 87/87 全成功 | 低中（需实验排除） |

### 3.1 必须承认的混淆

最后一次 405 在 `21:56:22`，首个成功在 `22:14:37`（间隔 18 分 15 秒）；本网关风控冷却阶梯为 `300/900/3600` 秒——**15 分钟档恰好能解释这个间隔**。所以「回退即恢复」不能单独作为因果证明，必须靠静态证据（H1 已具备）与下面的判别实验闭环。

### 3.2 判别实验设计（单账号，随时可退）

| 实验 | 配置 | 预期结果 | 说明 |
|---|---|---|---|
| **E1（推荐先做）** | 版本 3.17.1 + **新版 block0/身份段**（按 §4.2 生成） | 200 ⇒ H1 成立且修复方案有效 | 单账号、单条探针请求；失败也只是 405，不影响其它账号 |
| E2 | 版本保持 3.16.3 + **新版 block0** | 405 ⇒ 反向验证「版本-文案必须成对」 | 若 E2 也 200，说明上游只比对「当前官方首段」，H1 需降级 |
| E3 | 版本 3.17.1 + 旧文案，**等冷却期完全过去**再打 | 仍 405 ⇒ 排除 H3 | 与事故窗口相隔 ≥1 小时，避免冷却混淆 |
| E4（可选） | E1 + 同时补 `x-zcode-session-features` | 若 E1 失败而 E4 成功 ⇒ H1b | 仅在 E1 失败后做 |

---

## 4. 处置建议

### 4.1 立即（已由用户完成，确认无误）

- 部署侧设 `ZCODE_CLIENT_VERSION=3.16.3` 回退；日志 87/87 成功证明**当前服务已恢复**。
- 不要在全量池上重复「只改版本号」的试验：每次试验都会按 `MarkRiskControl` 累积账号冷却，超阶梯还会置 `invalid`（`internal/gateway/classify.go:140+`）。

### 4.2 修复方案（版本化文案，按版本成对发布）

1. 把 `zcode_system.json` 拆成版本档案：`zcode_system_3.16.x.json`（现状）、`zcode_system_3.17.1.json`（block0 = `You are ZCode, an interactive agent`；身份段首行按 3.17.1 `pBa`；Harness 保留前导空格）。
2. 由 `ZcodeClientVersion` 选择档案（`OfficialSystemPrefix()` 与 `ZcodeSystemBlocks()` 都走同一选择器），杜绝「版本与文案各改一半」。
3. 补测试：`TestOfficialSystemPrefixMatchesClientVersion`——按版本断言 block0 精确值；再补一条「版本档案缺失时启动失败/显式告警」，避免静默用错文案。
4. 3.17.1 身份段还需确认桌面上下文（`# ZCode Desktop Context` 1009 字符）在 3.17.1 是否原样保留——**这是 E1 之前唯一的不确定项**；若 E1 仍 405，下一步抓一版真实 3.17.1 model-io 做逐字节还原。
5. 可选：在模型请求上补 `x-zcode-session-features`（空集/按会话能力），仅在 E1 失败后作为 H1b 的补丁。

### 4.3 发布与验证协议（写进流程，不再靠猜）

- **版本升级必须附该版本的真实 model-io 抓包**（system 三段 + 关键头），先做首段比对，再谈改常量。
- 升级顺序：单账号探针（1 条请求）→ 观察 30 分钟 → 小流量灰度 → 24 小时 → 全量；任何一步 405，立即回退版本与文案。
- 回退口径固化：`ZCODE_CLIENT_VERSION` 与 `system 档案`必须一起回退（本轮教训）。

### 4.4 是否需要热修发版

建议发 **v2.11.5-go** 把默认版本改回 `3.16.3`（正文写清事故原因与 3.17.1 暂不支持），原因：
- v2.11.4-go 的默认值对**其它部署**同样是雷；用户不会都像本机这样及时发现并回退；
- 热修只动默认值 + 文档 + 版本档案框架，不动协议；
- 3.17.1 支持等 E1 通过后再单独发版。

---

## 5. 方法论复盘：上一轮为什么没抓住

| 环节 | 上一轮的判定 | 实际 | 教训 |
|---|---|---|---|
| CLI 提示词文本变化 | 记为 P2「未验证/建议抓包复核」 | **就是本次事故的根因** | 首段是被逐字节校验的字段，不能降级为观察项 |
| 「模型 wire 未变」 | 依据端点/头/UA 未变得出 | 端点/头确实未变，但 **system 文本变** | 「wire」必须把 system 文案算进去 |
| 版本升级风险评估 | 认为「只改版本值」低风险 | 版本与文案强耦合 | 版本升级 = 文案升级，必须成对验证 |
| 发版门禁 | CI 全绿即发 | CI 不校验上游接受度 | 需要「真实流量探针」这一步，而不是只有 CI |

---

## 6. 证据索引

| 证据 | 位置 |
|---|---|
| 失败日志（16 行/8 请求） | `temp/incident-20261031/zcode_logs/01_new_api_v2.11.4_errors.log` |
| 恢复日志（87/87 成功） | `temp/incident-20261031/zcode_logs/04_new_api_v2.11.3_success.log` |
| 站点访问/断连 | `temp/incident-20261031/zcode_logs/02_openresty_v2.11.4_access.log`、`03_openresty_error.log` |
| 3.17.1 首段/身份段/Harness 原文 | `D:\Programs\ZCode\resources\glm\zcode.cjs`（`uBa` / `pBa` / `SDn`） |
| 3.15.3 对照原件 | `temp/zcode-v3153-20261014/bundled/cli/zcode.cjs` |
| 我方文案快照 | `internal/upstream/zcode_system.json`（blocks[0]/blocks[1]） |
| 首段取值与透传 | `internal/upstream/capabilities.go:66-76`、`internal/upstream/request.go:60` |
| 405 判定与阶梯冷却 | `internal/gateway/classify.go:140+`、`internal/config/config.go:289`（`ZCODE_RISK_COOLING_STEPS`） |
| 503 重放防护文案 | `internal/asyncpool/pool.go:449-453` |
| 历史 405 机制受控实验 | `docs/analysis-riskcontrol-dsh-405-root-cause-20261012.md` |
| 本轮重建/比对产物 | `temp/incident-20261031/{prompt-diff.json,prompt_rebuild3.py,stats.py}` |

---

## 7. 探针实验与修正（2026-11-01 追记）

`v2.11.5-go` 引入 `zcode2api probe`（只读账号库、单账号、单请求），做了一组 A/B：

| # | 版本 / 文案 | 出口 | 响应 |
|---|---|---|---|
| E0 | 3.16.3 / 3.16.x 档案 | proxy 146.190.80.158:9090 | **200**，正文 `OK`，`input_tokens=1684` |
| E1 | 3.17.1 / 3.17.1 档案（首段已对齐 `You are ZCode, an interactive agent`，sha16 `7779642e…`） | 同上 | **405 / 3012**，547ms |

E0/E1 同账号（`duhyod-13a388b1`）、同出口、同极简 body（`stream:false`、无 tools），
只改「版本 + system 文案」⇒ **账号未被标记，差异确实来自 3.17.1 侧**；但也说明
**H1（只差首段与身份段文字）不成立**：对齐首段后仍然 3012。

### 7.1 新发现：身份段有两个变体

3.17.1 的 `ContextBuilder.build()` 按「是否配置 Output Style」选择身份段：

```js
n = config.outputStyle?.prompt.trim() ? config.outputStyle : undefined
t.push(wDn(n, midConversationSystem))   // wDn(e,t) => e ? pBa : dBa
```

- `pBa`：「You are an interactive agent that helps users according to their "Output Style"…」
- `dBa`：「You are an agent working with the user toward their goals, using your own judgment along the way.」

E1 用的是 `pBa`；桌面端默认（未配 Output Style）实际走 `dBa` ⇒ 身份段不匹配，
这与 3012 完全一致。`v2.11.6-go` 已把档案重建为 `dBa` 变体（identity 1317 字符）。
注意 3.15.3 里没有 `dBa` 这段文字（当时恒用 `pBa`），所以这是 3.17.x 的新行为。

### 7.2 真实抓包与逐字节修正（2026-11-01）

官方 3.17.1 客户端（新账号，可正常对话）留下 `model-io-sess_e42884da….jsonl`，取其中一条真实请求：

| 项 | 真实值 | 我方 v2.11.6-go | 判定 |
|---|---|---|---|
| block 0 | 35 字符 `You are ZCode, an interactive agent` | 相同 | 一致 |
| 身份段 | 1231 字符，sha16 `4058874023c7541f` | 1317 字符 | 多 88 字符 |
| system 块数 | 3（35 / 3119 / 3794） | 2 | 差异（含用户私有路径，不可逐字节校验） |

88 字符来源：Harness 第 9 条项目符号在源码里是模板字面量
`${e?"…mid-conversation system turns…":"…<system-reminder> tags…"}`，
静态提取未展开、把 `${…}` 原样写入档案；真实桌面会话 `e=true` 只发前半句。
按文档化机制（校验范围＝第 0 字节到身份段末尾），这就是 3012 的直接原因。

真实请求其余画像：`user-agent: ZCode/3.17.1`（无 ai-sdk 后缀）、`x-zcode-agent: glm`、
`x-zcode-query-source: main_turn`、**无 `x-device-mid`**；体含 `output_config{effort:max}`、
`thinking{type:enabled}`、`tool_choice{type:auto}`、`tools 106`、`max_tokens 128000`。

### 7.3 仍未排除

- 若 `v2.11.6-go` 的 `dBa` 版本仍是 3012，则剩下两种可能：请求体字段差异
  （tools / `output_config` / `context_management`）或上游尚未放行 3.17.1；
  两者都只能靠官方客户端真实抓包定案（`probe --system-file` 可直接复现抓包文案）。
- `x-zcode-session-features` 已静态排除：仅在会话记录过特性（tool_search/workflow 等）时发送，
  一条 “hi” 的默认会话本来就不发。

---

## 8. 未验证与风险

1. **已做在线实验（见 §7）**：E0=200 / E1=405 证明「只对齐首段」不足；H1 修正为「身份段变体（dBa）+ 文本」假设，待 v2.11.6-go 的探针复验。
2. **3.17.1 桌面上下文段是否变化未知**：我们只能静态重建 CLI 身份段，桌面段（`# ZCode Desktop Context`）来自服务端下发，需真实抓包确认。
3. **H1b（新头）未排除**：若 E1 失败，需按 E4 补头复测。
4. **其它部署可能已受影响**：v2.11.4-go 默认即 3.17.1，v2.11.5-go 已把默认回退到 3.16.3，部署新版本即可解除。