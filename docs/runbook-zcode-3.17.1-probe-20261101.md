# 探针 runbook：验证 3.17.1 的 system 文案方案

> 适用版本：v2.11.5-go 及以后（`zcode2api probe` 子命令）。
> 目的：**不惊动生产**地验证「版本号 + 官方 system 文案」是否成对，从而判断 3.17.1 能否启用。
> 边界：只读账号库、单账号、单请求、不启调度、不写库、不冷却账号。

## 0. 为什么不能用「传流量」验证

上游从 system 第 0 字节校验到身份段末尾，并按 `app_version` 期望对应文案；
直接把生产流量切到 3.17.1 会让每个请求 405/3012，网关随即按 `ZCODE_RISK_COOLING_STEPS`
（默认 300/900/3600 秒）阶梯冷却账号，超阶梯还会置 `invalid`。探针把这件事压到
「一个账号、一条请求」。

## 1. 前置检查（都不发请求）

```bash
# 1.1 确认容器里是新版本（应输出 2.11.5-go）
docker compose run --rm zcode2api status | head -5

# 1.2 看当前默认版本的 system 指纹（应为 3.16.x 档案、block0 含 "coding"）
docker compose run --rm zcode2api probe --dry-run

# 1.3 看候选账号（只读；不会发请求）
docker compose run --rm zcode2api probe --list
```

把 `1.2` 的输出贴回给维护者，确认 `system_profile=3.16.x`、`system_first_text` 含
`interactive coding agent`。

## 2. 验证 3.17.1 文案（会发 1 条真实请求）

选一个**不参与生产高峰**的账号（`--list` 里 last_used_at 最早的那个，或显式指定）：

```bash
# 指定账号（id 或 name 都可以）
docker compose run --rm -e ZCODE_CLIENT_VERSION=3.17.1 \
  zcode2api probe --account <账号 id 或 name>

# 或者不指定，让探针自动挑「最久未使用」的
docker compose run --rm -e ZCODE_CLIENT_VERSION=3.17.1 zcode2api probe
```

预期输出（关键字段）：

```json
{
  "ok": true,
  "client_version": "3.17.1",
  "system_profile": "3.17.1",
  "system_first_text": "You are ZCode, an interactive agent",
  "system_first_sha16": "…",
  "system_identity_sha16": "…",
  "account": "…",
  "device_mid_source": "account_virtual",
  "egress": "direct 或 proxy:https://…",
  "http_status": 200,
  "elapsed_ms": 1234
}
```

判定：

| 结果 | 含义 | 下一步 |
|---|---|---|
| `http_status=200` 且无 `business_code` | 版本↔文案配对成立 | 可以计划灰度：先小流量实例开 3.17.1，观察 24h |
| `http_status=405` + `business_code=3012` | 文案仍不匹配（或还缺新的会话特性头） | 需要用官方 3.17.1 客户端抓一版 model-io 逐字节还原 |
| `business_code=3007` | 该账号当前需要验证码 | 换一个账号，或用 `ZCODE_SKIP_MODEL_CAPTCHA` 策略确认 |
| `error` 非空 | 网络/代理/账号问题 | 按 `egress` 与报错排查，不影响结论 |

## 3. 注意

- `device_mid_source` 若为 `ephemeral`，说明账号没有独立指纹、且数据目录没有
  `device_mid.txt`——此时上游可能把请求当成新设备，结论会失真；请先修好指纹再测。
- 探针**不会**写 `last_error`/冷却/计数，所以失败不会污染账号状态；但上游侧仍会记录这次调用。
- 不要在 1 分钟内重复跑：单账号连续 405 会提高上游对该身份的敏感度。
