# Templates

Angee templates are how a *shape* of deployment is declared once and
re-used. There are two kinds, both rendered with
[copier-go](https://github.com/fyltr/copier-go):

- **Stack template** — produces an `angee.yaml` (and optionally
  generated runtime files) for a runnable Stack root.
- **Workspace template** — produces a workspace tree under
  `$ANGEE_ROOT/workspaces/<name>`, may declare Sources to materialize,
  and may chain an inner Stack template.

Both kinds are themselves *abstract*: they only define which Services to
run, which Sources to materialize, and which inputs the user supplies.
The concrete result of rendering a template is a Stack root or a
Workspace directory the engine can operate on.

A template must contain `copier.yml` with Angee metadata under
`_angee`.

## Kinds

```yaml
_angee:
  kind: stack
  name: dev
```

```yaml
_angee:
  kind: workspace
  name: pr
```

`angee stack init <template>` resolves stack templates.
`angee workspace create <name> --template <template>` resolves workspace
templates.

## Local Resolution

For a short name like `dev`, stack resolution looks for `stacks/dev`. Workspace
resolution looks for `workspaces/dev`.

Current local search includes:

```text
$ANGEE_ROOT/.templates/<kind>/<name>
$ANGEE_ROOT/templates/<kind>/<name>
$ANGEE_ROOT/<kind>/<name>
$ANGEE_ROOT/<name>
ancestor/.templates/<kind>/<name>
$PWD/.templates/<kind>/<name>
$PWD/templates/<kind>/<name>
ancestor-of-PWD/.templates/<kind>/<name>
```

`<kind>` is `stacks` or `workspaces`.

`angee init --dev` requires a local or remote `stacks/dev` template. The
default Host that ships one is
[`angee-django`](https://github.com/fyltr/angee-django), under
`templates/stacks/dev/`.

## Remote Resolution

HTTP(S) GitHub URLs are supported. The URL must include owner, repo, and a
template path.

```sh
angee stack init https://github.com/example/templates/tree/main/.templates/stacks/dev
angee workspace create fix-issue-123 --template https://github.com/example/templates/tree/main/.templates/workspaces/pr
```

The resolver clones the repository into the user cache, checks out the
requested branch or `?ref=`, and renders the template path.

## Workspace metadata

Workspace templates may declare inputs, sources to materialize, chained
stack templates, port pool ranges to ensure, and persistent paths:

```yaml
_angee:
  kind: workspace
  name: pr
  instance_naming:
    pattern: "${inputs.branch | slug | truncate(40)}"
  inputs:
    branch:
      type: string
      required: true
  sources:
    app:
      source: app
      mode: worktree
      branch: "${inputs.branch}"
      subpath: app
  chain_root: stack
  chain:
    - template: stacks/dev
      root: stack
  ensure:
    operator.port_pool.workspace:
      range: "8100-8199"
  persist:
    browser-data:
      subpath: .browser-data
      scope: workspace
```

`sources:` is the GitOps half — when the workspace is created, each
listed Source is materialized (a git worktree, a local mount, etc.) on
the configured branch. `chain:` is the deployment half — the workspace
optionally renders a Stack template that runs against those Sources.

Stack templates use the same Copier rendering path and must produce an
`angee.yaml` under the initialized stack root. They are typically much
simpler — just `_angee.kind: stack` plus the Jinja-templated
`angee.yaml` and any seed files (env templates, runtime overlays).

## How "self-building" works

Putting templates and Sources together, the loop is:

1. **Sources are declared** — your repos are listed under `sources:` in
   the rendered `angee.yaml`. Angee fetches and caches them.
2. **A Workspace renders a development shape** — pick a Workspace
   template, supply the inputs (branch name, base ref, port ranges).
   Angee materializes Sources on that branch and renders any chained
   inner Stack template **as files** under the workspace tree.
   Workspaces are a pure file primitive — they never start services.
3. **An explicit stack command brings the inner stack up** — running
   services is always a Stack concern, not a Workspace concern. Drive
   it with `angee stack up --root workspaces/<name>/.angee` (or point a
   second operator at that root). The same `stack up`/`stack down`/
   `stack logs` commands work on a workspace's inner stack as on
   production.
4. **A production Stack runs the same Sources** — point the operator at
   a different root with the same `sources:` referring to release
   branches or tags.

The templating system is therefore the only place where "what runs"
needs to change. Promoting a feature to production does not rebuild any
images or rewrite any compose files — it just updates which ref each
Source points at and re-runs `angee stack up`.

## Service templates

A Copier template with `_angee.kind: service` renders a single
`manifest.Service` entry into the outer stack. Use it when an agent
runtime or other reusable service shape needs a Dockerfile, multiple
inputs, and per-instance port allocation.

**Metadata fields** (`_angee:`):

| Field | Required | Meaning |
| --- | --- | --- |
| `kind: service` | yes | Distinguishes service templates from workspace / stack templates. |
| `name` | yes | Display name of the template. |
| `name_pattern` | no | Substitution pattern for the resolved service name. Default: `agent-${workspace.name}`. Resolved against the workspace name + template inputs. |
| `inputs` | no | Caller-supplied inputs with `required` / `default` flags. |
| `ensure` | no | Port pools the template needs in the outer stack (`operator.port_pool.<pool>`). The operator allocates one port per declared pool, scoped to `service/<name>/<pool>`. |

**Rendered output:** the template must emit a `service.yaml` containing
exactly one service entry under `services:`. Anything else (jobs,
volumes, secrets, sources) is rejected. Other files in the rendered
tree — typically `docker/Dockerfile` and friends — are moved into
`<stack_root>/.angee/services/<service_name>/` so the rendered
`build.context: ./.angee/services/<service_name>/docker` resolves.

**Render variables** available in Jinja:

| Variable | Source |
| --- | --- |
| `service_name` | Resolved from `name_pattern` (or `--name` override). |
| `workspace_name` | `--workspace` flag. |
| `workspace_path` | Absolute path to the workspace dir. |
| `alloc_<pool>` | Allocated port for each pool declared in `ensure`. |
| Caller inputs | Every key from `--input k=v` plus template `_angee.inputs` defaults. |

Secret markers (`${secret.foo}`) in the rendered output are resolved
at compose-render time, not at service-create time.

**Example skeleton:**

```yaml
# templates/services/my-agent/copier.yml
_subdirectory: template
_templates_suffix: .jinja
_angee:
  kind: service
  name: my-agent
  name_pattern: "agent-${workspace.name}"
  inputs:
    api_key:
      required: true
  ensure:
    operator.port_pool.acp:
      range: "3000-3999"

api_key:
  type: str
```

```yaml
# templates/services/my-agent/template/service.yaml.jinja
services:
  {{ service_name }}:
    runtime: container
    build:
      context: ./.angee/services/{{ service_name }}/docker
    ports: ["{{ alloc_acp }}:3007"]
    mounts: ["workspace://{{ workspace_name }}:/workspace"]
    env:
      API_KEY: "{{ api_key }}"
```

Run with:

```sh
angee service create \
  --template ./templates/services/my-agent \
  --workspace my-pa \
  --input api_key=sk-...
```

`angee service destroy agent-my-pa` removes the manifest entry,
releases the port lease, and deletes the build-context dir.

## Bundled templates

The repo ships a small set of templates under `templates/`. Today this
is just `agent-runtime`; more will follow as the host integrations
solidify.

### `agent-runtime`

Materialises a single long-running process that an external host —
today, the `agents` addon in
[`angee-django`](https://github.com/fyltr/angee-django) — addresses
over [ACP](https://github.com/anthropics/agent-client-protocol). The
template is the contract between the operator and any host that wants
to provision per-agent workspaces; the actual runtime binary is
expected to be wired in by the consuming host.

**Inputs:**

| Name | Required | Purpose |
| --- | --- | --- |
| `AGENT_ID` | yes | Identifier for this agent runtime instance. Slugged into the workspace name and passed through to the spawned process. |
| `MCP_URL` | no | URL of the MCP server the agent should connect to. Empty / unset means "no MCP". |
| `MCP_TOKEN` | no | Bearer token for `MCP_URL`. v1 stores it in the workspace env file; rotate by re-running the workspace with updated inputs. |

**Env contract.** The materialised process receives these env vars; this
shape is the load-bearing contract for host integrations:

| Env var | Source |
| --- | --- |
| `AGENT_ID` | Caller input. |
| `MCP_URL` | Caller input (may be empty). |
| `MCP_TOKEN` | Caller input (may be empty). |
| `ACP_PORT` | Allocated from the host stack's `acp` port pool — the host stack must declare one in `operator.port_pool.acp`. |
| `ACP_TOKEN` | Resolved from `${secret:acp_token}` against the operator's secret backend. The host is responsible for provisioning the secret before bringing the workspace up. |

The v1 template renders a placeholder service that prints the contract
and sleeps forever; replace the `services.agent.command` block in your
fork with the real agent runtime invocation.

**Provisioning shape (Django side):**

```graphql
mutation {
  workspaceCreate(input: {
    template: "agent-runtime"
    inputs: [
      {key: "AGENT_ID", value: "agent-claude-1"}
      {key: "MCP_URL", value: "https://mcp.internal/sse"}
      {key: "MCP_TOKEN", value: "..."}
    ]
  }) {
    name
    path
    processComposePort
  }
}
```

`workspaceCreate` only renders the workspace's files (including the inner
`angee.yaml`) and materializes its sources — it does not start the agent
process. Bring the agent up explicitly with a stack operation against the
workspace's inner root:

```sh
angee stack up --root workspaces/agent-claude-1/.angee
# or run a per-workspace operator the host can talk to over HTTP:
angee operator --root workspaces/agent-claude-1/.angee --port 9100
```

Keep this contract in lockstep with
`templates/agent-runtime/copier.yml` and the consuming addon's
provisioning code. Changes to the env var names or semantics need a
coordinated bump on both sides.
