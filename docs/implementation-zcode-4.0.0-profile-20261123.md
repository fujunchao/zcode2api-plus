# ZCode 4.0.0 档案登记与灰度记录

记录日期：2026-11-23，Asia/Shanghai。项目基线：`go-rewrite`，`63a339d`（2.14.4-go）。
本记录对应 2.14.5-go（只登记别名）与后续 2.14.6-go（默认版本切换）。

## 1. 结论

本机 ZCode 4.0.0（4.0.0.10870 / build `3bc8a247`）对网关的默认 GLM/Anthropic 主链**没有协议变化**：
模型 API 路径、请求头、body 字段、能力头、签名模块、SDK 版本（anthropic 3.0.81 / openai 3.0.65 /
provider-utils 4.0.27 与 4.0.39）与两份随包模型目录均与 3.19.1 一致；默认桌面条件下的三块 system
（35 / 1,233 / 4,564 B）与 Desktop Context（1,100 B）逐字节相同。4.0.0 的实质增量在客户端侧
OpenAI Responses 无状态加密推理回放，不影响网关的 Anthropic 主链。

因此只需要登记「4.0.0 → 3191」的精确档案别名，**不复制**一份正文相同的 4000 JSON，也不做前缀匹配。

## 2. 证据

- 样本：4.0.0 app.asar 385,203,576 B / sha256 `99f33f89…4253a6`；3.19.1 app.asar 385,193,027 B /
  `f9186044…94ce41a`；随包 CLI `0de95ddd…3afe19`（4.0.0）与 `4b90ae04…8cf8da`（3.19.1）。
- 全量差分：应用成员 31,963 / 31,963，同内容 31,876、同路径改变 41、新增 46、消失 46（均为哈希命名
  chunk 与重建原生依赖）；外部资源 3,477 相同、67 改变、4 个 `visual-judge.md` 删除；安装根 21 个文件
  中 12 相同、9 改变（签名/时间戳层）。
- 协议面：`/api`、`/v1` 路径字面量 51/51，`x-zcode-*` 头名 8/8，URL 75/75，SDK 版本与模型目录
  逐字节一致；`zcode-builtin.json` 261,659 B / `ef3d6465…` 两版相同。
- system 重建：用 AST 白名单解释 4.0.0 CLI 的默认 6 个 section，再由真实的
  `orderSectionsForInjection` / `buildSectionContent` / `ContextBuilder.assembleSystemMessages`
  组装，得到 35 B `7779642e…`、1,233 B `aac4ba4c…`、4,564 B `929856d5…`、Desktop Context 1,100 B
  `39a1e86c…`，与项目 3191 档案逐字节相同；`totalTokensReminderEnabled` 两处默认 false，开启时
  第三块 4,615 B / `36458a63…`。
- 生产只读核查：4.0.0 发布后（11-22 19:30 之后）容器 2.14.4-go + 3.19.1 身份仍有 200 成功记录；
  近 48h 日志无 3012 / 405 / unusual activity（样本量小，不代表云端策略永久不变）。

完整逆向材料见 `.local/zcode-v4-audit-20261123-171432`、`.local/zcode-v400-review-20261123-181745`
与 `.local/zcode-v400-verify-20261123`（本轮独立复算）。

## 3. 代码改动（2.14.5-go）

- `internal/config/zcode_system_profile.go`：`ZcodeSystemProfileForExact` 增加 `4.0.0` 精确映射到
  `ZcodeSystemProfile3191`；未知的 4.0.x 继续回落 legacy 并告警。
- `internal/config/zcode_system_profile_test.go`：映射与精确性用例加入 4.0.0 / 4.0.1。
- `internal/upstream/system_profile_3191_test.go`：新增 `Test3191AliasFor400`，锁死 4.0.0 与 3.19.1
  三块系统与桌面上下文逐字节一致，以及 4.0 / 4.0.1 / 4.0.0.1 回落 legacy。
- 默认 `ZCODE_CLIENT_VERSION` 仍为 `3.19.1`；本次不改变默认请求形态。

## 4. 探针（2.14.5-go 镜像，未动生产）

探针只读账号库副本、单账号、单请求；不写生产库、不改派账号。

```bash
# 1) 复制账号库到临时目录（避免对生产库加锁/写入）
mkdir -p /tmp/zcode-probe && cp /opt/1panel/apps/local/zcode2api/data/accounts.db* /tmp/zcode-probe/

# 2) 先用新镜像做 dry-run，确认 4.0.0 的 system 指纹
docker run --rm -v /tmp/zcode-probe:/data \
  -e TZ=Asia/Shanghai -e ZCODE_CLIENT_VERSION=4.0.0 ghcr.io/fujunchao/zcode2api-plus:2.14.5-go \
  probe --db /data/accounts.db --dry-run

# 3) 真实单请求（挑选隔离账号；glm-5.3 当时并发准入受限，用 flash 验证身份）
docker run --rm -v /tmp/zcode-probe:/data \
  -e TZ=Asia/Shanghai -e ZCODE_CLIENT_VERSION=4.0.0 ghcr.io/fujunchao/zcode2api-plus:2.14.5-go \
  probe --db /data/accounts.db --account <账号 id 或 name> --model glm-5.3-flash
```

判定：`http_status=200` 且无 `business_code` 通过；405 + `3012` 说明版本↔文案不匹配，停止切换；
`3007` 换账号；其他网络/账号错误按 egress 排查，不改文案结论。

**结果（2026-11-23 21:12–21:15 CST，镜像 2.14.5-go，digest `sha256:05e99f18…`）：**

| 步骤 | 账号 / 模型 | 结果 |
|---|---|---|
| dry-run | — / — | `client_version=4.0.0`、`system_profile=3.19.1`、3 块，sha16 `7779642e…` / `aac4ba4c…` / `929856d5…` 全部正确 |
| 真实请求 | `ahxvtu` / glm-5.3 | HTTP 429、`3009 model concurrency limit exceeded`（账号/模型并发，不是版本拒绝） |
| 真实请求 | `alevqo` / glm-5.3 | 同上 429 / 3009 |
| 真实请求 | `arkcoh` / glm-5.3-flash | **HTTP 200**，约 3.6 s，input 1,691 / output 16，无业务错误码；system 三项 sha16 正确 |

`glm-5.3` 当时的 3009 与生产日志里同类并发准入一致；换 flash 后同镜像、同账号库、同 4.0.0 身份返回 200，
说明 4.0.0 版本号 + 3191 文案的配对被上游接受。探针只读副本库，未改生产账号状态。

## 5. 默认版本切换（2.14.6-go，已执行）

- `config.ZcodeClientVersion` 默认值改为 `4.0.0`；`docker-compose.yml`、README、默认版本测试
  （`TestDefaultClientVersionHasVerifiedPromptPair`）、`version_env_test` 与相关注释同步。
- 回退：`ZCODE_CLIENT_VERSION=3.19.1`（3191 档案保留），或整个镜像回退到 2.14.5-go / 2.14.4-go。

**状态：4.0.0 默认已随 2.14.6-go 提交；待 CI / release / 部署验收。**
