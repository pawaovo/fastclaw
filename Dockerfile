FROM golang:1.24-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o fastclaw .

FROM alpine:latest
RUN apk --no-cache add ca-certificates tzdata docker-cli
WORKDIR /app
COPY --from=builder /app/fastclaw .
EXPOSE 18080
CMD ["./fastclaw", "server"]
