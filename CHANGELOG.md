# Changelog

## Unreleased

A tunnel that loses its connection now comes back on its own.

### Added

- **Reconnect with backoff.** ([#114]) `expose`, `inject deployment` and
  `client` no longer die with their tunnel, and no longer sit there
  holding a dead one. When the connection is lost, ktunnel says so and
  rebuilds it: 1 second before the first retry, doubling to a ceiling of
  30 seconds, spread by ±20% so that everyone who lost the same cluster
  does not come back at the same instant. The delay returns to 1 second
  once a tunnel has stayed up for a minute, so a link that flaps once an
  hour does not creep to the maximum and stay there.

  What that covers, in the terms it happens to you: a closed laptop lid,
  a VPN that drops, a network that comes and goes, and — for `expose`
  and `inject` — a tunnel server pod that is rescheduled onto another
  node. The pod case rebuilds the whole local stack, not just the gRPC
  stream: the pod is resolved again by name and the port-forward is
  built again, because a forward is bound to the pod name it was created
  with and a replacement pod has a different one.

  The state changes are logged at INFO and are meant to be grepped, since
  that is what they replace:

  ```
  tunnel lost: rpc error: code = Unavailable ...
  reconnecting in 4s (attempt 3)
  tunnel established
  ```

  If you have a wrapper script that tails ktunnel's logs for a lost
  connection and kills the process, you can delete it.

- **`--exit-on-disconnect`**, for running ktunnel under a process
  supervisor: exit non-zero the first time the tunnel drops, instead of
  reconnecting. ([#133])

- **`--max-reconnect-attempts`**, to give up after N consecutive failed
  attempts. `0`, the default, retries forever.

  Both flags are on `expose`, `inject deployment` and `client`. The
  defaults keep the interactive behaviour you would expect — a tunnel
  that just keeps working — and Ctrl+C still exits 0. Giving up exits 1.

- **gRPC keepalive on the client**: a ping every 30 seconds, timing out
  after 10. Without it a half-open connection — the suspended laptop —
  never errors at all: the stream goes quiet and the client waits
  forever for data that is not coming. There is nothing to reconnect if
  nothing ever notices.

### Open connections do not survive a reconnect

This is worth stating plainly, because "it reconnects" invites the
opposite assumption. When the tunnel drops, every TCP connection that
was open through it stops working, and the reconnect binds a fresh
listener on the same port. Anything using the tunnel has to reconnect
too — the same as when any TCP proxy restarts. A long-running database
session or a streaming request is ended, not resumed. Resuming them
would need a session-resumption protocol on both sides, which is not
something a development tool should carry.

A connection that was idle across the break may still *look* open from
the cluster side until something is sent on it; the first use is what
fails. Reconnect on failure rather than trusting an idle socket.

### Fixed

- **`--tls` never did anything.** ([#114] work, found on the way) The
  option that turns TLS on read the certificate path *before* it was
  assigned, so the flag it set was always false. Every ktunnel ever run
  with `--tls` and `--ca-file` connected in plaintext and said nothing
  about it. If you believed a tunnel of yours was encrypted, it was not.
  It is now — see the known issue below for `expose` and `inject`, where
  it still cannot work end to end.

- **The tunnel server did not stop when it was told to.** Cancelling its
  context closed the port it accepts connections on and nothing else, so
  every tunnel already open kept running and kept its forwarded ports
  bound. A stopped server went on carrying traffic, and nothing could
  take its place. `ktunnel server` also reported the resulting closed
  listener as a fatal error on Ctrl+C; a shutdown you asked for is now
  a shutdown, and exits 0.

- **Neither side leaks a goroutine and a socket per session, per
  reconnect.** On both the client and the in-cluster server, the reader
  for each open connection handed it over to the sender on a channel
  that no longer had anybody reading it once the stream died, and parked
  there for good. Harmless when a dead tunnel ended the process; not
  harmless now that the tunnel is rebuilt whenever the network blinks,
  and least of all on a server pod that stays up for days.

### Known issues

- **A v2.1 client against a v2.0.x server image loses its tunnel every
  two minutes.** The client now sends a keepalive ping every 30 seconds.
  A gRPC server rejects pings closer together than five minutes unless
  it is configured otherwise, and after three of them it sends GOAWAY
  `too_many_pings` and drops the connection — so the tunnel dies roughly
  every two minutes, reconnects, and dies again.

  The v2.1 server image permits them, so this only bites when the
  in-cluster server is older than the client: `--server-image` pinned to
  a v2.0.x tag, or an existing deployment reused with `-r`/`--reuse`
  that is still running one. The default image tracks the client
  version, so an unpinned `expose` is unaffected. The fix is to let the
  image follow the client, or pin it to v2.1 or later.

- **`--tls` is now rejected by `expose` and `inject deployment`.** It
  never worked there and cannot be made to work by fixing the client
  alone: nothing mounts a certificate into the in-cluster server's
  container, it is never started with `--tls`, and `--cert`/`--key`
  reach it as a single unparsed argument (the v2.0.1 known issue, still
  open). There is no certificate provisioning to speak of.

  So rather than let a `--tls` client fail its handshake against the
  plaintext server it has just created — after creating a Deployment and
  a Service — both commands refuse the flag before they touch the
  cluster, and say what does work. If you were passing `--tls` to
  `expose` and it appeared to work, it was never encrypting anything.
  TLS between a standalone `ktunnel client` and `ktunnel server` does
  work, and is now the way to have an encrypted tunnel.

- **Reconnecting the port-forward to a *renamed* pod is not covered by
  an automated test**, because it cannot be exercised without a real API
  server — and it is the scenario [#114] is actually about. The stream
  layer above it is tested end to end, including killing the tunnel
  server and restarting it. This path is covered by code review and
  manual testing only.

- The tunnel is still unauthenticated, `--scheme udp` is still ignored,
  and `inject` still supports only single-replica Deployments. See the
  v2.0.1 known issues.

## v2.0.2

Packaging fix. Functionally identical to v2.0.1 — same code, same
container image contents.

v2.0.1 published its GitHub release and container images correctly, but
the Homebrew tap update failed on a configuration error: goreleaser
requires the tap `token` field to be exactly `{{ .Env.VAR }}` and
rejects anything else, including a conditional. It rejects it at
*publish* time, after the release is already out, so the release
succeeded and the packaging did not. The krew-index update was then
skipped because the job had already failed.

If you installed v2.0.1 from a GitHub release or a container registry,
there is no reason to upgrade. If you install via Homebrew or krew,
v2.0.2 is the first version you will see.

## v2.0.1

The first release since v1.6.1 (September 2023). It carries about two
years of unreleased work on master, plus a round of repairs to the
release pipeline itself — which turned out to be the reason nothing had
shipped.

If you have been running v1.6.1, or building from master and wondering
why `ktunnel expose` pulled an image that did not exist, this is the
release that fixes it.

> **Why v2.0.1 and not v2.0.0?** A `v2.0.0` tag was pushed in December
> 2024, back when the release pipeline was broken. It produced no
> release and no artifacts, but it is a published tag and moving it
> would silently break anyone who had already fetched it. Numbering
> forward was cheaper than rewriting history. There is no v2.0.0
> release, and there never was.

### Breaking changes

- **`pkg/common` session API.** `NewSession`, `NewSessionFromStream` and
  `GetSession` are replaced by methods on a new `SessionStore` type,
  created per tunnel side rather than shared in a package-level
  variable. Only ktunnel's own client and server used these, so this
  affects you only if you imported `pkg/common` directly.
- **Go 1.25** is now the minimum for building from source, following
  security updates to `golang.org/x/net` and `google.golang.org/grpc`.
  Released binaries and container images are unaffected.

### Fixed

- **`invalid UUID length: 0` — the error that was never the error.**
  ([#88], [#66], [#143])

  When the in-cluster server could not bind its listener it sent back a
  frame saying exactly why — a port conflict, or a permission denial on
  a privileged port. That frame carries no session ID, because it is not
  about a session. The client discarded the message entirely, failed to
  parse the empty ID, logged `failed parsing session uuid from stream,
  skipping`, **did not skip**, and opened a bogus session on the zero
  UUID. The real diagnosis never reached anyone.

  Server errors are now reported at the level the server sent them, and
  an unparseable session ID actually skips the frame.

- **The tunnel server no longer panics on a closing session.** A close
  frame for a session already reaped from the store dereferenced a nil
  pointer and took the whole server pod down.

- **`waiting for deployment to be ready` no longer hangs.** ([#134],
  part of [#121])

  Readiness was observed with a watch, and a watch only delivers events
  that occur after it is established — so a rollout that finished first
  produced no event at all, and the caller blocked until the progress
  deadline expired, 605 seconds later. Readiness is now polled, so a
  rollout that is already complete is seen immediately. The watch was
  also keyed on whatever labels the caller passed in, which with
  `--reuse` could match nothing, or everything in the namespace.

- **Ctrl+C works while waiting for a rollout.** ([#134], part of [#114])
  `expose` and `inject` waited on readiness with no escape, so an
  interrupt was accepted, logged, and then ignored until the rollout
  watcher gave up.

- **`expose` no longer proceeds after a failed rollout.** It discarded
  the readiness result entirely and set up port-forwarding regardless.

- **Readiness is read from the created deployment**, not the local spec
  that was sent to the API server. On the local spec
  `ProgressDeadlineSeconds` is always nil and `Spec.Strategy` is zero,
  so the progress deadline silently fell back to the default and the
  `MaxUnavailable` warning could never fire.

- **Session state is no longer read and written unsynchronised.**
  `Session.Open` now goes through the mutex.

- Resolved several data races and a shutdown deadlock that occurred when
  the local port was already in use.

- Memory requests and limits are interpreted as megabytes, not
  gigabytes.

### Added

Much of this landed on master over the past two years and has never
appeared in a release.

- **Automatic cleanup of created resources.** ([#25]) Deployments and
  services created by `expose` are tracked and removed on `SIGINT` or
  `SIGTERM`, with a 30-second timeout so shutdown cannot hang.
- **Server resource limits**: `--server-cpu-request`,
  `--server-cpu-limit`, `--server-memory-request`,
  `--server-memory-limit`. (partially [#118])
- `--pod-tolerations`, for scheduling the tunnel server onto tainted
  nodes.
- `--deployment-annotations` and `--deployment-labels`.
- `--service-type`, for exposing the tunnel as `NodePort`,
  `LoadBalancer` or `ExternalName`.
- `--portname`, for naming the container port.
- `--context`, for selecting a kubeconfig context.
- `--verbose` now also raises the log level of the Kubernetes package.
- **Container images for `linux/arm64`** alongside `linux/amd64`.
  ([#90])
- **Images are also published to `ghcr.io/omrikiei/ktunnel`**, which has
  no anonymous pull limits. Docker Hub remains the default.

### Changed

- The version is stamped into the binary from the git tag at build time.
  Previously it was a constant, and since the default `--server-image`
  tag is derived from it, editing one without the other left `expose`
  pulling an image that was never published. That cannot happen now.
- The container image has an `ENTRYPOINT`, so `docker run ktunnel
  version` works. Previously any argument replaced the binary itself.
- Release names no longer contain the CI runner's username. Every
  previous release is titled something like "ktunnel-v1.6.1 runner".
- Test coverage on the tunnel data path went from nothing to 88.9%
  (`pkg/common`) and 68.0% (`pkg/client`), including an end-to-end test
  that runs a real client and a real server over TCP.
- CI now builds the container image on every pull request, so a broken
  image is caught before a release rather than after.

### Known issues

- **`--scheme udp` is silently ignored.** The scheme lookup is
  case-mismatched against the protobuf enum, so it always falls back to
  TCP. Tracked for v2.1; until then, treat ktunnel as TCP-only.
- **`--cert` and `--key` do not work for `expose` and `inject`.** They
  are passed to the in-cluster container as a single argument, which
  cobra never parses. Tracked for v2.1. TLS between a standalone
  `ktunnel client` and `ktunnel server` is unaffected.
- **The tunnel is unauthenticated.** Any pod that can reach the created
  Service can open a tunnel to the machine running the client. Avoid
  `--service-type LoadBalancer`. Authentication is planned for v2.1.
- **There is no reconnect.** A closed laptop lid, a network interruption
  or a pod restart leaves a tunnel that does not recover and does not
  exit. Tracked as [#114] for v2.1.
- **`inject` supports only single-replica Deployments**, not
  StatefulSets. Tracked as [#91].
- The `--reuse` help text describes the wrong behaviour. ([#120])

### Thanks

To [@doctorpangloss][dp] for diagnosing the readiness hang and the
deployment-template mix-up ([#147]), and to [@idsulik][id] for
identifying the shutdown freeze ([#133]) that shaped the reconnect work
planned for v2.1.

[#25]: https://github.com/omrikiei/ktunnel/issues/25
[#66]: https://github.com/omrikiei/ktunnel/issues/66
[#88]: https://github.com/omrikiei/ktunnel/issues/88
[#90]: https://github.com/omrikiei/ktunnel/issues/90
[#91]: https://github.com/omrikiei/ktunnel/issues/91
[#114]: https://github.com/omrikiei/ktunnel/issues/114
[#118]: https://github.com/omrikiei/ktunnel/issues/118
[#120]: https://github.com/omrikiei/ktunnel/issues/120
[#121]: https://github.com/omrikiei/ktunnel/issues/121
[#133]: https://github.com/omrikiei/ktunnel/pull/133
[#134]: https://github.com/omrikiei/ktunnel/issues/134
[#143]: https://github.com/omrikiei/ktunnel/issues/143
[#147]: https://github.com/omrikiei/ktunnel/pull/147
[dp]: https://github.com/doctorpangloss
[id]: https://github.com/idsulik

---

Older releases predate this changelog; see the
[releases page](https://github.com/omrikiei/ktunnel/releases).
