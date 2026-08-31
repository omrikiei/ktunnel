# ktunnel roadmap

ktunnel opens a reverse tunnel from a Kubernetes cluster to your machine, so
a Service in the cluster resolves to code running on your laptop. It is a
single static binary, it installs no operator, it defines no CRDs, and it
needs no cluster-wide permissions. That is the whole pitch, and everything
below is ordered to protect it.

## How to read this

Every item is a task. Tick it when it lands; a release is done when its list
is. Each carries its effort and its issues:

- **S** ≈ a focused change · **M** ≈ a change with a design decision in it ·
  **L** ≈ needs its own design document

Releases are ordered by four questions, applied in order:

1. **Does the tool do what it says?** A command that cannot work is worse
   than a missing feature, because it costs the user a debugging session
   before they find out.
2. **Is it predictable?** Resources it creates, adopts and deletes should be
   obvious before and after the fact.
3. **Is it safe?** Ordered third deliberately — see the note in v2.4.
4. **Does it reach far enough?** More workload kinds and more platforms.

---

## v2.1 — Reliability · **shipped**

The tunnel survives losing its connection: closed laptop, dropped VPN,
rescheduled tunnel-server pod.

- [x] Reconnect with exponential backoff and jitter, rebuilding the whole
      local side each attempt — **L** · #114
- [x] `--exit-on-disconnect` and `--max-reconnect-attempts`, for process
      supervisors — **S** · #133
- [x] gRPC keepalive on the client, so a half-open connection is noticed at
      all — **S**
- [x] `--tls` turns TLS on — it never did, on any release — **S** · #114
- [x] The server stops when its context is cancelled, instead of carrying
      traffic on every tunnel already open — **M**
- [x] Neither side leaks a goroutine and a socket per session per
      reconnect — **M**
- [x] A failed port-forward no longer leaks two goroutines per attempt,
      unbounded — **S**

---

## v2.2 — `inject` actually works · **done, unreleased**

**Why first:** `inject` is one of ktunnel's two headline commands, and it
could not forward to any Deployment that did not already carry the two
labels `expose` sets on the Deployments it creates. The sidecar injected,
the pod reported `2/2 Running`, and the port-forward retried `found 0
running pod(s)` forever. Not a regression — it predated the v2.x work, so
half the product had been broken for years.

- [x] Resolve pods from the Deployment's own `spec.selector` instead of
      ktunnel's labels — **S** · #171, #115
- [x] Refuse a Deployment whose selector is absent or empty, by name, rather
      than matching every pod in the namespace — **S**
- [x] Skip pods that are terminating: `Phase: Running` for the whole grace
      period, and the newest match until a replacement exists — **S**
- [x] Rollout-window test: the selector matches old and new pods at once —
      **S** · #171
- [x] Decide and document multi-replica `inject` semantics — **M** · #96
      <br>Decided: every replica is injected and tunnelled. The sidecar's
      listeners are pod-local, so one arbitrary pod of N leaves N-1 pods
      with the port closed and nothing to say which one works.
- [x] Document the threat model, ahead of the fix in v2.4 — **S** · #80
      <br>[docs/security.md](docs/security.md). Writing down "the tunnel is
      unauthenticated; anyone who can reach the Service can reach your
      laptop" cost an afternoon. The fix is expensive; the warning was not.

---

## v2.3 — Predictable resources

**Why second:** this is the single largest cluster of user complaints, and
#134 is worth reading in full — one user listing everything confusing about
the tool at once. Half of its list is already fixed in v2.0–v2.1. What
remains is resource lifecycle and messaging.

- [x] `--reuse` genuinely reuses, instead of merge-patching ktunnel's own
      template over the Deployment it was pointed at — **M** · #120, #94
- [x] `--reuse` removes what the run created and leaves what it adopted;
      teardown keyed off the flag rather than off what happened — **S** · #120
- [x] Errors on the `expose` path name the object and the way out, including
      a `Get` failure that is not "not found" — **S** · #134
- [ ] Say what will be created, adopted and deleted **before** doing it —
      **S** · #134
      <br>Each object is now reported as it is created or adopted, and the
      exit message says which will be removed. The pre-flight summary is
      what is left.
