# Build Stage
FROM golang:1.21-alpine AS builder

WORKDIR /app
COPY go.mod ./
COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /app/antigravity-proxy ./cmd/antigravity-proxy

# Final Stage
FROM alpine:3.19

RUN apk --no-cache add ca-certificates tzdata

WORKDIR /app
COPY --from=builder /app/antigravity-proxy /app/antigravity-proxy
COPY config.example.json /app/config.example.json

EXPOSE 8080

ENTRYPOINT ["/app/antigravity-proxy"]
CMD ["-config", "/app/config.json"]
