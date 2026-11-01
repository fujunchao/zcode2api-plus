# Runbook：客户端升级时如何用抓包校准 system 档案

> 适用场景：ZCode 桌面端升级后，默认伪装版本要跟着升级（`ZCODE_CLIENT_VERSION` → 新版本），
> 但上游按「system 第 0 字节 → 身份段末尾」逐字节校验，文案必须与版本成对。
> 本流程在 2026-11-01 的 3.17.1 升级中实战验证：三轮探针 405 → 200。

## 0. 前置

- 官方客户端能**正常对话**（否则先别动探针：可能上游未放行该版本）。
- 需要 `zcode2api probe`（v2.11.5+）；`--system-file` 需 v2.11.6+。

## 1. 取一份真实抓包（只读，不看对话）

官方客户端每个模型请求都落盘到：

- Windows：`%USERPROFILE%\.zcode\cli\rollout\model-io-*.jsonl`
- macOS / Linux：`~/.zcode/cli/rollout/model-io-*.jsonl`

用仓库脚本提取（**只输出头与 system 指纹，绝不打印 messages**）：

```bash
python temp/incident-20261031/extract_model_io.py --roots "<rollout 目录>"
```

## 2. 对齐三项

| 项 | 取值来源 | 校验点 |
|---|---|---|
| block 0 | 抓包 `system[0].text` | 长度 + sha16 |
| 身份段（第 0 字节 → `# ZCode Desktop Context`） | 抓包 `system[1].text` 前缀 | **字节数 + sha16** 双锁 |
| 桌面上下文段 | 可沿用旧版静态文本 | 含用户私有路径，不参与校验 |

致命坑（3.17.1 踩过）：CLI 源码里身份段含**模板字面量**（`${e?"…":"…"}`），
静态提取会把 `${…}` 原样抄进去 → 多几十字节 → 全量 3012。
**一律以抓包原文为准**，不要用源码字符串直接拼。

## 3. 生成档案

档案结构：`{"blocks":[{"type":"text","text":...},…],"dynamicSections":{…}}`；
block 0 的 `cache_control` 出站时剥离，其余块保留 `ephemeral`。

## 4. 单账号验证（不惊动生产）

```bash
# 4.1 先用抓包文件验证（免发版）
docker cp real3171.json <容器>:/tmp/real3171.json
docker exec -e ZCODE_CLIENT_VERSION=<新版本> <容器> zcode2api probe \
  --account <账号> --system-file /tmp/real3171.json

# 4.2 档案内置后，用默认档案验证
docker exec -e ZCODE_CLIENT_VERSION=<新版本> <容器> zcode2api probe --account <账号>
```

判定：`200` 且 `business_code` 为空 ⇒ 通过；`405 / 3012` ⇒ 身份段仍不一致，
回到第 2 步比对 `system_identity_sha16` 与抓包 sha16。

## 5. 灰度与回滚

1. 只把「一个实例/部署」的 `ZCODE_CLIENT_VERSION` 改成新版本并重启；
2. 观察 24h：出现 `3012` / `unusual activity` / `405` 立即回滚；
3. 回滚＝把变量改回旧版本并重启，不需要换镜像。

## 6. 反面教材（本次真实教训）

| 轮次 | 做法 | 结果 |
|---|---|---|
| v2.11.4 | 只升版本号，文案仍 3.16.x | 全量 405（事故） |
| v2.11.5 | 首段对齐，身份段用 `pBa` 变体 | 405（变体错） |
| v2.11.6 | 换 `dBa` 变体，但模板字面量未展开（1317 vs 1229 字符） | 405（多 88 字节） |
| v2.11.7 | 抓包原文，字节级一致（1235 B） | **200** |

教训：**「看起来一样」不算数，字节才算数。**