- [ ] Namespace precedence: flag vs. kubeconfig context, resolved once and
      reported — **S** · #134
- [ ] Errors naming the object and the fix on the `inject` and forwarding
      paths — **M** · #134
- [ ] `--print-manifests` / `--dry-run`: emit the Deployment and Service to
      apply yourself — **M** · #94, #120
      <br>Reprioritised down: both issues wanted this because they were
      fighting `-r` to adopt a hand-written Deployment, and `-r` adopts
      properly now. What remains is the convenience of not writing the
      manifest from scratch — still worth having, no longer load-bearing.

---

## v2.4 — Secure by default

**Why third, not first.** The in-cluster tunnel server is unauthenticated:
anything in the cluster that can reach its Service can open a tunnel to your
machine. That is the most serious design gap ktunnel has, and it is ordered
below broken commands and confusing resources for two reasons — ktunnel is
used against development clusters by the person who owns both ends, and the
fix is the largest piece of work on this page. Ordered third, not ignored:
v2.2 shipped the honest warning while this is built.

- [ ] Per-session credentials: generate a CA and server certificate, ship
      them as a Secret, mount into the server pod — **L** · #166
- [ ] Make `--tls` real for `expose` and `inject` — today both reject it,
      because nothing mounts a certificate — **L** · #166, #70
- [ ] Bearer token shared between client and server, on by default —
      **M** · #80
- [ ] `--cert`/`--key` reach the server as one unparsed argument —
      **S** · #166
- [ ] Least-privilege RBAC and a documented ServiceAccount — **S**
      <br>Cheapest item in this release, and half-written already: the verb
      table is in [docs/security.md](docs/security.md).
- [ ] Ingress annotation for TLS backends — **S** · #69

---

## v2.5 — Beyond single-replica Deployments

- [ ] `ktunnel inject statefulset` — **M** · #91
- [ ] OpenShift: drop the `RunAsUser` that OCP rejects; a contributor has
      offered a PR — **S** · #87
- [ ] Tunnel server can bind privileged ports (<1024) — **S** · #164
- [ ] Windows behaviour regressed somewhere around 1.5 — needs a
      reproduction first — **M** · #121

---

## Continuous

Not tied to a release; pick up whenever.

- [ ] Shell completion — Cobra provides it almost free — **S** · #76
- [ ] `ktunnel status`: what is running in this namespace and what is
      tunnelled — **M**
- [ ] Config file for repeated setups — **M**
- [x] `--server-memory-limit` help text said "CPU Limit in mega-bytes" — **S**
- [ ] Automated coverage for the pod-rename reconnect path, manual today — **S**
- [ ] Codespaces / remote-dev-container guidance — **S** · #81

---

## Issues to verify and close

Resolved as far as anyone can tell; worth confirming rather than carrying.

- [ ] **#118** — custom requests/limits: the four `--server-cpu-*` /
      `--server-memory-*` flags exist.
- [ ] **#123** — pods picked by name prefix, so `react1` matched `react11`:
      pod lookup is an exact label match now, not a prefix.
- [ ] **#70** — `--ca-file` ignored: it genuinely was. `--tls` is fixed for
      standalone client/server in v2.1 and rejected up front for `expose`
      and `inject`, which is #166.
- [ ] **#96** — answered by the multi-replica decision in v2.2.
- [ ] **#80** — answered by [docs/security.md](docs/security.md); the token
      itself is the v2.4 item above.
- [ ] **#171**, **#115** — fixed in v2.2.
- [ ] **#120**, **#94** — `--reuse` adopts instead of overwriting, and cleans
      up only what it created.

---

## Not planned

Saying no keeps the pitch at the top of this page true. These are decisions,
not tasks.

- **Web UI** (#42) — a different product with a different threat model.
- **UDP** — `--scheme udp` is accepted and ignored today; the honest fix is
  to reject it, not to implement a datagram tunnel over a TCP stream.
- **Session resumption across a reconnect** — when the tunnel drops, open TCP
  connections through it end, the same as when any proxy restarts. Resuming
  them needs a session-resumption protocol on both sides, which is more
  machinery than a development tool should carry.
- **Replacing Telepresence or mirrord** — they intercept traffic with far more
  cluster machinery. ktunnel's reason to exist is that it does not.
