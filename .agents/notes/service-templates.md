# Implementation brief: service templates (Option B)

> **Status:** design approved, implementation not started.
> **Author of brief:** previous session; written 2026-05-19.
> **Audience:** the next agent or engineer to pick this up. Read top-to-bottom.

## Goal

Add a new template kind — `_angee.kind: service` — that lets a Copier
template render **one** `manifest.Service` entry into the outer
`angee.yaml`, parameterised by a target workspace plus user inputs.

End-to-end, the verb is:

```sh
angee service create \
  --template ./templates/agents/claude-code \
  --workspace my-pa \
  --input auth_mode=api_key \
  [--name agent-my-pa]            # override the resolved name
```

That run must:

1. Validate the template (`_angee.kind == service`, required inputs
   satisfied, port pools declared in `_angee.ensure` exist in the outer
   stack or can be ensured).
2. Verify the target workspace exists.
3. Allocate ports from the outer stack's port pools for any pool listed
   in the template's `_angee.ensure`, owner = the resolved service name.
4. Compute `service_name` from `_angee.name_pattern` using
   `internal/substitute` (default `agent-${workspace.name}`).
5. Run Copier with a flat input map that includes `workspace_name`,
   `workspace_path`, `alloc_<pool>` for every allocated pool,
   `service_name`, and all user inputs verbatim.
6. Parse the rendered `service.yaml` as a partial `manifest.Stack` (only
   `services:` is expected).
7. Copy any other rendered files (typically `docker/`) into
   `<stack_root>/services/<service_name>/` so the rendered
   `build.context: ./services/<service_name>/docker` resolves.
   (`<stack_root>` is the control root / ANGEE_ROOT, normally `.angee`;
   the build context is a sibling of `workspaces/` and `run/`, never
   nested under a second `.angee`.)
8. Merge the parsed service entry into `stack.Services` and persist
   `stack` via `manifest.SaveFile`.
9. Persist the port leases into `stack.PortLeases[<pool>]` (owner is the
   service name), so subsequent renders see the same allocations.
10. Run `Platform.StackPrepare` to re-render docker-compose.

`service destroy <name>` (existing) should already work for cleanup. Add
deletion of `services/<service_name>/` and release of the port
leases.

## Canonical templates the implementation must support

Both live in the sibling **angee-django** repo:

```text
angee-django/templates/agents/
├── README.md
├── claude-code/
│   ├── copier.yml                # _angee.kind: service
│   └── template/
│       ├── service.yaml.jinja
│       └── docker/Dockerfile
└── opencode/
    ├── copier.yml
    └── template/
        ├── service.yaml.jinja
        └── docker/Dockerfile
```

Read them. They use flat Jinja keys (`{{ workspace_name }}`,
`{{ alloc_acp }}`, `{{ service_name }}`, `{{ auth_mode }}`, ...) and the
rendered output uses angee-go substitution markers (`${secret.foo}`,
`${ports.bar}`) that are resolved later at compose render — **don't**
attempt to resolve secrets at `service create` time.

The rendered `service.yaml` looks like:

```yaml
services:
  agent-my-pa:
    runtime: container
    build:
      context: ./services/agent-my-pa/docker
    ports: ["3017:3007"]
    mounts: ["workspace://my-pa:/workspace"]
    env:
      ANTHROPIC_API_KEY: "${secret.anthropic-api-key}"
      CLAUDE_PERMISSION_MODE: bypassPermissions
      ACP_LOG_LEVEL: info
```

That entry is appended verbatim under the outer stack's `services:` map.

## Concrete edit map

The following file:line citations are accurate as of HEAD = `2b1c0f5` on
branch `feat/graphql-subscriptions`. Verify before editing.

### 1. Schema (small)

`internal/copierx/copierx.go:130-141` — `Metadata` struct. Add:

```go
NamePattern string `yaml:"name_pattern"`
```

