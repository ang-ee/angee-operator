# Commands

This page documents the CLI surface implemented in this repository.

Global flags:

```sh
--root string       ANGEE_ROOT containing angee.yaml (default: auto-discover)
--operator string   operator URL for HTTP mode
--json              write JSON output
--verbose, -v       increase diagnostic verbosity (repeat as -vv)
--version           print the Angee CLI version
```

`-v` reports active phases and emits periodic heartbeats for slow steps. `-vv`
also traces every subprocess and HTTP call, including its duration. Set
`ANGEE_VERBOSE=0`, `1`, or `2` for the corresponding default, where `0` is the
normal warning-only output, `1` is `-v`, and `2` is `-vv`; an explicitly passed
`--verbose` flag wins over the environment variable.

`-v` no longer prints the version. Use `angee version` or `angee --version`
instead. The version command supports the global JSON mode:

```sh
angee version
angee --json version
```

If a command seems slow or stuck, rerun it with `-v`. Angee names each phase as
it starts and prints a heartbeat every few seconds while a phase is still
running, so you can distinguish slow work from a stalled command; use `-vv` to
identify the specific subprocess or HTTP request.

`-vv` output includes up to 4 KiB of what each subprocess printed. Resolved
secret values and credential-bearing URLs are masked before anything is
written, and control characters are escaped, but review the output before
sharing it, since a tool may print other data you consider sensitive.

## Environment variables

| Variable | Default | Purpose |
|---|---:|---|
| `ANGEE_VERBOSE` | `0` | Default diagnostic verbosity: `0` is warnings only, `1` names phases, and `2` traces commands and requests. |
| `ANGEE_ACCESSIBLE` | unset | Set to `1` to use scripted line prompts for init instead of the interactive form. |
| `ANGEE_GIT_TIMEOUT` | `2m` | Deadline for network git clone, fetch, pull, and push operations. Accepts a Go duration; `0` disables the deadline. |
| `ANGEE_LOCK_TIMEOUT` | `0` | Maximum wait for `run/operator.lock`. Accepts a Go duration; `0` keeps waiting until the caller is cancelled. |
| `ANGEE_OPERATOR_TIMEOUT` | `30m` | Deadline for non-streaming requests to a remote operator. Accepts a Go duration; `0` disables the deadline. Streaming requests are not given this timeout. |

Without `--root`, the CLI walks upward from the current directory, preferring
`angee.yaml`, then `.angee/angee.yaml`. In dev checkouts that expose workspace
templates at `templates/workspaces` or legacy `.templates/workspaces`, it uses
`.angee` so workspace state stays out of the source root.

## Stack

```sh
angee doctor
angee init [path] [--template <ref>] [--input key=value ...] [--yes] [--force]
angee stack init <template> [path] [--input key=value ...] [--yes] [--force]
angee stack update [--template] [--dry-run] [--overwrite]
angee stack destroy [--purge]
angee status
```

`angee init` renders the `dev` stack template by default. `--template` takes a
name (`dev`), a pinned name (`dev@v1.2`), an `owner/repo//subpath` reference,
a URL, or a local path; names resolve from the local template search paths
first and fall back to the template registry (ang-ee/angee-django, or
`ANGEE_TEMPLATE_REGISTRY`).

For named or remote templates, `angee init` and `angee stack init` open a
single-screen, scrollable form in a terminal. Inputs follow `copier.yml` order,
with help under each input, choices shown as lists, and value origins marked as
`default`, `flag`, or `changed`. Secret inputs are masked. Generated, immutable,
and other read-only inputs appear above the final confirmation.

Use Tab or Enter to move forward, Shift+Tab to go back and edit an answer,
↑↓ to choose a list option, and ←→ to toggle Yes/No. Space toggles multiselect
options. The final `Render the template?` confirmation must be Yes to continue.
No or Ctrl+C aborts with `aborted, nothing rendered`, exits with code 130, and
renders nothing. Terminals shorter than 15 lines page through groups of five
fields using the same keys.

After confirmation, a plain summary is printed to stderr before rendering:

```text
inputs for Initialize stack stacks/dev:
  project_name = notes (changed)
  runtime_mode = process (default)
  api_key = ******** (flag)
```

`--input key=value` pre-fills the form; answers are validated against declared
types and choices, including with `--yes`. Multiselect values use JSON arrays,
for example `--input 'features=["api","worker"]'`; an empty selection is `[]`.
When no static choices are available, multiselects use validated JSON text.
`--yes` accepts template defaults without prompts or a form, but requires the
input descriptor to be fetched successfully. Missing required answers are
reported together, one per line, naming the exact `--input key=value` flags.

