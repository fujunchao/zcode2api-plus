# 领取失败根因：优惠套餐的投放入口已从 billing/preview 换成 marketing/touch

- 日期：2026-11-10（Asia/Shanghai）
- 现象：本地官方客户端能领取「优惠套餐」，服务器上部署的本项目（v2.13.5-go）一律领取失败
- 结论：**本项目用 `GET /api/v1/zcode-plan/billing/preview` 发现可领套餐，该端点现已不返回 `data.plans`；
  领取的套餐号（plan_id）现在只能从营销触达 `GET /api/v1/marketing/touch` 的 delivery 里取。**
  因此 `Claim(acc, "")` 永远走「沒有待領取的套餐」分支，从未真正 POST `/billing/claim`。

> 快照口径：本文所有「账号状态」数字都带时点，**不要**用服务端日志里是否还有
> 「上游无投放套餐」当验收判据——那批日志在修复部署前必然一直存在。

## 证据

### 1. 服务端日志与数据（154.12.85.101 / 容器 zcode2api）
- 日志里全是「上游无投放套餐，跳过领取」「定时领取完成：成功 0，失败 50」。
- **2026-11-10 19:5x 快照**：`accounts.db` 里 49 个 JWT 账号 `claim.claimed_at` 全为 null（从未成功过一次），
  11 个留的 `claim.last_error` 是「沒有待領取的套餐」。
  （20:0x 用 marketing/touch 的 plan_id 批量补领后的状态见 §6，快照时间不同。）

### 2. 复现（只读，走本项目自己的代码路径）
```
GET /admin/api/claim/preview?account_id=wpdcat-145d9fb6
→ {"preview":[{"account_name":"wpdcat","activated":true,"error":null,"plans":[]}]}
```
即 `PreviewPlans` 返回空 → `Claim` 报「沒有待領取的套餐」。

### 3. 上游端点直测（服务器侧，带账号 JWT）
| 请求 | 结果 |
| --- | --- |
| `GET /api/v1/zcode-plan/billing/preview?app_version=3.17.2&platform=windows-x86_64` | `{"code":0,"msg":"","logid":...}` — **没有 data** |
| 同上，`platform=win32-x64`（官方 `nD()` 形态） | 同上，无 data |
| 同上，不带 platform 参数 | 同上，无 data |
| 同上，带本项目完整来源头（UA/X-Title/X-Device-Mid…） | 同上，无 data（一次 429 为瞬时限流，重测 200） |
| `GET /api/v1/marketing/touch?seq=0` | `code:0`，deliveries[] 里 banner 按钮 `action.type="claim_zcode_plan"`，`args.plan_id="zcode-v3-start-plan-1110-nt"` |

三个不同账号（inboea / tlcmlk / yjdftr）的 marketing/touch 均返回同一活动：campaign `2101141259813175296`，
plan `zcode-v3-start-plan-1110-nt`，名 `ZCODE GLOBAL BUILD`，权益 GLM-5.3-Flash 300,000,000 token、`ends_at=1794358800`（本地 2026-11-11 15:00）。

### 4. 官方客户端源码（D:\Programs\ZCode\resources\app.asar，v3.17.2）
- `getManualClaimPlanPreviews()`：`GET /api/v1/zcode-plan/billing/preview?app_version&platform`，
  **只带 Authorization**（无版本/平台头、无 Content-Type），`if(!r.data) throw "manual_claim_preview_missing_data"`
  —— 官方自己的这条路径现在同样拿不到套餐，说明 preview 已不再投放。
  `platform` 取 `nD() = process.platform + '-' + process.arch`（Windows 即 `win32-x64`）。
- 领取的真实入口在营销触达：delivery 的按钮 `action.type==="claim_zcode_plan"`，
  点击后 `claimManualPlan({planId: e.args.plan_id, ...captchaToken})` → `POST /api/v1/zcode-plan/billing/claim` body `{plan_id}`，
  请求头为 `Authorization / Content-Type / …captcha 头 / X-ZCode-App-Version / X-Platform`。
- 领取成功后回执：`POST /api/v1/marketing/touch/action` body `{campaign_id, action_type:"confirm"}`，且是 `.catch(() => warn)` 不阻断。
- marketing/touch 请求头**只有四项**：`Authorization`、`X-Device-Mid`、`X-Client-Language`、`X-ZCode-App-Version`；
  `seq` 进程内 `String(Xde++)`（首调 0）；`timeoutMs=15e3`、`redirect:"error"`。
  **`X-Device-Mid` 必须是合法 UUID**：官方有 `uuid().parse` 断言，实测传 `n/a` 会 HTTP 400 / code 3001 parameter error。
