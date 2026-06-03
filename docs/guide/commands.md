# Commands

This page documents the CLI surface implemented in this repository.

Global flags:

```sh
--root string       ANGEE_ROOT containing angee.yaml (default: auto-discover)
--operator string   operator URL for HTTP mode
--json              write JSON output
```

Without `--root`, the CLI walks upward from the current directory, preferring
`angee.yaml`, then `.angee/angee.yaml`. In dev checkouts that expose workspace
templates at `templates/workspaces` or legacy `.templates/workspaces`, it uses
`.angee` so workspace state stays out of the source root.

## Stack

```sh
angee doctor
angee init --dev [path] [--input key=value ...] [--yes] [--force]
angee stack init <template> [path] [--input key=value ...] [--yes] [--force]
angee stack update [--template] [--dry-run]
angee stack destroy [--purge]
angee status
```

`angee init --dev` is shorthand for the `dev` stack template. The template must
be available through the local or remote template resolver.

`angee stack update` regenerates the derived runtime files from `angee.yaml`.
With `--template` it first **re-renders `angee.yaml` from the stack's Copier
template** (so template changes — a new service, job, port, or source — reach an
already-initialized stack), then regenerates. Template-origin sections are
refreshed (the template wins for keys it emits), while user-added keys and
operator-managed state (`operator`, `workspaces`, `port_leases`, and allocated
port values) are preserved. `--dry-run` (with `--template`) prints the changes
without writing. `--template` runs locally and needs the stack's
`.copier-answers.yml`. This brings `stack update` to parity with
`workspace update`, which already re-renders its templates.

## Runtime

```sh
angee build [service...]
angee up [service...] [--build]
angee dev [--build]
angee down
angee start <service>...
angee stop <service>...
angee restart <service>...
angee logs [service...] [--follow]
```

`angee up` starts container services only. `angee dev` starts container services
and local-process services. Runtime actions are routed by each service's
`runtime` value.

## Services

```sh
angee service init <name> [flags]                       # field-based
angee service create --template <ref> --workspace <ws>  # template-based
angee service update <name> [flags]
angee service destroy <name> [--stop=false]
angee service list  # alias: ls
angee service start <service>...
angee service stop <service>...
angee service restart <service>...
angee service logs <name> [--follow]
```

`service init` builds a service from explicit flags (image, command,
env, ports). `service create` renders a Copier template with
`_angee.kind: service` into the stack — useful for bundling agent
runtimes or other reusable service shapes that need a Dockerfile and
multiple inputs. See [Templates](/guide/templates) for the template
contract.

`service init` flags:

```sh
--runtime container|local
--image image
--command arg
--env key=value
--mount uri
--port spec
--workdir uri-or-path
--start
```

`service create` flags:

```sh
--template <ref>      template ref or absolute path (required)
--workspace <name>    target workspace (required)
--input key=value     repeatable; passed to the Copier template
--name <name>         override the resolved service name (default: agent-${workspace.name})
--start               start the service after create
```

If `--runtime` is omitted, `--image` creates a container service and
`--command` creates a local service.

## Jobs

```sh
angee job list  # alias: ls
angee job run <name> [--input key=value ...]
```

`job run` executes the declared job command and writes the job output to stdout.

## Sources

```sh
angee source list  # alias: ls
angee source fetch <name>
angee source status <name>
angee source pull <name>
angee source push <name> [--ref ref]
```

Implemented source materialization is `git` and `local`. `source pull` is
the top-level "update from upstream" operation: it fetches and
fast-forwards the cached source's tracking ref.

The per-source `diff` and per-slot convergence operations (`merge`,
`rebase`, `merge-abort`, `rebase-abort`, `rebase-continue`, `publish`)
do not yet have CLI subcommands — they're reachable via the operator's
REST + GraphQL surfaces (`GET /sources/{name}/diff`,
`POST /workspaces/{name}/sources/{slot}/{merge,rebase,...}` and the
matching `sourceDiff` / `workspaceSource*` GraphQL mutations). See
[Operator API](/reference/operator-api).

## Workspaces

```sh
angee workspace create <name> --template <template> [--ttl duration] [--input key=value ...]
angee workspace update <name> [--ttl duration] [--input key=value ...]
angee workspace list  # alias: ls
angee workspace get <name>
angee workspace status [name]
angee workspace logs <name> [--follow]
angee workspace git <name>
angee workspace push <name> [--ref ref]
angee workspace sync-base [name] [--merge|--rebase]
angee workspace open <name> [--editor vscode|idea|gh-desktop]
angee workspace destroy <name> [--purge]
```

