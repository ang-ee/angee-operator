package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/vcs"
)

// moveBase pushes a commit setting name to content on main from the seed,
// fetching it nowhere.
func (f *jjFixture) moveBase(name, content string) {
	f.t.Helper()
	mustWriteFile(f.t, filepath.Join(f.seed, name), content)
	runGit(f.t, f.seed, "add", ".")
	runGit(f.t, f.seed, "commit", "-q", "-m", "upstream "+name)
	runGit(f.t, f.seed, "push", "-q", "origin", "HEAD:main")
}

// jjCommit records name with content in the jj slot at dir as a commit with
// message, leaving an empty working-copy commit on top, as an agent does.
func (f *jjFixture) jjCommit(dir, name, content, message string) {
	f.t.Helper()
	mustWriteFile(f.t, filepath.Join(dir, name), content)
	f.jj(dir, "commit", "-m", message)
}

// revision is jj's template field of revision r in the slot at dir, read
// without snapshotting it.
func (f *jjFixture) revision(dir, r, field string) string {
	f.t.Helper()
	return strings.TrimSpace(f.jj(dir, "--ignore-working-copy", "log", "--no-graph", "-r", r, "-T", field))
}

// sync-base over a workspace with two jj slots and a git worktree slot of one
// cache, once the base has moved: with no method, the worktree is merged and
// each jj slot rebased, its commit and its unrecorded edit riding along. One
// jj slot conflicts: its conflict is recorded and reported on its state with
// its files, and the sync goes on to the slots after it. The workspace stays
// usable, and once the conflict is resolved a second sync reports none. An
// explicit merge is refused first, before anything is fetched or changed
// (plan D11).
func TestSyncBaseRebasesJJSlots(t *testing.T) {
	f := newJJFixture(t, "")
	ctx := context.Background()
	template := f.template("sync", map[string]string{
		"a":    f.app("mode: jj\nref: main\nsubpath: a"),
		"b":    f.app("mode: jj\nref: main\nsubpath: b"),
		"tree": f.app("mode: worktree\nbranch: \"${inputs.branch}\"\nref: main\nsubpath: tree"),
	})
	if err := f.create(template, "ws", false); err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	a, b, tree := filepath.Join(f.workspace("ws"), "a"), filepath.Join(f.workspace("ws"), "b"), filepath.Join(f.workspace("ws"), "tree")
	f.jjCommit(a, "f.txt", "one\nslot a\n", "a's change")
	f.jjCommit(b, "b.txt", "b\n", "b's change")
	mustWriteFile(t, filepath.Join(b, "wip.txt"), "unrecorded\n")
	mustWriteFile(t, filepath.Join(tree, "t.txt"), "tree\n")
	runGit(t, tree, "add", ".")
	runGit(t, tree, "commit", "-q", "-m", "tree's change")
	treeHead := strings.TrimSpace(runGitOutput(t, tree, "rev-parse", "HEAD"))
	f.moveBase("f.txt", "one\nupstream\n")
	baseBefore := strings.TrimSpace(runGitOutput(t, f.cache(), "rev-parse", "origin/main"))
	bCommit := f.revision(b, "@-", "commit_id")

	_, err := f.platform.WorkspaceSyncBase(ctx, "ws", "merge")
	if opErr := AsOperationError(err); opErr == nil || opErr.Code != CodeInvalidInput || opErr.Slot != "a" || !errors.Is(err, vcs.ErrUnsupported) {
		t.Fatalf("WorkspaceSyncBase(merge) error = %v (%+v), want INVALID_INPUT naming jj slot a", err, opErr)
	}
	if got := strings.TrimSpace(runGitOutput(t, f.cache(), "rev-parse", "origin/main")); got != baseBefore {
		t.Errorf("a refused merge fetched the base: origin/main %s, was %s", got, baseBefore)
	}
	if got := strings.TrimSpace(runGitOutput(t, tree, "rev-parse", "HEAD")); got != treeHead || f.revision(b, "@-", "commit_id") != bCommit {
		t.Errorf("a refused merge changed a slot")
	}

	states, err := f.platform.WorkspaceSyncBase(ctx, "ws", "")
	if err != nil {
		t.Fatalf("WorkspaceSyncBase() error = %v", err)
	}
	if len(states) != 3 || states[0].Slot != "a" || states[1].Slot != "b" || states[2].Slot != "tree" {
		t.Fatalf("WorkspaceSyncBase() states = %+v, want a, b and tree", states)
	}
	if got := states[0]; !got.Conflicted || !slices.Equal(got.ConflictPaths, []string{"f.txt"}) || got.Behind != 0 || got.Error != "" {
		t.Errorf("slot a = %+v, want rebased with its conflict in f.txt recorded", got)
	}
	if got := states[1]; got.Conflicted || got.Behind != 0 || got.Ahead != 1 || got.State != "dirty" || got.Error != "" {
		t.Errorf("slot b = %+v, want rebased cleanly, one commit ahead, its edit carried along", got)
	}
	if got := states[2]; got.Conflicted || got.Behind != 0 || got.State != "ahead" || got.Error != "" {
		t.Errorf("slot tree = %+v, want merged, ahead and not behind", got)
	}
	newBase := strings.TrimSpace(runGitOutput(t, f.cache(), "rev-parse", "origin/main"))
	if got := f.revision(b, "(@-)-", "commit_id"); got != newBase {
		t.Errorf("b's commit sits on %s, want the moved base %s", got, newBase)
	}
	if got := f.jj(b, "--ignore-working-copy", "file", "show", "-r", "@", "wip.txt"); got != "unrecorded\n" {
		t.Errorf("b's working-copy commit holds wip.txt %q, want the unrecorded edit", got)
	}
	if got := strings.TrimSpace(runGitOutput(t, tree, "rev-parse", "HEAD^1")); got != treeHead {
		t.Errorf("tree's HEAD^1 = %s, want its own commit %s merged onto", got, treeHead)
	}
	if got := strings.TrimSpace(runGitOutput(t, tree, "rev-parse", "HEAD^2")); got != newBase {
		t.Errorf("tree's HEAD^2 = %s, want the base %s merged in", got, newBase)
	}

	status, err := f.platform.WorkspaceStatus(ctx, "ws")
	if err != nil || status.State != "ready" {
		t.Fatalf("WorkspaceStatus() after the sync = %+v, %v, want a ready workspace", status, err)
	}
	for _, source := range status.Sources {
		if source.Error != "" {
			t.Errorf("slot %s after the sync: %s", source.Slot, source.Error)
		}
	}
	f.jj(a, "status")
	mustWriteFile(t, filepath.Join(a, "f.txt"), "one\nupstream\nslot a\n")
	f.jj(a, "squash", "-m", "a's change")
	states, err = f.platform.WorkspaceSyncBase(ctx, "ws", "")
	if err != nil {
		t.Fatalf("WorkspaceSyncBase() once resolved error = %v", err)
	}
	for _, state := range states {
		if state.Conflicted || state.Error != "" {
			t.Errorf("slot %s after the second sync = %+v, want no conflict", state.Slot, state)
		}
	}
}

