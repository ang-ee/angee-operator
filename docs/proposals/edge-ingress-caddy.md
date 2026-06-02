# Proposal: `ingress` — an optional edge backend (Caddy) + minted, scoped operator tokens

**Status:** Draft · **Area:** manifest, runtime backends, operator auth, operator API · **Surfaces:** manifest + compile + operator GraphQL/REST

## Summary

Add an `ingress:` backend to the stack manifest, selected by `type` and
defaulting to `none` — the same shape as `secrets_backend`
(`env-file`/`openbao`, see [`internal/secrets/backend.go`](../../internal/secrets/backend.go))
and the runtime backends (compose/proccompose). When `ingress.type: caddy`,
the operator compiles a **central Caddy edge** into the compose file, puts
routed services on a private network with **no host-published ports**, and
authenticates every inbound connection — HTTP and WebSocket — with a
**short-lived, scoped token** validated at the edge.

The same token mechanism is then applied to **the operator's own API**: instead
of handing the browser the real admin bearer (`ANGEE_OPERATOR_TOKEN`), the host
backend (Django) holds that bearer **server-side only** and uses it to **mint
short-lived, capability-scoped tokens** for the browser. One verifier — the
operator's — serves both the edge (routed services) and the operator API.

Today every service that needs reachability leases a host port, and (for agents)
ships its own Caddy + token-verifier sidecar; and every authorized browser
receives the same long-lived operator admin token. This proposal collapses the
*N* sidecars into one edge, and collapses the shared admin token into per-actor,
short-lived, scoped tokens — moving transport-auth into the layer that already
owns ports, networks, and token minting.

## Motivation

Two problems, one mechanism:

1. **Auth is co-located with each workload.** Each agent service publishes a host
   port and runs its own Caddy + `verify-acp-token.mjs` + a per-agent HMAC
   secret staged into the container. Adding a service means leasing a port and
   shipping a sidecar; the workload participates in its own authentication.
2. **The operator admin token is handed to the browser.** The host backend exposes
   `operatorConnection { endpoint, token }` where `token` is the single
   `ANGEE_OPERATOR_TOKEN`. Every authorized browser gets the same long-lived
   root credential, and the operator API is all-or-nothing
   ([`operator.go:671`](../../internal/operator/operator.go) compares the bearer
   against one configured token — no actor, no scope).

Both are the same anti-pattern: a long-lived secret distributed to the edge, and
auth enforced at the wrong layer. The fix is one mechanism — **the operator mints
short-lived scoped tokens and verifies them centrally** — applied to two
upstreams (routed services and the operator API).

## Ownership split (the load-bearing decision)

- **Operator = mechanism.** Owns the edge service, the private network, the route
  table, port allocation, and **both minting and verifying** tokens. It already
  owns ports/network/process and already mints actor-scoped JWTs
  ([`tokens.go`](../../internal/operator/tokens.go)).
- **Host backend (Django) = policy.** Owns authorization (REBAC). It holds the
  operator admin bearer **server-side**, decides *whether* to mint (after an authz
  check), and hands the browser `{ url, token }`. It never touches Caddy config
  and never ships the admin bearer to the browser.

This is the deliberate answer to "should the host backend manage Caddy?" — **no.
The operator does; the host only asks for a token.**

## Part 1 — `ingress` backend

### Manifest additions

```go
// internal/manifest/manifest.go
type Stack struct {
    ...
    Ingress Ingress `yaml:"ingress,omitempty" json:"ingress,omitempty"`
}

type Ingress struct {
    // "none" (default — today's host-published-ports behavior) | "caddy"
    Type    string `yaml:"type,omitempty" validate:"omitempty,oneof=none caddy"`
    Domain  string `yaml:"domain,omitempty"`  // base domain; defaults to operator.domain
    Image   string `yaml:"image,omitempty"`   // default: lucaslorentz/caddy-docker-proxy:2.9
    Network string `yaml:"network,omitempty"` // default: "<name>_edge"
    Verify  string `yaml:"verify,omitempty"`  // forward_auth target; default: the operator's /edge/verify
}

// A service opts into routing instead of publishing host ports:
type Service struct {
    ...
    Route *Route `yaml:"route,omitempty" json:"route,omitempty"`
}

type Route struct {
    Port int    `yaml:"port"`           // container port to proxy to (e.g. 3008)
    Host string `yaml:"host,omitempty"` // default "<service>.<ingress.domain>"
    Path string `yaml:"path,omitempty"` // alternative: path-prefix routing
    Auth string `yaml:"auth,omitempty"` // "forward" (default) | "none"
}
```

