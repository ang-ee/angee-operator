# Manifest

Angee reads one manifest at `$ANGEE_ROOT/angee.yaml`.

Editor schema:

```yaml
# yaml-language-server: $schema=https://docs.angee.ai/angee.schema.json
```

The checked-in schema lives at `docs/public/angee.schema.json` and is
refreshed with `make schema`. A field-by-field reference is available at
[Manifest schema reference](/reference/manifest-schema). The schema is
intentionally a completion/type aid; runtime validation in
`internal/manifest` remains authoritative for cross-field rules such as
local services requiring `command` and container services requiring
`image` or `build`.

Minimal shape:

```yaml
version: 1
kind: stack
name: example

services:
  web:
    runtime: container
    image: nginx:alpine
    ports:
      - "8080:80"
```

## Top-Level Fields

```yaml
version: 1
kind: stack
name: example
template: {}
operator: {}
secrets_backend: {}
ingress: {}
secrets: {}
ports: {}
volumes: {}
sources: {}
workspaces: {}
services: {}
jobs: {}
port_leases: {}
```

`version`, `kind`, and `name` are required. Empty maps are accepted.

## Operator

```yaml
operator:
  url: http://127.0.0.1:9000
  domain: operator.example.test
  token_secret: operator-token
  port_pool:
    workspace:
      range: "8100-8199"
```

`url`, `domain`, `token_secret`, and `port_pool` are used by substitutions,
workspace allocation, and operator setup.

## Secrets

Env-file backend:

```yaml
secrets_backend:
  type: env-file
  path: .env

secrets:
  django-secret-key:
    generated: true
    length: 48
  github-token:
    import: GITHUB_TOKEN
```

OpenBao backend:

```yaml
secrets_backend:
  type: openbao
  address: http://127.0.0.1:8200
  mount: secret
  token: ${BAO_TOKEN}
```

Secret substitutions use `${secret.name}` in service and job fields.

## Ingress

`ingress` selects an edge backend by `type`, defaulting to `none` (today's
host-published-ports behavior). With `type: caddy`, the operator compiles a
single Caddy edge (`lucaslorentz/caddy-docker-proxy`) into the compose file,
puts routed services on a private network with **no** host-published ports, and
authenticates inbound connections at the edge.

```yaml
ingress:
  type: caddy            # none (default) | caddy
  domain: agents.localhost  # base domain; defaults to operator.domain
  # image:   lucaslorentz/caddy-docker-proxy:2.9   # override the edge image
  # network: <name>_edge                            # override the private network
  # verify:  http://operator/edge/verify            # forward_auth target
```

A service opts into routing with a `route:` block instead of publishing host
ports — it is reached only through the edge:

```yaml
services:
  agent:
    runtime: container        # routing is container-only
    image: angee/agent:latest
    route:
      port: 3008              # container port the edge proxies to
      host: agent.agents.localhost  # default: <service>.<ingress.domain>
      # auth: forward          # forward (default) | none
```

A routed service publishes no host port and takes no lease from
`operator.port_pool` — only the edge publishes (`:443`/`:80`). `route:` on a
`runtime: local` service is rejected (it can't join a Docker network).
TLS terminates at the edge; backends stay plaintext on the private network.

> **Operational note:** every container start/stop reconciles
> caddy-docker-proxy, which reloads Caddy and severs active WebSockets. Use
> short connection-token TTLs (~60 s) and client auto-reconnect, and debounce
> bursts of container events. Edge tokens passed as `?token=` are stripped from
> access logs.

## Services

Container service:

```yaml
services:
  web:
    runtime: container
    image: nginx:alpine
    command: ["nginx", "-g", "daemon off;"]
    env:
      EXAMPLE: value
    ports:
      - "8080:80"
    mounts:
      - "source://app:/app"
    workdir: /app
    depends_on: [db]
```

Local service:

```yaml
services:
  api:
    runtime: local
    command: ["go", "run", "./cmd/server"]
    env:
      PORT: "${ports.api}"
    workdir: "source://app"
```

Container services require `image` or `build`. Local services require
`command` and must not set `image`.

## Jobs

```yaml
jobs:
  migrate:
    runtime: local
    command: ["go", "test", "./..."]
    workdir: "source://app"
    depends_on: [db]
```

Jobs are run explicitly with `angee job run <name>`.

## Sources

Implemented source kinds:

```yaml
sources:
  app:
    kind: local
    path: ..

  library:
    kind: git
    repo: https://github.com/example/library.git
    default_ref: main
    cache_path: sources/library
```

Git commands use the host git environment.

## Workspaces

Workspace records are usually written by `angee workspace create`.

```yaml
workspaces:
  fix-123:
    template: workspaces/pr
    inputs:
      branch: fix-123
    ttl: 24h
    ttl_expires_at: 2026-05-10T12:00:00Z
```

TTL values are stored and surfaced by status commands.

## Substitutions

Supported namespaces include:

```text
${secret.name}
${service.name.host}
${service.name.port}
${service.name.url}
${ports.name}
${alloc.pool}
${workspace.name.path}
${source.name.path}
${persist.name}
${operator.url}
${operator.domain}
${inputs.name}
${name}
```

Supported filters include `slug`, `lower`, `upper`, `local_part`,
`truncate(n)`, `default(value)`, `required(message)`, `b64encode`, and
`replace(old,new)`.
