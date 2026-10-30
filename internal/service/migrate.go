package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/copierx"
	"github.com/ang-ee/angee-operator/internal/git"
	"github.com/ang-ee/angee-operator/internal/logctx"
	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/vcs"
	"github.com/ang-ee/angee-operator/internal/vcs/gitdriver"
	"github.com/ang-ee/angee-operator/internal/vcs/jjdriver"
)

// jjMigrator is what migrating a slot asks of the jj driver beyond the jj
// lifecycle (jjLifecycle), which jjdriver.Driver answers.
type jjMigrator interface {
	jjLifecycle
	Import(ctx context.Context, primary string) error
	Tip(ctx context.Context, dir string) (string, error)
	TrackedPaths(ctx context.Context, dir, commit string) ([]string, error)
	PrepareWorktree(ctx context.Context, s vcs.Slot) (jjdriver.Handoff, error)
	RestoreBookmark(ctx context.Context, s vcs.Slot, h jjdriver.Handoff) error
}

var _ jjMigrator = jjdriver.Driver{}

// worktreeStager is what migrating a slot to a git worktree asks of the git
// driver beyond vcs.Driver: an add staged where the migrate's record names
// before git runs (gitdriver.Driver.AddStaged).
type worktreeStager interface {
	vcs.Driver
	AddStaged(ctx context.Context, s vcs.Slot, dest vcs.Dest, stageRoot string) error
}

var _ worktreeStager = gitdriver.Driver{}

// The steps after which a test fails a migrate (Platform.migrateFault), each
// named for what has just been done. A failure at any of them, or in any
// action between them, rolls the migrate back.
const (
	// migrateStepAside: the slot is set aside, its git registration repaired.
	migrateStepAside = "aside"
	// migrateStepAdding: the step that removes the new checkout is recorded,
	// and the checkout is made next.
	migrateStepAdding = "adding"
	// migrateStepAdded: the new checkout is at the slot's path.
	migrateStepAdded = "added"
	// migrateStepCarried: the first entry is renamed into the new checkout,
	// when there is one, and nothing more is done: the record names it, and
	// every other entry, before the first rename (carry).
	migrateStepCarried = "carried"
	// migrateStepSave: every entry is carried across, and the manifest is
	// written next.
	migrateStepSave = "save"
	// migrateStepSaved: the manifest records the new mode, and the clean-up
	// comes next. A failure here is a warning.
	migrateStepSaved = "saved"
	// migrateStepForgotten: after the manifest is written, the jj workspace
	// is forgotten, and the new worktree, set aside for it, is moved back
	// next. A failure here is a warning, as all clean-up is.
	migrateStepForgotten = "forgotten"
)

// migrateRollbackTimeout bounds a migrate's rollback and its clean-up, which
// run on a context the request cannot cancel.
const migrateRollbackTimeout = 2 * time.Minute

// asidePrefix starts the private name a migrate sets a slot aside under, in
// the slot's own parent directory.
const asidePrefix = ".angee-migrate-"

// keptSuffix ends the name of the directory an undo of a migrate keeps a
// carried copy in when the old checkout holds that entry again (carry): the
// aside's name, so beside the slot and outside both checkouts, and reported
// by asidesBeside and the destroy guard. It holds the copies under the
// slot's own name, and, at its top, a .gitignore of keptIgnore, so neither
// git nor jj records them when the slot's parent is itself a checkout (a
// slot at the workspace's root, with this one inside it).
const keptSuffix = "-kept"

// keptIgnore is the .gitignore at the top of the kept directory: everything
// in it, itself included.
const keptIgnore = "/*\n"

// WorkspaceSourceMigrate moves a workspace slot between a git worktree and a
// jj slot of its source's cache, keeping what neither tracks (plan phase 9).
// to is manifest.WorkspaceSourceModeJJ or manifest.WorkspaceSourceModeWorktree;
// empty means the other one of the two. A slot already in that mode is left
// as it is.
//
// `git worktree remove` deletes ignored files, and a real slot always has
// some (node_modules, .venv, local env files), so neither direction removes
// a checkout with its files in it. The slot is renamed aside in its own
// parent, which keeps the relative links of its .git and .jj working, and
// its git registration repaired to follow it; the new checkout is made at
// the slot's path, of the same commit, and the entries the old one held that
// neither tracks are renamed into it one by one; then the manifest records
// the new mode. Only after that is the old checkout removed: its git
// registration dropped, and, for a jj slot, its jj workspace forgotten.
//
//   - worktree to jj: the worktree must be on its branch with no changes to
//     tracked files (a slot on no branch must hold no commits beyond its
//     base). The cache is colocated with jj and its git refs imported, and
//     the jj slot made at the path on the slot's branch, under the name
//     phase 6 gives it, which must be free. Untracked files that are not
//     ignored become changes in the jj slot's working-copy commit, as jj
//     records every file it does not ignore, and the result's message says
//     so.
//   - jj to worktree: the slot's working copy must hold nothing (no changes,
//     no files jj does not track: untracked_paths), the commit under it must
//     not be conflicted, and the slot must be the jj workspace this stack
//     made (destroyedJJSlot). Its bookmark is set on that commit, moved
//     forward or created, unless it holds commits the slot does not
//     (branch_moved); the worktree is cut on that branch, or detached there
//     for a slot on no branch.
//
// Before the manifest is written, any failure rolls back what was done, in
// reverse: the entries carried across go back (beside the slot, when the old
// checkout holds one again, written into it meanwhile: keptDir), the new
// checkout is removed, once it holds none of them, the slot is renamed back
// and its registration repaired, and a bookmark set is put back, so the slot
// is as it was. A rollback step that fails stops the rollback, since the
// ones after it would delete what it did not put back, and the error says
// where each thing is. A failure after the manifest is written leaves the
// slot migrated, and the result's message warns of it.
//
// A slot at the workspace's root, or one other slots lie inside, is refused:
// setting it aside would move them too. So is one that holds a repository
// the migrate would not move whole, inside an entry it carries across
// (refuseNestedRepositories): one in a tracked directory, or, to jj, one
// outside what git ignores. Anything that writes the slot by
// path during the migrate (a running service) may write into either
// checkout; stop it first.
func (p *Platform) WorkspaceSourceMigrate(ctx context.Context, workspaceName, slot, to string) (result api.GitOpResult, err error) {
	defer workspaceSlotOperation("workspace.source-migrate", "Migrate", workspaceName, slot).annotate(&err)
	ctx, release, err := p.beginMutation(ctx, "workspace")
	if err != nil {
		return api.GitOpResult{}, err
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return api.GitOpResult{}, err
	}
	tx, stack, err := openParentStackTransaction(p.root, false)
	if err != nil {
		return api.GitOpResult{}, err
	}
	defer func() { _ = tx.Close() }()
	m, err := p.planMigration(stack, workspaceName, slot, to)
	if err != nil {
		return api.GitOpResult{}, err
	}
	if reason, hint, unfinished := p.unfinishedMigrate(workspaceName, slot, m.path); unfinished {
		return api.GitOpResult{}, p.migrateIncomplete(workspaceName, slot, m.sourceName, m.path, reason, hint)
	}
	if m.from == m.to {
		status := p.workspaceSourceStatus(ctx, workspaceName, slot, m.wsSource, stack)
		return api.GitOpResult{OK: true, ConflictFiles: []string{}, Message: fmt.Sprintf("source slot %q is a %s already; nothing to migrate", slot, modeNoun(m.to)), Source: &status}, nil
	}
	if err := p.requireJJSlots(ctx); err != nil {
		return api.GitOpResult{}, err
	}
	if err := requireWorkspaceSlotsOnDisk(workspaceName, []workspaceSlotPath{{slot: slot, path: m.path, source: m.source, mode: m.from}}); err != nil {
		return api.GitOpResult{}, err
	}
	rel, err := filepath.Rel(p.root, m.path)
	if err != nil {
		return api.GitOpResult{}, err
	}
	dest, err := copierx.OpenGuardedPath(p.root, p.root, filepath.ToSlash(rel), nil)
	if err != nil {
		return api.GitOpResult{}, fmt.Errorf("validate workspace source destination %s: %w", m.path, err)
	}
	defer func() { _ = dest.Close() }()
	m.dest, m.tx, m.stack = dest, tx, stack
	var message string
	if m.to == manifest.WorkspaceSourceModeJJ {
		message, err = p.migrateToJJ(ctx, m)
	} else {
		message, err = p.migrateToWorktree(ctx, m)
	}
	if err != nil {
		return api.GitOpResult{}, err
	}
	if m.stranded {
		message += fmt.Sprintf("\nWarning: the migrate's record %s is kept, so status and the verbs report the slot until its checkout is moved back and the record deleted.", p.migrateMarkerPath(workspaceName, slot))
	} else if err := m.endMarker(); err != nil {
		message += "\nWarning: " + err.Error()
	}
	status := p.workspaceSourceStatus(ctx, workspaceName, slot, stack.Workspaces[workspaceName].Sources[slot], stack)
	return api.GitOpResult{OK: true, ConflictFiles: []string{}, Message: message, Source: &status}, nil
}