`internal/copierx/copierx.go:359` — `ValidateMetadata`. The third arg
(`wantKind`) is already a string. The validator only needs the new kind
value `"service"` to be accepted; the caller (new code) supplies it.

`internal/manifest/manifest.go:147-159` — `Service` struct is unchanged.
The renderer parses the rendered partial as a `manifest.Stack` (just the
`services:` field is populated) and the existing schema handles it.

### 2. API DTO

`api/types.go` (top of file, near `ServiceInitRequest` at line 186) — add:

```go
type ServiceCreateRequest struct {
    Template  string            `json:"template"`
    Workspace string            `json:"workspace"`            // required
    Inputs    map[string]string `json:"inputs,omitempty"`
    Name      string            `json:"name,omitempty"`       // override resolved name
    Start     bool              `json:"start,omitempty"`
}
```

### 3. Platform method

New file `internal/service/service_create.go`. Single public function:

```go
func (p *Platform) ServiceCreate(ctx context.Context, req api.ServiceCreateRequest) (api.ServiceState, error)
```

Outline (preserves the order from the Goal section):

1. Load stack via `p.LoadStack()`.
2. Resolve template path with the same logic as `WorkspaceCreate` uses
   (`internal/service/workspaces.go:27` and `resolveWorkspaceChainTemplate`
   in the same file — extract a shared `resolveTemplateRef` helper).
3. Read metadata via `copierx.ReadMetadata` (`copierx.go:190`); require
   `kind == "service"`.
4. Validate `req.Workspace` exists in `stack.Workspaces`.
5. Allocate ports for every pool in `metadata.Ensure` matching the
   prefix `operator.port_pool.` (see `allocateWorkspacePorts` at
   `internal/service/workspaces.go:1287` for the existing pattern; copy
   it and parameterise on owner). Owner = `service:<resolved-name>`
   (not `workspace:<name>`) so leases are distinct.
6. Build a `substitute.Context` with the workspace path, allocations,
   inputs. Resolve `metadata.NamePattern` (or default
   `agent-${workspace.name}`). `req.Name` overrides if set. Validate
   the result against `manifest.IsValidServiceName` (add it if missing).
7. Reject if `stack.Services[serviceName]` already exists.
8. Build flat Copier inputs: `workspace_name`, `workspace_path`,
   `service_name`, `alloc_<pool>` for each allocation, plus
   `req.Inputs` verbatim. (Same shape as `internal/service/workspaces.go:67-69`.)
9. Render with `copierx.LocalRenderer{}.Copy` (`copierx.go:198`) into
   a temp dir.
10. Read the rendered `service.yaml` and parse as a partial
    `manifest.Stack`. Confirm exactly one service is declared and it
    matches `serviceName`. Reject anything else (jobs, volumes, ports,
    secrets — render produced more than just the service).
11. Move the non-`service.yaml` outputs (typically `docker/`) into
    `filepath.Join(p.root, "services", serviceName)` (create
    parents). Overwrite is fine for re-render. If a directory entry
    would collide with existing files, error.
12. Append the service to `stack.Services` and persist port leases in
    `stack.PortLeases`. Call `manifest.SaveFile`.
13. Call `p.StackPrepare(ctx)` to re-render compose.
14. If `req.Start`, call `p.ServiceStart(ctx, []string{serviceName})`.
15. Return a `ServiceState` for the freshly-created service (reuse the
    list logic from `internal/service/services.go:104`).

Cleanup on failure (rollback): if step 11 or 12 fails, undo the
allocated port leases. Worth wrapping in a `defer` that releases unless
the function returns nil.

`ServiceDestroy` (`internal/service/services.go:84`) needs a sibling
change: after removing the service from the manifest, also delete the
build context dir at `<root>/services/<name>` and release any
port leases owned by `service:<name>`. Check the current implementation
before touching it — it may already be sufficient.

### 4. CLI

`internal/cli/root.go:380-389` — `serviceCommand`. Register one more
subcommand:

```go
cmd.AddCommand(serviceCreateCommand(stdout, root, operatorURL, jsonOutput))
```

New func `serviceCreateCommand` mirroring `workspaceCreateCommand`
shape (`internal/cli/root.go:1046`). Flags:

| Flag | Type | Notes |
|---|---|---|
| `--template` | string, required | Local path or shorthand handed to template resolver. |
| `--workspace` | string, required | Name of the workspace this service binds to. |
| `--input k=v` | string slice | Repeatable. Parsed via `parseKeyValues`. |
| `--name` | string | Optional override of the resolved service name. |
| `--start` | bool | Start the service after create. Default false. |
| `--json` | inherited from parent | Output the resulting `ServiceState` as JSON. |

Position arg: none. The service name is *derived*, not supplied.

### 5. Remote / operator client

`internal/cli/operator_client.go` — `remotePlatform` already implements
`Platform`'s service methods. Add `ServiceCreate` against `POST
/services/create`. Match the existing pattern at lines 180-200ish
(`serviceAction`).

### 6. REST

`internal/operator/operator.go:125-132` — service routes. Add:

```go
mux.Handle("POST /services/create", s.auth(http.HandlerFunc(s.serviceCreate)))
```

`mux.Handle("POST /services", ...)` at line 126 stays (it's the
field-based `serviceInit`). The new endpoint is template-based.

Implement `s.serviceCreate` next to `s.serviceInit` at line 408. Decode
`api.ServiceCreateRequest`, call `s.platform.ServiceCreate`, write the
returned `ServiceState`.

### 7. GraphQL

`internal/operator/schema.graphql` (Mutation block starts at line 287):

```graphql
input ServiceCreateInput {
  template: String!
  workspace: String!
  inputs: [KeyValueInput!]
  name: String
  start: Boolean
}

# Add to type Mutation
serviceCreate(input: ServiceCreateInput!): ServiceState
```

Then regenerate: `cd internal/operator && go run github.com/99designs/gqlgen generate`
(or whatever the project's gqlgen invocation is — check `gqlgen.yml`
and the Makefile). Implement the resolver in
`internal/operator/gql/schema.resolvers.go` next to
`ServiceInit` (line 115). Pattern: build `api.ServiceCreateRequest` from
the input, dispatch through `Platform`, map `ServiceState` to the
generated model.

### 8. Docs

Update:

- `docs/reference/operator-api.md` — REST endpoint table.
- `docs/reference/surfaces.md` — CLI + GraphQL surfaces table.
- `docs/guide/commands.md` — under "Services", document `service create`.
- `docs/guide/templates.md` — add a "Service templates" section
  alongside the existing workspace/stack sections, pointing at the two
  canonical examples in angee-django.
- `docs/reference/manifest-schema.md` — regenerate
  (`cd docs && npm run gen:schema`) once the new `NamePattern` field
  lands on `copierx.Metadata`. Note that this file is not the
  `manifest.Stack` schema; the new field is on the *template metadata*
  (`_angee.name_pattern`), which is not part of the manifest schema.
  Confirm whether template metadata is documented anywhere generated;
  if not, document the new field by hand in `docs/guide/templates.md`.

### 9. Tests

At minimum:

- `internal/service/service_create_test.go` — table-driven covering:
  (a) happy path with the canonical `claude-code` template fixture;
  (b) auth_mode=oauth variant;
  (c) target workspace missing;
  (d) duplicate service name;
  (e) port pool not declared in outer stack;
  (f) template kind != "service";
  (g) rendered partial contains forbidden fields (jobs, volumes, secrets).
- `internal/cli/root_test.go` — CLI flag parsing for `service create`.
- `internal/operator/operator_test.go` — REST round-trip on
  `POST /services/create`.
- `internal/operator/rest_parity_test.go` (or similar parity harness) —
  GraphQL `serviceCreate` parity with REST.

Fixture templates: drop minimal stand-ins under
`internal/service/testdata/service-templates/claude-code/` (don't pull
from angee-django at test time — vendor a minimal copy). The fixture
only needs `copier.yml` with `_angee.kind: service` and a tiny
`service.yaml.jinja` that exercises the substitution context.

## Design points the previous session settled

These are committed and should not be re-litigated unless something
breaks:

1. **No new merge layer in the compose compiler.** `service create`
   appends a literal entry to `angee.yaml`. The file ends up
   hand-readable. Rejected the alternative `includes:` glob design.

2. **Workspaces stay pure file primitives.** They have no lifecycle
   (no `WorkspaceStart/Stop`). The `feat/graphql-subscriptions` branch
   already shipped that removal (`f48784c "Make workspaces a pure file
   primitive"`). Don't reintroduce it. The boundary memory is at
   `~/.claude/projects/-Users-alexis-Work-fyltr-angee-go/memory/feedback_workspaces_files_only.md`.

