# Manifest

Angee reads one manifest at `$ANGEE_ROOT/angee.yaml`.

Editor schema:

```yaml
# yaml-language-server: $schema=https://docs.angee.ai/angee.schema.json
```

The checked-in schema lives at `docs/public/angee.schema.json` and is
refreshed with `make schema`. A field-by-field reference is available at
[Manifest schema reference](/cli/manifest-schema). The schema is
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
vcs: git
sources: {}
workspaces: {}
services: {}
jobs: {}
port_leases: {}
```

`version`, `kind`, and `name` are required. Empty maps are accepted.
`vcs` (`git`, the default, or `jj`) is the version control a workspace
slot that names no `mode` is checked out with; see
[jj slots](#jj-slots).

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

> For the end-to-end picture — the auth model, the token kinds, WebSockets, and
> dev setup — see the [Edge Ingress & Scoped Tokens guide](./ingress.md). This
> section is the field reference.

```yaml
ingress:
  type: caddy            # none (default) | caddy
  routing: host          # host (default) | path
  tls: auto              # auto (default, Caddy HTTPS) | off (plain ws://)
  domain: agents.localhost  # base domain; defaults to operator.domain
  # aliases: [ap.example.com]  # routing: path only — more hosts for the same site
  # port: 8080            # tls: off only — host port for the plain ws:// edge (default 80)
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
      # host: agent.agents.localhost  # host mode override (default: <service>.<ingress.domain>)
      # path: /agent                  # path mode override (default: /<service>)
      # auth: forward          # forward (default) | none
```

A routed service publishes no host port and takes no lease from
`operator.port_pool` — only the edge publishes (`:443`/`:80`, or a single
plain-HTTP host port with `tls: off` — `:80` by default, `ingress.port` to
override). `route:` on a `runtime: local` service is rejected (it can't join a
Docker network). TLS terminates at the edge; backends stay plaintext on the
private network.

### Routing modes

`ingress.routing` selects how the edge matches inbound requests:

- **`host`** (default) — one subdomain per routed service:
  `wss://<service>.<ingress.domain>/`. The edge matches on the `Host` header.
  Override the subdomain per service with `route.host`. Best for production:
  one wildcard cert, clean per-service TLS, no path coupling.
- **`path`** — one shared host with a prefix per service:
  `wss://<ingress.domain>/<service>/`. The edge matches a path prefix and
  **strips** it before proxying, so the backend still sees `/`. Override the
  prefix per service with `route.path` (default `/<service>`). `routing: path`
  requires a domain (`ingress.domain` or `operator.domain`).

With `routing: path`, `ingress.aliases` lists more host names the edge serves
as the same site, with every route at the same path on each, and with
`tls: auto` a certificate for each:

```yaml
ingress:
  type: caddy
  routing: path
  domain: angee.example.com          # canonical: route URLs use it
  aliases: [ap.example.com, painkiller.example.com]
```

DNS for each alias must point at the edge. Each alias must be a host name, not
the domain, and not repeated (host names compare case-insensitively). The
application decides what to serve per `Host`; the edge only answers for it.

`route.host` and `route.path` are mutually exclusive on a single service.

`ingress.tls: off` drops the edge to plain HTTP, so URLs become `ws://…`
instead of `wss://…` and the edge publishes a single plain-HTTP host port
(`:80` by default; set `ingress.port: <n>` to bind another so parallel stacks on
one host don't all contend for `:80` — the route URL then carries it,
`ws://<host>:<n>/…`). Combined with
`routing: path` and `domain: localhost`, this is the zero-setup local dev path —
`localhost` always resolves and no local-CA cert needs trusting:

```yaml
ingress:
  type: caddy
  routing: path
  tls: off
  domain: localhost   # a freshly provisioned agent is reachable at
                      # ws://localhost/<service>/ with no DNS or cert setup
```