// migration is one migrate of a slot under way: what it acts on, and what it
// has done that a rollback undoes.
type migration struct {
	p                   *Platform
	workspaceName, slot string
	sourceName          string
	source              manifest.Source
	wsSource            manifest.WorkspaceSource // as recorded
	from, to            string                   // effective modes
	path, primary       string
	dest                *copierx.GuardedPath
	tx                  *parentStackTransaction
	stack               *manifest.Stack
	aside               *copierx.Aside
	undo                []migrationUndo
	warnings            []string
	carried, untracked  []string
	jjName              string
	// forgotten reports that the jj workspace the slot was is forgotten.
	forgotten bool
	// head is the commit the old checkout is on, and tracked, for a jj slot,
	// what that commit tracks: the clean-up checks the checkout set aside
	// still holds nothing else before it removes it.
	head    string
	tracked []string
	// started is when the migrate's record was first written.
	started time.Time
	// strandedSteps, for a new worktree left set aside, are the clean-up the
	// record keeps.
	strandedSteps []string
	// marked reports that the migrate's record is written (beginMarker);
	// saved, that the manifest records the new mode; stranded, that the new
	// checkout was left set aside (forgetBeside), so the record stays.
	marked, saved, stranded bool
	// kept are the entries whose carried copies the rollback kept in
	// keptDir, the old checkout holding them again, relative to both.
	kept []string
}

// migrationUndo is one thing a migrate did, and how to undo it; what says
// what is left where when the undo fails.
type migrationUndo struct {
	what string
	run  func(context.Context) error
}

// planMigration resolves and checks a migrate of slot to mode to (empty: the
// other of worktree and jj) before anything is touched.
func (p *Platform) planMigration(stack *manifest.Stack, workspaceName, slot, to string) (*migration, error) {
	workspace, ok := stack.Workspaces[workspaceName]
	if !ok {
		return nil, &NotFoundError{Kind: "workspace", Name: workspaceName}
	}
	wsSource, ok := workspace.Sources[slot]
	if !ok {
		return nil, &NotFoundError{Kind: "workspace-source", Name: slot}
	}
	source, ok := stack.Sources[wsSource.Source]
	if !ok {
		return nil, &NotFoundError{Kind: "source", Name: wsSource.Source}
	}
	from := stack.EffectiveSourceMode(wsSource)
	switch to {
	case "", manifest.WorkspaceSourceModeJJ, manifest.WorkspaceSourceModeWorktree:
	default:
		return nil, &InvalidInputError{Field: "to", Reason: fmt.Sprintf("migrate moves a slot to mode %q or %q, not %q", manifest.WorkspaceSourceModeJJ, manifest.WorkspaceSourceModeWorktree, to)}
	}
	if source.Kind != "git" {
		return nil, &InvalidInputError{Field: "slot", Reason: fmt.Sprintf("source slot %q is of %s source %q: migrate moves a slot of a git source's cache between a git worktree and a jj slot", slot, source.Kind, wsSource.Source)}
	}
	if from != manifest.WorkspaceSourceModeJJ && from != manifest.WorkspaceSourceModeWorktree {
		return nil, &InvalidInputError{Field: "slot", Reason: fmt.Sprintf("source slot %q is a %s: migrate moves a slot between a git worktree and a jj slot of its source's cache", slot, modeNoun(from))}
	}
	if to == "" {
		to = manifest.WorkspaceSourceModeJJ
		if from == manifest.WorkspaceSourceModeJJ {
			to = manifest.WorkspaceSourceModeWorktree
		}
	}
	subpath, path, err := p.workspaceSourcePath(workspaceName, slot, wsSource)
	if err != nil {
		return nil, fmt.Errorf("workspace %q source %q: %w", workspaceName, slot, err)
	}
	if subpath == "." {
		return nil, &InvalidInputError{Field: "slot", Reason: fmt.Sprintf("source slot %q is the workspace's own directory, which migrate would have to set aside with everything in it", slot)}
	}
	if nested := nestedSlots(subpath, workspace.Sources, slot); len(nested) > 0 {
		return nil, &InvalidInputError{Field: "slot", Reason: fmt.Sprintf("other slots of workspace %q lie inside source slot %q (%s), and setting it aside would move them too", workspaceName, slot, strings.Join(nested, ", "))}
	}
	return &migration{
		p: p, workspaceName: workspaceName, slot: slot, sourceName: wsSource.Source, source: source,
		wsSource: wsSource, from: from, to: to, path: path, primary: p.sourcePath(wsSource.Source, source),
	}, nil
}

// modeNoun names the kind of checkout an effective mode makes.
func modeNoun(mode string) string {
	switch mode {
	case manifest.WorkspaceSourceModeJJ:
		return "jj slot"
	case manifest.WorkspaceSourceModeWorktree:
		return "git worktree"
	case manifest.WorkspaceSourceModeClone, "":
		return "clone or link"
	}
	return "slot of mode " + mode
}

// migrateToJJ makes the worktree slot m a jj slot (plan phase 9, steps 1 to
// 6), under the cache's lock: the slot's registrations change throughout.
func (p *Platform) migrateToJJ(ctx context.Context, m *migration) (string, error) {
	client := p.gitClient()
	worktree := withEffectiveMode(m.stack, m.wsSource)
	gitDriver, gitSlot := p.workspaceSlotDriver(m.path, m.source, worktree)
	if err := requireSlotReady(ctx, gitDriver, gitSlot, vcs.VerbMigrate, m.workspaceName, m.slot, m.sourceName); err != nil {
		return "", err
	}
	if _, _, err := m.gitEntries(ctx, client, m.path); err != nil {
		return "", err
	}
	if err := p.checkJJPrimary(m.sourceName, m.source, m.primary); err != nil {
		return "", err
	}
	head, err := client.Commit(ctx, m.path, "HEAD")
	if err != nil {
		return "", gitFailure(err, gitStep{action: "reading", object: fmt.Sprintf("source slot %q", m.slot), slot: m.slot, source: m.sourceName})
	}
	jjSource := m.wsSource
	jjSource.Mode, jjSource.VCS = manifest.WorkspaceSourceModeJJ, ""
	sources := map[string]manifest.WorkspaceSource{}
	for name, ws := range m.stack.Workspaces[m.workspaceName].Sources {
		sources[name] = ws
	}
	sources[m.slot] = jjSource
	names, err := p.jjWorkspaceNames(m.stack, m.workspaceName, sources)
	if err != nil {
		return "", err
	}
	jjSlot := p.workspaceLifecycleSlot(m.path, m.source, jjSource)
	jjSlot.Name, m.jjName = names[m.slot], names[m.slot]
	// A slot on no branch is made at its base, so its HEAD must hold nothing
	// the base does not.
	var start string
	if m.wsSource.Branch == "" {
		if start, err = m.jjStartCommit(ctx, client, jjSlot); err != nil {
			return "", err
		}
	}
	jj, ok := p.slotDriver(jjSlot).(jjMigrator)
	if !ok {
		return "", fmt.Errorf("the driver of jj slot %s cannot migrate it", m.path)
	}
	m.head = head
	err = withPrimaryLock(ctx, m.primary, func() error {
		// A cache that is no jj repository yet has no name to be taken, so it
		// is colocated with jj only once nothing refuses the migrate.
		if _, err := os.Lstat(filepath.Join(m.primary, ".jj")); err == nil {
			if err := refuseTakenJJName(ctx, jj, jjSlot); err != nil {
				return err
			}
		}
		if err := jj.EnsurePrimary(ctx, m.primary); err != nil {
			return err
		}
		if err := jj.Import(ctx, m.primary); err != nil {
			return err
		}
		if err := refuseTakenJJName(ctx, jj, jjSlot); err != nil {
			return err
		}
		if err := m.run(ctx, func() error { return m.toJJ(ctx, client, gitDriver, gitSlot, jj, jjSlot, head, start) }); err != nil {
			return err
		}
		m.finishToJJ(ctx, client)
		return nil
	})
	if err := m.lockReleased(ctx, err); err != nil {
		return "", err
	}
	message := fmt.Sprintf("Migrated source slot %q to a jj slot: jj workspace %q of %s at %s", m.slot, jjSlot.Name, m.primary, m.path)
	if m.wsSource.Branch != "" {
		message += fmt.Sprintf(", on bookmark %q", m.wsSource.Branch)
	}
	message += "."
	return m.describe(message), nil
}

// refuseTakenJJName refuses a jj name the primary already has, for another
// checkout: the phase 6 CONFLICT (jjNameTaken). A migrate never reclaims one.
func refuseTakenJJName(ctx context.Context, jj jjMigrator, jjSlot vcs.Slot) error {
	taken, err := jj.Exists(ctx, jjSlot)
	if err != nil || !taken {
		return err
	}
	elsewhere, _ := jj.Elsewhere(ctx, jjSlot)
	return jjNameTaken(jjSlot, elsewhere)
}

