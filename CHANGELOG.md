# Changelog

## v2.0.0

The first release since v1.6.1 (September 2023). It carries about two
years of unreleased work on master, plus a round of repairs to the
release pipeline itself — which turned out to be the reason nothing had
shipped.

If you have been running v1.6.1, or building from master and wondering
why `ktunnel expose` pulled an image that did not exist, this is the
release that fixes it.

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