> **Operational note:** every container start/stop reconciles
> caddy-docker-proxy, which reloads Caddy and severs active WebSockets. Use
> short connection-token TTLs (~60 s) and client auto-reconnect, and debounce
> bursts of container events. The operator does not log request URIs, so tokens
> passed as `?token=` are not written to operator logs; short TTLs remain
> defense-in-depth.
>
> **Security note:** the edge mounts the Docker socket, a high-privilege grant
> inherent to caddy-docker-proxy. The operator must be the sole owner of the
> `ingress.verify` name on a dedicated edge network.

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

A container mount is `<scheme>://<what>:<target>[:ro]`:
- `source://<name>[/<subpath>]` mounts a source's checkout.
- `workspace://<name>[/<subpath>]` mounts a workspace.
- `volume://<name>` mounts a declared volume.
- `bind://<host path>` mounts a host path. A relative path is relative to the
  stack root.

A read-only `bind://` mount (`:ro`) must exist: the container fails to start
with `bind source path does not exist`, rather than Docker creating an empty
root-owned directory in its place. A writable bind of a missing path is still
created, as before.

Set `stop_grace_period` to bound graceful shutdown before the runtime
force-stops a service. The value uses Go duration syntax, such as `30s` or
`2m`. Compose receives the duration directly; process-compose rounds it up to
whole seconds. The default is `10s`.

### Restart policy

`restart` says what the runtime does when a service exits:

- `on_failure` (the default) restarts it after a non-zero exit, a crash or a
  kill from outside the runtime;
- `always` restarts it after any exit;
- `no` leaves it stopped.

```yaml
services:
  worker:
    runtime: local
    command: ["celery", "-A", "app", "worker"]
    restart: always
```

Each runtime applies its own policy. Angee writes it into the generated files
and never restarts a process itself:

| `restart` | process-compose (`availability`) | Docker Compose (`restart`) |
|---|---|---|
| `on_failure` | `restart: on_failure` | `on-failure` |
| `always` | `restart: always` | `unless-stopped` |
| `no` | `restart: "no"` | `"no"` |

- **Backoff.** Angee sets none, so each runtime uses its own. process-compose
  waits a constant second before each restart. It also waits that second
  between stopping and starting a process on `angee service restart`, so a
  longer backoff would add to every restart. Docker doubles its wait from
  100 ms up to a minute. Neither limits the number of restarts.
- **Crash loops.** A local service that keeps crashing restarts about once a
  second, plus however long it runs. Its output goes to its own
  `run/logs/<name>.log`, which process-compose rotates at 10 MB, keeping three
  rotated files, so a crash loop's log stays bounded.
- **Status.** A service waiting out its backoff reads `restarting` in
  `angee status` and `angee service list`. A local service also shows the exit
  that ended its last run and its restart count, such as
  `restarting (exit 1, 3 restarts)`, and once running again, the count, such
  as `running (3 restarts)`. Docker's status reports neither for a container.
  A job or service waiting on a service the runtime keeps restarting says so
  in its reason, and `angee dev` prints it while it waits for jobs.
- **Deliberate stops.** A stop either runtime was asked for never triggers a
  restart: `angee down`, `angee service stop` and `angee service restart`, and
  for Docker also `docker kill`.
- **Fixing a crash loop.** After fixing `angee.yaml`, run
  `angee service restart <name>`: it always gives the service the compiled
  definition and restarts it. `angee service up <name>` passes a changed
  definition only to a local service caught in its restart backoff; one that
  has relaunched reads as running, so `up` leaves it on the old definition.
  A job that was waiting on the service is skipped when process-compose
  replaces it; run it again with `angee job run <job>`.
- **Readiness.** process-compose stops a local service whose readiness probe
  fails `retries` times in a row. With `always` it then restarts it, and with
  `on_failure` it does unless the service exited 0 on that stop. Docker only
  marks a failing container unhealthy.
- **Jobs** take no `restart`. They run on request, and a failed run is reported
  as failed.
- **The edge** container that serves routes restarts on failure.

A process-compose supervisor that is already running applies a changed policy
when Angee next starts or restarts the process, or after `angee down`.
Compose recreates container services whose policy changed on the next
`angee up`.

### Readiness probes

A service can declare when it is ready to accept work. Set exactly one probe
kind under `ready`:

