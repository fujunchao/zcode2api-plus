# zcode2api

Z.AI ZCode Coding Plan → OpenAI/Anthropic 兼容網關（**Go 版，現為主線**）。

把 Z.AI Coding Plan 賬號池包裝成標準 API：多賬號輪詢（優惠額度優先）、驗證碼全自動求解、
額度與套餐到期監控、OAuth 登錄、賬號級出站代理、活動套餐自動領取、賬號歸檔，
單二進制交付（前端已內嵌，無外部運行時）。

仪表板与账号池分别汇总 **GLM-5.3** 与 **GLM-5.3-Flash** 的总额度、已用和剩余，
同一模型跨账号、跨套餐累加，不再混合两个模型的额度。
统计采用非归档账号的最近上游快照（含停用、冷却及失效账号），并非当前可调度额度；
未获取数据与额度为零分别展示。升级无需数据库迁移。

> 📦 歷史沿革：本項目原為 Python 實現，現已由 Go 重寫版取代成為主線。
> Python 舊版保留在 [`python-legacy`](../../tree/python-legacy) 分支（僅歸檔維護，不再更新）。

## 端點一覽

| 端點 | 協議 | 說明 |
|------|------|------|
| `POST /v1/messages` | Anthropic Messages | 流式/非流式，字節級透傳 |
| `POST /v1/chat/completions` | OpenAI Chat | 本地轉換為 Anthropic Messages，含工具調用 |
| `POST /v1/responses` | OpenAI Responses | 服務 Codex CLI（無狀態模式；`previous_response_id` 返回 400） |
| `POST /async/v1/messages` | Anthropic 異步 | ticket + keepalive + 流中斷語義 |
| `GET /v1/models` | 雙兼容超集 | Anthropic 與 OpenAI 形態字段並存 |
| `/admin/*` | — | 內嵌 React 管理後台 |

對外三種請求格式，內部統一走 Anthropic Messages 上游管道：選號循環、驗證碼求解、
錯誤分類（401/402/429 碼族/3010/F001）、賬號狀態機（瞬時限流先原地重試一次，
冷卻時長按連續限流次數遞進）與用量統計只維護一份。

## 快速開始

```bash
# 容器（最快）：鏡像見 GHCR，或就地從源碼構建
docker compose up -d
docker compose logs zcode2api | grep -E '初始后台密码|网关 API Key'

# 或裸二進制：下載現成產物（Releases 頁：linux / darwin / windows × amd64 / arm64）
# 或源碼構建：
go build -o zcode2api ./cmd/zcode2api
./zcode2api serve            # http://127.0.0.1:3000

# 驗證碼自動求解：首次啟動自動下載補丁 Chromium（約 200MB，無需 Python）；
# 也可用 ZCODE_CAPTCHA_BROWSER_BIN 指定已有的瀏覽器二進制
ZCODE_CAPTCHA_BROWSER=true ./zcode2api serve
```

首次啟動橫幅輸出後台密碼與網關 API Key（也可 CLI 設定）。
瀏覽器求解不可用時自動回退人工回填（後台 `/admin/captcha`），功能不中斷。

ZCode 3.14.4 起，网关识别上游 `captcha.skip_model_request`：明确为 `true` 时，
同步、异步及 OpenAI 兼容模型请求均跳过验证码，不依赖浏览器或人工回填；
**套餐领取仍保留验证码**，因此自动领取场景不要关闭浏览器求解开关。
该策略使用既有配置缓存（默认 10 分钟），字段缺失或配置获取失败时保留原验证要求。
默认客户端版本已同步至 `3.14.5`；若部署显式设置了 `ZCODE_CLIENT_VERSION` 或
`UPSTREAM_USER_AGENT`，需同步检查这些覆盖值。升级只需替换网关程序/镜像并重启，无数据库迁移。

`v2.9.10-go` 改善现有请求的一致性，不扩展官方 API Key 套餐适配：
- 模型 UA 默认声明 Node `24`，与已核实的 ZCode 3.14.5 随包运行时基线一致；
  网关仍为 Go 单二进制，不因此增加 Node 运行依赖。显式 `ZCODE_CLIENT_NODE_MAJOR` 仍优先。
