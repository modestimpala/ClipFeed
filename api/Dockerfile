FROM golang:1.24-alpine AS builder
WORKDIR /src
COPY go.mod ./
COPY *.sql .
COPY *.go .
COPY migrations/ ./migrations/
RUN go mod tidy && CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /app/server .

FROM alpine:3.19
RUN apk add --no-cache ca-certificates tzdata sqlite \
    && addgroup -g 1000 -S appgroup && adduser -u 1000 -S appuser -G appgroup
WORKDIR /app
COPY --from=builder /app/server .
USER appuser
EXPOSE 8080
ENTRYPOINT ["./server"]
