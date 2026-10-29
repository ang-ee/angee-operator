package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
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

// The steps after which a test fails a migrate (Platform.migrateFault), each
// named for what has just been done. A failure at any of them, or in any
// action between them, rolls the migrate back.
const (
	// migrateStepAside: the slot is set aside, its git registration repaired.
	migrateStepAside = "aside"
	// migrateStepAdded: the new checkout is at the slot's path.
	migrateStepAdded = "added"
	// migrateStepCarried: the first recorded entry is carried across, when
	// there is one.
	migrateStepCarried = "carried"
	// migrateStepSave: every entry is carried across, and the manifest is
	// written next.
	migrateStepSave = "save"
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
// reverse: the entries carried across go back, the new checkout is removed,
// the slot is renamed back and its registration repaired, and a bookmark set
// is put back, so the slot is as it was. A rollback step that fails stops
// the rollback, since the ones after it would delete what it did not put
// back, and the error says where each thing is. A failure after the manifest
// is written leaves the slot migrated, and the result's message warns of it.
//
// A slot at the workspace's root, or one other slots lie inside, is refused:
// setting it aside would move them too. Anything that writes the slot by
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
	if reason, unfinished := p.unfinishedMigrate(workspaceName, slot, m.path); unfinished {
		return api.GitOpResult{}, p.migrateIncomplete(workspaceName, slot, m.sourceName, m.path, reason)
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
	// marked reports that the migrate's record is written (beginMarker);
	// saved, that the manifest records the new mode; stranded, that the new
	// checkout was left set aside (forgetBeside), so the record stays.
	marked, saved, stranded bool
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
		if err := m.run(ctx, func() error { return m.toJJ(ctx, client, jj, jjSlot, head, start) }); err != nil {
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
func (m *migration) toJJ(ctx context.Context, client git.Client, jj jjMigrator, jjSlot vcs.Slot, head, start string) error {
	if err := m.setAside(ctx, client); err != nil {
		return err
	}
	if err := m.step(migrateStepAside); err != nil {
		return err
	}
	// What writes the slot by its path cannot reach the aside, so it is
	// checked again, and its entries listed, there.
	if paths, err := client.ChangedPaths(ctx, m.aside.Path()); err != nil {
		return err
	} else if len(paths) > 0 {
		return uncommittedChanges(paths, "", fmt.Sprintf("source slot %q", m.slot), m.slot, m.sourceName)
	}
	if now, err := client.Commit(ctx, m.aside.Path(), "HEAD"); err != nil {
		return err
	} else if now != head {
		return m.movedMeanwhile(head, now)
	}
	entries, err := client.OtherPaths(ctx, m.aside.Path(), true)
	if err != nil {
		return err
	}
	if m.untracked, err = client.OtherPaths(ctx, m.aside.Path(), false); err != nil {
		return err
	}
	if err := jj.Add(ctx, jjSlot, m.dest, nil); err != nil {
		if errors.Is(err, vcs.ErrNameTaken) {
			return errors.Join(jjNameTaken(jjSlot, ""), err)
		}
		return err
	}
	m.push("the jj slot made at "+m.path, func(ctx context.Context) error {
		return jj.Remove(ctx, jjSlot, m.dest)
	})
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
	var held []string
	err := m.aside.Verify()
	if err == nil {
		held, err = client.ChangedPaths(ctx, m.aside.Path())
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
	gitDriver := p.slotDriver(gitSlot)
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
		m.push(fmt.Sprintf("bookmark %q, now at %s", handoff.Branch, shortCommit(handoff.Tip)), func(ctx context.Context) error {
			err := jj.RestoreBookmark(ctx, named, handoff)
			if errors.Is(err, jjdriver.ErrBookmarkMoved) {
				logctx.From(ctx).Warn(err.Error())
				return nil
			}
			return err
		})
	}
	return handoff, nil
}

// toWorktree is migrateToWorktree's work under the lock once the bookmark is
// set, each step recorded for the rollback.
func (m *migration) toWorktree(ctx context.Context, client git.Client, jj jjMigrator, gitDriver vcs.Driver, named, gitSlot vcs.Slot, handoff jjdriver.Handoff) error {
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
	tracked, err := jj.TrackedPaths(ctx, aside.Path, handoff.Tip)
	if err != nil {
		return err
	}
	m.head, m.tracked = handoff.Tip, tracked
	if err := m.aside.Verify(); err != nil {
		return err
	}
	entries, err := untrackedEntries(aside.Path, tracked)
	if err != nil {
		return err
	}
	if err := gitDriver.Add(ctx, gitSlot, m.dest, nil); err != nil {
		return err
	}
	m.push("the git worktree made at "+m.path, func(ctx context.Context) error {
		if err := m.dest.RemoveAll(); err != nil {
			return err
		}
		return client.DeregisterWorktree(ctx, m.primary, m.path)
	})
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
	if err := jj.ForgetName(ctx, named, change); err != nil {
		m.warn(ctx, fmt.Sprintf("jj workspace %q was not forgotten (%v); %s", named.Name, err, m.forgetHint(named.Name)))
	} else {
		m.forgotten = true
	}
	if err := m.step(migrateStepForgotten); err != nil {
		m.warn(ctx, err.Error())
	}
	if err := m.dest.RestoreAside(moved); err != nil {
		m.stranded = true
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
// before every entry carried into it is back would delete the rest), and
// says what is left where.
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
			var left []string
			for _, undo := range m.undo[:i+1] {
				left = append(left, undo.what)
			}
			paths := []string{m.path}
			if m.aside != nil {
				paths = append(paths, m.aside.Path())
			}
			hint := fmt.Sprintf("Put the slot's checkout back at %s by hand, then delete %s.", m.path, m.p.migrateMarkerPath(m.workspaceName, m.slot))
			return &OperationError{
				Code:    CodeInternal,
				Cause:   CauseMigrateIncomplete,
				Slot:    m.slot,
				Source:  m.sourceName,
				Paths:   paths,
				Hint:    hint,
				Summary: fmt.Sprintf("source slot %q was not migrated (%v), and rolling it back stopped (%v), so these are left as they are: %s. %s", m.slot, cause, err, strings.Join(left, "; "), hint),
				Err:     errors.Join(cause, err),
			}
		}
	}
	m.undo = nil
	if err := m.endMarker(); err != nil {
		logctx.From(ctx).Warn(err.Error())
	}
	return cause
}

// push records something done, with how to undo it.
func (m *migration) push(what string, run func(context.Context) error) {
	m.undo = append(m.undo, migrationUndo{what: what, run: run})
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
	m.push(fmt.Sprintf("the slot's checkout, set aside at %s (move it back to %s, then point its registration back with `%s`)", aside.Path(), m.path, repairCommand(m.primary, m.path)), func(ctx context.Context) error {
		if err := m.dest.RestoreAside(aside); err != nil {
			return err
		}
		return repointRegistration(ctx, client, m.primary, m.path)
	})
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

// carry renames each recorded entry of the checkout set aside into the new
// one, recording each for the rollback, which puts it back.
func (m *migration) carry(entries []string) error {
	for i, entry := range entries {
		rel := strings.TrimSuffix(entry, "/")
		if err := m.dest.MoveFromAside(m.aside, rel); err != nil {
			return fmt.Errorf("carry %s across from %s: %w", entry, m.aside.Path(), err)
		}
		m.push(fmt.Sprintf("%s, carried into %s", entry, m.path), func(context.Context) error {
			return m.dest.MoveToAside(m.aside, rel)
		})
		m.carried = append(m.carried, entry)
		if i == 0 {
			if err := m.step(migrateStepCarried); err != nil {
				return err
			}
		}
	}
	return nil
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
	if err := m.tx.Save(m.stack); err != nil {
		return err
	}
	m.saved = true
	return nil
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
// following no link, so a link is an entry of its own. jj refused the
// untracked files it would not snapshot before this runs, so what it finds
// are the files jj ignores, empty directories, and repositories nested in
// the slot.
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
