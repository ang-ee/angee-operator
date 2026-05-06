# Refactor: Copier Templates And Stack Init

Status: draft  
Date: 2026-05-06

## Purpose

This note captures the current `angee-go` init/template implementation, the target direction from `../django-angee/docs/packages/platform/`, and a practical refactor path that makes templates Copier-based instead of expanding Angee's bespoke renderer.

The main user-facing change should be:

```sh
angee init stack dev
```

with:

```sh
angee init --dev
```

as shorthand for the same operation.

`stack dev` means: resolve and apply `templates/stacks/dev` using Copier, then run Angee post-render steps for secrets, ports, runtime manifests, generated compose files, and framework bootstrap.

## Research Inputs

- `cli/init.go`: current compose-mode init and `--dev` special cases.
- `cli/init_runtime.go`: current runtime/project-mode `angee init --dev` branch.
- `internal/tmpl/fs.go`: bespoke `.angee-template.yaml` loader, Go `text/template` renderer, secret resolver, and agent scaffold copier.
- `cli/project.go`, `cli/dev.go`, `internal/projmode/*`: current runtime dispatch and dev orchestration.
- `templates/default/`: only built-in template in `angee-go` today.
- `../django-angee/docs/packages/platform/DESIGN.md`: server-side platform concepts: workspace, repository, service, job, port lease, recipe/template provisioning.
- `../django-angee/docs/packages/platform/EXAMPLES.md`: target template surface: `templates/stacks`, `templates/workspaces`, `templates/agents`, `copier.yml + _angee`, template update, `stack switch`.
- `../django-angee/docs/AGENT-PROVISIONING.md`: local agent workspace provisioning and `angee init --dev` as canonical post-provision hook.
- `../django-angee/docs/IMPORT-EXPORT.md`: direction away from template-local `fixtures:` toward framework-managed assets/fixtures loading.
- `../copier-go` on branch `refactor-go`: Go implementation of Copier with `Copy`, `Recopy`, `Update`, `CheckUpdate`, Jinja-like rendering via `pongo2`, answers files, VCS refs, tasks, migrations, and three-way update support.

## Current State

`angee init` is doing several different jobs in one path:

- It auto-detects `.angee-template/` or falls back to `https://github.com/fyltr/angee#templates/default`.
- It loads `.angee-template.yaml` through `internal/tmpl.LoadMeta`.
- It renders `angee.yaml.tmpl` or `angee.dev.yaml.tmpl` with Go `text/template` and a fixed `TemplateParams` struct.
- It resolves generated, derived, prompted, and supplied secrets into `.env`.
- It initializes ANGEE_ROOT, writes `.gitignore`, writes `operator.yaml`, compiles `angee.yaml` into `docker-compose.yaml`, clones configured repositories, and creates agent directories.
- If `--dev` is set, it changes several filesystem targets and emits a Django-friendly `.env` into the project root.
- If template metadata has `runtime != ""` and `services: []`, it diverts into `runInitRuntimeOnly` and skips compose-mode work entirely.

The runtime adapter behavior is already useful, but its marker/config file should be collapsed into the stack manifest:

- `.angee/project.yaml` is currently the parent-walk marker.
- `angee build`, `angee migrate`, `angee doctor`, and `angee fixtures` exec into the runtime adapter.
- `angee dev` launches build watcher, runtime server, frontend server, and currently pyproject-defined extras.
- The Django adapter is concrete and enough for v1.

The current template system is the part that needs replacement:

- `.angee-template.yaml` duplicates concepts that Copier already has as `copier.yml` questions, answers, skip behavior, update behavior, tasks, and migrations.
- Go `text/template` init rendering is incompatible with the platform docs, which assume Jinja/Copier templates.
- There is no `.copier-answers.yml`, so there is no robust template update story.
- There is no `templates/stacks/dev` layout in `angee-go` today; only `templates/default` exists.
- The `--dev` flag is overloaded: compose infra-only mode in one path, runtime-only project bootstrap in another.
- The project-mode branch is triggered by `runtime != "" && services empty`, which is an implicit sentinel rather than a first-class template kind.
- `pyproject.toml` merge is currently a textual append guarded by a substring check.
- Deploy-time agent config files are still rendered by Go `text/template`; that can remain for now but should not influence init-time template design.

