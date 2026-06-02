# Ingress Phase — Caddy edge backend (build loop)

**Status:** In progress · **Branch:** `feat/ingress-caddy-edge` · **Source:**
[`docs/proposals/edge-ingress-caddy.md`](../../docs/proposals/edge-ingress-caddy.md)
(Part 1 + `/edge/verify`) and the 2026-06 prior-art research folded into it.

This is the in-repo "ingress phase": manifest `ingress`/`route`, compose
networks/labels, the `edge` backend package, the `Compile()` hook, the
`/edge/verify` forward_auth endpoint, and the `serviceEndpoint`/`ingressStatus`
queries. Token minting (`mintRouteToken`, scoped `mintConnectionToken`) and the
shared `Verify` already shipped in v0.5.6/0.5.7 — this phase consumes them.

Cross-repo follow-ups (Django `agentAcpEndpoint`, service-template teardown) are
**out of scope** here.

## Build loop

For each chunk: **Codex builds** → **Claude verifies** (runs the Verify
criteria; on failure, re-delegates to Codex with the failure) → check both
boxes → advance. Keep this file as the single source of truth for state.

## State

- **Current chunk:** ✅ ALL CHUNKS COMPLETE (A–I built by Codex, verified by
  Claude, committed). `make check` green. Ready for review/PR. Remaining: the
  caddy-docker-proxy global-snippet/forward_auth label + WS-through-edge
  run-spike (TODO(spike) in `edge/caddy.go`), and the cross-repo Django +
  service-template work (out of scope here).
- **Legend:** `[ ]` todo · `[x]` done · `[~]` in progress · `[!]` blocked
- **Design guardrails (from research — every chunk must respect):**
  - `ingress.type` defaults to `none`; a `none`/absent ingress compiles
    **byte-identically** to today.
  - `route:` is **container-only** (`runtime: local` → validation error).
  - `/edge/verify` returns **2xx-never-101**; reads token from
    `?token=`/`Authorization`/`Sec-WebSocket-Protocol`; requires
    `aud=svc:<service>`.
  - Edge labels must keep idle WebSockets alive (`flush_interval -1`); the
    edge is the **only** service publishing a host port.
  - Reconcile churn drops live WebSockets — note debounce/short-TTL/auto-reconnect
    as a requirement in docs (Chunk I); no operator reconcile loop is added.

| Chunk | Build | Verify | Title |
|---|---|---|---|
| A | [x] | [x] | Manifest `Ingress`/`Route` types + defaults + validation |
| B | [x] | [x] | Compose `Networks`/`Labels` fields |
| C | [x] | [x] | `edge` backend package (interface + FromManifest + None) |
| D | [x] | [x] | `CaddyBackend.Contribute` (inject edge, network, labels) |
| E | [x] | [x] | `Compile()` hook wiring the edge backend |
| F | [x] | [x] | `/edge/verify` forward_auth endpoint |
| G | [x] | [x] | `serviceEndpoint` + `ingressStatus` GraphQL |
| H | [x] | [x] | Port-lease skip for routed services |
| I | [x] | [x] | Docs + CHANGELOG + schema regen |

---

## Chunk A — Manifest `Ingress`/`Route` types + defaults + validation

**Build (`internal/manifest/manifest.go`):**
- Add `Ingress Ingress` field to `Stack` (yaml/json `ingress,omitempty`).
- `Ingress` struct: `Type` (`validate:"omitempty,oneof=none caddy"`), `Domain`,
  `Image`, `Network`, `Verify` — all `omitempty`, matching `SecretsBackend` tag
  style (`yaml`+`json`+`jsonschema`).
- Add `Route *Route` to `Service` (`route,omitempty`). `Route`: `Port int`
  (required), `Host`, `Path`, `Auth` (`omitempty,oneof=forward none`).
- `Stack.Defaults()`: set `Ingress.Type = "none"` when empty (one line, mirrors
  `SecretsBackend.Type`).
- `ValidateExtended`: a service with a non-nil `Route` and `Runtime == local`
  is an error ("route requires runtime: container").

