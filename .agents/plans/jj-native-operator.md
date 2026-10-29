# jj-native operator: implementation plan

**Status:** Draft, revised after a Codex review (2026-10-01) and the owner's decisions of 2026-10-05, 2026-10-09, 2026-10-10 and 2026-10-11 · **Created:**
2026-10-01 · **Source:**
[`docs/proposals/jj-native-operator.md`](../../docs/proposals/jj-native-operator.md)
(merged in #90), a read of `internal/` at `60fa9c2`, and nine scratch-repo
probes of jj `0.45.1-f8f808f` (a build from jj main) with git 2.50.1.
File:line references are valid at `60fa9c2`.

This plan turns the proposal into mergeable pull requests. The proposal owns
the *why* and the ownership split between jj and the operator; this file owns
the *order*, the files, and the decisions the proposal left open or got wrong.

**Where it stands (2026-10-29):** phase 1 is merged (#93) and released in
v0.16.0. Phase 4a (reads behind the driver) is merged as #107
(`cb27d2e`), phase 2 steps 1-4 (remotes, push default, canonical remote,
cache reconcile) as #116 (`12e69d1`), 4b (verbs behind the driver) as #117
(`86fc88d`), 4d (driver-owned push and publish rules) as #120 (`adcc2a2`),
and phase 2 step 5 (publish's default remote, the `branches` allow-list and
a remote the slot doesn't have) as #122 (`a242891`), which completes phase
2. 4d and step 5 land in the release after v0.21.3. Phase 5 steps 1-5, 7
and 8 (the `jjdriver` package, the doctor probe and the jj CI job) are
merged as #126 (`6315a53`), and 4c (the slot lifecycle behind the driver,
and the primary lock of D9) as #125 (`39cbd1f`), which completes phase 4.
Phase 5 step 6 (the jj slot lifecycle) is merged as #131 (`2708e40`), which
completes phase 5, and the prerequisites phase 6 found as #138 (`0e5b28a`,
the cache's git format) and #139 (`4775021`, Q10 (a) and a pushed verdict
on a missing base). Phase 6 (`mode: jj` lifecycle and status) is merged as
#140 (`7eccb17`) and released in v0.24.0. Phase 7 (status fields and
topology) is merged as #142 (`50a4f38`).
Q1 (jj slots are created in place), Q2 (2026-10-09:
`<stack>--<workspace>`, a clash is refused), Q6 (2026-10-10: slot state is
read on demand, so every read snapshots and there is no interval, which
drops phase 10), and Q3 to Q5 (2026-10-11: `link` mode is dropped, sync
runs only on request, and CI pins jj in `.github/jj-rev`) are answered.
Q8 (a jj slot pushes to git's push remote) and Q9 (the runner passes
`snapshot.auto-update-stale` on every call) are built as recommended in
#126, pending the owner's confirmation, and so is Q10 (files jj will not
snapshot: jj's own report counts as dirty) in #139; what jj does not report,
the owner answered on 2026-10-29: the destroy guard refuses a jj slot that
holds a nested repository, and files outside sparse patterns are
unsupported. Q7 is answered (2026-10-29): the verbs that walk a whole
workspace act only on slots whose manifest names a mode. Phase 8's first
half (sync-base and rebase on jj slots, the identity fallback, and the
preconditions sync-base needs answered per driver) is merged as #143
(`8ba74d9`). Its second half (publish, push, merge, fetch and diff on jj
slots, and every slot verb's preconditions answered per driver) is in review
in the PR that adds "As built: publish, push and merge" to phase 8; with it a
jj slot does everything a git slot does but migrate (phase 9), and is never
pulled.

Operator lanes: the angee-operator primary (`~/Work/sources/angee-operator`)
was jj-colocated on 2026-10-05 at the owner's request. Each new lane is a jj
workspace of it (`jj -R ~/Work/sources/angee-operator workspace add
--colocate --name angee-operator--<task> -r main@origin <path>`), published
under a `dev-alexis/<task>` bookmark: the repository's `branch-namespaces`
ruleset only accepts `main`, `release/**`, `feature/**` and `dev-*/**`.

Terms used below:

- **primary**: the one clone of an upstream repository on the machine (a
  `kind: git` source cache, or a `kind: local` store). Workspaces attach to it.
- **slot**: one source inside one Angee workspace
  (`workspaces/<name>/<subpath>`). Today a git worktree, a clone, or a symlink.
- **jj slot**: a slot whose effective mode is `jj`; a secondary jj workspace of
  the primary, colocated so it also has a working `.git`.
- **`N`**: the jj workspace name of a slot. **`B`**: the slot's resolved base
  (`<ref>@<remote>` when that exists, else `<ref>`).

## 1. Decisions

Fixed by the proposal and the owner:

1. The switch is per slot (`mode: jj`) with a stack-level `vcs` default. Git
   stays the default. One stack may mix git and jj slots.
2. **The jj baseline is the latest jj, not the last stable release.** Nothing
   waits for a release that carries `jj workspace add --colocate`. `doctor`
   and the service probe the capability and refuse `mode: jj` without it.
3. jj is driven through its CLI. No cgo, no Rust sidecar.

Added by this plan, from the code read, the probes and the review. Each one
changes what gets built, so challenge them here rather than in a PR:

| # | Decision | Why |
|---|---|---|
| D1 | An unknown slot `mode` is a validation error. | Today any mode other than `worktree` on a git source silently takes the full-clone path (`workspaces.go:1610` falls through to `CloneRef`), and a local source is symlinked. `mode: jj`, or any typo, would do the wrong thing quietly. |
| D2 | The seam is a slot-level `vcs.Driver`, not the `git.Client` methods lifted into an interface. | `Upstream`, `Dirty`, `PushSetUpstream`, `WorktreeRegistered` have no jj meaning. An interface shaped like git would force the jj driver to fake them. |
| D3 | An omitted mode keeps today's behaviour under `vcs: git`: a git source is **cloned**, a local source is **symlinked**. | The proposal says an omitted mode means `worktree`. The code says clone. Changing that would re-materialize every existing work-state slot. |
| D4 | **Decided 2026-10-05: a jj slot is created in place**, with `jj workspace add --colocate` writing straight to its final path. The guard verifies the parent and an absent or empty destination before, verifies the path identity after, and rolls back (forget, remove, prune) on a mismatch or any error. No staging, and no edit of jj's files. | Staging protects against a parent directory swapped for a symlink during the checkout, and makes the slot appear atomically. For a fresh create nothing can write the new workspace directory yet. In an in-use workspace the agents can already write the shared jj store, so the guard adds little. jj refuses a non-empty destination, so a swap can only create files elsewhere, and the after-check catches it. A moved jj workspace also needs its path record rewritten, which no released jj supports (section 2, Q1). |
| D5 | Conflicts are detected by a read after the command, never by exit status. | `jj rebase` exits 0 when it produces conflicts (probe). |
| D6 | Publish pre-checks the commits it is about to push for conflicts and missing descriptions, and returns any other refusal as a typed push-refused error carrying jj's message. | `jj git push` exits 1 with only stderr text (probe). The pre-check cannot mirror every jj push policy (private commits, future ones), so the plan does not promise to. |
| D7 | On a jj slot, "dirty" means the working-copy commit `@` is non-empty, and **every status read snapshots the working copy**: slot reads never pass `--ignore-working-copy`. Mutating verbs and the destroy guard snapshot first too (jj does this itself before any command that does not skip the working copy). Slot state is read on demand only (owner, 2026-10-10, Q6), so every read is an explicit request and nothing snapshots on a timer. | A read with `--ignore-working-copy` does not see edits made since the last snapshot (probe), so a never-snapshotting status would report `clean` over unsaved work, and destroy could delete it. A snapshot costs about what the `git status` the git path runs on every read costs. Measured: a snapshot that finds no change writes nothing to the operation log; one that finds a change writes one `snapshot working copy` operation (40 to 200 ms on a small repo, depending on machine load). |
| D8 | Slot mutations run with the slot as working directory, and every primary gets `snapshot.auto-update-stale = true`. *As built in phase 5:* the jj runner also passes `--config snapshot.auto-update-stale=true` on every command, since jj keeps repository config per user (Q9). | Rewriting a slot's commits from elsewhere leaves it stale and its next plain `jj` command fails; with the setting it repairs itself (probe). |
| D9 | The operator takes one cross-process file lock for a primary, git or jj, at `<parent>/.<name>.angee.lock` beside the primary, around its registrations, clone, fetch, pull and push: adding and pruning git worktrees (jj: workspace add and forget), cloning or fetching the primary, and pulling or pushing it. The verbs on a slot take none. (Narrowed 2026-10-11, review of #125, from "every mutation of a primary".) | `beginMutation` is an in-process, per-`Platform` try-lease (`platform.go:105`). It does not serialize two stacks, or the CLI and the operator, on one shared primary. What interleaves badly is a registration being added while another process prunes, and two clones or fetches of one primary; a slot verb is safe without the lock, because git's ref transactions keep each ref update whole and no verb prunes a registration, and jj's operation log merges concurrent commands, so jj needs the lock only around workspace add and forget. The lock sits outside the primary because `.jj` does not exist before initialization. Order: stack mutation lease, then the root lock, then the primary lock. |
| D10 | The jj driver uses `jj rebase --onto`. | `-d/--destination` is no longer in `jj rebase --help`; it still parses as a legacy alias. |
| D11 | `sync-base` treats an unset method as "the driver's default": merge for git slots, rebase for jj slots. An explicit `--merge` on a workspace that has a jj slot is refused before any slot is touched. | The CLI hardcodes `method := "merge"` (`cli/root.go:1231`), so the service cannot tell a default from an explicit choice. Silently rebasing when merge was asked for is worse than refusing. |
| D12 | A rebase on a jj slot is refused when the commits it would rewrite, `(B..N@)::`, include another workspace's working copy or the checked-out branch of a git worktree of the same primary. A bookmark on them that nothing has checked out is allowed to move (owner, 2026-10-29). | `jj rebase -b` rewrites descendants too, so one Angee workspace's sync could rewrite another's commits. For a git worktree, jj moves the branch, and jj 0.45 detaches that worktree when it exports the moved branch (probed), leaving it off its branch. A bookmark nothing has checked out moves as jj moves it, and a pushed one force-updates, with a lease, on its next publish. |
| D13 | Workspace names and refs are always quoted when placed in a revset (`"<name>"@`), and a name collision is reported by the operator before `jj workspace add` runs. | Names come from stack and workspace names, which are barely validated (`manifest.go:504`, `workspaces.go:2080`). |

## 2. What the probes established

Run against a bare remote, a colocated clone as primary, and secondary
workspaces. Scripts are not checked in; phase 5 turns them into tests.

| Behaviour observed | Consequence |
|---|---|
| `jj git init --colocate` on an existing clone leaves git on `main` and writes its own `.jj/.gitignore`. | No `.git/info/exclude` edit is needed. Git worktree slots of the same primary keep working (mixed stacks). |
| `jj workspace add --colocate --name N -r main@origin <path>` creates a slot with a `.git` file, `git rev-parse` works, HEAD is detached, no branch exists. It also works into an existing empty directory. | Matches the proposal. |
| `jj workspace add` with a revision that resolves to nothing creates the workspace and then exits 1. | A failed `Add` must always roll back (forget, remove). |
| The slot's `.git` reads `gitdir: ../primary/.git/worktrees/<slot dir name>` and `.jj/repo` reads `../../primary/.jj/repo`. | Moving the slot or the primary breaks both. |
| After a move to a **different depth**, the slot works again only once the absolute primary path is written into `.jj/repo` and `git worktree repair <slot>` is run. | Staging in the system temp directory, as the git path does, needs an edit to a jj internal file. |
| After a move at the **same depth** (a sibling directory, or `workspaces/.angee-stage-<token>/<subpath>` renamed to `workspaces/<ws>/<subpath>`), `jj status` and `git status` work straight away, with the primary inside or outside the stack root and with nested subpaths. A deliberate depth mismatch fails with exit 1. | The relative pointers survive a same-depth rename, so no jj file is edited. A `jj status` after the rename is the check. |
| Until `git worktree repair <slot>` runs, git lists the moved slot as `prunable` and `git worktree prune` would delete its admin directory. | The repair is mandatory and must run under the primary lock, before anything else can prune. |
| After any move, jj's record of the workspace path stays stale. It lives in a binary index (`.jj/repo/workspace_store/index`). `jj workspace rename`, `update-stale` and commits do not refresh it. Consequences: the `root` template keyword is empty, `jj workspace root --name N` and `jj workspace remove N` fail with an error, and `jj workspace forget N` no longer removes the git worktree registration. Inside the slot, `jj workspace root` is correct. | This is why slots are created in place (D4). The operator must still never depend on jj's `root` for a workspace it did not create. |
| `jj workspace list -T 'json(self)'` yields `{name, target{commit_id, change_id, parents, description, …}}`. | Reconciliation is by name. |
| `jj rebase` onto a moved base with a conflicting change: exit 0, the commit is marked conflicted, the slot stays usable. `conflicts() & (B.."N"@)` lists the commits; the `conflicted_files` template keyword lists paths without entering the slot. | D5. |
| `git status` inside a conflicted slot shows `.jjconflict-*` entries. | Never derive jj slot state from git. |
| `jj git push --bookmark X` for a brand-new bookmark works without extra flags. A conflicted commit: exit 1, "Won't push commit … since it has conflicts". An undescribed commit: exit 1, "… since it has no description". | D6. |
| After a rewrite from the primary, `jj status` in the slot exits 1 "working copy is stale" unless `snapshot.auto-update-stale` is set, in which case the slot updates itself. | D8. |
| Rewriting an immutable commit (trunk, an untracked remote bookmark) exits 1 with a hint. | A sync-base or rebase can fail for a reason that is not a conflict; return jj's message as the error. |
| A file created in the slot is invisible to `--ignore-working-copy` reads (`empty=true`) until a snapshotting command runs. | D7. |
| `JJ_USER`, `JJ_EMAIL`, `JJ_EDITOR` are honoured. `--config ui.editor=true` is rejected (the value parses as a boolean). | The runner sets a no-op editor through the environment. It no longer sets an identity (commit-identity fix, 2026-10-28): jj commits as the user, from their jj config. |
| `jj new "N"@- <ref> -m "Merge <ref>"` leaves the merge as `@`; a following `jj new` leaves it as `@-` under a fresh empty `@`. | Merge is two commands, so publish (which pushes `@-`) includes the merge. |
| `working_copies()` and the `working_copies` keyword exist; `"stack--a"@` parses. | D12 and D13 are implementable as written. |
| `git worktree remove` silently deletes ignored files and refuses when untracked, non-ignored files exist. | The migrate verb must carry untracked and ignored content across (phase 9). |
| The path record (`message Workspace { string name = 1; bytes path = 2; }`, path relative to `.jj/repo`) is written only when a workspace is created (`Workspace::init_*` in `lib/src/workspace.rs`); loading a workspace never refreshes it, and no command rewrites it. Rewriting the entry under jj's lock protocol (flock on `index.lock`, temp file renamed over `index`) was probed and fixes all four consequences. Upstream PR jj-vcs/jj#10219 adds `jj workspace move`, which needs the workspace still at its recorded path. | Not used (owner, 2026-10-05: no interim). `jj workspace move` is an optional later hardening, not a dependency (Q1). |
| A snapshot that finds no change writes no operation; one that finds a change writes one `snapshot working copy` operation. 40 to 200 ms on a small repo. | A status read of an idle slot writes nothing (D7). |
| `jj workspace forget N` removes the git worktree registration when the directory exists; when the directory was deleted first, the registration stays until `git worktree prune`. Forgetting an unknown name exits 0. | Removal order: forget, remove directory, `git worktree prune`. Forget is idempotent. |

## 3. Inventory at `60fa9c2`

| Item | Where |
|---|---|
| `git.Client`, a concrete struct; `Platform.gitClient()` builds one per call | `internal/git/git.go`, `service/platform.go:135` |
| Slot materialization: worktree (staged, installed through `GuardedPath.ReplaceFrom`, registration repaired), clone, local symlink | `service/workspaces.go:1570-1738`; symlink at `:1726`; repair at `:1740` |
| Slot mode is copied from the template verbatim, not substituted | `service/workspaces.go:1553` (`Mode: spec.Mode`), `copierx/copierx.go:292` |
| Slot mode is never validated | no check in `internal/manifest`, `internal/service` or `internal/copierx` |
| Branch-mismatch guard and state | `service/workspaces.go:572-605`, used by status `:392`, push `:1144`, sync-base `:1214`, destroy `:511`, source pull/push `gitops.go` |
| Destroy guard: refuses dirty or ahead-of-base slots | `service/workspaces.go:607-675` |
| Slot removal: remove directories, then `git worktree prune` per cache | `service/workspaces.go:1469` |
| `ws push`: skips modes other than `worktree` and `clone`; per slot dirty check, then push with or without upstream | `service/workspaces.go:1144-1212` |
| `ws sync-base`: worktree slots only; fetch in the slot, resolve `origin/<ref>`, merge or rebase; stops at the first failure | `service/workspaces.go:1214-1284` |
| Merge/rebase/abort/continue/publish verbs; publish defaults the remote to `origin` | `service/gitops_merge.go:23-101`, `:73` |
| Slot diff | `service/diff.go:35` |
| `gitOpEnv()` with the `gh` credential variables (#89) | `service/gitops_merge.go:216` |
| Source cache clone/fetch from `source.Repo`, best-effort on bring-up | `service/sources.go:373` |
| Topology build; counts `worktree` links | `service/gitops.go:33-104` |
| Topology and status subscriptions poll every 2 s and hash (off by default once `owner-pushed-live-views.md` step 5 lands) | `operator/gql/events.go:25`, `:126` |
| DTOs: `WorkspaceSourceStatus`, `GitOpsSummary`, `GitOpResult`, `SourceState` | `api/types.go:148`, `:212`, `:439`, `:548` |
| Service interface the adapters dispatch through | `service/api.go:128` (`WorkspaceSourceAPI`) |
| Adapters for slot operations | REST `operator/operator.go:241-250`; GraphQL `operator/schema.graphql:850`, `:888-902`; CLI `cli/parity.go:296-431`; remote client `platformclient/client.go:510-653` |
| `doctor` tool checks: `git --version` only, no floor, no jj | `cli/doctor.go:125` |
| `manifest.Source` has one `Repo`; `WorkspaceSource` has `Mode`, `Branch`, `Ref`, `Subpath` | `manifest/manifest.go:155`, `:197` |
| Cross-process lock helper | `internal/fslock` (`New(path)`, `With(ctx, fn)`) |
| The operator container mounts the stack root at the identical path | `service/compile_stack_root_test.go` |
| CI: `go test -race ./...` on ubuntu and macOS, no jj; cross-builds a Windows binary | `.github/workflows/ci.yml` |
| Generated artefacts and their gates | `make generate` + `make check-generated` (gqlgen, SDL); `make schema` + `make check-schema` (manifest JSON schema) |

## 4. Phases

Each numbered item is one pull request unless it says otherwise. Every Go PR
ends with `make check`; `make check-generated` when the GraphQL schema
changed; `make check-schema` when the manifest changed; a `CHANGELOG.md`
entry; and a pass of the `go-code-reviewer` agent.

```text
0  landed
1  mode validation + branchless worktree slots      (git only, can start now)
2  remotes with roles                               (git only; merged)
4a..4d  vcs.Driver seam, git driver only            (after 1; merged, 4c in #125)
5  jj driver package + doctor probe + CI            (after 4; merged in #126 and #131)
6  mode: jj lifecycle + status, other verbs refuse  (after 5; merged in #140)
7  status fields and topology                       (after 6; merged in #142)
8  sync-base, publish, push, merge on jj slots      (after 7; needs 2; sync-base and rebase merged in #143, the rest in review)
9  ws source migrate                                (after 8)
10 op-heads watch: dropped, slot state is read on demand (Q6)
11 templates, docs, live check                      (after 8, 9)
3  link mode: dropped (Q3)  12 automatic sync: not in this plan (Q4)
```

### Phase 0. Landed

- Credential passthrough (#89) and the proposal (#90) are merged.
- `.jj` watcher ignore in `@angee/app`: ang-ee/angee-django PR #151, merged
  2026-10-02. **It must reach the stacks' framework version before phase
  11's live check**, or Vite and Vitest watch jj's store.

### Phase 1. Slot mode validation and branchless worktree slots (git only)

**State: done.** Merged as #93 (`bbc5339`) and released in v0.16.0 on
2026-10-05, together with the #91 restart fix (#92). A code review added
behaviour beyond the steps below, all
tested: a branchless worktree needs a ref or default_ref (else create
fails); a base that exists only as `origin/<ref>` is started from
explicitly, with `--no-track` for a new branch; "own commits" for push,
publish and the destroy guard are those neither `<ref>` nor `origin/<ref>`
holds; the destroy guard refuses commits on a detached HEAD that no branch,
remote branch or tag holds; the detached push refusal is a `ConflictError`
(409); publish with an explicit branch skips the "nothing to publish" check.
Deliberately not done: making `ws push` also push git slots with no mode
(clones such as a git-backed `.work`).

**Goal:** make the manifest strict about modes, and let a worktree slot exist
without a branch. Both are worthwhile without jj.

Files: `internal/manifest/manifest.go`, `internal/service/workspaces.go`,
`internal/service/gitops.go`, `internal/git/git.go`, tests beside each,
`docs/guide/manifest.md`, the regenerated manifest schema.

Steps:

1. Add slot mode constants in `manifest` (`worktree`, `clone`, empty). In
   `ValidateExtended` (`manifest.go:529`) reject any other value for a
   declared workspace source. Reject the same in `resolveWorkspaceSource`
   (`workspaces.go:1553`) so a template with a bad mode fails at create,
   before anything is materialized. *Changed while implementing
   (2026-10-05):* `worktree` or `clone` on a `kind: local` source is **not**
   rejected; it keeps linking as today. A template sets a slot's mode without
   knowing whether the stack declares that source as git or local, so
   rejecting the mismatch would break templates that work for both kinds. A
   survey of every manifest and workspace template on the owner's machine
   found only `worktree` (always with a branch) and omitted modes.
2. Resolve `spec.Mode` through `substitute.Resolve` like `Branch` and `Ref`,
   so a template can write `mode: "${inputs.slot_mode}"`. Phase 11 needs this
   to let a stack opt in without forking the template.
3. Branchless worktree: add `git.Client.WorktreeAddDetached(repoDir, dest,
   ref)` running `git worktree add --detach`. In `materializeWorkspaceSource`
   use it when `ws.Branch == ""`. Today that case runs `git worktree add
   <dest> <ref>` with no `--detach`, which fails when the ref is checked out
   in the primary or quietly creates a tracking branch.
4. Skip slots with nothing to publish. In `WorkspacePush` (`:1144`),
   `WorkspaceSourcePush` (`gitops.go`) and `WorkspaceSourcePublish`
   (`gitops_merge.go:61`): when the slot has no commits beyond its base and
   is clean, return its state without pushing. A branchless slot with commits
   is refused with a message naming the missing branch, instead of today's
   bare `git push` on a detached HEAD.
5. `workspaceSourceRequiresBranch` (`:596`) already returns false for an
   empty branch, so status and the destroy guard need no change; add tests
   that pin that.

Tests: manifest validation table; create with `branch: ""` yields a detached
worktree at the base; `ws push` on a workspace where one slot is untouched
pushes only the other and creates no remote branch for the untouched one;
a template with `mode: "${inputs.x}"` resolves.

### Phase 2. Remotes with roles (git only)

**Goal:** a source can name several remotes, one push default, and a
fetch-only canonical remote, so publish stops assuming `origin`.

Files: `internal/manifest/manifest.go`, `internal/service/sources.go`,
`internal/service/workspaces.go` (clone-mode slots,
`resolveWorkspaceTemplateSource`), `internal/service/gitops_merge.go`,
`internal/copierx/copierx.go` (`TemplateSource`), docs, schema.

Steps:

1. `manifest.Source` gains `Remotes map[string]SourceRemote` and
   `PushDefault string`. `SourceRemote` is `{URL string; Canonical bool;
   Branches []string}` with a custom `UnmarshalYAML` accepting a bare string
   as the URL (same pattern as `StringList`, `manifest.go:354`). The proposal
   writes `map[name]url`, which cannot hold the canonical flag or the
   `branches` allow-list the same paragraph requires.
2. `repo:` stays and means `remotes: {origin: {url: repo}}`; setting both is
   a validation error. `push_default` must name a declared, non-canonical
   remote. **The clone URL must be determinable**: the canonical remote,
   else `origin`, else the only remote; anything else is a validation error.
   A `Source.CloneURL()` helper replaces every direct read of `source.Repo`
   (`sources.go:401`, the clone-mode slot at `workspaces.go:1719`).
3. Carry `remotes` and `push_default` through `TemplateSource` and
   `resolveWorkspaceTemplateSource` (`workspaces.go:1516`).
4. In `materializeSource` (`sources.go:373`), after clone or fetch, reconcile
   the primary's git config under the primary lock (D9): `git remote
   add`/`set-url` per declared remote, `remote.pushDefault`, and for a
   canonical remote a push URL that cannot be pushed to. Only touch remotes
   the manifest declares.
5. `WorkspaceSourcePublish`: the default remote becomes `push_default`, then
   `git.Client.PushRemote` (the existing triangular resolution), then
   `origin`. Refuse a branch that matches no pattern in the target remote's
   `branches` (`path.Match`), with an `InvalidInputError`. This step lands
   with or after 4d, which moves the publish rules into the driver, and
   before the next release after v0.21.3. Built on 4d; see "As built:
   step 5" below.

Tests: YAML round-trip of both forms; clone-URL resolution table; reconcile
is idempotent; publish to a canonical remote is refused; allow-list refusal.

**As built: steps 1-4 (2026-10-11).** Step 5 is not in this PR; it follows
4b, which moves publish behind the driver.

- **Manifest.** A remote is a bare URL or a mapping with `url`, `canonical`
  and `branches`.
  - The mapping form checks its own keys, because a custom `UnmarshalYAML`
    decodes with a fresh decoder that does not inherit `LoadFile`'s
    `KnownFields`.
  - Saving writes a URL-only remote back as a bare string, so the operator's
    rewrites of `angee.yaml` keep the form it was written in. JSON accepts
    both forms. `cmd/schema` describes both (a `Mapper`, so `manifest` does
    not import jsonschema).
  - The mapping form accepts `<<` merge keys, as the rest of the manifest
    does, and checks the merged mappings' keys too.
  - `ValidateSource` also requires remote names to match
    `^[A-Za-z0-9][A-Za-z0-9._-]*$` (no `/`, no `..`, no `.lock` suffix) and
    to differ by more than case (remote-tracking refs are files), rejects
    remotes on a `kind: local` source, `branches` on the canonical remote and
    invalid `branches` patterns (`path.Match`), refuses relative URLs under
    `remotes:` (`manifest.IsRelativeURL`, which the `repo:` reconcile also
    uses), and refuses control characters and surrounding spaces in every
    URL, `repo:` included.
  - A git source with no remote at all stays valid: an adopted cache needs
    none.
- **Helpers.** `Source.DeclaredRemotes()` and `Source.CloneRemote()`, with
  `CloneURL()` on top. `CloneRemote` returns the remote's name as well as its
  URL, because the clone is made under that name. `git.Client.CloneRef`
  passes `--origin <name>` for every name, origin included: without it git
  takes the user's `clone.defaultRemoteName`, which could give a canonical
  origin another name the reconcile never blocks. The template cache clones
  as origin explicitly, since it reads `origin/HEAD`.
- **Base resolution.** `git.Client.SyncBaseRef(dir, ref, baseRemote)` tries,
  in order: git's own `<ref>@{upstream}` (a remote branch only), then
  `<baseRemote>/<ref>` (origin when empty), then the other remotes in name
  order, then the local ref. It used to try origin first and then go-git's
  map order, so with origin a fork the base was the fork's branch, and
  without origin it varied between calls. `vcs.Slot` gains `BaseRemote`
  (the source's clone remote, set by `workspaceVCSSlot` and the cache read),
  and `gitdriver.CommitsBeyondBase` takes it, so sync-base, the worktree
  start point and status agree. **For 4b:** `ResolveBase` must pass
  `s.BaseRemote` to `SyncBaseRef`.
- **Canonical push block.** The canonical remote's push URL is set to
  `angee-fetch-only://<name>`. No git remote helper serves that scheme, so a
  push fails with "remote helper 'angee-fetch-only' aborted session", and
  `git remote -v` shows why. Every push form was tried (`--all`, `--mirror`,
  `--tags`, `--repo`, `-u`, a plain `git push`): none reaches it. A remote
  that `remotes:` declares without `canonical` loses that push URL. A push
  URL the user set on a non-canonical remote is left alone.
- **Reconcile.** `syncSourceRemotes` replaces `syncSourceOrigin`, keeping
  its rules, and runs in three modes:
  - *report* (bring-up, workspace provisioning, clone-slot reads) only warns;
  - *adopt* (`source fetch` / `source pull`) writes and warns per change;
  - *fresh* (just cloned) writes silently.

  The `repo:` form behaves as before: origin's URL only, with the same
  warnings. It never reads or writes push URLs or tracking, so a `repo:`
  stack sharing a cache cannot lift another stack's canonical block. A
  missing canonical remote is reported once, as missing.
- **Tracking.** Under `remotes:`, adopt mode sets `default_ref`'s upstream to
  `<clone remote>/<default_ref>` after the fetch (`git branch
  --set-upstream-to refs/remotes/…`, in full so a local branch of the same
  short name is not taken for it), as a fresh clone has it; report mode
  warns. Without it a cache cloned under `repo:` keeps tracking origin, which
  `remotes:` may now name a fork, and `source pull` and the base follow the
  fork.
- **Staged clone.** A missing cache is cloned into `clone/` inside a sibling
  temporary directory (`.<name>.clone-*`), reconciled there, and renamed into
  place. git creates `clone/` with the umask's permissions, as a plain clone
  has them; the temporary directory itself is 0700. Until the reconcile the
  canonical remote is pushable, so nothing may see the clone before it.
  - A failure removes only the staging directory. A new attempt first
    removes staging directories an interrupted one left.
  - A symlinked `cache_path` is resolved first, so the clone lands in its
    target, as `git clone` did.
  - An existing empty directory, which a rename may not replace (a mount
    point), is cloned in place and configured right after, as before this
    PR, with that short window. A failed configuration empties it again; a
    failed clone leaves the directory as git left it, since what is in it may
    be another writer's.
  - A path that holds anything else is refused before cloning.
- **Clone slots.** A clone slot (`mode: clone`, or a git slot with no mode,
  which `materializeWorkspaceSource` also clones) is cloned and reconciled
  the same way (`workspaces.go`, the clone branch of
  `materializeWorkspaceSource`), because it is a repository of its own and
  would otherwise push to the canonical remote. Because nothing reconciles it
  afterwards, workspace status (and so repair) runs the reconcile on it in
  report mode whenever it reads the slot, on demand, if the slot has its own
  `.git`. Its warnings point at git in the slot, since `source fetch` acts on
  the cache, and repeat on every read until the slot is fixed. 4c moves this
  code with the rest of the lifecycle. `workspace push` still selects only
  `worktree` and `clone` slots, so it skips a git slot with no mode; see Q7.
- **Status of a cache with a local branch named like a remote branch** (a
  local `upstream/main`, say) fails: the driver counts against the short
  upstream name, which git finds ambiguous. This predates the PR; the
  tracking write above no longer trips over it.
- **Template sources.** An inline source's remotes and `push_default` are
  substituted like its other strings, and it becomes `kind: git` when it
  declares remotes. It is validated before anything is cloned.
- **Order with step 5 (owner, 2026-10-11).** Publish still defaults to
  `origin`; there is no interim fallback. v0.21.3 is tagged before this PR
  merges, and step 5 lands before the release after that.
- **Gaps recorded here.**
  - No primary lock: D9 arrives in 4c. The reconcile runs under the stack's
    mutation lease, as `materializeSource` did, so two stacks sharing one
    cache are not serialized. (4c adds the lock; see "As built in 4c".)
  - A remote the manifest stops declaring stays in the cache: removing
    remotes is out of scope.
  - Step 5 must also make publish refuse a remote that the slot does not
    have configured.

**As built: step 5 (2026-10-11),** stacked on 4d.

- **Default remote.** The service passes the caller's remote as is; the
  driver resolves an empty one with git's own push-remote order
  (`git.Client.PushRemote`: the branch's `pushRemote`, `remote.pushDefault`,
  the branch's remote, `origin`, the only remote). `push_default` reaches it
  as the `remote.pushDefault` the cache and clone slots carry, so publish and
  plain `git push` agree, and the branch's own `pushRemote` comes first. When
  git names none (`git.NoPushRemoteError`), `PlanPublish` returns
  `*vcs.NoPushRemoteError` with git's reason and the service asks for a
  remote; there is no `origin` guess. A first version passed `push_default`
  from the service and fell back to `origin` on any lookup error; the review
  of #122 showed that overrode a branch's `pushRemote` against the docs and
  hid git's reason.
- **A remote the slot doesn't have** is refused: `PlanPublish` reads the
  remote's URL and returns `*vcs.UnknownRemoteError` without one, and the
  service reports it as invalid input on `remote`, saying so when the slot's
  own push configuration named it (`Resolved`). The URL travels in the plan,
  so the push step's message no longer reads it a second time.
- **The manifest's rules for the target remote** are checked between
  `PlanPublish` and `PreparePublish`, so a refused publish never creates a
  detached slot's branch: the canonical remote is refused, and so is a
  branch that matches none of a declared remote's `branches` patterns
  (`path.Match`, where `*` does not cross `/`). git's push block stays the
  enforcement for pushes made outside angee.
- **Tests.** Real repositories, worktree and clone slots:
  - the default follows `remote.pushDefault` (from `push_default`), and a
    branch's `pushRemote` comes first;
  - a `repo:` source publishes to `origin`; a clone slot to its own push
    default;
  - a remote the slot lacks, by name or in a URL's form, is refused with
    nothing changed, and one its push configuration names is reported as
    the slot's;
  - several remotes and no push default ask for a remote with git's reason;
  - a named remote is judged by its own patterns and wins over the default;
  - the canonical remote is refused with no branch created, while git's
    push block still refuses a direct push;
  - a refused branch on a detached slot is never created, and an allowed
    one is created and pushed.

  Six mutants are caught: `push_default` injected ahead of git, git's
  reason swallowed for an `origin` guess, a resolved remote reported as
  input, no canonical refusal, the wrong remote's patterns, and the branch
  created before the rules.

### Phase 3. `link` slot mode

Dropped (Q3, answered 2026-10-11): the proposal's own field evidence argues
against it, and every `.work` becomes its own jj workspace in phase 6.

### Phase 4. `vcs.Driver` seam with the git driver only

**Goal:** route every slot operation through one interface with no behaviour
change. Three PRs, each behaviour-preserving on its own.

New package `internal/vcs`:

```go
// Slot identifies one workspace slot and the facts a driver needs about it.
type Slot struct {
    Mode     string // effective mode: worktree, clone, jj
    Primary  string // source cache or local store
    CloneURL string // clone mode only
    Path     string // the slot's working copy
    Name     string // jj workspace name; unused by git
    Branch   string // manifest branch or bookmark; may be empty
    BaseRef  string // slot ref, else the source default ref
    Remote   string // push default; empty lets the driver resolve
}

// Dest is the stack-rooted capability a driver must use for every write to
// the slot path. copierx.GuardedPath satisfies it. The service opens and
// closes it; a driver never resolves the slot path on its own.
type Dest interface {
    Lstat() (fs.FileInfo, bool, error)
    IsEmptyDirectory() (bool, error)
    ReplaceFrom(ctx context.Context, stage string) error
    VerifyPathIdentity(path string) error
    RemoveAll() error
}

type Status struct {
    CurrentRef, ChangeID, Upstream string
    Dirty                          bool
    Ahead, Behind                  int // against Upstream when set, else the base
    BaseBehind                     int // commits the base has that the slot lacks
    MismatchReason                 string // git only
    Conflicted                     bool
    ConflictPaths                  []string
    Pushed                         bool
    UnpushedReason                 string
}

type Driver interface {
    Kind() string // "git" or "jj"
    // lifecycle
    Exists(ctx context.Context, s Slot) (bool, error) // registered with the primary
    Add(ctx context.Context, s Slot, d Dest) error
    Remove(ctx context.Context, s Slot, d Dest) error
    // reads
    Status(ctx context.Context, s Slot) (Status, error) // jj: snapshots first (D7)
    Diff(ctx context.Context, s Slot, ref string) ([]api.DiffFile, error)
    // verbs
    Fetch(ctx context.Context, s Slot) error
    Pull(ctx context.Context, s Slot) error
    SyncBase(ctx context.Context, s Slot, method string) (api.GitOpResult, error)
    Push(ctx context.Context, s Slot, ref string) error
    Publish(ctx context.Context, s Slot, remote, branch string) (api.GitOpResult, error)
    Merge(ctx context.Context, s Slot, ref string) (api.GitOpResult, error)
    Rebase(ctx context.Context, s Slot, ref string) (api.GitOpResult, error)
    MergeAbort(ctx context.Context, s Slot) (api.GitOpResult, error)
    RebaseAbort(ctx context.Context, s Slot) (api.GitOpResult, error)
    RebaseContinue(ctx context.Context, s Slot) (api.GitOpResult, error)
}
```

`Status.Pushed` and `UnpushedReason` replace the separate destroy-guard
computation (`workspaces.go:643`); the guard reads them from a snapshotting
status. The `--sync` decision (reclaim, keep, or refuse a leftover) stays in
the service and uses `Exists`.

**As built in 4a (2026-10-09).** The sketch above is the target; each PR adds
only what it uses:

- `vcs.Slot` has `Path`, `Branch` and `BaseRef`. `Branch` is the branch the
  slot *must* be on (the service sets it only for a worktree slot that names
  one, in `workspaceVCSSlot`), so the manifest rule stays in the service and
  the driver only compares. `Mode` arrives with phase 6, where `slotDriver`
  first has a choice to make; `Primary`, `CloneURL`, `Name` and `Remote`
  arrive with the verbs and lifecycle that need them.
- `vcs.Status` has the git fields (`CurrentRef`, `Upstream`, `Dirty`,
  `Ahead`, `Behind`, `MismatchReason`, `Pushed`, `UnpushedReason`); the jj
  fields come with phase 7. On error it holds what was read before the
  failure, as the service's status always showed. A `*vcs.CountError` means
  the pushed verdict was decided but counting against the upstream or base
  failed (a deleted base branch): status shows an error, and destroy still
  lets a slot with nothing of its own go, as the guard did before.
- `Driver` has `Status` and `Diff`. `Kind` arrives with the jj driver
  (phase 6). `Status` takes no snapshot flag: every slot read is an explicit
  request, so the jj driver always snapshots (D7, Q6).
- The git driver reads any git checkout, so source caches use it for status
  and diff too: the cache and its slots share one status rule. `git.Client`
  stays the owner of running git (it gains `Diff`); the driver only
  interprets.
- **One "pushed" rule (owner, 2026-10-09).** Slot status and the destroy
  guard used to compute it separately, and disagreed after phase 1. The
  driver now computes it once, with the guard's rule: a detached HEAD
  counts the commits no branch, remote branch or tag holds, and a slot
  without an upstream counts the commits neither `<ref>` nor its remote
  counterpart (`<remote>/<ref>`, origin's first) holds. Visible change:
  after `sync-base`, status shows the slot `ahead` of the local base but
  `pushed`. State names (`ahead`, `behind`, `diverged`) still count against
  the upstream, else the base.
- That rule holds for a linked worktree, whose branches and tags stay in the
  cache. A repository of its own (a `clone` slot, a cache) takes its
  branches with it, so on a detached HEAD or without an upstream it counts
  every commit no remote branch or tag holds (tags count: git keeps fetched
  tags with local ones, and a clone pinned at a release tag is pushed). The
  driver asks git which one a checkout is (`git.Client.LinkedWorktree`); the
  guard on `main` let a detached clone slot go while only its own branch
  held its commits (Opus review, 2026-10-10).
- Known limit, unchanged from `main`: the verdict counts only what HEAD
  reaches. A clone's other local branches and its stash are not looked at
  (`rev-list --count --branches --glob=refs/stash --not --remotes --tags`
  would). A follow-up for the owner to decide.
- Ahead and behind count against the remote counterpart when the cache has
  no local `<ref>`; status used to report such a slot as an error.
- `gitdriver.BranchMismatch` and `gitdriver.CommitsBeyondBase` are the one
  implementation of each; the service's verb preconditions and push/publish
  call them until 4b moves the verbs behind the driver (done: they are
  driver methods since 4b).

The four PRs:

- **4a. Reads.** `internal/vcs` types, `internal/vcs/gitdriver` with
  `Status` and `Diff`, `Platform.slotDriver`/`Platform.slot`. The service's
  `workspaceSourceStatus` (`:392`), the destroy guard (`:607`) and
  `WorkspaceSourceDiff` (`diff.go:35`) call the driver.
- **4b. Verbs.** Fetch, pull, push, sync-base, publish, merge, rebase and
  the three state-machine verbs move behind the driver, with
  `runGitOpAt`/`gitOpEnv` (`gitops_merge.go:119`, `:216`).
- **4c. Lifecycle.** `Add` and `Remove` absorb the worktree
  stage-install-repair sequence (`workspaces.go:1610-1708`, `:1740`), the
  clone path, and removal with prune (`:1469`). `workspaceSourceCleanup`
  stores a `vcs.Slot` and a driver per entry. The primary lock (D9) is
  introduced here, around git worktree add/prune and around
  `materializeSource`. The worktree start point ("the local `<ref>`, else
  its remote counterpart", `workspaces.go` in the worktree add path) and
  `gitdriver.baseRefs` make the same choice; the move makes them one.
  Built as described below ("As built in 4c").
- **4d. Driver-owned push and publish rules.** Before phase 5. 4b leaves the
  push and publish rules in the service, asking the driver git-shaped
  questions; 4d hands each driver its rules (review of #117, 2026-10-11):
  - `PlanPush(s, ref)` returns a plan the service can hold for every slot
    before `Push(s, plan)` runs any, and `Publish(s, intent)` takes what the
    caller asked for (remote, branch, explicit or not). Each refuses with a
    typed `vcs` error (nothing to push, detached with work of its own, a
    branch that exists) that the service maps to its messages. Then
    `CurrentBranch`, `Upstream`, `CommitsBeyondBase`, `PushSetUpstream` and
    `CreateBranch` leave the interface.
  - The preconditions are answered per driver: whether a slot is on the
    branch it must be on, and whether it is clean enough for a verb, mean
    different things for a jj slot, so the driver answers "may this verb
    run" instead of the service reading a branch and dirty paths.
  - Failure wording is VCS-neutral: the service's messages and hints stop
    naming git commands (`git -C ... switch`, the " git: " quote), and the
    driver supplies the tool's own words.
  - The sync default reaches the driver: an empty method means the driver's
    natural sync (merge for git), not a fixed "merge" in the service.

  Phase 2 step 5 (the publish default remote and branch allow-list) lands
  with or after 4d, before the next release after v0.21.3.

  Built as described below ("As built in 4d"), except the preconditions
  and the wording, which change messages and move to phase 8.

**As built in 4b (2026-10-11).** Every service verb on a workspace slot asks
its driver (`Platform.workspaceSlotDriver`). The service keeps the mutation
lease, the manifest lookups, the kind check, input validation, the push and
publish rules and every error message; the driver carries the verb out and
answers the reads those rules need.

- Verbs moved: slot fetch, pull and push, `workspace push`, sync-base,
  publish, merge, rebase, and merge-abort, rebase-abort and
  rebase-continue. The list above was complete, but the sketch's one-call
  `SyncBase(s, method)` and `Push(s, ref)` cannot keep what the aggregates
  promise: sync-base fetches and resolves every slot's base before changing
  any, and `workspace push` plans every slot before pushing any. So the
  driver has `Fetch`, `ResolveBase` and `SyncBase(onto, method)`, and
  `Push` and `PushSetUpstream` carry out the service's push plan.
- `Driver` grows from 2 methods to 22 (+20), each one a verb uses. The reads:
  `BranchMismatch` (the branch precondition), `DirtyPaths` (the
  clean-checkout precondition, for caches too), `CurrentBranch`, `Upstream`
  and `CommitsBeyondBase` (the push and publish rules), and `FetchURL` and
  `PushURL` (the remote a failure names). `CreateBranch` returns
  `vcs.ErrBranchExists` for publish's refusal. The push and publish reads
  are git-shaped; 4d replaces them.
- Conflicts are results: `Merge`, `Rebase`, the state-machine verbs and
  `SyncBase` return `api.GitOpResult` with `Conflicted` and the files, and an
  error only for a failure. `SyncBase` still runs `git.Client`'s merge and
  rebase, so a sync records the user, not angee.
- `vcs.ErrBaseNotFound` is a base that exists nowhere (the service's
  "Check the base ref" refusal); any other `ResolveBase` failure is a failure
  of that step. The service no longer inspects git's error types there.
- Rebased onto phase 2 steps 1-4 (#116): `ResolveBase` and
  `CommitsBeyondBase` resolve the base from `Slot.BaseRemote` (the source's
  clone remote), as status does, so sync-base and the push and publish
  "nothing to push" checks count against the canonical remote, not a fork
  named origin. `FetchURL` names `Slot.BaseRemote` too, else origin, else
  the only remote.
- `vcs.ErrUnsupported` is a verb a driver has no meaning for (merge-abort on
  a jj slot, say). The service reports it once, as invalid input on the slot
  (`classifyOperationError`), for phases 6 and 8 to use.
- One git runner: `git.Client.RunIsolated` runs a command with only the
  variables git needs to reach ssh and credential helpers (#89) plus the
  caller's, never prompting, and returns stdout and stderr apart. Merge,
  rebase, their abort and continue, publish's push and the conflict listing
  run through it with angee's identity; their arguments and environment are
  byte-identical to `runGitOpAt`'s (a test records them through a fake git,
  and the same expectations pass against `cb27d2e`). `gitdriver` keeps only
  the reading of results. No verb runs git on a slot path from the service
  any more.
- Source caches keep `git.Client` for their verbs, but ask the git driver
  for the same reads (`DirtyPaths` through the one clean-check helper,
  `PushURL`), as cache status reads through the driver since 4a.
- The publish remote is still chosen in the service (`origin` when none is
  given); phase 2 step 5 replaces that line with `push_default`.
- No behaviour change. Characterization tests, written first and passing on
  `cb27d2e`, pin: merge records a merge commit with angee's identity even
  when it could fast-forward; a conflict reports raw non-ASCII paths, and
  abort restores; rebase abort, and continue without an editor (angee
  commits); fetch and pull; the remote each network verb's failure names,
  including publish to a named remote; sync-base by rebase and by merge as
  the user, its non-ASCII conflict message, and that it resolves every base
  before syncing any; `workspace push` checks every slot is clean before
  pushing any. A fake driver proves each verb's calls and the slot facts it
  is handed, with no git on PATH.

**As built in 4d (2026-10-11).** The driver owns the push and publish rules
(design note 2); the service keeps the manifest facts and every message.

- `PlanPush(s, ref, branch)` returns a `vcs.PushPlan` (`Ref`, `Track`, or
  `Skip`) or `vcs.ErrDetachedWithWork`; `Push(s, plan)` carries it out.
  `workspace push` still plans every slot before pushing any.
- Publish is three calls. `PlanPublish(s, intent)` changes nothing and
  returns a `vcs.PublishPlan` (remote, branch, whether to create it) or
  `vcs.ErrNothingToPublish`, `vcs.ErrNoBranch` or `vcs.ErrBranchExists`.
  `PreparePublish(s, plan)` creates the branch; `Publish(s, plan)` pushes.
  Splitting creation from the push keeps the order git sees (create, read
  the push URL, push) and keeps a failed creation reported as it is, not as
  a failed push. It also gives step 5 a point between deciding and changing
  anything, where the manifest's refusals go.
- `vcs.PublishIntent` carries what the caller asked (remote, branch,
  whether the caller named it) and `Unmanaged`: a slot whose source is not
  kind `git` skips both publish rules, as the service did with its
  `source.Kind == "git"` check. When local slots get a driver of their own
  (or are refused), the flag goes.
- `CurrentBranch`, `Upstream`, `CommitsBeyondBase`, `PushSetUpstream` and
  `CreateBranch` leave the interface: 22 methods become 20.
- The sync default reaches the driver (design note 5): an empty method is
  `vcs.SyncDefault`, which git resolves to a merge. The failure wording
  still says "merging" for it.
- An unsupported verb, reported as invalid input, still matches
  `vcs.ErrUnsupported` and the driver's error.
- Not done here, because each changes messages, and carried to phase 8:
  design note 3 (the preconditions answered per driver: `BranchMismatch` and
  `DirtyPaths` stay git-shaped reads the service judges) and note 4
  (VCS-neutral wording: the branch-mismatch hint still names `git -C …
  switch`, and failures still quote a " git: " line).
- No behaviour change. Characterization tests, written first and passing on
  `86fc88d`, put a recording git on PATH and pin every git command each
  push and publish case runs (directory, committer, arguments, order) and
  what it returns; they pass unchanged after the move.

**As built in 4c (2026-10-11).** The slot lifecycle is the driver's; the
service keeps the manifest facts, the destination checks, the `--sync`
decision, the cache refresh, the primary lock and every message.

- `Driver` gains `Exists`, `Add` and `Remove`: 20 methods become 23.
  `gitdriver/lifecycle.go` carries the worktree stage, install and
  registration repair, the clone, and removal with `git worktree prune`, as
  they were. `Add` leaves nothing behind when it fails; `Remove` prunes even
  when the removal failed, as rollback and destroy always did.
- `vcs.Slot` gains `Mode` (`worktree`, or `clone` for any other git slot,
  D3), `Primary` (a worktree's cache) and `CloneURL` (a clone's remote, named
  by `BaseRemote`). Only `workspaceLifecycleSlot` sets them, on top of
  `workspaceVCSSlot`; reads and verbs get the 4a facts until phase 6 has
  `slotDriver` choose by mode. `Name` waits for jj.
- `vcs.Dest` has what the git driver uses: `Lstat`, `ReplaceFrom`,
  `VerifyPathIdentity`, `RemoveAll`. `IsEmptyDirectory` stays with the
  service, which checks the destination before it calls `Add`; jj's in-place
  add (phase 6) brings it. `copierx.GuardedPath` satisfies it (a
  compile-time check in the service).
- **`Add` takes a `configure` step**, which the sketch lacked. A clone slot
  is configured from the manifest's remotes (`syncSourceRemotes`, fresh
  mode) in its staging directory, before it is installed, so it never sits
  at its path able to push to the canonical remote. The reconcile is the
  service's, so the service passes it in; worktrees pass nil.
- `--sync` (`addWorktreeSlot`): a leftover the cache lists (`Exists`) is
  removed through `Remove`, an unlisted populated directory is refused, and
  then `Add` runs, all under one hold of the primary lock.
- `workspaceSourceCleanup` entries hold the guard, the driver and the slot;
  rollback and destroy remove a git slot through `driver.Remove`.
- **A `kind: local` source's symlink stays in the service.** It is not
  version control, and making it needs nothing a driver knows; such entries
  have no driver. Phase 6 decides what a local source with `mode: jj` is.
- **One start point.** `gitdriver.startRef` is the first of `baseRefs`,
  through one iterator that resolves the remote counterpart only when the
  local base is missing, so the commands are unchanged.
- The registration repair still writes the cache's `gitdir` file through a
  `copierx` guard rooted at `.git/worktrees` (`gitdriver` now imports
  `copierx`). **Follow-up, the next PR:** run `git -C <cache> worktree
  repair <slot>` instead, so git owns its files, and add worktrees with `git
  -c worktree.useRelativePaths=false worktree add …`; that drops the
  `copierx` import and `strictlyInside`. It also fixes a bug older than 4c,
  found in the review of #125: with `worktree.useRelativePaths=true` (git
  2.48+), staging and then copying leaves the slot's `.git` relative to the
  temporary stage, and git calls the slot "not a git repository".
  **Settled in the follow-up PR.** The `-c` on the add alone fixes that bug,
  since the staged `.git` is then absolute, and it keeps the add from
  setting `extensions.relativeWorktrees` on the cache. `git worktree repair`
  was tried and rejected: whichever slot it is given, it checks every
  worktree registered in the cache and rewrites any broken one's `.git`
  through that worktree's path, outside the destination guard, and it fails
  a create when an unrelated slot of the cache is broken. Git has no command
  that re-points one registration, so the guarded `gitdir` write, the
  `copierx` import and `strictlyInside` stay.
- The "branch already exists; checking it out" warning is logged by the
  driver, which is where that choice is made.
- **The primary lock (D9, narrowed in the review of #125)** is
  `withPrimaryLock` in the service: `fslock` at `<parent>/.<name>.angee.lock`,
  beside the target when the cache path is a symlink, and beside the path
  only when the cache does not exist yet (any other failure to resolve it
  refuses the lock rather than locking another file). It is held around
  `materializeSource` for a git source (bring-up, `StackCompile`, `source
  fetch`); around `source pull`'s refresh and pull together; around `source
  push`'s clean check and push; around a worktree slot's refresh, `Exists`,
  `Remove` and `Add` together (the repair must come before anything else can
  prune); and around `Remove` in rollback and destroy. Clone slots, local
  sources and the slot verbs take none (D9 says why). Order: the mutation
  lease, then the root lock (bring-up only), then the primary lock; nothing
  under it takes another lock, and code under it calls
  `materializeSourceLocked`.
- **Waiting.** `fslock` gains `ReportWait` and `ErrTimeout`. The primary
  lock warns, at the default verbosity, after a second: "waiting for source
  cache <path>: another angee process is changing it", with the holder.
  `ANGEE_LOCK_TIMEOUT` running out is a `CONFLICT` to retry, and a lock file
  that cannot be created (a `cache_path` bind-mounted on its own) is a
  precondition that says to mount the parent directory. Every failure to
  take the lock matches `errPrimaryLockNotHeld`.
- **A rollback or destroy that cannot get the lock still removes the slot**
  through its guard, warns, and leaves the registration for the cache's next
  prune. Removing our own guarded path needs no lock; only the prune does.
  A first version waited for the lock before removing anything, so a
  rollback that timed out left the slot on disk, which `a242891` never did
  (review of #125).
- **Behaviour.** The git commands, their arguments, order and environment
  are unchanged, with two exceptions found while pinning them. With several
  worktree slots of one cache, destroy and rollback now prune once per slot,
  where they pruned once per cache: `Remove` is per slot, as jj's forget
  will be. And destroy no longer prunes the cache of a slot whose path it
  cannot open a guard on; it skips that slot, whose own registration a prune
  would have kept anyway.
- Tests. Characterization tests, written first and passing unchanged on
  `a242891`: a create that fails at a later slot rolls back the worktree
  (removed and pruned), the clone and the link, leaving no staging
  directory; `--sync` reclaims a listed leftover, refuses an unlisted one and
  a listed one without `--sync`, and reclaims a listing whose directory is
  gone; a slot whose parent is swapped for a symlink while git runs never
  reaches the symlink's target, for a worktree and a clone; and a recording
  git pins create (new branch, existing branch, detached, remote-only base),
  destroy and the rollback. New:
  - five cache changes (create, destroy, `source fetch`, `pull`, `push`)
    wait while a second `fslock` holder has the cache's lock, warn naming the
    cache, and go on once it is released;
  - a git wrapper tries the cache's lock without waiting before every
    command that changes the cache or lists its worktrees, and records any
    that ran unlocked, across create, a reclaim, destroy, a rollback and the
    source verbs. The waiting test alone missed a lock moved in to
    `driver.Add` only;
  - a rollback whose cache lock another holder keeps removes the slot and
    leaves a prunable registration that the next create drops;
  - a held lock past `ANGEE_LOCK_TIMEOUT` is a `CONFLICT`, an unwritable
    cache directory names the fix, and a cache path that will not resolve is
    not locked elsewhere;
  - a fake driver proves create, rollback and destroy dispatch through the
    driver with no git on PATH, and `startRef` is the first of `baseRefs`.

  Eighteen mutants are caught: the lock taken nowhere, or not around a
  worktree add (or only around `driver.Add`), a removal, a fetch, a pull or a
  push; rollback bypassing the driver, doing nothing, or leaving the slot
  when the lock is held; a lock failure not marked as one; create or destroy
  bypassing `slotDriver`; the leftover check skipped; the fallback to the
  unresolved path on any error; no `CONFLICT`, no permission hint, and the
  wait reported only at `-v`.

`git.Client` remains for what is not a slot: source cache clone/fetch,
template repositories (`templates.go`), `commits.go`. (Since 4a, a cache's
status and diff are read through the git driver, which reads any git
checkout.)

Tests: "the existing suite passes" is necessary, not sufficient. Before 4c,
add characterization tests for what the move could break and that the suite
may not pin today: create rollback after a mid-sequence failure; `--sync`
reclaim of a registered leftover and refusal of an unregistered one; a
destination whose parent is swapped for a symlink during create (the
guarded-path attack: prevented for git slots, which are staged; for jj slots,
which are created in place, the test asserts detection and rollback); two
processes adding worktrees to one primary at once.
Add a fake driver and one service test proving dispatch.

### Phase 5. `internal/vcs/jj`, the doctor probe, and CI

**Goal:** a jj driver that can read and mutate, exercised by tests, not yet
reachable from a manifest.

Steps:

1. Runner. `jj` from `PATH`. Every invocation gets `--no-pager --color
   never`; mutations pass `-m` wherever jj would open an editor.
   Environment is `gitOpEnv()` (moved to a shared helper) plus
   `JJ_USER=angee`, `JJ_EMAIL=angee@example.invalid` and `JJ_EDITOR=true`.
   Anything about a slot runs with the slot as the working directory, so
   jj snapshots that slot's working copy first (D7, D8): status, the
   destroy guard's read and every mutation. A command run with
   `-R <primary>` would snapshot the primary's working copy instead, so
   `-R <primary>` is used only with `--ignore-working-copy`, for store-level
   reads (`Exists`, the workspace list, bookmarks) and for further reads of
   a slot after its snapshot. Trace through `logctx.TraceExec` like git. Network
   commands (`jj git fetch`, `jj git push`) go through
   `git.RunNetworkOperation` so `ANGEE_GIT_TIMEOUT` applies.
2. Revset building. One helper quotes workspace names, bookmarks and refs
   (D13). No revset is built by string concatenation outside it.
3. `Probe(ctx) Capabilities`: `jj --version`; `jj workspace add --help`
   contains `--colocate`; `git --version` at least 2.42; the platform is not
   Windows. Cached per process. A typed `ErrUnsupported` carries the hint
   ("install a current jj build, e.g. `brew install --HEAD jj`"). **Windows:
   `mode: jj` is refused there** until someone verifies colocated secondary
   workspaces on it; the package must still compile for `windows/amd64`.
4. Templates, each a Go constant with a decode struct:
   - workspaces: `json(self)` (name and target only; never `root`)
   - commits: `json(self)` plus `empty`, `conflict`, `bookmarks`,
     `working_copies`, `conflicted_files.map(|f| f.path())`
   - bookmarks: `jj bookmark list --all-remotes -T json(self)`
5. `Status`, for slot `N`, base `B`, slot branch `X` and push remote `R`:
   - `Dirty`: `"N"@` is non-empty.
   - `Upstream`: `X@R` when the slot has a branch and that remote bookmark
     exists; otherwise empty.
   - `Ahead`/`Behind`: against `Upstream` when set, otherwise against `B`,
     measured from `"N"@-`. This mirrors the git path, which measures
     against the upstream when there is one and the base when there is not.
   - `BaseBehind`: count of `"N"@-..B`, so "the base moved" stays visible on
     a published slot.
   - `Pushed`: not dirty and `Ahead == 0`. `UnpushedReason` as the git path
     words it.
   - `Conflicted`/`ConflictPaths`: from `conflicts() & (B.."N"@)`.
   - `CurrentRef`: `X` when a local bookmark `X` points into `B.."N"@`;
     otherwise the short commit id of `"N"@-`. Never another bookmark that
     happens to be an ancestor.
   - `ChangeID`: of `"N"@`.
6. Mutations: `EnsurePrimary` (`jj git init --colocate` when `.jj` is absent,
   then `jj config set --repo snapshot.auto-update-stale true`), `Add`,
   `Remove` (forget, remove through `Dest`, `git worktree prune`). There is
   no separate snapshot step: jj snapshots a slot itself before any command
   run in it (step 1). `Add` rolls back on any error, including the
   exit-1-after-create case from the probes. All run under the primary lock
   introduced in 4c.
7. `doctor` (`cli/doctor.go:125`): add a `tool.jj` check that reports the
   version and the colocate capability, and a git floor of 2.42. It warns
   rather than fails when no slot in the stack is a jj slot.
8. CI: add a job that installs jj at the revision named in a new
   `.github/jj-rev` file (`cargo install --git … --rev`, cached by that
   revision), sets `ANGEE_REQUIRE_JJ=1`, and runs
   `go test -race ./internal/vcs/... ./internal/service/...`. With that
   variable set, a missing jj or a missing capability **fails** the
   integration tests instead of skipping them.

Tests:

- Decode tests over recorded NDJSON fixtures in `testdata/`. They run
  everywhere, with no jj installed, and fail when a template or struct drifts.
- Integration tests ported from the seven probes. Without `ANGEE_REQUIRE_JJ`
  they `t.Skip` when jj or the capability is missing, so the default matrix
  stays green.
- A fixture-refresh path behind a `-update` flag that regenerates the NDJSON
  from the installed jj, so a field rename shows up as a diff.
- Stale recovery (D8) across two slots that share history, entered through
  status, sync-base and publish.

**As built in phase 5 (steps 1-5, 7, 8) (2026-10-11).** Step 6
(`EnsurePrimary`, `Add`, `Remove`) is not in this PR: it waits for 4c's
lifecycle interface and primary lock.

- **Package.** `internal/vcs/jjdriver`, not `internal/vcs/jj`, to match
  `gitdriver`. `Driver` satisfies `vcs.Driver` (a compile-time assertion).
  `Status` and `Diff` are real. Every other method returns an error
  wrapping `vcs.ErrUnsupported`, except `FetchURL` and `PushURL`, which
  have no error and return "". `MergeAbort`, `RebaseAbort` and
  `RebaseContinue` already say a jj slot never has an operation in
  progress (phase 8 step 6); the rest say "not implemented yet".
  `Platform.slotDriver` never returns it.
- **Runner (step 1).**
  - jj comes from `PATH`. Every run starts with `--no-pager --color never
    --config snapshot.auto-update-stale=true`. D8 then holds wherever the
    operator runs, whatever the repository's per-user config says (Q9).
  - Environment: `git.IsolatedEnv`, which `git.Client.RunIsolated` now
    builds through too, plus `JJ_USER`, `JJ_EMAIL` and `JJ_EDITOR=true`.
    *Changed 2026-10-28 (commit-identity fix):* `JJ_USER` and `JJ_EMAIL`
    are gone and `JJ_CONFIG` is passed through, so jj commits as the user;
    the git ops likewise commit with the identity git resolves.
  - jj never runs in the process's working directory. `run` refuses an
    empty directory, and `snapshot` and `read` refuse an empty slot.
    - A slot command runs in the slot with `-R .`. jj then loads the slot's
      own workspace, so `@` is its working copy and no workspace name is
      needed. It also never falls back to a repository above the slot: a
      stack root inside a jj checkout would otherwise be snapshotted when a
      slot has lost its `.jj`.
    - The probe runs in the system temporary directory. jj records every
      repository it loads under `$XDG_CONFIG_HOME/jj/repos/` (a
      `metadata.binpb`, even for `jj --version`), so a probe run from inside
      a checkout wrote to the user's config. Review of #126 found this.
  - `snapshot` lets jj snapshot first. A call's first command is `jj op log
    -n 1`, which snapshots and returns the operation that recorded the
    snapshot. `read` adds `--ignore-working-copy --at-op <that operation>`
    to every read after it. The six to eight reads of a status then agree
    with each other while agents work in the repository. A base bookmark
    that vanishes mid-call cannot turn `(B)..(@-)` into all of history.
  - Nothing names a primary with `-R` yet.
  - `network` runs under `git.RunNetworkOperation`, for phase 8's fetch and
    push. Failures are a `*jjdriver.CommandError` with redacted arguments
    and stderr.
  - No command mutates yet, so none passes `-m`.
- **Slot and Status.** No `vcs.Slot` field was added: running in the slot
  stands in for `Name` and `Primary` for reads. `vcs.Status` gains
  `ChangeID`, `BaseBehind`, `Conflicted` and `ConflictPaths`. The git
  driver leaves them unset, and the DTOs and GraphQL wait for phase 7.
  `ConflictPaths` makes `Status` non-comparable, so the git driver's test
  compares with `reflect.DeepEqual`.
- **Revsets (step 2).** `revset.go` builds every revset.
  - `quote` writes jj's double-quoted literal: `\"`, `\\`, and ASCII
    control characters as escapes; every other byte as it is.
  - `exact` builds string patterns, for revset functions and for `jj
    bookmark list` arguments.
  - Bookmarks use the function forms (`bookmarks(exact:"X")`,
    `remote_bookmarks(exact:"X", remote=exact:"R")`). A missing bookmark is
    then an empty set, not an error, and a tag of the same name never wins.
    Workspaces use `"N"@`, and `symbol` names a ref.
  - jj also parses bookmark *name arguments* (`jj bookmark create/set X`)
    as revset symbols, so phase 8 must quote those too.
  - The `name` template keyword prints a name quoted that way, while
    `json()` prints it raw. The decode structs read `json()`.
- **Probe (step 3).**
  - `Probe(ctx)` returns `Capabilities` (`JJ`, `JJErr`, `Git`, `GitErr`,
    `OSErr`). It is cached for the process once a probe completes. A probe
    that its caller cancelled, or its own 10 s timeout cut short, is
    returned but not kept.
  - `Err()` is a `*jjdriver.UnsupportedError` that lists every shortfall
    with its hint. It wraps its causes and `vcs.ErrUnsupported`.
  - `--colocate` must start an option line of `jj workspace add --help`.
  - `GitSupported` (the 2.42 floor) is shared with doctor.
- **Templates (step 4)** are as listed, with three additions: commits carry
  `parents_short` (jj's short id of each parent, for `CurrentRef`) and
  `working_copies` as names, and `jj op log` prints the operation id. None
  uses `root`. The fixtures are regenerated with jj's own test settings
  (`JJ_RANDOMNESS_SEED` per command, `JJ_TIMESTAMP`, fixed git dates), so
  `-update` with the same jj changes no byte. CI regenerates them on Linux
  and fails on any difference.
- **Status (step 5)** is as worded above.
  - **D7 holds except for Q10.** Every status snapshots first, so an edit
    no jj command has recorded counts as dirty. The exception is a new file
    jj refuses to snapshot (over `snapshot.max-new-file-size`), or one
    `snapshot.auto-track` leaves out. A test pins that as a known limit.
    *Settled before phase 6 (2026-10-28):* Status counts the untracked paths
    `jj status` lists as dirty; see phase 6, "As built: the driver
    prerequisites of step 5".
  - B is `BaseRef@BaseRemote` (origin when `BaseRemote` is empty), else the
    local bookmark. jj's `git` pseudo-remote is never B's remote. With
    neither, Status returns `vcs.ErrBaseNotFound` and what it read before.
  - Reads, in order:
    - the snapshot (`jj op log`);
    - then, at its operation: `@`; the bookmarks; the commits of `B..@`
      (conflicts and `CurrentRef` come from that one listing); the counts.

    That is seven to nine jj runs, about 12 ms each on a small repository.
  - A two-parent `@` reports its first parent as `CurrentRef`, and its
    counts run from both parents.
  - **For phase 6:** with a missing base, the jj driver returns
    `vcs.ErrBaseNotFound` with no `Pushed` verdict, while the git driver
    returns a `*vcs.CountError` that keeps its verdict. The destroy guard
    must treat the two the same: refuse unless a verdict says pushed.
    *Done (2026-10-28):* the jj driver keeps the verdict wherever it needs no
    base, with a `*vcs.CountError`, as the git driver does (phase 6, same
    section).
- **R, the push remote (Q8, built as recommended).** R is git's push remote
  for the slot's branch, read from the slot's colocated git config:
  `git.Client.PushRemoteFor(slot, X)`. That is publish's order for git
  slots (#122): the branch's `pushRemote`, `remote.pushDefault`, the
  branch's remote, origin, the only remote.
  - When git names none (`*git.NoPushRemoteError`), Status reports no
    upstream.
  - A jj `git.push` setting is ignored by angee. jj itself ignores git's
    `remote.pushDefault` and `branch.<name>.pushRemote`. It pushes to the
    remotes its `git.push` names, else the only remote, else origin, and
    with several remotes and no origin it refuses.
  - So phase 8 must pass this R to `jj git push --remote`. jj keeps to the
    canonical remote's push block, since it pushes through git.
  - The first version of this PR read `git.push` and copied jj's fallback.
    That was a second implementation of jj's rule, and it matched patterns
    loosely. The review of #126 replaced it.
- **Repository config lives outside the repository (finding; Q9, built as
  recommended).** jj 0.45 writes `jj config set --repo` values, and the
  `trunk()` alias of `jj git init`, to
  `$XDG_CONFIG_HOME/jj/repos/<id>/config.toml`, keyed by
  `.jj/repo/config-id`, not to `.jj/repo/config.toml`. D8's setting is
  therefore per user, so the runner passes it on every command, and the
  test fixtures no longer set it. Every jj call in the tests runs under a
  temporary HOME and `XDG_CONFIG_HOME`. A test checks that the probe and the
  fixtures, run from inside a jj repository, write nothing to the HOME they
  started with.
- **Stale working copies (finding).** A rewrite from another workspace
  leaves a slot stale only when it changes the slot's files (a rebase onto
  a moved base). A new description on `@-` does not.
- **Diff.**
  - Uncommitted changes: `jj diff --git` of `@`, against its parents.
  - With a ref: `--from first_parent(@) --to "<ref>"`, so a two-parent `@`
    works too. The ref is one quoted symbol, so a remote bookmark
    (`main@origin`) cannot be named yet; phase 8 decides how a jj slot's
    refs are spelled.
  - The diff reads at the snapshot's operation. The output is git's format,
    and it goes through `vcs.DiffFiles`, moved out of `gitdriver` so both
    drivers share it.
- **For phase 8.** Once `jj git push --remote fork --bookmark X` has run,
  pushing X to origin is refused until `jj bookmark track X@origin`.
- **Doctor (step 7).** `tool.jj` reports the version and "workspace add
  --colocate", and warns without them. `tool.git` warns below 2.42.
- **CI (step 8).**
  - `.github/jj-rev` holds `f8f808f9…`.
  - Job `test (jj)` builds jj with `cargo install --git … --rev … --locked
    --bin jj jj-cli` into `~/.jj-ci`. It restores and saves that directory
    by OS, architecture and revision, saving right after the build so a
    failing test keeps it.
  - It runs `go test -race ./internal/vcs/... ./internal/service/...` with
    `ANGEE_REQUIRE_JJ=1`, then regenerates the template fixtures and fails
    on a diff.
  - A cold build took 2 min 30 s on an 18-core laptop with an empty cargo
    home, and 3 min 42 s on the runner. Branch protection is unchanged.
- **Tests.**
  - Fixtures: decode tests over `testdata/*.ndjson` run with no jj
    installed.
  - Status, ported from the probes: clean; an edit jj has not recorded
    (dirty, D7); ahead with no upstream; on and ahead of its upstream;
    behind; the base moved under a published slot (`BaseBehind`); a
    conflicting rebase that exits 0 (D5).
  - The `CurrentRef` rules. A two-parent `@` in Status and in Diff.
  - The push remote follows git:
    - a jj `git.push` has no effect;
    - `remote.pushDefault` applies, and a branch's `pushRemote` comes
      before it;
    - the only remote is used even when it is not named origin;
    - with no push remote there is no upstream;
    - the `git` pseudo-remote is never the base or the upstream.
  - Stale recovery (D8) on a repository that does not set
    auto-update-stale, where a plain jj fails. Only through status:
    sync-base and publish come with phase 8.
  - Reads pinned to the snapshot's operation; a missing base; Diff.
  - The Q10 known limit, for both cases (replaced once Q10 was settled, in
    phase 6's prerequisites).
  - Nothing reaches the developer's jj config.
  - Hostile names, which jj itself resolves: quotes, backslashes, `@`,
    `..`, spaces, an injected expression, a tab, non-ASCII.
  - The runner's arguments, environment, redaction and timeout, and its
    refusal of an empty slot, through a fake jj.
  - The probe through fake jj and git, including a cancelled and a
    timed-out probe that is not kept, and the per-verb refusals.
  - Mutants caught:
    - an unescaped quote, an unescaped backslash, bare names;
    - a non-snapshotting first read, a runner that never snapshots;
    - a conflict flag that is never read, the slot's commits never listed;
    - a probe run in the process's directory;
    - an empty slot let through;
    - a timed-out probe kept;
    - an upstream always on origin, or on jj's `git.push`;
    - reads not pinned to the snapshot.

**As built in phase 5 step 6 (2026-10-12).** `EnsurePrimary`, `Exists`,
`Add` and `Remove` are in `jjdriver/lifecycle.go`. Nothing selects the
driver yet; phase 6 wires it.

- **Where `EnsurePrimary` lives.** It is a method of `jjdriver.Driver`, not
  of `vcs.Driver`. A git primary needs nothing, so the git driver would only
  fake it (D2's argument). And the service must call it before `Exists`
  reads the primary's workspaces, so it cannot be a step of `Add`. Phase 6
  calls it under the primary lock, after materializing the cache.
- **`EnsurePrimary`** runs `jj git init --git-repo <primary> <primary>`
  when the primary has no `.jj`, and runs nothing when it has one. (`jj git
  init` on a repository that has one fails.)
  - That is step 6's `--colocate` with one difference, found while probing.
    It makes the same colocated repository: the same `.jj` files, and `jj
    git colocation status` says colocated. But jj refuses a directory that
    is not a git repository, where `--colocate` creates a new, empty one in
    a missing or emptied cache. The two flags cannot be combined.
  - jj runs in the system temporary directory, with the primary as its
    destination. Run inside the primary, it also loads any jj repository
    above it, and records that one in the user's config directory (probed).
- **Q9: the operator needs nothing else.** `EnsurePrimary` writes no
  repository config. The runner passes `snapshot.auto-update-stale` on
  every command, and `jj config set --repo` would write to the user's
  config directory, outside the repository. jj's init records a `trunk()`
  alias there by itself; nothing in the driver reads `trunk()`. The agents'
  side of Q9 stays open: a host operator writing the setting would have
  covered agents running as the same user, and this rules that out.
- **`vcs.Slot.Name`** carries the jj workspace name. The service computes
  it in phase 6 (Q2) and never stores it. Only the jj lifecycle reads it,
  since slot reads and verbs run in the slot. `Primary` is reused for the
  jj primary. `Mode` stays git's; phase 6 adds `jj` when `slotDriver`
  chooses by mode.
- **`vcs.Dest`** gains `IsEmptyDirectory` and `MkdirAll`, which
  `copierx.GuardedPath` already had. `vcs.ErrNameTaken` marks a collision.
- **`Exists`** runs `jj workspace list -T json(self)` with `-R .
  --ignore-working-copy` in the primary, as slot commands run in the slot,
  and matches by name. It never reads `root`.
- **`Add`** creates the slot in place (D4). Every write goes through
  `dest`:
  1. it refuses a non-nil `configure`, a missing name or primary, a
     relative path and an empty base ref;
  2. the destination must be absent or an empty real directory;
  3. the primary must have no workspace of that name, else
     `vcs.ErrNameTaken` naming it (D13);
  4. `dest.MkdirAll`, then `dest.VerifyPathIdentity` (before);
  5. `jj workspace add --colocate --name=<N> -r <start> -- <path>`;
  6. `dest.VerifyPathIdentity` (after).

  Any error from step 4 on rolls back through `Remove`.
  - The start is one revset that jj resolves:
    `coalesce(bookmarks(exact:"X"), remote_bookmarks(exact:"B",
    remote=exact:"R"), bookmarks(exact:"B"))`. That is the local bookmark
    of the slot's branch, else the base by Status's rule
    (`baseCandidates`, now shared by both). A branch that only a remote has
    does not count, as for a git worktree slot. No bookmark is created.
  - `jj workspace add` is the one command that runs in the primary without
    `--ignore-working-copy`. jj refuses it with that flag, after creating
    the workspace (exit 1). And the snapshot's git import is what shows jj
    the base the service has just fetched into the cache with git. A test
    moves the base with git and checks that the slot starts on the new
    commit.
  - **D13's check is load-bearing, not just a clearer message.** jj
    refuses a taken name itself, before writing anything. But the rollback
    forgets N, so without the operator's check a collision would forget the
    other slot's workspace and delete its `.git` (the mutant shows it). One
    window remains: a process that does not take the lock, such as a human
    running `jj workspace add`, could take the name between the check and
    the add.
  - The destination check comes first for the same reason. jj refuses a
    non-empty directory, but the rollback would then delete what was in
    it.
  - With a swapped parent, jj writes the checkout where the symlink leads,
    and records that path. The after-check catches it. The rollback forgets
    the workspace and removes the directory `Add` made, through the guard.
    `Add` itself removes nothing through the swapped path, which is what the
    guard prevents, but the forget does, as jj: it deletes the `.git` jj
    wrote at the recorded path and runs its own `git worktree prune`
    (probed; it also pruned an unrelated missing worktree). The rest of what
    jj wrote there stays, and the test pins both.
  - Parents that `MkdirAll` created stay after a failure, as they do after
    the git driver's late failures.
- **`configure` is refused on a jj slot**, with `vcs.ErrUnsupported`. A
  colocated slot shares its primary's git config (it is a linked worktree)
  and jj repository, so it has nothing of its own to configure: its remotes
  are the primary's, reconciled on the cache. And running `configure` after
  jj has written the slot would break `vcs.Driver`'s promise that nothing at
  the path is unconfigured. Phase 6 passes nil, as for worktrees.
- **`Remove`** runs `jj workspace forget -- <N>` on the primary with
  `--ignore-working-copy`, then `dest.RemoveAll`, then `git worktree prune`
  through `git.Client`.
  - Forgetting an unknown name changes nothing, so `Remove` can run twice.
  - Each step runs even when an earlier one failed, and the errors are
    joined.
  - The forget drops the git registration (and the slot's `.git`) while the
    directory exists. The prune drops it when the directory was already
    gone.
- **The primary lock** stays the service's. The docs of `vcs.Driver` and
  `jjdriver` say which calls need it:
  - `EnsurePrimary`, since it creates `.jj` the way a clone creates the
    cache;
  - `Add` and `Remove`: workspace add and forget, then the prune.

  `removeWorkspaceSlot` already takes the lock for any slot with a
  `Primary`, so jj rollback and destroy take it once phase 6 hands it jj
  slots. They are not yet fully covered: when the lock times out,
  `removeWorkspaceSlot` falls back to `dest.RemoveAll()` and relies on the
  next `git worktree prune` to drop the registration, but nothing forgets a
  jj workspace, so its name stays taken and recreating the workspace fails
  with `vcs.ErrNameTaken` until someone runs `jj workspace forget`. Phase 6
  must forget the name on that path too (or retry under the lock). The
  create path needs a jj branch beside `addWorktreeSlot`. No service code
  changed. *Settled in phase 6: the name is forgotten once the directory is
  gone, which needs no lock (`ForgetName`).*
- **A failed `Add` removes an empty directory that was already at the slot
  path.** `dest.MkdirAll` keeps it, and the rollback's `Remove` deletes it.
  The git driver builds the checkout elsewhere and installs it at the end,
  so its failed `Add` leaves the directory. Phase 6 either recreates the
  directory after the rollback or treats this as the jj lifecycle's
  contract and says so where the service calls `Add`. *Settled in phase 6:
  the service puts the directory back.*
- **Status** of a slot that `Add` made works unchanged (a test).
- **Probe findings** (jj `0.45.1-f8f808f`):
  - `jj workspace add --ignore-working-copy` creates the workspace and its
    git worktree, then exits 1.
  - `jj workspace add` with a taken name, or into a non-empty directory,
    refuses before writing anything.
  - Until it is forgotten, a failed add's git worktree reads `HEAD 0000…`
    on `refs/jj/root`.
  - `jj workspace forget --ignore-working-copy` removes the git worktree
    registration and the slot's `.git` file while the directory exists,
    through a swapped parent too. `.jj` and the files stay.
  - After `git pull` in a jj-colocated cache, a `jj workspace add` there
    leaves git's HEAD on `main`, and `git pull` still works. So git worktree
    slots and `source pull` keep working on a cache that also has jj slots.
- **Tests.** Integration tests run under `isolate`. They skip without jj,
  and fail with `ANGEE_REQUIRE_JJ=1`.
  - `EnsurePrimary` colocates and leaves git on `main`. A second call with
    no jj on PATH succeeds and records nothing. A plain directory and a
    missing one are refused, and get neither `.jj` nor `.git`.
  - `Add` makes a colocated slot: `git rev-parse` works, and HEAD is
    detached at the base just fetched with git. No branch or bookmark is
    created, and git and jj both list the slot.
  - `Add` lands on an existing local bookmark, and Status reads that slot
    as on its branch. A branch that only the remote has starts at the base.
  - Status of a slot `Add` made: clean and pushed, then dirty.
  - A taken name gives `vcs.ErrNameTaken` naming it, with no directory, no
    operation, and the other slot intact.
  - A non-empty directory and a file are refused and left untouched, with
    no operation. An empty directory is taken.
  - A base that resolves to nothing (jj creates the workspace, then exits
    1) leaves no workspace, no directory and no registration.
  - A parent swapped for a symlink after the before-check is detected. The
    workspace is forgotten and unregistered, the guard's directory is
    removed, and what jj wrote outside is left.
  - `Remove` leaves no workspace and no registration. Its order is pinned:
    the primary no longer lists the workspace when the directory is
    removed, and a recording git shows the prune after the removal.
  - `Remove` after the directory was deleted forgets and prunes. A second
    `Remove` changes nothing.
  - `Exists` before `Add`, after it (also for another name), and after
    `Remove`.
  - Hostile names: quotes, `\..@`, a leading `-`, a space, non-ASCII, and
    an injected revset.
  - Missing facts and a non-nil `configure` are refused before any command
    runs. A fake jj and git pin each command, its arguments and its
    directory.
  - Through the fake, which can fail chosen commands: a failed forget still
    lets `Remove` remove the directory and prune, and a failed prune is
    joined to it; a caller's context cancelled during `Add` still rolls
    back (forget, remove, prune); and a failed rollback is joined into
    `Add`'s error.
  - `TestNothingReachesTheDevelopersConfig` runs the lifecycle too. It now
    also checks that no jj the driver ran loaded the repository the test
    runs in.
- **Mutants caught:**
  - no rollback;
  - no name check (the other slot's workspace is forgotten);
  - the removal before the forget, or the prune before the removal;
  - no identity check after jj, or none before it;
  - no destination check (the rollback deletes the directory's contents);
  - `--colocate` in `EnsurePrimary`;
  - `jj git init` in the process's directory;
  - `workspace add` with `--ignore-working-copy`;
  - `Remove` stopping at a failed forget, or dropping its error;
  - the rollback on the caller's context, or with its error dropped.
- **For phase 6:**
  - Call `EnsurePrimary` only for a `kind: git` cache, never for a `kind:
    local` store (step 7: the operator never initializes one). Step 7 also
    needs `Add` without `--colocate` for a store that has no `.git`.
  - Hold the lock around `Exists`, `Remove` and `Add` together, as
    `addWorktreeSlot` does, but not through its leftover removal. For git,
    `Exists` finds a worktree at the slot's own path, a leftover. For jj it
    finds the name taken, possibly by another live slot, which `Remove`
    would forget, deleting its `.git`. Step 4's keep-a-registered-slot rule
    decides before `Add` runs. Map `vcs.ErrNameTaken` to a conflict.
  - Set `Slot.Name` and `Slot.Primary` in `workspaceLifecycleSlot`, and
    pass a nil `configure`.
  - **`Add` changes the shared cache's git format.** `jj workspace add
    --colocate` runs `git -c worktree.useRelativePaths=true worktree add`,
    which sets `core.repositoryformatversion=1` and
    `extensions.relativeworktrees=true` on the cache and writes relative
    links. That undoes what #130 keeps for git worktree slots (`worktreeAdd`
    in `internal/git/git.go`). Found by probe (jj 0.45.1, git 2.50.1):
    - go-git's `PlainOpen` refuses the cache, so `sourceCommits`
      (`internal/service/commits.go:40`) returns no commits, and no error,
      for that source in the GitOps topology;
    - git before 2.48 does not know the extension and may refuse the cache
      and its git worktree slots, while doctor accepts 2.42.

    So before phase 6 hands jj a cache: require git ≥ 2.48 for jj slots
    (doctor's floor and the create path); give `sourceCommits` a git CLI
    fallback, or surface its error; and pin the cache's config after `Add`
    in a test, with a git worktree slot of the same cache still working.

    **As built (2026-10-28)**, all but the create path's check, which comes
    with `mode: jj`:
    - **The floor.** `git.RelativeWorktreesVersion` ("2.48") and
      `git.VersionAtLeast(versionLine, floor)` are in `internal/git`, for
      doctor and the create-time check alike. `jjdriver.MinGitVersion` and
      `GitSupported` stay at 2.42: the probe belongs to the `mode: jj` PR,
      which can raise its floor with them.
    - **Doctor.** `tool.jj` warns when jj can create slots but git is older
      than 2.48 (`jj 0.45.1 (workspace add --colocate), but git version
      2.47.2 is older than 2.48, which jj slots need`) or reported no
      version. `tool.git` still warns only below 2.42, now naming 2.48, so a
      stack that does not use jj gets no new warning. `tool.git` reads
      `jjdriver.GitSupported` (`gitBelowJJFloor`), so raising the probe's
      floor would make it warn every stack with git 2.42 to 2.47: whoever
      raises it keeps `tool.git` on 2.42 or leaves the warning to `tool.jj`.
    - **`sourceCommits`** falls back to the git CLI when go-git refuses the
      cache for any reason but `gogit.ErrRepositoryNotExists`, which still
      returns nothing: git would read the repository above a directory that
      is not one. The fallback, `sourceCommitsCLI`, reads `git.Client.Log`
      and `RefTargets` (`git for-each-ref`, symbolic refs left out), both
      from standard output only. A failed fallback carries go-git's error
      too, and the topology now logs it as a warning instead of dropping it.
      - `Log` runs `git --no-replace-objects log -z --no-show-signature
        --encoding=UTF-8 --max-count=N --format=… HEAD --`, so neither
        replace refs (go-git follows none) nor `i18n.logOutputEncoding`
        change what it reads. A HEAD that `rev-parse --verify --quiet`
        rejects with exit 1 (an unborn branch) is an empty history; a HEAD
        naming a missing commit is an error. `--ignore-missing` was tried
        and dropped, since it reads that broken HEAD as no commits.
      - It returns what the go-git walk returns, which a test compares
        field by field across a merge, tags, `origin/HEAD` and a non-ASCII
        author, and another end to end through the topology before and
        after the extension is set. The one difference is order among
        commits with the same committer time: git keeps them first in,
        first out, and go-git's heap in no set order, so with a limit the
        last of them in the window can differ too.
    - **The other go-git opens.** `git.Client`'s readers (`RefExists`,
      `CurrentRef`, `CurrentBranch`, `Upstream`, `Remotes`) already fell
      back to the CLI on any failure to open. A test pins that they answer
      the same before and after the extension is set, in the cache and in a
      worktree slot. No other code opens a cache or a slot with go-git.
    - **The pin.** `TestAddChangesTheCachesGitFormat` (`jjdriver`): after
      `Add`, the cache has `core.repositoryformatversion=1` and
      `extensions.relativeworktrees=true`, and the jj slot's `.git` is
      relative. Git worktree slots the git driver adds before and after it
      keep absolute links, read clean on their branch through the git
      driver's Status, and stay registered when the other is removed and
      pruned, as does the jj slot. It skips without jj or git 2.48, and
      fails under `ANGEE_REQUIRE_JJ=1`.
    - **Finding.** A jj-colocated cache has a `refs/jj/keep/<commit>` ref
      for each commit jj keeps from git's garbage collection, the base
      included (probed), and the topology lists them among a commit's refs,
      through go-git and the CLI alike. Phase 7 decides whether to hide them.
  - **Workspace names can collide across stacks.** `<stack>--<workspace>`
    is ambiguous once a part contains `--`: stack `a--b` with workspace `c`
    and stack `a` with workspace `b--c` both get `a--b--c` (and `--<slot>`
    adds more ways). On a shared cache D13 refuses the second with a clash
    its own manifest does not show, and `Exists` reports the name taken.
    Refuse `--` in the parts, or pick a form that cannot collide, before
    names are computed.

    *Settled in phase 6:* a part that is empty, holds `--`, or starts or
    ends with `-` is refused, which makes the names injective.

### Phase 6. `mode: jj` lifecycle and status

**Goal:** `ws create`, `ws status` and `ws destroy` work for jj slots,
including `.work`. Q2 is answered: workspaces are named `<stack>--<workspace>`.

From this PR on, `main` accepts `mode: jj`. So that it is never half-wired,
the same PR makes every slot verb that phase 8 has not yet implemented
(fetch, pull, push, sync-base, publish, merge, rebase, abort, continue,
diff, and `ws push`/`ws sync-base` over a workspace containing a jj slot)
return a typed "not supported on a jj slot yet" error, tested per verb.

Steps:

1. Manifest: `Stack.VCS` (`vcs: git|jj`, default `git`) and `jj` added to the
   slot mode enum from phase 1. Effective mode, one function used everywhere:
   an explicit mode wins; an omitted mode is `jj` when `vcs: jj`, and
   otherwise today's behaviour (D3). `mode: jj` is valid on `kind: git` and
   on a `kind: local` path that already contains `.jj/`; the operator never
   initializes a local path.
2. Workspace name per the answer to Q2, computed in one helper and never
   stored in the manifest. Before adding, list the primary's workspaces: a
   name already present that this stack's manifest does not account for is
   an error naming the collision (D13).
3. `materializeWorkspaceSource` (`:1570`), jj branch: probe and fail with the
   hint when unsupported; materialize the source cache as today;
   `EnsurePrimary`; then create the slot in place (D4), on the slot's branch
   when a bookmark of that name exists and on the base otherwise: through
   the guard, verify the parent directory and that the destination is
   absent or an empty directory; run `jj workspace add --colocate` with the
   final path, under the primary lock; verify the path identity again; on a
   mismatch or any error, `jj workspace forget`, remove the directory
   through the guard, `git worktree prune`.
4. `--sync` and an existing destination. If the slot is registered (`Exists`)
   and the directory is a workspace of this primary, **keep it as it is**:
   create becomes a no-op for that slot. Never forget-and-recreate a
   registered slot, because forgetting detaches the slot from its
   working-copy commit and the recreated slot would not contain that work.
   A registered name whose directory is missing is forgotten and re-added.
   A non-empty directory that is not a registered workspace still errors.
5. Rollback and destroy: `workspaceSourceCleanup` and
   `removeWorkspaceSources` (`:1469`) call `driver.Remove`. The destroy guard
   uses a snapshotting status, so unsaved edits count as dirty. Two things
   from phase 5 come first (both built: "As built: the driver prerequisites
   of step 5" below):
   - Q10 must be settled, since a file jj refuses to snapshot reads as
     pushed today;
   - with a missing base, the jj driver returns `vcs.ErrBaseNotFound` with
     no `Pushed` verdict, where the git driver returns a `*vcs.CountError`
     that keeps one, and the guard must treat the two alike.
6. Status: `workspaceSourceStatus` (`:392`) builds the existing DTO fields
   from `driver.Status` for jj slots. There is no `branch-mismatch` on a jj
   slot. Every status read snapshots (D7).
7. `kind: local` with effective mode `jj` no longer symlinks (`:1726`); it
   gets a jj workspace of the store. `--colocate` is passed only when the
   store has a `.git`.
8. Containers. The operator container mounts the stack root at the identical
   path, but a shared primary under `sources_home` is outside the stack
   root. The create path checks that the primary is reachable at the same
   absolute path the manifest names and fails with a message naming the
   missing mount otherwise. No path translation is attempted.

Tests (integration): create a workspace with one worktree slot and one jj
slot from the same primary; `git rev-parse` succeeds in the jj slot and no
branch exists; status of clean, dirty (unsnapshotted edit), ahead and
behind; neither slot is `branch-mismatch`; destroy forgets the workspace and
leaves no `git worktree` registration; a failed create rolls back;
`--sync` over a slot holding uncommitted work keeps the work; a local store
slot becomes a jj workspace; `mode: jj` without the capability fails before
touching disk; every unfinished verb returns the typed error.

**As built: the driver prerequisites of step 5 (2026-10-28).** Both are in
`internal/vcs/jjdriver`. No service code changed, and nothing selects the
driver yet.

- **Q10, recommendation (a).** Status counts jj's own report of the files it
  leaves out as dirty.
  - Probed (jj `0.45.1-f8f808f`):
    - A new file over `snapshot.max-new-file-size` makes every snapshotting
      command print `Warning: Refused to snapshot some files:` on stderr,
      then a line per file (`  big.bin: 200.0B (200 bytes); the maximum size
      allowed is 16.0B (16 bytes)`) and a hint, and exit 0.
    - A file outside `snapshot.auto-track` prints nothing at all.
    - Only a snapshotting `jj status` names both, on stdout: an `Untracked
      paths:` line, then a `? <path>` line each. With
      `--ignore-working-copy` (so `--at-op` too) it names neither: the
      listing comes from the command's own snapshot.
    - A directory jj tracks nothing in is one `? dir/` entry. Names print
      raw, so a line break in one splits its line. Ignored files are never
      listed. `--quiet` and `ui.quiet` change none of it.
  - So Status reads `jj status`'s listing, which covers both cases, and not
    the warning, which covers one. **`jj status` is the call's snapshot**,
    run in the slot without `--ignore-working-copy`, so the working copy is
    walked once. Diff keeps `jj op log` as its snapshot.
  - **The reads are pinned to the operation that snapshot left the
    repository at** (`statusSnapshot`). `jj status` does not print it, and
    another jj command can record an operation just before or just after
    it. So Status reads the latest operation (`jj op log -n 1
    --ignore-working-copy`, `runner.latest`, with jj's `json(self)`) before
    the snapshot (A) and after it (C), and takes C only when C is A (the
    snapshot found nothing) or C is a snapshot (`is_snapshot`) whose only
    parent is A (`settled`). Otherwise it snapshots again from C, at most
    three times, then fails ("other jj commands recorded operations during
    each of 3 snapshots"), and the destroy guard refuses. A status is then
    nine to eleven jj runs.
    - Probed (jj 0.45): an operation's `json(self)` carries `id`, `parents`,
      `is_snapshot`, `workspace_name` and `attributes.args`. `jj status`
      records nothing when nothing changed, and one snapshot after A when
      something did. It also records operations that are not snapshots:
      `reset git head` when it updates a stale working copy (D8), and
      `import git refs` after a `git fetch` in the colocated primary. Each
      costs one retry.
    - "C's only parent is A" alone is not enough: when the snapshot records
      nothing and another command's operation follows A, C's parent is A
      but `jj status` described A. Requiring a snapshot rules that out. A
      snapshot of another workspace changes only that workspace's working
      copy, so it is accepted.
    - `operationTemplate` stays `id` for Diff's snapshot and for
      `BackupEvidence` (#136), which reads the bare id;
      `operationEntryTemplate` is the `json(self)` one. An operation has no
      testdata fixture: its id hashes the command lines before it, which
      hold the test's temporary paths. A recorded line sits in
      `templates_test.go` instead.
    - Found in the review of #139 (Sourcery).
    - A first version kept `jj op log` as the snapshot and ran `jj status`
      after it, a second snapshot. The review showed the gap: a file written
      between the two was in the report's changes but not in `@` at the
      pinned operation, so Status read clean and pushed; and every status
      walked the working copy twice.
  - `vcs.Status.UntrackedPaths` lists them as jj does, and they make `Dirty`
    true, so the slot is not `Pushed`. `UnpushedReason` names up to ten
    (`untracked paths jj does not snapshot: big.bin`), after `uncommitted
    changes, and ` when `@` holds changes too. The DTOs carry `Dirty` only;
    phase 7 adds the paths with the other jj fields.
  - **It fails closed.** Before the working-copy commit's line (`Working
    copy  (@) : …`), jj 0.45 prints `Working copy changes:` and a line per
    changed path, then `Untracked paths:` and its entries, each only when
    there are any, or `The working copy has no changes.` alone. The parser
    refuses:
    - an unknown line before the commit's line (a reworded or added
      header);
    - one of the three headings anywhere but its place: out of order,
      twice, inside the untracked listing, or after the commit's line (a
      listing jj moved);
    - a report with none of them, or without the commit's line (cut short);
    - the untracked heading with no entry after it.

    Status then fails, and the destroy guard refuses. Since D2 makes the
    baseline the jj on PATH, a newer jj than CI's pin reaches users first,
    and must fail loudly rather than read as clean. The placement and
    cut-short refusals came from the review of #139.
    - A line in the untracked listing that starts no entry is the rest of a
      name holding a line break, so the name is listed whole. A false
      positive (a changed file whose name holds a line break and a header)
      only makes a slot dirty.
    - A first version accepted the three lines anywhere and anything else
      around them, so a jj that rewords the listing and says "no changes"
      over it would have read as clean (review).
  - `UnpushedReason` quotes a name holding a character that does not print
    (`strconv.Quote`): jj prints names as they are, escape sequences
    included, and the reason reaches terminals and API clients.
    `UntrackedPaths` keeps them raw.
  - `testdata/status.txt` records the report (a tracked file, one over the
    size limit and a directory outside auto-track, in the conflict
    scenario). `-update`
    regenerates it with the template fixtures, so CI's fixture check shows a
    reworded report as a diff.
  - What jj leaves out follows the jj config of the operator's user, which
    the runner passes through (`HOME`, `XDG_CONFIG_HOME`, `JJ_CONFIG`). An
    operator with another config than the agents' (Q9) decides differently,
    but never reads a slot as pushed over such a file: a stricter config
    lists it as untracked, and a looser one snapshots it into the slot's
    working-copy commit, which then has changes. That snapshot records the
    file in the agent's commit, as any operator snapshot under D7 would.
  - **Not covered (found in review, probed): what jj neither records nor
    lists.** A directory holding a repository of its own (a nested `.git`,
    or `.jj`), and a new file outside the slot's sparse patterns, stay out of
    `@` and out of `jj status`; `git status` would list a nested repository
    as untracked. Status reads such a slot clean and pushed, and the destroy
    guard would delete them. A test pins both as a known limit. For the
    owner, under Q10.
  - **Known limit: check, then delete.** The destroy guard reads a slot and
    the removal follows, so a file written in between is deleted unseen.
    The git driver has the same window; only a lock the agents also take
    could close it.
- **A missing base keeps the verdicts that need none**, as in the git
  driver:
  - dirty (`@` has changes, or there are untracked paths): not pushed, with a
    `*vcs.CountError`;
  - clean, with an upstream: Ahead and Behind against it, pushed when
    nothing is ahead, with a `*vcs.CountError`;
  - clean, with no upstream: the base's error alone (wrapping
    `vcs.ErrBaseNotFound`), with no verdict, as for a git branch with no
    upstream on a deleted base.

  BaseBehind, the conflicts and CurrentRef's bookmark, which only the base
  bounds, are not read; CurrentRef is `@-`'s short id. Only
  `vcs.ErrBaseNotFound` counts as a missing base; any other failure to
  resolve one is returned as it is, with no verdict. The guard
  (`ensureWorkspaceGitSourcesPushed`) needed no change: it lets a slot go on
  a `*vcs.CountError` whose verdict says pushed, and refuses on any other
  error. `vcs.CountError`'s doc now says both drivers keep their verdict
  this way. The git driver is unchanged. For a dirty slot, and one with an
  upstream, it reads no base at all, so a deleted base returns no error
  there.
- **Tests.**
  - Real jj, mandatory with `ANGEE_REQUIRE_JJ=1`:
    - files jj leaves out: one over the size limit; one outside auto-track
      (`none()`); a directory outside it (`local/`) beside a file it tracks
      (`uncommitted changes, and …`). None of them ends up in `@`. An
      ignored file over the limit is neither dirty nor untracked.
    - a missing base: dirty; holding a file jj does not snapshot; on, and
      ahead of, its upstream (each a `*vcs.CountError` with its verdict);
      clean, or with a commit of its own, and no upstream (the base's error
      alone).
    - the known limit: a nested git repository, and a new file outside the
      sparse patterns, read clean and pushed.
    - another jj command's operation (a bookmark created in the primary)
      injected before or after `jj status` by a jj wrapper on PATH: one
      retry, for a dirty slot and for a clean one whose snapshot records
      nothing; injected around every attempt, a failure after three; none,
      one snapshot. A recorded and a live operation decode.
  - Without jj: the parser over jj 0.45's report shapes (no changes,
    changes, untracked, both, conflicts after the commit's line, odd names,
    a line break) and its refusals (a reworded listing, one after "no
    changes", an unknown line, a listing with no path, nothing; a listing
    after the commit's line, twice, or after "no changes"; changes after the
    listing; "no changes" after the commit's line; three reports cut short
    and one with only the commit's lines); `settled` over six operation
    shapes; the recorded fixture; the reason's wording, cap and quoting; the
    runner's `latest` (its arguments, and its refusal of an empty slot).
  - The git driver: dirty, on and ahead of its upstream, each on a deleted
    base, with no error.
  - Mutants caught (15): no `jj status` read (the old behaviour); untracked
    paths not dirty; `jj status` without a snapshot; the operation read
    before the snapshot; unknown lines accepted; no refusal of an
    unrecognized report, or of an empty listing; continuation lines
    dropped; the commit's line not ending the report; a missing base
    returned as before; an undecided slot reported pushed; no upstream count
    without a base; a verdict without a `*vcs.CountError`; a reason that
    names no paths; names not quoted. Not caught: the read after the
    snapshot snapshotting too, which costs a second walk and changes no
    result.
  - Mutants caught in the review of #139 (9): no operation check; the
    weaker "C's parent is A"; no retry; a retry that keeps the first A; one
    attempt past the bound; headings after the commit's line ignored; the
    commit's line not required; a heading inside the listing read as a
    name; the listing allowed after "no changes". The parser as first
    committed fails eight of the nine new parser cases.
- **For the rest of phase 6:**
  - `workspaceVCSSlot` sets `Slot.Branch` only for a worktree slot that
    names one. A jj slot needs it too: without it there is no upstream, so a
    published slot counts against the base, reads as ahead and unpushed,
    and with a missing base gets no verdict at all.
  - The guard and `workspaceSourceStatus` skip any source whose kind is not
    `git`. A local store's jj slot (step 7) is a real checkout, so both must
    read it through the jj driver.
  - The guard's refusal says "git sources that have not been pushed".
  - `ws status` will show a slot with only untracked paths as `dirty`, and
    a jj slot on a missing base as `error` with its reads, where git shows a
    state for a dirty slot or one with an upstream. Keep a bare
    `vcs.ErrBaseNotFound` a refusal in the guard.
  - The known limit above stands until the owner decides it (Q10).
- **For phase 8:** the jj driver's `DirtyPaths` must include
  `UntrackedPaths`, so the clean-checkout precondition (`requireCleanSlot`)
  agrees with Status.

**As built (2026-10-29).** The jj driver's additions, the manifest with the
refusals, the lifecycle, merges of main for #138 and #139, the
nested-repository guard with #138's git helper, and the fixes from the
`go-code-reviewer` pass. #139's notes for the rest of phase 6 are all done:
`Slot.Branch` for jj slots, `kind: local` jj slots read by status and the
guard, the guard's wording, and a bare `vcs.ErrBaseNotFound` refusing.

- **Step 1, the manifest.** `Stack.VCS` (`vcs: git|jj`, validated, in the
  JSON schema) and `manifest.WorkspaceSourceModeJJ`.
  `Stack.EffectiveSourceMode(ws)` is the one answer: the slot's own mode,
  else `jj` under `vcs: jj`, else empty (D3). The service reads every slot
  through `withEffectiveMode`, never saved: the record keeps the mode as
  written, so `vcs` decides for every slot without one, existing ones too.
  Changing `vcs` on a stack with workspaces therefore reads their unmoded
  slots the other way; migrate (phase 9) is the way to move a slot.
- **Step 2, names.** `jjWorkspaceNames` computes `<stack>--<workspace>`,
  and `--<slot>` for each of several jj slots of one primary (grouped by
  the primary's path), at create, repair, rollback and destroy. A part that
  is empty, holds `--` or starts or ends with `-` is `INVALID_INPUT`, so
  two names never meet. The names follow the recorded slots, so adding a
  second slot of one repository to an existing workspace renames the first;
  its old name then reads as a collision, never as a name to forget.
- **Steps 3 and 4, create.** Before any slot is cut (`jjSlotTargets`): the
  capability probe (`requireJJSlots`: `jjdriver.Probe`, plus
  `git.VersionAtLeast(git, git.RelativeWorktreesVersion)`;
  `PRECONDITION_FAILED`, cause `jj_unavailable`, with the hint, matching
  `vcs.ErrUnsupported`; `Platform.jjProbe` replaces the probe in tests),
  the names, and step 8's check. An optional jj slot only fails itself.
  Then, in `materializeJJSlot` and under the primary lock, `addJJSlot`:
  refresh the cache and `EnsurePrimary` (a git source only), then by
  `Exists` and what the path holds:
  - taken, and the path holds that workspace's checkout (`Holds`: jj run in
    the path loads a workspace of that name whose working-copy change is
    the primary's; --sync only): **kept** as it is, and a rollback leaves it;
  - taken, and the path is absent or empty: forgotten and added again only
    when the saved manifest records the slot (create of a declared
    workspace, repair) and `Elsewhere` finds no other checkout. `Elsewhere`
    reads jj's record of the name's path (`Recorded`: `jj workspace root
    --name`, or, when nothing is there, the path jj's `Error: Cannot
    resolve absolute workspace path: <path>` names) and fails closed: a
    record of the slot's own path is the slot's, resolved or not; a record
    of another path counts whenever something is at it, readable or not;
    a record of another path with nothing at it is
    `jjdriver.ErrRootUnresolved`, which `reclaimJJName` treats as this
    stack's own only for a primary inside the stack root with symlinks
    resolved (`vcs.PathInside`), since a shared cache's record may be
    another stack's checkout out of sight (a container that does not mount
    it). Repair names the slot by the record at its path too
    (`recordedJJSlotTarget`): of `<stack>--<workspace>` and its `--<slot>`
    form, the one jj recorded at the slot's path, else the computed one, so
    a slot whose name create gave beside a skipped optional sibling is
    added again under that name, not beside it;
  - taken otherwise: `CONFLICT` naming the name, where it lives when known,
    and the forget command, shell-quoted (D13). An unrecorded create never
    forgets a name, since the name alone cannot tell this stack's leftover
    from another stack's slot;
  - not taken, path populated: refused; otherwise `Add`.
  `addWorktreeSlot`'s leftover removal is never used.
- **Step 5, rollback and destroy.** Through `driver.Remove`. A jj slot made
  in an empty directory records its mode, and both a failed create of that
  slot and a later slot's rollback put the directory back (a forget and
  re-add of an empty directory recreates it at 0755). Destroy forgets the
  name the checkout at the path is (`destroyedJJSlot`: `HeldChange` for
  `<stack>--<workspace>`, then `--<slot>`), else the one jj recorded at the
  slot's own path (an emptied directory), not the one the manifest computes
  now, which drifts when an optional sibling was skipped or a slot added or
  removed since; with neither, the slot is removed from disk only, with a
  warning, so a name another stack holds is never forgotten. `HeldChange`
  reads only a real directory, never through a link. Destroy removes its
  slots on a context the request cannot cancel, 30 s per slot, and logs
  what fails. **The driver's `Remove` removes the directory first**, then
  forgets the name, then drops the slot's own git registration with `git
  worktree remove --force --force <path>` (`deregister`), which with
  nothing at the path removes only that registration (probed on git 2.50).
  This replaces section 2's "forget, remove, prune": `jj workspace forget`
  of a workspace whose directory exists also prunes every missing worktree
  of the primary (probed), and `git worktree prune` does the same, both
  dropping registrations of other stacks' slots a container cannot see. No
  path of the driver prunes now.
  When the lock times out (`removeJJSlotUnlocked`), the slot's change is
  read, the slot removed through its guard, and the name forgotten
  (`ForgetName`) only while it still has that change, so a workspace of
  the same name the lock's holder added meanwhile is left alone: with the
  directory gone, `jj workspace forget` changes only jj's record and
  prunes nothing (probed), so it needs no lock.
  The destroy guard reads jj slots, `kind: local` ones included, through
  the snapshotting status; only a `*vcs.CountError` lets an unpushed read
  through, so a bare `vcs.ErrBaseNotFound` refuses. Its message no longer
  says "git sources". **Nested repositories (owner, 2026-10-29):** the
  guard also walks a jj slot through `os.Root`, following no link and
  skipping nothing, and refuses while any `.jj` directory, or `.git`
  directory or file, sits below the slot's own (`nestedRepositories`): jj
  records none of them, so status reads clean and pushed over their work
  (a test pins that). `.git` files count too, since a git worktree or
  submodule checkout holds work the same way. The workspace's other slots
  nested in the jj slot (a jj slot at `.`) are skipped, since each is read
  on its own. An unreadable directory refuses, saying to make it readable
  or remove it. The walk runs for a destroy without `--purge` too, as the
  pushed check does. Every `.git` or `.jj` entry counts, links included;
  the top-level `.git` is the slot's own only when the primary is
  colocated. Status does not walk. A link at a jj slot's path (a local
  source linked before `vcs: jj`) is not a checkout: status reports an
  error, the guard has nothing to read, and destroy removes the link. An
  empty directory at a jj slot's path, which repair takes for a missing
  slot, holds nothing either: the guard lets it go and destroy forgets the
  name recorded there.
- **Step 6, status.** `workspaceSourceStatus` asks the jj driver for a jj
  slot, `kind: local` included; `Mode` is the effective mode; no
  `branch-mismatch`. `workspaceVCSSlot` sets `Slot.Branch` for a jj slot
  (its bookmark, for the upstream and `CurrentRef`) and `Slot.Mode = jj`,
  which `slotDriver` selects by. Repair recognises a jj slot by its `.jj`.
- **Step 7, local stores.** A `kind: local` source with effective mode jj
  gets a jj workspace of its store, which must already have `.jj` (else
  `PRECONDITION_FAILED`, and angee never initializes it). The driver passes
  `--colocate` only when the store has `.git`; it then also skips the
  prune on Remove and asks git for no push remote in Status, since git run
  without a `.git` acts on whatever repository encloses the store. The
  store's lock is taken as a cache's is.
- **Step 8, containers.** `checkJJPrimary`: a cache inside the stack root
  is reachable wherever the stack is, so a missing one is cloned as usual;
  one outside it, or a `cache_path` symlink to nothing, must exist, else
  `PRECONDITION_FAILED` naming the path to mount at that path (or `angee
  source fetch` on the host). No path is translated.
- **The refusals.** Every verb on a jj slot that phase 8 has not built
  (fetch, pull, push, publish, merge, rebase, merge-abort, rebase-abort,
  rebase-continue, diff, and `workspace push` and `sync-base` over a
  workspace with one) is refused by the service before any driver call:
  `INVALID_INPUT` (HTTP 400), cause `jj_slot_unsupported`, naming the verb
  and the jj slots, matching `vcs.ErrUnsupported`. Diff is refused although
  the driver implements it, as listed above, until phase 8 decides how a
  jj slot's refs are spelled. The aggregates would otherwise skip a jj slot
  silently (they select by mode).
- **Driver additions.** `Holds`/`HeldChange`, `Recorded`, `Elsewhere` (with
  `ErrRootUnresolved`) and `ForgetName(s, change)` (`jjdriver/lifecycle.go`,
  not on `vcs.Driver`, reached through the service's `jjLifecycle`
  interface); `vcs.ModeJJ`. `GitSupported` uses `git.VersionAtLeast`,
  keeping 2.42 for doctor. **Finding:** git refuses `jj workspace add
  --colocate` at a path where it still registers a missing worktree ("is a
  missing but already registered worktree"), as the unlocked rollback leaves
  it, so `Add` first drops that one registration (`dropStaleRegistration`:
  an empty directory there goes first, since git deregisters a path only
  once nothing is at it), never pruning. jj itself leaves nothing behind
  when that add fails. `vcs.CanonicalPath` and `vcs.PathInside` compare
  paths whose last components may not exist. Known limits: `HeldChange`
  reads a workspace that `jj edit`ed another's working-copy commit as
  holding it; `ForgetName` checks the change and forgets in two jj
  commands, so a name re-added in the moment between them is still
  forgotten.
- **Tests.** As listed above, plus: a slot with no mode under `vcs: jj`;
  rollback into a pre-existing empty directory, for the jj slot's own
  failure and a later slot's; --sync refusing a path that holds something
  else, whether the name is taken or not, and a --sync create that fails
  later leaving the kept slot; repair re-adding a slot whose directory was
  deleted; two stacks named alike sharing an outside cache (`CONFLICT`,
  the first stack's slot intact); name parts; the capability (no jj; git
  2.47); step 8 (outside the root, dangling symlink, a local store with no
  `.jj`); colocated and non-colocated local stores, with the destroy guard
  on one; the unlocked rollback forgetting the name, and the next create
  succeeding over the stale registration; nested repositories (the walk,
  and destroy end to end); an operator contract case for the refusal's
  REST status and GraphQL extensions. Driver tests: a non-colocated store
  inside a git repository whose prunable worktree a stray prune would drop;
  the commands without `.git`; `Holds`, `Elsewhere` (readable, unreadable
  and gone), `ForgetName` (a name re-added meanwhile is kept); an add over
  a stale registration, and one that leaves another slot's stale
  registration alone. After the review: a second stack that declares the
  workspace (refused whether the first stack's checkout is readable,
  unreadable or out of sight), destroy forgetting only its own name beside
  another stack's, a linked slot after `vcs: jj`, destroy on a missing base
  (#139's verdict for a dirty slot, a bare error for a clean one), and the
  hint for git below 2.42. Thirty mutants are caught (the collision guard,
  `Holds`, `Elsewhere` failing open two ways, the shared-primary rule, the
  destroy's ownership check, the forget's change check, the narrowed
  prune, `EnsurePrimary`, the probe and its git hint, the outside-root
  check, the name suffix, a kept slot rolled back, the empty directory not
  restored, local jj slots skipped by the guard or by status, purge
  bypassing the jj driver, the colocation checks, the pre-add prune, the
  nested-repository guard and its slot skip, a linked slot's status).
- **Review record (`go-code-reviewer`, 2026-10-29).** Two blockers, both
  fixed: destroy forgot a recomputed name another stack could hold (now
  `destroyedJJSlot`), and `Elsewhere` read any jj failure as "no checkout
  elsewhere", so a recorded create forgot a live name it could not see (now
  fail closed, and the shared-primary rule). Should-fixes taken: the
  narrowed prune, nested slots skipped, the forget's change check, the git
  hint below 2.42, links at a jj slot's path, a warning for a skipped
  optional slot, the readable reverse test. Not taken: gating the nested
  walk on `--purge` (the pushed check is not gated either); recording the
  name, or always adding `--<slot>` (Q2 says names are computed and the
  suffix is only for two slots of one repository; ownership is now read
  from the checkout instead).
- **For phase 7.** `vcs.Status` already carries `ChangeID`, `BaseBehind`,
  `Conflicted` and `ConflictPaths` for jj slots; the DTOs drop them. A jj
  slot's name is computed (`jjWorkspaceNames`), so the topology's
  `workspaces: [{name, owned, path}]` can mark `owned` by comparing the
  primary's list with the names this stack's manifest computes. The
  topology's `Worktrees` count still counts `mode: worktree` only.
- **The lock file** `.<name>.angee.lock` is now also written beside a
  `kind: local` jj store, in the user's own directory.
- **Second review (2026-10-29), on #140.** Fixed: repair naming a re-added
  slot differently from create; inside-the-root checks that did not
  resolve symlinks; the remaining global prunes on jj removal; destroy
  refusing a jj slot whose directory is empty; destroy's cleanup on the
  request's context; `HeldChange` reading through a link; the nested walk
  missing links named `.git`/`.jj` and a `.git` at the top of a slot that
  is not colocated. Recorded, not built: the `vcs` change of phase 9
  below, and `ForgetName`'s window.
- **For phase 8.** Remove each refusal from `workspaceSourceTarget`'s verb
  list and `refuseJJSlots` as the verb lands; the aggregates must then
  select jj slots (`workspaceGitSlots` filters by recorded mode). The jj
  driver's `DirtyPaths` must include #139's `UntrackedPaths`. `ws push` on
  a jj slot needs Q7's answer for unmoded slots under `vcs: jj`, which are
  now jj slots.

### Phase 7. Status fields and topology

1. DTOs and schema: `WorkspaceSourceStatus`, `GitOpsLink` and `SourceState`
   gain `conflicted`, `conflict_paths`, `change_id`, `base_behind`;
   `GitOpsSummary` gains `conflicted`. The state vocabulary is unchanged
   (`dirty`, `ahead`, `behind`, `diverged`, `clean`); `conflicted` is an
   orthogonal flag, so existing clients keep working. Regenerate gqlgen and
   the SDL.
2. Topology: `SourceState` gains `workspaces: [{name, owned, path}]` for a
   colocated primary. `owned` and `path` come from this stack's manifest;
   for a workspace another stack owns, `path` is empty (jj's own record is
   not trusted, section 2).
3. CLI `ws git` and `ws status` print the conflict flag and paths.

Tests: JSON shape tests for the new fields; a conflicted slot reports paths;
`make check-generated`.

**As built (2026-10-29).**

- **Step 1, the fields.** `api.JJSlotState` holds `change_id`, `base_behind`,
  `conflicted`, `conflict_paths` and `untracked_paths`, and is embedded in
  `WorkspaceSourceStatus`, `GitOpsLink` and `SourceState`, so the JSON keys sit
  beside `state` and the three DTOs cannot drift apart. `conflicted` is always
  present (`false` for a git slot, `Boolean!` in GraphQL); the others are
  omitted when empty. `GitOpsSummary.Conflicted` counts beside the states, as
  `Unpushed` does (`countGitOpsState`). gqlgen binds the promoted fields with
  no resolver; a nullable `String` or `Int` bound to a Go value answers `""`
  or `0`, as `currentRef` already does.
- **One mapping.** `setSlotStatus(status, read, err)` (`workspaces.go`) is the
  only place a `vcs.Status` becomes API fields. `workspaceSourceStatus` calls
  it, the cache read in `sourceState` now goes through it too, and
  `sourceStateFromWorkspaceStatus` and `gitOpsLinkFromWorkspaceSource` copy
  `JJSlotState` whole. Phase 8's sync-base on jj slots reports `conflicted`
  through `workspaceSourceState` with no change here.
- **`untracked_paths` is carried** (decided here; the list predates #139,
  whose as-built note said phase 7 adds the paths). They are the one case
  where a jj slot holds work no commit has, publish will not push, and
  destroy would delete, so a client should be able to name them, and
  `unpushed_reason` stops at ten. Raw, as jj lists them: JSON escapes what
  does not print, and the CLI does not print them (the reason already
  quotes them).
- **Step 2, topology.** `SourceState.Workspaces` (`[{name, owned, path}]`,
  `api.SourceWorkspace`) is set by `sourceState`, so source status and list
  carry it too, not only the topology. It is listed for any source whose path
  has a `.jj` directory, a local store that is not colocated with git
  included, since its jj slots are workspaces all the same; a source without
  one, or an operator whose jj probe (`probeJJ`, kept for the process) found
  no jj, runs no jj and warns of nothing. `jjdriver.Driver.Workspaces` reads
  `jj workspace list` by name, as `Exists` does, with each workspace's
  working-copy change id (`ListedWorkspace`).
  - **Ownership is read from the checkout, not the name (review).** The plan
    said `owned` comes from comparing jj's list with the names this stack's
    manifest computes. That marks another stack's workspace as this stack's,
    with a path where nothing is: a stack of the same name sharing the cache
    computes the same names (`sharedCacheStacks`), which phase 6 already
    found for destroy. So `jjSlotCandidates` lists, per name, the paths of
    the slots this stack could have given it (the computed name, and both
    `<stack>--<workspace>` and `<stack>--<workspace>--<slot>`, as
    `recordedJJSlotTarget` and `destroyedJJSlot` allow), and a listed name is
    owned only when the checkout at one of them is that workspace, by
    `HeldChange`'s rule: `CheckoutAt` (one `jj log` per path, `HeldChange`'s
    first half, which it now calls) lists the name among the working copies
    there, with the change the listing gives the name. `path` is that one.
    Every other workspace, the primary's own (`default`), other stacks', and
    this stack's whose checkout is gone or cannot be read, is not owned and
    has no path. A path that cannot be read only leaves its slot not owned
    (debug log); only a cancelled context fails the listing. Both name forms
    make the drift of create's names harmless: an optional sibling skipped at
    create, and a second slot added to the manifest by hand and repaired in.
  - **Ambiguity owns nothing.** `HeldChange`'s known limit (a workspace that
    `jj edit`ed another's working-copy commit shares it) would make two names
    read as held at one path. A name is owned only when exactly one path
    holds it and that path holds no other name, so the case reads as not
    owned rather than as either guess.
  - Cost: one workspace list per jj source, and one `jj log` per distinct
    candidate path whose name is listed (about one per jj slot of this stack
    on that primary), on every source status, source list and topology
    read. A first version ran `Holds` per candidate, which listed the
    workspaces again each time (1 + 2N).
  - The primary's own workspace is listed: `WorkspaceRef` has no marker for
    the current workspace, and telling it apart needs another jj call per
    source.
  - A listing jj cannot read is a warning and is omitted, as the commits are;
    the source's git state is reported as before.
- **The worktree count stays right.** It counts links whose effective mode is
  `worktree`, so a jj slot, `mode: jj` or unmoded under `vcs: jj`, is not
  counted, although git registers a colocated jj slot as a worktree of its
  cache. A test pins it with both kinds of slot of one cache.
- **Step 3, CLI.** `workspace git` and `workspace status` append
  `conflicted: <paths>` after the state, naming up to ten
  (`conflictDetail`). `vcs.NamePaths` now names paths for it and for the jj
  driver's `dirtyReason` alike: up to a limit, then "and N more", quoting a
  name that does not print or that holds ", ". `sync-base`'s and `push`'s
  lines are unchanged, for phase 8 to extend with the same helper.
- **Tests.** The JSON shape of all four DTOs, `workspaces` included; a GraphQL
  query of every new field on a git slot; a real jj slot rebased into a
  conflict reporting `f.txt` through workspace status, workspace git, the
  topology's link and summary, clearing once resolved, with `base_behind`
  before the rebase and `untracked_paths` for a file over jj's size limit;
  the topology's `workspaces` with the cache's own, owned slots (one under
  the longer name), another stack's, this stack's whose checkout is gone, two
  primaries' workspaces of one name each with its own path, a local store
  not colocated with git, none and no jj run before any jj slot or without
  jj, and a warning over a broken `.jj`; a same-named stack that does not
  own the first's workspace; a hand-added second slot; the driver's
  `Workspaces` and `CheckoutAt` (a slot, the primary, a shared commit, a
  link, a plain or broken directory); a slot that cannot be read leaving the
  rest listed; two names one checkout holds owning neither; another
  repository's checkout at the slot's path under its name; `NamePaths`; the
  CLI's lines, cap and quoting. Twenty-three mutants caught: each of the
  five fields' mapping, the copies into `SourceState` and the link, the
  summary count, the cache read, the source's workspaces, the `.jj`,
  directory and probe gates, ownership by name without the checkout, the
  change id check, the ambiguity rule, an unreadable slot failing the
  listing, either name form missing, both quotings of `NamePaths`, the CLI's
  cap and its line. Survive, as they should: dropping `jjSlotCandidates`'
  primary filter, which only spares jj calls, and the working-copy name
  check, which the change id implies within one repository (kept, as
  `HeldChange` keeps it).
- **Review record (`go-code-reviewer`, 2026-10-29).** One blocker, fixed:
  `owned` by name alone (above). Should-fixes taken: no jj run or warning
  for an operator without jj (a container that mounts a host cache); the
  docs no longer say push, sync-base and the per-slot verbs carry the fields
  today (they refuse jj slots until phase 8); tests for the primary filter
  (two primaries), a non-colocated store and `untracked_paths`. Nits taken:
  `conflicted` documented as false for a slot whose base does not resolve;
  the empty GraphQL `path` documented; the Go API change (DTOs no longer
  comparable) in the changelog; `NamePaths` shared, quoting ", " too; a test
  assertion that panicked. Not taken: giving a slot's `SourceState` the
  GraphQL id `<source>:<slot>`. Today a slot's entry and the cache share the
  source's name as id, so a client that normalizes by id merges them; that
  is so for `state` and `dirty` already, and the id of an existing type is
  the owner's call. Rewriting the `%+v` expectations in
  `push_publish_git_test.go` as struct comparisons is left as it is.
- **Second review (`go-code-reviewer`, 2026-10-29), of the fixes.** The
  blocker is resolved. Taken: one unreadable slot no longer drops the whole
  listing; the ownership check runs one `jj log` per path against the
  listing in hand instead of `Holds` per candidate; two names one checkout
  holds own neither; the docs say a slot that cannot be read is not owned,
  and that the probe is kept for the process (an operator started before jj
  was installed lists nothing until it restarts); the candidates' comment no
  longer says another repository's change ids never match (an imported git
  commit has the same one in two clones). Each CLI `source status` or `list`
  on a jj source now runs the probe's three commands once.
- **For phase 8.** Sync-base, rebase and merge on a jj slot should return
  each slot's state through `workspaceSourceState` (or `setSlotStatus`), so
  a conflict is `conflicted: true` with its paths; the CLI's `sync-base`
  line can print it with `conflictDetail`.

### Phase 8. Sync-base, publish, push and merge on jj slots

Replaces the phase 6 refusals verb by verb. May land as two PRs (sync-base
and rebase; then publish, push and merge).

0. Identity fallback, first. The runner sets no identity, so an operator
   whose user has no jj `user.name`/`user.email` records commits with an
   empty author and committer (every snapshot rewrites `@`), and `jj git
   push` refuses those. The git ops fall back to angee's placeholder per role
   only when git has none configured; give jj the same rule, probing `jj -R
   . --ignore-working-copy config get user.name` (and `user.email`) and
   setting `JJ_USER`/`JJ_EMAIL` only when empty, since those override user
   and repo config.

1. Rebase scope (D12). Before any rebase on a jj slot, read `(B.."N"@)::`.
   If it contains another workspace's working copy, or a commit that is the
   checked-out branch of a git worktree of the same primary, refuse and name
   what is in the way.
2. `sync-base`: the CLI sends an empty method unless `--rebase` or a new
   explicit `--merge` is given (D11). For a jj slot: snapshot,
   `jj git fetch` on the primary, the scope check, `jj rebase -b @ --onto
   <B>` in the slot, then status. No dirty refusal: the working-copy commit
   rides along. A conflict is a successful result with `conflicted: true`.
   A non-conflict failure (immutable commit, scope refusal) is an error for
   that slot. The loop no longer stops at the first conflicted slot.
3. Publish, for a jj slot: snapshot; refuse a non-empty `@` ("commit the
   working copy first"), mirroring the git dirty refusal; require a branch
   `X` from the slot or the request; compute the push range (`X@R.."N"@-`
   when `X@R` exists, else `B.."N"@-`); an empty range returns OK without
   pushing; refuse when the range holds a conflicted commit or a non-empty
   commit with no description, naming them; `jj bookmark set X -r "N"@-`;
   `jj git push --remote R --bookmark X`. Any other refusal comes back as
   the typed push-refused error with jj's stderr, URL-redacted (D6).
4. `ws push` and `ws source push` on a jj slot are publish with the default
   remote. `ws source pull` is refused on a jj slot with an error naming
   `sync-base`. Fetch and diff are implemented.
5. Merge: refuse a non-empty `@`; `jj new "N"@- <ref> -m "Merge <ref>"`,
   then `jj new`, so the merge is `@-` and publish includes it. Rebase: the
   scope check, then `jj rebase -b @ --onto <ref>`.
6. `MergeAbort`, `RebaseAbort`, `RebaseContinue` on a jj slot return an
   `InvalidInputError` saying a jj slot never has an operation in progress.
   They wrap `vcs.ErrUnsupported`, which the service reports once.
7. Carried from 4d (design notes 3 and 4), since each changes messages:
   the preconditions are answered per driver ("may this verb run" instead
   of the service judging `BranchMismatch` and `DirtyPaths`, so a jj slot's
   working-copy commit can ride along), and failure wording is VCS-neutral
   (the branch-mismatch hint stops naming `git -C … switch`, and the
   driver supplies the tool's own line where failures quote " git: ").
   Steps 3 and 4 live in the jj driver's `PlanPublish`, `PreparePublish`
   (`jj bookmark set`), `Publish`, `PlanPush` and `Push`.

Tests: base moves, sync-base rebases two slots, one conflicts and reports
paths, the workspace stays usable; a slot stacked on another slot's commits
is refused; publish pushes only the slot with commits; conflicted and
undescribed refusals are structured; first publish creates the remote
branch; publish after a sync-base rebase updates the remote branch; merge
followed by publish pushes the merge; the three state-machine verbs are
rejected; explicit `--merge` with a jj slot present is refused.

To state in the changelog: rebase-based sync rewrites commits that were
already published, so the next publish force-updates the remote branch (jj
does this with a lease). That is the proposal's intent, and different from
today's default merge.

Settled by Q7 (owner, 2026-10-29): sync-base, and `ws push` in the second
half, walk only the slots whose manifest names a mode, so a slot with no mode
under `vcs: jj` (`.work`) is skipped by them and reached only by the per-slot
verbs.

**As built: sync-base and rebase (2026-10-29).** The first of the two PRs:
steps 0, 1, 2, the rebase half of 5, 6, and the part of 7 sync-base needs.

- **Step 0, identity.** `jjdriver/identity.go`. A runner made by `New`
  holds an `identity`, resolved once per runner (the service makes one
  driver per slot operation) on the first run that may record commits:
  `snapshot` (so every status, fetch and rebase), `inPrimary` (`jj workspace
  add`) and `recording` (`jj git init` in the system temporary directory).
  Reads with `--ignore-working-copy` record nothing and get nothing. The
  identity is the repository's whose commits the run records: `jj git init`
  runs in the temporary directory but resolves it for the primary (found in
  review), so a repository-level or `includeIf` git identity counts. The
  operator's own `JJ_USER` and `JJ_EMAIL` need no probe; otherwise one `jj -R
  . --ignore-working-copy config list --include-defaults -T <name, json
  value> user` gives both values (in the temporary directory, for the user's
  config alone, where the directory holds no `.jj` yet; jj's defaults are
  `""`), and for each that is empty, git's author identity:
  `git.Client.Identity`, the git ops' rule moved from `gitdriver` (git's
  configured identity, angee's placeholder only when git has none, nothing
  when git refuses otherwise). git runs in the slot when it is colocated, else
  in the system temporary directory, never in a store without a `.git`.
  `runner.exec` now returns standard error too, for a mutation's message.
- **Step 1, the rebase scope (D12).** `jjdriver/rebase.go` `checkScope`:
  the primary's git worktrees, then a snapshot, then at that operation
  `(B..@)::` with the commits template (the second review, below, changed
  how the worktrees are found and what the rebase then acts on).
  Refused, with a `*vcs.SharedCommitsError`, when it holds another jj
  workspace's working-copy commit, or a commit at which a git worktree of the
  primary (`git.Client.Worktrees`, `worktree list --porcelain -z`, the main
  worktree included, the slot's own skipped) has a branch checked out; and,
  with a `*vcs.ImmutableCommitsError` (cause `immutable_commits`), when it
  holds an `immutable()` commit, which `jj rebase` would refuse only once
  the sync is under way (found in review). jj does not say which
  workspace runs a command, so the slot's own working-copy commit is `@`, and
  when another workspace shares it every name holding it is listed. It is
  `vcs.Driver.CheckRebase` (the git driver answers nil: git rebases only the
  checkout's own branch, which no other worktree can have checked out), and
  the jj rebases check it again right before running. The service maps it to
  `PRECONDITION_FAILED`, cause `shared_commits`, naming the holders, with the
  worktrees in `paths`. Known limit: check, then rebase; an agent stacking a
  workspace in between is rewritten, as jj itself would.
- **Step 2, sync-base.**
  - A machine that cannot run jj slots (`requireJJSlots`) refuses sync-base
    and rebase of one as `jj_unavailable` before anything is fetched; a jj
    that vanished later is `jj_unavailable` from `gitFailure` too.
  - D11: the CLI sends no method unless `--merge` or `--rebase`.
    `vcs.Driver.SyncMethod(method)` resolves each slot's method before any
    slot is read (git: merge by default; jj: rebase, and a merge wraps
    `vcs.ErrUnsupported`), so an explicit merge with a jj slot is refused
    (`INVALID_INPUT`, naming the slot) before anything is fetched, and the
    failure wording says what each slot does.
  - Selection (Q7): `workspaceGitSlots` selects by recorded mode, now
    `worktree` and `jj`, and a jj slot of a `kind: local` store too.
  - The aggregate keeps its order and extends it: every slot's method, then
    readiness and base ref, then fetch and base, then **the scope of every
    rebase**, then the syncs. So a scope refusal, like every refusal, leaves
    no slot changed (a test pins that the free slot sorted first is not
    rebased).
  - jj `Fetch` is `jj git fetch --all-remotes` **in the slot**, under
    `ANGEE_GIT_TIMEOUT`, not on the primary: the slot shares the primary's
    store, so it is the primary's fetch, and it is the step 1 snapshot. It
    takes no primary lock, as a git worktree slot's fetch takes none (D9:
    slot verbs). `--all-remotes` mirrors git's `fetch --all`. A `kind: local`
    store is never fetched (angee never changes a user's store, as at
    create): its slot is rebased onto its base as the store has it.
  - `ResolveBase` names B as Status picks it (`main@origin`, else the local
    `main`), at the latest operation. `destination` maps that name back to
    the exact bookmark, wrapped in `exactly(…, 1)`, so a tag of that name never
    wins and a conflicted bookmark is an error, never a merge of its targets;
    any other ref is `refRevision`.
  - `jj rebase -b @ --onto <B>` runs in the slot through `snapshot`; its
    standard error is the result's message. Conflicts are read after it at
    the latest operation (D5): the conflicted commits of `B..@` and their
    files. When that read fails after jj succeeded, the result stays `OK`
    with the error: sync-base records it on the slot's state and goes on,
    and the slot's rebase says it was the read that failed. The result is
    `OK` and `Conflicted`; a git conflict stays
    `Conflicted` without `OK`, which is how the service tells the two apart,
    without asking the slot's mode. A recorded conflict reaches the slot's
    state through its status (phase 7's `api.JJSlotState`, from
    `setSlotStatus`), and the loop goes on. (A stopgap commit had added
    `conflicted` and `conflict_paths` to `SourceState` alone, set from the
    result; merging #142 replaced it with phase 7's struct.) A git
    conflict still ends the sync with `merge_conflict`. A non-conflict
    failure ends the sync as that slot's error, quoting jj (`vcs.ToolError`).
  - The state after a sync is the slot's status, as for git.
- **Step 5, rebase.** `jjdriver.Rebase(ref)`: the scope check, then the same
  rebase. `ref` is jj's spelling: `refRevision` quotes `name@remote` as
  `"name"@"remote"` (split at the last `@`) and anything else as one name,
  so `origin/main` names nothing, and it must be exactly one commit, so a
  conflicted bookmark is an error, never a merge of its targets. Merge stays
  refused.
- **Step 6.** The three verbs reach the driver, whose
  `noOperationInProgress` wraps `vcs.ErrUnsupported`; `classifyOperationError`
  reports it once as `INVALID_INPUT` on the slot.
- **Step 7, the part sync-base needs.** `vcs.Driver.Ready(ctx, s, verb)`
  answers "may this verb run", for `vcs.VerbSyncBase` only so far: git checks
  the branch (when the slot must be on one) and the uncommitted paths and
  returns `*vcs.BranchMismatchError{Reason, Fix}` (Fix is git's own `git -C …
  switch …`) or `*vcs.UncommittedError{Paths}`; jj lets its working-copy
  commit ride along, and refuses only untracked paths (second review,
  below). `vcs.VerbRebase` is the slot's own rebase (git: no preconditions). `requireSlotReady` words them as before,
  so git's messages are unchanged. Failures quote the tool: a driver error
  that is a `vcs.ToolError` (`jjdriver.CommandError`: `Tool`, `Line`, the
  first `Error:` line, `Output`) fills `OperationError.Tool` and `ToolLine`
  (was `GitLine`), and the message ends ` jj: <line>`. A failure with
  neither tool's words now ends with the error's own text (it said only
  "failed." before, losing a decode error, say).
- **Refusals lifted.** `workspaceSourceTarget` lets `jjSlotVerbs` (rebase and
  the three state-machine verbs) through, and now hands the verb the slot's
  effective mode, so a slot with no mode under `vcs: jj` gets the jj driver.
  `refuseJJSlots` is gone from sync-base, and for `workspace push` counts only
  slots whose manifest names mode `jj` (Q7). Fetch, pull, push, publish,
  merge and diff stay refused.
- **Tests.** Real jj (`ANGEE_REQUIRE_JJ=1`): the driver's fetch, base, sync
  with an unrecorded edit riding along, a recorded conflict that resolves,
  rebase onto a named ref and a refused revset, the scope (a stacked
  workspace, a shared working-copy commit, a git worktree's branch, the
  slot's own git branch and checkouts on the base let through), immutable
  commits refused, a bad ref as jj's error, the base's fallback and
  `exactly`; the identity (git's, the placeholder, jj's name with git's
  email). The service: two jj slots and a worktree slot synced by default
  (one conflict, reported, the rest synced, the workspace usable, a second
  sync clean; `--merge` refused first, nothing fetched); a stacked slot
  refused by sync-base and rebase from both sides, nothing rebased, the
  free slot included; a tagged commit refused before the slot sorted first
  is rebased; a slot with no mode skipped under `vcs: jj` and reached by
  rebase; a `kind: local` store rebased without a fetch; the state-machine
  verbs; the D11 refusal; no jj (`jj_unavailable`); the operator contract
  for both `INVALID_INPUT` refusals over REST and GraphQL. Fake tools: the
  identity probe, which runs get it, a new primary's resolved for the
  primary. Fake driver: readiness worded, dispatch order, a sync made but
  unread. git: `Ready`, `SyncMethod`, `Worktrees`. Mutants caught: no
  identity fallback, no pre-check of every slot's scope, a local store
  fetched, conflicts ending the loop, slots with no mode selected.
- **Second review (an independent `go-code-reviewer` pass on #143,
  2026-10-29).** No blockers. Each finding was checked before it was built:
  - *The git worktree check skipped a slot with no `.git`* (confirmed): the
    worktrees are the primary's, so `checkScope` now lists them from the
    repository `jj --ignore-working-copy git root` names, whatever the slot
    has, and fails closed when jj names none. A test removes the slot's
    `.git`.
  - *Check, then act* (confirmed): the rebase now runs `jj rebase -b
    'exactly(change_id(<C>), 1)' --onto 'exactly(commit_id(<B>), 1)'` with
    the working-copy change and the base commit the check read (`pinned`),
    so a base that moves after the check is not followed, and a working
    copy moved meanwhile, or a divergent change, fails. The worktrees are
    listed before the snapshot, and a branch is matched by jj's bookmark of
    its name as well as by its commit, so one a worktree commits to between
    the two is still found. Tests: the base moving, and the working copy
    moving, after the check.
  - *A failed identity read could replace a real identity*: half right. A
    cancelled context could not produce the placeholder, since
    `git.Client.Identity` also refuses on a cancelled context; but a `jj
    config list` that failed any other way was read as empty, so git's
    identity (or the placeholder) could override one jj has. A failed read
    now adds nothing, and a resolution its context cut short is not kept
    (`sync.Once` became a mutex and a flag). Tested.
  - *Extra processes per status* (as asked, left): each operation's first
    recording run costs one `jj config list`, and one `git var` when jj has
    no identity. Status is read on demand (Q6), and a process-wide cache
    would keep a long-running operator from seeing an identity change.
  - *A rebase could revert the base's file* (confirmed, and also for an
    ignored file): jj keeps a file the working copy holds where the new base
    adds one, says "1 of those updates were skipped", and the next snapshot
    records it as undoing the base's. `Ready` now refuses sync-base and the
    slot's rebase while `jj status` lists untracked paths
    (`*vcs.UntrackedPathsError`, cause `untracked_paths`, naming up to ten),
    before anything is fetched. An ignored file cannot be seen ahead, so jj's
    warnings (each `Warning:` line with its hints, but for the one that says
    conflicts were left) come back as a `*vcs.Warning` beside the OK result:
    on the slot's state (`error`) in sync-base, in the result's message in
    the slot's rebase.
  - *The git refusal order* is in the changelog now.
  - *A mixed workspace hid earlier jj rebases* (confirmed): a slot's sync
    that fails after others synced names them, in the message and the
    detail (`afterSynced`).
  - The comment on sync-base no longer says a refusal leaves no slot
    changed: the checks before the syncs do, and a later failure names
    what synced.
- **Review record (`go-code-reviewer`, 2026-10-29).** No blockers. Fixed:
  an immutable commit failing the sync partway (now pre-checked); a rebase
  jj made reported as failed when the conflict read after it failed (and a
  shadowed `err` that dropped it); the identity resolved for the temporary
  directory of `jj git init`; a missing jj reported as a fetch failure; a
  caller's ref not forced to one commit; the slot's own git branch read as
  another checkout's; the forget command in the `shared_commits` hint; two
  probes where one `config list` does; failures that lost their error text;
  stderr not redacted in the result's message; the Q7 wording of `--merge`;
  a stale comment. Tests added for each. Not taken: keeping every slot's
  branch check before any slot's clean check (git's refusal order across
  slots changed: with slot `a` dirty and `b` off its branch, `a` is now
  named; nothing is touched either way), and `GIT_CEILING_DIRECTORIES` for
  the identity probe in the temporary directory.
- **For the second half (publish, push, merge).**
  - `Ready` takes the other verbs: jj `VerbPublish` and `VerbMerge` refuse a
    non-empty `@` (`*vcs.UncommittedError`); git's pull and push move from
    `ensureWorkspaceGitSourceOnExpectedBranch` and `requireCleanSlot` to it,
    with destroy's branch check, so the service stops naming
    `git -C … switch` there. `jjdriver.DirtyPaths` must include
    `UntrackedPaths`, if it is kept at all.
  - jj `PushURL` still returns "" and `PlanPush`, `Push`, `PlanPublish`,
    `PreparePublish`, `Publish`, `Pull` and `Merge` are not built; the
    identity is in place for `jj git push`.
  - `refRevision` is the spelling for a ref a caller names, Diff's included.
  - `jjSlotVerbs` grows with each verb, and `workspace push` drops
    `refuseJJSlots` once it selects jj slots by recorded mode, as sync-base
    does.
  - Bookmarks on the commits a rebase rewrites that no worktree has checked
    out (another slot's published bookmark, say) move with them.
    *Answered (owner, 2026-10-29): allowed, as jj does.* The rebase moves
    them, and a pushed one force-updates, with a lease, on its next publish;
    D12 says so, and so do the docs and the changelog.

**As built: publish, push and merge (2026-10-29).** The second of the two
PRs: steps 3, 4, the merge half of 5, and the rest of 7. A jj slot now does
everything a git slot does but migrate, and is never pulled.

- **Step 3, publish** (`jjdriver/publish.go`). `PlanPublish` changes nothing:
  the branch X is the intent's (the service fills in the slot's manifest
  branch), else `vcs.ErrNoBranch`; R is the intent's remote, else
  `pushRemote`, git's push remote for X in the slot's colocated config
  (`git.Client.PushRemoteFor`, Q8), which must exist (`remoteURL`: git's
  config, the slot's when colocated, else that of the repository `jj git
  root` names, so the name matches exactly); a slot that is not colocated has
  no config of its own to name a push remote, so it needs a named one
  (`*vcs.NoPushRemoteError`). X must be the slot's to set
  (`*vcs.BranchTakenError`, `INVALID_INPUT` on `branch`): not checked out by
  another git worktree of the primary (`checkedOut`, through #143's
  `primaryWorktrees`, the primary's own `main` included), since the bookmark
  moves the branch and leaves that worktree detached (D12); and, when R has
  X, tracked there (`tracked`: jj sets `tracking_target` only then), since
  an untracked X@R is someone else's and jj refuses to push over it, after
  `jj bookmark set` had already made the local bookmark (found in review).
  `pendingPush` snapshots, then reads at that operation `X@R..@-` when the
  remote bookmark exists, else `B..@-` (B as Status resolves it). None is a
  `*vcs.NothingToPublishError` (`ok`, "nothing to publish: <why>"), whatever
  `Explicit` says. `unpushable` then reads the
  range less `::remote_bookmarks(remote=R)` (what R already holds, such as
  the base's own commits after a rebase, which jj does not check either) and
  refuses conflicted or undescribed commits as a `*vcs.UnpushableCommitsError`
  (D6). The plan pins `Commit`, the full id of `@-`, and `Create` says
  whether bookmark X is new. `PreparePublish` re-reads `@-` and refuses one
  that moved since the plan (`vcs.ErrCheckoutMoved`, a `CONFLICT` to retry),
  then `jj bookmark set -r
  'commit_id("<id>")' -- '"X"'` (jj parses a bookmark name as a revset
  symbol, so it is quoted, and `--` keeps a leading `-` from being a flag).
  `Publish` is `jj git push --remote=exact:"R" --bookmark=exact:"X"` (both
  are glob patterns to jj 0.45; a bare `--remote='*'` pushed to every remote,
  found in review) through
  `networkOutputs` (ANGEE_GIT_TIMEOUT), its standard error, redacted, the
  result's message. jj tracks a bookmark it pushed new, and moves one a
  rebase rewrote sideways, force-pushing with its lease (probed: "move
  sideways", and "unexpectedly moved on the remote … stale info" when the
  remote branch moved). Any other refusal is jj's `*CommandError`, which the
  service reports through `gitFailure` as `GIT_FAILED`, cause `push_failed`,
  ` jj: <line>`. `PushURL` is `pushRemote`'s URL.
- **Step 4, the other verbs.** `PlanPush(ref, branch)` is `PlanPublish` under
  ref, else branch, to the default remote, returned as `PushPlan.Publish`
  (`Track` stays empty);
  nothing to send is `Skip` with `Reason`; no branch with commits beyond the
  base is `vcs.ErrDetachedWithWork` (the service's `detached_head` refusal,
  worded for jj), none is a skip. `Push` is `PreparePublish` and `Publish` of
  that plan. The service's `planWorkspaceGitPush` holds a plan that
  publishes to `publishRulesRefusal` (canonical remote, `branches`), the
  check publish makes, factored out of it, and words a push plan's failures
  (`pushPlanRefusal`: detached, no or unknown push remote, a missing base,
  unpushable commits, else the tool's failure). `Pull` and `Ready(pull)`
  wrap `vcs.ErrUnsupported` ("never pulled"), and the service words it as
  `INVALID_INPUT`, cause `jj_slot_unsupported` (`jjSlotNotPulled`), naming
  `workspace sync-base` for a slot whose manifest names its mode and the
  slot's own fetch and rebase onto its base for any (Q7). Fetch needed no
  driver change; a jj slot of a `kind: local` store is refused it
  (`INVALID_INPUT`), as sync-base never fetches one. `Diff` takes a caller's
  ref through `refRevision(…).single()`. A jj command's failure that reaches
  `classifyOperationError` without a step (a diff's, a `Ready` read's) is
  `GIT_FAILED` quoting jj, as a git command's is; it was `INTERNAL` (review).
- **Step 5, merge** (`jjdriver/merge.go`). Ready first (below); then Merge
  snapshots again, refuses changes made since (in Ready's words, through
  `readinessRefusal` after the run) and merges onto the commit it found
  under the working copy, by id. A ref already in `::@-` makes no merge ("Nothing to merge", as git's `--no-ff` records
  nothing for an ancestor; probed: `jj new @- <ancestor>` makes an empty
  merge commit). Otherwise `jj new 'exactly(@-, 1)' 'exactly(<ref>, 1)' -m
  "Merge <ref>"`, then `jj new`; the merge commit's conflicts are read after
  (D5), and jj's warnings come back as a `*vcs.Warning` beside the OK result,
  as #143's rebase does. A failure of the second `jj new` is a warning too,
  saying a publish leaves the merge out until `jj new` runs.
- **Step 7, readiness.** `vcs.Driver.Ready` answers every verb with
  preconditions: `VerbPull`, `VerbPush`, `VerbPublish`, `VerbMerge`,
  `VerbDestroy` and `VerbRepair` join sync-base and rebase. git: sync-base,
  pull and push need the branch (when the slot must be on one) and a clean
  tree; destroy and repair the branch; rebase, publish and merge nothing. jj:
  sync-base and rebase refuse untracked paths (#143); publish, push and merge
  refuse changes in the working-copy commit (`*vcs.UncommittedError` with
  `Fix`, jj's `jj -R <slot> commit -m <message>`, which the hint quotes),
  else untracked paths (`*vcs.UntrackedPathsError`, worded "<verb> leaves out,
  since no commit holds them"), else a working-copy commit with parents other
  than one (`*vcs.NotReadyError{Reason, Fix}`, a new generic precondition,
  `PRECONDITION_FAILED` naming the fix); destroy and repair nothing, running
  nothing.
  The service's `ensureWorkspaceGitSourceOnExpectedBranch(es)` are gone:
  pull, push, `workspace push`, destroy and repair ask `Ready`
  (`requireSlotReady`, `requireWorkspaceSlotReady`). git's codes, causes,
  hints and messages are unchanged: `TestGitSlotVerbsKeepTheirRefusals`
  passes on main's code as on this one, and the characterization tests that
  pin git's command lines pass unchanged. `requireCleanSlot` stays for the
  source cache's push. `jjdriver.DirtyPaths` is the working-copy commit's
  changed files (`changedFilesTemplate`, recorded in
  `testdata/changed.ndjson`) then `UntrackedPaths`. `BranchMismatch` left
  `vcs.Driver`: nothing calls it through the interface now (the git driver
  keeps it for `Ready` and Status).
- **Refusals lifted.** `jjSlotVerbs` lists every slot verb, so
  `jjSlotNotYet` refuses only a verb added later and not listed;
  `refuseJJSlots` is gone, and `workspace push` selects `worktree`, `clone`
  and `jj` slots by recorded mode (Q7), checking jj (`requireJJSlots`) when
  it has one, then each slot's readiness and plan before it pushes any. The
  slot verbs that run jj check it first (`requireJJSlot`).
- **Bookmarks a rebase moved** (owner, 2026-10-29): allowed, as jj does;
  nothing refuses them, and their next publish force-updates with the lease.
- **Departures.** Undescribed commits are refused empty or not: jj refuses
  an empty undescribed commit too (probed), where step 3 said "non-empty".
  Untracked paths refuse publish, push and merge as well as a non-empty
  `@`, as asked for this PR. `Explicit` and `Unmanaged` change
  nothing on a jj slot. `PushPlan` gained `Reason` and `Publish`, and
  `PublishPlan` `Commit`. A failure to plan a push or publish, or to create
  or set its branch, now names the step (`planning the publish of …
  failed. git: …`) instead of the git command.
- **Known limit.** Check, then act: an agent's commit between
  `PlanPublish` and `PreparePublish` is refused, but one between the
  bookmark and the push rides along unchecked, and jj's own check refuses
  what it must; after the push, X@R is read back, and one that is not the
  plan's commit is a warning on the OK result.
- **Review record (`go-code-reviewer`, 2026-10-29).** No blockers; six
  should-fixes, all taken: an untracked remote branch failing the push after
  the bookmark was set; a git worktree's checked-out branch moved by the
  bookmark; jj failures outside a step classified `INTERNAL`; fetch of a
  local store's jj slot an untyped error; `--remote` passed as a glob; and
  the URL parsed from `jj git remote list` (a push URL's credentials kept,
  quoted names never matched), now read from git's config of `jj git root`.
  Nits taken: merge pinning `@-` and re-checking the working copy; a failed
  second `jj new` as a warning; the moved slot a `CONFLICT`; the two-parent
  working copy a precondition in `Ready`; `BranchMismatch` off the interface
  and `Track` unset for jj. Not taken: removing `jjdriver.DirtyPaths`, which
  the interface still requires and this PR's brief asks to include
  untracked paths.
- **Second review (an independent `go-code-reviewer` pass on #144,
  2026-10-29).** One blocker, confirmed with jj 0.45.1 and fixed:
  - *Publish dropped a collaborator's commits from the remote.* The slot
    publishes C1, sync-base rebases it to C1', a collaborator pushes C2 on
    C1, and a fetch leaves bookmark X conflicted between C1' and C2; `jj
    bookmark set` resolves it as a forward move, and the lease passes since
    C2 was fetched, so C2 was lost. `ownBranch` now refuses, as a
    `*vcs.BranchMovedError` (`PRECONDITION_FAILED`, cause `branch_moved`,
    the hint naming `workspace source merge … X@R`), any commit of
    `@-..X@R`, or of `@-..X`, whose change none of `B..@-` has: an old
    version of the slot's own commit shares its change and passes, a
    collaborator's is a change of its own. That covers the conflicted
    bookmark (its targets are X's) and a remote that moved ahead with no
    local conflict. A test runs the exact order, then the merge remedy.
  - Taken: a caller-named branch that exists and is not the slot's (not its
    manifest branch, nor only on `B..@-` under no other workspace's working
    copy) is refused, wrapping `vcs.ErrBranchExists`, so `--branch develop`
    no longer moves `develop` (`INVALID_INPUT` on `branch`); `workspace
    push` leaves a `kind: local` store's jj slot to its own publish (the
    owner's call, as main left non-`kind: git` slots out of the aggregate,
    and angee writes nothing to a user's store unasked; sync-base still
    rebases one, `workspaceGitSlots`' `localStores`); a failure to plan or
    prepare a push or publish is `gitFailure` only for a tool's failure
    (`toolFailure`), else the annotation classifies it, so a deadline stays
    `TIMEOUT` and anything else `INTERNAL`, as on main; the two-parent fix
    is `jj -R <slot> new`; a merge over a described empty working copy is
    refused (`*vcs.NotReadyError`, fix `jj new`), not handled, since jj
    keeps a described commit and the merge would leave it behind for the
    destroy guard to count; X@R is read back after the push, a mismatch a
    warning. Error-contract rows over REST and GraphQL for
    `conflicted_commits`, `undescribed_commits`, `branch_moved`, branch
    exists and the moved slot's `CONFLICT` (a jj on PATH that commits in the
    slot before publish's second snapshot), and repair in
    `TestGitSlotVerbsKeepTheirRefusals`, which still passes on main's code.
  - The owner's Q8 answer is built with it (above): `PublishPlan.RecordRemote`,
    `git.Client.SetBranchPushRemote`.
  - *Third pass (2026-10-29), three gaps, all confirmed with probes and
    fixed.* A jj collaborator who squashes a fix into the published commit
    keeps its change, so C1x passed the change rule while bookmark X was
    conflicted between C1' and C1x, and the push dropped C1x: the change
    rule now applies to X@R alone (which may hold the old versions a rebase
    leaves), and bookmark X must be on the slot's history (`X ~ ::@-`
    empty), its stray targets reported as X@R's when they are the remote's,
    so the merge hint names one commit; the merge remedy passes, both sides
    being ancestors of the merge, and a test runs the order then the merge,
    resolved, published with C1x in it. A tracked X@R with no bookmark X
    escaped the existing-branch check: it now runs for either, and `B..X@R`
    must be non-empty and only the slot's changes. The recorded push remote
    is the cache's to replace and never a local store's (Q8 above). Six more
    mutants caught. A bookmark on an old version of the slot's own commit is
    now refused by the plan, where jj refused it at `bookmark set` before.
  - Eighteen more mutants caught: `ownBranch` skipped, its change match, its
    base rule, the existing-branch check and its working-copy rule, the
    record and the default-remote test, the read-back, the described empty
    working copy in `Ready` and in Merge, `localStores` both ways,
    `toolFailure` for publish and push, the `branch_moved` and jj
    branch-exists wordings, and a warned publish and push taken as done.
- **Tests.** Real jj: the first publish creating the branch, then nothing to
  publish; no commits and no branch; conflicted and undescribed commits
  refused with nothing set or pushed; publish after a sync-base rebase moving
  the branch sideways, over an undescribed base commit the remote holds; the
  lease refusing a moved remote branch; the remote by push default, named,
  unknown both ways, none; a slot that moved between plan and prepare;
  push as publish under ref or branch, then skipped; `Ready` and
  `DirtyPaths` with untracked and changed files, and a working copy with two
  parents; merge (shape, nothing to merge, git spelling refused), a
  conflicting merge, merge then publish, a change made after `Ready`; Diff
  onto `main@origin` against `main`; a branch not the slot's (untracked on
  the remote, a worktree's, the primary's own); a bookmark moved sideways
  left with jj's words; publish from a store without git. The service: `workspace push`
  pushing only the slot with commits and skipping a slot with no mode under
  `vcs: jj`, which its own push reaches; structured refusals from push,
  publish and `workspace push`, nothing pushed; publish, sync-base, publish
  force-updating; merge then publish; publish, push, `workspace push` and
  merge refusing changes and untracked files; diff in jj's spelling; pull
  refused naming sync-base, both hints; an unlisted verb refused; every
  jj verb's `jj_unavailable`; git's refusals pinned; a push that publishes
  held to the remote's `branches`, a moved slot and a not-ready one (fake
  driver); `workspace push` over a git worktree and a jj slot, and publish
  refused under the worktree's branch; jj's diff error as `GIT_FAILED`; a
  local store's fetch refused; the operator contract for the pull refusal
  over REST and GraphQL. Fake jj: the remote and bookmark passed as exact
  patterns, and a refusal's credentials redacted. Twenty-two mutants caught:
  the unpushable pre-check, the tracking and worktree checks, the moved
  check, untracked paths and two parents in `Ready`, untracked paths in
  `DirtyPaths`, merge's second `jj new` and its re-check, Diff's spelling,
  jj slots left out of `workspace push`, the publish rules on a push, the
  tool error's classification, the local store's fetch, destroy's and repair's
  branch check, the moved slot's wording, the pull hint, and the remote's
  quoting.

### Phase 9. `ws source migrate`

New service method `WorkspaceSourceMigrate(workspace, slot, to)` with CLI
(`angee workspace source migrate <ws> <slot> [--to jj|worktree]`), REST
(`POST /workspaces/{name}/sources/{slot}/migrate`), GraphQL mutation, remote
client method, and a row in `docs/reference/surfaces.md`.

`git worktree remove` deletes ignored files without asking (probe), and a
real slot always has some (`node_modules`, `.venv`, local env files). So the
verb preserves them by default:

1. Refuse a slot with uncommitted changes to tracked files.
2. Record the slot's untracked and ignored top-level entries
   (`git ls-files --others --directory`).
3. Rename the slot directory aside, within the same parent, through the
   guarded path, and run `git worktree repair <aside>` so its registration
   stays valid.
4. `EnsurePrimary`, `jj git import`, then create the jj slot at the original
   path on the slot's branch (as phase 6 does).
5. Move the recorded entries from the set-aside directory into the new slot,
   then `git worktree remove` the set-aside directory.
6. Write `mode: jj` through the parent stack transaction.

This sequence was probed end to end: ignored files and a local `.env`
survive, jj's path record is correct, and `@-` carries the branch. Untracked
files that are not ignored become part of the jj working-copy commit, which
is jj's normal behaviour; the verb says so in its output.

A failure before step 5's removal undoes cleanly: forget and remove the new
slot, rename the set-aside directory back, `git worktree repair`. The
reverse direction (`--to worktree`) is the mirror image: snapshot, refuse a
non-empty `@`, ensure the bookmark is on `@-`, set aside, forget, re-cut a
git worktree on that branch, move entries back, write `mode: worktree`.

Tests: both directions; ignored and untracked files survive; a forced
failure at each step leaves the original slot intact and usable.

**Known gap from phase 6 (review of #140).** The manifest keeps a slot's
mode as written, and `vcs` decides for every slot without one. Changing a
stack's `vcs` from jj to git therefore sends its existing jj slots that name
no mode down the git path: status and the destroy guard read them as clones
through the git driver, and destroy removes them without forgetting their
jj workspaces, so the names stay taken and the next create of the workspace
is a `CONFLICT` naming them. Nothing is destroyed; only names leak. Migrate
is where a slot changes how it is checked out, so this phase should either
refuse a `vcs` change while unmoded slots would change kind, or write each
existing slot's effective mode into the record when `vcs` changes.

### Phase 10. Op-heads watch

**Dropped (owner, 2026-10-10).** It would have watched each primary's
`.jj/repo/op_heads/heads` and kept a throttled snapshot ticker so unsaved
edits became operations. The console now reads slot state on demand only,
and updates live only for what an owner pushes (Docker, process-compose and
the operator's own actions), so there is nothing to watch.
`.agents/plans/owner-pushed-live-views.md` owns that change; a jj slot
needs nothing from it beyond D7.

### Phase 11. Templates, docs, live check

1. angee-django: the `src` workspace template takes a `slot_mode` input
   (default `worktree`) used by the three code slots; the stack template
   takes `vcs` (default `git`), renders `vcs:` in the manifest and
   `slot_mode` into `workspace_defaults`; the work-state slot keeps no mode,
   so it follows `vcs`. Tests in `tests/test_workspace_templates.py` and
   `tests/test_stack_templates.py`.
2. Docs here: `docs/guide/manifest.md` (modes, `vcs`, remotes),
   `docs/guide/commands.md` (migrate, sync-base on jj),
   `docs/guide/templates.md`, `docs/reference/operator-api.md`,
   `docs/reference/surfaces.md`, and the agent contract from the proposal's
   section 5 in the workspace template's `AGENTS.md`.
3. Live check on the `dev-alexis` stack: re-create `dev-merge` through
   `angee ws create` instead of by hand, migrate one existing git workspace,
   run the operator in its container against the shared primaries, and walk
   the proposal's nine acceptance bullets.
4. Feed section 6 back into the proposal, mark it accepted, and mark this
   plan done.

Removal of the git merge/rebase state machine is **not** in this plan. It
happens when no stack has a git slot left, which is a separate decision.

## 5. Open questions for the owner

Q1 to Q7 and Q10 are answered; Q8 and Q9 are built as recommended.

- **Q1. How does a jj slot reach its final path? Answered 2026-10-05: in
  place** (D4). The alternatives that were weighed:
  - *Stage at the same depth, rename, repair* (the owner's first idea,
    probed): keeps the path guard and makes the slot appear atomically, but
    jj's record of the workspace path stays stale (`root` empty, `jj
    workspace root --name` and `jj workspace remove` fail, `forget` stops
    cleaning up the git worktree). Rewriting that record directly works but
    edits a jj-internal file; the owner chose no interim workaround.
  - *Wait for upstream:* jj-vcs/jj#10219 adds `jj workspace move WORKSPACE
    DESTINATION` (rename, rewrite the `.jj/repo` link, `git worktree
    repair`, update the record, roll back on failure). It requires the
    workspace to still be at its recorded path, so it can replace a staged
    rename but not re-record one done by the operator. Open, review
    pending, as of 2026-10-05.

  Why in place is enough: for a fresh create nothing else can write the new
  workspace directory; in an in-use workspace (a `--sync` re-add, migrate, a
  template update adding a slot) agents can write that directory but also
  the shared jj store, so the guard adds little; jj refuses a non-empty
  destination; the before/after identity check and rollback catch a swap.
  Optional later hardening, not a dependency: once #10219 is in jj main,
  the in-use cases can stage and call `jj workspace move`, shrinking the
  window to one rename.
- **Q2. Workspace naming. Answered 2026-10-09: `<stack>--<workspace>`**
  (plus `--<slot>` when one workspace has two slots of the same
  repository), and a clash is refused before `jj workspace add` runs (D13).
  The question as it was put: the proposal's `<stack>--<workspace>` matches
  the convention the shared work-state already uses, but two stacks with the
  same name on one machine collide. Options: keep it and fail with a clear
  message on collision (D13 as written); or append a short hash of the stack
  root. Recommendation: keep the readable name and fail clearly, because
  the names are what a human reads in `jj workspace list`.
- **Q3. Is `link` mode still wanted? Answered 2026-10-11: no; phase 3 is
  dropped.** The proposal lists it as prerequisite 3 (a slot that is a
  symlink to the primary checkout, for the shared work-state). Its own field
  evidence says the work-state protocol forbids a symlinked `.work` and that
  every `.work` must be its own jj workspace, which phase 6 delivers. Recommendation, taken: drop `link` from the
  proposal (correction 12).
- **Q4. Automatic sync cadence. Answered 2026-10-11: on request only, not
  in this plan.** The proposal's text wants the operator to rebase every
  slot whenever a fetch moves the base; its acceptance list only requires
  that `sync-base` does so when run. Automatic sync makes the
  operator rewrite commits under a working agent and force-update published
  branches without being asked. Recommendation, taken: not in this plan;
  reword the proposal's sentence to "on request" (correction 11), and
  revisit as a separate, default-off feature after phase 11.
- **Q5. What is "latest jj" in CI? Answered 2026-10-11: the jj main commit
  named in `.github/jj-rev`** (phase 5 step 8). Recommendation, taken: a
  `.github/jj-rev` file holding a jj main commit, bumped deliberately;
  developer machines run whatever `brew upgrade --fetch-HEAD jj` gives. The fixtures then record
  the newest jj the project has verified, and a bump that changes a template
  field fails one obvious test.
- **Q6. Snapshot interval. Answered 2026-10-10: there is none.** The 2 s
  polling loop goes: the console updates live only for what an owner pushes
  (Docker events, process-compose's state stream, the operator's own
  actions), and reads everything else on demand, slot state included, with
  a refresh button (`.agents/plans/owner-pushed-live-views.md`). Every slot
  read then snapshots (D7), and phase 10 is dropped. The question as it was
  put: D7 defaulted the poller to one snapshot per slot per 10 s
  (`ANGEE_JJ_SNAPSHOT_INTERVAL`). An idle slot costs nothing; while a slot
  is being edited, each snapshot writes one operation and briefly takes the
  slot's working-copy lock. The owner asked why the operator polls at all,
  which led to the on-demand rule.
- **Q7. Should `workspace push` cover clone slots with no `mode`?** A git
  slot with no mode is materialized as a clone, and status reports its
  remotes like a `mode: clone` slot's (phase 2), but `workspace push`
  selects only `worktree` and `clone` slots, so it skips it. Including it
  was tried in #116 and dropped: angee-django's `templates/workspaces/src`
  declares the `.work` work-state slot with no mode and a `kind: git`
  source, so `workspace push` would refuse whenever `.work` is dirty (almost
  always), and would push a clean, ahead `.work` with git, outside the
  work-state store's own publish recipe. Covering them needs a way to
  exclude a work-state slot first: a slot or source flag (`push: false`,
  say), a dedicated mode for it, or a rule on the work-state source.

  *Answered (owner, 2026-10-29): explicit mode only.* The verbs that walk
  a whole workspace (`sync-base`, and `workspace push` when it reaches jj
  slots) act only on slots whose manifest names a mode (`worktree`,
  `clone`, `jj`). A slot that names none is skipped by them, even when the
  stack's `vcs: jj` makes it a jj slot; the `.work` work-state slot is the
  case that matters, and it keeps its own publish recipe. The per-slot
  verbs (`workspace source ...`) still reach such a slot. This matches
  phase 11, where the code slots get an explicit `slot_mode` and `.work`
  keeps none. Built for sync-base in phase 8's first half, and for
  `workspace push`'s refusal of a workspace with a jj slot (it no longer
  counts a slot with no mode); `workspace push` on jj slots comes with the
  second half. A clone slot with no mode stays out of `workspace push`, as
  it was.
- **Q8. Which remote does a jj slot push to? Built as recommended in
  #126; the owner can still reverse it.** jj reads neither git's
  `remote.pushDefault`, which phase 2 writes from `push_default`, nor a
  branch's `pushRemote`. Without its own `git.push`, jj pushes a source that
  has an origin and a fork to origin. With several remotes and no origin,
  it refuses. The options were:
  - EnsurePrimary also writes `git.push = <push_default>` into jj's
    repository config (but see Q9: that config is per user);
  - the jj driver resolves the remote by git's order from the slot's git
    config (`git.Client.PushRemoteFor`, as publish does for git slots) and
    always passes `--remote` to `jj git push`; Status then counts against
    that remote too;
  - jj slots follow jj's rule, and the docs say `push_default` does not
    apply to them.

  Built: the second, so a git slot and a jj slot of one source publish to
  the same remote, and the manifest stays the one place that says where.
  Status counts the upstream on that remote, and phase 8 passes it to `jj
  git push --remote`. A jj `git.push` setting is ignored by angee.

  *Answered (owner, 2026-10-29): a publish to another remote is recorded,
  like `git push -u`.* A jj publish to a remote other than the branch's
  default push remote writes `branch.<X>.pushRemote = <remote>` in the
  slot's shared git config, and jj tracks `X@<remote>` (it does so for a
  bookmark it pushes new; an untracked one there is refused). Status
  (`PushRemoteFor`), later publishes and the destroy guard then follow that
  remote. Built in phase 8's second half, for a slot colocated with git; a
  store jj keeps without git has no config to record it in, so it names its
  remote on every publish. *Within that rule (coordinator, 2026-10-29):* a
  `kind: git` cache is angee's, so the record replaces whatever the branch
  had, as `git push -u` would, and the caller's latest explicit publish
  wins; a `kind: local` store's git config is the user's and is never
  written, so the publish warns that the remote was not recorded, and
  status keeps counting against the default push remote
  (`PublishPlan.Unmanaged`).
- **Q9. Where does jj's repository config live? Built as recommended in
  #126 for the operator; the agents' side is open.** jj 0.45 keeps `jj
  config set --repo` values in the user's config directory
  (`$XDG_CONFIG_HOME/jj/repos/<id>/config.toml`, keyed by
  `.jj/repo/config-id`), not inside `.jj`. D8's
  `snapshot.auto-update-stale` is therefore per user. An operator in a
  container with another HOME neither sees what the host user wrote nor
  writes where the agents' own jj reads. The options:
  - the runner passes `--config snapshot.auto-update-stale=true` on every
    operator command, which covers the operator wherever it runs, while
    EnsurePrimary still writes the repository setting for the host user;
  - the container mounts the host's jj config directory;
  - the agents' environment sets it in their user config.

  Built: the first. The agents' side (an agent's own `jj` in a slot the
  operator rebased) still needs one of the others. *Phase 5 step 6:*
  `EnsurePrimary` writes no repository setting either, so the runner's flag
  is the operator's whole answer.
- **Q10. Files jj will not snapshot. Built as recommended, (a), before
  phase 6 (2026-10-28); the owner can still reverse it.** (Found in the
  review of #126.)
  jj refuses to snapshot a new file over `snapshot.max-new-file-size`, and
  leaves untracked anything `snapshot.auto-track` excludes. The snapshot
  still exits 0 and `@` stays empty, so Status reports `Dirty=false,
  Pushed=true` for a slot holding a file nothing else has. Only jj's
  human-readable output says otherwise: the stderr warning "Refused to
  snapshot some files", and the `Untracked paths:` section of `jj status`.
  The options:
  - (a) treat jj's own report as dirty, reading the refusal warning, `jj
    status`'s untracked section, or both;
  - (b) accept it, and say so in the docs;
  - (c) ask git. Ruled out: section 2 forbids deriving jj slot state from
    git.

  Recommendation: (a). It is jj's account of the slot, and the destroy
  guard must not delete files only the slot holds. A test pins today's
  behaviour as a known limit. **Phase 6 must settle Q10 before the destroy
  guard reads a jj slot's `Pushed`.**

  Built: (a), from the untracked paths of `jj status`, which is the status
  call's snapshot and covers both cases; the warning covers only the first.
  They make the slot dirty, and a report the driver does not recognize is an
  error, never no untracked paths. Details in phase 6, "As built: the driver
  prerequisites of step 5".

  **Still open for the owner (found in that review).** jj also leaves out,
  and does not list, a directory holding a repository of its own (a nested
  `.git` or `.jj`) and a new file outside the slot's sparse patterns. Status
  reads such a slot clean and pushed, and a test pins that as a known limit.
  The options:
  - accept it, and say so in the docs (angee makes no slot sparse; a nested
    repository is an agent's clone inside a slot);
  - have the jj driver walk the slot for nested `.git` and `.jj` entries
    through `os.Root`, skipping its own: a check of the filesystem, not of
    git's state, so section 2 still holds. Sparse patterns would need jj's
    `jj sparse list` too.

  Recommendation: the walk for nested repositories before phase 6 turns the
  guard on, since agents do clone into slots, and the docs for sparse
  patterns.

  *Answered (owner, 2026-10-29):* only the destroy guard walks a jj slot,
  through `os.Root`, following no link and skipping nothing, and refuses
  the destroy naming each nested repository; Status does not walk. New
  files outside sparse patterns are documented as unsupported, since angee
  never makes a slot sparse. Built in phase 6, which counts a nested `.git`
  file too (a git worktree or submodule checkout).

  **Known limit (review of #139): check, then delete.** A file written
  between the destroy guard's status read and the slot's removal is deleted
  unseen. The git driver has the same window, and no read of the slot can
  close it.

## 6. Corrections to feed back into the proposal

1. An omitted mode is not `worktree` today; it is clone for git sources and
   symlink for local ones (D3).
2. `beginMutation` does not serialize per primary (D9).
3. "Keys decisions on exit status" is not enough: conflicts exit 0, and push
   refusals need a pre-check to be structured (D5, D6).
4. `.jj/` does not need adding to `.git/info/exclude`; jj writes
   `.jj/.gitignore`.
5. `remotes: map[name]url` cannot carry the canonical flag or the branch
   allow-list (phase 2).
6. `jj rebase -d` should read `--onto` (D10).
7. `workspaceSourceMerge` as `jj new <slot>@ <ref>` leaves the merge as the
   working copy, which publish skips; it needs a following `jj new` (phase 8).
8. `jj rebase` reaches descendants in other workspaces; the proposal has no
   rule for shared history (D12).
9. Topology cannot report other stacks' workspace paths reliably (phase 7).
10. Migration must carry ignored and untracked files across (phase 9).
11. Automatic sync is in the text but not in the acceptance list (Q4): it
    runs on request only.
12. `link` mode is dropped (Q3): every `.work` is its own jj workspace.

## 7. Review record

Codex reviewed the first draft on 2026-10-01 against the code and jj's help
output (its sandbox could not run jj in a scratch repo, so the jj-dependent
findings were re-probed here before being accepted). Fifteen findings, all
accepted in whole or in part:

| Finding | Outcome |
|---|---|
| `--sync` forget-and-recreate could drop a slot's work | Accepted. Phase 6 step 4 keeps a registered slot in place. |
| `mode: jj` enabled before status and verbs handle it | Accepted. Status moved into phase 6; unfinished verbs refuse with a typed error. |
| `jj rebase -b` rewrites other slots' descendants | Accepted. D12. |
| In-place creation weakens the path guard | Weighed and accepted as a known limit (D4, Q1, 2026-10-05): staging would leave jj's path record stale, which no released jj can fix; in place is checked before and after and rolled back. |
| `vcs.Driver` lacked mode, clone URL, the dest capability, diff | Accepted. Interface extended; phase 4 split into three PRs with characterization tests. |
| Polled status would report `clean` over unsaved edits | Accepted. D7 rewritten: status snapshots, poller throttled. Since 2026-10-10 there is no poller: slot state is read on demand and every read snapshots (Q6). |
| Merge left the merge commit as `@`, which publish skips | Accepted and probed. Phase 8 step 5. |
| Multi-remote sources had no clone URL | Accepted. Phase 2 step 2. |
| The lock was under `.jj` (absent before init) and too narrow | Accepted. D9 moved beside the primary, covers git and jj. |
| Base drift, publication and branch identity conflated in status | Accepted. Phase 5 step 5 separates upstream, base and `CurrentRef`. |
| Publish pre-check did not cover jj's whole push policy | Accepted by narrowing the promise. D6. |
| Migrate lost ignored files and could not roll back | Accepted and probed. Phase 9 preserves them by default. |
| Workspace names not unique, not quoted in revsets | Quoting accepted (D13). Uniqueness is Q2. |
| Inventory gaps, `make check-schema`, jj CI must fail not skip, Windows | Accepted. Section 3, section 4 preamble, phase 5 steps 3 and 8. |
| Container paths and automatic sync unsettled | Accepted. Phase 6 step 8, phase 11 step 3, Q4. |