// jjStartCommit is the commit a jj slot on no branch starts on, its base as
// jj picks it (ref on the base's remote, else the local ref), for the
// worktree slot at s's path: refused when the worktree's HEAD holds commits
// the base does not, which the jj slot would start without.
func (m *migration) jjStartCommit(ctx context.Context, client git.Client, s vcs.Slot) (string, error) {
	remote := s.BaseRemote
	if remote == "" {
		remote = "origin"
	}
	var start string
	for _, ref := range []string{"refs/remotes/" + remote + "/" + s.BaseRef, "refs/heads/" + s.BaseRef} {
		if client.RefExists(ctx, s.Primary, ref) {
			start = ref
			break
		}
	}
	if start == "" {
		return "", fmt.Errorf("source slot at %s has no base %q to start a jj slot on: %w", s.Path, s.BaseRef, vcs.ErrBaseNotFound)
	}
	beyond, err := client.CountNotOn(ctx, s.Path, start)
	if err != nil {
		return "", err
	}
	if beyond > 0 {
		hint := "Name a branch for the slot in angee.yaml (`branch:`) and put the commits on it, then retry."
		return "", &OperationError{
			Code:    CodePreconditionFailed,
			Cause:   CauseDetachedHead,
			Slot:    m.slot,
			Source:  m.sourceName,
			Paths:   []string{s.Path},
			Hint:    hint,
			Summary: fmt.Sprintf("source slot %q names no branch, and its HEAD holds %d commit(s) beyond its base %s, which a jj slot on no branch would start without. %s", m.slot, beyond, strings.TrimPrefix(start, "refs/"), hint),
		}
	}
	return client.Commit(ctx, s.Primary, start)
}

// toJJ is migrateToJJ's work under the lock, each step recorded for the
// rollback.
func (m *migration) toJJ(ctx context.Context, client git.Client, gitDriver vcs.Driver, gitSlot vcs.Slot, jj jjMigrator, jjSlot vcs.Slot, head, start string) error {
	if err := m.setAside(ctx, client); err != nil {
		return err
	}
	if err := m.step(migrateStepAside); err != nil {
		return err
	}
	// What writes the slot by its path cannot reach the aside, so it is
	// checked again, all of it, and its entries listed, there.
	aside := gitSlot
	aside.Path = m.aside.Path()
	if err := requireSlotReady(ctx, gitDriver, aside, vcs.VerbMigrate, m.workspaceName, m.slot, m.sourceName); err != nil {
		return err
	}
	entries, untracked, err := m.gitEntries(ctx, client, aside.Path)
	if err != nil {
		return err
	}
	m.untracked = untracked
	if now, err := client.Commit(ctx, m.aside.Path(), "HEAD"); err != nil {
		return err
	} else if now != head {
		return m.movedMeanwhile(head, now)
	}
	// Recorded before jj makes anything, so a migrate killed while it does
	// says how to remove what it made. The rollback removes the slot only
	// once Add made it: Add removes what it made when it fails, and forgets
	// no name it found taken.
	added := false
	if err := m.push(fmt.Sprintf("remove what the migrate made of the jj slot at %[1]s, as far as it got, unless it still holds an entry carried into it: its directory, then, with nothing there, its jj workspace, then git's registration of it alone, if git lists one: `%[2]s && jj -R %[3]s workspace forget %[4]s && %[5]s`", shellQuote(m.path), m.removeNewCommand(entries), shellQuote(m.primary), shellQuote(jjSlot.Name), deregisterCommand(m.primary, m.path)), func(ctx context.Context) error {
		if !added {
			return m.nothingMade(deregisterCommand(m.primary, m.path))
		}
		if err := m.refuseCarriedLeft(entries); err != nil {
			return err
		}
		return jj.Remove(ctx, jjSlot, m.dest)
	}); err != nil {
		return err
	}
	if err := m.step(migrateStepAdding); err != nil {
		return err
	}
	if err := jj.Add(ctx, jjSlot, m.dest, nil); err != nil {
		if errors.Is(err, vcs.ErrNameTaken) {
			return errors.Join(jjNameTaken(jjSlot, ""), err)
		}
		return err
	}
	added = true
	if err := m.step(migrateStepAdded); err != nil {
		return err
	}
	want := head
	if start != "" {
		want = start
	}
	if tip, err := jj.Tip(ctx, m.path); err != nil {
		return err
	} else if tip != want {
		return fmt.Errorf("the jj slot at %s starts on %s, not on %s, the commit the worktree slot was on", m.path, shortCommit(tip), shortCommit(want))
	}
	if err := m.carry(entries); err != nil {
		return err
	}
	return m.save()
}

// finishToJJ removes the worktree set aside, once the manifest records the
// jj slot: through its guard, then its git registration alone. A failure is
// a warning, since the slot is migrated.
//
// Something that had the slot open when it was set aside (a shell, an
// editor, a server whose working directory was in it) follows it into the
// aside, so the aside is read again first: it must be on the same commit,
// with no change to a tracked file and nothing git does not track left in
// it, else it is kept, and the warning says what to do with it.
func (m *migration) finishToJJ(ctx context.Context, client git.Client) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), migrateRollbackTimeout)
	defer cancel()
	m.recordCleanup(migratePhaseSaved, []string{m.removeAsideStep()})
	var held []string
	err := m.aside.Verify()
	if err == nil {
		held, err = nestedRepositories(m.aside.Path(), nil, true)
	}
	if err == nil {
		var changed []string
		if changed, err = client.ChangedPaths(ctx, m.aside.Path()); err == nil {
			held = append(held, changed...)
		}
	}
	if err == nil {
		var others []string
		if others, err = client.OtherPaths(ctx, m.aside.Path(), true); err == nil {
			held = append(held, others...)
		}
	}
	if err == nil {
		var now string
		if now, err = client.Commit(ctx, m.aside.Path(), "HEAD"); err == nil && now != m.head {
			err = fmt.Errorf("it moved from %s to %s", shortCommit(m.head), shortCommit(now))
		}
	}
	if err == nil && len(held) == 0 {
		m.removeAside(ctx, client)
		return
	}
	reason := fmt.Sprintf("it holds what was written into it after its files were carried across: %s", vcs.NamePaths(held, maxMessagePaths))
	if err != nil {
		reason = fmt.Sprintf("reading it again found %v", err)
	}
	m.warn(ctx, fmt.Sprintf("the old worktree was kept at %s, since %s; take what you need from it, then remove it with `git -C %s worktree remove --force --force %s`", m.aside.Path(), reason, shellQuote(m.primary), shellQuote(m.aside.Path())))
}

// migrateToWorktree makes the jj slot m a git worktree of its cache (plan
// phase 9, the reverse direction), under the cache's lock.
func (p *Platform) migrateToWorktree(ctx context.Context, m *migration) (string, error) {
	client := p.gitClient()
	jjSource := withEffectiveMode(m.stack, m.wsSource)
	jjSlot := p.workspaceLifecycleSlot(m.path, m.source, jjSource)
	jj, ok := p.slotDriver(jjSlot).(jjMigrator)
	if !ok {
		return "", fmt.Errorf("the driver of jj slot %s cannot migrate it", m.path)
	}
	// First, read without a snapshot: jj reads a directory that holds a
	// repository as empty, so one in a tracked directory would make Ready
	// report its files as deleted.
	if _, _, err := m.jjEntries(ctx, jj, jjSlot, m.path, ""); err != nil {
		return "", err
	}
	if err := requireSlotReady(ctx, jj, jjSlot, vcs.VerbMigrate, m.workspaceName, m.slot, m.sourceName); err != nil {
		return "", err
	}
	named, held := p.heldJJSlot(ctx, jj, m.stack, m.workspaceName, m.slot, jjSlot)
	if !held {
		hint := "Make it the checkout of its jj workspace again, or move it aside and run `angee workspace repair` to make the slot afresh."
		return "", &OperationError{
			Code:    CodePreconditionFailed,
			Cause:   CauseSlotNotHeld,
			Slot:    m.slot,
			Source:  m.sourceName,
			Paths:   []string{m.path},
			Hint:    hint,
			Summary: fmt.Sprintf("source slot %q at %s is no checkout of a jj workspace of %s this stack made, so migrate cannot tell which one to forget. %s", m.slot, m.path, m.primary, hint),
		}
	}
	m.jjName = named.Name
	worktree := m.wsSource
	worktree.Mode, worktree.VCS = manifest.WorkspaceSourceModeWorktree, ""
	gitSlot := p.workspaceLifecycleSlot(m.path, m.source, worktree)
	gitDriver, ok := p.slotDriver(gitSlot).(worktreeStager)
	if !ok {
		return "", fmt.Errorf("the driver of git worktree %s cannot migrate a slot to it", m.path)
	}
	var handoff jjdriver.Handoff
	err := withPrimaryLock(ctx, m.primary, func() error {
		if err := m.run(ctx, func() error {
			var err error
			if handoff, err = m.prepareWorktree(ctx, jj, named); err != nil {
				return err
			}
			return m.toWorktree(ctx, client, jj, gitDriver, named, gitSlot, handoff)
		}); err != nil {
			return err
		}
		m.finishToWorktree(ctx, client, jj, named)
		return nil
	})
	if err := m.lockReleased(ctx, err); err != nil {
		return "", err
	}
	message := fmt.Sprintf("Migrated source slot %q to a git worktree of %s at %s", m.slot, m.primary, m.path)
	if handoff.Branch != "" {
		message += fmt.Sprintf(", on branch %q", handoff.Branch)
		if handoff.Moved {
			message += fmt.Sprintf(", which now points at %s, the commit under the jj slot's working copy", shortCommit(handoff.Tip))
		}
	} else {
		message += fmt.Sprintf(", detached at %s, the commit under the jj slot's working copy", shortCommit(handoff.Tip))
	}
	if m.forgotten {
		message += fmt.Sprintf("; jj workspace %q is forgotten", m.jjName)
	}
	message += "."
	return m.describe(message), nil
}