- delivery 是按 `resource_position` 判别的联合：`banner` 形态与 `popup` 形态**互斥**；
  `success_popup` 嵌在 banner 内、`hero` 嵌在 popup/success_popup 内，`zcode_plan` 元数据三处都可能挂：
  `banner.background.args.zcode_plan`、`banner.success_popup.hero.args.zcode_plan`、`popup.hero.args.zcode_plan`。

### 5. 本地成功日志（~/.zcode/v2/logs/2026-11-10.log）
`19:55:13 marketing-touch.query OK` → `19:55:14 coding-plan-subscription.getRequestVerificationConfig` →
验证码 `outcome:"success"` → `19:55:17 coding-plan-subscription.claimManualPlan OK (166ms)` → `marketing-touch.report OK`。
领取后 `billing/balance` 出现 `plan_id=zcode-v3-start-plan-1110-nt / ZCODE GLOBAL BUILD / GLM-5.3-Flash 3e8`。

### 6. 反证与批量补领结果
用 marketing/touch 拿到的 plan_id 走本项目现有领取链路：
```
POST /admin/api/claim {"account_ids":["wpdcat-145d9fb6"],"plan_id":"zcode-v3-start-plan-1110-nt"}
→ {"outcomes":[{"ok":true,"plan_id":"zcode-v3-start-plan-1110-nt","next_at":1794358800}],"summary":{"ok":1,"fail":0}}
```
该账号 `claim.claimed_at` 有值、`plans` 里出现 300M GLM-5.3-Flash；再查 marketing/touch → **deliveries 变成 0**
（活动按账号投放、领过即消失）。即：验证码求解、claim 请求、落盘冷却这条链路本身是好的，**坏的只有 plan_id 的来源**。

**2026-11-10 20:20 批量补领（全部 JWT 账号，同一 plan_id，耗时 238s）**：`ok:35 fail:14`；
当时时点 `accounts.db` 统计：49 个账号中 36 个 `claimed_at` 有值（含手动验证的 wpdcat）。
13 个未领：12 个上游回 `request has been blocked due to unusual activity.`（集中在 102.244.78.61:8086 ×6 /
204.199.106.226:999 ×2 等少数线路上），1 个 `unexpected EOF`（线路抖动，可重试）。

### 7. 「0 条投递」是瞬态（claim-verify 独立复算）
账号 kjvbpw（`claimed_at=null`、plan 还是旧 `zcode-v3-start-plan-0817`）：
20:22 查 marketing/touch → `deliveries: []`；20:36 同账号、同 UUID 头、中间无任何领取动作，连测 3 次 → `deliveries` 长度 1（`zcode-v3-start-plan-1110-nt`）。
10 账号相关性快照：已领本次活动的账号 delivery=0，未领的 delivery=1 —— 方向对，但不能把「0 条」当成瞬时可靠判据。
因此新链路对「无投递」只给可重试语义（短冷却），不给长冷却。

另：preview 抽了 **18 个不同账号**（含已领/未领/不同 plan），全部返回 `keys=[code,logid,msg]、code=0、无 data` ——
这条路对当前全部账号都是死的，保留它只作兜底。

## 修复（分支 codex/marketing-touch-claim）
1. `internal/claim/marketing.go`（新增）：营销触达客户端 —— `GET {MarketingBaseURL}/marketing/touch?seq=<自增>`（四头、15s、不跟随重定向、走 `clientFor`），
   按 banner/popup 两种形态取 `claim_zcode_plan` 的 `args.plan_id` 与 `campaign_id`，并从三处 `zcode_plan` 补 name/priority/grants。
2. `ClaimablePlans(acc)`：**营销触达优先**，为空回退 `billing/preview`；`PreviewPlans` 改为它的别名，
   于是 `AutoClaimAllPlans` / `RunInitialization` / `/admin/api/claim/preview` / `POST /admin/api/claim` 全部看到同一份套餐。
   营销触达不可用而回退路径抛 `ClaimError` 时，以回退错误为准（保住业务码与 proxyguard 依赖的 `endpointUnreachable/riskControl` 信号）。
3. 领取成功后回执 `ReportMarketingClaim`（`confirm`）；失败只 warn，不影响领取结论。无投递时返回
   `ClaimError{msg:"該賬號當前沒有可領取的活動套餐"}`（**刻意不带 1005**：营销触达的「0 条」是瞬态，见 §7；
   带 1005/1003/3007 会落长冷却，一次空响应就把可领账号锁一小时）。
4. 回归测试：`internal/claim/marketing_test.go` 覆盖「preview 恒空 + 营销触达有投递」的端到端发现/领取/回执、
   banner 与 popup 两种形态、seq 自增与四头形态、无投递文案与 1005、回执失败不阻断、回退语义、UUID 设备指纹。
