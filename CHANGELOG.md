# Changelog

## Unreleased

### Added

- **`--print-manifests`** on `expose`: writes the Deployment and Service
  ktunnel would create, as YAML, and exits without contacting the
  cluster. ([#94], [#120])

  ```sh
  ktunnel expose myapp 80:8000 --print-manifests | kubectl apply -f -
  ```

  Both issues arrived at hand-written manifests the same way — a private
  registry, a security context their cluster admits — and then fought
  `-r` to get ktunnel to adopt what they had written. `-r` adopts
  properly as of v2.2.0, so this is no longer the way out of a bug; it
  is the convenience of starting from what ktunnel would have created.
  The output comes from the same code the command runs, so it cannot
  drift from what is actually created. It goes to stdout on its own —
  every log line, including which namespace was chosen, goes to stderr —
  so the pipe above stays clean.

### Fixed

- **Resource requests and limits read the way Kubernetes writes them.**
  ([#118]) `kubectl describe pod` showed `cpu: 500e-3` and `memory:
  1e9`. Those values were correct and nothing else in a cluster writes
  them that way, so they read as a bug and could not be compared by eye
  against a LimitRange or the deployment next to them. They are now
  `500m` and `1G`. The cause was a zero-valued `resource.Quantity`,
  whose empty format serialises as DecimalExponent.

  The other half of that issue — the four `--server-cpu-*` /
  `--server-memory-*` flags — has existed since v2.0.

### Changed

- **A port argument that cannot be parsed is now an error.** It was
  logged and skipped, so a typo in one of several ports produced a
  tunnel that came up looking healthy and was quietly missing the port
  you cared about.

## v2.2.0

`inject` works against deployments ktunnel did not create, which is to
say: against deployments. `--reuse` reuses. Both commands say what they
are about to do to which namespace, before they do it.

### Breaking changes

- **`k8s.KubeService.ExposeAsService`** returns `(*ResourceTracker,
  error)` rather than `error`, and no longer takes a `kubecontext`
  argument. The tracker holds what the call created, and only that, so
  teardown can remove exactly that; the context argument re-derived
  clients the receiver already had. Internal to ktunnel's own commands.

### Fixed

- **`inject` never forwarded to anything.** ([#171], [#115]) Pods were
  resolved by the two labels `expose` puts on the deployments it creates
  itself. An application deployment is labelled however its author chose,
  so nothing matched: the sidecar went in, the pod reported `2/2
  Running`, and the port-forward retried `found 0 running pod(s)`
  forever. Pods are now resolved from the deployment's own
  `spec.selector`, the way every other tool that has to find a
  deployment's pods does it. This was not a regression — half the product
  had been broken for years.

  A deployment whose `spec.selector` is absent or empty is refused by
  name rather than matched, since an empty selector matches every pod in
  the namespace and forwarding to an unrelated one would be reported as
  success.

- **A pod being deleted is no longer forwarded to.** It stays `Phase:
  Running` for the whole of its grace period and is the newest match
  until its replacement exists, so it was preferred over the pod that
  was staying — and the forward died with it.

- **`--reuse` reuses, instead of overwriting what it was pointed at.**
  ([#120], [#94]) It merge-patched ktunnel's own template over the
  existing deployment — ktunnel's image, ktunnel's resources, ktunnel's
  security context — keeping only the labels and selector that a patch
  cannot change anyway.

  Both reports are the same story: you hand-write a tunnel-server
  deployment because you need an image from your own registry and a
  security context your cluster admits, pass `-r` so ktunnel adopts it,
  and ktunnel overwrites the image with `docker.io/omrieival/ktunnel`,
  rolls a second revision, and leaves a pod stuck pulling an image the
  cluster cannot reach.

  Nothing is written now. An existing Deployment or Service is used
  exactly as it stands, and ktunnel logs what it adopted — replicas and
  image — so it is clear which object the tunnel is running in.

- **`--reuse` cleans up what it created.** Teardown keyed off the flag
  rather than off what had happened, so `-r` against a namespace where
  the objects did not exist created them and then left them behind on
  every run. It now removes exactly what the run created, and leaves
  exactly what the run adopted — and the exit message says which.

- **Errors on the `expose` path name the object and the way out.**
  `deployment with same name already exists` became `deployment
  default/myapp already exists; pass --reuse to tunnel through it as it
  is, or --force to replace it`. A `Get` that failed for any reason
  other than "not found" — forbidden, API server unreachable — used to
  fall through to that same "already exists" message, sending you to
  look at an object when the problem was your permissions.

- **The namespace of your kubeconfig context is used.** ([#134])
  `--namespace` defaulted to the literal string `default`, so the flag
  was always set and always won: the namespace your context selects was
  never read. If your context points at your team's namespace, and you
  ran ktunnel the way you run every other kubectl command, your
  Deployment and Service went to `default` — and nothing said so.

  The precedence is now the usual one: `--namespace` if given, otherwise
  the namespace of the kubeconfig context in play (`--context` selects
  which), otherwise `default`. It is resolved once, at startup, and
  reported with its source: `Using namespace team-a (kubeconfig context
  "dev")`.

  If you were relying on the old behaviour — running from a context with
  a namespace and expecting `default` anyway — pass `-n default`
  explicitly.

- **`expose` no longer creates a Deployment before refusing.** The
  Deployment was created first and the Service second, so a namespace
  that already held a Service of that name, without `--reuse`, got a
  Deployment created, then the run failed on the Service and left the
  Deployment behind. Both objects are checked before either is written,
  so the run either does all of it or touches nothing.

- **Errors on the `inject` and forwarding paths name the object and the
  fix.** ([#134]) `deployments.apps "api" not found` says neither which
  namespace was looked in nor which context was used, and between those
  two lies the answer most of the time. Every API failure on these paths
  now names the object as `deployment team-a/api` and, for the classes
  that have one, says what to do: a missing object points at
  `--namespace` and `--context`, a forbidden one at the permissions in
  [docs/security.md](docs/security.md), a rejected one at the
  credentials in the kubeconfig context, a conflict at whatever else is
  writing to the object.

  The forwarding path names both ends. "Found 1 running pod(s) for
  deployment web, want 3" now names the namespace and prints the
  `kubectl get pods -l ...` that shows you what ktunnel was looking at,
  and a forward that cannot bind its local port says so as a local
  problem with a local fix — `--port` — rather than as a port-forward
  failure that reads like a cluster one.

- **Ejecting a sidecar that is not there is no longer an error.** It
  came back as `IMAGE is not present on spec`, logged as `Failed
  removing tunnel sidecar` — an error, naming an image rather than the
  deployment, for a cluster that is already in the state you asked for.
  That happens after a rollout that never finished, and after someone
  has taken the container out by hand.

- **A port that fails to parse no longer becomes port 0.** The service
  and container port lists were indexed rather than appended, so a port
  argument that was skipped with a message left a zero-valued entry
  behind, and it was sent to the API server as if it had been asked for.

### Changed

- **`inject deployment` supports more than one replica.** ([#96]) It used
  to refuse with `sidecar injection only support deployments with one
  replica`, which rules out most of what a cluster runs.

  The alternative to refusing is not "forward to one of them". The
  sidecar's listeners are pod-local — only containers in an injected pod
  reach your machine through them — so forwarding to one arbitrary pod of
  N leaves N-1 pods with the port closed and nothing to say which pod is
  the working one. A deployment where a third of the requests reach your
  laptop and the rest get connection refused is worse to debug than one
  where none of them do.

  So it is all of them: one port-forward and one tunnel client per
  replica, all carrying traffic to the same local service. A deployment
  with three replicas takes three local ports counting up from `--port`,
  and opens three streams to your machine; ktunnel says so before the
  rollout starts. Replicas added while the tunnel is up are picked up the
  next time it is rebuilt, since the pods are resolved once per attempt.

### Added

- **An adopted Service that does not route to the tunnel is called out.**
  Under `--reuse`, if the Service has no port targeting the port the
  tunnel server binds, ktunnel says so and names the fix. A warning
  rather than an error: `--reuse` means the objects are yours, and
  overruling you on ktunnel's reading of your Service is how `--reuse`
  got into trouble in the first place.

- **A pre-flight summary.** ([#134]) `expose` and `inject deployment`
  now say what they are about to do before they do it, rather than
  narrating each object after the fact:

  ```
  In namespace team-a, ktunnel will:
    use the existing deployment team-a/myapp as it is (2 replica(s), image nexus.corp.example/ktunnel:v2.1.0); it will be neither modified nor deleted
    create service team-a/myapp (ClusterIP, port(s) 80->8080)
  On exit it will remove service myapp, and leave deployment myapp as it was.
  ```

  The last line is the one that was missing: with `--reuse` in play, which
  of these objects is yours and which disappears on Ctrl+C is not
  something you should have to infer from the flags you passed. `inject`
  states the same thing about the container it adds, how many pods that
  restarts, and which local ports the replicas take. `--force` says which
  objects it is about to delete before deleting them.

- **A written security model**, at [docs/security.md](docs/security.md).
  ([#80]) The tunnel is unauthenticated: anything in the cluster that can
  reach a tunnelled port reaches whatever is behind it on your machine.
  That has always been true and was never written down, so it is now —
  what is reachable from where, what is encrypted and what is not, what
  authenticates what, the namespaced Kubernetes permissions ktunnel uses,
  and how to narrow the exposure until the credentials work lands.

  It answers [#80] as asked, too: there is no `--token` flag; put the
  token in a kubeconfig context and select it with `--context`.

## v2.1.0

A tunnel that loses its connection now comes back on its own.

### Breaking changes

Three exported signatures changed. All three are internal to ktunnel's
own client and server, so this affects you only if you imported these
packages directly.

- **`k8s.KubeService.PortForward`** takes a `context.Context` and no
  longer takes a `*sync.WaitGroup`, and returns
  `([]string, <-chan error, error)` rather than `(*[]string, error)`.
  The context is what makes a forward against an unresponsive API server
  cancellable; the channel is how a forward that dies *after* startup
  reports it, which previously went to a channel nobody read. The ports
  are a plain slice because a `*[]string` could be nil, and callers
  checking it for emptiness dereferenced it.
- **`client.ReceiveData`** takes a `context.Context` and returns an
  `error`; **`client.SendData`** returns an `error`. A tunnel that
  stopped carrying traffic previously had no way to say so.

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

- **A failed port-forward no longer leaks two goroutines.** client-go
  closes its ready channel only after a dial *and* a listen have both
  succeeded, so the two most common reconnect failures — the API server
  unreachable, the local port not yet released by the previous attempt —
  never closed it, and the two goroutines waiting on it stayed for the
  life of the process. Measured at exactly two per failed attempt,
  unbounded: a laptop left overnight on a dead VPN accumulated
  thousands.

- **A configuration error is no longer retried forever.** ktunnel now
  tells a network that will come back apart from a command line that
  will not. A malformed port spec, a scheme it does not speak, a
  `--ca-file` that is not there: reported once, exit 1 — as before the
  reconnect loop existed. Anything else is retried. Without this,
  `ktunnel client 8000:not:a:port` logged the same parse failure every
  backoff interval, forever, under the default policy.

- **`getPodNames` no longer panics** when fewer pods are Running than
  the deployment asks for. It wrote into a slice sized by the replica
  count and indexed past the end — a crash, taking the process with it,
  during precisely the window between a pod being deleted and its
  replacement reaching Running. That window is the case reconnecting
  exists for.

- **`PortForward` no longer panics on a deployment scaled to zero.** It
  could report success with a nil port list, which the caller
  dereferenced while checking it for emptiness — intermittently, on
  about half of attempts, because the race behind it is a uniform choice
  between two ready channels.

- **A second Ctrl+C now kills the process.** Every signal after the
  first used to be swallowed, so a user watching a slow teardown — or an
  attempt stuck in a call to an unreachable API server — had no way out
  but another terminal. The first signal still shuts down cleanly; the
  second is handed back to the runtime. `ktunnel server` was the last
  command without this and now has it too.

- **`expose` and `inject` exit non-zero when the rollout fails.** They
  logged `deployment failed to become ready` and exited 0, so a systemd
  unit or a CI step saw success for a tunnel server that never started.
  Cluster resources created by the failed run are still cleaned up
  first.

- **`expose` no longer reports a cleanup failure for a cleanup that
  worked.** Two signal handlers raced to delete the same deployment and
  service -- one installed by the resource tracker, one by the tunnel
  session -- and whichever lost the race logged `Failed deleting k8s
  objects: services "..." not found` on every clean Ctrl+C, for work that
  had in fact succeeded. The tracker's handler is gone rather than
  coordinated with: it called `os.Exit` itself, so it could also cut the
  session's teardown short and override the exit code the supervisor
  meant to return, and it handled a strictly smaller set of signals.
  Teardown is idempotent besides, so a `kubectl delete` from another
  terminal is not reported as a failed cleanup either.

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
[#80]: https://github.com/omrikiei/ktunnel/issues/80
[#88]: https://github.com/omrikiei/ktunnel/issues/88
[#90]: https://github.com/omrikiei/ktunnel/issues/90
[#91]: https://github.com/omrikiei/ktunnel/issues/91
[#94]: https://github.com/omrikiei/ktunnel/issues/94
[#96]: https://github.com/omrikiei/ktunnel/issues/96
[#114]: https://github.com/omrikiei/ktunnel/issues/114
[#115]: https://github.com/omrikiei/ktunnel/issues/115
[#118]: https://github.com/omrikiei/ktunnel/issues/118
[#120]: https://github.com/omrikiei/ktunnel/issues/120
[#121]: https://github.com/omrikiei/ktunnel/issues/121
[#133]: https://github.com/omrikiei/ktunnel/pull/133
[#134]: https://github.com/omrikiei/ktunnel/issues/134
[#143]: https://github.com/omrikiei/ktunnel/issues/143
[#147]: https://github.com/omrikiei/ktunnel/pull/147
[#171]: https://github.com/omrikiei/ktunnel/issues/171
[dp]: https://github.com/doctorpangloss
[id]: https://github.com/idsulik

---

Older releases predate this changelog; see the
[releases page](https://github.com/omrikiei/ktunnel/releases).
