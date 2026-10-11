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

**Where it stands (2026-10-11):** phase 1 is merged (#93) and released in
v0.16.0. Phase 4a (reads behind the driver) is merged as #107
(`cb27d2e`), and phase 2 steps 1-4 (remotes, push default, canonical
remote, cache reconcile) as #116 (`12e69d1`), and 4b (verbs behind the
driver) as #117 (`86fc88d`). 4d (driver-owned push and publish rules) is
#120, and phase 2 step 5 (publish's default remote, the `branches`
allow-list and a remote the slot doesn't have) is stacked on it in the PR
that updates this paragraph; both land before the release after v0.21.3.
4c (lifecycle) follows before phase 5. Phase 4 is the
critical path. Q1 (jj slots are created in place), Q2 (2026-10-09:
`<stack>--<workspace>`, a clash is refused), Q6 (2026-10-10: slot state is
read on demand, so every read snapshots and there is no interval, which
drops phase 10), and Q3 to Q5 (2026-10-11: `link` mode is dropped, sync
runs only on request, and CI pins jj in `.github/jj-rev`) are answered;
Q7 (`workspace push` and clone slots with no mode) is open. Phase 6 waits
only on phase 5.

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
| D8 | Slot mutations run with the slot as working directory, and every primary gets `snapshot.auto-update-stale = true`. | Rewriting a slot's commits from elsewhere leaves it stale and its next plain `jj` command fails; with the setting it repairs itself (probe). |
| D9 | Every operator mutation of a primary, git or jj, takes one cross-process file lock for that primary, at `<parent>/.<name>.angee.lock` beside the primary. | `beginMutation` is an in-process, per-`Platform` try-lease (`platform.go:105`). It does not serialize two stacks, or the CLI and the operator, on one shared primary. The lock sits outside the primary because `.jj` does not exist before initialization. Order: stack mutation lease, then the root lock, then the primary lock. |
| D10 | The jj driver uses `jj rebase --onto`. | `-d/--destination` is no longer in `jj rebase --help`; it still parses as a legacy alias. |
| D11 | `sync-base` treats an unset method as "the driver's default": merge for git slots, rebase for jj slots. An explicit `--merge` on a workspace that has a jj slot is refused before any slot is touched. | The CLI hardcodes `method := "merge"` (`cli/root.go:1231`), so the service cannot tell a default from an explicit choice. Silently rebasing when merge was asked for is worse than refusing. |
| D12 | A rebase on a jj slot is refused when the commits it would rewrite, `(B..N@)::`, include another workspace's working copy or the checked-out branch of a git worktree of the same primary. | `jj rebase -b` rewrites descendants too, so one Angee workspace's sync could rewrite another's commits. For a git worktree the moved branch ref shows up as a dirty tree. |
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
| `JJ_USER`, `JJ_EMAIL`, `JJ_EDITOR` are honoured. `--config ui.editor=true` is rejected (the value parses as a boolean). | The runner sets identity and a no-op editor through the environment. |
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
2  remotes with roles                               (git only; steps 1-4 merged, 5 in review)
4a..4d  vcs.Driver seam, git driver only            (after 1; 4a, 4b merged, 4d in review, 4c next)
5  jj driver package + doctor probe + CI            (after 4)
6  mode: jj lifecycle + status, other verbs refuse  (after 5)
7  status fields and topology                       (after 6)
8  sync-base, publish, push, merge on jj slots      (after 7; needs 2)
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
    cache are not serialized.
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
   uses a snapshotting status, so unsaved edits count as dirty.
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

### Phase 8. Sync-base, publish, push and merge on jj slots

Replaces the phase 6 refusals verb by verb. May land as two PRs (sync-base
and rebase; then publish, push and merge).

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

Q1, Q2 and Q6 are answered. The rest can be answered later.

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
  say), a dedicated mode for it, or a rule on the work-state source. Open.

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