// heldJJSlot names the jj workspace whose checkout is at slot.Path, of the
// two names this stack could have given the slot (destroyedJJSlot's), by
// HeldChange alone: jj run there loads that workspace, whose working copy is
// the primary's. Unlike destroy, a migrate removes the checkout it reads, so
// a name jj only records at the path does not do.
func (p *Platform) heldJJSlot(ctx context.Context, jj jjMigrator, stack *manifest.Stack, workspaceName, slotName string, slot vcs.Slot) (vcs.Slot, bool) {
	base := stack.Name + "--" + workspaceName
	for _, name := range []string{base, base + "--" + slotName} {
		candidate := slot
		candidate.Name = name
		if change, err := jj.HeldChange(ctx, candidate, slot.Path); err == nil && change != "" {
			return candidate, true
		}
	}
	return slot, false
}

// prepareWorktree readies the jj slot to become a worktree
// (jjdriver.Driver.PrepareWorktree), in the service's words, recording the
// bookmark it set for the rollback.
func (m *migration) prepareWorktree(ctx context.Context, jj jjMigrator, named vcs.Slot) (jjdriver.Handoff, error) {
	handoff, err := jj.PrepareWorktree(ctx, named)
	if err != nil {
		if refusal := readinessRefusal(err, named, vcs.VerbMigrate, m.workspaceName, m.slot, m.sourceName); refusal != nil {
			return handoff, refusal
		}
		if refusal := branchMovedRefusal(err, m.workspaceName, m.slot, m.sourceName, "migrated", "which a git worktree on it would check out instead of the slot's own"); refusal != nil {
			return handoff, refusal
		}
		var taken *vcs.BranchTakenError
		if errors.As(err, &taken) {
			hint := "Switch that worktree to another branch, or give the slot another branch in angee.yaml, then retry."
			return handoff, &OperationError{
				Code:    CodePreconditionFailed,
				Cause:   CauseBranchCheckedOut,
				Slot:    m.slot,
				Source:  m.sourceName,
				Paths:   []string{taken.Worktree},
				Hint:    hint,
				Summary: fmt.Sprintf("source slot %q cannot become a git worktree on its branch: %v, and git checks a branch out in one worktree only. %s", m.slot, taken, hint),
				Err:     err,
			}
		}
		return handoff, toolFailure(err, gitStep{action: "preparing the migrate of", object: fmt.Sprintf("source slot %q", m.slot), slot: m.slot, source: m.sourceName})
	}
	if handoff.Moved {
		restore := fmt.Sprintf("jj -R %s bookmark delete %s", shellQuote(m.primary), shellQuote(`exact:"`+handoff.Branch+`"`))
		if handoff.Previous != "" {
			restore = fmt.Sprintf("jj -R %s bookmark set --allow-backwards -r %s %s", shellQuote(m.primary), handoff.Previous, shellQuote(handoff.Branch))
		}
		what := fmt.Sprintf("if bookmark %q is still at %s, where the migrate set it, set it back: `%s`", handoff.Branch, shortCommit(handoff.Tip), restore)
		if handoff.Resolved {
			what = fmt.Sprintf("bookmark %q, conflicted before, is resolved at %s, which holds every commit it had; leave it", handoff.Branch, shortCommit(handoff.Tip))
		}
		if err := m.push(what, func(ctx context.Context) error {
			err := jj.RestoreBookmark(ctx, named, handoff)
			if errors.Is(err, jjdriver.ErrBookmarkMoved) {
				logctx.From(ctx).Warn(err.Error())
				return nil
			}
			return err
		}); err != nil {
			return handoff, err
		}
	}
	return handoff, nil
}

// toWorktree is migrateToWorktree's work under the lock once the bookmark is
// set, each step recorded for the rollback.
func (m *migration) toWorktree(ctx context.Context, client git.Client, jj jjMigrator, gitDriver worktreeStager, named, gitSlot vcs.Slot, handoff jjdriver.Handoff) error {
	if err := m.setAside(ctx, client); err != nil {
		return err
	}
	if err := m.step(migrateStepAside); err != nil {
		return err
	}
	// What writes the slot by its path cannot reach the aside, so it is
	// checked again, and its entries listed, there.
	aside := named
	aside.Path = m.aside.Path()
	if err := m.aside.Verify(); err != nil {
		return err
	}
	// Before jj snapshots the aside (Ready), as before the migrate.
	tracked, entries, err := m.jjEntries(ctx, jj, named, aside.Path, handoff.Tip)
	if err != nil {
		return err
	}
	m.head, m.tracked = handoff.Tip, tracked
	if err := jj.Ready(ctx, aside, vcs.VerbMigrate); err != nil {
		if refusal := readinessRefusal(err, named, vcs.VerbMigrate, m.workspaceName, m.slot, m.sourceName); refusal != nil {
			return refusal
		}
		return err
	}
	if tip, err := jj.Tip(ctx, aside.Path); err != nil {
		return err
	} else if tip != handoff.Tip {
		return m.movedMeanwhile(handoff.Tip, tip)
	}
	// Recorded before git makes anything, as in toJJ. git makes the worktree
	// in a stage first, which is made here so the record names it
	// (gitdriver.Driver.AddStaged): a migrate killed while git makes it, or
	// before it is installed, leaves it there, registered in the cache.
	stageRoot, err := gitdriver.StageRoot()
	if err != nil {
		return err
	}
	stage := gitdriver.StagePath(stageRoot)
	added := false
	if err := m.push(fmt.Sprintf("remove what the migrate made of the git worktree at %[1]s, as far as it got, unless it still holds an entry carried into it: its directory, then git's registration of it alone, if git lists one, then the worktree git staged it in, with its registration, if git lists one there, and the staging directory: `%[2]s && %[3]s && %[4]s && rm -rf %[5]s`", shellQuote(m.path), m.removeNewCommand(entries), deregisterCommand(m.primary, m.path), deregisterCommand(m.primary, stage), shellQuote(stageRoot)), func(ctx context.Context) error {
		if !added {
			// AddStaged removes the stage root however it ends, unless it
			// never ran.
			if err := os.RemoveAll(stageRoot); err != nil {
				logctx.From(ctx).Warn(fmt.Sprintf("left the staging directory %s: %v", stageRoot, err))
			}
			return m.nothingMade(deregisterCommand(m.primary, m.path))
		}
		if err := m.refuseCarriedLeft(entries); err != nil {
			return err
		}
		if err := m.dest.RemoveAll(); err != nil {
			return err
		}
		return client.DeregisterWorktree(ctx, m.primary, m.path)
	}); err != nil {
		return err
	}
	if err := m.step(migrateStepAdding); err != nil {
		return err
	}
	if err := gitDriver.AddStaged(ctx, gitSlot, m.dest, stageRoot); err != nil {
		return err
	}
	added = true
	if err := m.step(migrateStepAdded); err != nil {
		return err
	}
	if handoff.Branch == "" {
		// A worktree on no branch is cut detached at the base; the jj slot's
		// commit may be another.
		if err := client.CheckoutDetached(ctx, m.path, handoff.Tip); err != nil {
			return err
		}
	}
	if head, err := client.Commit(ctx, m.path, "HEAD"); err != nil {
		return err
	} else if head != handoff.Tip {
		return fmt.Errorf("the git worktree at %s is at %s, not at %s, the commit under the jj slot's working copy", m.path, shortCommit(head), shortCommit(handoff.Tip))
	}
	if err := m.carry(entries); err != nil {
		return err
	}
	return m.save()
}

