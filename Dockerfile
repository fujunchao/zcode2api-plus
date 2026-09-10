# zcode2api-go — 纯 Go 单二进制 + Node(前端构建) + Chromium(rod 验证码求解)
# 阶段一：构建管理后台 SPA；阶段二：静态编译 Go 二进制；
# 最终镜像 debian:bookworm-slim + Debian chromium（ZCODE_CAPTCHA_BROWSER_BIN 指向它）。

# ── 阶段一：构建管理后台 SPA ─────────────────────────────────────────────────
FROM node:20-alpine AS frontend
WORKDIR /fe
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

# ── 阶段二：编译 Go 网关（CGO 关闭，modernc/sqlite 纯 Go）───────────────────
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/zcode2api ./cmd/zcode2api

# ── 最终镜像 ─────────────────────────────────────────────────────────────────
FROM debian:bookworm-slim

ENV ZCODE_HOST=0.0.0.0 \
    ZCODE_PORT=3000 \
    ZCODE_DATA_DIR=/data \
    # 验证码求解用 Debian 官方 chromium；禁用浏览器池时无需安装即可运行
    ZCODE_CAPTCHA_BROWSER=true \
    ZCODE_CAPTCHA_BROWSER_BIN=/usr/bin/chromium

WORKDIR /app

# Chromium 运行库（chromium 包自带依赖链；fonts-liberation 保证中文/拉丁渲染）
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        chromium \
        fonts-liberation \
        tzdata \
    && apt-get autoremove -y \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/zcode2api /app/zcode2api
COPY --from=frontend /fe/dist /app/frontend/dist

VOLUME ["/data"]
EXPOSE 3000

ENTRYPOINT ["/app/zcode2api"]
CMD ["serve"]
