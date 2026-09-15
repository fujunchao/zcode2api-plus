# syntax=docker/dockerfile:1

# ── 構建階段 ────────────────────────────────────────────────────────────────
# 前端 dist 已入庫並由 go:embed 嵌進二進制，故構建階段不需要 Node
#（與 .github/workflows/release.yml 的做法一致）。
# 但 dist 缺失會得到一個「能啟動、後台卻 404」的二進制——下面顯式攔一道。
FROM --platform=$BUILDPLATFORM golang:1.25-bookworm AS build

WORKDIR /src

# 先只拷依賴清單：改動源碼時這一層仍能命中緩存，省掉重新拉模組
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN test -f frontend/dist/index.html || { \
      echo "構建失敗：frontend/dist/index.html 不存在。" >&2; \
      echo "前端產物必須入庫（go:embed all:frontend/dist）。請執行：" >&2; \
      echo "  cd frontend && npm ci && npm run build && git add dist" >&2; \
      exit 1; \
    }

# 交叉編譯：構建階段跑在 BUILDPLATFORM 上（原生速度），目標平台靠 GOOS/GOARCH 指定，
# 不走 QEMU 模擬。CGO_ENABLED=0 才能得到靜態二進制（SQLite 用純 Go 的 modernc）。
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/zcode2api ./cmd/zcode2api

# ── 運行階段 ────────────────────────────────────────────────────────────────
# 必須是 glibc 基礎鏡像：cloakbrowser 的補丁 Chromium 是 glibc 構建，
# Alpine（musl）跑不起來——那會表現為「JWT 賬號一直 503」，極難排查。
FROM debian:bookworm-slim

# Chromium 運行期共享庫。缺任何一個都會讓瀏覽器起不來，而驗證碼求解又強依賴
# 真實瀏覽器，所以這裡寧可裝多不裝少（合計約 100MB）。
# curl 供 HEALTHCHECK 與容器內排查用；fonts-liberation 補足無頭渲染字體。
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl fonts-liberation \
      libasound2 libatk-bridge2.0-0 libatk1.0-0 libatspi2.0-0 \
      libcairo2 libcups2 libdbus-1-3 libdrm2 libexpat1 libgbm1 \
      libglib2.0-0 libnspr4 libnss3 libpango-1.0-0 libudev1 \
      libx11-6 libxcb1 libxcomposite1 libxdamage1 libxext6 libxfixes3 \
      libxkbcommon0 libxrandr2 libxshmfence1 \
 && rm -rf /var/lib/apt/lists/*

# 非 root 運行；uid/gid 固定為 10001，便於宿主 bind mount 時對齊屬主。
RUN groupadd -g 10001 zcode \
 && useradd -u 10001 -g 10001 -m -s /usr/sbin/nologin zcode

COPY --from=build /out/zcode2api /usr/local/bin/zcode2api

# 數據與瀏覽器緩存都放 /data 下：accounts.db、device_mid.txt、
# cloakbrowser/chromium-<版本>/。三者都必須落在卷上——
# device_mid 每次重建都變會被上游當成新設備（風控），
# 瀏覽器二進制丟了要重下約 200MB。
ENV ZCODE_DATA_DIR=/data \
    ZCODE_HOST=0.0.0.0 \
    ZCODE_PORT=3000 \
    CLOAKBROWSER_CACHE_DIR=/data/cloakbrowser

RUN mkdir -p /data && chown -R zcode:zcode /data
USER zcode

EXPOSE 3000

# /meta 不需要鑑權，適合探活（見 internal/web/spa.go）
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD curl -fsS "http://127.0.0.1:${ZCODE_PORT}/meta" || exit 1

ENTRYPOINT ["/usr/local/bin/zcode2api"]
CMD ["serve"]