**Verify (Claude):** ✅ done — gofmt/build/vet/`go test ./internal/manifest/...`
all clean; Port validation tightened to `gte=1,lte=65535` during verify.
- [x] `go build ./...` clean.
- [x] `go test ./internal/manifest/...` passes.
- [x] New test: empty ingress → `Type=="none"` after `Defaults()`.
- [x] New test: `route:` on a `runtime: local` service → validation error; on a
  container service → OK.
- [x] ~~existing manifest round-trips unchanged~~ — **corrected**: `LoadFile`/
  `SaveFile` already call `Defaults()`, so the manifest normalizes `ingress: none`
  on save exactly like `secrets_backend: env-file` does today. The real
  byte-stability guarantee is at the **compile** layer (Chunk E's no-op
  none-backend), not manifest save.

## Chunk B — Compose `Networks`/`Labels` fields

**Build (`internal/runtime/compose/doc.go`):**
- `File` gains `Networks map[string]Network \`yaml:"networks,omitempty"\``;
  define `Network` struct (empty/`{}` for now, `External bool` optional).
- `Service` gains `Networks []string \`yaml:"networks,omitempty"\`` and
  `Labels map[string]string \`yaml:"labels,omitempty"\``.

**Verify (Claude):** ✅ done — gofmt/build/vet/`go test ./internal/runtime/compose/...`
all clean; tests assert round-trip DeepEqual + `omitempty` proof.
- [x] `go build ./...` clean.
- [x] Marshal test: a service with networks+labels and a file-level network
  round-trips through `Marshal`.
- [x] A service/file without them marshals **byte-identically** to before
  (omitempty).

## Chunk C — `edge` backend package (interface + FromManifest + None)

**Build (new `internal/runtime/edge/backend.go`):**
- `Backend` interface: `Contribute(stack *manifest.Stack, compiled *compose.File) error`.
- `FromManifest(cfg manifest.Ingress) (Backend, error)`: `""`/`"none"` →
  `NoneBackend{}`; `"caddy"` → `NewCaddyBackend(cfg)`; else error.
- `NoneBackend.Contribute` is a no-op returning nil.

**Verify (Claude):** ✅ done — gofmt/build/vet/test clean; test asserts DeepEqual
no-op on a populated compose file + unsupported-type error. Caddy case is a
placeholder error wired in Chunk D.
- [x] `go build ./...` clean.
- [x] Test: `FromManifest({Type:""})` and `{Type:"none"}` → no-op Contribute
  leaves the compose unchanged; unknown type → error.

## Chunk D — `CaddyBackend.Contribute`

**Build (new `internal/runtime/edge/caddy.go`):**
- Resolve defaults: `Network` → `<stack.Name>_edge`; `Image` →
  `lucaslorentz/caddy-docker-proxy:2.9`; `Domain` → `operator.domain`;
  `Verify` → the operator's `/edge/verify`.
- `Contribute`: add `compiled.Networks[<net>] = {}`; inject the edge service
  (image, docker socket RO mount, **only** published port `443`/`80`, joined to
  `<net>`, global `forward_auth` snippet → `Verify`); for each service whose
  manifest `Route != nil`: clear `Ports`, append `<net>` to `Networks`, stamp
  Caddy labels (`caddy` host, `caddy.reverse_proxy {{upstreams <port>}}`,
  `caddy.reverse_proxy.flush_interval -1`, forward_auth import for that service).
- Services without a `Route` are untouched.

**Verify (Claude):** ✅ done — gofmt/build/vet/test clean; test asserts edge
service (exact ports + docker-socket vol + net), routed service (ports cleared,
net joined, all 4 caddy labels exact), plain service untouched. Exact
caddy-docker-proxy global-snippet/forward_auth label syntax is best-effort
(TODO(spike)) to validate by running Caddy.
- [x] `go build ./...` clean + unit tests pass.
- [x] Test: caddy backend on a stack with one routed + one plain service →
  compose has the edge service (one published port), the `<net>` network, the
  routed service stripped of `Ports` + joined to `<net>` + labeled, and the
  plain service unchanged.

## Chunk E — `Compile()` hook

**Build (`internal/service/platform.go`):**
- After services/jobs are built and before `return compiled, nil`, call
  `edge.FromManifest(stack.Ingress).Contribute(stack, &compiled.Compose)` and
  wrap any error.

