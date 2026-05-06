# Usage

Status: target after template refactor  
Date: 2026-05-06

This is the target user guide for `angee` after the Copier/template refactor. It is intended to become the single user-facing usage reference.

This document explains how to run Angee. It does not explain how to author Copier templates.

## Quick Start

Bootstrap local dev from a project directory:

```sh
cd ../django-angee/examples/angee-notes
angee init --dev --yes
angee dev
```

Create a feature workspace with its own sources, services, and port leases:

```sh
angee init --workspace feat-refactor-2 --branch feat-refactor-2 --yes
cd .angee/workspaces/feat-refactor-2/code
angee dev
```

Create an agent-backed workspace:

```sh
angee agent add feat-refactor-2 \
  --branch feat-refactor-2 \
  --template agents/claude-code \
  --workspace-template workspaces/feature-dev \
  --secret anthropic-api-key=env:ANTHROPIC_API_KEY \
  --yes
```

Install a Docker-backed staging stack:

```sh
angee init stack staging-docker \
  --set domain=staging.example.com \
  --secret anthropic-api-key=env:ANTHROPIC_API_KEY \
  --yes
angee up
```

Update the current stack from its active template:

```sh
angee update
```

## Core Terms

Angee uses the same vocabulary locally and on the server-side platform.

| Term | Meaning |
|---|---|
| ANGEE_ROOT | The Angee control directory for one stack. In examples this is usually `.angee`, but it can be any path. |
| Stack | The deployable or runnable system managed by one ANGEE_ROOT. A stack has one active stack template. |
| Stack worktree | The directory that receives stack template output. For project-local roots, this is usually the parent of `./.angee`. |
| Template | A stack, workspace, or agent template, referenced as `stacks/<name>`, `workspaces/<name>`, or `agents/<name>`. |
| Workspace | A filesystem root that contains repositories, volumes, services, jobs, and local state. |
| Repository | A git-tracked source checkout inside a workspace. |
| Volume | Persistent storage attached to a workspace and mountable into services. |
| Service | A long-running workload. A service may run as a local process, Docker container, or future k8s workload, and may expose one or more ports. |
| Port lease | A host port allocated to a service or named endpoint. Leases prevent local dev, workspaces, and agents from colliding. |
| Job | A one-shot execution such as provisioning, migration, fixture loading, build, or doctor checks. Jobs do not expose ports. |
| Agent | A durable actor attached to a workspace, usually backed by an agent template and one or more services. |
| Secret | A template-declared value that Angee generates, derives, or receives from the user. Secret values never go into committed files. |

The CLI understands these generic concepts only. It must not hardcode framework facts such as Django, React, Vite, `manage.py`, uv, pnpm, fixed port names, migration commands, or data directory layouts. Those details belong in templates and the rendered stack manifest.

## Files, Defaults, And Environment

In this guide, `.angee` means the current `$ANGEE_ROOT` directory. If `ANGEE_ROOT=/srv/angee/notes`, then `.angee/stack.yaml` in examples means `/srv/angee/notes/stack.yaml`.

ANGEE_ROOT can live inside a project as `./.angee`, under a user directory such as `~/.angee/notes`, or under a server path such as `/srv/angee/notes`. The path is not required to be named `.angee`; `.angee` is only the default project-local directory name.

### Root Lookup

Commands that need an existing stack resolve ANGEE_ROOT in this order:

1. `--root PATH`.
2. `ANGEE_ROOT`.
3. Parent-walk from the current directory for `.angee/stack.yaml`; the discovered `.angee` directory is ANGEE_ROOT.
4. `~/.angee` if it already contains `stack.yaml`.
5. Fail with a clear message if no stack is found.

`angee init` is different because it can create a root. If no root is supplied and no parent marker exists, it creates `./.angee` by default.

Roots with non-default names are valid, but they are not auto-discovered by parent walk. Select them with `--root` or `ANGEE_ROOT`.

Examples:

```sh
angee --root .angee status
ANGEE_ROOT=/srv/angee/notes angee deploy
angee init stack dev --root /tmp/notes-angee --yes
```

`--root` and `ANGEE_ROOT` point at the ANGEE_ROOT directory itself, not the parent directory.

### Environment Variables

