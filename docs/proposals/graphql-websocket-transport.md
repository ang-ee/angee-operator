# Proposal: graphql-ws WebSocket transport on `/graphql`

**Status:** Draft · **Area:** operator, graphql, transport, auth · **Surfaces:** GraphQL

## Summary

Register gqlgen's `transport.Websocket` on `/graphql` so browser clients can run
subscriptions over the standard `graphql-transport-ws` protocol, alongside the
existing SSE transport. Today the operator's only subscription transport is SSE
(`POST /graphql` with `Accept: text/event-stream`), which no off-the-shelf
browser GraphQL client speaks as a default — every web consumer has to hand-roll
an SSE-over-`fetch` subscription forwarder (frame parsing, `ReadableStream`
reader, abort teardown) just to receive `onServiceLogs` / `onGitOpsTopologyChange`
updates. A WebSocket transport lets those clients use their built-in graphql-ws
exchange unchanged. SSE stays for curl/REST parity and server-side consumers.

The WebSocket handshake authenticates with the **minted, scoped token** model
introduced in [`edge-ingress-caddy.md`](edge-ingress-caddy.md): the browser
presents a short-lived `aud: operator` token in the graphql-ws `connectionParams`,
validated by that proposal's shared verifier — so the browser never holds the
admin bearer over the socket. This proposal owns the *transport* (and how the
token reaches the daemon over a WebSocket); the token model, the verifier, and
the two-tier `s.auth` are owned by that sibling proposal. The two compose.

## Problem

The operator is increasingly driven from the browser (the web console subscribes
to logs, GitOps topology, and workspace status). The standard browser GraphQL
clients (urql, Apollo, graphql-ws) ship a WebSocket subscription transport out of
the box and treat it as the default; **none ship an SSE subscription transport by
default**. So every browser consumer of the operator's live updates must write
and maintain a bespoke SSE forwarder against `POST /graphql` +
`Accept: text/event-stream` — bespoke transport code for a protocol the daemon's
own GraphQL library can speak natively.

The asymmetry: the daemon already has a full `Subscription` root and a working
event hub; the *only* thing forcing custom client transport code is that the
single registered subscription transport is SSE. Adding the WebSocket transport
the GraphQL library already implements removes that custom code from every
browser client.

## Current behavior

- `newGraphQLHandler` (`internal/operator/graphql.go:27`) registers
  `transport.SSE{}`, `transport.POST{}`, `transport.GRAPHQL{}` on the gqlgen
  handler — **no `transport.Websocket{}`**.
- The handler is wrapped in an HTTP `http.HandlerFunc` that **hard-rejects any
  non-`POST` method** (`graphql.go:41`) and validates a JSON/`application/graphql`
  content type before delegating to `gqlServer.ServeHTTP`.
- It is mounted method-scoped:
  `mux.Handle("POST /graphql", s.auth(cop.Handler(s.graphqlHandler)))`
  (`internal/operator/operator.go:114`). Go's method-pattern routing means a WS
  upgrade (`GET /graphql` with `Upgrade: websocket`) does **not** match this route
  at all.
- `s.auth` (`operator.go:671`) authenticates by reading the `Authorization:
  Bearer <token>` **header** and constant-time-comparing it to the configured
  admin bearer. A browser cannot set request headers on a WebSocket, so a
  header-based gate cannot authenticate a WS upgrade.
- `cop` (`http.NewCrossOriginProtection`, `operator.go:109`) guards *unsafe*
  methods (the POST path) via `Sec-Fetch-Site`/Origin. A WS upgrade is a `GET`,
  which it treats as safe — so it provides **no** cross-site protection for a
  WebSocket route.

## What already exists (and is unused)

This proposal mostly *wires up* parts that are already built:

- **gqlgen's WebSocket transport** — `github.com/99designs/gqlgen/graphql/handler/transport.Websocket`
  implements the full `graphql-transport-ws` protocol (`connection_init`/`_ack`,
  `subscribe`/`next`/`complete`, `ping`/`pong`). It is part of the gqlgen version
  already in `go.mod` (`v0.17.90`); nothing new is generated.
- **`gorilla/websocket`** — already in the module graph (`go.mod`, currently
  `// indirect`). Registering the transport promotes it to a direct dependency
  (`go mod tidy`); no new third-party code enters the tree.
