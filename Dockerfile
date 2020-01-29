FROM golang:1.13.1-alpine as builder
ENV GO111MODULE=on
COPY . /build
WORKDIR /build
RUN apk update && \
    apk add upx && \
    CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o="ktunnel" && \
    upx ktunnel

FROM scratch
WORKDIR /ktunnel
COPY --from=builder /build/ktunnel ./
EXPOSE 28688
CMD ["./ktunnel", "server"]