// finishToWorktree forgets the jj workspace the slot was and removes its
// checkout, set aside, once the manifest records the worktree. A failure is
// a warning, since the slot is migrated.
//
// Something that had the slot open when it was set aside follows it into
// the aside, so the aside is read again first, as finishToJJ does: still the
// jj workspace (HeldChange), on the same commit, with nothing in its working
// copy (Ready) and nothing jj does not track left in it. Else the aside and
// the jj workspace are both kept, and the warning says what to do.
//
// jj forgets a workspace without touching git only while nothing is at the
// path it recorded for it (jjdriver.Driver.ForgetName); with something
// there, `jj workspace forget` removes the git worktree there, which is now
// the slot's new one, and prunes the cache's missing ones. So the name is
// forgotten while the new worktree is set aside for a moment, and only when
// jj recorded the workspace at the slot's path, by the change read from the
// checkout set aside, so a workspace of that name another added meanwhile is
// left alone.
func (m *migration) finishToWorktree(ctx context.Context, client git.Client, jj jjMigrator, named vcs.Slot) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), migrateRollbackTimeout)
	defer cancel()
	m.recordCleanup(migratePhaseSaved, []string{m.forgetStep(named.Name), m.removeAsideStep()})
	held := named
	held.Path = m.aside.Path()
	var change string
	var left []string
	err := m.aside.Verify()
	if err == nil {
		change, err = jj.HeldChange(ctx, held, held.Path)
	}
	if err == nil && change == "" {
		err = fmt.Errorf("it is no longer jj workspace %q", named.Name)
	}
	if err == nil {
		err = jj.Ready(ctx, held, vcs.VerbMigrate)
	}
	if err == nil {
		var tip string
		if tip, err = jj.Tip(ctx, held.Path); err == nil && tip != m.head {
			err = fmt.Errorf("it moved from %s to %s", shortCommit(m.head), shortCommit(tip))
		}
	}
	if err == nil {
		left, err = untrackedEntries(held.Path, m.tracked)
	}
	if err == nil {
		var nested []string
		if nested, err = nestedRepositories(held.Path, nil, true); err == nil {
			left = append(left, nested...)
		}
	}
	if err != nil || len(left) > 0 {
		reason := fmt.Sprintf("it holds what was written into it after its files were carried across: %s", vcs.NamePaths(left, maxMessagePaths))
		if err != nil {
			reason = fmt.Sprintf("reading it again found %v", err)
		}
		m.warn(ctx, fmt.Sprintf("the old jj slot was kept at %s, and jj workspace %q with it, since %s. Take what you need from it, then remove it with `git -C %s worktree remove --force --force %s`, and %s", held.Path, named.Name, reason, shellQuote(m.primary), shellQuote(held.Path), m.forgetHint(named.Name)))
		return
	}
	m.forgetBeside(ctx, jj, named, change)
	m.removeAside(ctx, client)
	if m.stranded {
		// The record keeps what is left: the new worktree to move back.
		steps := m.strandedSteps
		if _, err := os.Lstat(m.aside.Path()); err == nil {
			steps = append(steps, m.removeAsideStep())
		}
		m.recordCleanup(migratePhaseForgetting, steps)
	}
}

// removeAsideStep is the clean-up step that removes the old checkout, set
// aside, by hand: once what is needed is taken from it, through git, which
// drops its registration alone.
func (m *migration) removeAsideStep() string {
	return fmt.Sprintf("take what you need from %[1]s, the old checkout, then remove it, and its registration alone: `git -C %[2]s worktree remove --force --force %[1]s`", shellQuote(m.aside.Path()), shellQuote(m.primary))
}

// forgetStep is the clean-up step that forgets jj workspace name by hand
// (forgetHint).
func (m *migration) forgetStep(name string) string {
	return m.forgetHint(name)
}

// gitEntries lists what the worktree checked out at dir holds that git does
// not track (entries, as git.Client.OtherPaths lists them, the tops of it),
// and, of that, what git does not ignore (untracked), which a jj slot
// records as changes. It refuses a repository nested in the worktree outside
// the directories git ignores whole (refuseNestedRepositories, of
// ignoredEntries): one in a tracked directory is no entry to carry, and jj
// records nothing of one in its working copy, where an untracked one would
// land.
func (m *migration) gitEntries(ctx context.Context, client git.Client, dir string) (entries, untracked []string, err error) {
	if entries, err = client.OtherPaths(ctx, dir, true); err != nil {
		return nil, nil, err
	}
	if untracked, err = client.OtherPaths(ctx, dir, false); err != nil {
		return nil, nil, err
	}
	ignored, err := client.IgnoredPaths(ctx, dir)
	if err != nil {
		return nil, nil, err
	}
	if err := m.refuseNestedRepositories(dir, ignoredEntries(entries, untracked, ignored)); err != nil {
		return nil, nil, err
	}
	return entries, untracked, nil
}

// ignoredEntries are what a migrate to jj moves whole that git ignores
// whole, so jj ignores all of it too: of entries (what a worktree holds that
// git does not track, the tops of it), those none of untracked (what git
// does not ignore) is or lies in; and the directories of ignored (what git
// ignores of it, as git.Client.IgnoredPaths lists it) inside a directory
// entry, so moved with it. git lists a directory there only when it ignores
// all of it, a repository in it included, which is the guarantee: with
// `/vendor/*` and `!/vendor/keep/`, the entry is vendor/, which holds what
// git does not ignore, and vendor/other/ is ignored whole. untracked, which
// git collapses to the entry, rarely names anything in such a directory; it
// is checked for it all the same.
func ignoredEntries(entries, untracked, ignored []string) []string {
	holdsUntracked := func(entry string) bool {
		dir := strings.HasSuffix(entry, "/")
		return slices.ContainsFunc(untracked, func(u string) bool { return u == entry || dir && strings.HasPrefix(u, entry) })
	}
	var whole []string
	for _, entry := range entries {
		if !holdsUntracked(entry) {
			whole = append(whole, entry)
		}
	}
	for _, dir := range ignored {
		if !strings.HasSuffix(dir, "/") || slices.Contains(whole, dir) || holdsUntracked(dir) {
			continue
		}
		if slices.ContainsFunc(entries, func(entry string) bool { return strings.HasSuffix(entry, "/") && strings.HasPrefix(dir, entry) }) {
			whole = append(whole, dir)
		}
	}
	return whole
}

// jjEntries lists, for the jj slot s checked out at dir, the paths commit
// tracks (commit "": the one under its working copy, Tip), without a
// snapshot, and the tops of what the slot holds beyond them
// (untrackedEntries), which a migrate carries across. It refuses a
// repository nested in the slot outside those entries
// (refuseNestedRepositories): it is in a directory that holds tracked
// files, which jj, reading a directory that holds a repository as empty,
// would take for deleted, and is no entry to carry.
func (m *migration) jjEntries(ctx context.Context, jj jjMigrator, s vcs.Slot, dir, commit string) (tracked, entries []string, err error) {
	if commit == "" {
		if commit, err = jj.Tip(ctx, dir); err != nil {
			if refusal := readinessRefusal(err, s, vcs.VerbMigrate, m.workspaceName, m.slot, m.sourceName); refusal != nil {
				return nil, nil, refusal
			}
			return nil, nil, err
		}
	}
	if tracked, err = jj.TrackedPaths(ctx, dir, commit); err != nil {
		return nil, nil, err
	}
	if entries, err = untrackedEntries(dir, tracked); err != nil {
		return nil, nil, err
	}
	if err := m.refuseNestedRepositories(dir, entries); err != nil {
		return nil, nil, err
	}
	return tracked, entries, nil
}

// refuseNestedRepositories refuses a migrate of the slot checked out at dir
// while it holds a repository (a .git or .jj below its own, such as a clone
// made in it; nestedRepositories, which walks dir through os.Root, following
// no link) outside the directories of whole: entries the migrate renames
// into the new checkout whole, so a repository inside one moves with it,
// such as an editable install's clone under .venv or a package's under
// node_modules. Any other is in a directory that holds tracked files, which
// a migrate cannot carry across, or, to jj, in what would be the jj slot's
// working copy, where jj records nothing of it, so whole is then only what
// git ignores. An entry that is itself a .git or .jj is no such directory:
// its repository's files are in the directory above. PRECONDITION_FAILED,
// cause nested_repositories, naming them.
func (m *migration) refuseNestedRepositories(dir string, whole []string) error {
	var carried []string
	for _, entry := range whole {
		name, isDir := strings.CutSuffix(entry, "/")
		if base := path.Base(name); isDir && base != ".git" && base != ".jj" {
			carried = append(carried, name)
		}
	}
	nested, err := nestedRepositories(dir, carried, true)
	if err != nil {
		return fmt.Errorf("source slot %q: look for repositories inside it: %w", m.slot, err)
	}
	if len(nested) == 0 {
		return nil
	}
	where, inside := "in directories that hold tracked files", "a directory that holds no tracked file"
	if m.to == manifest.WorkspaceSourceModeJJ {
		where, inside = "outside the directories git ignores", "a directory git ignores, since jj records nothing of a repository in its working copy"
	}
	hint := "Move them out of the slot, or remove them, then retry."
	return &OperationError{
		Code:    CodePreconditionFailed,
		Cause:   CauseNestedRepositories,
		Slot:    m.slot,
		Source:  m.sourceName,
		Paths:   nested[:min(len(nested), maxErrorPaths)],
		Hint:    hint,
		Summary: fmt.Sprintf("source slot %q holds repositories %s: %s. A migrate carries a repository across only inside %s, which it moves whole. %s", m.slot, where, vcs.NamePaths(nested, maxMessagePaths), inside, hint),
	}
}

