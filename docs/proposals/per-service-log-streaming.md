# Proposal: per-service log streaming sockets routed by the operator

**Status:** Draft · **Area:** operator, runtime backends, operator auth, logs · **Surfaces:** operator HTTP/WS + GraphQL `serviceEndpoint`

## Summary

For each service, the **operator returns a streaming-log endpoint URL plus a
matching short-lived credential** in the service-info response, and the consumer
(the web console) simply opens that URL with that credential — one socket per
service it wants to watch, instead of re-querying or holding an aggregate
stream. The endpoint URL is a **routing indirection the operator owns**: it can
resolve to

1. the **operator's own port** (the ephemeral dev proxy, served directly),
2. the **Caddy edge** (the existing per-service ingress route), or
3. an **external production endpoint** (a durable log service / store),

chosen by the operator per service and per environment. The consumer is
**target-agnostic** — it never needs to know which of the three it's talking to;
it reads `{ url, token }` and connects. Behind the operator-served targets, each
service's socket is backed by a pluggable `LogStreamer`: in development an
**ephemeral follow proxy** (no persistence); in production a **pluggable
backend** (a stub here) reading from a durable store. This reuses the existing
edge/ingress *pattern* — per-service, route-token gated, advertised in the
service descriptor.

This proposal also fixes a latent bug it depends on: the runtime backends
**buffer** follow output today, so live log streaming does not actually work
(see Problem). The streaming-follow primitive added here is the prerequisite,
and it repairs the existing `onServiceLogs` / `onWorkspaceLogs` subscriptions as
a side effect.

## Problem

The console needs live, per-service logs with low latency and clean attribution,
and Angee wants the *option* to back that with a durable production store later
without changing the frontend contract. Three gaps stand in the way:

1. **Follow logs don't stream.** Both runtime backends shell out and read via
   `cmd.Run()` / `CombinedOutput` — `compose.Backend.Logs`
   (`internal/runtime/compose/backend.go:107`) and `proccompose.Backend.Logs`
   (`internal/runtime/proccompose/backend.go:110`). With `--follow` the child
   process never exits on its own, so the call **blocks until teardown and then
   delivers one buffered blob** as a single channel element. The HTTP sink
   (`writeLogStream`, `internal/operator/operator.go:822`) doesn't flush either.
   Consequently the `onServiceLogs` / `onWorkspaceLogs` GraphQL subscriptions
   (`internal/operator/gql/events.go`, `schema.graphql:350-351`) — which call
   `StackLogs(ctx, …, follow=true)` — emit nothing until the stream is torn
   down. Live streaming is effectively broken.

2. **No per-service delivery surface for the frontend.** The only per-service
   path is `GET /services/{name}/logs` (`internal/operator/operator.go`), a
   buffered, auth-wrapped, non-streaming read. There is **no** per-service
   socket the console can open and no proxy surface in the operator today.

3. **No dev/prod seam.** There is no abstraction that lets the same socket be
   served by an ephemeral live proxy in dev and a durable store in production.

## Current behavior

- `api.ServiceEndpoint` (`api/types.go:269`) carries `{ Routed, URL,
  InternalHost, InternalPort }`; `serviceEndpoint(name)` builds the URL via
  `routeURL` (`internal/service/ingress.go:86`), whose scheme/port already
  respect ingress `routing` (host|path) and `tls` (auto|off).
- Route tokens already exist: `MintRoute(actor, service)` mints a JWT with
  `aud=svc:<service>` (`internal/operator/tokens.go:87`,
  `serviceAudience` `:34`), and `tokens.Verify(raw, serviceAudience(name))`
  (`:158`) validates one against a specific service. `/edge/verify`
  (`internal/operator/edge.go:11`) uses exactly this to gate Caddy
  `forward_auth`, extracting the token from `?token=` /
  `Sec-WebSocket-Protocol` / `Authorization` (`edge.go:29`). `Claims` already
  has an optional `Scope []string` (`tokens.go:46`).
