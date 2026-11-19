# syntax=docker/dockerfile:1

# 多阶段构建：构建阶段装 Go 工具链，最终镜像只留静态二进制与配置。
FROM golang:1.23-alpine AS build
WORKDIR /src

# 先只拷依赖清单，代码改动不会让模块下载层失效。
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
# modernc.org/sqlite 是纯 Go 实现，CGO_ENABLED=0 也能编译出静态二进制。
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/unipush ./cmd/unipush \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/push ./cmd/push

FROM alpine:3.20
# ca-certificates 用于渠道 webhook 的 HTTPS 校验；wget 来自 busybox，供 HEALTHCHECK 使用。
RUN apk add --no-cache ca-certificates \
 && addgroup -S -g 10001 unipush \
 && adduser -S -u 10001 -G unipush -h /app unipush \
 && mkdir -p /data \
 && chown unipush:unipush /data

WORKDIR /app
COPY --from=build /out/unipush /app/unipush
COPY --from=build /out/push /app/push
COPY config.yaml /app/config.yaml
RUN chown -R unipush:unipush /app

USER unipush
# 容器里监听 0.0.0.0（仅通过 compose 的端口映射暴露到宿主机 127.0.0.1），数据放 /data 卷。
ENV UNIPUSH_CONFIG=/app/config.yaml \
    UNIPUSH_ADDR=0.0.0.0:8080 \
    UNIPUSH_DB_PATH=/data/unipush.db \
    UNIPUSH_LOG_FORMAT=json

EXPOSE 8080
VOLUME ["/data"]
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/app/unipush"]