// nothingMade is the rollback of a new checkout its tool failed to make:
// the tool removes what it made when it fails (jjdriver.Driver.Add,
// gitdriver's addWorktree), so anything at the slot's path is what it could
// not remove, or what another process wrote there, and the rollback stops
// at it rather than move the old checkout back on top. The record then
// keeps a step that removes nothing there (restepped): what is left is for
// a person to look at, and then deregister, the command that drops git's
// registration at the path alone, runs once nothing is there.
func (m *migration) nothingMade(deregister string) error {
	if _, err := os.Lstat(m.path); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return &restepped{
		err:  fmt.Errorf("%s holds what was left there while the new checkout was being made", m.path),
		step: fmt.Sprintf("look at what %[1]s holds, left there while the new checkout was being made (what its tool could not remove, or what another process wrote there): move out what you need, and remove it; then drop git's registration of it alone, if git lists one: `%[2]s`", shellQuote(m.path), vacantCommand(m.path, deregister)),
	}
}

// restepped is the failure of an undo after which the step that does by
// hand what is left is step, not the undo's own.
type restepped struct {
	err  error
	step string
}

func (e *restepped) Error() string { return e.err.Error() }
func (e *restepped) Unwrap() error { return e.err }

// vacantCommand is the shell command that refuses, naming path, while
// something is at it, a link included, and then runs then: a step that
// moves something to path, or lets git remove what is there, runs only
// once nothing a person may need is in the way.
func vacantCommand(path, then string) string {
	refusal := shellQuote("%s is in the way: move out what you need, and remove it, then run this again\n")
	return fmt.Sprintf("( if %[1]s; then printf %[2]s %[3]s >&2; exit 1; fi ) && %[4]s", existsTest(path), refusal, shellQuote(path), then)
}

// deregisterCommand is the shell command that drops primary's registration
// of a worktree at path, and no other, if git lists one: with nothing at
// path, `git worktree remove` removes only git's record of it; with the
// worktree there, as a staging directory may hold one, the checkout too.
// git lists a worktree by the path it was added at, or that path resolved,
// so either is looked for.
func deregisterCommand(primary, path string) string {
	canonical := vcs.CanonicalPath(path)
	match := "-e " + shellQuote("worktree "+canonical)
	if canonical != path {
		match += " -e " + shellQuote("worktree "+path)
	}
	return fmt.Sprintf("if git -C %[1]s worktree list --porcelain | grep -qxF %[2]s; then git -C %[1]s worktree remove --force --force %[3]s; fi", shellQuote(primary), match, shellQuote(canonical))
}

// forgetHint says how to forget jj workspace name safely by hand while the
// new worktree is at the slot's path, where jj recorded the workspace: jj's
// forget would remove the git worktree it finds there.
func (m *migration) forgetHint(name string) string {
	return fmt.Sprintf("forget the workspace with nothing at %[1]s: `mv %[1]s %[1]s.away && jj -R %[2]s workspace forget %[3]s && mv %[1]s.away %[1]s`", shellQuote(m.path), shellQuote(m.primary), shellQuote(name))
}

// forgetBeside forgets jj workspace named, whose working copy is change,
// with the new worktree at its path set aside for the forget.
func (m *migration) forgetBeside(ctx context.Context, jj jjMigrator, named vcs.Slot, change string) {
	recorded, _, err := jj.Recorded(ctx, named)
	if err != nil || vcs.CanonicalPath(recorded) != vcs.CanonicalPath(m.path) {
		m.warn(ctx, fmt.Sprintf("jj workspace %q was not forgotten: jj records it at %q, not at the slot's path (%v); if nothing uses it, forget it with `jj -R %s workspace forget %s` once nothing is at %q", named.Name, recorded, err, shellQuote(m.primary), shellQuote(named.Name), recorded))
		return
	}
	moved, err := m.dest.SetAside(asidePrefix, m.path)
	if err != nil {
		m.warn(ctx, fmt.Sprintf("jj workspace %q was not forgotten: setting the new worktree aside for it failed (%v); %s", named.Name, err, m.forgetHint(named.Name)))
		return
	}
	m.recordCleanup(migratePhaseForgetting, []string{
		fmt.Sprintf("once nothing is at %[1]s, move the new worktree back from where it was set aside for the forget: `%[2]s`", shellQuote(m.path), vacantCommand(m.path, fmt.Sprintf("mv %s %s", shellQuote(moved.Path()), shellQuote(m.path)))),
		fmt.Sprintf("if `jj -R %s workspace list` still lists %s, %s", shellQuote(m.primary), named.Name, m.forgetStep(named.Name)),
		m.removeAsideStep(),
	})
	if err := jj.ForgetName(ctx, named, change); err != nil {
		m.warn(ctx, fmt.Sprintf("jj workspace %q was not forgotten (%v); %s", named.Name, err, m.forgetHint(named.Name)))
	} else {
		m.forgotten = true
	}
	if err := m.step(migrateStepForgotten); err != nil {
		m.warn(ctx, err.Error())
	}
	if err := m.dest.RestoreAside(moved); err == nil {
		steps := []string{m.removeAsideStep()}
		if !m.forgotten {
			steps = append([]string{m.forgetStep(named.Name)}, steps...)
		}
		m.recordCleanup(migratePhaseSaved, steps)
	} else {
		m.stranded = true
		m.strandedSteps = []string{fmt.Sprintf("once nothing is at %[1]s, move the new worktree back from where it was set aside for the forget: `%[2]s`", shellQuote(m.path), vacantCommand(m.path, fmt.Sprintf("mv %s %s", shellQuote(moved.Path()), shellQuote(m.path))))}
		if !m.forgotten {
			m.strandedSteps = append(m.strandedSteps, m.forgetStep(named.Name))
		}
		m.warn(ctx, fmt.Sprintf("the new worktree was left at %s, not moved back to %s (%v): once nothing is at %s, move it back with `mv %s %s`", moved.Path(), m.path, err, m.path, shellQuote(moved.Path()), shellQuote(m.path)))
	}
}

// lockReleased is err, what the cache's lock returned around the migrate,
// unless the manifest records the migrate already: then only releasing the
// lock failed, which is a warning, since the slot is migrated.
func (m *migration) lockReleased(ctx context.Context, err error) error {
	if err == nil || !m.saved {
		return err
	}
	m.warn(ctx, fmt.Sprintf("releasing the lock of %s failed after the migrate: %v", m.primary, err))
	return nil
}

// run runs work, rolling back what it did when it fails.
func (m *migration) run(ctx context.Context, work func() error) error {
	err := work()
	if err == nil {
		m.undo = nil
		return nil
	}
	return m.rollback(ctx, err)
}

// rollback undoes, in reverse, what the migrate did, on a context of its
// own, so a cancelled request still rolls back. It stops at the first undo
// that fails, since each one before it relies on it (a new checkout removed
// before every entry carried into it is back would delete the rest, so its
// removal refuses while one is there), and says what is left where. One
// that finishes after keeping copies beside what the old checkout held
// again (carryBack) is cause, saying so (keptBeside).
//
// A rollback that stops is its own error, INTERNAL, cause
// migrate_incomplete, naming the slot and where its checkout is: the
// operation's annotation would otherwise report the cause's own words, such
// as a refusal saying the slot was left as it was. The migrate's marker stays
// (beginMarker), so status, repair, destroy and the next migrate say so too.
func (m *migration) rollback(ctx context.Context, cause error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), migrateRollbackTimeout)
	defer cancel()
	for i := len(m.undo) - 1; i >= 0; i-- {
		if err := m.undo[i].run(ctx); err != nil {
			var instead *restepped
			if errors.As(err, &instead) {
				m.undo[i].what = instead.step
			}
			m.undo = m.undo[:i+1]
			left := m.undoSteps()
			if note := m.keptNote(); note != "" {
				left = append(left, note)
			}
			if recordErr := m.record(migratePhaseRollbackStopped, left); recordErr != nil {
				logctx.From(ctx).Warn(recordErr.Error())
			}
			paths := []string{m.path}
			if m.aside != nil {
				paths = append(paths, m.aside.Path())
			}
			hint := recoverySteps(left, m.p.migrateMarkerPath(m.workspaceName, m.slot))
			return &OperationError{
				Code:    CodeInternal,
				Cause:   CauseMigrateIncomplete,
				Slot:    m.slot,
				Source:  m.sourceName,
				Paths:   paths,
				Hint:    hint,
				Summary: fmt.Sprintf("source slot %q was not migrated (%v), and rolling it back stopped (%v). %s", m.slot, cause, err, hint),
				Err:     errors.Join(cause, err),
			}
		}
	}
	m.undo = nil
	if err := m.endMarker(); err != nil {
		logctx.From(ctx).Warn(err.Error())
	}
	if len(m.kept) > 0 {
		return m.keptBeside(cause)
	}
	return cause
}