- 客户端配置查询使用查询平台格式（如 `windows-x86_64`），保留 `ZCODE_CLIENT_PLATFORM` 覆盖并转换已知别名；不改动余额和领取查询。
- JWT 的标准 system 块精确去重；调用者段落、不同缓存策略及附加字段原样保留。
- 异步限流/过载原地重试逐次生成新的上游请求 ID，会话/轮次标识与正文保持一致，原有重试策略不变。
- `[attempt]` 日志末尾追加账号模式、目标 origin、上游请求 ID，以及会话/追踪/轮次的短哈希引用；
  新摘要不记录正文、认证头、URL 用户信息、路径、查询参数或片段。详见 [发布说明](docs/releases/v2.9.10-go.md)。

上游 API Key 账号的模型额度耗尽后，普通网关会在**正常候选用尽**时进行有界恢复探测：
等待 5 分钟后借一次真实请求检查，未成功则依次等待 15 分钟、1 小时、6 小时（封顶）；
欠费、套餐过期或不包含模型的明确业务码从 6 小时等待起步。同一账号/模型只允许一个探测在途，
每个入站请求最多探测一个已耗尽账号；没有流量或仍有健康候选时，不主动产生探测请求。
完整成功才解除目标模型的耗尽标记，不解除风控、未到期冷却、归档或人工停用。
等待状态保存在现有数据库内部，重启继续生效；JWT 仍按余额刷新恢复，异步池仍只用 JWT。
这不是 Coding Plan 手动重置功能，也不会领取或消耗重置机会。

## CLI

```
zcode2api serve [--port 3000]        啟動網關 + 後台 UI
zcode2api login zai [--no-browser]   OAuth 登錄 Z.AI 並入池（自動領取活動套餐）
zcode2api add-account zai <name> <jwt|key>
zcode2api accounts [zai]             查看賬號列表
zcode2api remove-account <provider> <id|name>
zcode2api quota                      查看各賬號實時額度
zcode2api status                     配置概覽
zcode2api set-admin-key <key>        設置後台密碼
zcode2api export [file] / import <file>   賬號導出/導入（與 python-legacy 互通）
```

同一个账号库只允许一个服务或 CLI 进程直接访问。服务运行期间，涉及账号库的 CLI
命令会明确拒绝执行，请使用管理后台，或先停止服务再运行 CLI。数据库旁的
`accounts.db.lock` 是正常的锁文件，不需要删除；进程正常或异常退出都会由操作系统
释放锁。该限制从本次修复版本起生效，旧版本和外部 SQLite 工具不会自动遵守此锁。

## 部署

### 方式一：Docker（推薦）

```bash
docker compose up -d            # 同目錄的 docker-compose.yml，預設從源碼構建
docker compose logs zcode2api | grep -E '初始后台密码|网关 API Key'
```

CI 推 `v*` tag 時會構建 `linux/amd64` + `linux/arm64` 多架構鏡像並發到
`ghcr.io/fujunchao/zcode2api-plus`（用上現成鏡像可把 compose 裡的 `build:` 註釋掉，
改指該 image；公開倉庫的 package 默認繼承 Public 可見性，匿名即可 pull——
若你把倉庫改成私有，需到 package 的 Settings 自行調整）。

數據持久化在命名卷 `zcode-data`，內含 `accounts.db`、`device_mid.txt` 與補丁 Chromium 緩存；
**首次求解驗證碼時**自動下載約 200MB 補丁 Chromium（僅一次，走卷持久化）。

收到 `SIGTERM`（`docker stop` / `docker compose down`）時會先停止接受新連接、等待在途請求
收尾（上限 10 秒）再退出，存儲與瀏覽器池都會正常關閉，不會被拖到容器超時強殺。
HTTP 生命周期会等待 `Shutdown` 完成后才清理依赖；超出等待预算时主动关闭剩余连接，
取消仍在运行的请求，而不是把监听关闭误认为请求已经全部结束。
后台额度刷新、自动领取、线路巡检同时响应停机取消；HTTP 与后台收尾共享上述
10 秒总预算，而非各自再等待 10 秒。超出剩余预算时记录清理未完成并退出，不无限等待。

三個容易踩的坑：

- 應用**不讀 `.env` 文件**（只認進程環境變量）。同目錄的 `.env` 之所以生效，是因為 compose
  讀它來做 `${VAR}` 插值——所以變量必須在 `docker-compose.yml` 的 `environment:` 裡顯式引用，
  寫進 `.env` 卻沒被引用的不會進容器；把 `.env` 放進 `/data` 更是完全無效。
