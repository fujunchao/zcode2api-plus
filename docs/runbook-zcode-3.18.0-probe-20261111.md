# 探针 runbook：验证 ZCode 3.18.0 身份与 system 文案

> 适用分支：`codex/zcode-3.18.0-profile`（默认客户端版本 3.18.0）。
> 目的：**不惊动生产**地验证「3.18.0 版本号 + 3180 文案档案 + 首轮环境提醒」是否被上游接受。
> 边界：只读账号库、单账号、单请求；不启调度、不写库、不冷却账号。

## 0. 前置检查（不发模型请求）

在服务器仓库目录执行：

```bash
# 0.1 确认容器/二进制是新构建（产品版本仍可能是 2.13.6-go，客户端版本看下一行）
docker compose run --rm zcode2api status | head -8

# 0.2 只看默认版本的 system 指纹，不发请求
docker compose run --rm zcode2api probe --dry-run
```

3.18.0 预期关键字段（`probe --dry-run` 的指纹在 `system` 对象内，与真实请求报告的顶层字段不同）：

```json
{
  "client_version": "3.18.0",
  "system": {
    "profile": "3.18.0",
    "blocks": 3,
    "first_sha16": "7779642ea44a4696",
    "identity_sha16": "aac4ba4cc7b555b3",
    "dynamic_sha16": "26a2daa8c8381086"
  }
}
```

三项 SHA16 必须同时正确；任何一项不符都不要继续发真实请求（说明镜像/档案/环境变量没有真正生效）。真实请求（下一节）报告才是顶层 `system_first_sha16` 等字段。

## 1. 选择隔离账号

```bash
docker compose run --rm zcode2api probe --list
```

选一个**不参与生产高峰、可接受一次真实调用**的账号；优先显式传 `--account`，不要依赖自动挑选。

## 2. 验证 3.18.0（会发 1 条真实模型请求）

```bash
docker compose run --rm -e ZCODE_CLIENT_VERSION=3.18.0 \
  zcode2api probe --account <账号 id 或 name>
```

预期：

```json
{
  "ok": true,
  "client_version": "3.18.0",
  "system_profile": "3.18.0",
  "system_blocks": 3,
  "system_first_sha16": "7779642ea44a4696",
  "system_identity_sha16": "aac4ba4cc7b555b3",
  "system_dynamic_sha16": "26a2daa8c8381086",
  "http_status": 200
}
```

判定：

| 结果 | 含义 | 下一步 |
|---|---|---|
| `http_status=200` 且无 `business_code` | 3.18.0 版本↔文案配对成立 | 可以计划小流量灰度，观察 24h |
| `http_status=405` + `business_code=3012` | system 文案仍不匹配 | 停止灰度；用 3.18.0 客户端 model-io 重新逐字节还原 |
| `business_code=3007` | 该账号需要验证码 | 换隔离账号，或先确认验证码池状态 |
| `error` 非空 | 网络/代理/账号问题 | 按 `egress` 与报错排查，不改文案结论 |

## 3. 回退对照（同一账号，可选）

```bash
docker compose run --rm -e ZCODE_CLIENT_VERSION=3.17.2 \
  zcode2api probe --account <同一账号>
```

预期回到：

```json
{
  "client_version": "3.17.2",
  "system_profile": "3.17.1",
  "system_blocks": 2,
  "system_first_sha16": "7779642ea44a4696",
  "system_identity_sha16": "4058874023c7541f"
}
```

同一账号在 3.17.2 下返回 200，说明回退通道可用；两条都 200 才能排除「只是账号/网络碰巧可用」。

## 4. 注意

- 如果部署显式设置了 `UPSTREAM_USER_AGENT`，回退时必须同时改成 `ZCode/3.17.2`；只改 `ZCODE_CLIENT_VERSION` 会留下「新版本号 + 旧 UA」的矛盾。
- 探针不写 `last_error`/冷却/计数，失败不会污染本网关账号状态；但上游仍会记录这次调用，**不要在 1 分钟内重复跑**。
- `system_blocks=3` 但 SHA16 不对，说明档案未生效或环境变量覆盖了版本；不要继续。
- 本 runbook 只验证模型请求被接受；领取/额度链路的投放变化需单独观察，不能由 probe 代替。