// A jj slot whose commits another workspace's slot is stacked on is refused
// by sync-base and by rebase, naming that workspace, and nothing is rebased
// (plan D12): jj would rewrite the other slot's work too. sync-base checks
// every slot first, so the free slot sorted before the refused one is not
// rebased either. The stacked slot is refused the same way, since its own
// commits hold the first's.
func TestSyncBaseRefusesAStackedJJSlot(t *testing.T) {
	f := newJJFixture(t, "")
	ctx := context.Background()
	one := f.template("one", map[string]string{"work": f.app("mode: jj\nref: main\nsubpath: work")})
	two := f.template("two", map[string]string{
		"a":    f.app("mode: jj\nref: main\nsubpath: a"),
		"work": f.app("mode: jj\nref: main\nsubpath: work"),
	})
	if err := f.create(two, "ws1", false); err != nil {
		t.Fatalf("WorkspaceCreate(ws1) error = %v", err)
	}
	if err := f.create(one, "ws2", false); err != nil {
		t.Fatalf("WorkspaceCreate(ws2) error = %v", err)
	}
	free, first, second := filepath.Join(f.workspace("ws1"), "a"), filepath.Join(f.workspace("ws1"), "work"), filepath.Join(f.workspace("ws2"), "work")
	f.jjCommit(free, "a.txt", "free\n", "free change")
	f.jjCommit(first, "g.txt", "mine\n", "first's change")
	freeOwn, own := f.revision(free, "@-", "commit_id"), f.revision(first, "@-", "commit_id")
	f.jj(second, "new", f.revision(first, "@-", "change_id"))
	f.moveBase("f.txt", "one\nupstream\n")

	for _, tc := range []struct {
		workspace, other string
		call             func(workspace string) error
	}{
		{"ws1", "dev--ws2", func(ws string) error { _, err := f.platform.WorkspaceSyncBase(ctx, ws, ""); return err }},
		{"ws1", "dev--ws2", func(ws string) error {
			_, err := f.platform.WorkspaceSourceRebase(ctx, ws, "work", "main@origin")
			return err
		}},
		{"ws2", "dev--ws1--work", func(ws string) error { _, err := f.platform.WorkspaceSyncBase(ctx, ws, ""); return err }},
	} {
		err := tc.call(tc.workspace)
		opErr := AsOperationError(err)
		var shared *vcs.SharedCommitsError
		if opErr == nil || opErr.Code != CodePreconditionFailed || opErr.Cause != CauseSharedCommits || opErr.Slot != "work" || opErr.Workspace != tc.workspace ||
			!errors.As(err, &shared) || !strings.Contains(err.Error(), `jj workspace "`+tc.other+`"`) {
			t.Fatalf("%s: error = %v (%+v), want shared_commits naming %s", tc.workspace, err, opErr, tc.other)
		}
	}
	if got := f.revision(first, "@-", "commit_id"); got != own {
		t.Errorf("a refused rebase rewrote %s into %s", own, got)
	}
	if got := f.revision(free, "@-", "commit_id"); got != freeOwn {
		t.Errorf("a refused sync-base rebased the free slot: %s became %s", freeOwn, got)
	}
}

