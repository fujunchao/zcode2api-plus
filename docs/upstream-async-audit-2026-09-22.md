# 上游 async 端点审计（2026-09-22）

对象：上游 `gakiyukr/zcode2api-plus`（默认分支 `go-rewrite`，HEAD = tag `v2.0.9-go` = `e4f524f`）。
问题：它的 `POST /async/v1/messages` 实现是否正常，与我们这版差在哪。

## 结论速览

**上游的 async 端点正常，而且比我们完整。** 它在 2026-09-15 有一批「把 async 池对齐网关」的
修复（6 笔），我们只挑走了其中 1 笔与另一笔，**剩下 4 笔从未进入本仓库**——其中 3 笔上游带
回归用例，1 笔混在 M11 批次里。

反过来，我们这边在 async 上比上游多的东西是：405 风控分支、503 递进冷却、`[#]` 诊断行、
`no_account` 池状态分解。即**两边各有领先项**，本次要补的是下面这 4 项。

## 证据：血缘与「挑拣」路径

| 项 | 值 |
| --- | --- |
| 与我们同源的最后一笔上游提交 | `ff97604`（09-08，M3 移植；**同一 SHA**，双方共有） |
| 本地仓库存在的上游提交对象 | 仅到 `7675309`（09-15）为止，且不在本分支历史里 |
| `0d370e5` 及之后的对象 | **本地全无**（`cat-file -e` 失败） |
| 上游 `v2.0.9-go` 与 `0d370e5` 的关系 | `compare/0d370e5...e4f524f` → `status=ahead, merge_base=0d370e5` ⇒ **它是 v2.0.9-go 的祖先** |
| 我们的「同消息、异 SHA」提交 | `55e7f23`≡`7675309`、`5ace74e`≡`d3990b3`、`3536dde`≡`b4d084a` |

即：我们是**逐笔挑**上游提交（消息照搬、SHA 重写），挑了 09-15 批次的一部分与 09-18/19 批次，
**09-15 批次的另外 5 笔漏了**。而 `docs/upstream-sync-2026-09-19.md` 把基线记作
`23903667`（v2.0.7-go）「已同步」，于是这批从未被审——它的结论速览里也没有 async 行。

## 四项缺口

| # | 上游提交 | 上游做了什么 | 我们现状 | 影响 |
| --- | --- | --- | --- | --- |
| 1 | `0d370e5` `fix: route async requests through the account proxy` | `Pool.client()` → `clientFor(acc)`，走 `proxy.TransportForTimeout(raw, 180s)`；`TransportFor` 拆出 `TransportForTimeout` 且**缓存键纳入超时**（网关 120s 与 async 180s 各持一份连接池） | `p.client()` 返回「只设 `ResponseHeaderTimeout` 的裸 Transport」；`internal/asyncpool` 全包对 `ProxyURL` **零引用** | 配了代理的账号在这条路径上**以服务器真实 IP 直连上游**——IP 绑定、地区要求、避开风控全部失效（上游原话：泄露部署 IP） |
| 2 | `e86c5bc` `fix: skip apiKey accounts instead of failing the async ticket` | 选号改成循环：非 jwt 就 `tried[id]=true` 后重选，**先记 tried 再判断** | `if acc == nil \|\| acc.Mode != "jwt" { emitError; return }`（`pool.go:304`） | 池里只要混有 apiKey 账号，轮询落到它就**整票 `no_account` 失败**；且 tried 记在检查之后 ⇒ 下次轮询可能又选中同一账号。表现为「池里有可用 JWT 账号却间歇性失败，与账号状态无关」 |
| 3 | `5105b5d` `fix: record usage and revive status on async success` | 抽出导出的 `gateway.MarkSuccess`（`UseCount++` / `LastUsedAt` / cooling·exhausted→active），engine 与 async 共用 | async 收尾只做 `AccumulateTokens` + 清三个 streak；**本仓库根本没有 `MarkSuccess` 这个函数**（`engine.success` 把逻辑内联着） | ① 后台用量页**漏算 async 流量**（只多 token，不记调用次数/最后使用）；② 冷却**已到期**的账号在 async 成功后仍停在 `cooling`，要等下一轮额度轮询（默认 60s）才回调度 |
| 4 | `6da8df6`（M11 批次里的 async 段） | 入口补 `gateway.NormalizeBody(body, false)` | 只在 `processTicket` 的**副本**上做 `NormalizeBody(actualBody, true)`；入口 `handleAsyncMessages` 没有 | 同步路径 `handler.go:43` 先归一化再 `ModelAllowed`，async 直接拿原始名去判 ⇒ `anthropic/GLM-5.3` 这类 `provider/model` 写法**同步能过、async 返回 400 `model_not_allowed`**；且票内 `model` 名未归一，`Select`/额度比对也用错键 |

