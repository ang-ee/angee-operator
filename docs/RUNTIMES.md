# Runtime And Backend Model

The target design does not have a separate project-mode runtime manifest. There is no `.angee/project.yaml`.

All desired state lives in:

```text
$ANGEE_ROOT/angee.yaml
```

## Runtime Concepts

Angee uses generic resources:

| Resource | Meaning |
|---|---|
| Source | Content tree from GitHub/git, local path, URL, S3, Google Drive, archive, template output, or volume. |
| Service | Long-running workload. |
| Job | One-shot workload. |
| Workflow | Ordered durable or inline orchestration of activities. |
| Volume | Persistent storage mounted into services, jobs, or sources. |
| Port lease | Named host port allocation. |

Framework-specific commands such as Django migrations are declared as jobs or workflow activities in `angee.yaml`. The Go CLI must not hardcode Django, Node, Rust, or other framework behavior.

## Backends

The operator reconciles `angee.yaml` through a deployment backend.

| Backend | Role |
|---|---|
| Local process | Useful for `angee dev` services and jobs. |
| Docker Compose | First deployment backend for staging and self-hosting. |
| Kubernetes | Future scale-out backend. |
| Temporal | Future durable workflow backend for provisioning, deploys, updates, rollbacks, workspaces, and agents. |

Backends are implementation details. Users interact with Angee resources and commands, not backend-specific APIs.

## Operator Path

All runtime work goes through the operator:

```text
CLI/API/MCP/Django -> operator -> backend
```

`angee dev`, `angee up`, and `angee deploy` dispatch to the in-process operator runtime unless `--operator` points at an explicitly running operator service. The Django backend can also control the operator by API or direct DB-backed state sources.
