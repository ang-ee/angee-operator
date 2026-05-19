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
  chain_lifecycle: auto
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
   Angee materializes Sources on that branch and brings up the chained
   inner Stack.
3. **A production Stack runs the same Sources** — point the operator at
   a different root with the same `sources:` referring to release
   branches or tags.
4. **The same operator drives both** — `stack up`, `stack down`, `stack
   logs` (and the matching REST + GraphQL endpoints) work the same in a
   workspace as in production.

The templating system is therefore the only place where "what runs"
needs to change. Promoting a feature to production does not rebuild any
images or rewrite any compose files — it just updates which ref each
Source points at and re-runs `angee stack up`.

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
    start: true
  }) {
    name
    path
    processComposePort
  }
}
```

Keep this contract in lockstep with
`templates/agent-runtime/copier.yml` and the consuming addon's
provisioning code. Changes to the env var names or semantics need a
coordinated bump on both sides.