// A rebase jj would refuse for an immutable commit, here one of a slot's own
// that a tag holds, is refused before any slot is rebased, naming the commit,
// so the slot sorted before it is left as it was too.
func TestSyncBaseRefusesImmutableCommitsBeforeAnySlotIsRebased(t *testing.T) {
	f := newJJFixture(t, "")
	ctx := context.Background()
	if err := f.create(f.template("two", map[string]string{
		"a": f.app("mode: jj\nref: main\nsubpath: a"),
		"b": f.app("mode: jj\nref: main\nsubpath: b"),
	}), "ws", false); err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	a, b := filepath.Join(f.workspace("ws"), "a"), filepath.Join(f.workspace("ws"), "b")
	f.jjCommit(a, "a.txt", "a\n", "a's change")
	f.jjCommit(b, "b.txt", "b\n", "b's change")
	aOwn, bOwn := f.revision(a, "@-", "commit_id"), f.revision(b, "@-", "commit_id")
	runGit(t, f.cache(), "tag", "v1", bOwn)
	f.moveBase("f.txt", "one\nupstream\n")

	_, err := f.platform.WorkspaceSyncBase(ctx, "ws", "")
	if opErr := AsOperationError(err); opErr == nil || opErr.Code != CodePreconditionFailed || opErr.Cause != CauseImmutableCommits || opErr.Slot != "b" ||
		!strings.Contains(err.Error(), bOwn[:12]) {
		t.Fatalf("WorkspaceSyncBase() error = %v (%+v), want immutable_commits naming b's tagged commit", err, opErr)
	}
	if f.revision(a, "@-", "commit_id") != aOwn || f.revision(b, "@-", "commit_id") != bOwn {
		t.Error("a refused sync-base rebased a slot")
	}
}