3. **Port allocations on services use the same pool mechanism as
   workspaces.** Owner key is `service:<name>` to keep them distinct
   from `workspace:<name>` leases.

4. **Build contexts live at `<root>/services/<service_name>/`.**
   Not under the workspace's directory. Per-service, owned by the stack.
   Allows multiple services from the same template (different inputs)
   without collision.

5. **Secret resolution stays at compose-render time.** The rendered
   service entry contains literal `${secret.foo}` markers. The renderer
   does not touch secrets.

6. **`service create` is a new verb, not a flag on `service init`.**
   `service init` is field-based (image, command, ports). `service
   create` is template-based. They're distinct CLI surfaces; keep them
   separate.

## Conventions to follow

- Read `AGENTS.md` at the repo root before touching code — it's the
  canonical instructions file.
- Use the `go-code-reviewer` sub-agent proactively after any non-trivial
  change. It checks for context propagation, error wrapping, goroutine
  hygiene, and the project's docker/process-compose conventions.
- The repo uses Go 1.25+; only deps are cobra + yaml.v3 (+ gqlgen for
  generated code).
- `make check` = fmt + vet + lint + test. Run before declaring done.
- Errors: prefer typed errors (`NotFoundError`, `ConflictError`,
  `InvalidInputError`) from `internal/service` so HTTP handlers can map
  them to status codes.

## Open questions worth asking before implementing

1. **Should `service create` also accept multiple `--workspace` flags
   for multi-workspace mounting?** The two canonical templates only
   mount one workspace each. The design supports it (just multiple
   workspace_name_X keys), but no template needs it yet. Recommendation:
   defer until someone has a real second use case; don't speculatively
   build the multi-workspace input shape.

2. **Should we auto-add missing port pools to the outer stack at
   `service create` time?** Simpler: fail with a clear error pointing
   at the missing pool and instructing the user to add it to
   `angee.yaml`. Workspaces have the same gap; matching their behaviour
   is right.

3. **Idempotence on re-render: should `service create` with an existing
   name update the entry, or error?** Recommendation: error. Use
   `service update` for updates. The render flow is non-trivial enough
   that conflating create and update will hide bugs.

4. **What happens to a service if its bound workspace is destroyed?**
   The compose render will fail to resolve `workspace://<name>` mounts.
   Best: `workspace destroy` warns about (or refuses with `--force`)
   destroying a workspace that's mounted by a service. Out of scope
   for this brief but worth a follow-up.

## Definition of done

- `angee service create --template <path> --workspace <name>` works
  against both canonical templates and produces a startable service.
- `angee service start agent-<name>` brings the container up.
- `angee service destroy agent-<name>` removes the entry, releases the
  port lease, and deletes the build context dir.
- `make check` passes.
- Docs updated (commands.md, operator-api.md, surfaces.md, templates.md).
- A `CHANGELOG.md` entry is added under the next-version heading.