## Target Vocabulary

Use the platform docs' vocabulary consistently.

| Term | Meaning | State |
|---|---|---|
| Stack root | A project/deployment root that receives rendered stack files. | Contains `.angee/stack.yaml`. |
| Stack template | Template under `templates/stacks/<name>`. | Renders a stack root: `angee.yaml`, `.angee/`, `.gitignore`, compose files, app scaffolding, etc. |
| Workspace template | Template under `templates/workspaces/<name>`. | Renders an isolated workspace tree under `.angee/workspaces/<name>` or an agent workspace under `.angee/agents/<name>`. |
| Agent template | Template under `templates/agents/<name>`. | Renders agent persona/config scaffolding into a stack root. |
| `.angee/stack.yaml` | The single committed Angee manifest. | Records active stack template, runtime adapter config, dev process config, and workspace defaults. |
| `.copier-answers.yml` | Copier state. | Records rendered answers and template metadata for Copier update. |

Do not keep a separate `.angee/project.yaml`. It was useful as an implementation seam while project mode was added, but it is tech debt if we can break cleanly. The single parent-walk marker should be `.angee/stack.yaml`; runtime mode is just one section inside that stack manifest.

## Command Behavior

### Canonical Dev Init

```sh
angee init stack dev [path]
```

Behavior:

1. Resolve template ref `stacks/dev`.
2. Build an effective Copier template, including any Angee-level `_angee.extends` chain once inheritance lands.
3. Run Copier `Copy` into `[path]` or `.`.
4. Read `_angee` metadata from the effective `copier.yml`.
5. Run Angee post-render steps.
6. Write `.angee/stack.yaml`.
7. If a rendered `angee.yaml` exists, compile it to `docker-compose.yaml`.
8. If `.angee/stack.yaml` has a `runtime:` section, run runtime post-init: `migrate`, then framework fixture/assets loading.

`angee init --dev` should be exactly shorthand for `angee init stack dev`. It should not have a separate code path.

### Default Init

`angee init` should become a thin convenience wrapper, not another mode.

Proposed detection order:

1. If `.angee/stack.yaml` exists, run `angee update` against its active template.
2. If `templates/stacks/dev/` exists in the current root, use `angee init stack dev`.
3. If `templates/stacks/default/` exists, use `angee init stack default`.
4. Otherwise use the first-party default stack template, or fail with a clear "pass `angee init stack <name>` or `--template`" message.

### Update

```sh
angee update [path]
angee update --ref v4
```

Behavior:

1. Read `.angee/stack.yaml`.
2. Resolve the active template.
3. Run Copier `Update` with previous answers by default.
4. Re-run Angee post-render steps.
5. Recompile generated runtime files if inputs changed.

`update` is the correct replacement for repeated `init --force` behavior. It should preserve user edits through Copier's three-way update algorithm instead of clobbering rendered files.

### Stack Switch

```sh
angee stack switch staging
```

Behavior:

1. Change `.angee/stack.yaml.template.active` to `stacks/staging`.
2. Run `angee update`.
3. Surface Copier conflicts normally.

This is a small wrapper around marker mutation plus update.

### Workspace And Agent Follow-ups

Defer these until stack init is clean:

```sh
angee init workspace <name> [--template workspaces/feature-dev]
angee agent add <name> [--template agents/opencode-admin]
```

The platform docs already define the target shape. The stack refactor should avoid blocking those commands later.

## Template Layout

The target layout is:

```text
templates/
  stacks/
    dev/
      copier.yml
      template/
        .gitignore.jinja
        angee.yaml.jinja                 # optional; absent for runtime-only dev stacks
    default/
      copier.yml
      template/
        angee.yaml.jinja
        opencode.json.jinja
        agents/
          angee-admin/
            AGENTS.md
  workspaces/
    feature-dev/
      copier.yml
      template/
        CLAUDE.md.jinja
        .envrc.jinja
  agents/
    opencode-admin/
      copier.yml
      template/
        AGENTS.md.jinja
```