- 想沿用宿主機既有的 `./data` 目錄時，bind mount 前先 `sudo chown -R 10001:10001 ./data`
  （鏡像以 uid 10001 非 root 運行），或改用命名卷。
- **前面掛了反向代理（Nginx / Caddy 等）時，把端口改成只綁回環**
  （`- "127.0.0.1:${ZCODE_PORT:-3000}:${ZCODE_PORT:-3000}"`）。反代轉發後 `RemoteAddr`
  恆為代理地址，後台登入的「單 IP 10 次失敗」限速會退化成**全局限速**——任何人的十次
  失敗都能鎖死整個後台。只綁回環可確保外部只能經反代進來，同時讓這個桶不再對外部流量生效。

### 方式二：裸二進制 + systemd

```ini
# /etc/systemd/system/zcode2api.service
[Service]
WorkingDirectory=/opt/zcode2api
Environment=ZCODE_PORT=3010
Environment=ZCODE_DATA_DIR=/opt/zcode2api/data
Environment=ZCODE_CAPTCHA_BROWSER=true
ExecStart=/opt/zcode2api/zcode2api serve
Restart=on-failure
```

> 💡 驗證碼瀏覽器：啟動時自動從 cloakbrowser.dev 下載補丁 Chromium（SHA256SUMS +
> Ed25519 簽名校驗，GitHub Releases 兜底），緩存於 `~/.cloakbrowser/`（容器內為
> `/data/cloakbrowser`），零 Python 依賴。
> 下載源可用 `CLOAKBROWSER_DOWNLOAD_URL` 覆蓋；`ZCODE_CAPTCHA_BROWSER_BIN` 可指向
> 任意已有瀏覽器。實測部分發行版自帶 Chromium（如 Debian 150）會被風控拒絕——
> 自動下載的補丁二進制即為此問題的內建解法。

## 配置（環境變量）

核心項（全部見 `internal/config/config.go`）：

| 變量 | 默認 | 說明 |
|------|------|------|
| `ZCODE_PORT` / `ZCODE_HOST` | 3000 / 0.0.0.0 | 監聽地址 |
| `ZCODE_DATA_DIR` | `./data` | 賬號庫、密鑰、全局設備指紋（每賬號指紋存於賬號庫） |
| `ZCODE_CAPTCHA_BROWSER` | false | 啟用 rod 瀏覽器池自動求解 |
| `ZCODE_CAPTCHA_BROWSER_BIN` | 自動發現 | Chromium 二進制路徑 |
| `ZCODE_ASYNC_ENABLED` | — | 掛載 /async/v1/messages 空閒池 |
| `ZCODE_LINE_TRUNCATE_STRIKES` | 3 | 同一線路連續 N 次上游側斷流即移除線路並改派綁定賬號（0=關閉；後台可在線改） |
| `ZCODE_LINE_TRUNCATE_AVOID_SECONDS` | 60 | 斷流後該賬號選號回避秒數（僅選號層軟過濾，不是冷卻；0=關閉） |

## 賬號級出站代理

賬號配置 `proxy_url` 後，該賬號的網關請求、額度查詢與套餐領取均走對應代理；
支持 `http(s)://`（CONNECT）與 `socks4://`、`socks5://`、`socks5h://`（socks5h
由代理解析域名）。代理無效時回退直連並記錄 `last_error`。

**登入時即可選線路**：新增帳號對話框的「授權登入」頁有出口線路下拉，
本次登入會用它完成授權碼交換與 API Key 兌換，登入成功後線路一併寫入賬號
（`/admin/api/login/start` 接受 `proxy_id`，或直接給 `proxy_url`）。
會這樣做是因為「登入 → 兌換 API Key → 刷新額度 → 領取活動」是同一條出站鏈路，
只在登入本身走代理等於拿真實 IP 去打上游。線路不存在或協議不受支持會直接 400。

**自动分配代理**：新增、批量新增、导入和自动登录选线时，优先使用启用的空闲代理；
没有空闲代理则共享绑定账号数最少的代理，只有无可用代理或分配失败才回退直连。
绑定数包含停用、冷却和归档账号；批量分配逐个更新计数。新增时显式选择代理或直连不受影响。

