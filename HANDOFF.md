# zcode2api-go 交接文檔

> 給新會話（Claude 或人類協作者）的快速上手指南。計劃與進度台账在 `PLAN.md`（唯一權威），
> 本文檔只做「狀態快照 + 工作流 + 紅線 + 踩坑記錄」，避免重複維護。
> 最後更新：2026-09-11（M5 代码完成：rod 池 + 失败注入单测全绿；新增 M8 套餐自动领取规划）。

---

## 1. 當前狀態快照

| 項目 | 狀態 |
|------|------|
| 模塊 | `zcode2api`（Go 1.22+，標準庫優先，僅 sqlite/rod/godotenv 為外部依賴） |
| 遠端 | origin = github.com/gakiyukr/zcode2api-go（分支 master）；upstream = github.com/gakiyukr/zcode2api-plus（**只讀參照，絕不推送**） |
| 里程碑 | M0-M4 全部完成並勾選：M0 骨架+數據層、M1 網關核心、M2 Admin API+SPA、M3 額度+async、M4 OpenAI 兼容層；**M5 代碼完成**（rod 池 + 12 組失敗注入單測，真機驗收待做） |
| 提交鏈 | d01c740（M0-M2）→ ff97604（M3）→ b97acc2（M4）→ M5 提交（見 git log），全部 GPG 簽名 |
| 測試 | `go build ./... && go vet ./... && go test ./...` 全綠（9 個含測試的包）；`-race` 本機不可用（無 gcc），M6 驗收時補 |
| 行為契約 | 主倉庫 Python 版（`C:\Projects\zcode2api`，`app/`）為權威對照；唯 M4 為 Go 版增量，契約是 `PLAN.md` §5.7 |

## 2. 新會話上手步驟

1. 讀 `PLAN.md` 全文（約 300 行）——範圍、§5.x 端點契約、里程碑勾選狀態都在那裡。
2. 本文件 §4 紅線與 §6 踩坑記錄**必讀**。
3. 驗證環境：`cd C:\Projects\zcode2api-go && go build ./... && go test ./...`。
4. 從 `PLAN.md` 未勾選的第一項開工（當前是 M5）。
5. 用戶母語溝通用**繁體中文**（CLAUDE.md 全局強制）；提交訊息用英文。

## 3. Git 與語言規範（每次提交都適用）

- 提交訊息**英文**，`git commit -S`（強制 GPG 簽名），正文描述做了什麼與為什麼，附
  `Co-Authored-By: Claude Code <noreply@anthropic.com>`。
- 推送走本地代理：`HTTPS_PROXY=http://127.0.0.1:7890 git push origin master`。
- **只推 origin（zcode2api-go）**。upstream（zcode2api-plus）是 Python 主倉庫，僅讀取參照。
- 代碼註釋與文檔：本倉庫既有慣例為簡體中文（M0 起延續，已隨提交固化），新代碼註釋保持
  簡體與周圍一致；**獨立新文檔**（如本文件）按 CLAUDE.md 用繁體。代碼標識符一律英文。

## 4. 安全紅線（逐字遵守，無例外）

1. `data/` 目錄含**真實賬號憑證**（accounts.db、device_mid.txt 等）：絕不可提交、複製入
   測試夾具、寫入任何文檔或日誌。測試一律 `config.DBPath = filepath.Join(t.TempDir(), "accounts.db")` 隔離。
2. 後台密鑰（admin key）、網關密鑰（gateway_key）屬敏感信息，不落入倉庫文件。
3. 絕不把本倉庫任何提交推到 upstream（zcode2api-plus）。
4. 遠端已複核零憑證入庫；每次提交前 `git status` 確認 `data/` 未被暫存（.gitignore 已覆蓋，勿移除）。

## 5. 接下來的工作（按 PLAN.md 順序）

### M5 驗證碼池（高風險，單獨攻堅，下一項）
- 目標：rod 池復用 cloakbrowser 下載的 Chromium 二進制，實現 `internal/captcha` 的 Solver
  接口（接口與 manager 已在 M1 就緒：緩存 10min / 人工回填 45s / SetSolver 注入點 / SetConfigProvider）。
- 驗收：真實賬號連續 20 次 JWT 請求全部自動通過（無 F001）；失敗注入測試（超時 / 崩潰替換 / 遲到 token 不誤投）。
- 對照 Python 版 `app/captcha_pool.py`（或同名模塊，先在主倉庫定位）。
- 對策見 PLAN.md §8 風險表：參數逐項對齊 → stealth → 人工回填兜底。

### M6 OAuth + 代理出口 + CLI + 交付
- OAuth 登錄鏈（對照主倉庫 `app/routes/oauth.py`）、賬號級出站代理（http/socks，`internal/proxy` 包已建空殼）、
  CLI 子命令（serve/login/add-account/…）、Dockerfile 多階段（node 構建前端 → go 構建 → bookworm-slim + Chromium）。
