FROM golang:1.24-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /app/server .

FROM alpine:3.19
RUN apk add --no-cache ca-certificates tzdata sqlite \
    && addgroup -g 1000 -S appgroup && adduser -u 1000 -S appuser -G appgroup
WORKDIR /app
COPY --from=builder /app/server .
RUN mkdir -p /data && chown appuser:appgroup /data
USER appuser
EXPOSE 8080
ENTRYPOINT ["./server"]