Piped stdin, `ANGEE_ACCESSIBLE=1`, and `TERM=dumb` use line prompts on stdin,
with help and available choices printed to stderr. Boolean questions accept
`y`/`yes`, `n`/`no`, or `true`/`false`. Invalid types or choices produce a warning
and retry the question, up to three attempts. Secret defaults and entered
secret values are omitted from prompt output. EOF returns an error naming the
input and suggesting `--yes` or `--input key=value`; it never opens the form.
Local path templates (absolute paths or refs containing `..`) retain their
existing direct-render behavior without descriptor validation or prompts.

The init commands bootstrap a new root, so they tolerate a missing operator: if
`ANGEE_OPERATOR_URL` (or `--operator`) is set but the operator is not reachable,
init prints a notice and renders the template locally instead of failing. A
reachable operator still handles init remotely.

`angee stack update` regenerates the derived runtime files from `angee.yaml`.
With `--template` it first re-renders the complete stack template and its chain,
including files such as `AGENTS.md`, then structurally merges `angee.yaml` and
regenerates runtime files. User-added manifest keys and operator-managed state
(`operator`, `workspaces`, `port_leases`, and allocated port values) survive.
Rendered files are tracked: unchanged template files update or delete
automatically, while locally edited or ambiguous legacy files are preserved and
reported as conflicts. Use `--overwrite` to replace conflicts. `--dry-run`
prints all changes without writing. Bare `stack update` remains derived-files
only; template reconciliation needs the stack's `.copier-answers.yml`.

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
angee service create --template <template> --workspace <name> [--name name] [--input key=value ...]
angee service update <name> [field flags]
angee service update <name> --template [--input key=value ...] [--dry-run] [--overwrite]
angee service destroy <name> [--stop=false]
angee service list
```

Field-based `service update` is unchanged. With `--template`, Angee recovers the
service's recorded template, workspace binding, inputs, and port allocations;
re-renders `service.yaml` and build assets; and recursively three-way merges the
service entry. Independent local and template map changes are preserved. A
same-field or asset conflict fails before writing unless `--overwrite` is set.
Service identity, workspace binding, and existing allocations cannot be changed
through template inputs. With `--dry-run`, conflicts are reported without
writing and without turning the preview into a failed apply.

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
multiple inputs. See [Templates](/cli/templates) for the template
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
[Operator API](/operator/api).

## Workspaces

```sh
angee workspace create <name> --template <template> [--ttl duration] [--input key=value ...] [--sync]
angee workspace update <name> [--ttl duration] [--input key=value ...] [--overwrite]
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

`create --sync` reconciles a worktree left behind by an earlier create that
failed after materializing it: it removes that one leftover worktree and
re-adds it, instead of failing with `already exists and is not empty`. It
only reclaims a genuine git worktree at the destination and never touches
sibling worktrees that share the same source cache. (A stale "missing but
already registered" worktree is reclaimed automatically, without `--sync`.)

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
angee template list [--json]
angee template get <ref> [--json]
```

Walks `<root>/.templates/<kind>/<name>` and
`<root>/templates/<kind>/<name>`, listing every discoverable Copier
template plus its input schema.

`template get` accepts `stacks/`, `workspaces/`, and `services/` refs. Its text
output lists inputs in template question order, followed by metadata-only
inputs sorted by name:

```text
ref     stacks/dev
kind    stack
name    dev
path    /abs/path
inputs
  project_name   str  default "app"
      Machine name of the project host; also the chained project's name.
  runtime_mode   str  default "process"  choices: process | docker
      Run framework application services as local processes or Docker containers.
  api_key        str  required  secret
  operator_port  int  generated  read-only
```

Inputs may also show `multiselect`, `immutable`, and a separate `when:` line.
Secret defaults appear as `default set`. Dynamic choices appear as
`choices: <expression>`; `when` and `validator` expressions are informational
and are not evaluated by these prompts. `--json` returns the complete
descriptor, including labels, placeholders, and raw expressions, for clients
building forms.

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
[Operator API](/operator/api).

## Files

Read and write files inside a stack source (e.g. edit a workspace's
`settings.yaml` through the operator rather than by hand):

```sh
angee file get <path> --source <name>                       # prints file content
angee file set <path> --source <name> --content "…"         # write a literal
angee file set <path> --source <name> --file ./local.yaml   # write from a local file
angee file set <path> --source <name> --stdin               # write from stdin
angee file set <path> --source <name> --content "…" --etag <etag>  # compare-and-set
```

`--source` is required; `path` is relative to the source root (traversal and
symlink-escape are rejected) and content is UTF-8 text within a 1 MiB cap.
`get` returns the current `etag`; pass it back to `set --etag` as a
compare-and-set precondition — a stale value is a conflict. The same
operations are reachable over REST (`GET`/`PUT /files`) and GraphQL
(`file` / `fileWrite`) — see [Operator API](/operator/api).

## Operator

```sh
angee operator [--root root] [--bind address] [--port port] [--token token]
angee --operator http://127.0.0.1:9000 status
```

Non-loopback binds require `--token`. Remote CLI mode uses the REST operator
API for supported operations.
