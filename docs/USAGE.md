# Usage

Status: target after template refactor  
Date: 2026-05-06

This is the target user guide for `angee` after the Copier/template refactor. It is intended to become the single user-facing usage reference.

This document explains how to run Angee. It does not explain how to author Copier templates.

## Quick Start

Bootstrap local dev from a project directory:

```sh
cd ../django-angee/examples/angee-notes
angee stack init dev --yes
angee dev
```

Create a feature workspace with its own sources, services, and port leases:

```sh
angee workspace init feat-refactor-2 --branch feat-refactor-2 --yes
cd .angee/workspaces/feat-refactor-2/code
angee dev
```

Create an agent-backed workspace:

```sh
angee agent init feat-refactor-2 \
  --branch feat-refactor-2 \
  --template agents/claude-code \
  --workspace-template workspaces/feature-dev \
  --secret anthropic-api-key=env:ANTHROPIC_API_KEY \
  --yes
```

Install a Docker-backed staging stack:

```sh
angee stack init staging-docker \
  --set domain=staging.example.com \
  --secret anthropic-api-key=env:ANTHROPIC_API_KEY \
  --yes
angee up
```

Update the current stack from its active template:

```sh
angee stack update
```

## Core Terms

Angee uses terms that line up with Docker Compose, Kubernetes, and Temporal where possible. When those systems disagree, Angee keeps the user-facing term that best matches the CLI action.

| Term | Meaning |
|---|---|
| ANGEE_ROOT | The Angee control directory for one stack. In examples this is usually `.angee`, but it can be any path. |
| Stack | The deployable or runnable unit managed by one ANGEE_ROOT. This is closest to a Docker Compose project, a Kubernetes namespace plus release, or a Temporal namespace for the stack's workflows. |
| Manifest | YAML desired state for a stack, workspace, or agent. The operator must be able to reconcile directly from this YAML. |
| Control plane | The API/UI that asks the operator to reconcile. Locally this is usually the CLI. Server-side this can be a Django backend. |
| State source | One place the operator reads or writes observed state, locks, leases, workflow state, and service/job history. The operator can use multiple state sources at the same time, such as files plus API plus database. |
| Stack worktree | The directory that receives stack template output. For project-local roots, this is usually the parent of `./.angee`. |
| Template | A stack, workspace, or agent template, referenced as `stacks/<name>`, `workspaces/<name>`, or `agents/<name>`. |
| Workspace | A filesystem root that contains sources, volumes, services, jobs, workflows, and local state. |
| Source | A named content tree mounted into a workspace, service, or job. A source can come from Git/GitHub, a local path, archive, template output, volume, external URL, Google Drive, S3, or another object store. Sources have a `ref` and a `tree`. |
| Volume | Persistent storage attached to a workspace and mountable into services, jobs, or sources. This maps to Compose volumes and Kubernetes PVCs. |
| Service | A long-running workload, matching Docker Compose `service`. In Kubernetes this usually renders as a workload such as a Deployment or StatefulSet, plus a Kubernetes Service only when a network endpoint is needed. |
| Job | A one-shot workload, matching Kubernetes Job/CronJob semantics and `docker compose run` style execution. Examples: migration, fixture loading, build, doctor checks. |
| Workflow | Durable orchestration of steps, matching Temporal Workflow semantics. Provisioning, update, deploy, rollback, and agent setup can be workflows. |
| Activity | One step inside a workflow, matching Temporal Activity semantics. Examples: render a template, allocate ports, materialize a source, run a job, start a service. |
| Worker | A service that executes workflows or activities from a queue when Temporal is used. Use `Temporal worker` when the meaning could be confused with an application worker service. |
| Task queue | A named queue that Temporal workers poll for workflow and activity work. Local mode can model this as an in-process queue. |
| Operator | The controller that reads a manifest, compares desired state to actual state, and reconciles services, jobs, workflows, port leases, volumes, and deployment backend files. |
| Port lease | A host port allocated to a service or named endpoint. This maps to Compose published ports and Kubernetes Service/Ingress ports. Leases prevent local dev, workspaces, and agents from colliding. |
| Agent | A durable actor attached to a workspace, usually backed by an agent template and one or more services. |
| Secret | A template-declared value that Angee generates, derives, or receives from the user. This maps to Compose secrets/env files and Kubernetes Secrets. Secret values never go into committed files. |

The CLI understands these generic concepts only. It must not hardcode framework facts such as Django, React, Vite, `manage.py`, uv, pnpm, fixed port names, migration commands, or data directory layouts. Those details belong in templates and the rendered stack manifest.

### Vocabulary Map

