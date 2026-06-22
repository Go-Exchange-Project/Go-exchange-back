# syntax=docker/dockerfile:1

FROM golang:1.25.7-alpine AS builder

WORKDIR /src

RUN apk add --no-cache ca-certificates git

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /out/go-exchange-back ./cmd

FROM alpine:3.22

RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S app \
    && adduser -S -G app app

WORKDIR /app

COPY --from=builder /out/go-exchange-back /app/go-exchange-back
COPY config/market_rules.json /app/config/market_rules.json
COPY migrations /app/migrations

ENV GIN_MODE=release \
    GOEXCHANGE_MARKET_RULES_PATH=/app/config/market_rules.json \
    GOEXCHANGE_MIGRATIONS_DIR=/app/migrations

USER app

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=15s --retries=3 \
    CMD wget -qO- http://localhost:8080/ping >/dev/null || exit 1

CMD ["/app/go-exchange-back"]
