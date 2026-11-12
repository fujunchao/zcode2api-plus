# ZCode 3.18.0 单账号探针验证报告

> 日期：2026-11-11 22:04（Asia/Shanghai）。分支：`codex/zcode-3.18.0-profile`，提交 `b12eece`（代码 `1c86204`）。
> 目的：在服务器上验证 3.18.0 的「版本号 + 3180 system 文案 + 首轮环境提醒」是否被上游接受，并确认 3.17.2 回退通道仍可用。
> 方式：服务器本地 `docker build` 分支源码 → 用**复制出来的账号库快照**跑 `probe`；生产容器未替换、未写生产账号库。

## 1. 构建与样本

- 在服务器 `/root/zcode2api-plus-3180` 浅克隆分支，本地 `docker build` 成功。
- 测试镜像：`zcode2api-plus:3.18.0-test`，复审修复后按 `4f8caee` 重建，digest `sha256:42c7236fa8ab0c8dad0d6d5d0640b73b3f33695b2f2d46d0b395e1f35f07aaa3`，206 MB（首轮验证的旧镜像 digest 为 `6e30774d…`）。
- 账号库：从生产数据目录复制 `accounts.db` + WAL/SHM + `device_mid.txt` 到隔离目录；探针结束后已删除副本。
- 账号：一个未使用过的 active 账号（报告不记录 ID/名称）。

## 2. dry-run 指纹（不发请求）

```bash
docker run --rm -v /root/probe3180:/data \
  -e ZCODE_CLIENT_VERSION=3.18.0 -e ZCODE_CAPTCHA_BROWSER=false \
  zcode2api-plus:3.18.0-test probe --dry-run
```

```json
{
  "client_version": "3.18.0",
  "system": {
    "profile": "3.18.0",
    "blocks": 3,
    "first_chars": 35,
    "first_sha16": "7779642ea44a4696",
    "identity_chars": 1233,
    "identity_sha16": "aac4ba4cc7b555b3",
    "dynamic_chars": 1595,
    "dynamic_sha16": "26a2daa8c8381086"
  }
}
```

## 3. 3.18.0 真实请求

```bash
docker run --rm -v /root/probe3180:/data \
  -e ZCODE_CLIENT_VERSION=3.18.0 -e ZCODE_CAPTCHA_BROWSER=false \
  zcode2api-plus:3.18.0-test probe --account <隔离账号>
```

| 字段 | 结果 |
|---|---|
| `ok` / `http_status` | **true / 200** |
| `client_version` | 3.18.0 |
| `system_profile` | 3.18.0 |
| `system_blocks` | 3 |
| `system_first_sha16` | `7779642ea44a4696` |
| `system_identity_sha16` | `aac4ba4cc7b555b3` |
| `system_dynamic_sha16` | `26a2daa8c8381086` |
| `device_mid_source` | `account_virtual` |
| `elapsed_ms` | 6,788 |
| 响应 | `stop_reason=end_turn`，`input_tokens=1097`，`output_tokens=3` |

probe 内部走 `gateway.NormalizeBody`，因此这次请求同时包含 3.18.0 的三块 system 与**首轮 user 环境提醒**；上游返回 200，没有 3012。

## 4. 3.17.2 回退对照（同一账号）

```bash
docker run --rm -v /root/probe3180:/data \
  -e ZCODE_CLIENT_VERSION=3.17.2 -e ZCODE_CAPTCHA_BROWSER=false \
  zcode2api-plus:3.18.0-test probe --account <同一隔离账号>
```

第一次经同一出口代理返回 `unexpected EOF`（网络瞬态，不是 405/3012）；约 8 秒后重试：

| 字段 | 结果 |
|---|---|
| `ok` / `http_status` | **true / 200** |
| `client_version` | 3.17.2 |
| `system_profile` | 3.17.1 |
| `system_blocks` | 2 |
| `system_first_sha16` | `7779642ea44a4696` |
| `system_identity_sha16` | `4058874023c7541f` |
| `elapsed_ms` | 6,229 |
| 响应 | `stop_reason=end_turn`，`input_tokens=1688`，`output_tokens=3` |

3.17.2 的 input_tokens 更高，与旧档案含 Desktop Context、system 更大一致。

## 4.1 复审修复后的复验（2026-11-12）

- 代码修复续轮环境提醒与旧官方模板剥离（提交 `26364c6`）后，按 `4f8caee` 重建镜像；dry-run 指纹仍为 35 / 1,233 / 1,595 B 与三项 SHA16。
- 换一个未使用过的 active 账号发真实 3.18.0 请求：**200**，`input_tokens=1097`、`output_tokens=3`，无 3012。
- 原账号复验时曾返回 **429 / business_code=3009**（`model concurrency limit exceeded`）；这是上游并发限制，不是版本/文案拒绝，换账号即 200。

## 5. 结论与边界

- 上游接受 **3.18.0 身份 + 3180 三块文案 + 首轮环境提醒**（200，无 3012）。
- 同一账号回退 **3.17.2 + 3171 档案** 仍返回 200，回退通道可用。
- 生产容器未替换、未写生产账号库；测试镜像与隔离账号库副本已清理（镜像保留在服务器，可复用）。
- 本报告不记录账号 ID、名称、代理地址、凭据或响应正文全文。
- 仍需单独观察：领取/额度投放是否随 app_version 变化；全量流量下的错误率与风控分布。
