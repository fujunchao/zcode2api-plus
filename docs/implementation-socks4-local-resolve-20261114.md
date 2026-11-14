# socks4 本地解析改造（配合订阅放开 socks4）

基线：`v2.14.1-go`。工作分支：`codex/socks4-local-resolve`。

## 背景：代码支持 socks4，但一条也用不上

网关的 socks4 支持是全链路的：URL 校验、手写 SOCKS4/4a 拨号、`DialTunnel`、
订阅入库的 `Canonical` 与入库复检探针都认 socks4（`internal/proxy/client.go`、
`internal/proxy/tunnel.go`、`internal/proxysub/types.go`）。

生产上一条都没进池，原因有两层：

1. 订阅地址带 `exclude_socks4=1`（`meta.proxy_subscription_v1`），源头就被过滤；
2. 更关键的是**协议语义错配**：本项目的 socks4 握手对域名目标一律发 SOCKS4a
   （把域名交给代理解析），而 proxy-checker 用 curl 的 `socks4://` 判定可用性，
   libcurl 对 `socks4` 是**本地解析后按标准 SOCKS4 连 IP**。

2026-11-14 在服务器上用同一批 33 条 socks4 线路做完整 HTTPS GET
（`https://zcode.z.ai/api/v1/client/configs`，10s 超时）：

| 语义 | 成功(200) |
| --- | --- |
| `curl --socks4`（本地解析 = checker 口径） | 22 / 33 |
| `curl --socks4a`（远端解析 = 本项目此前口径） | 2 / 33 |

即：**只删掉 `exclude_socks4=1` 是白改**——入库复检探针用的就是同一份拨号器，
会把这 22 条全部判死。22 条里 `up_kbps>=85` 有 20 条、再排除 `ip_shared` 剩 16 条，
上行读数 721–9469 KB/s，明显高于现有订阅线路（~850–1200 KB/s）。

## 改动范围

- `internal/proxy/client.go`：`socks4Handshake` 增加 `ctx`，域名目标先本地解析
  取 IPv4 再发标准 SOCKS4；解析不到 IPv4 或解析失败才退回 SOCKS4a。IPv6 字面量
  目标两种形态都表达不了，直接报错（此前会发出畸形 4a 请求）。
  解析走新增的 `lookupIPv4` 接缝（`net.DefaultResolver` + 握手预算），测试可替换。
- `internal/proxy/socks4_test.go`：新增三例回归——本地解析发出 IPv4 而非 4a 标志、
  无 IPv4 时回落 4a 且报文尾部是 `域名+NUL`、IPv6 目标早失败。
- `internal/proxy/timeout_test.go`：跟随签名变化。
- `internal/adminapi/proxies.go`：删掉「socks4 无标准库支持直接报错」的过时注释
  （巡检与业务流量共用 `proxy.NewTransport`，socks4 早已可用）。
- 前端：`proxyScheme` 区分 SOCKS4/SOCKS5（此前一律显示 SOCKS5），代理页补 S4
  示例与地址占位文案。

## 验收口径

以最终分支提交 SHA 的完整 CI 与发布门禁为准；本地只做修改、格式化与前端产物构建。
CI 覆盖 `internal/proxy` 新增三例与既有的 socks4 超时/socks5 回归，前端 lint、
测试与 `frontend/dist` 一致性同样在 CI 校验。

## 上线步骤（生产）

1. 发布新版本并部署；
2. 订阅地址改为 `.../api/repo/<token>.json?exclude_shared_ip=1&up_min=85`
   （去掉 `exclude_socks4=1`），先跑 preview 看候选数，再跑 import；
3. 挑 1–2 条 socks4 线路做后台「检测」，再绑 1 个账号验证一次对话与领取，
   观察 30 分钟后再放量（来源池容量 200、单轮新增上限 50）。

## 显式限制

- 免费 socks4 寿命以分钟计、无鉴权，可靠 `stable=1`/`pass_streak` 兜底；
- socks4 只支持 TCP/IPv4 目标，UDP/域名远端解析能力都没有；
- 本地解析依赖网关主机的 DNS：若解析被污染，会把线路指到错误 IP。
  实测 `zcode.z.ai` 只有 A 记录 `43.109.129.220`，风险低；
