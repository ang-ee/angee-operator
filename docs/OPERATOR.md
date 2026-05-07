# The Angee Operator

The Angee operator is the shared controller for Angee.

It reads desired state from `angee.yaml`, provisions resources, reconciles actual state, and exposes HTTP and MCP APIs. The `angee` CLI has the operator compiled in and uses the operator API instead of implementing provisioning itself.

## What It Owns

The operator owns the full provisioning and reconcile path for:

- stacks
- workspaces
- agents
- sources
- volumes
- services
- jobs
- workflows
- secrets
- port leases
- MCP servers
- deployment backend files

This means `angee stack init`, `angee workspace init`, and `angee agent init` all reuse operator code. A Django backend, HTTP API client, or MCP agent should call the same path.

## Reconcile Loop

The loop is:

1. Read `angee.yaml` or receive desired state from a control plane.
2. Resolve templates when an init or update request needs rendering.
3. Materialize sources.
4. Resolve secrets.
5. Allocate port leases.
6. Create volumes and state directories.
7. Provision services, jobs, workflows, workspaces, agents, and MCP servers.
8. Compile backend files such as `docker-compose.yaml` or Kubernetes manifests.
9. Apply backend changes.
10. Observe status and logs.
11. Write observed state to configured state sources.

## How It Runs

Local development uses the operator runtime inside the CLI process:

```sh
angee dev
```

The CLI instantiates the operator runtime for the selected ANGEE_ROOT and dispatches through the same API request/response types used by remote clients, without opening a port. It then owns the dev lifecycle until interrupted.

Staging or production can run the same operator code as a service:

```sh
angee up
angee deploy
```

The operator can also be controlled by a Django backend or directly use the Django database as one state source.

## State Sources

State sources are composable:

```sh
angee operator --state-source file
angee operator --state-source file --state-source django-api
angee operator --state-source django-api --state-source django-db
```

| State source | Meaning |
|---|---|
| `file` | Local state under `$ANGEE_ROOT/state/`. |
| `django-api` | State and commands through Django backend APIs. |
| `django-db` | Direct Django database access when colocated. |
| Temporal persistence | Future durable workflow state. |

## Interfaces

| Interface | Use |
|---|---|
| CLI | Human local/dev/deploy commands. |
| HTTP API | Apps, dashboards, CI, and backend control planes. |
| MCP API | Agents provisioning and operating resources. |

All interfaces expose the same core capabilities because they call the same operator services.

## Backends

Docker Compose is the first deployment backend.

Kubernetes is the scale-out target. The operator should reconcile the same Angee resources to Kubernetes resources later without changing the user model.

Temporal is the durable workflow target for long-running provisioning, update, deploy, rollback, workspace, and agent workflows.
