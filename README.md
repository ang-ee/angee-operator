# angee

Angee is a CLI and operator for running an application stack from one declarative manifest: `angee.yaml`.

It is like Docker Compose with first-class agents, workspaces, sources, secrets, MCP servers, and future Kubernetes support.

## Target Model

- One manifest: `$ANGEE_ROOT/angee.yaml`.
- Templates are Copier templates with Angee metadata under `_angee`.
- The operator owns provisioning and reconciliation.
- The `angee` CLI has the operator compiled in. Local commands dispatch to the in-process operator runtime without opening ports.
- Framework details belong in templates and rendered manifests, not in the Go CLI.

## Quick Start

```sh
angee init --yes
angee dev
```

`angee init` is shorthand for the default stack init, usually `angee stack init dev`.

Explicit stack initialization:

```sh
angee stack init dev --yes
angee dev
```

Staging-style stack:

```sh
angee stack init staging-docker \
  --set domain=staging.example.com \
  --secret anthropic-api-key=env:ANTHROPIC_API_KEY \
  --yes
angee up
```

## Commands

```sh
angee init [path]
angee stack init <name> [path]
angee stack update
angee dev
angee up
angee deploy
angee status
angee logs <service>
angee workspace init <name>
angee workspace update <name>
angee agent init <name>
angee agent update <name>
angee agent list
angee agent logs <name>
```

Full target usage reference: [`docs/USAGE.md`](./docs/USAGE.md).

## Development

```sh
make build
make test
```

Current refactor plan: [`docs/REFACTOR.md`](./docs/REFACTOR.md).
