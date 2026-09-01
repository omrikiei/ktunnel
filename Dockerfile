FROM golang:1.27-alpine AS builder
ENV GO111MODULE=on
RUN apk update && \
    apk add upx

WORKDIR /build
COPY go.mod /build
COPY go.sum /build
RUN go mod download

COPY . /build
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o="ktunnel" && \
    upx ktunnel

FROM scratch
WORKDIR /ktunnel
COPY --from=builder /build/ktunnel ./

# The non-root guarantee lives here rather than in the pod spec.
#
# It used to be RunAsUser: 1000 on the container ktunnel creates, which
# OpenShift rejects: its SCCs assign a UID from a per-namespace range and
# refuse a pod that demands its own, so `ktunnel expose` did not work on OCP
# at all (#87). Declared on the image instead, a vanilla cluster runs as 1000
# exactly as before and OpenShift overrides it, which is what it wants to do.
#
# Numeric, because `FROM scratch` has no /etc/passwd for a name to resolve
# against.
USER 1000

EXPOSE 28688
ENTRYPOINT ["/ktunnel/ktunnel"]
CMD ["server"]
