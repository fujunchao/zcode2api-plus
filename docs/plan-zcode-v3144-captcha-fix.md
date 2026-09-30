# ZCode 3.14.4 模型验证码适配方案

日期：2026-09-30。依据：`D:/augment-projects/zcode2api-plus/docs/analysis-zcode-v3144-compatibility-20260930.md`。

## 目标与边界

官方下发 `captcha.enabled=true, skip_model_request=true` 时，所有模型入口不再依赖验证码；套餐领取继续验证。保留 JWT 鉴权、system、设备 metadata、会话归因、账号选择与已有风控策略。

- [x] 先用同步网关失败测试锁定：跳过模型验证码后应 200，而不是本地 503。
- [x] 解析 `SkipModelRequest`；提供 `GetModelVerifyParam` 模型专用入口。
- [x] 同步网关与异步池切换入口，拆除 JWT 形态与验证码必要性的命名耦合。
- [x] 补配置、缓存、领取、流式、重试及兼容接口回归。
- [x] 同步 Go 客户端默认版本和环境示例为 3.14.4，更新使用说明。
- [x] 全量测试、静态检查、构建与最终差分检查。

## 设计决策

1. `Config.SkipModelRequest` 使用 bool；只在上游明确返回 true 时跳过模型验证码。false、缺失、null 保留原策略；非法类型/配置请求失败保留现有默认验证回退。
2. `GetModelVerifyParam` 在人工缓存查询之前检查策略。跳过时返回 `(nil, nil)`，不调用 solver、不发送缓存 token，**不清空共享的领取人工 token**。
3. 保留 `GetVerifyParam` 供领取使用，其人工缓存优先、禁用时 nil、求解失败报错等行为不变。模型/领取共用内部缓存和求解实现，避免实现两套 solver。
4. 配置继续使用既有 10 分钟缓存；每次模型调用重新咨询 Manager 的有效配置，不缓存新的进程级开关。一次 Manager 调用最多获取一次配置，不因失败重复请求。
5. 同步网关用 `isJWT` 表示账号形态；`NormalizeBody` 始终按 JWT 形态注入 system，不受验证码策略影响。异步池有独立取码调用，必须一并更新；OpenAI Chat/Responses 通过公共 Engine 自动使用新策略。
6. 保留真实验证码挑战的既有分类及有界重试（最多 3 次），每次取码遵守有效配置。即使仍处于 skip 状态，也不将 3007/验证码挑战误判为账号鉴权失败，不强行开启求解，不无限重试。配置恢复要求验证码后，按正常缓存刷新恢复取码。
7. 不新增绕过上游策略的环境开关，不关闭整个浏览器池，不修改领取流程或数据库。独立且被忽略的 `zcode-switch` 仓库不在本次 Go 主项目改动范围内。

## 验证边界

- 配置解析/Manager 公共入口：模拟官方 JSON、缺失/错误响应、时钟控制刷新、缓存 token 与 solver 调用数。
- HTTP `/v1/messages`：同步/流式无 solver 成功，无验证码头，JWT 鉴权/system/metadata 保留；限流、并发、过载与验证码挑战有界。
- 异步票务：成功 ready/chunk/done、无验证码头和 solver 调用。
- 领取 Service：同一 Manager 模型跳过但领取仍发验证码，3007 后仍更换 token。
- OpenAI Chat/Responses：确认共享 Engine 的路径没有额外取码分支，并运行相应回归测试。

测试仅使用临时数据库、本地模拟服务和假 solver，不使用生产凭据、真实模型额度或实际领取操作。本次不提交、推送或部署。

## 执行结果

### 失败测试先行

修复前实际运行并得到以下失败（不是仅靠阅读代码推断）：

```text
go test ./internal/gateway -run '^TestJWTModelCaptchaSkipWithoutSolver$' -count=1
FAIL: 应无 solver 成功，实际 503 captcha_required

go test ./internal/captcha -run '^TestFetchConfigModelSkipFlag$' -count=1
FAIL: 配置响应 skip_model_request=true，解析后的 SkipModelRequest=false

go test ./internal/asyncpool -run '^TestAsyncModelCaptchaSkip$' -count=1
FAIL: 无缓存时票务只返回 error；有缓存时仍发送 claim-only-token
```

对应修复后均通过，并保留为长期回归测试。

### 实现清单

| 文件 | 改动 |
| --- | --- |
| `D:/augment-projects/zcode2api-plus/internal/captcha/captcha.go` | 解析新字段，模型专用入口先判策略；共享内部取码实现，领取入口保持不变 |
| `D:/augment-projects/zcode2api-plus/internal/gateway/engine.go` | 同步/兼容接口切换模型入口，JWT 形态变量更名，保留挑战分类及 system |
| `D:/augment-projects/zcode2api-plus/internal/asyncpool/pool.go` | 异步模型同样使用专用入口 |
| `D:/augment-projects/zcode2api-plus/internal/config/config.go` | 默认客户端版本更新为 3.14.4 |
| `D:/augment-projects/zcode2api-plus/.env.example`、`D:/augment-projects/zcode2api-plus/README.md` | 同步版本、解释模型跳过与领取验证码的区别及升级方式 |

新增 12 个顶层测试（含表驱动子用例），分布在 captcha/gateway/asyncpool/claim/openai；OpenAI 测试夹具仅增加 Manager 引用，以便通过真实 HTTP 入口验证 JWT 路径。所有生产取码调用点复查结果：同步网关和异步池使用 `GetModelVerifyParam`，只有领取使用 `GetVerifyParam`。

### 最终验证

```powershell
# 新增场景连续重复三轮，全部通过。
go test ./internal/captcha ./internal/gateway ./internal/asyncpool ./internal/claim ./internal/openai -run 'ModelCaptcha|FetchConfigModelSkipFlag|CompatibleAPIsSkip' -count=3

# 全项目无缓存测试、静态检查及构建全部通过。
go test ./... -count=1
go vet ./...
go build ./...
git diff --check
```

本机版本巡检确认：安装客户端 3.14.4，Go 默认声明 3.14.4，二者一致。
未运行 `-race`：本机 `CGO_ENABLED=0`，PATH 中未发现 GCC/Clang；没有为本次修复安装额外工具链。
没有执行真实账号模型调用或领取操作，所以以上结果是离线回归验证，不是生产环境验收。

### 使用与防复发

- 从修复后的源码重新构建网关/镜像，替换并重启即可，无需数据库迁移；本次没有自动部署或重启服务。
- 若部署显式设置旧版 `ZCODE_CLIENT_VERSION` / `UPSTREAM_USER_AGENT`，同步更新或移除覆盖值。
- 自动领取仍需原有浏览器或人工验证码；不要把模型级放宽误当成全局取消验证。
- 根因是新字段未解析，以及模型与领取共用入口缺乏业务场景区分。现在用独立模型入口和官方配置 JSON 夹具锁定语义，后续客户端更新需复查所有模型取码调用点，而不只是同步 `/v1/messages`。
