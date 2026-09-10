# zcode2api-go

ZCode2api-plus 的 Go 重寫版：把 Z.AI ZCode Coding Plan 網關（Anthropic 協議 `/v1/messages`、
OpenAI 兼容 `/v1/chat/completions`、`/async/v1/messages`、`/v1/responses`）以單二進制交付，
內建 Admin SPA、驗證碼瀏覽器池（rod + Chromium）、額度查詢、OAuth 登錄與賬號級出站代理。

> 行為契約以 Python 主倉庫（zcode2api-plus）為權威對照；逐項對齊的里程碑與進度見 `PLAN.md`，
> 交接注意事項見 `HANDOFF.md`。

## 快速開始

```bash
# 源碼構建
go build -o zcode2api ./cmd/zcode2api
./zcode2api serve            # http://127.0.0.1:3000

# Docker 一鍵起（含 Chromium 驗證碼求解）
docker compose up -d --build
```

首次啟動橫幅會輸出後台密碼與網關 API Key（也可用 CLI 設定）。

## CLI

```
zcode2api serve [--port 3000]        啟動網關 + 後台 UI
zcode2api login zai [--no-browser]   OAuth 登錄 Z.AI 並入池
zcode2api add-account zai <name> <jwt|key>
zcode2api accounts [zai]             查看賬號列表
zcode2api remove-account <provider> <id|name>
zcode2api quota                      查看各賬號實時額度
zcode2api status                     配置概覽
zcode2api set-admin-key <key>        設置後台密碼
zcode2api export [file] / import <file>
```

## 配置（環境變量）

核心項（全部見 `internal/config/config.go`）：

| 變量 | 默認 | 說明 |
|------|------|------|
| `ZCODE_PORT` / `ZCODE_HOST` | 3000 / 0.0.0.0 | 監聽地址 |
| `ZCODE_DATA_DIR` | `./data` | 賬號庫、密鑰、設備指紋 |
| `ZCODE_CAPTCHA_BROWSER` | false | 啟用 rod 瀏覽器池自動求解 |
| `ZCODE_CAPTCHA_BROWSER_BIN` | 自動發現 | Chromium 二進制路徑 |
| `ZCODE_ASYNC_ENABLED` | — | 掛載 /async/v1/messages 空閒池 |

## 賬號級出站代理

賬號（或代理線路指派）配置 `proxy_url` 後，該賬號的網關請求與額度查詢均走對應代理；
支持 `http://`、`https://`（CONNECT）與 `socks4://`、`socks5://`、`socks5h://`（socks5h
由代理解析域名）。代理無效時回退直連並記錄 `last_error`。

## 包結構

見 `HANDOFF.md` §7 速查表。
