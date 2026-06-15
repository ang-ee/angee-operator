# Proposal: `ingress.routing` — host-based **or** path-prefix edge routing

**Status:** Draft · **Area:** manifest, edge runtime backend, operator API · **Surfaces:** manifest + compile + `serviceEndpoint`

## Summary

Give the Caddy edge ([`edge-ingress-caddy.md`](edge-ingress-caddy.md)) two routing
modes, selected by a new `ingress.routing` field:

- **`host`** (today, default) — one subdomain per routed service:
  `wss://<service>.<domain>/`. The edge matches on the **Host** header.
- **`path`** (new) — one shared host, one prefix per service:
  `wss://<domain>/<service>/`. The edge matches on a **path prefix** and strips it
  before proxying.

Everything else from the edge proposal is unchanged — the same
`caddy-docker-proxy` edge, the same per-service `forward_auth → /edge/verify`, the
same minted route tokens. Only `routeURL()` and the per-service Caddy labels
differ. `serviceEndpoint.url` (and therefore the host backend's
`agentChatEndpoint`/`serviceEndpoint`) follows whatever the operator returns, so
no consumer changes.

## Motivation

Host routing is correct for production (one wildcard cert, clean per-service TLS,
no path collisions) but is the wrong **default for local dev**:

- **`*.localhost` does not resolve.** `/etc/hosts` matches literal names only — it
  has no wildcard — and macOS does not resolve arbitrary `*.localhost` subdomains.
  So `wss://agent-demo-agent.notes-angee.localhost/` is `NXDOMAIN` until the
  developer hand-adds a line **per agent**, which defeats "click Provision and
  chat." Every new agent is a new hostname.
- **Per-subdomain TLS trust.** Host mode obtains a Caddy local-CA cert per
  subdomain; each must be trusted by the browser. Path mode needs **one** cert
  (the shared host) — or none, if dev drops to plain `ws://` on a port.
- **Reload blast radius.** Host mode is one Caddy *site block* per service; path
  mode is one site block with N route handlers. Neither avoids the
  config-reload-drops-WebSockets risk (edge proposal § Prior art), but a single
  shared host is a smaller surface to reason about.

The dev story we want is: **`localhost` (always resolves) + one host + a prefix
per agent**, with TLS optional. That is path routing.

## The `routing` field

```go
// internal/manifest/manifest.go
type Ingress struct {
    Type    string `yaml:"type,omitempty" validate:"omitempty,oneof=none caddy"`
    Routing string `yaml:"routing,omitempty" validate:"omitempty,oneof=host path"` // NEW; default "host"
    Domain  string `yaml:"domain,omitempty"`
    Image   string `yaml:"image,omitempty"`
    Network string `yaml:"network,omitempty"`
    Verify  string `yaml:"verify,omitempty"`
}
```

`Stack.Defaults()` sets `Routing = "host"` when empty — existing `ingress.type:
caddy` stacks are byte-stable. The per-service `Route.Path` field already exists
in the manifest (edge proposal § Manifest additions) and now has a clear owner:
in `path` mode it overrides the default `/<service>/` prefix; in `host` mode
`Route.Host` overrides the default subdomain (mutually exclusive — validate that
a service sets at most one).

## `routeURL()` — the one resolver that branches

Today ([`internal/service/ingress.go`](../../internal/service/ingress.go)):

```go
func routeURL(serviceName string, route *manifest.Route, domain string) string {
    host := route.Host
    if host == "" {
        host = serviceName + "." + domain   // or serviceName if domain == ""
    }
    return "wss://" + host + "/"
}
```

Becomes mode-aware:

```go
func routeURL(routing string, serviceName string, route *manifest.Route, domain string) string {
    switch routing {
    case "path":
        prefix := route.Path
        if prefix == "" {
            prefix = "/" + serviceName + "/"
        }
        return "wss://" + domain + ensureTrailingSlash(prefix)   // wss://agents.localhost/agent-x/
    default: // "host"
        host := route.Host
        if host == "" && domain != "" {
            host = serviceName + "." + domain
        } else if host == "" {
            host = serviceName
        }
        return "wss://" + host + "/"
    }
}
```

`isRouted` is unchanged (`Ingress.Type == "caddy" && service.Route != nil`).
`IngressStatus` iterates the same routed services and reports each `routeURL`, so
`ingressStatus` and `serviceEndpoint` expose whichever shape is configured with no
extra fields.

## Edge label generation — `host` vs `path`

The edge backend ([`internal/runtime/edge/caddy.go`](../../internal/runtime/edge/caddy.go))
stamps different labels per mode. Both keep the **per-service `forward_auth →
/edge/verify?service=<name>`** the run-spike validated (edge proposal § Run-spike).

**`host` (today) — a site block per service:**

```caddyfile
agent-demo-agent.notes-angee.localhost {
    forward_auth host.docker.internal:9003 { uri /edge/verify?service=agent-demo-agent }
    reverse_proxy 192.168.107.3:3007 { flush_interval -1 }
}
```

**`path` (new) — one shared host, a `handle_path` per service:**

```caddyfile
notes-angee.localhost {
    handle_path /agent-demo-agent/* {
        forward_auth host.docker.internal:9003 { uri /edge/verify?service=agent-demo-agent }
        reverse_proxy 192.168.107.3:3007 { flush_interval -1 }
    }
    # one handle_path block per routed service — contributed by that container's labels
}
```

`handle_path` **strips the matched prefix** before proxying, so the backend
(`stdio-to-ws` serving at `/`) sees `/` regardless of the public prefix — the WS
upgrade path is `wss://notes-angee.localhost/agent-demo-agent/` on the wire and
`/` at the container. The per-service container labels in path mode (the exact
`caddy.*` ordering is an implementation detail to validate the same way host mode
was, but the intent):