`copier.yml` should carry both standard Copier settings/questions and Angee metadata:

```yaml
_min_copier_version: "9.0"
_subdirectory: template
_templates_suffix: .jinja

_angee:
  schema: 1
  kind: stack
  name: dev
  runtime:
    adapter: django-angee
    django:
      manage_py: manage.py
      invoker: uv
      uv: { project: . }
      settings: runtime.settings
  dev:
    runtime:
      django:
        bind: "127.0.0.1:${ports.django}"
    frontend:
      cwd: ui/react/web
  secrets:
    - { name: django-secret-key, generated: true, length: 50 }
  ports:
    - { name: django, answer: django_port, band: django, default: 8100, export_env: DJANGO_PORT }
    - { name: vite,   answer: vite_port,   band: vite,   default: 5173, export_env: VITE_PORT }
  post_init:
    - { run: "uv sync" }
    - { run: "angee migrate" }
    - { run: "angee fixtures load" }

project_name:
  type: str
  default: "{{ _folder_name }}"
  validator: >-
    {% if not project_name %}project_name is required{% endif %}

django_port:
  type: int
  default: 8100

vite_port:
  type: int
  default: 5173
```

Use `_angee` for Angee runtime orchestration metadata, not for rendering. Rendering inputs should remain normal Copier questions/answers.

## Angee Post-render Steps

Copier should render files. Angee should orchestrate runtime-specific side effects after rendering.

Post-render responsibilities:

- Resolve `_angee.secrets` into `.angee/.env`, root `.env`, or stack `.env` depending on template kind.
- Preserve existing secret values on update; generate only missing generated secrets.
- Prompt or error for required external secrets.
- Allocate `_angee.ports` from a host-global allocator unless explicit answers are provided.
- Write `.angee/state/ports.json` for dev/agent workspaces.
- Export declared port/env values into `.env`, `.envrc`, or both as declared by metadata.
- Compile `angee.yaml` into `docker-compose.yaml` when present.
- Initialize runtime data directories when `.angee/stack.yaml` has a `runtime:` section.
- Run framework post-init through the existing adapter: `migrate` and then `fixtures load` now, later `assets load --include-demo` when django-angee lands that replacement.
- Write `.angee/stack.yaml` after successful render and post-render.

Avoid using Copier `_tasks` for Angee's own operations by default. Copier tasks are intentionally unsafe and require trust. Angee post-render steps are first-party orchestration and should stay explicit in Go.

Copier `_tasks` can remain available for trusted user templates, but first-party templates should prefer `_angee.post_init` so the CLI can log, retry, and eventually run the same steps under the server-side platform Job model.

## `.angee/stack.yaml`

Proposed shape:

```yaml
version: 1
template:
  active: stacks/dev
  source: local:templates/stacks/dev
  ref: ""
  locked_sha: ""
  answers_file: .copier-answers.yml

runtime:
  adapter: django-angee
  django:
    manage_py: manage.py
    invoker: uv
    uv:
      project: .
    settings: runtime.settings

dev:
  runtime:
    django:
      bind: "127.0.0.1:${ports.django}"
  frontend:
    cwd: ui/react/web

workspaces:
  default_template: workspaces/feature-dev
  prefix: workspaces
```

This file is committed. It intentionally contains no secret values and should not duplicate Copier answers. Copier answers stay in `.copier-answers.yml`; final runtime allocations such as ports stay in `.angee/state/`.

Keep `.copier-answers.yml` as the Copier-owned file. Do not ask users to edit it manually. `.angee/stack.yaml` is Angee-owned configuration and provenance.

## Secret Handling

Do not rely only on Copier secret questions for Angee secrets.

Reasons:

- Copier `secret: true` prevents persistence in `.copier-answers.yml`, but it does not model generated secrets, derived secrets, or env-file output.
- Current Angee templates need generated and derived values such as passwords, SECRET_KEY, and DATABASE_URL.
- Server-side platform provisioning also needs deterministic metadata for secret generation without an interactive prompt.

