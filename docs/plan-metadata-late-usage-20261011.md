# metadata 隔离与迟到用量修复计划（v2.9.13-go）

## 范围

New API 的风控/冷却重试策略已由用户调整。本次仅修复本项目中可独立验证的缺陷：

1. **metadata 共享写入**：入口已有 metadata 时，顶层浅拷贝仍共享内部 map；逐账号身份注入
   会改写入口及其它副本，导致后续尝试的内容视图/bodyhash 漂移。
2. **迟到的输入/cache 用量漏记**：当前 UsageCollector 只从 message_start 读取输入/cache，
   message_delta 只读取输出。合法的后续累计输入/cache 字段因此可能被遗漏。

不修改 New API、不读取其生产配置、不调整网关错误状态码、重试预算、风险阈值、
代理轮换、签名或上下文策略；不重放生产请求。不把上述缺陷认定为上游 3012 的确切原因。

## 诊断与可证伪判据

按优先级验证：

- H1：已有 metadata 的共享引用造成污染。若成立，只复制 metadata 容器即可让入口及先前副本
  保持不变，同时每次实际出站仍具有当次账号的设备身份；同账号重试和跨账号切换的 bodyhash 稳定。
- H2：输入/cache 在后续 usage 事件补报时被遗漏。若成立，同一条合成 SSE 在协议适配器中有
  输入/cache 值，而网关统计为零；累积这些字段后两者应符合各自口径。
- H3：日志差异完全来自其它字段或 New API 计费口径。通过同一合成输入、实际本地 HTTP 出站
  和接收端验证排除本项目的已知遗漏；没有原始生产 SSE，不宣称解释或修复全部生产差异。

反馈环为 GitHub Actions Ubuntu 上的 `go test ./...`；定向回归命令：

```sh
go test ./internal/upstream ./internal/gateway ./internal/asyncpool ./internal/openai -run 'TestMetadataIsolation|TestLateUsage' -count=1
```

先提交会在旧实现上失败的回归，取得远端失败日志，再修复并复验。
本地仅阅读、编辑、gofmt 和差异检查，不运行自动化测试或构建。

## 实施方案

### A. 身份注入的所有权边界

- 在共享的 InjectDeviceMetadata 内复制已有 metadata map，再覆盖 user_id。
- 只复制实际写入的 metadata 容器，不深拷贝整段长历史；保留其它扩展字段和值。
- 缺省/null/非对象 metadata 继续使用原兼容创建逻辑。
- 不改变 user_id 的 JSON 字符串形态、键序、account_uuid 空值或会话头/正文同值规则。
- 不改变 input_ref/bodyhash 算法，仅消除意外共享写入；不修改重放键或保护窗口。

### B. 用量统一累积

- message_start、message_delta 及非流式 JSON 共用四类用量字段累积规则。
- 输入、输出、缓存读取、缓存创建都是累计观测值，取最大，不重复相加；
  缺失、零、负数或较小的重复值不得抹掉已记录的值。
- 保留 UsageComplete 和流完整性/取消判据：输入/cache 补报不自动变成成功，也不增加重试。
- 不估算正文 token、不将缓存读写混入 Anthropic 原始输入值，不改上游 SSE 内容。
- 检查 Messages、Async、Chat Completions、Responses 的实际交付、诊断和账号用量；
  OpenAI 兼容响应按已有契约把缓存计入总输入，不改变字段形态。
- Chat/Responses 必须累积多次 delta，而非只保存最后一个 delta；复用其已有 max 合并规则，
  防止最后的输出更新/零占位再次覆盖此前补报的输入与缓存。

## 回归矩阵

- 已有 metadata 的连续身份注入、原对象/先前副本隔离、其它扩展字段保留。
- 同步/异步真实本地 HTTP 的同账号重试、跨账号切换，实际设备/会话字段正确且内容引用稳定。
- 并发从同一个只读入口构造独立副本，结合 CI race 检查共享写入。
- SSE 开始用量缺失/为零、后续补报、重复/下降/非法值、分块、终值缺失、非流式对照。
- 四个模型 HTTP 入口的输入/cache 用量落账、流内容/终态及客户端 usage 兼容。
- 继续保留取消、异常流、风险保护、SDK/Pi、前端和平台交叉编译的完整 CI。

所有回归只使用隔离目录、合成凭据与本地 httptest 服务；日志包、用户正文和运行 data/ 不进入 CI。

## 推送与发布

- 开发分支：`codex/metadata-usage-fix`；只推 origin，保留已有未跟踪分析文件和用户改动。
- 当前基线：`f09b87da2808d871931bca6905470dd020877938` / v2.9.12-go。
- 完成实现后更新 AppVersion 与 v2.9.13-go 发布说明；等待同一提交的完整工作分支 CI。
- 快进合入 go-rewrite，等待主分支完整 CI 后才推 v2.9.13-go 标签。
- 标签发布必须再次通过 release.yml 复用的完整 CI；完成后核对五个二进制、
  linux/amd64 与 linux/arm64 镜像的版本/revision，并核对 latest digest。
- 无数据库迁移；只修复升级后的新请求，不伪造或回填历史用量。

## 执行记录

- `3431750aee3fdd6ca17027689c074d83826d4022` 的
  [CI 38149717745](https://github.com/fujunchao/zcode2api-plus/actions/runs/38149717745)
  已复现共享 metadata 被后续身份覆盖，以及 SSE 输入/cache 仍为零而同值 JSON 正确。
- 补足 FeedLine 测试的 SSE 空行边界，避免不合法夹具影响终态断言；保留并加强终态检查。
- 进一步加入引擎/异步票的实际 HTTP 重试及四种入口的用量落账/交付回归，不用单元测试替代调用点验证。
