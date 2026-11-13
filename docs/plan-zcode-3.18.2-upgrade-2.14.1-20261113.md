# ZCode 3.18.2 身份升级与 2.14.1-go 发布计划

> 日期：2026-11-13（Asia/Shanghai）。基线：`go-rewrite` / `bf93817`，产品版本 `2.14.0-go`（生产运行中）。
> 背景：[3.18.2 影响裁定](analysis-zcode-v3.18.2-verdict-20261113.md)、[完整逆向](analysis-zcode-v3.18.2-20261113.md)、[项目影响笔记](analysis-zcode-v3.18.2-project-impact-notes-20261113.md)。
> 本轮只做探针与计划；**未改任何业务代码、未发版本、未改生产部署**。代码改动等你点头后开工。

## 0. 结论

3.18.2 的 system 文案与 3.18.0 逐字节相同，因此这次升级**不新增档案**，只在版本→档案的精确映射里加一个别名，把默认声明从 `3.18.0` 抬到 `3.18.2`，随 `2.14.1-go` 一起发布。真机探针已验证「3.18.2 头 + 3180 文案」被上游接受（HTTP 200）。

## 1. 真机探针结果（已完成，2026-11-13 21:20）

生产服务器上用运行中的 `2.14.0-go` 镜像起一次性容器，账号库只读挂载，单账号 `cacxqs-1d3d6d2c`，每条命令只发一条 `max_tokens=16` 的真实请求；不启调度、不写库、不冷却账号。

| 组合 | 出站身份 | system 档案 | HTTP | 业务码 |
|---|---|---|---:|---|
| 基线（生产当前形态） | `ZCode/3.18.0` + `X-ZCode-App-Version: 3.18.0` | 3180（3 块） | **200** | 无 |
| 候选（本计划发版形态） | `ZCode/3.18.2` + `3.18.2` | 3180 文案（3 块，经 `-system-file` 逐字节注入） | **200** | 无 |

关键旁证：候选请求 `input_tokens=91 / cache_read_input_tokens=576`，上游把它判为与基线**同一缓存前缀**，与「两版文案字节等同」的静态结论互相印证。原始结果见 [probe-3182-20261113/notes.md](../temp/probe-3182-20261113/notes.md)。

反面取证（不发请求，只 `--dry-run`）：`-client-version 3.18.2` 不加映射时档案回落 `3.16.x` —— 2 块、block0 `You are an interactive coding agent` 42 B / `46dd360a22c87a92`、身份段 1,217 B / `fb4777a371fac341`。即「3.18.2 的头 + 3.16.x 的体」，正是 [3.17.1 事故](analysis-zcode-3.17.1-405-incident-20261101.md) 的形态。

探针边界：只覆盖单账号、单模型 `glm-5.3`、无 tools、无首轮环境提醒、非流式。它证明「云端当前接受 3.18.2 身份」，不等于长流/工具往返/多账号已验收——那部分由灰度观察覆盖。

## 2. 代码改动（2 处，均为一行级）

| 文件:行 | 现在 | 改为 |
|---|---|---|
| `internal/config/zcode_system_profile.go:45` | `case "3.18.0":` | `case "3.18.0", "3.18.2":` |
| `internal/config/config.go:616` | `env("ZCODE_CLIENT_VERSION", "3.18.0")` | `env("ZCODE_CLIENT_VERSION", "3.18.2")` |

配套注释：`zcode_system_profile.go:21-25`（补「3.18.2 逐字节相同」的核验依据）、`config.go:612-615`、`internal/config/profile.go:69,178-180`。

两条硬约束：

1. **不要新增 `ZcodeSystemProfile3182` 常量**。文案完全相同，复用 `ZcodeSystemProfile3180` 才能让 `UsesUserReminderEnvironment()`（`zcode_system_profile.go:61-63`）与 `zcodeProfileSource()`（`internal/upstream/request.go:62-68`）同时正确；新造常量必须同步改这两处，等于制造分叉。
2. **版本号必须三段式 `3.18.2`**，不能写客户端四段构建号 `3.18.2.10443`（`version_test.go:24` 的 `TestClientVersionShape` 会直接失败）。

