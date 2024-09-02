FROM golang:1.19-alpine AS builder
ENV GO111MODULE=on
RUN apk update && \
    apk add upx

WORKDIR /build
COPY go.mod /build
COPY go.sum /build
RUN go mod download

COPY . /build
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o="ktunnel" && \
    mkdir -p /ROOT/ktunnel /ROOT/etc && \
    echo "nonroot:x:1000:100:nonroot:/:/sbin/nologin" >/ROOT/etc/passwd && \
    upx -o /ROOT/ktunnel/ktunnel ktunnel

FROM scratch
COPY --from=builder /ROOT/ /
WORKDIR /ktunnel
EXPOSE 28688
CMD ["./ktunnel", "server"]
USER nonroot