```yaml
services:
  api:
    runtime: container
    image: example/api:latest
    ready:
      http:
        port: 8080
        path: /healthz       # default: /
      interval: 5s          # default: 5s
      timeout: 3s           # default: 3s
      retries: 12           # default: 12, minimum: 1
      start_period: 0s      # default: 0s

  database:
    runtime: container
    image: postgres:18
    ready:
      tcp:
        port: 5432

  worker:
    runtime: local
    command: ["python", "worker.py"]
    workdir: source://app
    ready:
      cmd: ["python", "manage.py", "check"]

  assets:
    runtime: local
    command: ["npm", "run", "watch"]
    workdir: source://app
    ready:
      file: dist/index.html
```

`http` succeeds when the endpoint responds successfully, `tcp` when the port
accepts a connection, `cmd` when the command exits with status 0, and `file`
when the path exists and is non-empty. Commands run inside the container or in
the local process's workdir. Relative file paths also resolve against that
workdir. Timing values use Go duration syntax, such as `500ms`, `5s`, or `1m`;
local services round them up to whole seconds because process-compose probes
count in seconds.

Container `http` probes require `wget` or `curl` in the image, and container
`tcp` probes require `nc`. Container `file` and `cmd` probes are always safe
from this image-utility caveat and are the preferred choice for minimal images.

When `after:` or `depends_on:` targets a service with `ready`, Angee waits for
the target to become healthy. A target without `ready` keeps the existing
started-only behavior. Dependencies on jobs still wait for successful job
completion. This applies to both Docker Compose and process-compose runtimes.

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

### Remotes

`repo` declares one remote, `origin`. A git source with more than one remote
lists them under `remotes` instead; setting both is an error:

```yaml
sources:
  app:
    kind: git
    remotes:
      origin: git@github.com:me/app.git        # a bare URL
      upstream:
        url: https://github.com/acme/app.git
        canonical: true                        # fetch-only
      staging:
        url: git@example.com:acme/app.git
        branches: ["release/*"]
    push_default: origin
    default_ref: main
```

- **`canonical`** marks the remote the project lives in. The cache is cloned
  from it, under its own name, and git is set up so that nothing can push to
  it: its push URL becomes `angee-fetch-only://<name>`, a scheme no git
  remote helper serves, and every push to it fails. At most one remote is
  canonical.
- **The clone remote** is the canonical remote, else `origin`, else the only
  remote. A source with several remotes where none qualifies is an error.
- **`push_default`** names the remote a push goes to when the branch names
  no `pushRemote`. It becomes the cache's (and a clone slot's)
  `remote.pushDefault`, so plain `git push` and
  `angee workspace source publish` without `--remote` both follow it. It
  must be declared, and cannot be the canonical remote.
- **`branches`** lists the branch patterns a publish to that remote may
  create or update, in `path.Match` syntax, where `*` does not match across
  a `/`: `feature/*` matches `feature/x` but not `feature/x/y`. Publishing a
  branch that matches none of them is refused before anything changes. The
  patterns apply to a publish to a declared remote; a remote added to the
  cache by hand has none. The canonical remote takes none, since nothing is
  published to it: a publish to it is refused before anything changes, and
  git's push block refuses any push to it made outside angee.
- Remote names start with a letter or digit and hold only letters, digits,
  `.`, `_` and `-`, and no two differ only in case.
- A URL under `remotes` is a URL, an `host:path` address or an absolute
  path, never a relative path. No URL, `repo` included, may hold a control
  character or start or end with a space.

**The base.** A slot's base ref (its `ref`, else the source's `default_ref`)
is read from, in order: the remote branch git has the local branch of that
name track, the clone remote's branch, then the other remotes' branches by
remote name. For `repo:` that is `origin`'s, unless the local branch tracks
another remote. The same base is used for a new worktree slot's start, for
status counts and for `sync-base`.

### How the cache follows the manifest