**删除代理后的重分配**：手动删除与批量清理均优先使用剩余的启用空闲代理；没有空闲代理时，
按绑定账号数最少逐个共享分配，只有没有启用代理时才回退直连并清除旧地址。
删除与重绑在同一事务中完成，写入失败时全部保留原状；不修改其他账号或历史上已直连的账号。

**风控自动换代理**：消息请求命中风控后，同步和异步路径都会尝试为该账号更换其他启用代理，
仍按“空闲优先 → 绑定最少”选择，排除原代理、相同地址及该账号最近 24 小时触发过
消息风控的线路（最多最近 64 条，重启保留）。没有未失败的替代代理则保留原绑定，
不来回切换旧线路或回退直连；管理员仍可手工指定线路。
换代理不解除原有冷却或失效状态，不立即重试，也不影响原代理上的其他账号。

**出站诊断日志**：`[attempt]` 为每次 HTTP 调用记录请求/票 ID、调用序号、账号 ID、
线路 ID/名称、脱敏端点及响应头状态；原地重试也单独编号。`risk-proxy` 记录换线前后
线路和 `result`：`changed` 已改派、`recent_routes_exhausted` 近期线路已用尽、
`no_alternative` 无其他候选、`stale_snapshot` 绑定已变化、`egress_override` 实际未使用
绑定代理、`persist_failed` 落库失败。强制直连/非法代理回退不将风控归因给未使用的代理。
端点仅表示代理配置入口，不代表已验证公网 IP 改变；`event=headers status=200` 也不代表
响应体完整成功，完整结果仍看原有 `[#]` 汇总。

**初始体验额度异常与领取风控联动**：后台新号自动领取、定时领取和手动领取时，
若有效额度查询确认账号尚无初始套餐和已分配额度，且领取链路明确命中风控，则自动
删除该次请求使用的命名代理，并将其全部绑定账号按“空闲优先 → 最少绑定”改派；
无启用代理时清除旧地址并回退直连。已有套餐（即使额度用尽）、成功使用/领取过的账号、
额度查询失败或验证码失败不会触发删除。保留领取冷却，不立即换线重领；程序重启或
更换代理后，需要新的有效额度查询证据，避免凭旧快照误删。

## 套餐自動領取

JWT 賬號入池（批量添加 / OAuth / CLI login）後自動：激活事件上報 →
`billing/preview` 按優先級逐個 `billing/claim`（驗證碼 3007 自動換碼重試一次）。
後台賬號頁另有「領取套餐」按鈕（工具欄全量 + JWT 賬號行內單賬號）。

領取狀態會落盤到賬號（`claim.next_at` 為上游給出的下次可領時間，前端顯示倒計時）。
冷卻只作用於**自動**路徑：手動點按鈕始終可強制領取——用戶點了沒反應是最糟的交互。
自動領取走單槽串行閘門，批量導入多個賬號時不會同時轟驗證碼池。

**設定在後台「系統設定」頁**，改動即時生效：入池自動領取開關、每日定時領取
（默認關閉，時間可配，默認 23:00 本地時區）、三個冷卻時長。定時批量尊重上游給的
下次可領時間，剛領過的賬號會跳過；`ZCODE_CLAIM_*` 環境變量只是默認值。

## 上游風控（HTTP 405 + `unusual activity`）

上游用 405 表達風控攔截。它看的是**身分維度**（賬號、設備指紋、出口 IP、請求頭），與請求的
哪個模型無關，所以處置是**停整個賬號**，而不是只灰一個模型——換模型照樣被攔。

- **判定看 body**，不看狀態碼：405 在本項目有三種含義（風控攔截 / 計費接口的重复查詢 /
  JWT 缺頂層 `system` 注入時上游回的錯）。第三種是我方構造請求的缺陷，每個賬號都會一樣地
  失敗，**不會**被當成風控去冷卻賬號。
- **階梯冷卻**：連續第 N 次命中取第 N 檔，默認 `300,900,3600` 秒（5／15／60 分鐘）。
  **連續次數超過檔位數則賬號置為失效**（需人工介入）——檔位數同時就是升級點，可在後台
  「風控冷卻」卡片裡改，環境變量 `ZCODE_RISK_COOLING_STEPS` 只是默認值。
- **恢復**：冷卻到期即重新參與調度；到期後成功調用一次就把計數歸零（回到最低檔）。
  人工換憑據也會清零計數——否則救回來的號下一次命中就會立刻又判失效。
- 冷卻期間該賬號不參與調度，也不做套餐領取。