**Verify (Claude):** ✅ done — hook placed before final return with error
wrapping; test proves none→no `edge` service + empty networks, caddy→edge present
+ routed ports cleared (through the real `Compile`); existing service tests
unchanged; `make check` green.
- [x] `Compile` with `ingress: none`/absent → no edge service, empty networks
  (none-backend inert; existing tests unchanged).
- [x] `Compile` with `ingress: caddy` + a routed service → edge injected per
  Chunk D.
- [x] `make check` green.

## Chunk F — `/edge/verify` forward_auth endpoint

**Build (`internal/operator/`):**
- `GET /edge/verify`: resolve the target service (`?service=` or derive from
  `X-Forwarded-Host`); read the token from `?token=` (via the request URI),
  `Authorization: Bearer`, or `Sec-WebSocket-Protocol`; call
  `s.tokens.Verify(raw, serviceAudience(service))`. **200** on success, **401**
  otherwise — **never 101**. Mount **without** `s.auth` (it is itself the auth
  target; only reachable from the edge network).

**Verify (Claude):** ✅ done — full operator suite passes with `-race` in a
non-sandboxed env (Codex's sandbox blocks `httptest` listeners); mounted outside
`s.auth`.
- [x] Test: valid `svc:<name>` token → 200; token for another service → 401;
  `aud=operator` token → 401; missing/garbage → 401.
- [x] Token accepted from each of `?token=` / `Authorization` / `Sec-WebSocket-Protocol`.
- [x] Response is 2xx/4xx, never 101.

## Chunk G — `serviceEndpoint` + `ingressStatus` GraphQL

**Build (`internal/operator/schema.graphql` + resolvers, `go generate`):**
- `serviceEndpoint(name: String!): ServiceEndpoint` and `ingressStatus: IngressStatus`.
- Types: `ServiceEndpoint { routed, url, internalHost, internalPort }`,
  `IngressStatus { type, domain, routes: [RouteRef!]! }`, `RouteRef { service, url }`.
- Resolvers via `service.Platform`, reading `stack.Ingress` + each service's
  `Route` (replaces host-side compose-port-scraping). `routed=false` when
  `ingress.type == none`.

**Verify (Claude):** ✅ done — fresh `go generate` consistent; full suite
(service/operator/gql/api) passes `-race`; `make check` green incl. the
`surfaces.md` exported-method guard. Types mapped to `api.*` via gqlgen.yml.
- [x] `go generate ./internal/operator` is clean; resolvers preserved.
- [x] Test: `ingress: none` → `serviceEndpoint.routed == false`; `ingress: caddy`
  + routed service → `wss://`-style `url`, `internalHost`/`internalPort` set;
  `ingressStatus` lists the route.

## Chunk H — Port-lease skip for routed services

**Build (`internal/ports` / `internal/service`):**
- Routed services (`Route != nil`) must **not** acquire a port lease — only the
  edge publishes a host port.

**Verify (Claude):** ✅ done — guard reuses the empty alloc, early-returns for
routed (caddy + route) services; test asserts routed→no lease, plain→leases;
`make check` green.
- [x] Test: a routed service acquires no lease; a non-routed service still does.
- [x] `make check` green.

## Chunk I — Docs + CHANGELOG + schema regen

**Build:**
- `docs/guide/manifest.md`: document `ingress:` and service `route:`.
- `docs/reference/operator-api.md`: `/edge/verify`, `serviceEndpoint`,
  `ingressStatus`, and the **operational note** (config-reload drops live WS →
  debounce + 60 s TTL + client auto-reconnect; token-in-query → strip from logs).
- `CHANGELOG.md` `Unreleased`.
- Regenerate `docs/reference/manifest-schema.md` if present.

**Verify (Claude):** ✅ done — `ingress`/`route` documented in the manifest
guide (with the WS-reload operational note); `/edge/verify`, `serviceEndpoint`,
`ingressStatus` documented in operator-api.md; CHANGELOG `Unreleased` updated.
No generated `manifest-schema.md` in-repo (site relocated). `make check` green.
- [x] Schema/docs regen clean; `make check` green; final review pass.
