# Security model

ktunnel builds a path from inside a Kubernetes cluster to a service running on
your machine. **Nothing on that path is authenticated, and nothing on it is
encrypted.** Anything in the cluster that can reach a tunnelled port reaches
whatever is listening behind it on your laptop, with your laptop's access to it.

That is a deliberate trade for a development tool — it is why ktunnel installs
no operator, defines no CRDs and needs no cluster-wide permissions — but it is
worth knowing before you point it at a cluster. Use it against development
clusters you control. It is not built for shared, multi-tenant or production
clusters.

The fixes are on the [roadmap](../ROADMAP.md) for v2.4 and tracked in
[#166](https://github.com/omrikiei/ktunnel/issues/166) and
[#80](https://github.com/omrikiei/ktunnel/issues/80). This page describes what
is true today.

## What is reachable, and by whom

### `ktunnel expose`

`expose` creates a Deployment and a Service. The Service is the way in.

- With the default `--service-type ClusterIP`, **every pod in the cluster** can
  resolve `myapp.<namespace>.svc` and connect to the exposed ports. Nothing
  narrows that except a NetworkPolicy, and only in clusters that enforce one.
- `--service-type NodePort` opens the port on every node's IP, so the reach is
  whatever can route to a node.
- `--service-type LoadBalancer` usually means a publicly routable address.
  Assume the internet unless you know otherwise.

### `ktunnel inject deployment`

The sidecar runs the tunnel server inside your application's pods, and its
listeners bind every interface in the pod (`:PORT`), not loopback only.

- Containers in an injected pod reach your machine at `localhost:PORT`, which is
  the point of the command.
- The same port is also open on the **pod IP**, so anything in the cluster that
  can route to that pod reaches your machine too, whether or not a Service
  points at it.
- Every replica is injected, so every replica is a way in.

### The gRPC port (`--port`, default 28688)

The tunnel server listens on `0.0.0.0:28688` in the pod. `expose` does not put
that port in the Service it creates, but it is open on the pod IP either way.

It is the more sensitive of the two. The tunnel server forwards incoming
connections to whichever client is attached to it, so anything in the cluster
that can reach this port can attach as a client — and be handed the traffic that
was meant for your machine.

## What is encrypted

- `--tls` is **rejected** by `expose` and `inject deployment`. Nothing mounts a
  certificate into the tunnel server pod, so the flag cannot do anything there;
  the commands refuse it up front rather than run in plaintext while claiming
  otherwise ([#166](https://github.com/omrikiei/ktunnel/issues/166)).
- Standalone `ktunnel server` and `ktunnel client` do support `--tls`, with
  `--cert`, `--key` and `--ca-file`.
- The leg between your machine and the API server is the API server's own TLS,
  since it is an ordinary port-forward. The tunnel stream inside it is plaintext
  gRPC, and the cluster-side hop from the calling pod to the tunnel server is
  plaintext.

## What authenticates what

- **ktunnel to the cluster:** your kubeconfig, and nothing else. `KUBECONFIG` is
  honoured, `--context` selects a context. There is no `--token` flag
  ([#80](https://github.com/omrikiei/ktunnel/issues/80)); to use a token, put it
  in a kubeconfig context with `kubectl config set-credentials` and select that
  context.
- **The cluster to your machine:** nothing. There is no shared secret, no client
  certificate and no allow-list. Reachability is authorization.

## Kubernetes permissions used

All namespaced — ktunnel asks for nothing cluster-scoped:

| Resource | Verbs | Used by |
|---|---|---|
| `deployments` | get, create, patch, delete | `expose` |
| `deployments` | get, update | `inject`, `--eject` |
| `services` | get, create, patch, delete | `expose` |
| `pods` | list | both, to resolve pods to forward to |
| `pods/portforward` | create | both |

A ServiceAccount limited to these is on the roadmap for v2.4; today ktunnel uses
whatever your kubeconfig user has.

## Reducing the exposure

- Use a namespace nobody else deploys into, and keep the default `ClusterIP`.
  Reach for `NodePort` or `LoadBalancer` only when something outside the cluster
  genuinely has to connect.
- Do not tunnel to something on your machine you would not hand to the cluster
  outright: a database holding your own data, an SSH agent, a Docker socket, a
  package registry you are authenticated to.
- A NetworkPolicy restricting ingress to the tunnel server pod is the only
  in-cluster control that helps today. It is worth writing if the cluster
  enforces them.
- Take the tunnel down when you stop using it. Ctrl+C removes what `expose`
  created and ejects the `inject` sidecar; a `SIGKILL` leaves both in place, and
  the way in stays with them. `kubectl get deploy,svc -n <namespace>` is the
  check.
