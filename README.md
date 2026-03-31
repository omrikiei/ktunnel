# ktunnel

Expose local ports to a Kubernetes cluster.

## Install

Pre-built binaries: [GitHub releases](https://github.com/AppMana/ktunnel/releases)

From source:

```bash
git clone https://github.com/AppMana/ktunnel && cd ktunnel
go build -o ktunnel .
```

## Usage

```bash
# Expose local port 3000 as a ClusterIP service in the cluster
ktunnel expose my-service 3000:3000 \
  --namespace=my-namespace \
  --server-image=ghcr.io/appmana/ktunnel:v3.0.0 \
  --node-selector-tags kubernetes.io/os=linux

# Reuse existing deployment (skip recreate)
ktunnel expose my-service 3000:3000 --reuse --namespace=my-namespace \
  --server-image=ghcr.io/appmana/ktunnel:v3.0.0

# Multiple ports
ktunnel expose my-service 3000:3000 8080:8080 --namespace=default

# Inject into existing deployment
ktunnel inject deployment mydeployment 3306
```

## How it works

```
Cluster Service (ClusterIP:3000)
  → ktunnel server pod (port-forward)
  → ktunnel client (your machine)
  → localhost:3000
```

`ktunnel expose` creates a Deployment + Service, port-forwards to the pod, tunnels traffic to your local machine, and cleans up on exit (Ctrl+C).