- 驗收：`docker compose up -d --build` 一鍵起；`-race` 全測試通過；兩版本交替用同一 db 無異常。

### M7 `/v1/responses`（見 PLAN.md §5.8）
- 復用 M4 的 `internal/openai` 轉換基建；`previous_response_id` v1 明確 400。
- 驗收：Codex CLI 指向網關無狀態模式完整會話。

### 欠賬驗收（需真實賬號環境，與 M5/M6 驗收合併做）
- M0：真實 `data/accounts.db` 互通實測（Go 寫回後 Python 版可讀）。
- M1：真實賬號非流式 + 流式各打通一次。
- M4：openai 官方 Python 客戶端指向網關跑通三場景。

## 6. 踩坑記錄（新會話必讀，避免重蹈）

1. **上游響應形態**：zcode.z.ai 非流式響應是**標準 Anthropic Messages 頂層形態**——
   id/content/stop_reason/usage 都在頂層，**沒有**嵌套 `message` 對象。權威依據是
   `internal/gateway/usage.go` 頭部註釋。M4 曾誤設嵌套形態，e2e 階段才修正。
2. **測試基建三件套**：① config 變量隔離（DBPath/DataDir/UpstreamZai/Fallback）+ `t.TempDir` + t.Cleanup 還原；
   ② captcha.Manager 離線化：`SetSolver`（假求解器）+ `SetConfigProvider`（固定配置）——否則
   FetchConfig 打真實 zcode.z.ai（經代理約 2s/次，離線回退 cn region 會讓斷言漂移）；
   ③ e2e 賬號用 api_key 模式（credential 不含兩個點）即不觸發驗證碼路徑，離線穩定；
   JWT 模式會走 captcha + zcode_system 注入，僅在專測這些語義時使用（gateway 包 e2e 已覆蓋）。
3. **inflight 廣播語義**：quota 的併發去重用 `done chan struct{}` close 廣播 + result 共享讀；
   容量 1 的 channel 只能配對一個接收者，會死鎖（M3 踩過）。
4. **字面反斜杠序列寫文件**：Edit 工具的 JSON 參數層會把 `\u4e2d` 解碼為字符；perl 替換側把
   `\u` 當大小寫轉義。要寫入字面 `\uXXXX` 用 `perl -i -pe 's/\x5cu…/'`（`\x5c` = 字面反斜杠）。
5. **Go map JSON 鍵序**：`encoding/json` 按鍵字母序序列化，字符串斷言不要假設鍵序
   （如 `"delta":{"role":…}`），分鍵 Contains 或用 reflect.DeepEqual。
6. **gateway 工具已導出**（供 openai/asyncpool 複用，勿再寫私有版）：`WriteJSON` / `WriteAuthError` /
   `IncomingHeaders` / `AnyToString` / `ModelAllowed` / `NormalizeBody` / `AvailableModels`。
7. **引擎錯誤體形態**：`runResult.Body` 已是 `{"error":{message,type,code}}`，OpenAI 客戶端
   兼容，handler 直接透傳即可。

## 7. 包結構速查

| 包 | 職責 | 測試 |
|----|------|------|
| `internal/config` | 全部 `ZCODE_*` 環境變量（包級變量，測試直接改） | — |
| `internal/model` | Account 狀態機 + 模型可用性 + JSON 契約 | ✓ |
| `internal/store` | SQLite（modernc 純 Go）+ 密鑰引導 + 輪詢游標 + 代理 + 導入導出 | ✓ |
| `internal/upstream` | build_request：頭 + zcode_system 注入 + 客戶端頭過濾 | ✓ |
| `internal/captcha` | 驗證碼管理器（M5 在此加 rod 池） | ✓ |
| `internal/gateway` | /v1/messages + /v1/models、選號循環 engine.go、整形 body.go、分類 classify.go、統計 usage.go | ✓（13 組 e2e） |
| `internal/openai` | M4 OpenAI 兼容層：convert / respond / stream / handler | ✓（24 組） |
| `internal/asyncpool` | /async/v1/messages ticket 全語義 | ✓ |
| `internal/quota` | 額度查詢 + 緩存去重 + 後台 monitor | ✓ |
| `internal/auth` | 網關 fail-closed + 後台限速（10 次/5min） | ✓ |
| `internal/adminapi` | /admin/api/* 全端點 | ✓ |
| `internal/web` | 終端日誌；`webui.go`（倉庫根）go:embed SPA | — |
| `frontend/` | React SPA（**不重寫**，dist 直接 embed） | — |
| `cmd/zcode2api` | main.go 入口接線 | — |
| `internal/proxy` | 賬號級出站代理（M6 實現，現為空殼） | — |

## 8. 驗證命令

```bash
cd C:\Projects\zcode2api-go
go build ./... && go vet ./... && go test ./...   # 全量驗證（當前全綠）
go test ./internal/openai/ -v                     # M4 轉換層詳情
HTTPS_PROXY=http://127.0.0.1:7890 git push origin master   # 推送
```