A missing cache is cloned from the clone remote, named as in the manifest
whatever git's `clone.defaultRemoteName` says, into a staging directory beside
the cache path. It is given every other declared remote, the canonical push
block and the push default there, and only then moved to the cache path, so
nothing can use it while it could still push to the canonical remote. Its
permissions are those of a plain `git clone`.

- A `cache_path` that is a symlink is cloned into the symlink's target.
- A `cache_path` that is an existing empty directory (a mount point, say) is
  cloned in place, as `git clone` does, and configured right after. For that
  short moment the canonical remote is not yet blocked.
- A path that holds anything other than a git checkout is refused.
- The directory that holds `cache_path` must be writable, for the cache's
  lock file (below). To bind-mount a cache into a container, mount its parent
  directory rather than the cache alone.

A clone slot (`mode: clone`, or a git slot with no `mode`) is set up the same
way, since it is a repository of its own. Worktree slots share the cache's
config.

A cache can be shared by several stacks, so angee holds a lock file beside
it, `.<name>.angee.lock` (beside the target of a symlinked `cache_path`),
while it changes the cache's worktree registrations or talks to its remotes:
cloning or fetching the cache, `source pull` and `source push`, and adding or
pruning workspace worktrees. A stack or the CLI that finds the lock held
warns that another angee process is changing the cache and waits, up to
`ANGEE_LOCK_TIMEOUT` when that is set. The verbs on a workspace slot (fetch,
pull, push, sync-base, merge, rebase, publish) take no lock: git keeps each
ref update whole, and they never prune registrations.

When the cache already exists, `angee source fetch` and `angee source pull`
first reconcile it with the declared remotes, logging a warning for each
change, so an edited remote takes effect:

- each declared remote's URL, added when the remote is missing;
- under `remotes`, the canonical remote's push block, which a remote declared
  without `canonical` loses;
- `remote.pushDefault`, when `push_default` is set;
- under `remotes`, after the fetch, the branch `default_ref` tracks: it is
  set to the clone remote's, as a fresh clone has it. A cache cloned under
  `repo:` tracks `origin`, which `remotes` may now name a fork.

A remote with several URLs is refused rather than rewritten. Stack bring-up,
`angee workspace create` and `angee workspace repair` change nothing and
only warn about each difference, because a `cache_path` can be shared by
several stacks or be your own clone. A source declared with `repo` never
reads or changes push URLs or tracking, so it cannot lift a block another
stack sharing the cache set. Remotes the manifest does not declare are never
read or changed, and one it stops declaring stays in the cache. A relative
`repo` path is not compared.

A clone slot is configured only when it is created. `angee workspace status`
and `angee workspace repair` compare it with the declared remotes whenever
they read it, and warn about each difference, such as a remote marked
canonical since. They change nothing, so the warning says to use git in the
slot, and it repeats on every read until the slot is fixed. A slot without
its own `.git` is not compared.

## Workspaces

Workspace records are usually written by `angee workspace create`.

```yaml
workspaces:
  fix-123:
    template: workspaces/pr
    inputs:
      branch: fix-123
    sources:
      app:
        source: app
        mode: worktree
        branch: fix-123
        ref: main
        subpath: app
    ttl: 24h
    ttl_expires_at: 2026-05-10T12:00:00Z
```

TTL values are stored and surfaced by status commands.

