# ZCode 3.19.1 适配实施与生产部署记录（v2.14.3-go）

> 实施与部署日期：2026-11-23（Asia/Shanghai）。方案：[plan-zcode-3.19.1-upgrade-20261123.md](plan-zcode-3.19.1-upgrade-20261123.md)。
> 逆向证据：`docs/analysis-zcode-v3.19.1-20261122.md`、`docs/analysis-zcode-v3.19.1-project-impact-notes-20261122.md`（按仓库惯例未跟踪）。

## 1. 发布

| 项目 | 值 |
|---|---|
| 提交 | `e6efefd`（`feat(zcode): 同步客户端身份到 3.19.1（新增独立 3191 档案）`） |
| 分支 CI | run 45097570961，全部 job 通过（test / test-race / test-repeat / newapi-compat / sdk-compat / docker / frontend / workflows / windows-proxy-journal / 5 个 cross-build） |
| 标签 | `v2.14.3-go`（annotated） |
| Release | run 45098057983：validate 全绿 → 5 个二进制 + 镜像 |
| 镜像 | `ghcr.io/fujunchao/zcode2api-plus:2.14.3-go`，digest `sha256:36dc6bf6282deef3bf1b9bd49a6922d39a3e71369d8aa3ec6d1599dc687978e6` |
| 附件 | zcode2api-linux-amd64 / linux-arm64 / darwin-amd64 / darwin-arm64 / windows-amd64.exe |

中途一次失败：首个提交的回归用例在闭包内用了 `continue`，`go vet` 报
`continue not in for statement`；改为 `return` 后 amend 并复验通过。

## 2. 生产部署

服务器 `154.12.85.101`，`/opt/1panel/apps/local/zcode2api/docker-compose.yml`：

```bash
cp -a docker-compose.yml docker-compose.yml.bak_pre2143_20261123_101141
sed -i 's|zcode2api-plus:2.14.2-go|zcode2api-plus:2.14.3-go|' docker-compose.yml
docker compose pull zcode2api && docker compose up -d
```

| 检查 | 结果 |
|---|---|
| 容器 | `zcode2api` / `ghcr.io/fujunchao/zcode2api-plus:2.14.3-go` / `Up (healthy)` |
| `/meta` | `{"version":"2.14.3-go"}` |
| 版本覆盖 | `docker exec zcode2api env` 无 `ZCODE_CLIENT_VERSION` / `UPSTREAM_USER_AGENT` → 走新默认值 3.19.1 |
| 启动横幅 | `v2.14.3-go · Go`，无「未登记档案」黄字告警 |
| 近 5 分钟日志 | `3012`/`405` 命中 0 次 |

## 3. 探针验收

`docker exec zcode2api zcode2api probe --dry-run`：

```json
{
  "client_version": "3.19.1",
  "system": {
    "profile": "3.19.1", "blocks": 3,
    "first_chars": 35,   "first_sha16": "7779642ea44a4696",
    "identity_chars": 1233, "identity_sha16": "aac4ba4cc7b555b3",
    "dynamic_chars": 4564,  "dynamic_sha16": "929856d5f112c283"
  }
}
```

真机探针（未加 `--dry-run`）：

| 账号 | 模型 | 结果 |
|---|---|---|
| `arkcoh` | glm-5.3-flash | **`ok: true`，HTTP 200** |
| `aszhvp` / `bhpwhw` | glm-5.3-flash | HTTP 200，业务码 `1005 exceed quota limit`（账号额度耗尽） |
| `aszhvp` / `arkcoh` | glm-5.3 | HTTP 429，业务码 `3009 model concurrency limit exceeded` |

`3009` **不是本次改动引入**：用旧身份 `-client-version 3.18.2` 对同一账号同一线路重试，
返回完全相同的 `429/3009`；换账号、换线路也一样。说明是上游当前的并发限流状态，
与档案无关。身份层面另有正面证据：`3.19.1` 身份拿到 HTTP 200 信封与 `ok: true`，
全程没有出现 `3012`/`405`。

## 4. 回退

```bash
cd /opt/1panel/apps/local/zcode2api
cp docker-compose.yml.bak_pre2143_20261123_101141 docker-compose.yml   # 或
# 在 compose 的 environment 段加 ZCODE_CLIENT_VERSION: "3.18.2"
docker compose up -d
```

3180 档案仍随镜像提供，回退不需要换镜像；若显式设过 `UPSTREAM_USER_AGENT`，同步改
`ZCode/3.18.2`。

## 5. 后续观察

- 观察 24 小时：`3012`/`405` 计数、首字延迟、`3008/3009/3010` 并发拒绝比例。
- 客户端下次小版本升级沿用本次流程：精确映射 + 独立档案 + 字节/指纹测试 + 探针。
