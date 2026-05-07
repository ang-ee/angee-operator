# Angee Overview

Angee is a CLI and operator for running an application stack from one declarative manifest: `angee.yaml`.

It is like Docker Compose with first-class agents, workspaces, sources, secrets, MCP servers, and future Kubernetes support.

## Pieces

| Piece | What it does |
|---|---|
| `angee` CLI | User interface for Angee. It has the operator compiled in, dispatches local commands to the in-process operator runtime, and gives users commands such as `init`, `stack init`, `workspace init`, `agent init`, `dev`, `up`, `deploy`, and `logs`. |
| operator | The shared reconciler and provisioner, available in-process to the CLI or as an explicitly started service. It reconciles desired state from `angee.yaml`, materializes sources, manages secrets and port leases, starts/stops services, runs jobs/workflows, provisions workspaces and agents, and exposes HTTP/MCP APIs when run as a service. |
| `angee.yaml` | The source-of-truth manifest under `$ANGEE_ROOT`. It describes sources, volumes, services, jobs, workflows, agents, MCP servers, secrets, and deployment backend settings. |
| Copier template | A reusable scaffold that creates or updates `angee.yaml` plus any project files needed for a stack, workspace, or agent. |

## How Init Works

`angee stack init` starts from a stack template. `angee init` is only a shorthand for the default stack init.

The template can be local, bundled, or fetched from a Git URL. Angee renders it with Copier, then performs Angee-specific setup:

1. Resolve the template, such as `stacks/dev` or `stacks/staging-docker`.
2. Render files into the stack worktree and `$ANGEE_ROOT`.
3. Write `$ANGEE_ROOT/angee.yaml`.
4. Resolve sources such as GitHub repositories, local paths, S3 buckets, Google Drive folders, URLs, archives, templates, or volumes.
5. Generate, derive, or load secrets without committing secret values.
6. Allocate named port leases so local stacks, workspaces, and agents do not collide.
7. Create state directories, volumes, workspaces, or agent folders declared by the manifest.

The template does not keep running the stack. Once `angee.yaml` exists, the operator works from that manifest.

The provisioning path belongs to the operator, not to a separate CLI implementation. The CLI calls the same operator runtime that a long-running operator, MCP client, or Django backend can use for stacks, workspaces, agents, services, and MCP servers.

## How The Operator Works

The operator is a controller.

It reads desired state from `angee.yaml`, compares it with actual state, and reconciles the difference.

The loop is:

1. Load `angee.yaml`.
2. Resolve sources, secrets, volumes, and port leases.
3. Compile deployment backend files if needed, such as `docker-compose.yaml` or Kubernetes manifests.
4. Apply changes through the selected backend.
5. Observe services, jobs, workflows, agents, and logs.
6. Store state in one or more state sources, such as files, a Django API, a Django database, or Temporal persistence.

The operator can run locally for development or as a long-running service in staging/production.

## Development

For local development, the normal flow is:

```sh
angee stack init dev --yes
angee dev
```

`angee init --yes` is shorthand for the default stack init. In a project with a `stacks/dev` template, that means `angee stack init dev --yes`.

`angee stack init dev` renders a dev stack template and writes `.angee/angee.yaml`.

`angee dev` runs the operator runtime in the foreground for the lifetime of the command. The CLI is the terminal UI and lifecycle owner; the operator runtime reconciles the stack.

In dev, services can run as local processes, Docker containers, or a mix. Example services are a web server, UI dev server, database, Redis, MCP server, or agent runtime.

## Deployment

For staging or production, the flow is usually:

```sh
angee stack init staging-docker --set domain=staging.example.com --yes
angee up
```

A deployment template renders `angee.yaml` and backend files such as `docker-compose.yaml`.

The operator then starts services, runs jobs such as migrations, provisions agents or workspaces, and exposes logs/status through the CLI, HTTP API, and MCP API.

Staging can use Docker Compose first. The same Angee concepts can later compile to Kubernetes manifests or Helm-style outputs.

## Workspaces And Agents

A workspace is an isolated filesystem root with sources, volumes, services, jobs, workflows, and state.

An agent is a durable actor attached to a workspace. Agent templates can render `AGENTS.md`, MCP configuration, model settings, and workspace instructions.

Examples:

```sh
angee workspace init feat-x --branch feat-x --yes
angee agent init feat-x --template agents/angee-developer --branch feat-x --yes
```

The CLI submits the request. The operator provisions the workspace or agent from `angee.yaml` plus the selected templates. The same path is available to HTTP, MCP, and a future Django control plane.

## Scaling Later

Angee starts simple with local files and Docker Compose, but the model is designed to scale.

| Today | Later |
|---|---|
| Local operator started by `angee dev` | Long-running operator controlled by a backend or platform UI |
| File state under `$ANGEE_ROOT/state/` | File state plus API, database, and Temporal state sources |
| Docker Compose backend | Kubernetes backend with Deployments, StatefulSets, Jobs, Services, PVCs, and Secrets |
| Inline workflows | Temporal-backed durable workflows and activities |
| Local workspaces and agents | Server-managed workspaces and agents with shared state and permissions |

The goal is one user model: templates produce `angee.yaml`, and the operator reconciles the stack from that manifest whether it is running on a laptop, a staging box, or a Kubernetes cluster.