- **The `Subscription` root + event hub** — `onGitOpsTopologyChange`,
  `onWorkspaceStatusChange`, `onServiceLogs`, `onWorkspaceLogs` already resolve
  through `opgql.NewEventHub(platform)` (`operator.go`) and stream today over SSE.
  The transport is the only thing changing; the resolvers are transport-agnostic.
- **The connection-token minter + the verifier proposal.**
  `internal/operator/tokens.go` already signs short-lived HS256 JWTs scoped to an
  actor (`sub=<actor>`, `iss=angee-operator`, TTL ≤ 24h) via `mintConnectionToken`.
  No request path verifies them *today* (`s.auth` only accepts the admin bearer),
  but [`edge-ingress-caddy.md`](edge-ingress-caddy.md) already proposes the missing
  half: extend the minter with `aud`/`scope`, add a shared
  `verifyToken(raw, wantAudience)`, and make `s.auth` a two-tier check (admin
  bearer **or** a minted `aud: operator` token). **The WS `InitFunc` is simply
  another caller of that same verifier** — it does not introduce a second auth
  mechanism.

## Proposal

Add the WebSocket transport as an **additive** second subscription transport.
Four focused changes, all in `internal/operator`:

1. **Register the transport.** `gqlServer.AddTransport(transport.Websocket{...})`
   in `newGraphQLHandler`, configured with an `InitFunc` (auth), an `Upgrader`
   with an origin allowlist (CSWSH protection), and a keepalive ping interval.
2. **Route the upgrade.** Add `mux.Handle("GET /graphql", s.graphqlWS)` for the
   WS handshake, distinct from the existing `POST /graphql`. The GET route is
   **not** wrapped in `s.auth` (the upgrade carries no `Authorization` header);
   authentication happens in the transport `InitFunc` instead.
3. **Don't let the POST-only wrapper eat the upgrade.** The
   `if r.Method != http.MethodPost` guard and JSON content-type check in
   `newGraphQLHandler` exist to bound the POST body and reject odd POST bodies —
   neither applies to a WS upgrade. Split those POST-only concerns from the bare
   `gqlServer.ServeHTTP` so the GET upgrade reaches the gqlgen handler unimpeded
   while POST keeps its body cap and content-type validation.
4. **Authenticate in `InitFunc` via the shared verifier.** The graphql-ws
   `connection_init` payload carries the token (the browser can't set a WS
   `Authorization` header); gqlgen exposes it as `transport.InitPayload`. Read it
   with `initPayload.Authorization()` and run the **same** check `s.auth` runs —
   the two-tier `admin-bearer-or-verifyToken(raw, "operator")` from
   [`edge-ingress-caddy.md`](edge-ingress-caddy.md) — putting the resolved
   actor/scope on the connection context. Reject with a typed error on failure;
   gqlgen closes the socket with the protocol's `4401`/`4403` close code.

SSE, POST, and the admin-bearer header gate on `POST /graphql` are **unchanged**.
Existing curl/REST and server-side subscribers keep working exactly as today.

## Design options

### Authentication over the WebSocket

The credential model is **not** this proposal's to define —
[`edge-ingress-caddy.md`](edge-ingress-caddy.md) owns it (the `aud`/`scope` token,
the shared `verifyToken`, and the two-tier `s.auth`). This proposal owns only the
WS-specific question: *how the token reaches the daemon when the client is a
browser WebSocket.* The answer: the browser cannot set an `Authorization` header
on a WS upgrade, so the token rides the graphql-ws `connection_init` `payload`
(`connectionParams` on the client), and `InitFunc` runs the same two-tier check
`s.auth` runs over HTTP:

- **Minted `aud: operator` token (the browser path).** The host backend
  authz-checks the actor and `mintConnectionToken(actor, scope, ttl)`s a
  short-lived, capability-scoped token server-side (over the admin bearer, which
  never leaves the server); the browser presents *that* in `connectionParams`.
  `InitFunc` calls `verifyToken(raw, "operator")`, enforces scope on subscribe,
  and binds the `sub` actor to the connection. This is the model the operator
  console uses — the browser never holds the admin bearer over the socket.
- **Admin bearer (the server-to-server path).** A server-side caller may present
  the admin bearer directly; `InitFunc` accepts it at full access via the same
  constant-time compare `s.auth` uses — parity with the HTTP full-access tier.

