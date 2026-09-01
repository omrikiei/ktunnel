# securityContext: OpenShift, privileged ports, and credential readability

Design for the v2.5 items [#87](https://github.com/omrikiei/ktunnel/issues/87)
and [#164](https://github.com/omrikiei/ktunnel/issues/164), and for the
class of bug that produced v2.4.1. Written 2026-08-31, against v2.4.1.

## Why these are one piece of work

All three touch the same field. `newContainer` builds one `SecurityContext`
holding exactly `RunAsUser: 1000`, and every question here is about what
belongs in it — or, more often, what has to leave.

The governing constraint, confirmed on a cluster rather than reasoned about:
**OpenShift owns `runAsUser` and `fsGroup`.** Its SCCs assign both from a
per-namespace range and reject a pod that demands its own values. Hardcoding
either breaks OCP by construction. That single fact decides #87, explains why
`fsGroup` was never the fix for the v2.4.0 crash loop, and shapes #164.

## What was measured

Every claim below was checked on a local `kind` cluster. The measurements
matter more than the design, because two of them contradict the obvious
answer.

### Secret volume ownership

| Mount mode | fsGroup | UID 1000 | OCP-style UID |
|---|---|---|---|
| `0400` | none | **denied** | denied |
| `0444` | none | ok | **ok** |
| `0440` | set | ok | ok |

Secret volume files are owned `root:root` unless the pod sets an `fsGroup`.
v2.4.0 shipped `0400` and every default `expose` crash-looped:

```
FATA Failed to generate credentials open /ktunnel/creds/tls.crt: permission denied
```

Fixed in v2.4.1 with `0444`, which is stricter than the Kubernetes default of
`0644` and needs no `fsGroup`.

### Binding ports below 1024

With `net.ipv4.ip_unprivileged_port_start` forced to `1024`:

| Config | CapPrm / CapEff / CapAmb | bind :80 |
|---|---|---|
| non-root, no help | all zero | denied |
| non-root + `capabilities.add: NET_BIND_SERVICE` | **all zero** | **denied** |
| non-root, `ip_unprivileged_port_start=0` | all zero | **ok** |

**`NET_BIND_SERVICE` is inert for a non-root container.** The capability never
reaches the permitted set: a process running as non-root with no file
capabilities gets an empty effective set on exec, and the runtime sets no
ambient capabilities. The obvious fix for #164 does nothing.

A second measurement matters as much: **kind/Docker sets
`ip_unprivileged_port_start` to `0` by default, and the kernel default is
`1024`.** Whether ktunnel can bind port 80 today depends on which cluster you
are on. That is why #164 reproduces for its reporter and not for everyone.

## The design

### Container securityContext — smaller, not larger

```go
SecurityContext: &apiv1.SecurityContext{
    // RunAsUser: gone (#87). The image carries USER 1000; OpenShift
    // overrides it from its range, which is what it wants to do.
    AllowPrivilegeEscalation: ptr(false),
    Capabilities: &apiv1.Capabilities{
        Drop: []apiv1.Capability{"ALL"},
    },
}
```

`Drop: ALL` and `AllowPrivilegeEscalation: false` are both required by
OpenShift's `restricted-v2` SCC and cost nothing on a vanilla cluster.

No `NET_BIND_SERVICE`: measured inert, see above.

### Dockerfile

One line, `USER 1000`. It takes over the non-root guarantee that `RunAsUser`
provided, and is what `05b502b` was actually trying to achieve. `FROM
scratch` has no `/etc/passwd`, but numeric UIDs need none.

### Privileged ports (#164)

A **pod-level** sysctl, set only when a requested source port is below 1024:

```go
Sysctls: []apiv1.Sysctl{{
    Name:  "net.ipv4.ip_unprivileged_port_start",
    Value: "0",
}}
```

Kubernetes classifies this sysctl as safe (1.22+), so it needs no cluster
configuration. Setting it explicitly also makes ktunnel behave the same
everywhere rather than inheriting whatever the runtime happened to choose,
which is the better argument for the change.

`expose` reads the ports from the container spec it already builds. `inject`
does not have them — its tunnelled ports arrive as CLI args — so
`cmd/inject.go` must plumb `args[1:]` down to the sidecar builder. That is the
only real cost of making the sysctl conditional.

A cluster admin can still forbid the sysctl through an SCC's
`forbiddenSysctls`, so a bind failure must name the sysctl, not only `--port`.

## Testing

Every bug that escaped this month was invisible to a fake client. `0400`
passed six shape assertions and crash-looped. `NET_BIND_SERVICE` would have
passed any assertion written for it and done nothing. Both are properties of a
running process, not of a struct.

### Unit, with the fake client — run always

Structural guards, each pinned to a reason rather than a literal value:

- no `RunAsUser` in the container spec at all — the #87 regression guard
- `Drop: ALL` present; `AllowPrivilegeEscalation` false
- the sysctl appears **only** when a source port is below 1024
- `inject` plumbs its ports through, so its sidecar gets the sysctl too

### Integration, on kind — in CI

The properties only a real kubelet can answer:

- the pod reaches Ready with **zero restarts** — the assertion that would have
  caught v2.4.0
- the server loads its certificate and serves TLS
- a probe pod reaches the Service and gets the local process's response
- a source port below 1024 actually binds

**The low-port test must first set `ip_unprivileged_port_start=1024` on a
control pod.** Otherwise kind's default of `0` makes it pass whether or not
ktunnel's sysctl works — a test that passes for the wrong reason, which is
worse than no test. This exact false positive occurred while measuring for
this document.

An **OpenShift-shaped case** covers what no local cluster can: no `RunAsUser`
in the spec, the pod forced to a high arbitrary UID, credentials still
readable.

Mechanics: build the image, `kind load docker-image`, guard with
`//go:build integration` so `go test ./...` stays fast, and run it as its own
GitHub Actions job on pull requests and main.

## Out of scope

- `setcap cap_net_bind_service=+ep` on the binary. It would work, but the
  binary is UPX-compressed in a `FROM scratch` image, and the sysctl needs no
  build-time machinery.
- Running as root to bind low ports. It contradicts the non-root property this
  design is preserving.
- `fsGroup` anywhere. OpenShift assigns it, and depending on it trades #87 for
  a different form of the same bug.