| Angee | Docker Compose | Kubernetes | Temporal |
|---|---|---|---|
| Stack | Project | Namespace plus release/application | Namespace for related workflows |
| Manifest | `compose.yaml` | YAML resources applied to the API server | Workflow input/config, not the workflow code itself |
| Control plane | Compose CLI | API server plus controllers | Temporal Frontend service plus clients |
| State source | Docker engine state plus local files | etcd plus API resources | Temporal persistence database plus client/API state |
| Service | Service | Workload controller such as Deployment or StatefulSet; optionally a Kubernetes Service for networking | Usually the process that hosts workers or application code |
| Source | Build context, bind mount, named volume content, or downloaded artifact | Volume, projected config, object-store mount, git-sync pattern, or init container output | Workflow input or Activity-produced content |
| Job | One-off `run` container | Job or CronJob | Often an Activity or child Workflow when durable orchestration is needed |
| Workflow | No direct equivalent | Controller/reconciler-style orchestration, not a native workload object | Workflow |
| Activity | No direct equivalent | One reconciliation/provisioning step | Activity |
| Worker | Service/container process | Pod running worker code | Worker polling a task queue |
| Task queue | No direct equivalent | Controller/work queue naming | Task Queue |
| Operator | Compose CLI plus engine reconciliation | Controller/operator pattern | Worker plus workflow execution when durable orchestration is needed |
| Volume | Volume or bind mount | PersistentVolumeClaim | Not a runtime primitive |
| Port lease | Published port | Service, Ingress, or NodePort allocation | Not a runtime primitive |
| Secret | Secret/env file | Secret | Workflow input or external secret reference |

### Source Shape

A source is broader than a repository. It names a content tree and says where that tree comes from.

Minimum fields:

| Field | Meaning |
|---|---|
| `name` | Stable source name used by services, jobs, and templates. |
| `kind` | Source kind such as `git`, `github`, `local`, `archive`, `template`, `volume`, `url`, `s3`, `gcs`, `azure-blob`, or `google-drive`. |
| `ref` | Version selector. For git/github this is a branch, tag, or commit. For URL/object-store sources this can be a version ID, ETag, generation, checksum, timestamp label, or `current`. For volume/local sources this can be a label, snapshot, or empty. |
| `tree` | Subtree inside the source root. For git this is a path inside the repository. For object stores this is a prefix. For Google Drive this is a folder path or folder ID. For volume sources this is a path inside the volume. |
| `target` | Workspace-relative path where the source is materialized or mounted. |

Example:

```yaml
sources:
  app:
    kind: github
    repo: fyltr/angee-notes
    ref: feat-refactor-2
    tree: .
    target: code

  framework:
    kind: github
    repo: fyltr/django-angee
    ref: same-name
    tree: packages/runtime
    target: vendor/django-angee-runtime

  imports:
    kind: volume
    volume: shared-imports
    ref: current
    tree: incoming
    target: data/imports

  demo-assets:
    kind: s3
    bucket: fyltr-demo-assets
    ref: version:2026-05-06
    tree: angee-notes/
    target: data/demo-assets

  design-docs:
    kind: google-drive
    drive: shared
    ref: current
    tree: folders/1AbCdEfGhIjKlMnOp
    target: docs/design

  seed-file:
    kind: url
    url: https://example.com/fixtures/users.json
    ref: sha256:4b7d...
    tree: .
    target: data/fixtures/users.json
```

Source authentication is always through secrets or ambient credentials declared by the stack. Do not put access tokens, signed URLs, service-account JSON, or cloud keys directly in the source definition.

## Files, Defaults, And Environment

In this guide, `.angee` means the current `$ANGEE_ROOT` directory. If `ANGEE_ROOT=/srv/angee/notes`, then `.angee/angee.yaml` in examples means `/srv/angee/notes/angee.yaml`.

ANGEE_ROOT can live inside a project as `./.angee`, under a user directory such as `~/.angee/notes`, or under a server path such as `/srv/angee/notes`. The path is not required to be named `.angee`; `.angee` is only the default project-local directory name.

### Root Lookup

Commands that need an existing stack resolve ANGEE_ROOT in this order:

1. `--root PATH`.
2. `ANGEE_ROOT`.
3. Parent-walk from the current directory for `.angee/angee.yaml`; the discovered `.angee` directory is ANGEE_ROOT.
4. `~/.angee` if it already contains `angee.yaml`.
5. Fail with a clear message if no stack is found.

`angee init` is different because it can create a root. If no root is supplied and no parent marker exists, it creates `./.angee` by default.

Roots with non-default names are valid, but they are not auto-discovered by parent walk. Select them with `--root` or `ANGEE_ROOT`.

Examples:

```sh
angee --root .angee status
ANGEE_ROOT=/srv/angee/notes angee deploy
angee stack init dev --root /tmp/notes-angee --yes
```

