# Proposal: drive sources and workspace slots with Jujutsu (jj)

**Status:** Draft · **Area:** sources, workspaces, gitops · **Surfaces:** CLI + REST + GraphQL (operator), stack and workspace templates (angee-django)

## Summary

Replace git worktrees as the workspace-slot mechanism with jj workspaces, and
replace the operator's merge/rebase state machine with jj's first-class
conflicts. Every upstream repository keeps exactly one clone on the machine,
which becomes that repository's jj primary workspace. Each slot of an Angee
workspace becomes a secondary jj workspace of that primary: a working copy that
exists before it has a branch, rebases without blocking when its base moves,
and records a conflict as data inside a commit instead of leaving the checkout
in a `MERGING` or `REBASING` state an agent cannot recover from.

The operator keeps owning everything jj does not know about: which slots form
one Angee workspace, the base ref per source, branch namespaces, remote roles,
publish policy, and the cross-repo topology. jj is driven through its CLI with
operator-owned JSON templates behind a `vcs` interface; git remains the
default until the switch is opted into per stack. GitHub, CI, `gh`, Copier,
uv, and pnpm keep seeing plain git.

The research behind this proposal (jj capability audit, integration options,
prior art, tooling survey) is recorded in the private work-state under
`research/2026-09-28-jj-and-multi-repo-flow/`.

## Problem

- **Agents get stuck.** `workspaceSourceMerge`, `Rebase`, `MergeAbort`,
  `RebaseAbort`, and `RebaseContinue` (`internal/service/gitops_merge.go`)
  exist because a git conflict halts the checkout. An agent that hits one mid
  task has to know the state machine; several did not, and the workspace was
  abandoned rather than repaired.
- **A slot cannot exist without a branch.** `WorktreeAddBranch` cuts every
  slot on `ws.Branch` at creation (`internal/service/workspaces.go`), and
  `workspacePush` pushes every slot's branch. Untouched repositories end up
  with empty branches on the remote; a workspace that only touches arpee still
  publishes a framework branch.
- **Drift is manual.** `workspaceSyncBase` merges or rebases each slot on
  demand. When the base moves, nothing follows it until someone runs the verb,
  and a `--no-ff` merge per sync produced the merge-commit history on the old
  `workspace/src` branches that could never become a pull request.
- **One clone per stack.** Each stack materializes its own cache under
  `sources/` (`Platform.sourcePath`, `internal/service/sources.go`), so a
  machine with several stacks holds several full clones and a developer cannot
  see all branches of all remotes in one place. The manifest now accepts an
  absolute `cache_path`, and the stack template renders a shared
  `sources_home`, so the shared clone exists in practice; the slot mechanism
  is what still assumes git worktrees.

## Current behavior (the machinery this replaces or keeps)

- `internal/git/git.go` (665 lines): a concrete `Client` struct, not an
  interface. The service layer calls nineteen distinct methods; the heaviest
  are `Dirty`, `Push`, `WorktreePrune`, `Upstream`, `Fetch`, `AheadBehind`,
  `CloneRef`, and the worktree add/remove/registered trio. `pushRemote()`
  already resolves the git triangular precedence (`branch.<b>.pushRemote`,
  `remote.pushDefault`, tracking remote, `origin`), and `SyncBaseRef()`
  already prefers `origin/<ref>`.
- `internal/service/gitops_merge.go` (232 lines): the merge/rebase verbs, the
  `ls-files -u` conflict enumeration, and `gitOpEnv()`, which hands git a
  stripped environment. That environment drops `GH_TOKEN`, so on a machine
  whose GitHub credentials come from the `gh` credential helper, every
  operator-driven push fails with "could not read Username". Fetch and clone
  in `git.Client.Run` inherit the full environment and work.
- `internal/service/sources.go`: `materializeSource` clones or fetches the
  cache (`fetch --all --prune`), best-effort on bring-up.
- `internal/service/commits.go` and read-only go-git calls in `git.go`:
  `PlainOpen`, `Head`, `Log`, `References`, `CommitObject`, `Remotes`.