Use `_angee.secrets` as the source of truth for runtime secrets:

```yaml
_angee:
  secrets:
    - name: django-secret-key
      generated: true
      length: 50
    - name: db-password
      generated: true
      length: 32
    - name: database-url
      derived: "postgresql://postgres:${db-password}@127.0.0.1:5432/{{ project_name }}"
    - name: anthropic-api-key
      required: true
```

Rules:

- `--secret name=value` wins.
- Existing secret files win on update.
- `generated: true` fills missing values.
- `derived` is recomputed if not explicitly supplied; derived values may reference other secrets and Copier answers.
- `required: true` without a value prompts interactively unless `--yes`/`--defaults`, where it errors.
- Secret values are never written to `.copier-answers.yml` or `.angee/stack.yaml`.

## Port Handling

`AGENT-PROVISIONING.md` requires side-by-side dev agents without port collisions. Stack dev init should grow the same primitive early.

Use `_angee.ports`:

```yaml
_angee:
  ports:
    - name: django
      answer: django_port
      band: django
      default: 8100
      export_env: DJANGO_PORT
    - name: vite
      answer: vite_port
      band: vite
      default: 5173
      export_env: VITE_PORT
```

Rules:

- A value supplied through `--set django_port=8110` wins if free.
- Without a supplied value, allocate from the named band.
- For ordinary single-project dev, the default can be accepted if free.
- For agent/workspace provisioning, the operator should allocate explicitly and pass `--set` values.
- Write `.angee/state/ports.json` with final leases.
- Runtime commands resolve `${ports.<name>}` placeholders from `.angee/state/ports.json`.
- Port leases should eventually live under a host-global lock in `~/.angee/port-allocator/`, matching `AGENT-PROVISIONING.md`.

## Copier-go Integration

`../copier-go` on `refactor-go` is the right base. It already provides the important pieces:

- Library API: `Copy`, `Recopy`, `Update`, `CheckUpdate`.
- Standard `copier.yml` / `copier.yaml` parsing.
- `.copier-answers.yml` writing/loading.
- Jinja-like rendering through `pongo2`.
- `_subdirectory`, `_templates_suffix`, `_exclude`, `_skip_if_exists`, `_answers_file`, `_secret_questions`, `_external_data`, messages, tasks, migrations.
- Git template refs, latest semver tag selection, commit metadata.
- Three-way update flow using Git diffs.
- Unsafe-feature gating for tasks, migrations, extensions, and unsafe external data.

Initial integration can be a local replace while the API settles:

```go
require github.com/fyltr/copier-go v0.0.0
replace github.com/fyltr/copier-go => ../copier-go
```

Release integration should use a tagged module.

### Copier-go Gaps Relevant To Angee

Some gaps are acceptable for first-party templates, but should be tracked:

| Gap | Impact | Recommendation |
|---|---|---|
| Unknown underscore keys are ignored but not exposed. | `_angee` is dropped by `copier-go` after config parse. | Angee should parse `copier.yml` itself before invoking Copier, or copier-go should expose raw config. |
| No public `WithSource` option for update. | `worker.runUpdate` has `cfg.SrcPath`, but callers cannot set it through the public API. | Add `copier.WithSource(src)` so Angee can resolve `stacks/dev` itself and still use Copier update. |
| Stored source metadata may be wrong for merged/inherited templates. | If Angee passes a temp merged template dir to Copier, `_src_path` points at a temp path. | Add a metadata override option or avoid temp dirs until source metadata is solved. |
| Multi-template repos are not Copier's recommended model. | Platform docs want `templates/stacks/dev` inside application repos/packages. | Let Angee own template resolution and call Copier with a concrete directory; rely on `.angee/stack.yaml` for Angee refs. |
| YAML `!include` and multi-doc merge are not fully ported. | Shared template config reuse is limited. | Implement `_angee.extends` in Angee first; do not depend on Copier includes. |
| Pattern matching is glob-based, not full PathSpec/gitignore. | Edge cases in `_exclude` / `_skip_if_exists`. | First-party templates should use simple patterns. |
| Python Jinja extensions are not supported. | Templates cannot use Python-only extension packages. | First-party templates should use plain Jinja/pongo2-compatible syntax. |
| No custom prompter option. | Harder to integrate future Angee UI prompts. | Add later if needed; terminal prompting is enough for v1. |

