# zcode2api

Z.AI ZCode Coding Plan → OpenAI/Anthropic 兼容網關（**Go 版，現為主線**）。

把 Z.AI Coding Plan 賬號池包裝成標準 API：多賬號輪詢（優惠額度優先）、驗證碼全自動求解、
額度與套餐到期監控、OAuth 登錄、賬號級出站代理、活動套餐自動領取、賬號歸檔，
單二進制交付（前端已內嵌，無外部運行時）。

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

## 發佈與開發

- 推 `v*` tag → GitHub Actions 自動交叉編譯五平台產物並上傳 Releases。
- 全量驗證：`go build ./... && go vet ./... && go test ./...`；併發檢查 `go test -race ./...`。
- 行為契約與里程碑台账見 `PLAN.md`；交接注意事項見 `HANDOFF.md`。

## 賬號歸檔

不再需要調用的賬號可「歸檔」：歸檔即強制停用並從賬號池隱藏，僅在後台歸檔區
保留記錄（累計用量可查）；歸檔賬號不參與調度、套餐領取與額度刷新，恢復後
保持停用狀態，需手動啟用才會重新入池。

## License

AGPL-3.0（見 [LICENSE](LICENSE)），僅供學習研究與個人自部署使用；使用本項目產生的
一切行為與後果由使用者自行承擔，請自行遵守上游服務條款。本項目與 Z.AI 無任何關聯。
