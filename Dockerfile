
FROM golang:1.27-bookworm AS builder

ARG GOPROXY=https://goproxy.cn,direct

ENV CGO_ENABLED=0 \
    GOPROXY=${GOPROXY}

WORKDIR /src

COPY go.mod ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w" -o /out/gotdx-webviewer ./cmd/webviewer

FROM alpine:3.22

ENV GOTDX_WEB_ADDR=0.0.0.0:8883 \
    TZ=Asia/Shanghai

WORKDIR /app

RUN apk add --no-cache ca-certificates tzdata wget

COPY --from=builder /out/gotdx-webviewer /app/gotdx-webviewer

EXPOSE 8883

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8883/api/health >/dev/null || exit 1

CMD ["/app/gotdx-webviewer"]
