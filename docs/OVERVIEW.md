# Angee Overview

Angee runs an application stack from one manifest: `angee.yaml`.

It provides:

- A CLI: `angee`.
- An operator compiled into the CLI and also runnable as a standalone service.
- Copier-based templates for stacks, workspaces, and agents.
- A generic resource model: sources, volumes, services, jobs, workflows, port leases, secrets, MCP servers, workspaces, and agents.

## User Model

The core loop is:

```sh
angee stack init dev
angee dev
```

For staging:

```sh
angee stack init staging-docker --set domain=staging.example.com
angee up
```

For isolated work:

```sh
angee workspace init feat-x --branch feat-x
angee agent init feat-x --template agents/angee-developer --branch feat-x
```

`angee init` is shorthand for the default stack init. The canonical commands are noun-first: `stack init`, `workspace init`, and `agent init`.

## Architecture

The CLI does not own provisioning logic. It dispatches to the in-process operator runtime, or contacts an explicitly configured operator service, using the same API request/response types.

The operator owns the provisioning and reconciliation path:

1. Render or update a Copier template.
2. Write or update `angee.yaml`.
3. Materialize sources.
4. Resolve secrets.
5. Allocate port leases.
6. Provision volumes, services, jobs, workflows, workspaces, agents, and MCP servers.
7. Compile deployment backend files such as `docker-compose.yaml` or Kubernetes manifests.
8. Reconcile actual state.

The same operator path is used by the CLI, HTTP API, MCP API, explicit operator services, and a future Django control plane.

## Deployment Backends

Docker Compose is the first backend.

Kubernetes is the scale-out backend. The resource model maps naturally to Kubernetes resources: services become Deployments/StatefulSets plus Services when networking is needed; jobs become Jobs/CronJobs; volumes become PVCs; secrets become Kubernetes Secrets.

Temporal can back durable workflows later. Local development can run workflows inline first.

## More

- Start with [`README.md`](./README.md).
- Use [`USAGE.md`](./USAGE.md) for commands.
- Use [`REFACTOR.md`](./REFACTOR.md) for the implementation plan.
- Use [`OPERATOR.md`](./OPERATOR.md) for operator details.