`Stack.Defaults()` sets `Ingress.Type = "none"` when empty, mirroring
`SecretsBackend.Type = "env-file"`. Existing manifests are byte-stable; nothing
changes until a stack opts in.

### The edge backend interface

Parallel to [`runtime.Backend`](../../internal/runtime/backend.go) and
[`secrets.Backend`](../../internal/secrets/backend.go):

```go
// internal/runtime/edge/backend.go
type Backend interface {
    // Contribute mutates the compiled compose: inject the edge service + the
    // network, and for each routed service drop host ports, join the edge
    // network, and stamp router labels. Pure compile-time — no runtime calls.
    Contribute(stack *manifest.Stack, compiled *compose.File) error
}

func FromManifest(cfg manifest.Ingress) (Backend, error) {
    switch cfg.Type {
    case "", "none": return NoneBackend{}, nil      // today's behavior
    case "caddy":    return NewCaddyBackend(cfg), nil
    default:         return nil, fmt.Errorf("unsupported ingress backend %q", cfg.Type)
    }
}
```

[`Compile()`](../../internal/service/platform.go) (`platform.go:213`) calls
`edge.FromManifest(stack.Ingress).Contribute(stack, &compiled.Compose)` after
services are built. The `none` backend is a no-op, so the change is inert for
current stacks.

### Compile changes (concrete)

[`compose.Service` / `compose.File`](../../internal/runtime/compose/doc.go) gain
the two fields they lack today:

```go
type File struct {
    Name     string
    Services map[string]Service
    Volumes  map[string]Volume
    Networks map[string]Network `yaml:"networks,omitempty"`  // NEW
}
type Service struct {
    ...
    Networks []string          `yaml:"networks,omitempty"`  // NEW
    Labels   map[string]string `yaml:"labels,omitempty"`    // NEW
}
```

The `caddy` backend's `Contribute`:

1. Adds `networks: { <name>_edge: {} }`.
2. Injects the edge service: `caddy-docker-proxy`, docker socket mounted
   read-only, the **only** published port (`443`/`80`), joined to `<name>_edge`,
   with a global `forward_auth` snippet pointing at `ingress.verify`.
3. For each service carrying `route:` — **delete its `Ports`**, append
   `<name>_edge` to `Networks`, and stamp Caddy labels:

```yaml
labels:
  caddy: "{{ host }}"
  caddy.reverse_proxy: "{{ upstreams 3008 }}"
  caddy.reverse_proxy.flush_interval: "-1"     # keep idle WebSockets alive
  caddy.import: "forward_auth_edge {{ name }}"  # snippet → /edge/verify?service=<name>
```

caddy-docker-proxy watches Docker and regenerates config with zero-downtime
reloads as containers start/stop — so "dynamic routes via API" is achieved
**without the operator running any reconcile loop**. The route table is a
function of the running compose, which the operator already owns.

Secondary win: routed services no longer lease from `operator.port_pool`; only
the edge holds a published port.

## Part 2 — minted, scoped tokens (shared by edge + operator API)

### Token model

Extend the existing [`tokenMinter`](../../internal/operator/tokens.go) to carry an
audience and a scope. The signing key is unchanged (explicit secret, else derived
from the admin bearer via `deriveJWTSecret` — already symmetric, so the verifier
derives the same key).

```go
type Claims struct {
    jwt.RegisteredClaims        // sub=actor, iss=angee-operator, exp, iat
    Audience string   `json:"aud"`   // "operator" | "svc:<service-name>"
    Scope    []string `json:"scope,omitempty"` // capability set for operator-API tokens
}
```

- **Route token** — `aud = "svc:<service>"`, no scope. Authorizes opening that
  one service's socket through the edge.
