# Getting started

Angee is a self-managed stack manager: a Go CLI (`angee`) and an HTTP
operator (`angee-operator`) that pulls source repositories, renders them
into Stacks for production and into Workspaces for development, and runs
the result on docker-compose or process-compose.

If you have not already, skim [Concepts](/operator/concepts) first — it
explains what the engine does, what a **Host** is (e.g. `angee-django`
is the first default Host), and the difference between abstract terms
(Stack template, Workspace template) and concrete runtime objects
(Source, Workspace, Service).

## Install

With Homebrew (macOS and Linux):

```sh
brew tap ang-ee/tap
brew trust ang-ee/tap
brew install angee
```

`brew trust` is a one-time step — Homebrew 6 refuses to load dependencies from
a tap you have not trusted, and `angee` pulls in `process-compose`. Installing
this way gets you both binaries plus `process-compose`; Docker is deliberately
left out, since it is only needed for `runtime: container` Services.

From a release:

```sh
curl -fsSL https://angee.ai/install.sh | sh
```

From a checkout:

```sh
make install
```

`make install` builds `dist/angee` and `dist/angee-operator`, then runs
`scripts/install.sh` against those local binaries. Set `ANGEE_INSTALL_DIR`
to install somewhere other than `/usr/local/bin`.

Requirements:

- Docker, for `runtime: container` Services.
- `process-compose`, for `runtime: local` Services.
- `git`, for git-kind Sources.
- Nothing else: `angee init` resolves its templates from the template
  registry ([ang-ee/angee-templates](https://github.com/ang-ee/angee-templates))
  and `angee dev` clones the Host sources the rendered manifest declares.

## First commands

Angee operates on one `ANGEE_ROOT` containing `angee.yaml`. The CLI
walks upward from the current directory to find it.

```sh
angee doctor       # check tooling and root
angee status       # show stack + service state
angee up           # start container Services
angee dev          # start container + local Services together
```

To bootstrap a fresh stack from a Stack template:

```sh
angee init --yes
angee dev
```

`angee init` renders the default `dev` Stack template — the framework-dev
stack from the template registry
([ang-ee/angee-templates](https://github.com/ang-ee/angee-templates); see
[Templates](/cli/templates)). The rendered manifest declares the
framework repos as Sources and the `src` Workspace; `angee dev`
materializes them all and boots.

## A typical development loop

```sh
# Develop a feature in an isolated Workspace — this renders files only.
angee workspace create fix-issue-123 --template dev-pr
angee workspace status fix-issue-123

# Bring up the workspace's inner stack explicitly (workspaces don't manage
# services). Use the second form to expose an HTTP operator against it.
angee stack up --root workspaces/fix-issue-123/.angee
# angee operator --root workspaces/fix-issue-123/.angee --port 9100

# Iterate. Each Source is a git worktree on workspace/fix-issue-123.
angee dev
# … edit code …
angee workspace push fix-issue-123          # push every Source's branch

# Promote to production
git -C ~/prod/.angee pull
angee --operator https://operator.example.com stack up
```

The same `angee.yaml` drives both the Workspace and the production
Stack. The only difference is which root the operator points at.

## Where to next

- [Concepts](/operator/concepts) — Stack, Source, Workspace, Service, Host,
  and the engine boundary.
- [Commands](/cli/commands) — every CLI subcommand and flag.
- [Manifest](/cli/manifest) — `angee.yaml` schema and patterns.
- [Templates](/cli/templates) — Copier templates for stacks and workspaces.
- [Operator API](/operator/api) — REST + GraphQL transports.
- [Surface parity](/operator/surfaces) — which Platform methods are exposed
  on which surface.