| Variable | Default | Meaning |
|---|---|---|
| `ANGEE_ROOT` | Auto-detected; `./.angee` for new init; `~/.angee` fallback for existing stacks | Path to the Angee control directory. |
| `ANGEE_OPERATOR_URL` | `http://localhost:9000` | Operator URL for operator-backed commands. |
| `ANGEE_API_KEY` | empty | Bearer token for operator API calls. |
| `ANGEE_TEMPLATE_ROOT` | unset | Optional user template library searched before global templates. |

### Files Under ANGEE_ROOT

| Path | Commit? | Meaning |
|---|---:|---|
| `$ANGEE_ROOT/stack.yaml` | yes, for project-local roots | Angee manifest, active template, stack worktree path, workspace defaults, service/job declarations, and local paths. |
| `$ANGEE_ROOT/.env` | no | Stack-local secret values. |
| `$ANGEE_ROOT/state/` | no | Port leases, source state, service/job state, locks, and logs. |
| `$ANGEE_ROOT/data/` | no | Template-declared runtime data and volumes. |
| `$ANGEE_ROOT/workspaces/` | no | Managed workspaces created by `angee init workspace` or `angee init --workspace`. |
| `$ANGEE_ROOT/agents/` | no | Agent-backed workspaces and agent state. |
| `$ANGEE_ROOT/templates/` | optional | Stack-local template library. |
| `$ANGEE_ROOT/docker-compose.yaml` | template-dependent | Generated backend file when a stack uses the compose backend and chooses to write it under ANGEE_ROOT. |

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

Commit `.angee/stack.yaml` when the project uses a project-local root. Do not commit secret files.

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
| Stack | `stacks/<name>` | `angee init stack <name>` |
| Workspace | `workspaces/<name>` | `angee init workspace <workspace> --template workspaces/<name>` |
| Agent | `agents/<name>` | `angee agent add <agent> --template agents/<name>` |

When `--template` is omitted, Angee chooses a logical ref from the command:

| Command | Default template ref |
|---|---|
| `angee init --dev` | `stacks/dev` |
| `angee init stack <name>` | `stacks/<name>` |
| `angee stack switch <name>` | `stacks/<name>` |
| `angee init workspace <workspace>` | `workspaces.default_template` from `$ANGEE_ROOT/stack.yaml` |
| `angee init --workspace <workspace>` | `workspaces.default_template` from `$ANGEE_ROOT/stack.yaml` |
| `angee agent add <agent>` | `agents.default_template` from `$ANGEE_ROOT/stack.yaml`, or require `--template` if no default is declared |

For a logical ref such as `stacks/dev`, Angee looks in this order:

1. Explicit `--template` path or Git ref, if supplied.
2. `templates/stacks/dev` under the current worktree.
3. `templates/stacks/dev` under the stack worktree recorded in `$ANGEE_ROOT/stack.yaml`.
4. `$ANGEE_ROOT/templates/stacks/dev`.
5. `$ANGEE_TEMPLATE_ROOT/stacks/dev`, if `ANGEE_TEMPLATE_ROOT` is set.
6. `~/.angee/templates/stacks/dev`.
7. First-party templates bundled with or installed for Angee.

The same lookup shape applies to `workspaces/<name>` and `agents/<name>`.

Templates whose name starts with `_`, such as `stacks/_base`, are abstract base templates and are not directly initable unless a template explicitly allows it.

## Command Form

```sh
angee [global-options] <command> [arguments] [command-options]
```

Global options:

| Option | Meaning |
|---|---|
| `--root PATH` | Use this ANGEE_ROOT instead of auto-detecting. |
| `--operator URL` | Use this operator URL. Default: `http://localhost:9000`. |
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
| `--skip-post-init` | Render and write state, but skip template-declared post-init jobs. |
| `--keep-failed` | Keep partial workspace or agent files after provisioning fails. |

Secret value forms:

| Form | Meaning |
|---|---|
| `--secret key=value` | Use the literal value. |
| `--secret key=env:VAR` | Read from environment variable `VAR`. |
| `--secret key=file:PATH` | Read from a local file. |

Port lease names are template-defined. Use `--port web=8120` only if the active template declares a `web` port lease.

## `angee init`

Initialize or update a stack or workspace.

```sh
angee init [path] [options]
angee init stack <name> [path] [options]
angee init workspace <workspace> [options]
angee init --workspace <workspace> [options]
angee init --dev [path] [options]
```

