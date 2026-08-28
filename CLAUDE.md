# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

### Build
```bash
CGO_ENABLED=0 go build -ldflags="-s -w"
# or simply:
make build
```

### Test
```bash
# Run all tests
go test -v -race -coverprofile=coverage.txt -covermode=atomic ./...

# Run a single test
go test -v -run TestName ./pkg/k8s/

# Run tests in a specific package
go test ./pkg/k8s/...
```

### Lint / Format
```bash
gofmt -l .        # list unformatted files
gofmt -w .        # fix formatting
gosec ./...       # security scan
```

### Regenerate protobuf
```bash
make proto        # requires buf CLI
```

### Generate docs
```bash
make docs         # builds binary then runs with GEN_DOC=true to emit markdown to ./docs
```

## Architecture

Ktunnel creates a **reverse tunnel** from a Kubernetes cluster to the local machine using a bidirectional gRPC stream. Cluster-side pods can connect to the gRPC server running in the cluster, and traffic is forwarded through the tunnel to the local machine.

### Core tunnel mechanism (`pkg/client`, `pkg/server`)

The tunnel uses a single bidirectional gRPC stream (`Tunnel.InitTunnel`) defined in `api/tunnel.proto`. Each logical TCP connection is tracked as a `Session` (in `pkg/common/common.go`) identified by a UUID.

- **Server** (`pkg/server/server.go`): Runs inside the cluster (or locally for standalone use). Listens on a TCP port, accepts connections, and multiplexes them over the gRPC stream to the client. On the first message of a stream it reads the target port; subsequent messages carry session data.
- **Client** (`pkg/client/client.go`): Runs on the local machine. Receives session data from the gRPC stream and connects to the local service. Uses functional options (`Option` / `Config`) pattern for configuration.

Both sides use the same session management: `common.Session` holds the net.Conn, a buffer, and an open flag. Sessions are stored in a process-global `sync.Map` (`openSessions`).

### Kubernetes orchestration (`pkg/k8s`)

- **`common.go`**: Core k8s helpers — builds `KubeService`, resolves kubeconfig (honors `KUBECONFIG` env var), creates deployment/service/container specs, port-forward via SPDY, and watches deployment rollout status.
- **`exposer.go`**: `ExposeAsService` creates a Deployment + Service in the cluster running the ktunnel server image, then port-forwards to it. Supports reuse (`-r`) to patch existing resources instead of failing. Uses `ResourceTracker` to auto-cleanup on exit.
- **`injector.go`**: `InjectSidecar` patches an existing Deployment by appending the ktunnel container as a sidecar. Only supports single-replica deployments. `RemoveSidecar` reverses this.
- **`cleanup.go`**: `ResourceTracker` tracks deployments/services created during a session and deletes them on `SIGINT`/`SIGTERM` with a 30-second timeout.
- **`client.go`**: Holds the `Clients` struct wrapping typed k8s client interfaces (used as package-level vars `deploymentsClient`, `podsClient`, `svcClient`).

### CLI (`cmd/`)

Built with Cobra. Key commands:
- `expose <name> <ports>` — creates a new Deployment+Service, port-forwards, and runs the gRPC client locally.
- `inject deployment <name> <port>` — injects a sidecar into an existing deployment and port-forwards.
- `server` — runs the gRPC server standalone (useful for testing without k8s).
- `client` — runs the gRPC client standalone.

Global flags: `--port` (gRPC port, default 28688), `--tls`, `--verbose`.

### Port format

Ports are parsed by `common.ParsePorts` and support three formats:
- `port` — same source and target on localhost
- `sourcePort:targetPort` — different source/target on localhost
- `sourcePort:targetHost:targetPort` — different host

### Default server image

The cluster-side container image is `docker.io/omrieival/ktunnel` (defined as `Image` constant in `pkg/k8s/common.go`). The `--image` flag overrides this.
