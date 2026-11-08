# Proposal: run container services on Apple's native `container` runtime

**Status:** Draft (research 2026-11-07; not started) · **Area:** runtime backends, ingress, packaging · **Surfaces:** host container-engine setting, compile, runtime dispatch, edge, `angee doctor`, a macOS app shell (new)

## Summary

Let a stack run on a Mac without Docker Desktop, OrbStack or any shared Linux
VM, by running `runtime: container` services on Apple's native container stack
([apple/container](https://github.com/apple/container), built on
[apple/containerization](https://github.com/apple/containerization)): one
lightweight VM per container, driven by the `container` CLI.

The recommended shape keeps every concern with an owner that already exists in
angee: Apple's `container` owns the VM, image and network; **process-compose**
owns supervision. On a host whose container engine is `apple`, compile renders
each container service as a process-compose process that runs an attached
`container run --rm --name <project>-<service> …`. process-compose then gives
the restart policy, readiness probes, `depends_on` conditions, log capture and
state events that Apple's runtime does not have, and the attached process's
exit is the lifecycle event, with no polling.

The manifest does not change shape: the engine is a host setting, so one
`angee.yaml` runs on Docker on Linux and on Apple `container` on a Mac. A
native macOS app shell (menu bar + the Go operator as a login item) is the
final step and is optional.

## Problem

- On macOS, angee's container services need Docker (Docker Desktop, OrbStack,
  Colima), which is a third-party VM and daemon the user must install, license
  and keep running, and which holds memory for the whole shared VM.
- macOS 26 ships a first-party container runtime. A stack that could run on it
  could be packaged as a native Mac app with no Docker dependency.
- The current container path is wired to Docker Compose well beyond
  `internal/runtime/compose` (see below), so a second container engine cannot
  be dropped in behind `runtime.Backend` as it stands.

## Research findings (2026-11-07)

Facts carry a source; **[inference]** marks our reading.

### Apple `container` today

- **Release and platform.** 1.0.0 shipped 2026-06-09; 1.7.1 is current
  (2026-11-06), with releases every 2–4 weeks. Apple silicon only; supported on
  macOS 26 (macOS 15 runs with no container-to-container networking). Apache-2.0.
  The CLI is backward compatible within a major; the `container-apiserver` XPC
  API is forward and backward compatible within a major; JSON output shapes and
  some flags still changed within 1.x.
  ([releases](https://github.com/apple/container/releases),
  [README](https://github.com/apple/container/blob/main/README.md))
- **Architecture.** The CLI talks XPC to a per-user launch agent
  (`com.apple.container.apiserver`), which spawns one `container-runtime-linux`
  helper per container. Each container is a Virtualization.framework VM with
  `vminitd` as PID 1 and a Kata kernel; defaults are 4 CPUs and 1 GiB.
  Memory freed in a guest is not returned to macOS.
  ([technical overview](https://github.com/apple/container/blob/main/docs/technical-overview.md))
- **Interfaces.** CLI and Swift client libraries over XPC only. No REST or
  Docker API ([#66](https://github.com/apple/container/issues/66), not planned).
  `--format json` on `ls`, `stats`, `image ls`, `network ls`, `volume ls`,
  `system status`; `inspect` prints JSON.
- **Missing compared with Docker** (confirmed on 1.5.0 locally and in
  [docker-migration.md](https://github.com/apple/container/blob/main/skills/container/references/docker-migration.md)):
  compose (out of scope for the project, [#1476](https://github.com/apple/container/issues/1476)),
  restart policies ([#2142](https://github.com/apple/container/issues/2142)),
  healthchecks ([#1918](https://github.com/apple/container/issues/1918)),
  an event stream ([#484](https://github.com/apple/container/issues/484)),
  `wait`, and exit codes in `inspect`/`ls` ([#1501](https://github.com/apple/container/issues/1501)).
  The attached `container run` / `start -a` exits with the container's code;
  attaching to an already running container is rejected.
- **Present:** `run -i`, `--rm`, `-e`, `--env-file`, `-l`, `-p` (default host
  IP `0.0.0.0`), `--mount`/`-v`, `--tmpfs`, `--init`, `-u`, `--read-only`,
  `--cpus` (whole vCPUs), `--memory`, `--shm-size`, `--network`,
  `container build` (BuildKit in a builder VM), Rosetta for amd64 images.
- **Networking.** Each container gets an IP on a vmnet network; named networks
  (`container network create`) are macOS 26 only and isolated from each other.
  Name resolution needs `[dns] domain` in `config.toml` plus a one-time
  `sudo container system dns create <domain>` (writes `/etc/resolver`); names
  are unique across all of a user's containers. Bare names on custom networks
  do not resolve ([#1809](https://github.com/apple/container/issues/1809));
  per-network aliases arrived in 1.6.0. Reaching host services needs
  `sudo container system dns create … --localhost <ip>`, a pf rule lost on
  reboot ([host-integration.md](https://github.com/apple/container/blob/main/docs/host-integration.md)).
- **Storage.** Named volumes are sparse ext4 images, documented as faster than
  bind mounts, and contain `lost+found`. Bind mounts use virtiofs: files appear
  root-owned with no UID/GID mapping ([#165](https://github.com/apple/container/issues/165)),
  inotify events do not cross ([#141](https://github.com/apple/container/issues/141),
  so in-container hot reload breaks), and one preliminary benchmark measured
  ~38 MB/s writes ([findings](https://github.com/Jmeg8r/apple-container-vs-docker/blob/main/docs/findings.md)).
- **Logs.** stdout and stderr are merged into one `stdio.log`; no timestamps or
  `--since`. If an attached client dies, the log stops being written for the
  rest of the container's life ([#2009](https://github.com/apple/container/issues/2009)).
- **Hazard.** `container system stop` stops every container the user has.
- **Ecosystem.** [socktainer](https://github.com/socktainer/socktainer) serves
  ~75% of the Docker Engine API on top of Apple's client libraries, emulates
  restart and healthchecks in its own process, polls every 5 s for events, and
  pins one exact Apple release. Community compose tools
  ([Container-Compose](https://github.com/Mcrich23/Container-Compose),
  [fruitbox](https://github.com/urjitbhatia/fruitbox) and others) are partial.
  No mature Go library exists; Go tools shell out to the CLI.

### Postgres on Apple containers

- **No Postgres is built or tuned for Apple containers.** Apple publishes no
  Postgres image; its own examples use stock `postgres:16`/`17-alpine`, and
  maintainers answer Postgres bugs with configuration
  ([#333](https://github.com/apple/container/issues/333),
  [#1730](https://github.com/apple/container/issues/1730)).
- **What works:** the official `postgres:18` image declares
  `VOLUME /var/lib/postgresql` and sets `PGDATA=/var/lib/postgresql/18/docker`,
  which is exactly the layout Apple recommends: a named volume mounted at
  `/var/lib/postgresql` keeps `lost+found` out of PGDATA and avoids chown
  failures. Add `--memory 2G`, `--shm-size 256m` (default `/dev/shm` is
  64 MiB), `shared_buffers=512MB`. Never bind-mount PGDATA.
- **Durability:** since container 0.8.0 named volumes use cached disk mode with
  fsync sync, roughly matching native macOS guarantees, and the earlier ext4
  corruption under heavy I/O is fixed
  ([#1041](https://github.com/apple/container/pull/1041),
  [#614](https://github.com/apple/container/issues/614)).
- **Images:** every official `postgres` tag has arm64; `pgvector/pgvector`,
  `supabase/postgres` and `timescale/timescaledb` have arm64. `postgis/postgis`
  is amd64-only (Rosetta or a custom image).
- **Cost:** at least ~190 MiB per database VM, never returned to macOS
  ([#1698](https://github.com/apple/container/issues/1698)).
- **Native alternative:** Postgres as a `runtime: local` service — Homebrew
  `postgresql@18`, signed bundled binaries (Postgres.app's
  [entitlements](https://github.com/PostgresApp/PostgresApp/blob/master/buildscripts/postgres.entitlements)
  and notarization are a working template), or
  [embedded-postgres](https://github.com/fergusstrange/embedded-postgres) from
  Go. Process memory only, native APFS I/O, pgvector and PostGIS available,
  lower Linux parity.

### Shipping as a Mac app

- **Depend on the user's apple/container install** (signed `.pkg` or Homebrew)
  and drive its CLI. Every existing GUI over apple/container (Orchard, Berthly,
  ContainerApp, Container Desktop) does this, gates on the daemon version, and
  ships **unsandboxed**; a maintainer said sandboxed apps cannot use the client
  APIs ([#1620](https://github.com/apple/container/issues/1620)), so the Mac
  App Store is out.
- **Embedding the Containerization Swift package** needs only
  `com.apple.security.virtualization` and can bundle the kernel and init
  filesystem, but means rebuilding what the apiserver does (IPAM, DNS, image
  store, port forwarding, logs) on a 0.x API. Not recommended.
- **Shell:** a Developer ID, notarized, hardened-runtime SwiftUI menu-bar app;
  the Go operator in `Contents/Helpers`, registered as an `SMAppService` launch
  agent; Sparkle 2 for updates. Go binaries must be re-signed (the linker
  ad-hoc signs them) and need an `LC_UUID` for Local Network privacy, which
  applies to launch agents but not to CLIs run from Terminal
  ([TN3179](https://developer.apple.com/documentation/technotes/tn3179-understanding-local-network-privacy)).

## Current behavior (where Docker Compose leaks)

`runtime.Backend` (`internal/runtime/backend.go`) is sound, but the container
path assumes Compose in these places:

- **Two fixed backends.** `Platform` holds `composeBackend` and `procBackend`
  (`internal/service/platform.go`); the two fields are referenced about 47
  times across `internal/service`, mostly as hand-written
  `Runtime == local` dispatch.
- **The compiled document is Compose's model.** `CompiledStack` carries
  `compose.File` and `proccompose.File`; `compiledRuntimeConfiguration`
  (`internal/service/jobs.go`) returns Compose YAML for every non-local
  runtime, so `Target.Configuration` and `JobSpec.Configuration` are Compose
  YAML for container services.
- **Behaviour delegated to Compose:** `depends_on` conditions
  (`composeDependsOn`), healthchecks run inside the container
  (`composeHealthcheck`), restart policy (`composeRestart`), `${ANGEE_SECRET_*}`
  interpolation from the env file, service-name DNS on the project network,
  `host.docker.internal:host-gateway`, `docker compose wait` for jobs, and
  `docker events` for one-off job cleanup (#160).
- **Edge.** `edge.Backend.Contribute(*manifest.Stack, *compose.File)`; the
  Caddy backend is caddy-docker-proxy, which needs `docker.sock` and Docker
  labels, and its forward-auth default `operator:9000` assumes Compose DNS.
- **Generated files gate behaviour.** Whether `docker-compose.yaml` exists
  decides status, down and logs.
- **`angee doctor`** checks only `docker` and `docker compose`.
- **No exec anywhere,** and live views still poll every 2 s; the owner-pushed
  `Backend.Watch` is a draft (`.agents/plans/owner-pushed-live-views.md`).

## Ownership (the load-bearing decision)

Apple's runtime deliberately leaves supervision out. Something must own
restart, readiness, ordering, logs and state events. The options are to
re-implement them in angee, to rely on a third-party shim that re-implements
them, or to hand them to a supervisor angee already ships. process-compose
already owns exactly these concerns for `runtime: local` services and is the
planned event source for `Backend.Watch` (`process-compose process monitor`).
Making it own them for Apple containers too means no double implementation and
no polling: process-compose pushes state, and the attached `container run`
child's exit carries the container's exit code.

## Proposal

### 0. Prerequisites in the runtime seam (useful on their own)

1. Replace the hand-written `Runtime == local` dispatch with one
   `p.backendFor(runtime)` lookup, so which backend owns `runtime: container`
   is decided in one place.
2. Let compile place container services in the process-compose document when
   the engine is `apple`, and make `compiledRuntimeConfiguration` follow that
   placement instead of keying on `runtime`.
3. Give the edge a renderer that does not need a Compose file or the Docker
   API: a static Caddyfile generated from `route:` and `ingress:`, with the
   forward-auth target as a configurable address. Docker hosts keep
   caddy-docker-proxy.
4. Gate behaviour on the backend's own artifact rather than on
   `docker-compose.yaml` existing.
5. Teach `angee doctor` to check the active engine.

### 1. A per-host container engine

`runtime: container` keeps its meaning ("an OCI image in an isolated Linux
environment"). Which engine runs it is a host setting: `docker` (default) or
`apple`. Proposed surface: `ANGEE_CONTAINER_ENGINE` and a global
`--container-engine` flag, recorded in the operator's runtime state so the
operator and CLI agree. The engine is not written into `angee.yaml`, which
stays portable. No auto-detection: an explicit choice avoids surprising
switches when both Docker and `container` are installed.

### 2. The `apple` engine: process-compose supervises `container run`

For each container service, compile emits a process-compose process:

```yaml
processes:
  db:
    command: >-
      container run --rm --name <project>-db
      --network <project> -p 127.0.0.1:5432:5432
      -v <project>-pgdata:/var/lib/postgresql
      --memory 2G --shm-size 256m postgres:18-trixie
    availability: { restart: on_failure }      # from restart:
    readiness_probe:                           # from ready:
      exec: { command: "container exec <project>-db pg_isready" }
    depends_on: { … }                          # from after/depends_on
    shutdown: { signal: 15, timeout_seconds: 10 }  # from stop_grace_period
```

- **Requires `container` ≥ 1.6.1**, where signals reach the container, so
  process-compose's SIGTERM stops the VM within the grace period.
- **Stale containers.** If process-compose is killed hard, the container keeps
  running and its log freezes (#2009). The command removes a leftover
  `<project>-<service>` (`container rm -f`) before `run`.
- **Readiness.** The existing in-container probe commands (`wget`/`curl`,
  `nc -z`, `cmd`, `test -s`) run as process-compose exec probes through
  `container exec`.
- **Status and logs** come from process-compose, whose state vocabulary
  `runtimeOutcome` already understands; logs are the attached process's
  stdout, captured by process-compose.
- **Build.** `Backend.Build` maps the supported subset of `build:`
  (`context`, `dockerfile`, `target`, `args`) to `container build -t
  <project>-<service>`; other keys fail with a clear error.
- **Stack down** deletes the stack's own containers, network and (only on
  explicit request) volumes. It never calls `container system stop`.
- **Cross-runtime ordering.** Local and container services share one
  process-compose graph, so `depends_on` between them becomes real.

### 3. Networking and naming

- Container names, hostnames and aliases are global per user, so every name
  carries the per-root project prefix (`composeProjectName`).
- One `container network create <project>` per stack.
- Service-name DNS: attach with `--network <project>,alias=<service>` and
  `--dns-search` so code that dials `db` keeps working (reported to work on
  1.6.0+, unverified). Fallback: expose `<project>-<service>.<domain>` through
  `${service.*}` substitution.
- The one-time `sudo container system dns create <domain>` belongs to engine
  setup (`angee doctor --fix` or the app's first run), never to `angee up`.
- Published ports always bind `127.0.0.1:` (Apple's default is `0.0.0.0`).
- Container-to-host access (today `host.docker.internal`): use the vmnet
  gateway if reachable (unverified); otherwise the `--localhost` DNS rule,
  re-applied by the setup step after reboot.

### 4. Mounts, volumes, secrets, resources

- `workspace://`, `source://` and `bind://` become virtiofs bind mounts.
  Document the costs (root-owned files, no inotify, slow writes) and recommend
  `runtime: local` for dev servers that hot-reload from a workspace.
- `volume://n` becomes a named volume `<project>-<n>`; data-directory images
  must keep their data in a subdirectory (see Postgres).
- Secrets: `${ANGEE_SECRET_*}` values reach the container through the process
  environment or `--env-file run/secrets.env`; which one is safe is an open
  question below.
- Resources: Apple VMs default to 1 GiB, too small for databases. Add an
  optional `resources: { cpus, memory }` to services and jobs, honoured by both
  engines (Compose `deploy.resources`), with an engine default for unset
  services.

### 5. Jobs and streams

- Shared container jobs become process-compose processes running
  `container run --rm` (the existing local job path).
- Streamed jobs (`RunJobStreams`, the snapshot seam) run
  `container run --rm -i` with stdin and stdout passed through. Whether the
  attached run keeps stderr apart from stdout is unverified and decides whether
  byte-exact stdout holds.

### 6. Edge without `docker.sock`

Run Caddy as a container or local service from the static Caddyfile in §0.3.
Default hostnames under `*.localhost` need no `/etc/resolver` entry; a
configurable domain and a local CA (Caddy's internal CA) cover HTTPS.

### 7. Postgres

No special image is needed. Stack templates (angee-django) that target Apple
`container` use `postgres:18-trixie` or `pgvector/pgvector:pg18-trixie`, a
named volume at `/var/lib/postgresql`, `memory: 2G`, `--shm-size 256m` and
`shared_buffers=512MB`. For the Mac app, offer a native `runtime: local`
Postgres variant (Homebrew or bundled signed binaries) as the default when
Linux parity matters less than memory, I/O and startup.

### 8. Mac app shell (optional, last)

```text
Angee.app  (Developer ID, hardened runtime, notarized, not sandboxed, Sparkle, macOS 26+, arm64)
├─ Contents/MacOS/Angee        SwiftUI MenuBarExtra + windows (or a web view of the operator UI)
├─ Contents/Helpers/angee      Go CLI/operator (re-signed, LC_UUID)
└─ Contents/Library/LaunchAgents/<id>.operator.plist   (BundleProgram → angee operator --socket …)
        │ SMAppService.agent
        ▼
angee operator (REST/GraphQL over a Unix socket; optional 127.0.0.1 + token)
  ├─ engine apple → process-compose → `container run` → com.apple.container.apiserver (Apple-owned)
  └─ runtime local → process-compose
```

First run: find `container` (`/usr/local/bin`, `/opt/homebrew/bin`), require
major 1 and ≥ 1.6.1, offer the pkg or Homebrew if missing, run
`container system start --enable-kernel-install` at login, and perform the
one-time DNS step with an admin prompt.

## Design options

### A. process-compose supervises attached `container run` (recommended)

Reuses the supervisor angee already ships and the event source the live-views
plan already chose. Smallest amount of new runtime code; no polling; one graph
for local and container services. Costs: an operator restart cannot re-attach
to a container process-compose did not start; logs depend on the attached
client staying alive (#2009).

### B. A native backend over `container … --format json`

A new `runtime.Backend` that creates, starts and lists containers directly.
Apple provides no restart, healthcheck, ordering or events, so angee would
re-implement all four and poll `ls` for state. Rejected: a double
implementation of what process-compose owns.

### C. socktainer plus the existing compose backend

Point `DOCKER_HOST` at socktainer and keep the Compose path. Least code, but a
third-party daemon re-implements restart and health (lost on its own restart),
polls for events every 5 s, covers ~75% of the API, and pins one exact Apple
release. Acceptable only as a throwaway prototype.

### D. Embed apple/containerization in our own app

Full control and no user-installed dependency, but rebuilds the apiserver on a
0.x Swift API and adds Swift/Xcode to the build. Revisit only if depending on
the user's `container` install proves unacceptable.

## Open questions (spike on macOS 26 before building)

1. Does an attached `container run` keep stdout and stderr separate?
2. Do `--network <net>,alias=<svc>` plus `--dns-search` give Compose-style
   short names on the current release?
3. Can containers reach host services through the vmnet gateway without the
   `--localhost` pf rule?
4. Does `container run -e KEY` (no value) inherit from the caller's
   environment, so secrets need not touch disk?
5. Does SIGTERM to `container run` stop the container within the grace period,
   and what is left behind after SIGKILL?
6. Does the apiserver come back after a reboot without `system start`?
7. Postgres throughput (pgbench) and memory on Apple `container` against
   Homebrew Postgres on the same Mac.
8. Where the engine choice persists so the CLI and the operator agree.

## Out of scope

- Docker API compatibility, or running Docker-socket tools (caddy-docker-proxy,
  Traefik) on the Apple engine.
- macOS 15 (no container-to-container networking) and Intel Macs.
- Mac App Store distribution.
- GPU and privileged containers (unsupported by Apple's runtime).

## Acceptance

On macOS 26, Apple silicon, with no Docker daemon running and
`ANGEE_CONTAINER_ENGINE=apple`:

- `angee up` starts a stack with Postgres (named volume), one app container
  and one `runtime: local` service; the app reaches Postgres by service name
  and the local service reaches both through published loopback ports.
- A routed service answers through Caddy at `https://<svc>.localhost`.
- Killing the app container's process restarts it per `restart:`; a failing
  readiness probe holds dependants.
- Status and the live view update from process-compose events, without a
  polling loop.
- A streamed job round-trips stdin to stdout byte-exactly.
- `angee down` removes only this stack's containers and network; other
  containers and named volumes survive.
- The same `angee.yaml` still runs unchanged on Docker.

## See also

- [local-platform-instance](./local-platform-instance.md) — the two runtime
  backends behind one interface.
- [edge-ingress-caddy](./edge-ingress-caddy.md) — the label-driven edge this
  proposal gives a static alternative.
- `.agents/plans/owner-pushed-live-views.md` — `Backend.Watch` from
  `process-compose process monitor`.
- `.agents/plans/stack-snapshots.md` — the `RunJobStreams` seam.
- apple/container [docs](https://github.com/apple/container/tree/main/docs):
  technical overview, networking, DNS, volumes, host integration, command
  reference.
