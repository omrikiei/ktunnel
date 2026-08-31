# Security model

ktunnel builds a path from inside a Kubernetes cluster to a service running on
your machine. Since **v2.4** that path is authenticated, and on `expose` it is
encrypted, with no flags to remember: every run mints a throwaway CA, a server
certificate and a bearer token, ships them to the tunnel server as a Secret,
and keeps its own half in memory.

What that buys: something in the cluster that finds the gRPC port can no
longer attach as a client and be handed the traffic meant for your machine.
It is turned away before the server binds anything.

What it does not buy: **anyone who can reach a tunnelled port still reaches
whatever is listening behind it on your laptop.** The token guards the tunnel,
not the ports the tunnel opens. Use ktunnel against development clusters you
control; it is not built for shared, multi-tenant or production clusters.

Two cases run with less than the full protection, and both say so before they
start rather than after:

- **`inject`** authenticates but does not encrypt. See below for why.
- **A namespace that forbids `secrets: create`** authenticates but does not
  encrypt, because the fallback carries a token in the pod spec and a private
  key there would be the whole channel rather than one run's revocable secret.

`--insecure` turns both off and restores pre-v2.4 behaviour.

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

- **`expose`** encrypts by default. The generated certificate names
  `localhost`, `127.0.0.1` and `<name>.<namespace>.svc`, so the client
  verifies it over the port-forward without `--server-host-override`. It is
  valid for 24 hours and never touches your filesystem.
- **`inject`** does **not** encrypt. It gets a token and no certificate,
  deliberately: injecting a volume and a volumeMount into a Deployment ktunnel
  does not own means an eject that can leave debris behind in someone else's
  object, and the sidecar's listeners are pod-local to begin with.
- `--tls` on `expose` and `inject` is **accepted and does nothing**. It is
  deprecated: it asks for what already happens. Through v2.3 it was rejected
  outright ([#166](https://github.com/omrikiei/ktunnel/issues/166)).
- `--cert`/`--key`/`--ca-file` still work, and mean *use these instead of
  generating any*. They reach the server as separate arguments now; before
  v2.4 they were passed as one unparsed string, which is the other reason
  in-cluster TLS could not work.
- Standalone `ktunnel server` and `ktunnel client` support `--tls` as before.
- The leg between your machine and the API server is the API server's own TLS,
  since it is an ordinary port-forward.

### An older server image

If `--image` pins a tunnel server older than v2.4, it serves plaintext and
ignores the token. ktunnel does not abort — a pinned image is a legitimate
thing to have — but it logs the cause and the flag, once, and continues
unencrypted and unauthenticated. If you see that line, the tunnel has no
protection at all.

## What authenticates what

- **ktunnel to the cluster:** your kubeconfig, and nothing else. `KUBECONFIG` is
  honoured, `--context` selects a context. There is no `--token` flag
  ([#80](https://github.com/omrikiei/ktunnel/issues/80)); to use a token, put it
  in a kubeconfig context with `kubectl config set-credentials` and select that
  context.
- **The cluster to your machine:** a bearer token, generated per run and
  checked before the tunnel server opens any port. A caller without it is
  refused with `Unauthenticated` and cannot cause a port to bind.
  - With a Secret, the token is a `secretKeyRef` and is not visible in the pod
    spec.
  - In the fallback, it is a literal env value, readable by anyone with `get
    pods` in that namespace. Still far narrower than "anything that can reach
    the Service", which is what v2.3 offered.
  - It authorizes attaching to the tunnel. It does not authorize reaching the
    ports the tunnel opens — that is still reachability.

## Kubernetes permissions used

All namespaced — ktunnel asks for nothing cluster-scoped:

| Resource | Verbs | Used by |
|---|---|---|
| `deployments` | get, create, patch, delete | `expose` |
| `deployments` | get, update | `inject`, `--eject` |
| `services` | get, create, patch, delete | `expose` |
| `pods` | list | both, to resolve pods to forward to |
| `pods/portforward` | create | both |
| `secrets` | get, create, update, delete | `expose`, for the per-run credentials |

A ready-to-apply ServiceAccount, Role and RoleBinding with exactly these verbs
is in [rbac.yaml](rbac.yaml). Without it ktunnel uses whatever your kubeconfig
user has.

Dropping the `secrets` rule is supported: `expose` falls back to an
authenticated but unencrypted tunnel and says so before it starts.

## Reducing the exposure

- Use a namespace nobody else deploys into, and keep the default `ClusterIP`.
  Reach for `NodePort` or `LoadBalancer` only when something outside the cluster
  genuinely has to connect.
- Do not tunnel to something on your machine you would not hand to the cluster
  outright: a database holding your own data, an SSH agent, a Docker socket, a
  package registry you are authenticated to.
- A NetworkPolicy restricting ingress to the tunnel server pod still helps,
  and is the only control over the *tunnelled* ports, which the token does not
  guard.
- Take the tunnel down when you stop using it. Ctrl+C removes what `expose`
  created and ejects the `inject` sidecar; a `SIGKILL` leaves both in place, and
  the way in stays with them. `kubectl get deploy,svc -n <namespace>` is the
  check.