Each entry under a workspace's `sources` records how one source was
materialized into the workspace. `mode` is `worktree` (a git worktree of the
source's shared cache), `clone` (a standalone clone) or `jj` (a jj workspace
of the cache, see [jj slots](#jj-slots)). Omitted, the stack's `vcs` decides:
under `vcs: jj` the slot is a jj slot; otherwise a git source is cloned and a
local source is linked. A local source is linked under any mode but `jj`.
Any other value is rejected when the manifest is loaded. A worktree source is
cut on `branch`. Without a branch it starts on a detached HEAD at `ref` and
gets its branch when it is published (see
[workspace commands](./commands.md)).

### jj slots

A jj slot is a [jj](https://jj-vcs.github.io/jj/) workspace of its source's
primary, made in place at the slot's path. For a git source the primary is
its cache, which angee colocates with jj (`jj git init --git-repo`) the
first time a jj slot needs it; git keeps working there and in the cache's
git worktree slots, so one stack can mix git and jj slots of one source. For
a `kind: local` source the primary is the local store, which must already be
a jj repository: angee never makes one.

```yaml
vcs: jj               # slots with no mode are jj slots
workspaces:
  fix-123:
    template: workspaces/src
    sources:
      app:
        source: app   # no mode: a jj slot under vcs: jj
        ref: main
      tree:
        source: app
        mode: worktree
        branch: fix-123
```

- **Colocated.** A slot of a primary that has a `.git` (a cache always has
  one) is colocated: it has a `.git` of its own, so git reads it too, on a
  detached HEAD. A slot of a local store without one has none.
- **Where it starts.** On the bookmark named by the slot's `branch` when the
  primary has it, else at the base: `ref` (or the source's `default_ref`) on
  the source's clone remote, else the local bookmark. A jj slot needs a base.
  No bookmark or branch is created for it.
- **Its name.** The jj workspace is named `<stack>--<workspace>`, with
  `--<slot>` added when a workspace has several jj slots of one primary. The
  name is computed from the manifest, never stored, so none of the stack,
  workspace or slot names may be empty, hold `--`, or start or end with `-`.
  Renaming a stack that has jj slots leaves their workspaces under the old
  names.
- **A name already taken.** When the primary already has a workspace of
  that name that this stack's manifest does not account for, such as one of
  another stack with the same name that shares the cache, the create is
  refused as a conflict naming it, and nothing is forgotten. A slot whose
  directory is gone is added again under its name only when nothing else is
  at the path jj recorded for it; for a cache outside the stack root, where
  that path may be another stack's out of angee's sight, the conflict says
  to forget the name with `jj workspace forget` instead. Destroy forgets
  only the workspace the slot's own checkout is.
- **Requirements.** jj with `jj workspace add --colocate` (a current build,
  such as `brew install --HEAD jj`), git 2.48 or later, since jj's add sets
  `extensions.relativeWorktrees` on the cache, and not Windows. A create
  checks them before any slot is cut, and `angee doctor` reports them.
- **Containers.** A slot's `.git` and `.jj` lead to the primary by path. A
  cache inside the stack root is reachable wherever the stack is; one
  outside it (an absolute `cache_path`) must already exist where angee runs,
  so an operator in a container must mount it at the same path. Otherwise
  the create is refused naming the path, rather than cloning a cache only
  the container sees. The primary's lock file, `.<name>.angee.lock`, is
  written beside a local store too.
- **What angee does with one.** `workspace create`, `status`, `git`,
  `repair` and `destroy` handle jj slots: status reads them through jj,
  which snapshots the working copy first, so an edit no jj command has
  recorded yet counts as uncommitted, and a jj slot is never
  `branch-mismatch`. The other slot verbs do not run on a jj slot yet (see
  [workspace commands](./commands.md#jj-slots)).
- **Not supported.** angee never makes a jj slot sparse, so files outside
  sparse patterns are not looked at. jj does not record a repository nested
  in a slot (a `git clone` into it, say), so status reads such a slot clean;
  `workspace destroy` refuses it until the nested repository is gone.

`vcs` applies to every slot that names no mode, existing ones included: the
manifest keeps a slot's mode as written. Set it before creating workspaces,
or give existing slots an explicit `mode`.

### Workspace defaults

`workspace_defaults` carries stack-level input defaults for every workspace
cut from one template. The map is keyed by template ref the way
`angee workspace create --template` resolves it (`workspaces/src`; the bare
name `src` is accepted). Inputs layer lowest to highest: the template's
`_angee.inputs` defaults, the stack's `workspace_defaults`, a declared
workspace's `inputs`, then the request's `--input` values. An explicit empty
`--input key=` still wins over a stack default.

```yaml
workspace_defaults:
  workspaces/src:
    inputs:
      work_state_source: work-angee
```

`angee workspace preflight --template src` reports the effective inputs and
lists the stack's contribution under `stack_defaults`.

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