`--root` and `ANGEE_ROOT` point at the ANGEE_ROOT directory itself, not the parent directory.

### Environment Variables

| Variable | Default | Meaning |
|---|---|---|
| `ANGEE_ROOT` | Auto-detected; `./.angee` for new init; `~/.angee` fallback for existing stacks | Path to the Angee control directory. |
| `ANGEE_OPERATOR_URL` | unset | Explicit operator service URL for remote-backed commands. |
| `ANGEE_API_KEY` | empty | Bearer token for operator API calls. |
| `ANGEE_TEMPLATE_ROOT` | unset | Optional user template library searched before global templates. |
| `ANGEE_STATE_SOURCES` | `file` | Comma-separated operator state sources, such as `file`, `file,django-api`, or `django-api,django-db`. |
| `ANGEE_DJANGO_URL` | unset | Django backend URL when the operator is controlled through Django APIs. |
| `ANGEE_DATABASE_URL` | unset | Django database connection string when the operator uses direct DB state. |

### Files Under ANGEE_ROOT

| Path | Commit? | Meaning |
|---|---:|---|
| `$ANGEE_ROOT/angee.yaml` | yes, for project-local roots | Angee manifest, active template, stack worktree path, workspace defaults, service/job/workflow declarations, and local paths. |
| `$ANGEE_ROOT/.env` | no | Stack-local secret values. |
| `$ANGEE_ROOT/state/` | no | Port leases, source state, service/job/workflow state, locks, and logs. |
| `$ANGEE_ROOT/data/` | no | Template-declared runtime data and volumes. |
| `$ANGEE_ROOT/workspaces/` | no | Managed workspaces created by `angee workspace init`. |
| `$ANGEE_ROOT/agents/` | no | Agent-backed workspaces and agent state. |
| `$ANGEE_ROOT/templates/` | optional | Stack-local template library. |
| `$ANGEE_ROOT/docker-compose.yaml`, `$ANGEE_ROOT/k8s/`, or other backend files | template-dependent | Generated deployment backend files when a stack uses Compose, Kubernetes, or another backend. |

Copier answers are written next to the rendered template output as `.copier-answers.yml`. Commit that file when the rendered output is committed. Secret values are not written there.

### Git Ignore Defaults

For project-local roots, templates should normally commit only safe Angee metadata and ignore runtime state:

```gitignore
.angee/.env
.angee/state/
.angee/data/
.angee/workspaces/
.angee/agents/
```

Commit `.angee/angee.yaml` when the project uses a project-local root. Do not commit secret files.

## Template Resolution

Template references have three forms:

| Form | Example | Meaning |
|---|---|---|
| Logical ref | `stacks/dev` | Resolve by kind and name through Angee's template search path. |
| Local path | `./templates/stacks/dev` | Use this local template directory directly. |
| Git ref | `https://github.com/org/repo#templates/stacks/dev` | Fetch the repo and use the subdirectory. |

Template kinds:

| Kind | Logical path | Typical command |
|---|---|---|
| Stack | `stacks/<name>` | `angee stack init <name>` |
| Workspace | `workspaces/<name>` | `angee workspace init <workspace> --template workspaces/<name>` |
| Agent | `agents/<name>` | `angee agent init <agent> --template agents/<name>` |

When `--template` is omitted, Angee chooses a logical ref from the command:

| Command | Default template ref |
|---|---|
| `angee init` | Shorthand for `angee stack init dev` when `stacks/dev` exists, otherwise `angee stack init default`. |
| `angee stack init <name>` | `stacks/<name>` |
| `angee stack switch <name>` | `stacks/<name>` |
| `angee workspace init <workspace>` | `workspaces.default_template` from `$ANGEE_ROOT/angee.yaml` |
| `angee agent init <agent>` | `agents.default_template` from `$ANGEE_ROOT/angee.yaml`, or require `--template` if no default is declared |

For a logical ref such as `stacks/dev`, Angee looks in this order:

1. Explicit `--template` path or Git ref, if supplied.
2. `templates/stacks/dev` under the current worktree.
3. `templates/stacks/dev` under the stack worktree recorded in `$ANGEE_ROOT/angee.yaml`.
4. `$ANGEE_ROOT/templates/stacks/dev`.
5. `$ANGEE_TEMPLATE_ROOT/stacks/dev`, if `ANGEE_TEMPLATE_ROOT` is set.
6. `~/.angee/templates/stacks/dev`.
7. First-party templates bundled with or installed for Angee.

The same lookup shape applies to `workspaces/<name>` and `agents/<name>`.

Templates whose name starts with `_`, such as `stacks/_base`, are abstract base templates and are not directly initable unless a template explicitly allows it.