- The graphql-ws transport (`internal/operator/graphql.go:48`) already shows the
  pattern for an authenticated WebSocket: `gorilla/websocket` upgrade with
  `checkWebSocketOrigin` (`operator.go:877`) and a token check in the
  `connection_init` handshake (`graphql.go:106`).
- `flushWriter` (`operator.go:847`) already exists for chunked streaming over
  HTTP.

So every primitive the design needs — per-service tokens, a verified WS upgrade,
a flushing writer, the service descriptor — already exists. What is missing is a
streaming log source, a per-service socket, and the dev/prod seam.

## Proposal

Five layers, bottom-up. Each lands in an identified place.

### 1. Streaming-follow primitive (prerequisite)

Add a line-streaming follow path to the runtime backends: attach
`StdoutPipe`/`StderrPipe`, scan with `bufio.Scanner`, and emit **one channel
element per line** as produced, closing on process exit or ctx cancel — modeled
on the existing streaming `runForeground` (`internal/runtime/compose/backend.go:166`).
Expose it as a dedicated `Backend.StreamLogs(ctx, LogsRequest) (<-chan string,
error)` so the bounded-query `Logs` path (used by the `stackLogs`/`serviceLogs`
queries) stays byte-for-byte unchanged. This single change makes follow-mode
actually stream and repairs `onServiceLogs` / `onWorkspaceLogs`.

### 2. Platform: a per-service structured stream

```go
func (p *Platform) StreamServiceLogs(ctx context.Context, service string) (<-chan api.LogLine, error)
```

Picks the backend by the service's runtime (`container` → compose, `local` →
process-compose), runs `StreamLogs` for that **one** service, and wraps each raw
line into a `LogLine`. Because each call is scoped to a single known service,
**attribution is trivial** — no prefix-parsing, no aggregate fan-in:

```go
type LogLine struct {
    Service string  `json:"service"`           // known from the call
    Runtime string  `json:"runtime"`            // "container" | "local"
    Message string  `json:"message"`            // raw line; ANSI preserved unless stripped
    Level   *string `json:"level,omitempty"`    // best-effort inferred; null when unknown
    Ts      *string `json:"ts,omitempty"`       // optional timestamp
}
```

`Level` is honest best-effort (regex over common patterns) and documented as
*inferred*, not authoritative — neither docker compose nor process-compose
supplies a per-line app severity over the live path.

### 3. The `LogStreamer` seam (dev vs prod routing)

```go
type LogStreamer interface {
    StreamService(ctx context.Context, service string) (<-chan api.LogLine, error)
}
```

- **`ephemeralStreamer` (dev):** wraps `Platform.StreamServiceLogs`. No
  persistence; the upstream `--follow` lives only while a client is connected.
- **`prodStreamer` (stub):** returns `errLogBackendNotConfigured`. Documented
  contract: tail a durable store per service (e.g. VictoriaLogs
  `/select/logsql/tail` filtered by service, or an OTLP-fed store — see the
  production track below).

The operator selects the backend by config (`logs.backend: ephemeral | <ref>`,
default `ephemeral`). The frontend contract is identical either way.

### 4. Transport: a per-service WebSocket

A new operator route `GET /services/{name}/logs/stream` (registered on the
existing mux, `internal/operator/operator.go:115`), upgrading to a WebSocket
that emits JSON `LogLine` frames. It reuses `checkWebSocketOrigin`
(`operator.go:877`) and the same token extraction as `/edge/verify`
(`edge.go:29`). An optional `?color=false` strips ANSI from `Message`; the
default preserves it for terminal-style rendering. Client disconnect cancels the
context, which tears down the upstream follow process.

### 5. Service-info descriptor + minted credential

Extend `api.ServiceEndpoint` with a descriptor the frontend uses verbatim — the
single place where the operator hands back the resolved endpoint and its
credential:

```go
type LogStream struct {
    URL       string `json:"url"`         // resolved target: operator port | edge | production
    Target    string `json:"target"`      // "operator" | "edge" | "production" (informational)
    Protocol  string `json:"protocol"`    // "ws"
    Token     string `json:"token"`       // credential matching the target, minted on this read
    ExpiresAt string `json:"expires_at"`
}
```

