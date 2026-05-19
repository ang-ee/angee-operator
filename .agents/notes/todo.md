# Angee Operator TODO

Follow-ups from the GitOps topology/operator manager pass.

## GitOps API

- [ ] Document the GraphQL GitOps API in `docs/reference/operator-api.md`:
  `gitOpsTopology`, `workspaceSourceFetch`, `workspaceSourcePull`, and
  `workspaceSourcePush`.
- [ ] Add a real-time topology update channel so clients can observe
  workspace/source divergence and convergence without manual refresh.
- [ ] Add conflict detection for workspace source pull/merge paths before
  exposing higher-level "bring together" flows.
- [ ] Add diff metadata for GitOps links: changed files, staged/unstaged
  counts, and commit range summaries.
- [ ] Add safe convergence operations beyond fetch/pull/push: merge, rebase,
  abort/continue when supported, and explicit branch publish flows.

## Operator Hardening

- [ ] Add integration tests for behind and diverged worktree states.
- [ ] Add integration tests for dirty worktrees blocking pull/push.
- [ ] Add coverage for missing workspace source paths and undeclared source
  references inside `gitOpsTopology`.
- [ ] Decide whether topology refresh should be polling, SSE, GraphQL
  subscription, or operator event stream.

## From angee-django operator-addon plan (2026-05-19)

These asks block v1.1 of the Django-side `operator` addon (see
`angee-django/.agents/plans/009-operator-addon.md`). The Django v1 ships
without any of them — log panels poll, GitOps renders as p0's lane SVG, the
token is a long-lived bearer, and the agents addon declares its template
inputs in Django settings instead of introspecting the daemon. Landing the
items below unlocks the corresponding Django-side follow-ups one for one.

- [ ] **Add a `Subscription` root type to `internal/operator/schema.graphql`.**
  Operator schema currently exposes only `Query` (line 216) and `Mutation`
  (line 234). Subscriptions enable live GitOps topology refresh, live
  service/workspace log tailing, and live `workspaceStatus` changes. Specific
  ops to land first: `onGitOpsTopologyChange`, `onServiceLogs(name)`,
  `onWorkspaceLogs(name)`, `onWorkspaceStatusChange(name)`. SSE transport is
  the cheap fit; subscriptions here are unidirectional. (Supersedes /
  resolves the existing "Add a real-time topology update channel" item
  above.)
- [ ] **Expose diff payloads for the v1.1 GitKraken-style diff panel.**
  Add a per-link / per-workspace `diff(...)` query returning a list of changed
  files plus per-file hunks: `[{oldPath, newPath, mode, hunks: [DiffHunk]}]`,
  with `DiffHunk` carrying standard unified-diff lines + header offsets. The
  Django side will mount `@git-diff-view/react` against this shape. (Resolves
  the existing "Add diff metadata for GitOps links" item above — but with the
  concrete operation shape pinned.)
- [ ] **Add commit-DAG fields on `GitOpsTopology`.** For a real DAG renderer
  (React Flow + ELK), per-source commit parents in the relevant window:
  `sources[].commits: [{sha, parents: [String!], refs: [String!], time,
  summary}]`. Heavy by default — gate behind a query argument (e.g.
  `withCommits: Int` for window size) so cheap polling stays cheap.
- [ ] **Add `workspaceCreatePreflight(input)` mutation.** Validates
  `[KeyValueInput]` against the copier template's `_required` declarations
  without materialising. Lets Django callers surface validation failures
  earlier and avoid partial state on input-shape mismatches.
- [ ] **Add per-actor scoped token mint.** A mutation
  `mintConnectionToken(actorSqid: String!, ttl: String): String!`, scoped by
  the daemon's existing admin bearer. Unlocks v2 "per-actor scoped tokens"
  on the Django side so the browser stops holding a shared admin bearer.
- [ ] **Expose template-descriptor introspection.** Add `templates:
  [TemplateDescriptor!]!` and `template(ref: String!): TemplateDescriptor`
  queries; descriptor carries the input schema parsed from copier's
  `copier.yml`. Lets the Django side retire the `ANGEE_OPERATOR_TEMPLATES`
  settings convention.
- [ ] **Lock the `agent-runtime` template's env-var contract.** The Django
  `agents` addon (`angee-django/.agents/plans/007-agents-addon.md` §10)
  provisions agent workspaces via `workspaceCreate(template="agent-runtime",
  inputs=[…])`. The operator-side copier template needs a documented env-var
  contract: at minimum `AGENT_ID`, `MCP_URL`, `MCP_TOKEN`, `ACP_PORT`,
  `ACP_TOKEN`. The template materialises into a runtime process listening on
  `ACP_PORT` with bearer `ACP_TOKEN`, reachable from Django via the daemon's
  network brokering. Pin the contract in `docs/guide/templates.md` (or a new
  `docs/reference/templates.md`) so Django and the template repo can sync
  independently. **Prerequisite:** the `agent-runtime` template does not yet
  exist in this repo — it must be authored (under `templates/agent-runtime/`
  via copier, or in a sibling template repo) before the env-var contract can
  be locked.