// A jj slot of a `kind: local` store is synced too, onto its base as the
// store has it: the store is the user's, so angee fetches nothing into it.
func TestSyncBaseRebasesALocalStoreSlotWithoutFetching(t *testing.T) {
	f := newJJFixture(t, "")
	ctx := context.Background()
	store := filepath.Join(f.base, "store")
	runGit(t, f.base, "init", "-q", "-b", "main", store)
	f.jj(store, "git", "init", "--colocate")
	f.jjCommit(store, "f.txt", "one\n", "base")
	f.jj(store, "bookmark", "create", "main", "-r", "@-")
	if err := f.create(f.template("local", map[string]string{"notes": "source: notes\nkind: local\npath: " + store + "\ndefault_ref: main\nmode: jj\nsubpath: notes"}), "ws", false); err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	slot := filepath.Join(f.workspace("ws"), "notes")
	f.jjCommit(slot, "g.txt", "mine\n", "mine")
	f.jjCommit(store, "h.txt", "moved\n", "the base moves")
	f.jj(store, "bookmark", "set", "main", "-r", "@-")

	states, err := f.platform.WorkspaceSyncBase(ctx, "ws", "")
	if err != nil || len(states) != 1 || states[0].Slot != "notes" || states[0].Behind != 0 || states[0].Ahead != 1 {
		t.Fatalf("WorkspaceSyncBase() = %+v, %v, want the store's slot rebased onto its main", states, err)
	}
	if got, want := f.revision(slot, "(@-)-", "commit_id"), f.revision(store, "main", "commit_id"); got != want {
		t.Errorf("the slot's commit sits on %s, want the store's main %s", got, want)
	}
	if ops := f.jj(store, "--ignore-working-copy", "op", "log", "--no-graph", "-T", `description ++ "\n"`); strings.Contains(ops, "fetch") {
		t.Errorf("sync-base fetched into the user's store:\n%s", ops)
	}
}

// sync-base walks only the slots whose manifest names a mode (owner,
// 2026-10-29, plan Q7): under `vcs: jj`, a slot that names none, as a
// work-state `.work` does, is a jj slot that sync-base neither rebases nor
// counts against an explicit merge. The per-slot rebase still reaches it,
// and a conflict it records is an OK result with the files.
func TestSyncBaseSkipsASlotWithNoMode(t *testing.T) {
	f := newJJFixture(t, manifest.VCSJJ)
	ctx := context.Background()
	template := f.template("mixed", map[string]string{
		"tree": f.app("mode: worktree\nbranch: \"${inputs.branch}\"\nref: main\nsubpath: tree"),
		"work": f.app("ref: main\nsubpath: .work"),
	})
	if err := f.create(template, "ws", false); err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	work := filepath.Join(f.workspace("ws"), ".work")
	f.jjCommit(work, "f.txt", "one\nnotes\n", "notes")
	own := f.revision(work, "@-", "commit_id")
	f.moveBase("f.txt", "one\nupstream\n")

	for _, method := range []string{"", "merge"} {
		states, err := f.platform.WorkspaceSyncBase(ctx, "ws", method)
		if err != nil || len(states) != 1 || states[0].Slot != "tree" {
			t.Fatalf("WorkspaceSyncBase(%q) = %+v, %v, want the worktree slot alone", method, states, err)
		}
	}
	if got := f.revision(work, "@-", "commit_id"); got != own {
		t.Errorf("sync-base rewrote the slot with no mode: %s became %s", own, got)
	}
	result, err := f.platform.WorkspaceSourceRebase(ctx, "ws", "work", "main@origin")
	if err != nil || !result.OK || !result.Conflicted || !slices.Equal(result.ConflictFiles, []string{"f.txt"}) {
		t.Fatalf("WorkspaceSourceRebase() = %+v, %v, want OK and conflicted in f.txt", result, err)
	}
	if got := f.slotStatus("ws", "work"); got.Mode != manifest.WorkspaceSourceModeJJ || got.Behind != 0 || got.Error != "" {
		t.Errorf("status after the rebase = %+v, want a jj slot on the moved base", got)
	}
}

