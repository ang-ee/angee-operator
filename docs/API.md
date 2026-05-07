# HTTP API

When explicitly run as a service, the operator exposes an HTTP API for dashboards, CI, agents, backend control planes, and CLI commands that opt in with `--operator` or `ANGEE_OPERATOR_URL`.

The API must call the same operator services used by CLI commands. It must not implement a separate provisioning path.

## Authentication

When `ANGEE_API_KEY` is configured, all endpoints except health checks require:

```http
Authorization: Bearer <api-key>
```

## Core Endpoints

| Endpoint | Meaning |
|---|---|
| `GET /health` | Operator liveness and basic state-source/backend information. |
| `GET /manifest` | Return the current `angee.yaml` as YAML or JSON. |
| `PUT /manifest` | Validate and write `angee.yaml`, then optionally reconcile. |
| `POST /stack/init` | Initialize a stack from a stack template. Mirrors `angee stack init`. |
| `POST /stack/update` | Update the current stack from its active template. Mirrors `angee stack update`. |
| `POST /workspace/init` | Provision a workspace. Mirrors `angee workspace init`. |
| `POST /workspace/update` | Update a workspace. Mirrors `angee workspace update`. |
| `POST /agent/init` | Provision an agent-backed workspace. Mirrors `angee agent init`. |
| `POST /agent/update` | Update an agent-backed workspace. Mirrors `angee agent update`. |
| `POST /reconcile` | Reconcile current desired state to actual state. |
| `POST /deploy` | Compile and apply deployment backend changes. |
| `POST /rollback` | Roll back to a prior stack revision when file/git state is enabled. |
| `GET /status` | List service, job, workflow, source, port lease, workspace, and agent state. |
| `GET /logs` | Stream or tail logs for services, jobs, workflows, and agents. |

## Init Request Shape

All init endpoints use the same model:

```json
{
  "template": "stacks/dev",
  "ref": "v1.2.3",
  "path": "../my-app",
  "answers": {
    "project_name": "my-app"
  },
  "secrets": {
    "anthropic-api-key": "env:ANTHROPIC_API_KEY"
  },
  "ports": {
    "web": 8120
  },
  "sources": {
    "app": {
      "ref": "feat-x",
      "tree": "."
    }
  }
}
```

The operator resolves the template, renders with Copier, materializes sources, resolves secrets, allocates port leases, writes manifests/state, and reconciles resources.

## State Sources

The API can be backed by any configured state sources:

- `file`
- `django-api`
- `django-db`
- future Temporal persistence

State-source choice must not change the API resource model.