## Manifest And Operator Model

Angee follows an operator model, closer to Kubernetes than to a one-shot script.

The manifest is YAML desired state. By default this is:

```text
$ANGEE_ROOT/angee.yaml
```

The operator must be able to work directly from that YAML file. Templates are only one way to create or update the manifest; once the manifest exists, the operator reconciles from YAML.

Operator responsibilities:

| Responsibility | Meaning |
|---|---|
| Load manifest | Read `$ANGEE_ROOT/angee.yaml` or an explicit `--file PATH`. |
| Provision | Run the same template/source/secret/port pipeline for stack init, workspace init, agent init, service provisioning, and MCP server provisioning. |
| Reconcile | Compare desired sources, volumes, services, jobs, workflows, secrets, and port leases to actual state. |
| Compile | Produce deployment backend files such as `docker-compose.yaml` or Kubernetes manifests when needed. |
| Apply | Materialize sources, mount volumes, and start, stop, update, or remove services and jobs through the selected backend. |
| Orchestrate | Run workflows inline locally, or delegate durable workflows to Temporal when configured. |
| Observe | Stream service, job, workflow, and agent status and logs back to the CLI. |

Operator process forms:

```sh
angee operator --root .angee
angee operator --file .angee/angee.yaml
angee operator --file ./angee.yaml --root /tmp/angee
```

`--file` points at the YAML manifest. `--root` points at the state directory used for secrets, logs, leases, generated files, and runtime state.

Most users do not run `angee operator` directly. Commands such as `angee init`, `angee stack init`, `angee workspace init`, `angee agent init`, `angee dev`, `angee up`, and `angee deploy` dispatch to the in-process operator runtime. `--operator` or `ANGEE_OPERATOR_URL` explicitly selects a running operator service.

There is one provisioning path. The CLI does not have a separate implementation for stack, workspace, or agent initialization. It has the operator compiled in, dispatches through shared API request/response types without opening ports, and reuses the same provisioning code that HTTP, MCP, and a Django control plane can call.

### Control Plane And State Sources

The operator has separate desired-state and observed-state inputs. State sources are composable: local files, Django API, and direct database access can be used together.

| Axis | Local mode | Server-side mode |
|---|---|---|
| Control plane | CLI dispatches to the in-process operator runtime. | Django backend controls the operator through API calls, queued workflows, or direct deployment integration. |
| Desired state | `$ANGEE_ROOT/angee.yaml` or `--file PATH`. | Django DB rows can be the desired state, or Django can render/export YAML for the operator. |
| State sources | Usually files under `$ANGEE_ROOT/state/`. | Any combination of Django API, Django database, file cache, and Temporal persistence for durable workflows. |

Supported operator state source forms:

```sh
angee operator --file .angee/angee.yaml --state-source file
angee operator --file .angee/angee.yaml --state-source file --state-source django-api --django-url https://app.example.com
angee operator --state-source django-api --state-source django-db --django-url https://app.example.com --database-url "$DATABASE_URL"
```

`file` means the operator reads/writes local state under `$ANGEE_ROOT/state/`. `django-api` means the operator talks to the Django backend over its API. `django-db` means the operator is colocated with the Django deployment and can read/write the Django database directly for workspace, source, volume, service, job, workflow, and port lease state.

Direct DB state is an optimization and deployment choice, not a different user model. The same resources still exist: sources, volumes, services, jobs, workflows, agents, secrets, and port leases. If multiple state sources are configured, the operator reconciles them according to the manifest's source priority and write policy.

## Command Form

```sh
angee [global-options] <command> [arguments] [command-options]
```

Global options:

| Option | Meaning |
|---|---|
| `--root PATH` | Use this ANGEE_ROOT instead of auto-detecting. |
| `--operator URL` | Use an explicitly running remote operator URL. |
| `--api-key KEY` | Bearer token for operator API calls. |
| `--json` | Print machine-readable JSON when supported. |
| `--help` | Show help. |
| `--version` | Show CLI version. |

## Common Options

These options are reused by init, update, workspace, and agent commands.

| Option | Meaning |
|---|---|
| `--template REF` | Template ref, local path, or Git ref. |
| `--ref REF` | Template Git ref, tag, branch, or commit. |
| `--set KEY=VALUE` | Set a template answer. Repeatable. |
| `--secret NAME=VALUE` | Supply a secret. Repeatable. |
| `--port NAME=PORT` | Reserve a numeric host port for a template-declared port lease. Repeatable. |
| `--yes`, `-y` | Non-interactive mode. Use defaults and fail on missing required values. |
| `--dry-run` | Show what would happen without writing files or starting services. |
| `--skip-post-init` | Render and write state, but skip the template-declared post-init workflow. |
| `--keep-failed` | Keep partial workspace or agent files after provisioning fails. |

