# Proposal: `stack update` re-renders the manifest from its template

**Status:** Draft · **Area:** stacks, templates, copier · **Surfaces:** CLI + operator

## Summary

Make `angee stack update` re-render `angee.yaml` from the stack's Copier
template (preserving runtime-managed state and local edits) before it
regenerates the derived runtime files. Today `stack update` only regenerates
`process-compose.yaml` / `docker-compose.yaml` / `.env` from the *existing*
manifest, so template changes never reach an already-initialized stack. This
brings `stack update` to parity with `workspace update`, which already re-renders
its templates.

## Problem

When a stack template gains or changes a `service`, `job`, `port`, or `source`,
existing stacks do not pick it up. There is no in-place "apply the template's
latest shape to this manifest" operation:

- `stack init` renders the template, but `--force` overwrites the whole manifest
  and drops operator-managed state (`operator.port_pool`, `workspaces`,
  `port_leases`).
- `stack update` regenerates derived files but reads the manifest as-is — it
  never touches `angee.yaml`.

Concrete case that motivated this: the `dev` stack template added a `frontend`
(Vite) service and a `deps` job. Stacks initialized before that change kept only
`web`; `stack update` faithfully regenerated `process-compose.yaml` without a
frontend, because the manifest still had none. The only recovery paths were a
destructive `stack init --force` or hand-editing `angee.yaml` — neither is a
template *update*.