`angee ws` is an alias for `angee workspace`, so `angee ws ls` and
`angee ws status <name>` are equivalent to their long forms.

Workspaces are a **pure file primitive**: `create`/`update` render Copier
templates (including any chained inner-stack templates) and materialize git
or local sources. They do **not** own service lifecycle. If a workspace
renders an inner stack and you want to bring it up, run a stack operation
against the inner root explicitly:

```sh
angee stack up --root workspaces/<name>/.angee
# or point a second operator at it for HTTP/GraphQL access:
angee operator --root workspaces/<name>/.angee --port 9100
```

When run from inside `$ANGEE_ROOT/workspaces/<name>/...`,
`angee workspace status` and `angee workspace sync-base` may omit the name.

For git worktree sources, the branch recorded in the workspace manifest is the
workspace identity. `sync-base` updates that branch from its base ref (normally
`origin/main`) without switching to another branch; push commands refuse
sources whose current branch does not match the manifest branch.
The same contract is exposed through the operator REST and GraphQL APIs:
workspace status includes `sources[].branch`, `sources[].current_ref` /
`currentRef`, `sources[].state`, and top-level `state: discrepancy` when any
source is on the wrong branch. The operator also exposes `POST
/workspaces/{name}/sync-base` and GraphQL `workspaceSyncBase`.

### Update scopes

"Update" has three scopes, all in the same family of git operation:

| Scope | CLI | Meaning |
| --- | --- | --- |
| Whole source | `angee source pull <name>` | Fetch + fast-forward the cached top-level source. |
| One workspace slot | `POST /workspaces/{name}/sources/{slot}/pull` / GraphQL `workspaceSourcePull` | Fast-forward a single workspace slot's worktree from its tracking ref. No CLI subcommand yet. |
| All slots of a workspace | `angee workspace sync-base [name] [--merge\|--rebase]` | Merge or rebase each slot's workspace branch against its declared base ref. Stays on the workspace branch. |

### Per-workspace source slots

Slot-level git operations are reachable as `angee workspace source <op>`:

```sh
angee workspace source fetch <workspace> <slot>
angee workspace source pull <workspace> <slot>
angee workspace source push <workspace> <slot> [--ref ref]
angee workspace source diff <workspace> <slot> [--ref ref]
angee workspace source merge <workspace> <slot> <ref>
angee workspace source rebase <workspace> <slot> <ref>
angee workspace source merge-abort <workspace> <slot>
angee workspace source rebase-abort <workspace> <slot>
angee workspace source rebase-continue <workspace> <slot>
angee workspace source publish <workspace> <slot> [--remote origin] [--branch name]
```

Convergence ops (`merge`/`rebase`/aborts/continue/publish) return a
`GitOpResult{ok, conflicted, conflictFiles, message}` — print as text
or `--json`.

### Workspace preflight

```sh
angee workspace preflight --template <ref> [--input k=v] [--name <name>] [--ttl 1h]
```

Validates the inputs against the resolved template's `_angee.inputs`
declarations without rendering anything. Useful for surfacing
validation failures earlier in a UI.

### GitOps topology

```sh
angee gitops topology [--with-commits N]
```

Prints the cross-source × workspace-slot topology snapshot. Pass
`--with-commits N` to include up to N recent commits per git source.
Subscriptions (`onGitOpsTopologyChange`) remain GraphQL-only — REST
has no native pubsub.

### Template introspection

```sh
angee template list
angee template get <ref>
```

Walks `<root>/.templates/<kind>/<name>` and
`<root>/templates/<kind>/<name>`, listing every discoverable Copier
template plus its input schema.

### Connection tokens

```sh
angee --operator <url> token mint <actor> [--ttl 30m]
```

Mints an HS256 JWT scoped to `<actor>`. Requires an admin-bearer-
authenticated operator URL — the CLI does not access the operator's
JWT signing material locally.

## Secrets

```sh
angee secret list                       # alias: ls
angee secret get <name>                 # metadata only
angee secret reveal <name>              # prints the value
angee secret set <name> --value=v       # or --stdin
angee secret delete <name>
```

`list` returns only declared secrets (entries in `stack.secrets`).
`set`/`delete`/`get` accept any valid name (declared or not). Names must
match `^[A-Za-z0-9._-]{1,256}$`.

The same operations are reachable over REST and GraphQL — see
[Operator API](/reference/operator-api).

## Operator

```sh
angee operator [--root root] [--bind address] [--port port] [--token token]
angee --operator http://127.0.0.1:9000 status
```

Non-loopback binds require `--token`. Remote CLI mode uses the REST operator
API for supported operations.