- `internal/operator/gql/events.go`: `pollTopology` re-reads the topology on
  a ticker, hashes it, and publishes on change. No file watching.
- `manifest.Source` carries one `Repo` URL; `manifest.WorkspaceSource` has
  `Mode` (`worktree` or `clone`) and a required `Branch` for worktrees.

## Ownership (the load-bearing decision)

| Concern | Owner after this proposal |
|---|---|
| Working copies, conflicts, rebase, undo, concurrency inside one repo | jj |
| Which slots form an Angee workspace; ports; templates; lifecycle | operator |
| Base ref per source; sync cadence | operator (manifest fact) |
| Branch names (`<stack>/<workspace>`, `feature/<topic>`) | operator, rendered by the stack template |
| Remote roles (canonical fetch-only, push default) and publish policy | operator (manifest fact) |
| Which branches a remote accepts | GitHub rulesets |
| Cross-repo coordination, CI, PR ordering | operator + CI convention |
| Graph, diff, op-log views inside one repo | lightjj (external, linked from the console) |

An Angee workspace and a jj workspace are different levels: one Angee workspace
is N jj workspaces, one per slot, each on a different repository's primary.
The operator names them `<stack>/<workspace>/<slot>` so `jj workspace list` on
any repository reads as the list of Angee workspaces across all stacks.

## Proposal

### 0. Prerequisites that are git-era changes jj inherits

These land first and are worthwhile on git alone.

1. **Credential passthrough.** `gitOpEnv()` inherits `GH_TOKEN`,
   `GITHUB_TOKEN`, and `GIT_ASKPASS` in addition to the current list. Without
   this neither `ws source publish` nor the future `jj git push` can
   authenticate through the `gh` helper.
2. **Remotes with roles.** `manifest.Source` gains `remotes: map[name]url`
   and `push_default: name`; the single `repo` form stays as a shorthand for
   `remotes: {origin: repo}`. The operator renders `remote.pushDefault` and,
   for a remote marked canonical, a disabled push URL. Publish refuses a
   branch that does not match the remote's `branches` allow-list; this is the
   only guard for private repositories on a GitHub plan without rulesets.
3. **`link` slot mode.** A git source's slot may be a symlink to the primary
   checkout itself (`mode: link`), for the shared work-state whose one `main`
   working copy every workspace binds. Today that shape is only expressible
   as a `kind: local` source, which loses drift reporting.
4. **Optional `branch` on worktree slots.** A slot without a branch is cut
   detached on its base; the branch is created on first commit. This is the
   git-era version of lazy bookmarks and lets `workspacePush` and publish skip
   slots with no commits beyond base, which they should do regardless of VCS.

### 1. A `vcs` interface with two drivers

Extract the nineteen methods the service layer uses from `git.Client` into
`internal/vcs.Driver`, keep the git implementation as is, and add
`internal/vcs/jj`. The jj driver:

- shells out to the pinned `jj` binary the way the git client shells out to
  git, with `--ignore-working-copy --no-pager --color never` injected on every
  read and `-m` forced on every mutation so nothing can open an editor or
  pager;
- owns NDJSON templates (`json(self)` and explicit field templates) for
  `jj log`, `jj workspace list`, `jj bookmark list --all-remotes`, and
  `jj op log -n 1`, decoded with `encoding/json`, frozen by golden tests
  against the pinned jj version, because jj documents its serialized field
  names as usually stable but not guaranteed;
- enforces a floor in `doctor`: jj 0.44 or newer for serializable maps,
  workspace roots in templates, and `--no-integrate-operation`; git 2.42 or
  newer for orphan worktrees, which colocated secondary workspaces use;
- treats stderr as opaque and keys decisions on exit status, since jj has no
  structured error contract;
- keeps go-git for raw blob and tree reads on the colocated `.git`, and never
  for change ids, conflicts, workspaces, or the operation log, which git
  either does not store or stores as pseudo-directories that mislead.

No Rust sidecar and no cgo: jj has no C ABI, the operator builds with
`CGO_ENABLED=0` everywhere, and an RPC mode is on jj's roadmap without a
date. When it ships, the same `Driver` interface re-points at it.

