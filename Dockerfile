FROM node:24-alpine AS web-builder
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.26-alpine AS go-builder
RUN apk add --no-cache build-base
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web-builder /src/web/dist ./internal/transport/http/static/dist
RUN CGO_ENABLED=1 GOOS=linux go build \
    -tags sqlite_fts5 \
    -trimpath \
    -ldflags="-s -w -linkmode external -extldflags '-static'" \
    -o /out/nexusmail ./cmd/server

FROM alpine:3.23
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S -g 10001 nexusmail \
    && adduser -S -D -H -u 10001 -G nexusmail nexusmail \
    && mkdir -p /data \
    && chown nexusmail:nexusmail /data
COPY --from=go-builder /out/nexusmail /usr/local/bin/nexusmail
USER nexusmail:nexusmail
VOLUME ["/data"]
EXPOSE 8080
ENV NEXUSMAIL_LISTEN_ADDR=:8080 \
    NEXUSMAIL_DATA_DIR=/data \
    NEXUSMAIL_DATABASE_PATH=/data/mail.db
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:8080/readyz || exit 1
ENTRYPOINT ["/usr/local/bin/nexusmail"]