Secret value forms:

| Form | Meaning |
|---|---|
| `--secret key=value` | Use the literal value. |
| `--secret key=env:VAR` | Read from environment variable `VAR`. |
| `--secret key=file:PATH` | Read from a local file. |

Port lease names are template-defined. Use `--port web=8120` only if the active template declares a `web` port lease.

Port leases should be self-contained declarations, not separate Copier questions for every port. The scalable pattern is:

```yaml
port_leases:
  web:
    default: 8100
    band: django
    export_env: DJANGO_PORT
  ui:
    default: 5173
    band: ui
    export_env: UI_PORT

services:
  web:
    ports: [web]
  ui:
    ports: [ui]
```

Users override the named lease with `--port web=8120`. Templates and services refer to `web`, not to a separate answer such as `web_port`.

## Init Commands

Initialization is noun-first. Use `stack init`, `workspace init`, or `agent init` depending on what you are provisioning.

`angee init` is a convenience shortcut for the default stack init. It is not a separate mode.

```sh
angee init [path] [options]
angee stack init <name> [path] [options]
angee workspace init <workspace> [options]
angee agent init <agent> [options]
```

`path` is the rendered stack worktree path when the template renders files outside ANGEE_ROOT. `--root` still controls ANGEE_ROOT.

All init commands go through the operator provisioning path in-process by default. They contact a service only when `--operator` or `ANGEE_OPERATOR_URL` is set.

### `angee init`

```sh
angee init --yes
```

Behavior:

1. Resolve or create ANGEE_ROOT.
2. If `$ANGEE_ROOT/angee.yaml` exists, run `angee stack update` against the active template.
3. If `templates/stacks/dev/` exists in the current worktree, run `angee stack init dev`.
4. If `templates/stacks/default/` exists, run `angee stack init default`.
5. Otherwise use the first-party default stack template or ask for `--template`.

Examples:

```sh
angee init --yes
angee init --root /srv/angee/notes --template stacks/dev --yes
angee init --set project_name=notes --secret provider-api-key=env:PROVIDER_API_KEY --yes
```

### `angee stack init`

```sh
angee stack init <name> [path] [options]
```

Use `dev` inside a project directory when you want a complete local dev environment from the `stacks/dev` template.

Examples:

```sh
angee stack init dev --yes
angee stack init dev --port web=8120 --port ui=5190 --yes
angee stack init staging-docker --set domain=staging.example.com --yes
angee stack init production --template gh:org/templates#templates/stacks/production --ref v1.4.0
```

Behavior:

1. Resolve template ref `stacks/<name>` unless `--template` overrides it.
2. Ask the operator to render the template with Copier.
3. Generate, derive, or load declared secrets.
4. Allocate declared port leases.
5. Materialize declared sources, volumes, services, jobs, workflows, MCP servers, and agents.
6. Write `$ANGEE_ROOT/angee.yaml`.
7. Compile backend files if the template declares a backend such as Docker Compose.
8. Run the template-declared post-init workflow unless skipped.

### `angee workspace init`

```sh
angee workspace init <workspace> [options]
```

Examples:

```sh
angee workspace init feat-refactor-2 --branch feat-refactor-2 --yes
angee workspace init feat-refactor-2 --template workspaces/feature-dev --branch feat-refactor-2 --yes
angee workspace init docs-pass --template workspaces/docs --yes
```

Workspace options:

| Option | Meaning |
|---|---|
| `--template REF` | Workspace template. Defaults to `workspaces.default_template` from `$ANGEE_ROOT/angee.yaml`. |
| `--branch REF` | Branch or ref used by sources that follow the workspace branch. |
| `--override SOURCE=REF` | Override one source ref. Repeatable. |
| `--create-branches` | Create missing same-name branches from their default refs. |
| `--agent-template REF` | Also render an agent template and place output under `$ANGEE_ROOT/agents/<workspace>/`. |
| `--port NAME=PORT` | Override a template-declared port lease. |
| `--secret NAME=VALUE` | Supply workspace or agent secret. |
| `--start` | Start declared services after provisioning. |
| `--no-start` | Provision only. This is the default unless the template says otherwise. |
| `--keep-failed` | Keep partial files after provisioning fails. |
| `--yes` | Non-interactive mode. |

Default output without an agent template:

```text
$ANGEE_ROOT/workspaces/<workspace>/
```

Default output with an agent template:

```text
$ANGEE_ROOT/agents/<workspace>/
```

### `angee agent init`

```sh
angee agent init <agent> [options]
```

Agent init provisions an agent-backed workspace. It may also provision a workspace template first, then render the agent template into that workspace.

