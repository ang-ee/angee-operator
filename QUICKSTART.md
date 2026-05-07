# Angee Quick Start

Status: target after template refactor

Angee runs a stack from one manifest: `$ANGEE_ROOT/angee.yaml`.

The CLI has the operator compiled in. Local commands dispatch to the in-process operator runtime without opening ports. The operator provisions templates, resolves sources, allocates ports, manages secrets, and reconciles services, jobs, workflows, workspaces, agents, and MCP servers.

## Install

```sh
curl https://angee.ai/install.sh | sh
```

Or with Homebrew:

```sh
brew install angee
```

## Local Dev

Bootstrap a project-local dev stack:

```sh
angee stack init dev --yes
angee dev
```

`angee init --yes` is shorthand for the default stack init. In a project with a `stacks/dev` template, it resolves to `angee stack init dev --yes`.

`angee dev` runs the operator runtime for the lifetime of the command and reconciles from `.angee/angee.yaml`.

## Staging

Create a Docker-backed staging stack:

```sh
angee stack init staging-docker \
  --set domain=staging.example.com \
  --secret anthropic-api-key=env:ANTHROPIC_API_KEY \
  --yes

angee up
```

## Workspaces

Create a feature workspace with its own sources, services, state, and port leases:

```sh
angee workspace init feat-refactor-2 --branch feat-refactor-2 --yes
angee workspace dev feat-refactor-2
```

Update it from its template:

```sh
angee workspace update feat-refactor-2
```

## Agents

Create an agent-backed workspace:

```sh
angee agent init feat-refactor-2 \
  --branch feat-refactor-2 \
  --template agents/claude-code \
  --workspace-template workspaces/feature-dev \
  --secret anthropic-api-key=env:ANTHROPIC_API_KEY \
  --start \
  --yes
```

Interact with the agent:

```sh
angee agent chat feat-refactor-2
angee agent ask feat-refactor-2 "summarize the current branch"
angee agent logs feat-refactor-2 --follow
```

## Updates

Update the current stack from its active template:

```sh
angee stack update
```

The operator rerenders the template with Copier, preserves generated secrets and port leases unless explicitly changed, then reconciles actual state from `angee.yaml`.

## Common Commands

```sh
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
```

Full target usage reference: [`docs/USAGE.md`](./docs/USAGE.md).