**Sequencing.** If this transport lands *before* the `edge-ingress-caddy.md` token
work, `InitFunc` ships with only the admin-bearer compare (a ~5-line share with
today's `s.auth`) and gains scoped minted-token acceptance for free the moment the
shared `verifyToken` lands — the WS path adds no second verifier either way.

### Origin protection (CSWSH)

Because `cop` does not guard the GET upgrade, cross-site WebSocket hijacking
protection must live on `transport.Websocket{Upgrader: websocket.Upgrader{
CheckOrigin: ...}}`. Enforce an `Origin` allowlist (the console's origin(s));
default to permitting loopback origins for local dev, configurable for
non-loopback binds. This is a **security requirement**, not optional — without a
`CheckOrigin`, gorilla's default permits all origins, so any web page the user
visits could open a socket to a loopback operator. Pair it with the existing
"non-loopback binds require `--token`" invariant (`operator.go:81`).

## Security

- **No token in the URL.** The token travels in the `connection_init` payload
  (post-upgrade application frame), never as a query parameter — query strings
  leak into logs and proxies.
- **Origin allowlist on the upgrader** (above) is mandatory; loopback-friendly by
  default, explicit allowlist for non-loopback.
- **Same auth strength as POST.** `InitFunc` runs the identical two-tier check as
  `s.auth` (constant-time admin-bearer compare, or `verifyToken` signature+`exp`+
  `aud` validation). One verifier, two transports — the WS path neither weakens nor
  forks the gate.
- **Browser holds no root credential.** The browser presents a short-lived,
  scoped `aud: operator` token; the admin bearer stays server-side in the host
  backend. A leaked socket token expires and is scope-bounded.
- **Keepalive.** Configure graphql-ws `ping`/`pong` (`KeepAlivePingInterval`) so
  idle sockets survive intermediary timeouts and dead peers are reaped.

## Backward compatibility

Purely additive. SSE (`transport.SSE`), POST queries, and the admin-bearer header
gate on `POST /graphql` are untouched; existing curl examples in
`docs/reference/operator-api.md § Subscriptions` keep working. The only new
surface is `GET /graphql` answering WebSocket upgrades. Browser clients opt in by
pointing their graphql-ws transport at the same `/graphql` URL; nothing forces a
client off SSE.

## Out of scope

- **Removing SSE.** It remains the right transport for curl, REST-parity tooling,
  and any consumer that prefers a plain HTTP stream. This proposal adds a
  transport; it does not retire one.
- **Subscription semantics.** The "no initial snapshot on connect", 2 s
  hash-polling for snapshot subscriptions, and slow-subscriber drop behavior
  (`docs/reference/operator-api.md`) are unchanged — they are resolver/event-hub
  concerns, independent of transport.
- **The token model itself** — `aud`/`scope`, the shared `verifyToken`, the
  two-tier `s.auth`, and the host-backend minting rewrite are owned by
  [`edge-ingress-caddy.md`](edge-ingress-caddy.md). This proposal consumes that
  verifier from `InitFunc`; it does not redefine it.

## Acceptance

- A graphql-ws client (e.g. the `graphql-ws` library over `ws://…/graphql`) that
  presents a minted `aud: operator` token in `connectionParams` receives
  `connection_ack` and then `next` frames for `onServiceLogs` /
  `onGitOpsTopologyChange` / `onWorkspaceStatusChange` / `onWorkspaceLogs`; the
  admin bearer is also accepted (server-to-server full access).
- A WS upgrade with a missing/expired/wrong-`aud` token is rejected (connection
  closed with the graphql-ws unauthorized close code); a token from a disallowed
  `Origin` is rejected at the upgrade.
- `POST /graphql` queries, mutations, and the SSE subscription path behave exactly
  as before (regression-covered by the existing SSE subscription tests).
- Cancelling a WS subscription tears down the underlying follow stream (no leaked
  `logs --follow` processes), matching the SSE teardown guarantee.
- `docs/reference/operator-api.md § Subscriptions` documents the WebSocket
  transport alongside SSE, including the `connectionParams` auth shape.

## See also

- [`docs/proposals/edge-ingress-caddy.md`](edge-ingress-caddy.md) — owns the
  minted-scoped-token model, the shared `verifyToken`, and the two-tier `s.auth`
  this transport's `InitFunc` consumes.
- [`internal/operator/graphql.go`](../../internal/operator/graphql.go) —
  `newGraphQLHandler`, where the transport is registered.
- [`internal/operator/operator.go`](../../internal/operator/operator.go) — the
  `/graphql` route mount and the `auth` middleware.
- [`internal/operator/tokens.go`](../../internal/operator/tokens.go) — the minter
  the verifier pairs with.
