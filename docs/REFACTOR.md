# Refactor Plan

Status: target refactor plan
Date: 2026-05-06

This branch is a fresh refactor. There is no backward-compatibility requirement for the old `.angee-template.yaml`, `.angee/project.yaml`, project-mode dispatcher, or split init paths. The goal is the smallest clean architecture with zero intentional tech debt.

## Target Shape

Angee has one stack manifest:

```text
$ANGEE_ROOT/angee.yaml
```

`angee.yaml` is the Angee-owned desired-state manifest. It declares the active template, sources, volumes, secrets, port leases, services, jobs, workflows, workspaces, agents, MCP servers, and deployment backend settings.

Copier answers stay in `.copier-answers.yml`. Secret values stay out of committed files.

## Command Grammar

Use noun-first lifecycle commands:

```sh
angee stack init <name> [path]
angee stack update
angee workspace init <name>
angee workspace update <name>
angee agent init <name>
angee agent update <name>
```

`angee init` is only a shorthand for the default stack init. In a project with `templates/stacks/dev`, it resolves to:

```sh
angee stack init dev
```

There is no separate flag-based init path in the target design.

## Operator-Owned Provisioning

The operator owns the provisioning pipeline. The CLI is a user interface and transport client.

All of these entry points must call the same operator provisioning code:

| Entry point | What it does |
|---|---|
| `angee stack init` | Render a stack template and create/update `$ANGEE_ROOT/angee.yaml`. |
| `angee workspace init` | Provision a workspace template, sources, port leases, services, jobs, and state. |
| `angee agent init` | Provision an agent-backed workspace and render agent config/instructions. |
| `angee dev` | Runs the in-process operator runtime for the lifetime of the dev command and reconciles the dev stack. |
| HTTP API | Lets dashboards, CI, or apps call the same provisioning/reconcile path. |
| MCP API | Lets agents provision and operate resources through the same operator path. |
| Django backend | Can control the operator by API or colocated DB access, but should not duplicate provisioning logic. |

This avoids a CLI-only init implementation and keeps local dev, staging, server-side platform provisioning, and future Kubernetes behavior aligned.

## Template System

Use Copier for rendering and update mechanics.

Template layout:

```text
templates/
  stacks/<name>/
  workspaces/<name>/
  agents/<name>/
```

Template refs:

```text
stacks/dev
stacks/staging-docker
workspaces/feature-dev
agents/angee-developer
agents/personal-assistant
gh:org/repo#templates/stacks/prod
./templates/stacks/dev
```

Angee owns template resolution. Copier owns file rendering and three-way updates.

## Operator Pipeline

The shared pipeline is:

```text
parse command or API request
dispatch to in-process operator runtime or explicitly configured remote operator
resolve template ref
load _angee metadata
render/update with Copier
write or update angee.yaml
resolve sources
resolve secrets
allocate port leases
materialize volumes and state dirs
provision services, jobs, workflows, agents, MCP servers
compile deployment backend files if needed
reconcile actual state
print or stream status/logs
```

Sources are broader than repositories. A source has `kind`, `ref`, `tree`, and `target`, and can come from GitHub/git, local paths, volumes, archives, URLs, S3, GCS, Azure Blob, Google Drive, or template output.

Port leases are self-contained declarations. Do not create separate Copier questions like `web_port` for every port. Users override named leases with `--port web=8120`.

## State Sources

The operator can use multiple state sources at the same time:

```sh
angee operator --state-source file
angee operator --state-source file --state-source django-api
angee operator --state-source django-api --state-source django-db
```

`file` stores local state under `$ANGEE_ROOT/state/`. `django-api` talks to a Django backend. `django-db` reads/writes the Django database directly when colocated with the backend. These are deployment choices, not separate resource models.

## Implementation Phases

### Phase 1: Manifest And Command Grammar

- Make `$ANGEE_ROOT/angee.yaml` the only parent-walk marker.
- Remove `.angee/project.yaml` output and lookup.
- Add noun-first commands: `stack init`, `workspace init`, `agent init`, and matching `update` commands.
- Keep top-level `angee init` only as default stack-init shorthand.
- Delete unused flags, commands, and packages from the active code path instead of leaving compatibility branches.

### Phase 2: Copier Integration

- Add `copier-go` as the rendering/update engine.
- Convert example templates to `templates/stacks`, `templates/workspaces`, and `templates/agents` layout.
- Store Copier state in `.copier-answers.yml`.
- Keep Angee metadata in `_angee` and final desired state in `angee.yaml`.

### Phase 3: Operator Provisioning

- Move stack/workspace/agent init logic behind operator services.
- Make CLI init commands dispatch to the compiled-in operator runtime instead of doing provisioning directly.
- Add HTTP/MCP surfaces for stack/workspace/agent init and update.
- Make `angee dev` run the in-process operator runtime and reconcile from `.angee/angee.yaml`.

### Phase 4: Sources, Secrets, And Ports

- Implement source materialization for local, git/github, volume, URL/archive, and object-store sources.
- Preserve generated secrets on update.
- Add named port leases with host-level collision avoidance.
- Remove template-level one-question-per-port patterns.

### Phase 5: Deployment Backends

- Keep Docker Compose as the first backend.
- Keep deployment backend generation behind the operator.
- Add Kubernetes backend later using the same resources: services, jobs, workflows, volumes, secrets, and port leases.

### Phase 6: Durable Workflows

- Run workflows inline locally first.
- Add Temporal-backed workflows and activities for server-side provisioning, updates, deploys, rollbacks, workspaces, and agents.

## Delete Or Replace

Delete or replace these old concepts during the refactor:

| Old concept | Replacement |
|---|---|
| `.angee-template.yaml` | Copier `copier.yml` plus `_angee` metadata. |
| Go `text/template` stack rendering | Copier/Jinja rendering through `copier-go`. |
| `.angee/project.yaml` | `$ANGEE_ROOT/angee.yaml`. |
| `services: []` runtime-only sentinel | Explicit stack template with local services/jobs/workflows. |
| CLI-only init paths | Operator-owned provisioning path. |
| Flag-based stack init | `angee stack init dev` and top-level `angee init` shorthand. |
| Flag-based workspace init | `angee workspace init`. |
| Add-style agent provisioning | `angee agent init`. |
| `web_port`/`ui_port` template answers | Named `port_leases` overridden with `--port name=value`. |

## Deferred Archive

Reference material that may still contain useful ideas can move to `deferred/`, but it must not remain buildable or imported. Use `.go.deferred`, `.txt`, or Markdown notes instead of active `.go` files. Git history remains the primary archive, so prefer short notes over large source copies.

Do not keep unused options, arguments, Cobra commands, runtime adapters, framework dispatchers, or compatibility branches in active packages. If a feature is not part of the current vision, remove it from code and document the deferred idea outside the build.

## Success Criteria

- `angee stack init dev --yes` creates `.angee/angee.yaml`, `.copier-answers.yml`, state dirs, secrets, and port leases.
- `angee init --yes` resolves to the same default stack-init path.
- `angee dev` runs the in-process operator runtime and reconciles from `.angee/angee.yaml`.
- `angee workspace init <name>` provisions through the operator path.
- `angee agent init <name>` provisions through the operator path.
- The Django backend can trigger the same operator provisioning path by API or DB-backed state sources.
- Docker Compose staging works from the same manifest model that can later compile to Kubernetes.
