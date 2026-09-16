# syntax=docker/dockerfile:1

# ── 构建阶段 ────────────────────────────────────────────────────────────────
# 前端 dist 已入库并由 go:embed 嵌进二进制，故构建阶段不需要 Node
#（与 .github/workflows/release.yml 的做法一致）。
# 但 dist 缺失会得到一个「能启动、后台却 404」的二进制——下面显式拦一道。
FROM --platform=$BUILDPLATFORM golang:1.25-bookworm AS build

WORKDIR /src

# 先只拷依赖清单：改动源码时这一层仍能命中缓存，省掉重新拉模块
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN test -f frontend/dist/index.html || { \
      echo "构建失败：frontend/dist/index.html 不存在。" >&2; \
      echo "前端产物必须入库（go:embed all:frontend/dist）。请执行：" >&2; \
      echo "  cd frontend && npm ci && npm run build && git add dist" >&2; \
      exit 1; \
    }

# 交叉编译：构建阶段跑在 BUILDPLATFORM 上（原生速度），目标平台靠 GOOS/GOARCH 指定，
# 不走 QEMU 模拟。CGO_ENABLED=0 才能得到静态二进制（SQLite 用纯 Go 的 modernc）。
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/zcode2api ./cmd/zcode2api

# ── 运行阶段 ────────────────────────────────────────────────────────────────
# 必须是 glibc 基础镜像：cloakbrowser 的补丁 Chromium 是 glibc 构建，
# Alpine（musl）跑不起来——那会表现为「JWT 账号一直 503」，极难排查。
FROM debian:bookworm-slim

# Chromium 运行期共享库。清单对齐上游对官方 linux-x64 补丁二进制 ldd 实测的结果
# （bookworm 用非 t64 包名）。缺任何一个都会让浏览器起不来，而验证码求解强依赖真实
# 浏览器，所以宁可多装（合计约 120MB）；其中 libxi6、libvulkan1 不在其他包的依赖
# 闭包内，必须显式列出。curl 供 HEALTHCHECK 与容器内排查用；fonts-liberation 补足
# 无头渲染字体。
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl fonts-liberation \
      libasound2 libatk-bridge2.0-0 libatk1.0-0 libatspi2.0-0 \
      libavahi-client3 libavahi-common3 libcairo2 libcups2 libdatrie1 \
      libdbus-1-3 libdrm2 libexpat1 libfontconfig1 libfreetype6 libfribidi0 \
      libgbm1 libglib2.0-0 libgraphite2-3 libharfbuzz0b libnspr4 libnss3 \
      libpango-1.0-0 libpixman-1-0 libpng16-16 libthai0 libudev1 libvulkan1 \
      libx11-6 libxau6 libxcb-render0 libxcb-shm0 libxcb1 libxcomposite1 \
      libxdamage1 libxdmcp6 libxext6 libxfixes3 libxi6 libxkbcommon0 \
      libxrandr2 libxrender1 libxshmfence1 \
 && rm -rf /var/lib/apt/lists/*

# 非 root 运行；uid/gid 固定为 10001，便于宿主 bind mount 时对齐属主。
RUN groupadd -g 10001 zcode \
 && useradd -u 10001 -g 10001 -m -s /usr/sbin/nologin zcode

COPY --from=build /out/zcode2api /usr/local/bin/zcode2api

# 数据与浏览器缓存都放 /data 下：accounts.db、device_mid.txt、
# cloakbrowser/chromium-<版本>/。三者都必须落在卷上——
# device_mid 每次重建都变会被上游当成新设备（风控），
# 浏览器二进制丢了要重下约 200MB。
ENV ZCODE_DATA_DIR=/data \
    ZCODE_HOST=0.0.0.0 \
    ZCODE_PORT=3000 \
    CLOAKBROWSER_CACHE_DIR=/data/cloakbrowser

RUN mkdir -p /data && chown -R zcode:zcode /data
USER zcode

EXPOSE 3000

# /meta 不需要鉴权，适合探活（见 internal/web/spa.go）
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD curl -fsS "http://127.0.0.1:${ZCODE_PORT}/meta" || exit 1

ENTRYPOINT ["/usr/local/bin/zcode2api"]
CMD ["serve"]