`path` is the rendered worktree path when the template renders files outside ANGEE_ROOT. `--root` still controls ANGEE_ROOT.

### Default Init

```sh
angee init --yes
```

Behavior:

1. Resolve or create ANGEE_ROOT.
2. If `$ANGEE_ROOT/stack.yaml` exists, run `angee update` against the active template.
3. If `templates/stacks/dev/` exists in the current worktree, initialize `stacks/dev`.
4. If `templates/stacks/default/` exists, initialize `stacks/default`.
5. Otherwise use the first-party default stack template or ask for `--template`.

Examples:

```sh
angee init --yes
angee init --root /srv/angee/notes --template stacks/dev --yes
angee init --set project_name=notes --secret provider-api-key=env:PROVIDER_API_KEY --yes
```

### Dev Init

```sh
angee init --dev --yes
```

Equivalent to:

```sh
angee init stack dev --yes
```

Use this inside a project directory when you want a complete local dev environment from the `stacks/dev` template.

Examples:

```sh
angee init --dev --yes
angee init --dev --port web=8120 --port ui=5190 --yes
angee init --dev --root /tmp/notes-angee --yes
```

### Stack Init

```sh
angee init stack <name> [path] [options]
```

Examples:

```sh
angee init stack dev --yes
angee init stack staging-docker --set domain=staging.example.com --yes
angee init stack production --template gh:org/templates#templates/stacks/production --ref v1.4.0
```

Behavior:

1. Resolve template ref `stacks/<name>` unless `--template` overrides it.
2. Render the template with Copier.
3. Generate, derive, or load declared secrets.
4. Allocate declared port leases.
5. Materialize declared volumes, repositories, services, and jobs.
6. Write `$ANGEE_ROOT/stack.yaml`.
7. Compile backend files if the template declares a backend such as Docker Compose.
8. Run template-declared post-init jobs unless skipped.

### Workspace Init

```sh
angee init workspace <workspace> [options]
angee init --workspace <workspace> [options]
```

Examples:

```sh
angee init --workspace feat-refactor-2 --branch feat-refactor-2 --yes
angee init workspace feat-refactor-2 --template workspaces/feature-dev --branch feat-refactor-2 --yes
angee init workspace docs-pass --template workspaces/docs --yes
```

Workspace options:

| Option | Meaning |
|---|---|
| `--template REF` | Workspace template. Defaults to `workspaces.default_template` from `$ANGEE_ROOT/stack.yaml`. |
| `--branch REF` | Branch or ref used by repositories that follow the workspace branch. |
| `--override SOURCE=REF` | Override one repository ref. Repeatable. |
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

## `angee update`

Refresh the current stack, workspace, or agent workspace from its recorded template.

```sh
angee update [path] [options]
```

Examples:

