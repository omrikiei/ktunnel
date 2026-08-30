# ktunnel roadmap

ktunnel opens a reverse tunnel from a Kubernetes cluster to your machine, so
a Service in the cluster resolves to code running on your laptop. It is a
single static binary, it installs no operator, it defines no CRDs, and it
needs no cluster-wide permissions. That is the whole pitch, and everything
below is ordered to protect it.

## How this is ordered

Four questions, applied in order:

1. **Does the tool do what it says?** A command that cannot work is worse
   than a missing feature, because it costs the user a debugging session
   before they find out.
2. **Is it predictable?** Resources it creates, adopts and deletes should be
   obvious before and after the fact.
3. **Is it safe?** Ordered third deliberately — see the note in v2.4.
4. **Does it reach far enough?** More workload kinds and more platforms.

Effort is relative: **S** ≈ a focused change, **M** ≈ a change with a design
decision in it, **L** ≈ needs its own design document.

---

## v2.1 — Reliability (shipped)

The tunnel survives losing its connection. Closed laptop, dropped VPN,
rescheduled tunnel-server pod: ktunnel reports the loss and rebuilds the
tunnel with exponential backoff and jitter, rather than dying with it or
holding a dead one. `--exit-on-disconnect` and `--max-reconnect-attempts`
opt out for process supervisors.

It also carries a set of fixes that only look small: `--tls` never turned TLS
on at all, the server ignored its own shutdown, both sides leaked a goroutine
and a socket per session per reconnect, and a failed port-forward leaked two
goroutines per attempt without bound.

Closes #114, #133.

---

## v2.2 — `inject` actually works

**Why first:** `inject` is one of ktunnel's two headline commands, and it
cannot forward to any Deployment that does not already carry the two labels
`expose` happens to set on the Deployments it creates. Verified against a
live cluster: the sidecar injects, the pod reports `2/2 Running`, and the
port-forward retries `found 0 running pod(s)` forever. This is not a
regression — it predates the v2.x work. Half the product has been broken
for years, and the fix is small.

| Item | Issue | Effort |
|---|---|---|
| Resolve pods from the Deployment's own `spec.selector.matchLabels` instead of ktunnel's labels | #171, #115 | **S** |
| Decide and document multi-replica `inject` semantics — forward to one arbitrary pod of many is not obviously right | #96 | **M** |
| Rollout-window test: the selector matches old and new pods at once | #171 | **S** |
| Document the threat model, ahead of the fix in v2.4 | #80 | **S** |

The threat-model note is here rather than in v2.4 because writing down "the
tunnel is unauthenticated; anyone who can reach the Service can reach your
laptop" costs an afternoon and stops people from being surprised by it. The
fix is expensive; the warning is not.

---

## v2.3 — Predictable resources

**Why second:** this is the single largest cluster of user complaints, and
#134 is worth reading in full — it is one user listing everything confusing
about the tool at once. Half of its list is already fixed in v2.0–v2.1 (Ctrl+C
now works, dangling resources are cleaned up, there is a release again). What
remains is about resource lifecycle and messaging.

| Item | Issue | Effort |
|---|---|---|
| `--reuse` genuinely reuses, instead of creating another Deployment revision | #120, #94 | **M** |
| Say what will be created, adopted and deleted — before doing it, and again at exit | #134 | **S** |
| Namespace precedence: flag vs. kubeconfig context, resolved once and reported | #134 | **S** |
| Replace vague failures with errors naming the object and the fix | #134 | **M** |
| `--print-manifests` / `--dry-run`: emit the Deployment and Service to apply yourself | #94, #120 | **M** |

`--print-manifests` deserves its place. Two separate issues describe users
hand-writing their own tunnel-server Deployment because they need a private
registry or a non-default security context, then fighting `-r` to make
ktunnel adopt it. Emitting the manifest turns that from a fight into a
supported workflow, and it is the cheapest possible answer to "I need to
customise the server pod."

---

## v2.4 — Secure by default

**Why third, not first.** The in-cluster tunnel server is unauthenticated:
anything in the cluster that can reach its Service can open a tunnel to your
machine. That is the most serious design gap ktunnel has, and it is ordered
below broken commands and confusing resources for two reasons — ktunnel is
used against development clusters by the person who owns both ends, and the
fix is the largest piece of work on this page. Ordered third, not ignored:
v2.2 ships the honest warning while this is built.

| Item | Issue | Effort |
|---|---|---|
| Per-session credentials: generate a CA and server certificate, ship them as a Secret, mount into the server pod | #166 | **L** |
| Make `--tls` real for `expose` and `inject` — today both reject it, because nothing mounts a certificate | #166, #70 | **L** |
| Bearer token shared between client and server, on by default | #80 | **M** |
| `--cert`/`--key` reach the server as one unparsed argument | #166 | **S** |
| Least-privilege RBAC and a documented ServiceAccount | — | **S** |
| Ingress annotation for TLS backends | #69 | **S** |

---

## v2.5 — Beyond single-replica Deployments

| Item | Issue | Effort |
|---|---|---|
| `ktunnel inject statefulset` | #91 | **M** |
| OpenShift: drop the `RunAsUser` that OCP rejects; a contributor has offered a PR | #87 | **S** |
| Tunnel server can bind privileged ports (<1024) | #164 | **S** |
| Windows behaviour regressed somewhere around 1.5 — needs a reproduction first | #121 | **M** |

---

## Continuous

Not tied to a release; pick up whenever.

- Shell completion — Cobra provides it almost free (#76)
- `ktunnel status`: what is running in this namespace and what is tunnelled
- Config file for repeated setups
- `--server-memory-limit` help text says "CPU Limit in mega-bytes"
- Automated coverage for the pod-rename reconnect path, which is manual today
- Codespaces / remote-dev-container guidance (#81)

---

## Not planned

Saying no keeps the pitch at the top of this page true.

- **Web UI** (#42) — a different product with a different threat model.
- **UDP** — `--scheme udp` is accepted and ignored today; the honest fix is
  to reject it, not to implement a datagram tunnel over a TCP stream.
- **Session resumption across a reconnect** — when the tunnel drops, open TCP
  connections through it end, the same as when any proxy restarts. Resuming
  them needs a session-resumption protocol on both sides, which is more
  machinery than a development tool should carry.
- **Replacing Telepresence or mirrord** — they intercept traffic with far more
  cluster machinery. ktunnel's reason to exist is that it does not.

---

## Issues that look resolved

Worth verifying and closing rather than carrying:

- **#118** — custom requests/limits: the four `--server-cpu-*` /
  `--server-memory-*` flags exist.
- **#123** — pods picked by name prefix, so `react1` matched `react11`: pod
  lookup is an exact label match now, not a prefix.
- **#70** — `--ca-file` ignored: it genuinely was. `--tls` is fixed for
  standalone client/server in v2.1 and rejected up front for `expose` and
  `inject`, which is #166.