- **Operator-API token** — `aud = "operator"`, `scope` = the capability set the
  host backend's authz layer approved (e.g. `["service:read","service:up",
  "workspace:create"]`).

### Verifier (one, shared)

A single `verifyToken(raw, wantAudience) (Claims, error)` validates signature +
`exp` + `aud`. It is used by:

- the **edge** `GET /edge/verify?service=<name>` forward_auth target — reads the
  token from `X-Forwarded-Uri` (`?token=…`, since browser WebSocket can't set
  headers) / `Authorization` / `Sec-WebSocket-Protocol`; requires
  `aud == "svc:<name>"`. This is `verify-acp-token.mjs` promoted to one
  operator-owned endpoint.
- the **operator API** auth middleware (`operator.go:671`) — see below.

### Operator API auth: accept admin bearer OR a scoped minted token

Today the middleware is admin-token-or-nothing. Extend it to a two-tier check:

```go
func (s *Server) auth(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        raw, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
        switch {
        case s.config.Token == "":
            next.ServeHTTP(w, r)                       // unauthenticated dev
        case constantTimeEqual(raw, s.config.Token):
            next.ServeHTTP(w, r)                       // full access (server-to-server)
        default:
            claims, err := s.tokens.Verify(raw, "operator")
            if err != nil { unauthorized(w); return }
            ctx := withActorScope(r.Context(), claims) // enforce per-field scope downstream
            next.ServeHTTP(w, r.WithContext(ctx))
        }
    })
}
```

A field→scope map gates mutations for minted tokens (the admin bearer bypasses
scope — it is the full-access server-to-server path the host backend uses to mint
in the first place). This gives the operator API a real capability model it lacks
today, additively: existing admin-bearer callers are unchanged.

### Host-backend (Django) connection rewrite

`operatorConnection` stops returning the admin bearer. Instead:

- The admin bearer stays in Django settings (`ANGEE_OPERATOR_TOKEN`),
  **server-side only**.
- On connect, Django authz-checks the actor, then calls
  `mintConnectionToken(actor, scope, ttl)` over the admin bearer and returns the
  **minted, scoped** token + endpoint to the browser.
- For chat, `agentAcpEndpoint` authz-checks, calls
  `mintRouteToken(actor, service, ttl=60s)`, and returns the public
  `serviceEndpoint` URL + route token. The per-agent `acp_auth_secret`, the
  `secretSet` of `acp-auth-secret`, and the compose-port-scraping all disappear.

The browser therefore never holds a long-lived or full-access operator
credential — only short-lived tokens scoped to exactly what the actor was
approved for.

## API the operator gains

```graphql
type Mutation {
  # actor is authz-approved by the caller (host backend) before minting.
  # Extends the existing mintConnectionToken with audience + scope.
  mintConnectionToken(actor: String!, scope: [String!], ttl: String): ConnectionToken!
  mintRouteToken(actor: String!, service: String!, ttl: String): ConnectionToken!
}

type Query {
  # Replaces the host-side compose-port-scraping.
  serviceEndpoint(name: String!): ServiceEndpoint
  ingressStatus: IngressStatus
}

type ServiceEndpoint {
  routed: Boolean!         # false when ingress.type == none
  url: String!             # "wss://agent-svc-x.agents.example.com/" when routed
  internalHost: String!    # docker DNS name
  internalPort: Int!
}
type IngressStatus { type: String!, domain: String, routes: [RouteRef!]! }
type RouteRef { service: String!, url: String! }
```

Plus the internal (non-public) `GET /edge/verify` forward_auth target described
above.

## How a service template looks (before → after)

Today an agent service template ships a Caddy + verifier sidecar and publishes
`:3007`. After:

```yaml
# rendered into angee.yaml by serviceCreate
services:
  agent-svc-{{ AGENT_ID }}:
    runtime: container
    image: angee/claude-code:latest
    command: ["stdio-to-ws", "--port", "3008", "--", "claude-code", "acp"]
    env:
      MODEL: "{{ MODEL }}"
    route:                          # ← replaces ports + the entire docker/ sidecar
      port: 3008
      host: "{{ AGENT_ID }}.{{ ingress_domain }}"
      auth: forward
