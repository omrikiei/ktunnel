FROM golang:1.13.1-alpine as builder
ENV GO111MODULE=on
COPY . /build
WORKDIR /build
RUN CGO_ENABLED=0 GOOS=linux go build

FROM alpine:latest
WORKDIR /ktunnel
COPY --from=builder /build/ktunnel ./
EXPOSE 28688
CMD ["./ktunnel", "server"]