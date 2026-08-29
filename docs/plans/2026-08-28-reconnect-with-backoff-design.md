# Reconnect with backoff

Design for [#114](https://github.com/omrikiei/ktunnel/issues/114).
Target: v2.1.

## Problem

There is no retry logic anywhere in the codebase. A closed laptop lid, a
VPN drop, a network interruption or a pod restart leaves a tunnel that
neither recovers nor exits — so a supervisor cannot restart it either.
Users work around this with shell wrappers that tail ktunnel's own logs
for "lost connection" and then kill every process in the container.

Three things combine to produce that zombie:

1. **`RunClient` never returns.** It blocks on `<-ctx.Done()`, so a dead
   stream leaves it parked forever.
2. **`PortForward` discards its own failures.** `forwarderErrChan` is
   read exactly once during startup. A forward that dies later sends to
   a channel nobody reads, blocking that goroutine permanently.
3. **Half-open connections are never noticed.** With no gRPC keepalive,
   a suspended laptop leaves TCP that never errors, so nothing detects
   the tunnel is gone.

The port-forward is bound to a *resolved pod name*
(`getPortForwardURL(config, namespace, podName)`). When a pod is
rescheduled the forward is permanently dead, because the replacement pod
has a different name. Reconnecting only the gRPC stream would therefore
look like a fix while still failing the case reporters actually hit.

## Decisions

| Question | Decision |
| --- | --- |
| Scope | Full-stack recovery: re-resolve pod → rebuild forward → reconnect stream |
| Give-up policy | Retry forever by default; `--exit-on-disconnect` and `--max-reconnect-attempts` to opt out |
| Commands | `expose`, `inject`, and `client` |
| Testing | Interface + fakes; no cluster-backed CI job |

## Architecture

A new `pkg/supervisor` package owning one idea: run an attempt until it
fails, then retry with backoff.

```go
// Attempt establishes a tunnel and blocks until it fails. It calls
// established once the tunnel is actually up, before it blocks serving it.
type Attempt func(ctx context.Context, established func()) error

type Supervisor struct {
    Attempt     Attempt
    Backoff     Backoff  // 1s → 2s → 4s … capped at 30s, ±20% jitter
    MaxAttempts int      // 0 means retry forever
    ExitOnFirst bool
}
```

Each command supplies a closure that establishes its whole stack and
blocks:

- **expose / inject** — resolve pod name, build the SPDY forward, dial
  gRPC, open `InitTunnel`, call `established`, block until any layer
  fails.
- **client** — dial gRPC, open `InitTunnel`, call `established`, block.
  No forward layer.

One loop, different closures.

Backoff resets to its floor once a tunnel has stayed up for 60 seconds,
so a flaky link does not creep to a permanent 30-second delay. That
minute is timed from `established`, not from the attempt starting. The
headline #114 scenario is a dead network, where a connect blocks for
around 75 seconds on default Linux and macOS timeouts before it fails —
longer than the threshold. An attempt timed from its launch would call
that a stable tunnel, reset the backoff to 1s and clear the
`MaxAttempts` streak: a hot retry loop that also logs the opposite of
what happened.

### Prerequisite changes

- `RunClient` returns an error when its streams die, instead of blocking
  on the context forever.
- `PortForward` surfaces post-startup forwarder errors rather than
  writing them to an unread channel.
- gRPC keepalive (`ClientParameters{Time: 30s, Timeout: 10s}`) so a
  half-open connection becomes a prompt error. Without this the headline
  laptop-sleep scenario still hangs.

## Failure semantics

**Open connections do not survive a reconnect.** When the stream dies,
the server closes its listener and drops every cluster-side TCP
connection. On reconnect a fresh listener binds the same port. This is
what any TCP proxy restart does; the documentation should say so rather
than implying seamless resumption.

**Each attempt gets a fresh `SessionStore`.** Because the store is now
per-side and injectable rather than a process global, a reconnect hands
the client a new one. Sessions from the dead stream are dropped and
their connections closed, instead of leaking into the next attempt keyed
by UUIDs the new server has never issued.

**Cluster resources are never recreated.** The supervisor rebuilds the
forward and the stream only. If the Deployment is deleted mid-session,
pod resolution fails and we keep retrying with backoff, logging why.
Silently recreating resources is not something a recovery path should
do. `ResourceTracker` teardown still runs exactly once, on exit.

**The local port must be released before retrying.** Each attempt closes
its `stopChan` and waits for the forwarder to release the local port
before the next begins. Otherwise the retry fails with `address already
in use`, and so does every attempt after it.

**State transitions are logged at INFO**, deliberately greppable, since
this is what replaces the wrapper scripts:

```
tunnel lost: rpc error: code = Unavailable ...
reconnecting in 4s (attempt 3)
port forwarding to .../pods/proxy-7d9f-x2m1/portforward
tunnel established
```

`tunnel established` is emitted the moment the attempt reports itself up,
because that is the line a user is waiting for. Crossing the 60-second
stability threshold a minute later logs `attempt stable, backoff reset`
at DEBUG: it is internal observability, not a second announcement of
something already reported.

Exit codes: `0` on Ctrl+C, `1` on give-up.

## CLI

| Flag | Default | Meaning |
| --- | --- | --- |
| `--exit-on-disconnect` | false | Exit non-zero on the first failure instead of retrying |
| `--max-reconnect-attempts` | 0 | Give up after N consecutive failures; 0 means never |

Defaults preserve today's "just keep working" expectation for
interactive use, while giving supervisor-style users the clean exit code
requested in [#133](https://github.com/omrikiei/ktunnel/pull/133).

## Testing

**Supervisor logic** is pure and fully unit-tested: backoff sequence,
jitter bounds, `MaxAttempts`, `ExitOnFirst`, reset-after-stable, and
context cancellation.

**gRPC-level reconnect** is tested end to end with the loopback harness
from #158: start a server and client, confirm bytes flow, kill the
server, assert the loss is detected, restart it on the same port, and
assert the tunnel re-establishes and bytes flow again. This also asserts
that a session from the dead stream does not survive into the next
attempt.

**The establish step sits behind an interface**, so the supervisor's
sequencing — release the port, re-resolve, rebuild, reconnect — is
tested with a fake that has no cluster behind it.

### Accepted gap

Rebuilding a SPDY forward against a **newly named pod** is not covered
by any automated test, because it cannot be exercised without a real API
server. That is precisely the scenario in #114.

A `kind`-based CI job was considered and deliberately not taken, to
avoid the CI cost and flakiness. The consequence is that this path is
covered only by code review and manual testing, so **before releasing
v2.1, verify by hand**: run `ktunnel expose`, `kubectl delete pod` the
tunnel server, and confirm the tunnel recovers on its own.

This gap should be recorded in the v2.1 release notes' known issues if
it has not been closed by then.

## Out of scope

- Resuming in-flight connections across a reconnect. Not possible
  without a session-resumption protocol on both sides; not worth it for
  a development tool.
- Recreating deleted cluster resources.
- Reconnect for the standalone `ktunnel server` command, which is a
  listener rather than a client and has nothing to reconnect to.