## 3. 配置与文档

| 位置 | 改动 |
|---|---|
| `docker-compose.yml:58` | `ZCODE_CLIENT_VERSION: ${ZCODE_CLIENT_VERSION:-3.18.2}` |
| `.env.example:98,100` | 注释与示例 UA 同步 3.18.2 |
| `README.md:56,60` | 默认版本、三块指纹说明、回退说明同步；补一句“3.18.2 与 3.18.0 文案逐字节相同，故复用 3180 档案” |
| `internal/config/config.go:639` | `AppVersion = "2.14.1-go"`（发版步骤，不是身份改动） |

生产 1panel 项目的 compose（`/opt/1panel/apps/local/zcode2api/docker-compose.yml`）当前**没有**设 `ZCODE_CLIENT_VERSION`，所以新默认值会自然生效，无需改服务器环境变量。

## 4. 测试

**必改（默认值一改就会红）**
- `internal/config/version_test.go:33`：`"3.18.0"` → `"3.18.2"`
- `internal/config/version_env_test.go:19`：`{"defaults","","","3.18.0","ZCode/3.18.0"}` → 3.18.2
- `internal/config/version_env_test.go:21`：`{"user_agent_override",…,"3.18.0",…}` → 3.18.2

**必加**
- `internal/config/zcode_system_profile_test.go`：映射表加 `{"3.18.2", ZcodeSystemProfile3180}`；精确表加 `{"3.18.2", ZcodeSystemProfile3180, true}`。
- `internal/upstream/system_profile_test.go:51-58`：加一行 `"3.18.2","3.18.0",<同 prefix>,<同 identityHead>,1233,"aac4ba4cc7b555b3"`（注意第二列是**档案常量** `"3.18.0"`，不是客户端版本号）。
- 新增 `Test3182AliasSameAs3180`：分别用 `withSystemVersion("3.18.0")` 与 `("3.18.2")`，断言 Blocks/FirstText/FirstChars/IdentityChars/IdentitySHA16/DynamicChars/DynamicSHA16 七项全等且 `exact=true`。
- 新增负向断言：`ZcodeSystemProfileForExact("3.18.1")` 仍返回 `(legacy,false)`，防止日后被改成 `"3.18"` 前缀匹配。

**不用改**：`gateway/body_test.go` 的 3.18.0 用例（显式设版本，不随默认值变化）；`claim/version_test.go`、`quota`、`captcha`（全部由 `app_version=config.ZcodeClientVersion` 派生）；模型白名单；`Dockerfile`；内嵌 `zcode_system_3180.json`。

## 5. 执行顺序与门禁

1. 开分支 `codex/zcode-3.18.2-profile`（只推 `origin`），改第 2–4 节内容，本地只做格式化与差异检查。
2. 推送后等当前 SHA 的完整 `ci.yml`（`gh run watch`）；失败读日志再修，不以本地构建替代。
3. 合回 `go-rewrite`，等合并后 SHA 的 CI 全绿。
4. 你确认发版后：改 `config.go:639` 为 `2.14.1-go`、更新 README 发布说明，再打 `v2.14.1-go` 标签；`release.yml` 复用完整 CI 通过才构建附件与推镜像 `ghcr.io/fujunchao/zcode2api-plus:2.14.1-go`。
5. 部署：把 1panel 项目 compose 的镜像 tag 改为 `2.14.1-go`，`docker compose up -d`；核对容器健康、`/meta` 显示 `2.14.1-go`。
6. 部署后复核（不改账号库）：
   ```bash
   docker run --rm -v /opt/1panel/apps/local/zcode2api/data:/data:ro -e ZCODE_DATA_DIR=/data \
     ghcr.io/fujunchao/zcode2api-plus:2.14.1-go probe --dry-run
   # 期望 client_version=3.18.2、profile=3.18.0、blocks=3、三项 sha16 为 7779642e/aac4ba4c/26a2daa8
   ```
   再对隔离开户发一条真实探针，期望 `http_status=200`；随后 24h 观察 `3012/405` 与首字延迟。
