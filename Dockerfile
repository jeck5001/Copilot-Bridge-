FROM golang:1.27-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/m365-gateway ./cmd/server

FROM alpine:3.23

RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S m365 \
    && adduser -S -G m365 -h /app m365 \
    && mkdir -p /app/web /data \
    && chown -R m365:m365 /app /data

WORKDIR /app
COPY --from=build /out/m365-gateway /usr/local/bin/m365-gateway
COPY --chown=m365:m365 web ./web

ENV M365_LISTEN=0.0.0.0:4141 \
    M365_TOKEN_CACHE=/data/accounts.json \
    M365_SESSION_CACHE=/data/sessions.json \
    M365_API_KEYS=/data/api-keys.json \
    M365_SETTINGS_FILE=/data/settings.json \
    M365_DEBUG_LOG=/data/debug-logs.jsonl \
    M365_ADMIN_PASSWORD_HASH_FILE=/data/admin-password.hash \
    M365_ADMIN_PASSWORD=admin888 \
    M365_COOKIE_SECURE=false \
    M365_LOG_LEVEL=warn

USER m365:m365
EXPOSE 4141
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:4141/api/health || exit 1

ENTRYPOINT ["/usr/local/bin/m365-gateway"]
