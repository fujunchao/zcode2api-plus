# 变更记录：TTFT 第一批止血（2026-11-07）

## 一、做了什么

| # | 动作 | 改前 | 改后 | 位置 |
|---|---|---|---|---|
| 1 | **账号改派到实测快线** | 40 个账号中 20 个绑在 <150 KB/s 线路上 | **40/40 全部绑在实测 ≥605 KB/s 线路** | accounts.db（经后台 API PUT） |
| 2 | **首字节预算收紧** | `ZCODE_UPSTREAM_FIRST_BYTE_TIMEOUT` 未设置 = 默认 **90s** | **25s** | /opt/1panel/apps/local/zcode2api/docker-compose.yml |

执行时间：2026-11-07 09:00–09:10 CST
执行时机：确认当前无活跃请求（`docker logs --since 10m | grep -c '[#]'` = 0）

## 二、为什么

### 改派
- 系统没有任何「带宽感知」的选路逻辑：`selectAccount` 五层筛选（资格/模型/优惠/额度/轮询）全部不看带宽
- `Slow` 慢样本计数器只写不读（`proxy_soft_failure.go:265` 是唯一写入点，全仓无读取点）
- 深探针默认 `shadow` 只记录不改判定；且确认轮 `deep=false`，浅探针 OK 就判 recovered
- 结果：**40% 账号绑在慢线上**，而 324KB 请求体在这些线上要 25–50s 才传完

### 预算
- 90s 预算对 324KB 请求体**等价于「速率 <3.7 KB/s 才中止」**
- 实测 32 个有采样请求，**触发 0 次**（最低 7 KB/s）—— 等于没有判据
- 结果：用户干等 20–57s 才拿到首字，系统全程不动作

## 三、改派依据（实测，非探针）

**关键教训：探针 kbps 不可信。** 实测同一条线路：
- `proxy-730bb93495954c70` 探针报 2547 KB/s，curl 实测 617 KB/s（高估 4 倍）
- `proxy-c0273fb37e246ffc` 探针报 501 KB/s，curl 实测 85 KB/s（高估 6 倍）
- `proxy-ca2b2e1f63ac49cd` 探针报 34 KB/s，curl 实测 2148 KB/s（**低估 63 倍**）

**采用方法**：`curl -x <proxy> -X POST --data-binary @8MiB.bin https://speed.cloudflare.com/__up`，
以 `http_code=200 且 time_total<40s` 为可信判据，`speed_upload/1024` 作为 KB/s。

**为什么不用 Python 的 send 计时**：初版用 Python 手写 socket send 计时，`proxy-70070077`
（本地 ssh -D 隧道）报 183909 KB/s，而 curl 实测只有 2302 KB/s。原因是隧道有深缓冲，
`send()` 在数据进内核队列时就返回了。**必须用「等到完整响应」的计时**。

### 实测快线池（13 条，全部 >=605 KB/s）

| 线路 | 实测 | 账号数 |
|---|---:|---:|
| proxy-70070077 (Azure-Japan-Tokyo) | 2302 | 4 |
| proxy-ec9b22126c077e1c | 2268 | 3 |
| proxy-23bba36df120a11f | 1696 | 4 |
| proxy-8c921fb136beafee | 1605 | 3 |
| proxy-cb313a533f896042 | 1516 | 3 |
| proxy-1e255992dc288e69 | 1217 | 4 |
| proxy-675820f5e294dff0 | 1189 | 3 |
| proxy-017db08b9174cf04 | 1008 | 2 |
| proxy-29af8ef740e4efbc | 968 | 3 |
| proxy-bbc7e3c0f19a0d25 | 894 | 2 |
| proxy-361617ee83a98909 | 801 | 3 |
| proxy-730bb93495954c70 | 617 | 3 |
| proxy-ba05348b0c8f609d | 605 | 3 |

改派前已在池内的 20 个账号未动；改派 20 个，每线占用 2–4/8（上限 8），均衡。

## 四、验证方式

容器重启后已确认：