Examples:

```sh
angee agent init feat-refactor-2 \
  --template agents/claude-code \
  --workspace-template workspaces/feature-dev \
  --branch feat-refactor-2 \
  --secret anthropic-api-key=env:ANTHROPIC_API_KEY \
  --yes
```

## Update Commands

Refresh the current stack, workspace, or agent workspace from its recorded template.

```sh
angee stack update [options]
angee workspace update <workspace> [options]
angee agent update <agent> [options]
```

Examples:

```sh
angee stack update
angee stack update --ref v4
angee stack update --set domain=staging.example.com
angee workspace update feat-refactor-2 --ref v4
angee agent update feat-refactor-2 --ref v4
```

Options:

| Option | Meaning |
|---|---|
| `--ref REF` | Update to a specific template ref. |
| `--set KEY=VALUE` | Change or supply an answer during update. |
| `--secret NAME=VALUE` | Add or replace a secret. |
| `--port NAME=PORT` | Change a port lease when allowed. |
| `--conflict inline` | Put conflicts inline in files. Default. |
| `--conflict rej` | Write rejected hunks to `.rej` files. |
| `--yes` | Use existing answers and defaults without prompting. |
| `--dry-run` | Preview update without changing files. |
| `--skip-post-init` | Do not run the post-update workflow. |

Behavior:

1. Read the target manifest: `$ANGEE_ROOT/angee.yaml` for stacks, or the workspace/agent manifest for managed targets.
2. Resolve the active template.
3. Run Copier update using `.copier-answers.yml`.
4. Preserve existing secrets unless changed explicitly.
5. Preserve existing port leases unless changed explicitly.
6. Recompile generated backend files.
7. Run the post-update workflow unless skipped.

## `angee stack`

Inspect and change the active stack template.

```sh
angee stack show
angee stack validate
angee stack templates [--kind stack|workspace|agent]
angee stack switch <name> [options]
angee stack set-template <ref> [options]
```

Commands:

| Command | Meaning |
|---|---|
| `stack show` | Print the resolved stack manifest. |
| `stack validate` | Validate the manifest, template state, secrets, port leases, services, jobs, and workflows. |
| `stack templates` | List templates visible to the resolver. |
| `stack switch <name>` | Set the active template to `stacks/<name>` and run `angee stack update`. |
| `stack set-template <ref>` | Set the active template to an explicit ref and run `angee stack update`. |

Examples:

```sh
angee stack show
angee stack templates --kind stack
angee stack switch staging-docker --set domain=staging.example.com --yes
angee stack set-template gh:org/templates#templates/stacks/prod --ref v2.0.0
```

## `angee dev`

Run the current stack's local dev services, prerequisite jobs, and optional dev workflow.

`angee dev` runs the operator runtime for the selected ANGEE_ROOT in the foreground. The CLI is the terminal UI and lifecycle owner; the operator runtime is the reconciler. This keeps dev mode on the same path as `up`, `deploy`, server-side Django control, and future Kubernetes/Temporal execution without leaving a background daemon behind.

```sh
angee dev [options]
```

Options:

| Option | Meaning |
|---|---|
| `--list` | Show declared dev services, jobs, and workflow steps, then exit. |
| `--only a,b` | Run only these service, job, or workflow step names. |
| `--except a,b` | Run all declared dev services, jobs, and workflow steps except these names. |
| `--ui lines` | Prefix output lines by service, job, or workflow step. Default. |
| `--ui panes` | Run the pane TUI. |

Examples:

```sh
angee dev
angee dev --list
angee dev --only web,ui
angee dev --except worker
angee dev --ui panes
```

The ad-hoc operator reads service names, commands, dependencies, readiness checks, cwd, env, sources, volumes, workflows, jobs, and port leases from `$ANGEE_ROOT/angee.yaml`. Service names are template-defined.

## Sources

Sources are named content trees. A source can be checked out, copied, generated, mounted from a volume, or refreshed from an external location.

```sh
angee source list
angee source show <source>
angee source sync <source>
angee source sync --all
```

Commands:

| Command | Meaning |
|---|---|
| `source list` | List sources declared by the current stack or workspace. |
| `source show <source>` | Show source kind, ref, tree, target path, and current materialized state. |
| `source sync <source>` | Reconcile one source to its declared `ref` and `tree`. |
| `source sync --all` | Reconcile all sources. |

Examples:

```sh
angee source list
angee source show app
angee source sync app
angee source sync --all
```

## Services

Services are long-running workloads. A service can run as a local process, Docker container, or future Kubernetes workload. A service may have port leases and mounted sources or volumes.

```sh
angee service list
angee service show <service>
angee service start <service>
angee service stop <service>
angee service restart <service>
angee service logs <service> [options]
```

