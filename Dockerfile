FROM golang:1.21-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o price-tracker .

FROM alpine:3.19
RUN apk add --no-cache tzdata ca-certificates
ENV TZ=Asia/Tokyo
COPY --from=builder /app/price-tracker /price-tracker
EXPOSE 8085
ENTRYPOINT ["/price-tracker"]