// push records something done, with how to undo it.
//
// what is the step that undoes it by hand, as an instruction: once the
// migrate's record is written (beginMarker), the record is rewritten with
// every such step, in the order to run them, so a migrate killed at any
// point says how to undo what it did (unfinishedMigrate). A record that
// cannot be written fails the migrate, which then rolls back.
func (m *migration) push(what string, run func(context.Context) error) error {
	return m.pushAll(migrationUndo{what: what, run: run})
}

// pushAll records several undos as push records one, rewriting the record
// once.
func (m *migration) pushAll(undos ...migrationUndo) error {
	m.undo = append(m.undo, undos...)
	if !m.marked || len(undos) == 0 {
		return nil
	}
	return m.record(migratePhaseMigrating, m.undoSteps())
}

// undoSteps are the undo steps of what is done so far, in the order to run
// them by hand: the last thing done is undone first.
func (m *migration) undoSteps() []string {
	steps := make([]string, 0, len(m.undo))
	for i := len(m.undo) - 1; i >= 0; i-- {
		steps = append(steps, m.undo[i].what)
	}
	return steps
}

// step lets a test fail the migrate after step (Platform.migrateFault).
func (m *migration) step(step string) error {
	if m.p.migrateFault == nil {
		return nil
	}
	return m.p.migrateFault(step)
}

// setAside renames the slot aside in its parent and points its git
// registration at it (gitdriver.RepointRegistration, which touches no other
// worktree's, where `git worktree repair` rewrites every worktree of the
// cache and fails over any unrelated broken one), so git does not take it
// for one whose checkout is gone. It writes the migrate's marker first
// (beginMarker), so a migrate that dies with the slot set aside is reported,
// not lost. The rollback renames it back and points the registration back.
func (m *migration) setAside(ctx context.Context, client git.Client) error {
	if err := m.beginMarker(); err != nil {
		return err
	}
	aside, err := m.dest.SetAside(asidePrefix, m.path)
	if err != nil {
		return err
	}
	m.aside = aside
	if err := m.push(fmt.Sprintf("once nothing is at %[1]s, move the slot's checkout back: `%[2]s`", shellQuote(m.path), vacantCommand(m.path, fmt.Sprintf("mv %s %s && %s", shellQuote(aside.Path()), shellQuote(m.path), repairCommand(m.primary, m.path)))), func(ctx context.Context) error {
		if err := m.dest.RestoreAside(aside); err != nil {
			return err
		}
		return repointRegistration(ctx, client, m.primary, m.path)
	}); err != nil {
		return err
	}
	if err := aside.Verify(); err != nil {
		return err
	}
	return repointRegistration(ctx, client, m.primary, aside.Path())
}

// repairCommand is git's command that points primary's registration of the
// worktree moved back to path at it, for a hint: with absolute links, which
// git's repair would otherwise write relative for every worktree of the
// cache under a user's worktree.useRelativePaths.
func repairCommand(primary, path string) string {
	return fmt.Sprintf("git -c worktree.useRelativePaths=false -C %s worktree repair %s", shellQuote(primary), shellQuote(path))
}

// repointRegistration points primary's registration of the worktree now at
// path at it, and checks git lists it there.
func repointRegistration(ctx context.Context, client git.Client, primary, path string) error {
	if err := gitdriver.RepointRegistration(path, path, primary); err != nil {
		return err
	}
	registered, err := client.WorktreeRegistered(ctx, primary, path)
	if err != nil {
		return err
	}
	if !registered {
		return fmt.Errorf("%s does not register the worktree at %s once pointed at it", primary, path)
	}
	return nil
}

// carry renames each entry of the checkout set aside into the new one.
//
// Every entry's undo is recorded before the first is renamed, so no rename
// is ever left out of the record, wherever the migrate is killed. By hand,
// each step acts on what is there, since nothing says what was renamed: an
// entry in the new checkout goes back into the old one, or, when the old
// one holds it again (written into it meanwhile, as a watcher rebuilding
// node_modules does) or still does (never renamed, but written into the new
// checkout by its path), into keptDir, beside both (carryBackStep). The
// rollback moves back only what it renamed, into keptDir the same way
// (carryBack), and stops at anything else left in the new checkout. Either
// way the new checkout is removed only once it holds none of the entries
// (removeNewCommand, refuseCarriedLeft), so nothing written into it is
// deleted with it. A .git or .jj at the top gets no undo: the new checkout
// has its own, so carrying one fails, and its undo would move that one.
func (m *migration) carry(entries []string) error {
	moved := make([]bool, len(entries))
	undos := make([]migrationUndo, 0, len(entries))
	for i, entry := range entries {
		if ownEntry(entry) {
			continue
		}
		rel := strings.TrimSuffix(entry, "/")
		undos = append(undos, migrationUndo{
			what: m.carryBackStep(entry),
			run: func(context.Context) error {
				if !moved[i] {
					return nil
				}
				return m.carryBack(rel)
			},
		})
	}
	if err := m.pushAll(undos...); err != nil {
		return err
	}
	for i, entry := range entries {
		if err := m.dest.MoveFromAside(m.aside, strings.TrimSuffix(entry, "/")); err != nil {
			return fmt.Errorf("carry %s across from %s: %w", entry, m.aside.Path(), err)
		}
		moved[i] = true
		if i == 0 {
			if err := m.step(migrateStepCarried); err != nil {
				return err
			}
		}
		m.carried = append(m.carried, entry)
	}
	return nil
}

// carryBackStep is the step that undoes the carry of entry by hand: if the
// new checkout holds it, it goes back into the old one, or, when the old one
// holds it too, into keptDir, which is given its .gitignore first; with
// something there as well, it is left, and the removal of the new checkout
// refuses.
func (m *migration) carryBackStep(entry string) string {
	rel := strings.TrimSuffix(entry, "/")
	carried, back, kept, ignore := filepath.Join(m.path, rel), filepath.Join(m.aside.Path(), rel), m.keptPath(rel), filepath.Join(m.keptDir(), ".gitignore")
	keep := fmt.Sprintf("mkdir -p %s && { %s || echo %s > %s; } && mv %s %s", shellQuote(filepath.Dir(kept)), existsTest(ignore), shellQuote(strings.TrimSuffix(keptIgnore, "\n")), shellQuote(ignore), shellQuote(carried), shellQuote(kept))
	return fmt.Sprintf("if %[1]s is in the new checkout, move it back into the old one, or, if the old one holds it too, written into it meanwhile, to %[2]s: `if %[3]s; then if %[4]s; then %[5]s || { %[6]s; }; else mv %[7]s %[8]s; fi; fi`", entry, shellQuote(kept), existsTest(carried), existsTest(back), existsTest(kept), keep, shellQuote(carried), shellQuote(back))
}

// carryBack moves the entry rel, renamed into the new checkout, back into
// the old one, as its recorded step does by hand (carryBackStep): into
// keptDir, when the old checkout holds rel again, written into it
// meanwhile, so neither copy is lost.
func (m *migration) carryBack(rel string) error {
	err := m.dest.MoveToAside(m.aside, rel)
	if !errors.Is(err, fs.ErrExist) {
		return err
	}
	if err := m.keep(rel); err != nil {
		return fmt.Errorf("the old checkout holds %s again, written into it meanwhile, and keeping the copy carried across in %s failed: %w", rel, m.keptDir(), err)
	}
	m.kept = append(m.kept, rel)
	return nil
}

// keep moves the entry rel of the new checkout into keptDir, at keptPath,
// writing the directory's .gitignore first, through a guard on the stack
// root, when it keeps nothing yet.
func (m *migration) keep(rel string) error {
	if len(m.kept) == 0 {
		ignore, err := filepath.Rel(m.p.root, filepath.Join(m.keptDir(), ".gitignore"))
		if err != nil {
			return err
		}
		guard, err := copierx.OpenGuardedPath(m.p.root, m.p.root, filepath.ToSlash(ignore), nil)
		if err != nil {
			return err
		}
		err = guard.WriteFile([]byte(keptIgnore), 0o644)
		_ = guard.Close()
		if err != nil {
			return err
		}
	}
	return m.dest.MoveBeside(filepath.Join(filepath.Base(m.keptDir()), filepath.Base(m.path)), rel)
}

// keptDir is where an undo of the migrate keeps the carried copy of an entry
// the old checkout holds again (keptSuffix).
func (m *migration) keptDir() string {
	return m.aside.Path() + keptSuffix
}

// keptPath is where in keptDir the carried copy of the entry rel is kept:
// under the slot's name, as rel is in the slot.
func (m *migration) keptPath(rel string) string {
	return filepath.Join(m.keptDir(), filepath.Base(m.path), filepath.FromSlash(rel))
}

// ownEntry reports whether entry, of what a checkout holds beyond its
// commit, is a .git or .jj at its top, which the new checkout has its own of.
func ownEntry(entry string) bool {
	name := strings.TrimSuffix(entry, "/")
	return name == ".git" || name == ".jj"
}