// A jj slot holding files jj does not track (Q10) is refused by sync-base and
// by its rebase, naming them, before anything is fetched or rebased: a base
// that adds the same path would have the local file kept over its own, and
// the next snapshot record it as undoing the base's. The jj slot sorted
// before it is not rebased either.
func TestSyncBaseRefusesAJJSlotWithUntrackedPaths(t *testing.T) {
	f := newJJFixture(t, "")
	ctx := context.Background()
	if err := f.create(f.template("two", map[string]string{
		"a": f.app("mode: jj\nref: main\nsubpath: a"),
		"b": f.app("mode: jj\nref: main\nsubpath: b"),
	}), "ws", false); err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	a, b := filepath.Join(f.workspace("ws"), "a"), filepath.Join(f.workspace("ws"), "b")
	f.jjCommit(a, "a.txt", "a\n", "a's change")
	aOwn := f.revision(a, "@-", "commit_id")
	mustWriteFile(t, os.Getenv("JJ_CONFIG"), "[snapshot]\nmax-new-file-size = 8\n")
	mustWriteFile(t, filepath.Join(b, "big.txt"), "more than eight bytes\n")
	f.moveBase("big.txt", "upstream\n")
	baseBefore := strings.TrimSpace(runGitOutput(t, f.cache(), "rev-parse", "origin/main"))

	for name, call := range map[string]func() error{
		"sync-base": func() error { _, err := f.platform.WorkspaceSyncBase(ctx, "ws", ""); return err },
		"rebase":    func() error { _, err := f.platform.WorkspaceSourceRebase(ctx, "ws", "b", "main@origin"); return err },
	} {
		err := call()
		if opErr := AsOperationError(err); opErr == nil || opErr.Code != CodePreconditionFailed || opErr.Cause != CauseUntrackedPaths || opErr.Slot != "b" ||
			!slices.Equal(opErr.Paths, []string{"big.txt"}) || !strings.Contains(err.Error(), "big.txt") {
			t.Fatalf("%s error = %v (%+v), want untracked_paths naming big.txt in b", name, err, opErr)
		}
	}
	if got := strings.TrimSpace(runGitOutput(t, f.cache(), "rev-parse", "origin/main")); got != baseBefore {
		t.Errorf("a refused sync-base fetched the base: origin/main %s, was %s", got, baseBefore)
	}
	if got := f.revision(a, "@-", "commit_id"); got != aOwn {
		t.Errorf("a refused sync-base rebased slot a: %s became %s", aOwn, got)
	}
}

// When a git slot stops on conflicts after a jj slot sorted before it was
// rebased, the merge_conflict names the jj slot as synced, since it stays
// synced.
func TestSyncBaseConflictNamesTheSlotsSyncedBeforeIt(t *testing.T) {
	f := newJJFixture(t, "")
	ctx := context.Background()
	if err := f.create(f.template("mixed", map[string]string{
		"a":    f.app("mode: jj\nref: main\nsubpath: a"),
		"tree": f.app("mode: worktree\nbranch: \"${inputs.branch}\"\nref: main\nsubpath: tree"),
	}), "ws", false); err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	a, tree := filepath.Join(f.workspace("ws"), "a"), filepath.Join(f.workspace("ws"), "tree")
	f.jjCommit(a, "a.txt", "a\n", "a's change")
	mustWriteFile(t, filepath.Join(tree, "f.txt"), "one\ntree\n")
	runGit(t, tree, "commit", "-q", "-am", "tree's change")
	f.moveBase("f.txt", "one\nupstream\n")

	_, err := f.platform.WorkspaceSyncBase(ctx, "ws", "")
	opErr := AsOperationError(err)
	if opErr == nil || opErr.Cause != CauseMergeConflict || opErr.Slot != "tree" || !strings.Contains(err.Error(), `source slot "a" synced before it, and stay synced.`) {
		t.Fatalf("WorkspaceSyncBase() error = %v (%+v), want tree's merge_conflict naming a as synced", err, opErr)
	}
	newBase := strings.TrimSpace(runGitOutput(t, f.cache(), "rev-parse", "origin/main"))
	if got := f.revision(a, "(@-)-", "commit_id"); got != newBase {
		t.Errorf("slot a's commit sits on %s, want the base %s it was rebased onto", got, newBase)
	}
}