补充两条上游**也还没有**的（我们有，回移时不要被覆盖）：405 风控分支、503 递进冷却
（上游仍是 503 直接冷却换号）。

## 测试覆盖对照

上游 async 用例 25 个，我们 33 个。差集：

- **上游有、我们没有（真缺）**：`TestAccountProxyIsUsed`、`TestSkipsAPIKeyAccountsInMixedPool`、
  `TestSuccessRecordsUsageAndRevivesStatus`。
- 上游有、我们**改名等价**（不算缺）：`TestJSONBusinessErrorIsNotTreatedAsStream`≈
  `TestPlainJSON200IsNotForwardedAsStream`、`TestJSONNonZeroCodeDeliveredAsError`≈
  `TestBusinessCode1005In200MarksModelExhausted`。
- 我们有、上游没有（13 个）：405×2、503 阶梯、诊断行×3、池状态分解、529 重试×2 等。

## 回移方案（按可独立验证排序）

每笔都要做「退回旧逻辑 → 用例变红」的反向验证。

1. **代理（`0d370e5`）**：`internal/proxy/client.go` 增 `DefaultResponseHeaderTimeout = 120s` 与
   `TransportForTimeout(raw, timeout)`，`TransportFor` 变成它的薄包装，**缓存键追加超时值**
   （`fmt.Sprintf("%s\x00%d", key, timeout)`）——不加会让先到的超时值污染另一个用途。
   ⚠️ 不要用现成的 `proxy.ClientFor(raw, timeout)`：它设的是 `http.Client.Timeout`（**整体超时**），
   对 SSE 长连接等于给流设了上限，async 必须只在 Transport 层设 `ResponseHeaderTimeout`。
2. **跳过 apiKey 账号（`e86c5bc`）**：`processTicket` 的选号改循环 + 先记 `tried`。
3. **成功复位（`5105b5d`）**：⚠️ 不能照抄上游的窄版 `MarkSuccess`——它只记
   `UseCount/LastUsedAt` 与 `cooling→active`，而我们的 `engine.success` 还额外清
   限流/风控/503 三个 streak 计数。正确做法是把我们 `engine.success` 里那段 `Update` 体
   抽成导出函数，**engine 与 async 共用同一份**，否则回移会顺手丢掉自己的 streak 复位。
4. **入口归一化（`6da8df6`）**：`handleAsyncMessages` 在 `ModelAllowed` 之前加
   `gateway.NormalizeBody(body, false)`（在 `newTicket` 之前，保证票内 model 名已归一）。

## 验证方式

- 每笔补上游同名用例（或本地等价用例）；`go build ./...`、`go vet ./...`、`go test -count=1 ./...`。
- 本机 `-race` 不可用（无 gcc/cgo），由 CI 覆盖。
- 本机已知无关失败：`internal/captcha` 的 `TestExtractTarGzPreservesSymlink`（沙箱把符号链接
  降级为零字节文件）。
- 缺口 1 的用例照上游写法：起一个最小 CONNECT/绝对 URI 代理记录被转发的目标，断言上游是
  **经代理**被访问的；退回直连即红。

## 附：审计方法（可复用）

1. `gh api repos/<upstream>/git/trees/<branch>?recursive=1 --jq '…'` 定位文件；
2. `gh api -H "Accept: application/vnd.github.raw" repos/<up>/contents/<path>?ref=<tag>` 取原文
   （比 base64 解码省事）；
3. `gh api repos/<up>/commits?path=<file>&sha=<branch>` 列该文件提交史 —— **这是找出「漏挑」的关键**，
   只看 tag/版本号会漏掉这种批次性缺口；
4. `gh api repos/<up>/compare/<old>...<new>` 判血缘（`merge_base` 是否为旧提交）；
5. 本地 `git cat-file -e <sha>` / `git merge-base --is-ancestor <sha> HEAD` 证「对象从未进来」；
6. 逐笔取 patch：`gh api -H "Accept: application/vnd.github.patch" repos/<up>/commits/<sha>`。