When answering `serviceEndpoint(name)`, the operator resolves the target for that
service and environment and returns the matching `{ url, token }`:

- **operator** (dev ephemeral): `URL` is the operator's own
  `…/services/<name>/logs/stream`; `Token` is a route token (`aud=svc:<name>`,
  `scope:["logs:read"]`), verified in the WS handler with
  `tokens.Verify(raw, serviceAudience(name))` plus the admin-bearer /
  `aud=operator` tier.
- **edge**: `URL` is the service's Caddy route (`routeURL`, host/path + tls
  per ingress); the same route token gates it through `/edge/verify`. Use this
  when the consumer can reach the edge but not the operator directly.
- **production**: `URL` and `Token` come from the configured production log
  backend (its own endpoint + credential); the operator returns them opaquely.

`Target` is informational so the console can label the source; **connecting is
identical regardless** — read `{ url, token }`, open the socket. The descriptor
ships first over REST `GET /services/{name}/endpoint`, where the operator has
the request (host/scheme) and the minter; the scheme (`ws`/`wss`) is derived
from how the client reached the operator. A matching GraphQL `logStream` field
on the `ServiceEndpoint` type is a follow-up — it needs the request host and the
minter plumbed into the gql resolver (a configured external base URL), which the
REST path gets for free from the live request.

## Production track (context, not built here)