## 賬號身份與設備指紋

- 入池判重按 **user_id → email → 憑據** 三級：同一個號重新登錄時 token 字節會變，
  只看憑據會把它建成兩條記錄。`user_id` 取自 JWT payload（`sub` 兜底），
  手動添加 / 導入 / CLI 路徑自動獲得。
- 每個賬號有**獨立的設備指紋**（`virtual_device_mid`）。此前全倉共用一份全局
  `device_mid.txt`，同一台機器上的多個賬號會被上游按設備關聯；缺失時才回退全局值。
  升級後首次啟動會為存量賬號一次性補齊（冪等）。

## 工具调用与思考配置

网关支持函数工具协议转换，实际工具由 Pi 等客户端执行，不在服务端执行命令。
Chat Completions 支持 `tool_calls`、`reasoning_content`；Responses 补齐文本、思考摘要及函数工具的流事件。
GLM-5.3 / GLM-5.3-Flash 的原生思考档位为 `low/high/max`，默认且推荐 `max`，不能关闭思考。
`v2.0.4-go` 修正了旧版错误拒绝 `max` 的问题：按官方 Coding Plan 规则兼容别名，
并传递原生 effort，不再把思考程度替换成固定 token 预算；输出上限仍由调用方控制。
不支持的工具类型、`strict:true`、真正未知的档位或显式关闭思考请求会明确返回 400。

Pi 专用配置见 [examples/pi-models.json](examples/pi-models.json)；字段解释、测试命令和兼容边界见
[客户端兼容说明](docs/client-compatibility.md)。已有 Pi 配置请合并条目，不要覆盖其它提供商。

## 发布与开发

- **本地开发，远端验证**：本地修改代码、格式化和检查差异，不再重复执行 Go 或前端自动化测试。具体协作规则见 [AGENTS.md](AGENTS.md)。
- 推送分支或提交拉取请求后，GitHub Actions 的 Ubuntu 环境执行工作流语法检查、前后端静态检查和测试、Go 竞态检测、重复回归、SDK 兼容性、五平台交叉编译及容器构建。
- 修改前端时可在本地生成并提交 `frontend/dist`；是否通过测试、产物是否一致仍以当前提交的远端 CI 结果为准。
- **发版强制门禁**：先等待当前提交的分支 CI 通过，再推 `v*` 标签。发布工作流会再次复用同一套完整 CI；只有全部成功，才上传二进制和推送镜像。失败或取消不能发布，不接受其他提交的通过记录。
- **不发版验证**：手动运行 `release` 工作流只执行验证，二进制发布和镜像推送任务会跳过；可用 `gh workflow run release.yml --ref <工作分支>` 触发，无需创建测试版本标签。
- Windows 二进制继续交叉编译；需要验证 Windows 特有行为时，在远端增加针对性测试，不恢复本机全量测试。
- 行为契约与里程碑台账见 `PLAN.md`；`HANDOFF.md` 保留历史交接记录，当前开发验证流程以本节、`AGENTS.md` 和实际工作流为准。

## 賬號歸檔

不再需要調用的賬號可「歸檔」：歸檔即強制停用並從賬號池隱藏，僅在後台歸檔區
保留記錄（累計用量可查）；歸檔賬號不參與調度、套餐領取與額度刷新，恢復後
保持停用狀態，需手動啟用才會重新入池。

## 请求统计口径

仪表板的请求数、成功率及管理 API 中的 `requests` 统计，仅覆盖**本次服务进程启动以来**
的四个模型 POST 入口。每个入站 HTTP 请求计一次，内部重试单列；成功率按成功数除以
已完成请求数计算，无已完成请求时为空。鉴权/参数失败、流内错误、断流和票务超时
均计为失败，客户端取消另有 `canceled` 子计数，在途请求不参与成功率计算。

账号页与用量排行的历史调用/Token 计数仍保留在数据库中，与这些进程级指标分开。
接口 `GET /admin/api/usage` 增加 `requests` 字段；`GET /admin/api/monitor` 的
`requests.errors` 与 `requests.failed` 同值，供既有调用方兼容。

## License

AGPL-3.0（見 [LICENSE](LICENSE)），僅供學習研究與個人自部署使用；使用本項目產生的
一切行為與後果由使用者自行承擔，請自行遵守上游服務條款。本項目與 Z.AI 無任何關聯。
