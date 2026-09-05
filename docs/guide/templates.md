# Templates

Angee templates are how a *shape* of deployment is declared once and
re-used, rendered with [copier-go](https://github.com/ang-ee/copier-go).
The operator renders three kinds:

- **Stack template** (`kind: stack`) — produces an `angee.yaml` (and
  generated runtime files) for a runnable Stack root. Stack templates come
  in two flavours matching the
  [two layouts](/operator/concepts#two-stack-layouts): the **dev** flavour
  renders the framework-dev stack — the project host at the Stack root with
  the framework repos as `git` Sources and a `src` Workspace (plain
  `angee init`); the **local** flavour (local / staging / prod) renders a
  self-contained docker-compose root (`angee stack init`).
- **Workspace template** (`kind: workspace`) — produces a workspace tree
  under `$ANGEE_ROOT/workspaces/<name>`, may declare Sources to
  materialize, and may chain an inner Stack template.
- **Service template** (`kind: service`) — produces one Service
  definition (see [Service templates](#service-templates)).

A fourth kind, **project** (`kind: project`), belongs to the Host rather
than the operator: it scaffolds the application repository a Stack runs as
a Source (for the default Host, a Django + React project — see the
[Host glossary](/guide/glossary)). It is rendered by copier / the
framework, not `angee stack init`; the operator lists it but does not
render it.

Every kind is *abstract*: it only declares which Services to run, which
Sources to materialize, and which inputs the user supplies. Rendering
produces a Stack root, a Workspace directory, a Service, or a project repo
the engine can operate on. A template must contain `copier.yml` with Angee
metadata under `_angee`.

Angee's stateful reconciliation preserves Copier templates with
`_preserve_symlinks: true`: it treats each rendered symlink as a first-class
entry, fingerprinting it by its link target and applying it through a rooted
symlink write, so layered and dry-run writes stay safe. Symlink *parents* are
governed separately: existing workspace links for declared local Sources have
their resolved target verified before Angee reads or writes through them, and an
undeclared symlink parent is rejected.

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

`angee init` resolves `stacks/dev` from the local search paths first, then
from the template registry —
[`ang-ee/angee-django`](https://github.com/ang-ee/angee-django) by
default; `ANGEE_TEMPLATE_REGISTRY` overrides it with another repository
(URL, `owner/repo`, or a local path).

## Remote Resolution

HTTP(S) GitHub URLs are supported. The URL must include owner, repo, and a
template path.

```sh
angee stack init https://github.com/example/templates/tree/main/.templates/stacks/dev
angee workspace create fix-issue-123 --template https://github.com/example/templates/tree/main/.templates/workspaces/pr
```

The resolver clones the repository into the user cache, checks out the
requested branch or `?ref=`, and renders the template path.

## Questions and inputs

Every top-level key of `copier.yml` that is not prefixed with `_` is a
question. Angee reads the question metadata into the template descriptor
(`angee template get <ref>`, `GET /templates/{ref}`, GraphQL `templates`)
and drives its input form from it, so what you declare is what the user
sees:

```yaml
runtime_mode:
  type: str
  default: process
  choices: [process, docker]
  help: Run framework application services as local processes or Docker containers.

api_key:
  type: str
  secret: true
  help: Anthropic API key; stored in the stack's secret backend.

seed_users:
  type: str
  multiselect: true
  choices:
    Admin account: admin
    Demo tenant: demo
  help: Fixtures to load on first provision.
```

| Field | Effect in the form |
| --- | --- |
| Declaration order | Questions are asked in `copier.yml` order; put the decisions a user must make first and image pins last. |
| `help` | Shown under the input; keep it to one or two sentences. |
| `default` | Pre-filled value, shown as `(default)` until changed. |
| `choices` | Rendered as a list (a filterable one above eight entries); values are validated in every mode, including `--input` and `--yes`. The mapping form (`Label: value`) shows the label and stores the value. A Jinja string is kept as an expression and falls back to free text. |
| `multiselect: true` | Multiple choices; the stored value is a JSON array string such as `["admin","demo"]`. |
| `type: bool` | A Yes/No toggle; `yes`/`no`/`y`/`n`/`true`/`false` are accepted from flags and answers files. |
| `type: int` | Validated as an integer. |
| `type: path` | Free text; the value is rewritten relative to the stack root as described under [Local Resolution](#local-resolution). |
| `secret: true` | Masked while typing and in the summary; never written to the answers file, so it is asked again on re-render. |
| `placeholder` | Hint shown while the input is empty. |
| `required: true` (in `_angee.inputs`) | The form refuses an empty value; non-interactive runs name the `--input` flag to pass. |
| `when`, `validator` | Carried in the descriptor but not evaluated by the form yet; the render still applies them. |

`_angee.inputs` declares metadata-only inputs (`generated`, `immutable`,
`required`) that are not questions; they appear read-only in the form.
When the same name is declared as a question and under `_angee.inputs`,
the question wins.

Answers recorded by a render (`.copier-answers.stack.yml` for stacks,
`.copier-answers.yml` for workspaces and services) can be replayed with
`--answers <file>` on the create commands, and `angee stack update
--template -i` re-opens the form with the recorded values.

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

### Reaching outside the template directory

When a workspace renders a chained template, Angee first snapshots that
template to a private directory so the render is pinned against edits
mid-flight. By default the snapshot is the template directory alone, so a
template that includes a file from outside it — a shared partial kept beside
the collection, say — would not find it.

Declare how far the template reaches with `_angee.include_root`, and that
ancestor is snapshotted instead:

```yaml
# templates/stacks/dev/copier.yml
_angee:
  kind: stack
  name: dev
  include_root: "../.."   # pins templates/, so ../../_shared/ resolves
```

```jinja
{% include "../../_shared/AGENTS.md.jinja" %}
```

`include_root` is relative to the template directory, must name one of its
ancestors, and must stay inside the workspace — pointing it at the workspace
root or beyond is rejected. Angee does not infer this from directory names:
how you lay out a template collection is your business, so a collection named
`hosts/` behaves exactly like one named `stacks/`.

Leave it unset for a self-contained template; the snapshot is then exactly the
template, which is both cheaper and tighter.

::: warning Keep the root tight
Everything under `include_root` is recursively copied on **every** chain
render. The workspace is the only bound Angee enforces, so a template living
inside a materialized Source can legitimately declare a root that pins the
whole checkout — `node_modules` and all. Point it at the smallest directory
that makes your includes resolve.
:::

`include_root` only applies to templates chained from inside a workspace, which
is where the snapshot happens. It is ignored for absolute and remote refs, and
for renders that are not chained — those read the template in place, so their
relative includes already resolve against the real tree.

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
tree — typically `docker/Dockerfile` and friends — are reconciled into
`<stack_root>/services/<service_name>/` so the rendered
`build.context: ./services/<service_name>/docker` resolves.

**Render variables** available in Jinja:

| Variable | Source |
| --- | --- |
| `service_name` | Resolved from `name_pattern` (or `--name` override). |
| `workspace_name` | `--workspace` flag. |
| `workspace_path` | Absolute path to the workspace dir. |
| `alloc_<pool>` | Allocated port for each pool declared in `ensure`. |
| Caller inputs | Every key from `--input k=v`, over the stack's `workspace_defaults` for the template, over the template's `_angee.inputs` defaults. |

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
      context: ./services/{{ service_name }}/docker
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

Template-created services can be refreshed in place:

```sh
angee service update agent-my-pa --template
angee service update agent-my-pa --template --input api_key=sk-new --dry-run
angee service update agent-my-pa --template --overwrite
```

Angee tracks the last rendered assets and `service.yaml`. Updates preserve
independent local manifest edits, apply independent template edits, and report
same-field or asset conflicts before writing. Lists and scalar values are
atomic; maps such as `env`, structured `build`, and `route` merge recursively.
`--overwrite` selects the newly rendered value for conflicts. The service name,
workspace binding, and allocated ports remain authoritative from current state.

`angee service destroy agent-my-pa` removes the manifest entry,
releases the port lease, and deletes the build-context dir.

## Bundled templates

The repo ships a small set of templates under `templates/`. Today this
is just `agent-runtime`; more will follow as the host integrations
solidify.

### `agent-runtime`

Materialises a single long-running process that an external host —
today, [`angee-django`](https://github.com/ang-ee/angee-django) —
addresses over [ACP](https://github.com/anthropics/agent-client-protocol). The
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
`templates/agent-runtime/copier.yml` and the consuming host's
provisioning code. Changes to the env var names or semantics need a
coordinated bump on both sides.