```bash
docker ps --filter name=zcode2api --format '{{.Status}}'          # Up (healthy)
docker inspect zcode2api --format '{{range .Config.Env}}{{println .}}{{end}}' | grep FIRST_BYTE
# -> ZCODE_UPSTREAM_FIRST_BYTE_TIMEOUT=25
# 账号绑定：40/40 在快线池
# 后台 API: GET /admin/api/accounts -> 200
# 网关: POST /v1/messages -> 401（未带密钥，符合预期）
```

**效果验证需等真实流量**（改动时 channel 4 已 12 小时无请求）。有流量后看：

```bash
# 1) new-api 侧首字分布
cd /opt/1panel/apps/local/new-api/logs
grep -h 'record consume log' oneapi-*.log | grep '"channel_id":4,' | awk '{
  if (match($0,/"frt":[0-9]+/)) { s=substr($0,RSTART,RLENGTH); gsub(/[^0-9]/,"",s); f=s+0 } else next
  if (f<=0) next; n++; sum+=f; if(f>=10000)a++; if(f>=30000)b++; if(f>=60000)c++
} END { printf "n=%d avg=%.0fms >10s=%.1f%% >30s=%.1f%% >60s=%.1f%%\n", n, sum/n, a*100/n, b*100/n, c*100/n }'

# 2) zcode 侧实测上行（应普遍 >600）
docker logs --since 1h zcode2api 2>&1 | sed 's/\x1b\[[0-9;]*m//g' | grep '\[#\]' | grep -oE 'up_kbps=[0-9-]+' | sort | uniq -c
```

**目标**：`>30s` 从 12.9% → <3%；平均 frt 从 13692ms → <5000ms。

## 五、回滚

### 回滚预算
```bash
cd /opt/1panel/apps/local/zcode2api
cp docker-compose.yml.bak_20261107_085114 docker-compose.yml
docker compose up -d
```

### 回滚改派
后台「账号管理」手动改回，或用 `/tmp/final_assign.py` 的日志反向执行。
**注意**：改派本身是纯绑定变更（只改 `ProxyID`/`ProxyURL`），不重置 `device_mid`、不影响登录态、不动凭据，风险低。

## 六、已知局限（重要）

### 1. 线路池每 15 分钟刷新，改派可能被冲掉

订阅配置（accounts.db → meta → `proxy_subscription_v1`）：

```json
{"url":"https://prx.120572.xyz/api/repo/user_hp4xz4hnlb.json?exclude_socks4=1&exclude_shared_ip=1",
 "auto":true,"interval_minutes":15,"candidates":200,"import_limit":50,"capacity":200}
```

**每 15 分钟拉一次，最多导入 50 条。** 当线路被删除时会触发自动改派
（`reassignAccountsLocked` → `chooseProxyLocked` → `leastLoadedProxy`），
而那个算法**只比占用数，不看带宽** —— 账号会被重新分到随机线路上。

本次改派的效果能持续到下一次大规模删线为止。

### 2. 根治路径（按持久性排序）

| 方案 | 持久性 | 状态 |
|---|---|---|
| **proxy-checker 加 `up_kbps` + 订阅 URL 加 `?up_min=85`** | **最持久，零代码** | 等 proxy-checker 改造（方案见 `proxy-checker-uplink-plan.md`） |
| 本项目加「导入后测速」+ 带宽感知选路 | 持久，需改代码 | P1，待排期 |
| 手动改派（本次） | 临时 | 已完成 |
| 首字节预算（本次） | 持久 | 已完成 |

**订阅源当前字段**：`proxy/grade/latency/ip/country/ip_type/recommended_use/target_profile/usable/pass_streak/...`
—— **没有 `up_kbps`**，且 `latency` 对上行吞吐无预测力。等 checker 改完后把订阅 URL 改成：

```
https://prx.120572.xyz/api/repo/user_hp4xz4hnlb.json?exclude_socks4=1&exclude_shared_ip=1&up_min=85&stable=1
```

即可从源头筛掉慢线，**不需要改本项目任何代码**。

---

## 七、本次未做（等确认）

- 首字节超时/连接失败冷却账号（`engine.go:556` 明确不冷却）
- 429 写 avoid（`engine.go:796` 不写，同号换线后照样被选中）
- `Slow` 参与选路排序（既有决定允许，未实现）
- 探针改造（`networkprobe/probe.go:66` 测的是内核缓冲）
- 日志补 `frt_total`

这些都是代码改动，需单独评审。
