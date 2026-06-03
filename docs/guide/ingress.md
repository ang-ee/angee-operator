# Edge Ingress & Scoped Tokens

Angee can front your services with a single authenticating **edge** instead of
publishing a host port per service. This is opt-in: `ingress` defaults to `none`
(every service publishes its own host ports, exactly as before), and turning it
on changes nothing for services that don't declare a `route:`.

- **Config reference:** [`ingress` and `route`](./manifest.md#ingress)
- **API reference:** [tokens, `/edge/verify`, `serviceEndpoint`](../reference/operator-api.md#connection-and-route-tokens)
- **Design:** [`docs/proposals/edge-ingress-caddy.md`](../proposals/edge-ingress-caddy.md)

## Why

Without ingress, every reachable service leases a host port, and anything that
needs authentication ships its own reverse-proxy + token-verifier sidecar with a
per-service secret. Adding a service means leasing a port and shipping a sidecar.

With `ingress.type: caddy`, the operator compiles **one** Caddy edge
(`caddy-docker-proxy`) into the stack. Routed services join a private network,
**drop their host ports**, and are reached only through the edge — which
authenticates every inbound connection (HTTP and WebSocket) centrally. *N*
sidecars collapse into one edge; per-service secrets collapse into one
operator-owned signing key.

## How it works

```
browser ──{ url, token }── host backend (holds the admin bearer, server-side)
   │                          │  mints a short-lived, scoped token via the operator
   │                          ▼
   │                   operator: mintConnectionToken / mintRouteToken
   └─(wss/https + ?token=)─► Caddy edge ──forward_auth──► operator GET /edge/verify
                              │                              (verifies aud + signature)
                              └─(2xx)─► routed service (private network, no host port)
```

1. **Compile.** When `ingress.type: caddy`, `Compile` injects the edge service
   (the only one publishing a host port — `:443`/`:80`), adds a private
   `<stack>_edge` network, and for each service with a `route:` block: removes
   its host ports, joins it to the edge network, and stamps Caddy router +
   `forward_auth` labels. Services without a `route:` are untouched.
2. **Mint.** A host backend (e.g. `angee-django`) holds the operator admin
   bearer **server-side**, authorizes the actor, and asks the operator to mint a
   short-lived, scoped token. The browser never sees the admin bearer.
3. **Verify.** The edge calls the operator's `GET /edge/verify` (forward_auth)
   on every request; the operator validates the token's signature, expiry, and
   audience and answers `200`/`401`. The operator owns the signing key, so the
   workload never participates in its own authentication.

## Enabling it

In the stack manifest, select the backend and a base domain:

```yaml
ingress:
  type: caddy
  domain: agents.localhost   # base domain; defaults to operator.domain
```

Opt a service into routing with a `route:` block instead of host `ports:`:

```yaml
services:
  agent:
    runtime: container          # routing is container-only
    image: angee/agent:latest
    command: ["stdio-to-ws", "--port", "3008", "--", "claude-code", "acp"]
    route:
      port: 3008               # container port the edge proxies to
      host: agent.agents.localhost   # default: <service>.<ingress.domain>
      # auth: forward           # forward (default) → /edge/verify · none → no auth
```

That service now has **no** host port and takes **no** `operator.port_pool`
lease — it is reachable only at `wss://agent.agents.localhost/` through the edge.
See the [manifest reference](./manifest.md#ingress) for every field.

## The token model

The operator mints two kinds of short-lived HS256 JWT (both returned as
`{token, actor, expiresAt}`), differing only in audience and scope:

| Token | Audience | Mint (GraphQL / REST) | Use |
|---|---|---|---|
| Connection | `operator` | `mintConnectionToken(actor, scope, ttl)` / `POST /tokens/mint` | Operator-API access (scoped); handed to the browser instead of the admin bearer |
| Route | `svc:<service>` | `mintRouteToken(actor, service, ttl)` / `POST /tokens/route` | Opening one service's socket through the edge |

A connection token can't be replayed against a service's edge route and a route
token can't be replayed against the operator API — the audiences are enforced by
the same verifier that `/edge/verify` and the operator's `auth` middleware share.
Minting is gated by the admin bearer, so only a server-side caller can mint.
Full details — signing-key resolution, scope mapping — are in the
[operator API reference](../reference/operator-api.md#connection-and-route-tokens).

## WebSocket subscriptions

Two WebSocket paths use the same token model:

- **Operator GraphQL subscriptions** over `graphql-transport-ws` on
  `GET /graphql` — the browser presents a minted `aud=operator` token in the
  graphql-ws `connectionParams`. See
  [Subscriptions › WebSocket transport](../reference/operator-api.md#websocket-transport).
- **Routed WebSocket services** through the edge — the browser opens
  `wss://<service>.<domain>/?token=<route-token>`; the edge's `forward_auth`
  reads the token (it arrives in `X-Forwarded-Uri`) and gates the upgrade via
  `/edge/verify`. A valid token completes the `101 Switching Protocols`
  handshake; an invalid one is rejected with `401`.

## Dev setup

In development, `ingress.domain: agents.localhost` plus Caddy's automatic local
TLS gives you `wss://<service>.agents.localhost/` with no host-port juggling.
Mint a route token (over the admin bearer) and connect with `?token=…`.

## Operational notes

- **WebSocket survivability across reloads.** Every container start/stop
  reconciles `caddy-docker-proxy`, which reloads Caddy and severs active
  WebSockets. Use short connection-token TTLs (~60 s) and client
  auto-reconnect, and debounce bursts of container events.
- **Tokens in the query string** are not written to operator logs (the operator
  does not log request URIs); short TTLs remain defense-in-depth.
- **TLS** terminates at the edge (Caddy automatic HTTPS); backends stay plaintext
  on the private network.
- **The edge mounts the Docker socket** — a high-privilege grant inherent to
  `caddy-docker-proxy`. Keep the operator the sole owner of the `ingress.verify`
  name on a dedicated edge network.
- **`runtime: local` services can't be routed** (they don't join a Docker
  network); `route:` on a local service is rejected at validation.
- **Single edge = single chokepoint** — fine at this scale; note it for capacity.

## See also

- [Manifest reference › Ingress](./manifest.md#ingress)
- [Operator API › Connection and route tokens](../reference/operator-api.md#connection-and-route-tokens)
- [Operator API › Ingress (`/edge/verify`, `serviceEndpoint`, `ingressStatus`)](../reference/operator-api.md#ingress)
- [Proposal: edge ingress (Caddy) + minted scoped tokens](../proposals/edge-ingress-caddy.md)