Options:

| Option | Command | Meaning |
|---|---|---|
| `-f`, `--follow` | `service logs` | Follow logs. |
| `-n`, `--lines N` | `service logs` | Number of lines to show. Default: `100`. |

Examples:

```sh
angee service list
angee service show web
angee service restart worker
angee service logs web --follow
```

Stack-level shortcuts:

```sh
angee ls
angee logs [service]
angee up [service...]
angee down [service...]
angee restart [service...]
```

## Workflows, Jobs, And `angee run`

Workflows are durable orchestrations made of activities. Locally, a workflow may run inline. On a server-side platform, a workflow may be backed by Temporal.

Jobs are one-shot workloads. Migrations, build steps, fixture loading, checks, and user-declared one-off commands are jobs. A workflow activity may run a job, but not every job needs a durable workflow.

```sh
angee workflow list
angee workflow show <workflow-id-or-name>
angee workflow logs <workflow-id-or-name> [options]
angee workflow cancel <workflow-id>
angee job list
angee job show <job-id-or-name>
angee job run <name> [-- args...]
angee job logs <job-id-or-name> [options]
angee job cancel <job-id>
angee run <name> [-- args...]
angee run --list
```

`angee run <name>` is the short form for running a named job declared by the stack.

Examples:

```sh
angee run build
angee run migrate
angee run fixtures -- load
angee job run doctor
angee workflow show deploy
angee job logs migrate --follow
```

Top-level aliases such as `angee build`, `angee migrate`, `angee doctor`, and `angee fixtures` may exist as shortcuts. They must dispatch to declared jobs from `$ANGEE_ROOT/angee.yaml`. The CLI should not contain framework-specific implementation for those names.

## Deployment Backend Commands

Use these when the stack renders deployment backend files such as `docker-compose.yaml`, Kubernetes manifests, or Angee intermediate files compiled into one of those backends.

```sh
angee compile
angee up [service...]
angee down [service...]
angee restart [service...]
angee pull [service...]
```

Commands:

| Command | Meaning |
|---|---|
| `compile` | Compile stack inputs into deployment backend files. |
| `up` | Compile and start stack services. |
| `down` | Stop stack services. |
| `restart` | Recompile, stop, and start services again. |
| `pull` | Pull container images without restarting services. |

Examples:

```sh
angee compile
angee up
angee up web worker
angee pull && angee restart
angee down
```

Users should not need to run backend tools such as `docker compose` directly for normal Angee operations.

## Operator Commands

Use these when an operator is running for the stack.

```sh
angee plan
angee deploy [-m MESSAGE]
angee rollback <sha|HEAD~N>
angee status
angee ls
angee list
angee ps
angee logs [service-or-agent] [options]
```

Commands:

| Command | Meaning |
|---|---|
| `plan` | Preview what `deploy` would change. |
| `deploy` | Ask the operator to compile and apply the stack. |
| `rollback` | Roll back to a previous stack commit and redeploy. |
| `status` | Show detailed service, job, workflow, and agent status. |
| `ls`, `list`, `ps` | List running services and agents. |
| `logs` | Tail logs from a service, job, workflow, agent, or all stack units. |

Options:

| Option | Command | Meaning |
|---|---|---|
| `-m`, `--message TEXT` | `deploy` | Commit/deploy message. |
| `-f`, `--follow` | `logs` | Follow logs. |
| `-n`, `--lines N` | `logs` | Number of lines to show. Default: `100`. |

Examples:

```sh
angee plan
angee deploy -m "enable staging worker"
angee status
angee logs web --follow
angee rollback HEAD~1
```

## Workspace Commands

Manage existing workspaces.

```sh
angee workspace list
angee workspace show <workspace>
angee workspace update <workspace> [options]
angee workspace dev <workspace> [dev-options]
angee workspace logs <workspace> [options]
angee workspace destroy <workspace> [--force]
```

Commands:

| Command | Meaning |
|---|---|
| `workspace list` | List managed workspaces. |
| `workspace show <workspace>` | Show sources, volumes, services, jobs, workflows, port leases, state, and paths. |
| `workspace update <workspace>` | Update workspace from its template and sync sources. |
| `workspace dev <workspace>` | Run the workspace's local dev services. |
| `workspace logs <workspace>` | Show workflow, job, or service logs. |
| `workspace destroy <workspace>` | Stop services, release port leases, and remove workspace files. |

Examples:

```sh
angee workspace list
angee workspace show feat-refactor-2
angee workspace dev feat-refactor-2 --only web,ui
angee workspace update feat-refactor-2 --ref v4
angee workspace destroy feat-refactor-2 --force
```

## Agent Commands