// existsTest is the shell test that something, a link included, is at path.
func existsTest(path string) string {
	return fmt.Sprintf("[ -e %[1]s ] || [ -L %[1]s ]", shellQuote(path))
}

// removeNewCommand is the shell command that removes the new checkout, at
// the slot's path, by hand: it refuses, naming one, while the checkout holds
// an entry of entries, what the migrate carries across, since by then the
// steps before it have moved back, or kept, every one they could
// (carryBackStep), and `rm -rf` would delete the rest.
func (m *migration) removeNewCommand(entries []string) string {
	remove := "rm -rf " + shellQuote(m.path)
	var rels []string
	for _, entry := range entries {
		if !ownEntry(entry) {
			rels = append(rels, shellQuote(strings.TrimSuffix(entry, "/")))
		}
	}
	if len(rels) == 0 {
		return remove
	}
	refusal := shellQuote("%s still holds %s, which the migrate carries across: move it to %s/%s if nothing is there, or out of the way, then run this again\n")
	return fmt.Sprintf(`( for e in %[1]s; do if [ -e %[2]s/"$e" ] || [ -L %[2]s/"$e" ]; then printf %[3]s %[2]s "$e" %[4]s "$e" >&2; exit 1; fi; done ) && %[5]s`, strings.Join(rels, " "), shellQuote(m.path), refusal, shellQuote(m.keptPath("")), remove)
}

// refuseCarriedLeft refuses to remove the new checkout while it holds an
// entry of entries, what the migrate carries across, as removeNewCommand
// does by hand: by then the rollback has moved back each one it renamed, so
// one still there was written there meanwhile.
func (m *migration) refuseCarriedLeft(entries []string) error {
	var left []string
	for _, entry := range entries {
		if ownEntry(entry) {
			continue
		}
		if _, err := os.Lstat(filepath.Join(m.path, strings.TrimSuffix(entry, "/"))); err == nil {
			left = append(left, entry)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if len(left) == 0 {
		return nil
	}
	return fmt.Errorf("the new checkout at %s holds %s, which the migrate carries across, so removing it would delete them; move each to the same path in %s if nothing is there, or out of the way", m.path, vcs.NamePaths(left, maxMessagePaths), m.keptPath(""))
}

// keptNote says which carried copies the rollback kept in keptDir, the old
// checkout holding those entries again (carryBack); empty when it kept none.
func (m *migration) keptNote() string {
	if len(m.kept) == 0 {
		return ""
	}
	return fmt.Sprintf("the old checkout held %s again when they were moved back, written into it by something still running there (stop the slot's services before a migrate), so the copies that were carried across are kept beside it, in %s: keep what you need of each, and remove the rest", vcs.NamePaths(m.kept, maxMessagePaths), m.keptPath(""))
}

// keptBeside is cause, the failure a migrate rolled back from, with its code
// and cause, also saying which carried copies the rollback kept (keptNote).
func (m *migration) keptBeside(cause error) error {
	kept := *AsOperationError(cause)
	note := "Rolled back, but " + m.keptNote() + "."
	if kept.Summary == "" {
		kept.Summary = cause.Error()
	}
	kept.Summary += " " + note
	kept.Hint = strings.TrimSpace(kept.Hint + " " + note)
	kept.Paths = slices.Clone(kept.Paths)
	for _, rel := range m.kept {
		kept.Paths = append(kept.Paths, m.keptPath(rel))
	}
	kept.Err = cause
	return &kept
}

// save records the slot's new mode through the parent stack transaction:
// its mode, and no vcs, which a slot that names a mode does not read.
func (m *migration) save() error {
	if err := m.step(migrateStepSave); err != nil {
		return err
	}
	workspace := m.stack.Workspaces[m.workspaceName]
	recorded := workspace.Sources[m.slot]
	recorded.Mode, recorded.VCS = m.to, ""
	sources := make(map[string]manifest.WorkspaceSource, len(workspace.Sources))
	for name, ws := range workspace.Sources {
		sources[name] = ws
	}
	sources[m.slot] = recorded
	workspace.Sources = sources
	m.stack.Workspaces[m.workspaceName] = workspace
	// A migrate killed between the write and the record's next rewrite
	// leaves both paths in the record, chosen by what the manifest says.
	if err := m.recordSaving(); err != nil {
		return err
	}
	if err := m.tx.Save(m.stack); err != nil {
		return err
	}
	m.saved = true
	m.recordCleanup(migratePhaseSaved, m.cleanupSteps())
	if err := m.step(migrateStepSaved); err != nil {
		m.warn(context.Background(), err.Error())
	}
	return nil
}

// cleanupSteps is what is left to do by hand once the manifest records the
// new mode, when the migrate stops there: forget the jj workspace the slot
// was (to a worktree), and remove the old checkout.
func (m *migration) cleanupSteps() []string {
	if m.to == manifest.WorkspaceSourceModeWorktree && m.jjName != "" {
		return []string{m.forgetStep(m.jjName), m.removeAsideStep()}
	}
	return []string{m.removeAsideStep()}
}

// removeAside removes the checkout set aside, through its guard, then drops
// its git registration alone (git.Client.DeregisterWorktree), as a jj slot's
// removal does, never pruning the cache's others. A failure is a warning.
func (m *migration) removeAside(ctx context.Context, client git.Client) {
	remove := fmt.Sprintf("`git -C %s worktree remove --force --force %s`", shellQuote(m.primary), shellQuote(m.aside.Path()))
	if err := m.aside.RemoveAll(); err != nil {
		m.warn(ctx, fmt.Sprintf("the old checkout was left at %s (%v); once nothing there is needed, %s removes it and its registration", m.aside.Path(), err, remove))
		return
	}
	if err := client.DeregisterWorktree(ctx, m.primary, m.aside.Path()); err != nil {
		m.warn(ctx, fmt.Sprintf("the old checkout's git registration was left in %s (%v); %s drops it alone", m.primary, err, remove))
	}
}

// warn records a warning for the result's message, and logs it.
func (m *migration) warn(ctx context.Context, text string) {
	m.warnings = append(m.warnings, text)
	logctx.From(ctx).Warn(text)
}

// movedMeanwhile is the CONFLICT of a slot whose commit changed while it was
// being migrated, from was to now: nothing is migrated.
func (m *migration) movedMeanwhile(was, now string) error {
	hint := "Run migrate again."
	return &OperationError{
		Code:    CodeConflict,
		Slot:    m.slot,
		Source:  m.sourceName,
		Paths:   []string{m.path},
		Hint:    hint,
		Summary: fmt.Sprintf("source slot %q moved from %s to %s while it was being migrated, so it was left as it was. %s", m.slot, shortCommit(was), shortCommit(now), hint),
	}
}

// describe ends a migrate's message with what it carried across and what it
// warns of.
func (m *migration) describe(message string) string {
	lines := []string{message}
	if len(m.carried) > 0 {
		lines = append(lines, fmt.Sprintf("Carried %d untracked or ignored entries across: %s.", len(m.carried), vcs.NamePaths(m.carried, maxMessagePaths)))
	}
	if m.to == manifest.WorkspaceSourceModeJJ && len(m.untracked) > 0 {
		lines = append(lines, fmt.Sprintf("jj records every file it does not ignore, so the untracked ones are now changes in the jj slot's working-copy commit: %s.", vcs.NamePaths(m.untracked, maxMessagePaths)))
	}
	if m.wsSource.Mode == "" {
		lines = append(lines, "The slot now names its mode, so `workspace sync-base` and `workspace push` act on it too.")
	}
	for _, warning := range m.warnings {
		lines = append(lines, "Warning: "+warning)
	}
	return strings.Join(lines, "\n")
}

// untrackedEntries lists, relative to the jj slot checked out at dir and
// slash-separated, the tops of what it holds that tracked, the paths its
// commit tracks, does not: a directory none of whose files are tracked is one
// entry, ending in "/", as `git ls-files --others --directory` lists one. The
// slot's own .jj and .git are left out. It walks the slot through os.Root,
// following no link, so a link is an entry of its own. Once jj has found
// nothing to snapshot in the slot (Ready), what it finds are the files jj
// ignores, empty directories, and directories that hold a repository, which
// jj reads as empty (refuseNestedRepositories).
func untrackedEntries(dir string, tracked []string) ([]string, error) {
	files := map[string]bool{}
	dirs := map[string]bool{}
	for _, file := range tracked {
		files[file] = true
		for parent := path.Dir(file); parent != "."; parent = path.Dir(parent) {
			dirs[parent] = true
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	var found []string
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case name == ".":
			return nil
		case name == ".jj" || name == ".git" || files[name]:
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		case entry.IsDir() && dirs[name]:
			return nil
		case entry.IsDir():
			found = append(found, name+"/")
			return fs.SkipDir
		}
		found = append(found, name)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list what %s holds that jj does not track: %w", dir, err)
	}
	return found, nil
}

// shortCommit cuts a commit id to the length jj and git show.
func shortCommit(id string) string { return id[:min(len(id), 12)] }