```

Deleted from the template: `docker/Caddyfile`, `docker/verify-acp-token.mjs`, the
`:3007` publish, and all `ACP_AUTH_SECRET` plumbing. The container runs only
`stdio-to-ws` and is unreachable except through the edge.

## How the stack manifest template looks

```jinja
{# templates/.../angee.yaml.jinja #}
version: 1
kind: stack
name: {{ STACK_NAME }}

ingress:
  type: caddy
  domain: {{ INGRESS_DOMAIN }}   # e.g. agents.localhost in dev

services:
  # routed services declare `route:` and publish nothing to the host
  ...
```

In dev, `ingress.domain: agents.localhost` + Caddy automatic local TLS gives
`wss://<svc>.agents.localhost/` with no host-port juggling. The operator's own
GraphQL API can itself be a routed upstream (`route:` on the operator service)
so the API and the agent services share one edge and one verifier.

## Design options

- **A. Label-driven (caddy-docker-proxy) — recommended.** Everything is
  compile-time labels; the proxy reconciles from Docker. No new runtime loop in
  the operator, deterministic, self-heals on restart, and fits the
  "compile one manifest → derived files" model exactly. The dependency is a
  *container image*, pulled only when `ingress.type: caddy` — no host binary to
  bundle (unlike process-compose).
- **B. Admin-API-driven (vanilla Caddy + `:2019`).** Operator PATCHes routes on
  service up/down. More explicit, but adds a runtime reconcile loop and drift
  handling the operator does not have today. Keep as a fallback only.

**Recommendation: A** — it requires zero new runtime machinery in the operator.

## Out of scope / caveats

- **`runtime: local` services** can't join a Docker network; routing applies to
  `runtime: container`. Agent services are containers. Local routing (static
  upstreams) is a follow-up.
- **forward_auth gates the upgrade, not the open socket** — short TTL bounds
  re-connection; the open WebSocket lives on (same as today).
- **`X-Forwarded-Uri` carrying `?token=`** through caddy-docker-proxy must be
  proven in a spike for the WS upgrade before relying on it.
- **Scope→field map** for operator-API tokens needs to be authored once and kept
  in sync as mutations are added; default-deny for unmapped fields.
- **Single edge = single chokepoint** — fine at this scale; note for capacity.
- **TLS** terminates at the edge (Caddy automatic HTTPS); backends stay plaintext
  on the private net.

## Rollout

1. **Token model + verifier + two-tier API auth** (additive; admin bearer still
   works). Ship `mintRouteToken` / extend `mintConnectionToken`.
2. **`ingress` backend + compile** behind `ingress.type` (default `none`).
   Prove an opt-in stack end-to-end.
3. **Host backend (Django)**: rewrite `operatorConnection` to mint scoped tokens;
   rewrite `agentAcpEndpoint` to mint route tokens + `serviceEndpoint`.
4. **Service template teardown**: drop the agent service's `docker/` sidecar and
   `acp_auth_secret`.

## Acceptance

- A stack with `ingress.type: none` compiles byte-identically to today.
- A stack with `ingress.type: caddy` + a routed service compiles a compose with
  one edge service (one published port), an `<name>_edge` network, the routed
  service stamped with labels and **no** host ports, and the edge
  `forward_auth` → `/edge/verify`.
- `serviceEndpoint(name)` returns the public `wss://` URL; `mintRouteToken`
  issues a token `/edge/verify` accepts for that service and rejects for another.
- The operator API accepts a minted `aud: operator` token, enforces its scope per
  field, and still accepts the admin bearer at full access.
- The host backend exposes only short-lived minted tokens to the browser; the
  admin bearer never leaves the server.
- The claude-code service template, stripped of its sidecar, chats end-to-end
  through the edge.

## See also

- [`internal/secrets/backend.go`](../../internal/secrets/backend.go) — the
  backend-by-`type` pattern this mirrors
- [`internal/runtime/backend.go`](../../internal/runtime/backend.go) — runtime
  backend interface
- [`internal/service/platform.go`](../../internal/service/platform.go) —
  `Compile()` the edge backend hooks into
- [`internal/operator/tokens.go`](../../internal/operator/tokens.go) — the minter
  this extends
- [`internal/operator/operator.go`](../../internal/operator/operator.go) — the
  `auth` middleware this extends
- [`docs/proposals/stack-update-template-sync.md`](stack-update-template-sync.md)
  — sibling proposal