```sh
angee update
angee update --ref v4
angee update --set domain=staging.example.com
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
| `--skip-post-init` | Do not run post-update jobs. |

Behavior:

1. Read the target manifest: `$ANGEE_ROOT/stack.yaml` for stacks, or the workspace/agent manifest for managed targets.
2. Resolve the active template.
3. Run Copier update using `.copier-answers.yml`.
4. Preserve existing secrets unless changed explicitly.
5. Preserve existing port leases unless changed explicitly.
6. Recompile generated backend files.
7. Run post-update jobs unless skipped.

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
| `stack validate` | Validate the manifest, template state, secrets, port leases, services, and jobs. |
| `stack templates` | List templates visible to the resolver. |
| `stack switch <name>` | Set the active template to `stacks/<name>` and run `angee update`. |
| `stack set-template <ref>` | Set the active template to an explicit ref and run `angee update`. |

Examples:

```sh
angee stack show
angee stack templates --kind stack
angee stack switch staging-docker --set domain=staging.example.com --yes
angee stack set-template gh:org/templates#templates/stacks/prod --ref v2.0.0
```

## `angee dev`

Run the current stack's local dev services and prerequisite jobs.

```sh
angee dev [options]
```

Options:

| Option | Meaning |
|---|---|
| `--list` | Show declared dev services and jobs, then exit. |
| `--only a,b` | Run only these service or job names. |
| `--except a,b` | Run all declared dev services and jobs except these names. |
| `--ui lines` | Prefix output lines by service/job. Default. |
| `--ui panes` | Run the pane TUI. |

Examples:

```sh
angee dev
angee dev --list
angee dev --only web,ui
angee dev --except worker
angee dev --ui panes
```

`angee dev` reads service names, commands, dependencies, readiness checks, cwd, env, volumes, repositories, and port leases from `$ANGEE_ROOT/stack.yaml`. Service names are template-defined.

## Services

Services are long-running workloads. A service can run through a local process runtime, Docker, or a future k8s runtime. A service may have port leases and mounted repositories or volumes.

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

## Jobs And `angee run`

Jobs are one-shot tasks. Provisioning, migrations, build steps, fixture loading, checks, and user-declared one-off commands are jobs.

```sh
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
angee job logs migrate --follow
```

Top-level aliases such as `angee build`, `angee migrate`, `angee doctor`, and `angee fixtures` may exist as shortcuts. They must dispatch to declared jobs from `$ANGEE_ROOT/stack.yaml`. The CLI should not contain framework-specific implementation for those names.

## Deployment Backend Commands

Use these when the stack renders backend files such as `angee.yaml` or `docker-compose.yaml`.

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
| `status` | Show detailed service, job, and agent status. |
| `ls`, `list`, `ps` | List running services and agents. |
| `logs` | Tail logs from a service, job, agent, or all stack units. |

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
| `workspace show <workspace>` | Show repositories, volumes, services, jobs, port leases, state, and paths. |
| `workspace update <workspace>` | Update workspace from its template and sync repositories. |
| `workspace dev <workspace>` | Run the workspace's local dev services. |
| `workspace logs <workspace>` | Show provisioning, job, or service logs. |
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
angee agent add <agent> [options]
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

`agent add` options:

| Option | Meaning |
|---|---|
| `--template REF` | Agent template. Defaults to `agents.default_template` if declared. |
| `--workspace-template REF` | Workspace template. Defaults to `workspaces.default_template` if declared. |
| `--branch REF` | Branch or ref for workspace repositories. |
| `--override SOURCE=REF` | Override one repository ref. Repeatable. |
| `--create-branches` | Create missing same-name branches. |
| `--secret NAME=VALUE` | Supply agent or workspace secret. |
| `--port NAME=PORT` | Override a template-declared port lease. |
| `--start` | Start agent services after provisioning. |
| `--keep-failed` | Keep partial files after provisioning fails. |
| `--yes` | Non-interactive mode. |

Examples:

```sh
angee agent add feat-refactor-2 \
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
angee init --dev --yes
angee dev
```

### Local Dev With Explicit Root

```sh
angee init --dev --root .angee --yes
angee dev --root .angee
```

### Local Dev With Explicit Ports

```sh
angee init --dev --port web=8120 --port ui=5190 --yes
angee dev
```

### Feature Workspace

```sh
angee init --workspace feat-x --branch feat-x --yes
cd .angee/workspaces/feat-x/code
angee dev
```

### Agent On A Feature Branch

```sh
angee agent add feat-x \
  --template agents/claude-code \
  --workspace-template workspaces/feature-dev \
  --branch feat-x \
  --secret anthropic-api-key=env:ANTHROPIC_API_KEY \
  --start \
  --yes
```

### Staging Stack

```sh
angee init stack staging-docker \
  --set domain=staging.example.com \
  --secret anthropic-api-key=env:ANTHROPIC_API_KEY \
  --yes
angee up
```

### Template Update

```sh
angee update
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
| Post-init job fails | Exit non-zero and keep logs under `$ANGEE_ROOT/state/logs/` or the workspace state dir. |
| Workspace or agent provisioning fails | Exit non-zero and either clean the partial workspace or keep it when `--keep-failed` is set. |
| Service fails to start | Exit non-zero and show the service log location or live log command. |

## What Not To Expect

Do not expect `angee` to know framework-specific defaults. These belong in templates:

| Not hardcoded in CLI | Where it belongs |
|---|---|
| Default app port | Template-declared port leases. |
| Frontend server command | Template-declared service. |
| Migration command | Template-declared job. |
| Data directory layout | Template-declared volumes and data dirs. |
| Agent runtime image | Agent template metadata. |
| Source worktree layout | Workspace template metadata. |
| Docker staging services | Rendered stack service declarations. |

This keeps the Go CLI generic while still allowing one-command setup for concrete projects such as `examples/angee-notes`.
