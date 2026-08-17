# Runsite 适配版：上游 Dockerfile + 3 行环境变量预设（见下方 ENV 注释）
# 上游原版: https://github.com/trefeon/freebuff-proxy/blob/main/Dockerfile
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/freebuff-proxy ./cmd/freebuff-proxy

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S -g 1000 app \
    && adduser -S -u 1000 -G app app \
    && mkdir -p /app/dump /app/logs \
    && chown -R app:app /app
WORKDIR /app
COPY --from=build /out/freebuff-proxy /usr/local/bin/freebuff-proxy
USER app
# ── Runsite 适配 ────────────────────────────────────────────────────────────
# 1. Runsite 平台默认把流量转发到容器 8080 端口（可在 Settings → Build & Deploy 改）
# 2. 上游默认 LISTEN_ADDR=127.0.0.1:3457 只监听回环，容器外无法访问 → 改 :8080
# 3. AUTO_DISCOVER_TOKEN 是 env-only 变量，云上无 CLI 登录文件 → 必须关闭
#    （SAFE_MODE/COST_MODE 内置默认已是 true/free，显式写出防上游默认值变化）
# 换端口：改 LISTEN_ADDR 并在平台 Settings → Build & Deploy 同步改端口
ENV LISTEN_ADDR=:8080 \
    AUTO_DISCOVER_TOKEN=false \
    SAFE_MODE=true \
    COST_MODE=free
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/freebuff-proxy"]