Manage agent-backed workspaces.

```sh
angee agent init <agent> [options]
angee agent list
angee agent show <agent>
angee agent start <agent>
angee agent stop <agent>
angee agent restart <agent>
angee agent logs <agent> [options]
angee agent chat <agent>
angee agent ask <agent> <message>
angee agent update <agent> [options]
angee agent destroy <agent> [--force]
```

`agent init` options:

| Option | Meaning |
|---|---|
| `--template REF` | Agent template. Defaults to `agents.default_template` if declared. |
| `--workspace-template REF` | Workspace template. Defaults to `workspaces.default_template` if declared. |
| `--branch REF` | Branch or ref for workspace sources. |
| `--override SOURCE=REF` | Override one source ref. Repeatable. |
| `--create-branches` | Create missing same-name branches. |
| `--secret NAME=VALUE` | Supply agent or workspace secret. |
| `--port NAME=PORT` | Override a template-declared port lease. |
| `--start` | Start agent services after provisioning. |
| `--keep-failed` | Keep partial files after provisioning fails. |
| `--yes` | Non-interactive mode. |

Examples:

```sh
angee agent init feat-refactor-2 \
  --template agents/claude-code \
  --workspace-template workspaces/feature-dev \
  --branch feat-refactor-2 \
  --secret anthropic-api-key=env:ANTHROPIC_API_KEY \
  --yes

angee agent start feat-refactor-2
angee agent chat feat-refactor-2
angee agent ask feat-refactor-2 "summarize the current branch"
angee agent logs feat-refactor-2 --follow
```

Agent names are user-defined. The CLI should not assume `admin`, `developer`, or any other agent exists unless the stack declares it.

## Destroy And Cleanup

```sh
angee destroy [--force]
angee workspace destroy <workspace> [--force]
angee agent destroy <agent> [--force]
angee gc
```

Commands:

| Command | Meaning |
|---|---|
| `destroy` | Destroy the current ANGEE_ROOT after confirmation. |
| `workspace destroy` | Stop services, remove one workspace, and release port leases. |
| `agent destroy` | Stop services, remove one agent workspace, and release port leases. |
| `gc` | Clean stale leases, dead state entries, orphaned temp dirs, and old logs. |

Use `--force` only in scripts or when confirmation is impossible.

## Common Workflows

### Local Dev

```sh
angee stack init dev --yes
angee dev
```

### Local Dev With Explicit Root

```sh
angee stack init dev --root .angee --yes
angee dev --root .angee
```

### Local Dev With Explicit Ports

```sh
angee stack init dev --port web=8120 --port ui=5190 --yes
angee dev
```

### Feature Workspace

```sh
angee workspace init feat-x --branch feat-x --yes
cd .angee/workspaces/feat-x/code
angee dev
```

### Agent On A Feature Branch

```sh
angee agent init feat-x \
  --template agents/claude-code \
  --workspace-template workspaces/feature-dev \
  --branch feat-x \
  --secret anthropic-api-key=env:ANTHROPIC_API_KEY \
  --start \
  --yes
```

### Staging Stack

```sh
angee stack init staging-docker \
  --set domain=staging.example.com \
  --secret anthropic-api-key=env:ANTHROPIC_API_KEY \
  --yes
angee up
```

### Template Update

```sh
angee stack update
```

### Switch Stack Target

```sh
angee stack switch staging-docker --set domain=staging.example.com --yes
```

## Exit Behavior

Commands should fail loudly and leave enough state for recovery.

| Situation | Behavior |
|---|---|
| Missing required secret with `--yes` | Exit non-zero and name the missing secret. |
| Port already leased or unavailable | Exit non-zero and name the port plus owner if known. |
| Template update conflict | Exit non-zero after writing Copier conflict markers or `.rej` files. |
| Workflow, activity, or job fails | Exit non-zero and keep logs under `$ANGEE_ROOT/state/logs/` or the workspace state dir. |
| Workspace or agent provisioning fails | Exit non-zero and either clean the partial workspace or keep it when `--keep-failed` is set. |
| Service fails to start | Exit non-zero and show the service log location or live log command. |

## What Not To Expect

Do not expect `angee` to know framework-specific defaults. These belong in templates:

| Not hardcoded in CLI | Where it belongs |
|---|---|
| Default app port | Template-declared port leases. |
| Frontend server command | Template-declared service. |
| Migration command | Template-declared job or workflow activity. |
| Data directory layout | Template-declared volumes and data dirs. |
| Agent runtime image | Agent template metadata. |
| Source tree layout | Workspace template metadata. |
| Docker staging services | Rendered stack service declarations. |

This keeps the Go CLI generic while still allowing one-command setup for concrete projects such as `examples/angee-notes`.