### 2. Slot materialization

For a stack with `vcs: jj` (stack-level switch, default `git`):

- The source cache is initialized colocated: `jj git init --colocate` on the
  existing clone, `.jj/` added to `.git/info/exclude`. The primary stays
  parked on `main`; the operator never runs a mutation in it.
- A worktree slot becomes `jj workspace add --colocate --name
  <stack>/<workspace>/<slot> -r <base> <path>`. The `--colocate` flag is
  merged on jj main and expected in the release after 0.45; on 0.45.1 a
  secondary workspace has no `.git` and git tooling fails inside it, which is
  why the switch is opt-in and gated in `doctor`.
- `WorktreeRemove` and `WorktreePrune` become `jj workspace forget` plus the
  directory removal the operator already does.
- `ws create --sync` reconciliation reads `jj workspace list` instead of
  `git worktree list`.

### 3. Branches, bases, publish

- `WorkspaceSource.Branch` is the bookmark the operator sets on a slot's
  commits when it publishes; absent, the slot is anonymous. Namespacing is
  unchanged: the stack template renders `branch_prefix`, and a shared
  `feature/<topic>` is an explicit per-workspace value.
- `workspaceSyncBase` becomes `jj git fetch` on the primary followed by
  `jj rebase -d <base>` per slot. There is no `--merge` variant, no
  `--continue`, and no `--abort`: a conflict rebases anyway and is reported.
  The operator runs this after every fetch that moved the base, on a
  configurable cadence, instead of on demand only.
- `workspaceSourcePublish` becomes `jj bookmark set <branch> -r <slot>@-` then
  `jj git push --remote <push_default> --bookmark <branch>`, only when the slot
  has commits beyond its base. jj refuses to push conflicted, empty-description,
  or private commits (`git.private-commits`), which the operator surfaces as a
  validation error rather than overriding.
- `workspaceSourceMerge` maps to `jj new <slot>@ <ref>` for the rare explicit
  merge; `MergeAbort`, `RebaseAbort`, and `RebaseContinue` are removed from the
  jj path and rejected with a clear error on a jj stack.

### 4. Status, conflicts, and events

- `WorkspaceSourceStatus` gains `conflicted: Boolean!` and
  `conflict_paths: [String!]!`, read from the `conflicts()` revset scoped to
  the slot, and `change_id` beside `current_ref`. `ahead`/`behind` come from
  `<base>..<slot>@-` and the reverse.
- `GitOpsTopology` reports every jj workspace of every primary, including ones
  another stack owns, so the console matrix and lightjj agree on what exists.
- The event hub replaces `pollTopology` for jj stacks with an fsnotify watch
  on `<primary>/.jj/repo/op_heads/heads`, debounced, falling back to the
  op-log poll. This is the pattern jj's own FAQ documents and the one lightjj
  uses; it turns the topology subscription into push.

### 5. Agent contract

One workspace per agent. Reads with `--ignore-working-copy`. The operator
serializes its own mutations per primary with the existing `beginMutation`
lock; a human running `jj` in the same workspace concurrently is tolerated by
jj's lock-free operation log but produces divergent operations the operator
reports rather than resolves. `snapshot.auto-update-stale` is set so a
workspace rewritten from another workspace repairs itself.

## Design options

### A. Opt-in per stack behind a `vcs` switch, staged (recommended)

Prerequisites first on git, then the driver and jj slots for stacks that set
`vcs: jj`, then removal of the git state machine once no stack needs it.
Existing stacks are untouched until they opt in. Rollback per stack is
`vcs: git` plus re-cutting slots.

### B. Big-bang replacement of the git layer

Smaller end state, but it forces every stack through the migration at once
and depends on a jj release not yet published. Rejected for timing.

### C. Colocate per developer only, operator stays on git

What the machine-level setup already does today: developers get jj's
conflicts and undo in the shared primaries and lightjj over them, while slots
remain git worktrees. Zero operator change, and the right interim state, but
it leaves the merge state machine and the manual sync in place. This is the
current baseline, not the destination.

## Migration