7. 回滚：**不需要回滚镜像**，在 1panel compose 加 `ZCODE_CLIENT_VERSION: "3.18.0"` 并 `up -d` 即回到旧身份；必要时再把镜像 tag 退回 `2.14.0-go`。

## 6. 非目标（本轮明确不做）

- **不接入 `zai_cn` 中国区账号**（用户已确认不需要）；新版的国内回调分流、Start 材料识别、Linux 密钥环回退都与服务器 JWT 链路无关。
- 不实现桌面新能力（语音、Worktree、Design、Session 工具、浏览器点选/导出、分享有效期、手机推送、字体平滑、麦克风重授权）。
- 不改模型白名单、不动计费/领取/验证码逻辑、不登记未经核验的 `3.18.1`。
- 不模拟客户端每轮的 `total_tokens` 用户提醒。

## 7. 风险与判据

| 风险 | 判据 | 处置 |
|---|---|---|
| 只改版本不改映射 | `--dry-run` 出现 `profile=3.16.x`、`blocks=2` | CI 的 `TestDefaultClientVersionHasExactSystemProfile` 会拦；显式环境变量覆盖拦不住，部署前必须跑一次 `--dry-run` |
| 云端后续收紧最低版本 | 出现 `405` + `3012` | 回退 `ZCODE_CLIENT_VERSION=3.18.0`；本次探针已证明 3.18.2 当前可用，升级反而降低“版本过旧”风险 |
| 长流/工具往返未覆盖 | 灰度 24h 的 5xx、首字延迟、`3008/3009/3010` 并发拒绝 | 仅在异常时按回退路径处理 |
| 探针用掉隔离账号配额 | 已用 2 条、约 1.2k tokens | 无 |

## 8. 证据

- 探针原始结果与命令：[temp/probe-3182-20261113/notes.md](../temp/probe-3182-20261113/notes.md)
- 客户端逆向：[temp/zcode-v3182-verify-a/evidence-summary.md](../temp/zcode-v3182-verify-a/evidence-summary.md)、[temp/zcode-v3182-endpoints-c/REPORT.md](../temp/zcode-v3182-endpoints-c/REPORT.md)
- 代码触点审计：[temp/zcode-v3182-impact-b/audit-3.18.2-impact.md](../temp/zcode-v3182-impact-b/audit-3.18.2-impact.md)

## 9. 执行结果（2026-11-13 完成）

代码与测试：

- 分支 `codex/zcode-3.18.2-profile`：`c8c09cc` feat（映射别名 + 默认版本 + 测试 + compose/.env/README）、`ac4a406` chore（`AppVersion=2.14.1-go` + 发布说明）。仅 12 个文件、130 插入 / 18 删除，无档案新增。
- 分支 push 后 CI `43433774560` 全部 14 个 job 通过（test / test-race / test-repeat / 前端 / SDK / NewAPI 兼容 / Windows / 交叉构建 / docker）。
- 快进合并到 `go-rewrite`（同一 SHA `ac4a406`），推 origin。

发布：

- 标签 `v2.14.1-go` → `ac4a406`；release 运行 `43434540764` 的 validate 全量 CI 与 image 构建全部通过，Release 附件（5 平台二进制）已上传。
- 镜像 `ghcr.io/fujunchao/zcode2api-plus:2.14.1-go` = `latest`，同 digest `sha256:f0df726c…1d391c`。

生产部署（`154.12.85.101`）：

- `/opt/1panel/apps/local/zcode2api/docker-compose.yml` 镜像标签 `2.14.0-go` → `2.14.1-go`（已留备份 `docker-compose.yml.bak_pre2141_*`），`docker compose up -d` 重建，容器 `Up (healthy)`，`/meta` 返回 `{"version":"2.14.1-go"}`。
- 部署后复核：`probe --dry-run` → `client_version=3.18.2`、`profile=3.18.0`、`blocks=3`、三项 sha16 为 `7779642e / aac4ba4c / 26a2daa8`；隔离账号 `cacxqs` 真机探针 → HTTP 200；容器日志中 `3012`/`405` 计数 0。
- 回退路径（秒级、无需换镜像）：在 compose 加 `ZCODE_CLIENT_VERSION: "3.18.0"` 后 `up -d`。