```yaml
labels:
  caddy: "notes-angee.localhost"                                  # shared host (not per-service)
  caddy.handle_path: "/agent-demo-agent/*"
  caddy.handle_path.forward_auth: "host.docker.internal:9003"
  caddy.handle_path.forward_auth.uri: "/edge/verify?service=agent-demo-agent"
  caddy.handle_path.reverse_proxy: "{{ upstreams 3007 }}"
  caddy.handle_path.reverse_proxy.flush_interval: "-1"
```

Because every routed container contributes a `handle_path` block to the **same**
`caddy: <domain>` site, caddy-docker-proxy merges them into one site block — the
zero-runtime-loop reconcile model is unchanged.

## Dev sub-option: drop TLS entirely (`ws://localhost`)

Path mode unlocks a pure-loopback dev path that sidesteps **both** DNS and cert
trust: a single host `localhost` + a published HTTP port + plain `ws://`. This is
worth a companion `ingress.tls: auto | off` (default `auto` = Caddy local HTTPS):

- `tls: off` + `routing: path` → edge publishes `:<http-port>`, `routeURL`
  returns `ws://localhost:<http-port>/<service>/`. No `*.localhost`, no local-CA
  trust prompt, no NXDOMAIN — the exact failure the dev hit
  (`host …notes-angee.localhost → NXDOMAIN`, then untrusted local cert). `localhost`
  always resolves; `ws://` needs no cert.
- `tls: auto` (prod and trust-OK dev) keeps `wss://` with Caddy automatic HTTPS.

`forward_auth` still gates the upgrade either way (the token rides `?token=` →
`X-Forwarded-Uri`), so dropping TLS does not drop auth.

## Backend / frontend impact: none beyond following the URL

`agentChatEndpoint` (host backend) already returns the operator's
`serviceEndpoint.url` verbatim and the browser connects to exactly that — so
`host` → `wss://<svc>.<domain>/` and `path` → `ws(s)://<domain>[:port]/<svc>/`
both flow through unchanged. The ACP client opens whatever URL it is handed. No
schema or resolver change is required; this is purely an operator-side routing
decision surfaced through the existing `serviceEndpoint.url`.

## Tradeoffs

| | `host` (subdomain) | `path` (prefix) |
|---|---|---|
| Dev DNS | needs wildcard / per-agent `/etc/hosts` | `localhost` always resolves |
| TLS | cert per subdomain (or wildcard) | one cert, or none (`tls: off`) |
| Prod fit | ✅ clean per-service TLS/SNI, no path coupling | works, but one cert + path namespace |
| Backend path-awareness | backend sees `/` | edge strips prefix → backend still sees `/` |
| Cookie/origin isolation | per-subdomain origin | shared origin (fine for token-auth WS) |
| Reload surface | N site blocks | 1 site block, N handlers |

Neither mode changes the auth model or the reload-drops-WS caveat. Path mode trades
production-grade per-service origins for dev ergonomics.

## Recommendation

- **Default `host`** (byte-stable; best for prod).
- **Dev stacks set `routing: path`** (and may set `tls: off`), so a freshly
  provisioned agent is reachable at `ws(s)://<domain>/<service>/` with zero DNS or
  cert setup. The Angee dev stack template would emit:

  ```yaml
  ingress:
    type: caddy
    routing: path
    domain: notes-angee.localhost   # or just `localhost` with tls: off
  ```

## Rollout

1. **Manifest + defaults**: add `Routing` (and optionally `Tls`); default `host`.
   `ingress.type: none` and existing `caddy` stacks compile identically.
2. **`routeURL` branch + `IngressStatus`**: mode-aware URL; covered by table tests
   for both modes (host/no-domain, host/custom, path/default-prefix, path/custom).
3. **Edge backend label gen**: emit `handle_path` blocks on a shared host for
   `path`; keep per-service site blocks for `host`. Validate the path-mode label
   encoding with a `caddy-docker-proxy` spike mirroring the host-mode run-spike
   (WS upgrade + valid → `101`, bad → `401`, prefix stripped at backend).
4. **Dev template**: flip the dev stack to `routing: path` once proven.

## Acceptance

- `ingress.routing` absent or `host` compiles byte-identically to today.
- `routing: path` + a routed service compiles one shared `caddy: <domain>` site
  with a `handle_path /<service>/*` block (forward_auth + reverse_proxy,
  prefix-stripped); `serviceEndpoint.url` is `wss://<domain>/<service>/`.
- A browser WS upgrade to `…/<service>/` authenticates via `/edge/verify?service=<service>`
  (valid → `101`, bad/missing → `401`) and reaches the backend at `/`.
- `tls: off` (if adopted) yields `ws://localhost:<port>/<service>/` reachable with
  no DNS entry and no cert trust.
- Two services in `path` mode get distinct, non-colliding prefixes on one host.

## See also

- [`edge-ingress-caddy.md`](edge-ingress-caddy.md) — the edge backend this extends
  (already defines `Route.Path` and the `forward_auth → /edge/verify` mechanism)
- [`internal/service/ingress.go`](../../internal/service/ingress.go) — `routeURL`,
  `isRouted`, `ingressDomain`, `IngressStatus`
- [`internal/runtime/edge/caddy.go`](../../internal/runtime/edge/caddy.go) — the
  per-service label generation that branches by mode
- [`internal/manifest/manifest.go`](../../internal/manifest/manifest.go) —
  `Ingress` / `Route` structs and `Defaults()`