- 本改造不改变 socks5/socks5h/http/https 的任何行为。

## 执行结果（2026-11-14，生产）

### 代码与发布

- 分支 `codex/socks4-local-resolve`：`a7c59c6` 修复 + `446b2ab` 发版准备（`AppVersion=2.14.2-go`）。
- 分支 CI `43588152963`：14 个 job 全绿（含 test / test-race / test-repeat / 前端产物一致性 / docker / 交叉构建）。
- 标签 `v2.14.2-go`（`446b2ab`）：release `43588513476` 的 validate 全量 CI 与 image 构建全部通过，
  5 平台二进制已上传；镜像 `ghcr.io/fujunchao/zcode2api-plus:2.14.2-go`。
- 已快进合并回 `go-rewrite`（同一 SHA）。

### 生产部署（154.12.85.101）

- compose 镜像 `2.14.1-go → 2.14.2-go`（备份 `docker-compose.yml.bak_pre2142_202611141916`），
  `docker compose up -d` 重建；容器 `Up (healthy)`、重启 0、日志无 socks4/协议报错，`/meta` = `2.14.2-go`。
- 订阅地址去掉 `exclude_socks4=1`：
  `https://prx.120572.xyz/api/repo/user_hp4xz4hnlb.json?exclude_shared_ip=1&up_min=85`。

### 受控导入与真机验证（19:17–19:19）

| 步骤 | 结果 |
| --- | --- |
| preview `880fe8af…` | received 49 / existing 4 / blocked 8 / candidates 37 / probed 15 / passed 10 —— 通过的 10 条**全部是 socks4** |
| import `bdfe4460…`（临时 `import_limit=10`、`enable_imported=false`） | passed 13 / added 10 |
| 后台「检测」两条 | `zcode.z.ai` 与 `api.z.ai` 均 HTTP 200、未拦截（2051ms / 1550ms） |
| 真实流式探测（借最久未用账号 `htvjhf-90c65c07`，探测后已改回原线路） | `socks4://202.146.230.102:1080`：HTTP 200、first_data 1086ms、total 1486ms、**完整收到 message_stop**；`socks4://43.252.238.238:4153`：HTTP 200、first_data 1047ms、total 1363ms、同样完整 |

### 放量与自动同步（19:19 起）

- 十条 socks4 全部启用；订阅配置恢复 `auto=true / enable_imported=true / import_limit=50`（revision 4）。
- 19:35 的自动同步：received 48 / candidates 32 / probed 32 / passed 15 / added 15；
  池子 18 → 32（socks4 22、http 8、socks5h 1、socks5 1）。

### 观察（T+0 → T+30）

- 启用后 10 分钟复测 10 条：7 条通过（2.0–6.0s）、3 条 6s 超时；同一线路存在「19:19 通过、19:26 超时」的抖动。
- T+30 分钟复测 21 条（自动同步已扩到 22 条）：**14 通过 / 7 失败**，失败样本多数是 6000ms 超时。
- 期间账号错误只有既有的 `quota_query_failed`，且都发生在 **http** 订阅线上
  （`proxy-bea40284eb941e18` / `proxy-467a7b779151734e`）；**没有**任何 socks4 相关的业务失败，账号绑定也未发生改派。

### 现实边界与后续

- 免费 socks4 抖动明显、寿命以分钟计：建议观察一轮 30 分钟巡检的退役率，必要时给订阅加 `stable=1` 收紧。
- 入库复检的 6s 预算对 4.5–6.0s 的线路偏紧（本次失败多为超时），属既有预算、不在本次改动范围，可作为下一个优化点。
- socks4 只支持 TCP/IPv4；网关只用 TCP+TLS，无影响。

### 回滚

- 只退订阅：地址加回 `exclude_socks4=1` 阻止新增；已导入的 socks4 线路需手动禁用或删除。
- 退镜像到 `2.14.1-go`：socks4 回到「只发 4a」，已启用的 socks4 线路会大面积探测失败，
  **必须同时禁用这些线路**，否则它们仍会参与账号分配。