The smallest useful copier-go API additions for Angee are:

```go
func WithSource(src string) Option
func WithStoredSource(src string) Option          // optional, for answers metadata
func LoadConfig(path string) (*TemplateConfig, map[string]any, []QuestionDef, error) // optional
```

If `WithStoredSource` feels too Copier-specific, Angee can write its own `.angee/stack.yaml` and use `WithSource` for `update`. The answers file can remain Copier-owned even if `_src_path` is less meaningful for Angee-managed templates.

## Resolver Design

Angee should own logical template resolution. Copier should own rendering and update mechanics.

Logical refs:

```text
stacks/dev
workspaces/feature-dev
agents/opencode-admin
gh:org/repo#templates/stacks/dev
./templates/stacks/dev
```

Resolution order for `stacks/dev`:

1. Current stack root: `<root>/templates/stacks/dev/`.
2. First-party embedded or installed templates.
3. User library: `~/.angee/templates/stacks/dev/`.
4. Explicit Git URL form.
5. Explicit local path.

For v1, Go cannot discover Python package entry points directly without adding a Python runtime call. Keep first-party templates in `angee-go` or a small first-party template repo. Server-side `django-angee-platform` can later expose package templates through Python entry points on its side.

## Inheritance

The platform docs propose `_angee.extends`. Keep that, but implement it outside Copier.

Rules:

- Parent first, child overrides.
- Scalars: child wins.
- Maps: recursive deep merge.
- Lists of maps with `name`: merge by name.
- Lists of scalars: append and de-duplicate.
- Template files: child overlays parent by relative path.
- Copier questions: child overrides parent question definitions by key.
- Underscore-prefixed template names such as `_django-base` are abstract and not directly initable.
- Cycles are errors.

Implementation can render an effective template into a temporary directory, but only after source metadata/update behavior is solved. Until then, first-party `stacks/dev` can be self-contained and inheritance can be phase 2.

## Simplifications

This refactor lets us delete or shrink a lot of custom code:

- Replace `internal/tmpl.Render` with `copier.Copy`.
- Replace `TemplateParams` with normal Copier questions.
- Replace `.angee-template.yaml parameters` with `copier.yml` question definitions.
- Replace `angee.dev.yaml.tmpl` special handling with a concrete `templates/stacks/dev` template.
- Replace `.angee/project.yaml` and the `runtime != "" && services empty` sentinel with one `.angee/stack.yaml` manifest containing a `runtime:` section.
- Move `[tool.angee.dev.*]` process configuration out of `pyproject.toml` and into `.angee/stack.yaml` unless a concrete language ecosystem use case proves otherwise.
- Replace `CopyTemplateFiles` / `CopyAgentFiles` for init-time scaffolding with normal Copier-rendered files.
- Keep only Angee-specific post-render code: secrets, ports, compose compilation, runtime adapter post-init.
- Move fixture loading out of template metadata over time and into `angee fixtures load` / `angee assets load --include-demo`.
- Make `init`, `update`, and `stack switch` one pipeline with different entry points.

The remaining init pipeline should look like this:

```text
parse command
resolve logical template ref
load _angee metadata
copier copy/update
resolve secrets
allocate ports
write stack state
compile compose if present
run runtime post-init if present
print next steps
```

## Migration Path

### Phase 1: Add Copier-backed stack dev init

- Add copier-go dependency using local `replace` during development.
- Add `templates/stacks/dev/copier.yml` and `template/` tree.
- Implement `angee init stack dev`.
- Make `angee init --dev` call the same command path.
- Change runtime parent-walk to look for `.angee/stack.yaml` with `runtime:` instead of `.angee/project.yaml`.
- Keep the runtime adapter and `angee dev` orchestration behavior, but load their config from `.angee/stack.yaml`.

