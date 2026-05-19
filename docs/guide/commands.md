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
angee stack update
angee stack destroy [--purge]
angee status
```

`angee init --dev` is shorthand for the `dev` stack template. The template must
be available through the local or remote template resolver.

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
angee service init <name> [flags]
angee service update <name> [flags]
angee service destroy <name> [--stop=false]
angee service list  # alias: ls
angee service start <service>...
angee service stop <service>...
angee service restart <service>...
angee service logs <name> [--follow]
```

Service flags:

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

Implemented source materialization is `git` and `local`.

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

## Operator

```sh
angee operator [--root root] [--bind address] [--port port] [--token token]
angee --operator http://127.0.0.1:9000 status
```

Non-loopback binds require `--token`. Remote CLI mode uses the REST operator
API for supported operations.