The durable side that `prodStreamer` would read from is a separate effort,
informed by prior-art research. All three runtimes converge on **file tailing**:
Docker `json-file`/`local` driver files, Kubernetes `/var/log/pods`, and
process-compose `log_location` files. The practical shape is a file-tailing
shipper (Fluent Bit / Vector / Grafana Alloy — Promtail is EOL ~March 2026)
feeding a permissively-licensed store. **VictoriaLogs** (Apache-2.0 for both
single-node and cluster, single zero-config binary, ingests from essentially
every collector) is the low-friction default; emitting **OTLP** as the internal
contract keeps the collector and store swappable. Two prerequisites for Angee on
that track (out of scope here, noted for the stub's contract):

- process-compose persists logs **only if `log_location` is set** and emits
  structured JSON by default (`level`/`process`/`replica`/`message`); Angee
  should inject `log_location` + `log_configuration` into the generated
  `process-compose.yaml`.
- the store's per-service query/tail API is what `prodStreamer.StreamService`
  calls.

## Design options

### Token scope — narrow vs. reuse

Recommended: mint `aud=svc:<name>` **with `scope:["logs:read"]`**, so a leaked
log token can't drive the service's other capabilities. Reusing the bare
`aud=svc:<name>` route token also works and is simpler; the scope check is a
cheap, additive refinement using the existing `Claims.Scope` field.

### Endpoint target — how the operator picks

The operator resolves one of three targets per service/environment and returns
it in `LogStream`; the choice is the operator's, the consumer is agnostic.

- **operator** (default for dev): the consumer reaches the operator directly.
  Lowest-latency, no extra hop. The descriptor URL is derived from the request
  host or a configured external base URL.
- **edge**: the consumer can reach the Caddy edge but not the operator directly
  (typical of an externally-exposed stack). Caddy `forward_auth` is a guard, not
  a tunnel, so this needs a **reserved log route** on the edge that
  reverse-proxies the operator's log socket and is gated by the route token via
  `/edge/verify` — consistent with the existing per-service edge routes (and the
  edge can already reach the operator, since it is the `verify:` upstream).
- **production**: a configured durable backend owns both the endpoint and the
  credential; the operator passes them through opaquely.

The seam means a deployment can move from operator-direct to edge to a
production store without any frontend change — only what the operator returns in
`LogStream` changes.

### Fan-out — shared upstream vs. per-client

Recommended: **one upstream `--follow` per service, shared across watchers** via
a small per-service broker started lazily and torn down when the last subscriber
leaves (mirroring `pollWorkspaceStatus`'s lazy lifecycle), with a generous
buffer. Logs are high-volume and a broker drops on slow subscribers, so the
buffer must be generous; the simpler fallback is one upstream process per client.

### Transport — WS now, SSE later

WebSocket only for now (matches the graphql-ws transport and the per-service
socket model the frontend wants). An SSE variant for curl / server-side
consumers can be added later, mirroring the graphql SSE+WS pairing.

## Security

- For the operator and edge targets the socket rides the same token machinery as
  the edge: a per-service route token (`aud=svc:<name>`, optionally
  `scope:["logs:read"]`), or the admin bearer / `aud=operator` tier. No new
  audience family is introduced. For the production target the credential is
  whatever that backend issues; the operator returns it opaquely and never
  forges or stores it beyond the descriptor it just minted/relayed.
- Tokens embedded in the service descriptor are short-lived (route-token TTL,
  default 1h) and minted per read.
- The WS upgrade enforces the same `Origin` allowlist as graphql-ws
  (`checkWebSocketOrigin`), so a browser on a disallowed origin is rejected at
  the handshake.
- Logs may contain sensitive runtime output; access is gated per service, and
  the socket never exposes secret *values* (it streams process stdout/stderr,
  the same content `GET /services/{name}/logs` already returns to an authorized
  caller).

## Backward compatibility

Additive. The streaming-follow primitive is a new backend method; the bounded
`Logs` query path is unchanged, so the `stackLogs`/`serviceLogs` queries behave
identically. `onServiceLogs` / `onWorkspaceLogs` keep their schema but begin to
actually stream (a bug fix, not a contract change). The new WS route, the
`LogStreamer` seam, and the `logStream` descriptor field are all new surface.

## Out of scope

- **The production store and shipper.** This proposal defines the `prodStreamer`
  interface and stub only; provisioning VictoriaLogs/OTLP and the
  `log_location` injection are a separate effort (see the production track).
- **Aggregate / whole-stack streaming.** The per-service socket is the unit; an
  aggregate view is a frontend composition over several sockets, or a later
  addition.
- **`angee dev` foreground unification.** `dev` keeps its native attached TTY
  output for now; sharing the streaming primitive with `dev` is a follow-up.

## Acceptance

- A client opening `wss://…/services/<name>/logs/stream` with a valid
  `aud=svc:<name>` (`logs:read`) token receives `LogLine` frames **incrementally
  and live**, and the socket closes (tearing down the upstream follow) on client
  disconnect or ctx cancel.
- An invalid/missing/cross-service token is rejected at the handshake; a
  disallowed `Origin` is rejected.
- `serviceEndpoint(name)` returns a `logStream` descriptor with a working URL, a
  `target` of `operator` | `edge` | `production`, and a freshly-minted (or
  relayed) unexpired token; the consumer connects identically regardless of
  target, and changing the target changes only the descriptor, not the frontend.
- With `logs.backend: ephemeral` (default), no logs are persisted; switching to a
  configured prod backend swaps the source with no frontend change. The stub
  backend returns a clear "not configured" error.
- The streaming-follow primitive makes `onServiceLogs` emit lines live (verified
  against a running stack), confirming the prerequisite fix.
- A backend test drives the streaming follow with a fake runner emitting paced
  lines and asserts incremental delivery + teardown on cancel.

## See also

- [`docs/proposals/edge-ingress-caddy.md`](edge-ingress-caddy.md) — the
  per-service route-token + `/edge/verify` pattern this reuses.
- [`docs/proposals/graphql-websocket-transport.md`](graphql-websocket-transport.md)
  — the authenticated WebSocket upgrade pattern (`InitFunc`, origin check) the
  log socket mirrors.
- [`docs/proposals/stack-snapshot-subscription.md`](stack-snapshot-subscription.md)
  — the aggregate snapshot subscription; logs are deliberately *not* folded into
  it.
- `internal/runtime/compose/backend.go` / `internal/runtime/proccompose/backend.go`
  — the buffered `Logs` and streaming `runForeground` the primitive bridges.
- `internal/operator/tokens.go`, `internal/operator/edge.go` — the token mint /
  verify / extraction the socket auth reuses.
- `internal/service/ingress.go` — `routeURL`, the scheme/port logic the
  descriptor URL reuses.