### Phase 2: Move current templates

- Convert `templates/default/.angee-template.yaml` and `angee.yaml.tmpl` to `templates/stacks/default/copier.yml` and Jinja templates.
- Convert `../django-angee/examples/angee-notes/.angee-template/` to `templates/stacks/dev/`.
- Remove current `services: []` runtime-only sentinel from docs and code.
- Remove `.angee/project.yaml` output entirely.
- Remove pyproject TOML merge unless a template explicitly needs to edit `pyproject.toml` for package metadata.

### Phase 3: Secrets and ports

- Move current secret resolution into an Angee post-render package that reads `_angee.secrets`.
- Preserve existing `.env` values on update.
- Add `_angee.ports` handling and `.angee/state/ports.json`.
- Add `--set key=value` for Copier answers and port overrides.

### Phase 4: Update and stack switch

- Add `angee update` backed by copier-go `Update`.
- Add `angee stack switch <name>` as marker mutation plus update.
- Add conflict guidance in CLI output.
- Add tests around user-edited files surviving update.

### Phase 5: Workspaces and agents

- Add `templates/workspaces/<name>` resolution.
- Add host-global port allocator under `~/.angee/port-allocator/`.
- Add workspace state under `.angee/workspaces/<name>/state/` or `.angee/agents/<name>/state/`.
- Add `angee agent add` for agent scaffolding, then connect it to compose/platform agent declarations.

### Phase 6: Platform parity

- Reuse the same resolver and metadata schema in server-side `django-angee-platform` provisioning.
- Map `_angee.post_init` steps to platform `Job` rows.
- Map services, repositories, volumes, and ports into platform DB rows instead of local YAML state.

## Testing Strategy

Minimum tests for phase 1:

- `angee init stack dev --yes` creates `.angee/stack.yaml`, `.angee/data/*`, `.angee/state/ports.json`, and `.copier-answers.yml`.
- `angee init --dev --yes` produces the same files as `angee init stack dev --yes`.
- Existing runtime commands can find `.angee/stack.yaml` after Copier-backed init.
- Required secret without `--secret` fails in non-interactive mode.
- Generated secret is preserved on update.
- Existing `.gitignore` is not clobbered if template marks it skip/merge-safe.
- User edits to a rendered file survive `angee update` or produce Copier conflicts.
- `--set django_port=8110` appears in rendered files and `.angee/state/ports.json`.

## Open Questions

- Should first-party templates live in `angee-go/templates/` long term, or in a separate `angee-templates` repository?
- Should `templates/stacks/dev` be self-contained in v1 and defer `_angee.extends`, or should inheritance land before the first conversion?
- Should `angee init` without args prefer `stacks/dev` or `stacks/default` when both exist?
- Should `angee init stack dev` run `uv sync` itself, or leave that as a declared `_angee.post_init` step in the template?
- Should current deploy-time agent file rendering remain Go `text/template`, or should agent files become Jinja/Copier-rendered during `angee up` in a later pass?
- Should `.copier-answers.yml` be committed for all stack roots? Copier recommends it for updates; Angee should follow that unless a secret leak risk appears.
- Should the single manifest be named `.angee/stack.yaml` or `.angee/manifest.yaml`? Lean: keep `stack.yaml` because `angee init stack dev` makes the term concrete.
- How should Angee represent logical template refs in `.copier-answers.yml` when templates are resolved from package/embedded sources or merged through `_angee.extends`?

## Recommendation

Implement the refactor around this boundary:

- Copier-go owns template rendering, answers, and update mechanics.
- Angee owns template resolution, `_angee` metadata, secrets, ports, runtime post-init, compose compilation, and platform/workspace state.

Do not teach Angee another general-purpose template engine. Do not encode runtime-only behavior through `services: []`. Do not keep a separate project-mode manifest. Make `.angee/stack.yaml` the single Angee marker, make `templates/stacks/dev` the concrete dev bootstrap, and make `angee init --dev` a plain alias for it.
