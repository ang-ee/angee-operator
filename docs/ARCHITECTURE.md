# Architecture

Status: target architecture for the fresh refactor

Angee has one user model: templates create `angee.yaml`, and the operator reconciles that manifest.

## Components

| Component | Responsibility |
|---|---|
| `angee` CLI | User interface. Has the operator compiled in and dispatches local commands to the in-process operator runtime. |
| operator | Shared provisioning and reconciliation engine, callable in-process by the CLI or runnable as an explicitly started service. |
| `angee.yaml` | Desired-state manifest under `$ANGEE_ROOT`. |
| Copier templates | Render or update stack, workspace, and agent files. |
| Deployment backend | Runs actual workloads. Docker Compose first, Kubernetes later. |
| State sources | Files, Django API, Django database, and future Temporal persistence. Multiple sources can be used together. |

## Command Flow

```text
user
  -> angee CLI
  -> in-process operator runtime or explicitly configured operator service
  -> provisioning/reconcile pipeline
  -> deployment backend and state sources
```

The CLI must not duplicate provisioning logic. Stack, workspace, agent, service, and MCP server provisioning all go through the operator path.

Local commands instantiate the operator runtime for the selected ANGEE_ROOT and dispatch through the same API request/response types without binding a port. `--operator` or `ANGEE_OPERATOR_URL` explicitly selects a running remote operator service. This keeps `angee dev`, `angee up`, `angee deploy`, `angee stack init`, `angee workspace init`, and `angee agent init` DRY without creating background daemons.

## Manifest

The only Angee manifest is:

```text
$ANGEE_ROOT/angee.yaml
```

It can declare:

- active template and Copier state
- sources
- volumes
- secrets
- port leases
- services
- jobs
- workflows
- workspaces
- agents
- MCP servers
- operator and deployment backend settings

There is no `.angee/project.yaml` and no separate project mode.

## Resource Model

| Resource | Meaning |
|---|---|
| Source | Named content tree with `kind`, `ref`, `tree`, and `target`. Sources can be GitHub/git, local path, URL, object store, Google Drive, archive, template output, or volume. |
| Volume | Persistent storage mounted into sources, services, or jobs. |
| Service | Long-running workload. Maps to Compose services or Kubernetes workloads. |
| Job | One-shot workload. Maps to Compose run or Kubernetes Job/CronJob. |
| Workflow | Ordered orchestration of activities. Inline locally, Temporal-backed later. |
| Port lease | Named host port allocation. |
| Agent | Durable actor attached to a workspace and one or more services. |
| MCP server | Tool endpoint exposed to agents. |

## Provisioning Pipeline

The operator pipeline is:

```text
resolve template
render/update with Copier
write/update angee.yaml
materialize sources
resolve secrets
allocate port leases
create volumes/state dirs
provision services/jobs/workflows/workspaces/agents/MCP servers
compile backend files when needed
apply backend changes
observe status and logs
write state sources
```

This same pipeline is used by:

- `angee stack init`
- `angee workspace init`
- `angee agent init`
- `angee dev`
- HTTP API
- MCP API
- Django backend control plane

## Templates

Templates are Copier templates in this layout:

```text
templates/stacks/<name>/
templates/workspaces/<name>/
templates/agents/<name>/
```

Angee owns template resolution and `_angee` metadata. Copier owns rendering and update mechanics.

## Development

`angee dev` runs the operator runtime in the foreground for the lifetime of the dev command. It reads `.angee/angee.yaml`, reconciles sources, secrets, port leases, jobs, and services, and stops local dev processes when the command exits.

## Deployment

Docker Compose is the first deployment backend. The operator can compile Angee resources into `docker-compose.yaml` and apply them.

Kubernetes is the target scale-out backend. The same Angee resources compile to Kubernetes Deployments, StatefulSets, Services, Jobs, CronJobs, PVCs, and Secrets.

Temporal is the target durable workflow backend for long-running provisioning and deployment workflows.

## State Sources

The operator can combine state sources:

```sh
angee operator --state-source file
angee operator --state-source file --state-source django-api
angee operator --state-source django-api --state-source django-db
```

`file` stores local state under `$ANGEE_ROOT/state/`. `django-api` uses backend APIs. `django-db` reads and writes the Django database directly when colocated.

## Non-Goals

- No backward compatibility with `.angee-template.yaml`.
- No `.angee/project.yaml`.
- No CLI-only provisioning implementation.
- No framework-specific behavior hardcoded in Go.
- No one-Copier-question-per-port pattern.
