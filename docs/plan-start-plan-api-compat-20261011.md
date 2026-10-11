# Start Plan API 透明兼容方案

日期：2026-10-11；基线：`22c05877d8c6f1f3e3fa48987d32e27935d8da98`（v2.9.10-go）。
目标补丁版：v2.9.11-go；工作分支：`codex/start-plan-api-compat-2-9-11`。

## 目标与不变量

部署形态为“本地客户端 → 反向代理 → Docker 网关 → 上游模型”。网关负责协议适配，
不代替本地客户端执行工具，也不把所有客户端套进官方桌面代理的交互规则。

- 保留调用者的指令、消息顺序、工具定义及显式采样参数。
- 不复制官方私有会话、Memory、Desktop Context 或工具目录。
- 不新增真实模型探测、自动标题、签名或臆测的 JWT 续期。
- 不改默认 8192 输出上限、不强制 max 档位、不硬编码本机 OS 19044。
- 不改 API 路径、下游响应格式、SSE 终态、取消、账号调度或限流策略。
- 所有测试仅使用合成数据、隔离目录和 httptest 上游，不读取运行 data/。

## 实施切片

### 1. 调用者上下文所有权

新增 `ZCODE_PRESERVE_CLIENT_CONTEXT`，默认 true。

- JWT 请求已有非 null 的顶层 system 时原样保留，包括显式空字符串/空数组。
- 没有调用者系统指令时，沿用已有网关兼容兜底，不加入新的桌面内容。
- OpenAI 的显式 instructions/system/developer 也应保留其“由调用者提供”的语义。
- false 恢复旧版 JWT 标准三块前置及精确去重，以及 OpenAI 指令统一提升行为，
  供已有部署回退；不删除旧行为的测试。
- 不通过身份关键词、路径或缓存字段猜测后删除用户内容。

### 2. 中途系统消息（MCS）

- Chat/Responses 初始连续 system/developer 指令进入顶层；会话开始后的指令保留原位置，
  作为 Anthropic system 消息，不把后来的规则提前作用于历史消息。
- 原生 system 消息保持字符串；只有不带额外属性的单一 text 块可规范为字符串，
  不丢缓存信息或调用者扩展字段。
- 共享出站模块仅在实际消息序列包含 system、且是支持的模型协议时添加
  `mid-conversation-system-2026-04-07`，合并而不是覆盖已有 beta。
- 同步与异步使用同一功能声明逻辑；无 MCS 的请求不自动加头。
- 已安装官方 provider revision 30 对当前使用的 ZCode Plan 和 api.z.ai Anthropic
  两个上游均声明支持 MCS；不改变两种账号的鉴权与路由。
- 回退开关关闭时保留既有转换行为；显式传入的 beta 仍按原规则透传。

### 3. 显式参数保留

- 有效的 thinking=enabled 不因缺少 budget_tokens 被丢弃。
- 保留预算校验和 effort 映射，只移除非 Anthropic 的 clear_thinking。
- 参数缺省仍缺省，不替调用者发明预算、输出上限、工具选择或思考档位。

## 测试接口与验收

沿用已经约定的公开接口作为测试 seam（可替换行为的接口位置）：

1. `/v1/messages`：调用者 system 的逐字段保留、原生 MCS、既有 JSON/SSE 交付。
2. `/v1/chat/completions`：初始/中途指令、工具轮次和参数转换。
3. `/v1/responses`：instructions/input 指令顺序、函数调用往返和流式终态。
4. `/async/v1/messages`：后台实际出站的 system/MCS，不仅检查入口函数。

纯转换补充测试只通过已有的公开转换函数和请求构造接口验证；不复算实现得到期望值。
重点包括显式空指令、重复调用、JSON 往返、带缓存/扩展字段、已有 beta、没有 MCS、
回退模式和入参不被嵌套修改。保留已有工具、SSE、取消、Python SDK/Pi 回归。

问题机制已在前期一手源码审计中定位，本轮不重复猜因：先提交最小失败回归，
在 GitHub Actions 读取失败日志确认，再提交相应修复；每个切片完成后再推进下一项。
本机只阅读、编辑、gofmt 和差异检查，不运行自动化测试或构建。

## 发布流程

- 所有 git push 仅到 origin；所有 gh 查询显式 `-R fujunchao/zcode2api-plus`。
- 工作分支完整 CI 通过后，确认 origin 主分支未出现未合入的新提交，再快进主分支。
- 版本号、中文发布说明与代码同一提交；等待该提交主分支完整 CI 成功后推版本标签。
- release.yml 再次复用完整 CI，成功后才构建上传二进制并发布双架构镜像。
- 最终核对 Release 五个平台附件及 GHCR 的版本/latest 与 linux/amd64、linux/arm64。
- 不以推送成功、单个任务成功或旧 SHA 的成功记录代替发布完成。

## 实施记录

- `72d4824`：先提交调用者 system 的公开 HTTP 回归；远端 CI `38105661340` 按预期失败，
  确认现有注入改写了字符串、显式空值及完整客户端块。
- `27ddf5c`：新增默认保留/旧行为回退；远端 CI `38105931311` 完整成功，包含 SDK/Pi、race 和 Docker。
- `4b9c68d`：提交四入口 MCS/显式 thinking 回归；远端 CI `38106416270` 按预期复现
  指令前移、system 形态改写、能力声明缺失及 enabled 被丢弃。
- 发布提交另由完整分支 CI、主分支 CI 和 release.yml 复用验证验收；最终状态以 GitHub 当前提交结果为准。