- Existing git worktree slots cannot be adopted by `jj workspace add`; a slot
  is migrated by committing its work, removing the worktree, and cutting a jj
  workspace on the same branch. A `ws source migrate <workspace> <slot>` verb
  does exactly that and refuses on a dirty slot.
- The primaries are already shared and jj-colocated on the reference machine;
  the manifest `cache_path` and stack template `sources_home` express that.
- The angee-django templates need `**/.jj/**` in every Vite and vitest watcher
  ignore list before jj runs inside a slot: Vite watching `.jj` corrupts the
  working-copy lock and hangs vitest. This is a cross-repo prerequisite.
- The `change-id` commit header jj writes by default since 0.30 is invisible
  to git and GitHub; `git.write-change-id-header = false` is available if
  byte-identical commits ever matter.

## Backward compatibility

- `vcs` defaults to `git`; every current verb, output field, and manifest
  shape is unchanged for git stacks.
- On a jj stack, `workspaceSourceMergeAbort`, `RebaseAbort`, and
  `RebaseContinue` return a structured error naming the reason. Clients that
  call them only after a conflict never reach them, because a jj rebase never
  leaves an in-progress state.
- The remotes map, `link` mode, optional `branch`, and credential passthrough
  are additive and apply to both drivers.

## Security

No new auth surface. Credential passthrough forwards environment variables the
operator's own process already holds. jj's push refusals are stricter than
git's, not looser. The primaries are per repository, so a private repository's
objects never enter another repository's store; git namespaces are explicitly
not used for that isolation.

## Out of scope

- lightjj and the console's topology matrix: they consume `GitOpsTopology` and
  link to jj workspaces by path; nothing here changes their contract beyond the
  added status fields.
- Cross-repo CI resolution of a framework branch by name and `Depends-On`
  footers: an angee-django and consumer-CI concern.
- Rulesets on private repositories, which depend on the GitHub plan.
- The eventual `jj api` RPC transport; the `Driver` seam is where it plugs in.

## Acceptance

- `doctor` reports jj and git versions against the floors and refuses
  `vcs: jj` below them.
- On a `vcs: jj` stack, `ws create` yields one colocated jj workspace per git
  slot, with a working `.git` inside, `git rev-parse` succeeding there, and no
  branch until the first publish.
- A base move followed by sync-base rebases every slot; a conflicting slot is
  reported with its paths in `WorkspaceSourceStatus`, the workspace stays
  usable, and no verb is required to continue.
- Publish pushes only slots with commits beyond base, to the source's push
  default, refusing a conflicted or empty-description commit with a
  structured error; `ws push` authenticates on a machine whose credentials
  come from the `gh` helper.
- The topology subscription emits within one second of a commit made by a
  human running `jj` in a slot, without polling.
- The git driver passes the existing test suite unchanged; the jj driver's
  golden templates fail loudly when a jj upgrade changes a field.
- `ws source migrate` converts a clean git worktree slot to a jj workspace on
  the same branch and refuses a dirty one.

## See also

- [`internal/git/git.go`](../../internal/git/git.go): the client to extract
  into the driver interface; `pushRemote()` and `SyncBaseRef()` carry the
  remote-resolution logic the jj driver reuses.
- [`internal/service/gitops_merge.go`](../../internal/service/gitops_merge.go):
  the state machine and `gitOpEnv()` this proposal removes and fixes.
- [`internal/service/workspaces.go`](../../internal/service/workspaces.go):
  slot materialization and the `.git` pointer assumptions in staging.
- [`internal/operator/gql/events.go`](../../internal/operator/gql/events.go):
  `pollTopology`, replaced by the op-heads watch.
- [global-source-registry](./global-source-registry.md): registers sources
  by id into the same per-repository cache; with shared primaries the registry
  and the stack sources converge on one clone per upstream.
- jj documentation: workspaces, bookmarks, conflicts, templates, and the
  CHANGELOG entries for `--no-integrate-operation` (0.41), the removal of
  `--allow-new` and the old auto-tracking keys (0.42), and colocated
  secondary workspaces (unreleased at 0.45.1).
