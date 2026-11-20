# Stage 1: Build React Web UI
FROM node:22-alpine AS web-builder
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# Stage 2: Build Go Hub binary
FROM golang:1.24-alpine AS go-builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY pkg/ ./pkg/
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/ourchive-hub ./cmd/hub

# Stage 3: Minimal production runtime
FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S ourchive && adduser -S -G ourchive ourchive \
    && mkdir -p /data /app/web/dist \
    && chown -R ourchive:ourchive /data /app

WORKDIR /app
COPY --from=go-builder /out/ourchive-hub /app/ourchive-hub
COPY --from=web-builder /src/web/dist /app/web/dist

USER ourchive
EXPOSE 8080
VOLUME ["/data"]

ENV OURCHIVE_ADDR=:8080 \
    OURCHIVE_DB_PATH=/data/ourchive.db \
    OURCHIVE_THUMB_DIR=/data/thumbs \
    OURCHIVE_WEB_DIR=/app/web/dist

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/api/health >/dev/null || exit 1

ENTRYPOINT ["/app/ourchive-hub"]