This is an asymmetry, not just a gap: **`workspace update` already re-renders its
Copier templates** (see [Commands](/guide/commands) → Workspaces: "create/update
render Copier templates, including any chained inner-stack templates"). Top-level
stacks should behave the same way.

## Current behavior

- `StackUpdate` is a thin pass-through: `internal/service/stack.go` →
  `StackUpdate(ctx)` calls `StackPrepare(ctx)` and returns.
- `StackPrepare` (`internal/service/platform.go`) loads the existing manifest
  (`LoadStack`), resolves secrets and sources, `Compile`s, and `writeCompiled`s
  the derived files. **It does not render any template.**
- `stack init` renders via `copierx.LocalRenderer{}.Copy(...)`
  (`internal/service/stack.go`), passing `copier.WithAnswersFile(...)`, which
  writes `.copier-answers.yml` at the stack root.

## What already exists (and is unused)

This proposal mostly *connects* parts that are already built:

- **`Renderer.Update`** — `internal/copierx/copierx.go` defines
  `Renderer.Update(ctx, UpdateRequest)` and `LocalRenderer.Update` calls
  `copier.Update(dest, copierOptions(...)...)` with the same answers-file option
  as `Copy`. **It is never called anywhere in the codebase.**
- **The answers file** — `stack init` already writes `.copier-answers.yml` at the
  stack root, recording every input plus `_src_path` (the template path). A
  re-render has the inputs it needs without re-prompting.
- **`manifest.Template{ Active, AnswersFile }`** — already populated in
  `angee.yaml` (`template.active: stacks/dev`, `template.answers_file:
  .copier-answers.yml`). The metadata to locate the template + answers is
  persisted today.
- **`manifest/ensure.go`** — the `_angee.ensure` invariant check that a re-render
  should re-run.

## Proposal

`stack update` gains a template-render step in front of the existing prepare:

1. Resolve the template from `manifest.Template.Active` (fall back to
   `.copier-answers.yml` `_src_path`).
2. Re-render the template into the stack root using the recorded answers, merged
   into the current `angee.yaml` (see [Merge contract](#merge-contract)).
3. Re-run `_angee.ensure` invariants against the merged manifest; fail fast on
   violation.
4. Run the existing `StackPrepare` to regenerate `process-compose.yaml` /
   `docker-compose.yaml` / `.env` from the refreshed manifest.

Steps 2–4 are idempotent: when the template is unchanged, the manifest is
byte-stable and only derived files are rewritten (today's behavior).

### Merge contract

`angee.yaml` is one file that mixes **template-origin** sections with
**operator-managed** runtime state. The re-render must refresh the former and
preserve the latter:

| Section | Origin | On re-render |
| --- | --- | --- |
| `version`, `kind`, `name` | template | refresh |
| `sources`, `secrets`, `volumes` | template (+ user-added) | refresh template keys; keep user-added keys |
| `ports`, `services`, `jobs` | template (+ user-added) | refresh template keys; keep user-added keys; keep allocated port *values* |
| `secrets_backend` | template (ensure) | refresh; fail if it violates `ensure` |
| `operator` (`port_pool`, tokens) | **runtime** | preserve verbatim |
| `workspaces` | **runtime** (`workspace create`) | preserve verbatim |
| `port_leases` | **runtime** | preserve verbatim |
| `template` | metadata | refresh `active`/`answers_file` |

The invariant: **a key the template emits is owned by the template; a key only
the operator/user added is preserved.** Refreshing a template-origin `service`
must not discard a runtime-allocated port *value* on a template-origin `port`.

## Design options

### A. Reuse copier's file-level 3-way merge (`Renderer.Update`)

Wire `stack update` to call the existing `LocalRenderer.Update`. Copier performs
a git-style 3-way merge per file; operator-managed sections survive as "local
edits" not present in the template output.

- **Pro:** minimal code — the renderer, options, and answers file already exist.
- **Con / open question:** copier's 3-way needs a *base* (the original render).
  For **local, non-git templates** (our `_src_path` case, with no `_commit` in
  the answers) it is unclear whether copier-go reconstructs a base or falls back
  to overwrite. A 2-way overwrite would **clobber** `operator`/`workspaces`/
  `port_leases`. Line-based merge can also leave conflict markers in a file the
  operator must parse. **This must be verified before relying on Option A.**

### B. Structured manifest merge in the operator (recommended)

Render the template to a fresh in-memory manifest (`theirs`), parse the current
manifest (`ours`), and merge by section and key per the table above:

- template-origin sections: `theirs` wins for keys it emits; `ours`-only keys
  preserved; allocated `port` values carried over.
- runtime sections (`operator`, `workspaces`, `port_leases`): `ours` verbatim.
- emit canonical YAML — no conflict markers in a machine-read file.

- **Pro:** deterministic, YAML-structure-aware, fail-fast on genuine conflicts,
  independent of copier-go's local-template merge semantics. Fits the manifest's
  role as machine-read state.
- **Con:** more operator code than Option A; needs the section/provenance rules
  encoded once.

**Recommendation:** Option B. The manifest is operator-read state; a line-based
merge that can emit conflict markers or clobber runtime sections is the wrong
tool. Render-then-structurally-merge is deterministic and matches Angee's
"compile one manifest" model. Keep `Renderer.Update` (Option A) as a fast path
only if copier-go is confirmed to 3-way local templates without a base and to
preserve non-template blocks.

## Command semantics & backward compatibility

`stack update` today means "regenerate derived files." This proposal makes it
"refresh the manifest from the template, then regenerate derived files" — the
natural meaning of *update*, and what `workspace update` already does.

- Make the re-render the **default**, with `--no-template` to keep the
  derived-files-only behavior.
- Add `--dry-run` to print the manifest diff without writing — important for a
  machine-read file; let an operator/agent review template-driven changes before
  applying.
- A conservative rollout can ship the re-render behind `--template` (opt-in) for
  one release, then flip the default.

## Conflicts, invariants, migration

- **Conflicts:** when a template-origin key changed in both template and manifest
  in incompatible ways, fail with a structured report (section, key, both
  values). Never write a half-merged or marker-laden manifest.
- **Ensure:** re-run `_angee.ensure` after merge; a template that now requires an
  invariant the manifest violates is a fail-fast error, not a silent overwrite.
- **Migration:** stacks initialized before answers files were written lack
  `.copier-answers.yml`. Reconstruct answers from the manifest (`template`,
  `ports`, `sources`, names) or require a one-time `stack init --force`. Stacks
  created by current `stack init` already have the file.

## Out of scope

- Chained inner-stack re-render under `workspace update` already renders
  templates; this proposal only closes the top-level `stack update` gap. Any
  shared merge helper should be reused by both.
- Version-pinned template updates (`_commit`-based) for git-sourced templates —
  worth a follow-up once local-template re-render lands.

## Acceptance

- `stack update` on a stack whose template gained a service adds that service to
  `angee.yaml` and to the regenerated `process-compose.yaml`, while
  `operator`/`workspaces`/`port_leases` and allocated port values are unchanged.
- `stack update` on an up-to-date stack is a no-op for `angee.yaml`.
- `stack update --dry-run` prints the manifest diff and writes nothing.
- A conflicting template change fails with a structured error and leaves
  `angee.yaml` untouched.
