package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
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
	"github.com/ang-ee/angee-operator/internal/vcs/jjdriver"
)

// jjLifecycle is what creating and removing a jj slot, and listing a
// primary's jj workspaces, asks of its driver beyond vcs.Driver, which
// jjdriver.Driver answers. A git primary needs none of it, so it is not part
// of vcs.Driver (plan D2).
type jjLifecycle interface {
	vcs.Driver
	EnsurePrimary(ctx context.Context, primary string) error
	Workspaces(ctx context.Context, primary string) ([]jjdriver.ListedWorkspace, error)
	CheckoutAt(ctx context.Context, dir string) (jjdriver.Checkout, error)
	Holds(ctx context.Context, s vcs.Slot, dir string) (bool, error)
	HeldChange(ctx context.Context, s vcs.Slot, dir string) (string, error)
	Recorded(ctx context.Context, s vcs.Slot) (string, bool, error)
	Elsewhere(ctx context.Context, s vcs.Slot) (string, error)
	ForgetName(ctx context.Context, s vcs.Slot, change string) error
}

var _ jjLifecycle = jjdriver.Driver{}

// jjSlotTarget is what creating a jj slot needs beyond its manifest entry.
type jjSlotTarget struct {
	// name is the slot's workspace name in its primary (jjWorkspaceNames).
	name string
	// recorded reports that the stack's saved manifest has the slot under
	// that name, so a taken name whose checkout is gone is this stack's own
	// to forget and add again.
	recorded bool
	// err is why the slot has no name, for an optional slot that only fails
	// itself.
	err error
}

// jjWorkspaceNames names, in its primary, the jj workspace of each jj slot of
// sources, the slots of workspace workspaceName (plan Q2):
// `<stack>--<workspace>`, and `<stack>--<workspace>--<slot>` for each of
// several jj slots of one primary. The names are computed from the manifest
// whenever a slot is created or removed, never stored, so each part must keep
// two names apart: none may be empty, hold "--", or start or end with "-"
// (else stack `a--b`'s workspace `c` and stack `a`'s workspace `b--c` would
// both be `a--b--c`). A slot whose source is not declared is left out.
//
// The names follow from the slots the manifest records, so adding a second
// slot of the same repository to an existing workspace renames the first; its
// old name then reads as a collision (addJJSlot) rather than being forgotten.
func (p *Platform) jjWorkspaceNames(stack *manifest.Stack, workspaceName string, sources map[string]manifest.WorkspaceSource) (map[string]string, error) {
	byPrimary := map[string][]string{}
	for _, slot := range sortedKeys(sources) {
		wsSource := sources[slot]
		source, ok := stack.Sources[wsSource.Source]
		if !ok || stack.EffectiveSourceMode(wsSource) != manifest.WorkspaceSourceModeJJ {
			continue
		}
		primary := filepath.Clean(p.sourcePath(wsSource.Source, source))
		byPrimary[primary] = append(byPrimary[primary], slot)
	}
	if len(byPrimary) == 0 {
		return nil, nil
	}
	if err := checkJJNamePart("stack name", stack.Name); err != nil {
		return nil, err
	}
	if err := checkJJNamePart("workspace name", workspaceName); err != nil {
		return nil, err
	}
	names := map[string]string{}
	for _, slots := range byPrimary {
		for _, slot := range slots {
			name := stack.Name + "--" + workspaceName
			if len(slots) > 1 {
				if err := checkJJNamePart("slot name", slot); err != nil {
					return nil, err
				}
				name += "--" + slot
			}
			names[slot] = name
		}
	}
	return names, nil
}

// sourceJJWorkspaces lists the jj workspaces of primary, a source's cache or
// local store, when it is a jj repository (it has a .jj directory) and this
// machine had jj when the process first looked (probeJJ), and nil
// otherwise, so a source no jj slot uses, or an operator without jj, runs
// nothing and warns of nothing (plan phase 7). A workspace is owned when the
// checkout at the path this stack's manifest gives one of its jj slots of
// primary is that workspace, as HeldChange decides it: jj loads there a
// working copy that lists the name, and is that workspace's working-copy
// change in primary's list. Its path is then that slot's. Any other, the
// repository's own, other stacks' slots, and a slot of this stack whose
// checkout is gone or cannot be read alike, is not owned and has no path:
// jj's record of where a workspace is goes stale once it moves (plan section
// 2), and a name alone does not say whose it is, since a stack of the same
// name sharing the primary computes the same names. A checkout that holds
// two of the names, or a name two checkouts hold (a workspace that `jj
// edit`ed another's working-copy commit), owns none, since which is this
// stack's cannot be told.
//
// It runs one workspace list, and one `jj log` per candidate path whose name
// the list has. A path that cannot be read only leaves its slot not owned;
// the slot's status reports why.
func (p *Platform) sourceJJWorkspaces(ctx context.Context, stack *manifest.Stack, primary string) ([]api.SourceWorkspace, error) {
	if info, err := os.Lstat(filepath.Join(primary, ".jj")); errors.Is(err, fs.ErrNotExist) || err == nil && !info.IsDir() {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if p.probeJJ(ctx).JJ == "" {
		return nil, nil
	}
	jj, ok := p.slotDriver(vcs.Slot{Mode: vcs.ModeJJ}).(jjLifecycle)
	if !ok {
		return nil, fmt.Errorf("the jj driver cannot list the workspaces of %s", primary)
	}
	listed, err := jj.Workspaces(ctx, primary)
	if err != nil {
		return nil, err
	}
	candidates := p.jjSlotCandidates(stack, primary)
	checkouts := map[string]jjdriver.Checkout{}
	heldAt := map[string][]int{}
	for i, w := range listed {
		for _, path := range candidates[w.Name] {
			checkout, read := checkouts[path]
			if !read {
				if checkout, err = jj.CheckoutAt(ctx, path); err != nil {
					if ctx.Err() != nil {
						return nil, ctx.Err()
					}
					logctx.From(ctx).Debug(fmt.Sprintf("could not tell whether %s is jj workspace %q of %s: %v", path, w.Name, primary, err))
				}
				checkouts[path] = checkout
			}
			if checkout.ChangeID != "" && checkout.ChangeID == w.ChangeID && slices.Contains(checkout.Workspaces, w.Name) {
				heldAt[path] = append(heldAt[path], i)
			}
		}
	}
	// A name is owned only when one path holds it, and that path no other.
	heldBy := map[int][]string{}
	for path, held := range heldAt {
		for _, i := range held {
			heldBy[i] = append(heldBy[i], path)
		}
	}
	workspaces := make([]api.SourceWorkspace, len(listed))
	for i, w := range listed {
		workspaces[i] = api.SourceWorkspace{Name: w.Name}
		if at := heldBy[i]; len(at) == 1 && len(heldAt[at[0]]) == 1 {
			workspaces[i].Owned, workspaces[i].Path = true, at[0]
		}
	}
	return workspaces, nil
}

// jjSlotCandidates maps each jj workspace name this stack's manifest could
// have given one of its jj slots of primary to the paths of the slots it
// could be. A slot has the name jjWorkspaceNames computes now, but create
// named it by the jj slots of its primary it resolved then, so it may have
// either `<stack>--<workspace>` or `<stack>--<workspace>--<slot>`, as
// recordedJJSlotTarget and destroyedJJSlot allow for: the longer one when an
// optional sibling was skipped and never recorded, the shorter when a second
// slot was added to the manifest since. Only the shorter can be several
// slots', and only the checkout tells which (sourceJJWorkspaces). Slots of
// other primaries are left out: their checkouts are another repository's,
// and are not read. A workspace whose names jj could not take (an invalid
// part) has no slot under any.
func (p *Platform) jjSlotCandidates(stack *manifest.Stack, primary string) map[string][]string {
	primary = filepath.Clean(primary)
	candidates := map[string][]string{}
	for _, workspaceName := range sortedKeys(stack.Workspaces) {
		sources := stack.Workspaces[workspaceName].Sources
		names, err := p.jjWorkspaceNames(stack, workspaceName, sources)
		if err != nil {
			continue
		}
		base := stack.Name + "--" + workspaceName
		for _, slot := range sortedKeys(names) {
			wsSource := withEffectiveMode(stack, sources[slot])
			if filepath.Clean(p.sourcePath(wsSource.Source, stack.Sources[wsSource.Source])) != primary {
				continue
			}
			_, path, err := p.workspaceSourcePath(workspaceName, slot, wsSource)
			if err != nil {
				continue
			}
			for _, name := range []string{names[slot], base, base + "--" + slot} {
				if !slices.Contains(candidates[name], path) {
					candidates[name] = append(candidates[name], path)
				}
			}
		}
	}
	return candidates
}

// jjSlotTargets names the jj slots among the slots a create resolved (items),
// and checks, before any slot is cut, that this machine can run them and that
// their primaries are where the manifest says: a create with a jj slot it
// cannot make then fails with nothing materialized. An optional jj slot only
// fails itself, and is skipped, when it is the one that cannot be made. A
// name is recorded when the stack's saved manifest already declares the
// workspace with that slot under it, as a declared but never materialized
// workspace does.
func (p *Platform) jjSlotTargets(ctx context.Context, stack *manifest.Stack, workspaceName string, items []workspaceSourceMaterialization) (map[string]jjSlotTarget, error) {
	sources := map[string]manifest.WorkspaceSource{}
	var required []workspaceSourceMaterialization
	for _, item := range items {
		if stack.EffectiveSourceMode(item.resolved) != manifest.WorkspaceSourceModeJJ {
			continue
		}
		sources[item.slot] = item.resolved
		if !item.optional {
			required = append(required, item)
		}
	}
	if len(sources) == 0 {
		return nil, nil
	}
	if len(required) > 0 {
		if err := p.requireJJSlots(ctx); err != nil {
			return nil, err
		}
	}
	for _, item := range required {
		if err := p.checkJJPrimary(item.sourceName, item.source, p.sourcePath(item.sourceName, item.source)); err != nil {
			return nil, err
		}
	}
	names, err := p.jjWorkspaceNames(stack, workspaceName, sources)
	if err != nil && len(required) > 0 {
		return nil, err
	}
	var declared map[string]string
	if workspace, ok := stack.Workspaces[workspaceName]; ok {
		declared, _ = p.jjWorkspaceNames(stack, workspaceName, workspace.Sources)
	}
	targets := map[string]jjSlotTarget{}
	for slot := range sources {
		targets[slot] = jjSlotTarget{name: names[slot], recorded: names[slot] != "" && declared[slot] == names[slot], err: err}
	}
	return targets, nil
}

// recordedJJSlotTarget is the jj workspace of slot, at dest, of a workspace
// the stack's saved manifest records, which repair cuts again. The name the
// manifest computes now can differ from the one create gave the slot (an
// optional sibling of one primary that was skipped counted then, and a slot
// added or removed since counts now), so of the two names this stack could
// have given the slot, the one the primary records at the slot's own path is
// kept (Recorded), so that the slot is added again under it rather than
// beside it. The computed name is used when neither is recorded there.
func (p *Platform) recordedJJSlotTarget(ctx context.Context, stack *manifest.Stack, workspaceName, slot string, source manifest.Source, ws manifest.WorkspaceSource, dest string) (jjSlotTarget, error) {
	names, err := p.jjWorkspaceNames(stack, workspaceName, stack.Workspaces[workspaceName].Sources)
	if err != nil {
		return jjSlotTarget{}, err
	}
	target := jjSlotTarget{name: names[slot], recorded: names[slot] != ""}
	primary := p.sourcePath(ws.Source, source)
	if _, err := os.Lstat(filepath.Join(primary, ".jj")); errors.Is(err, fs.ErrNotExist) {
		return target, nil
	} else if err != nil {
		return jjSlotTarget{}, err
	}
	jj, ok := p.slotDriver(vcs.Slot{Mode: vcs.ModeJJ}).(jjLifecycle)
	if !ok {
		return target, nil
	}
	base := stack.Name + "--" + workspaceName
	for _, name := range []string{target.name, base, base + "--" + slot} {
		recorded, _, err := jj.Recorded(ctx, vcs.Slot{Name: name, Primary: primary, Path: dest})
		if err != nil {
			return jjSlotTarget{}, err
		}
		if recorded != "" && vcs.CanonicalPath(recorded) == vcs.CanonicalPath(dest) {
			target.name = name
			return target, nil
		}
	}
	return target, nil
}

// checkJJNamePart refuses a part of a jj workspace name that would let two
// names meet (jjWorkspaceNames).
func checkJJNamePart(what, part string) error {
	if part != "" && !strings.Contains(part, "--") && !strings.HasPrefix(part, "-") && !strings.HasSuffix(part, "-") {
		return nil
	}
	return &InvalidInputError{Field: "name", Reason: fmt.Sprintf(`%s %q cannot name a jj workspace: jj slots are named <stack>--<workspace>, plus --<slot> when a workspace has two of one repository, so each part must be non-empty, hold no "--", and neither start nor end with "-"`, what, part)}
}

// requireJJSlots refuses to create a jj slot on a machine that cannot run
// one: jj without `workspace add --colocate`, git older than
// git.RelativeWorktreesVersion (2.48), whose extensions.relativeWorktrees
// jj's add sets on the source cache, or Windows (jjdriver.Probe, cached for
// the process). The refusal is a
// precondition, cause jj_unavailable, whose hint says what to install, and
// it matches vcs.ErrUnsupported.
func (p *Platform) requireJJSlots(ctx context.Context) error {
	caps := p.probeJJ(ctx)
	unsupported := &jjdriver.UnsupportedError{}
	var hints []string
	// git's version is judged against the jj slots' floor alone, so a git
	// below the driver's own floor is told the version it actually needs.
	others := caps
	if caps.Git != "" {
		others.GitErr = nil
	}
	var probed *jjdriver.UnsupportedError
	if errors.As(others.Err(), &probed) {
		unsupported.Causes = append(unsupported.Causes, probed.Causes...)
		hints = append(hints, probed.Hint)
	}
	if caps.Git != "" && !git.VersionAtLeast(caps.Git, git.RelativeWorktreesVersion) {
		unsupported.Causes = append(unsupported.Causes, fmt.Errorf("%s is older than %s, which jj slots need: jj's workspace add sets extensions.relativeWorktrees on the source cache", caps.Git, git.RelativeWorktreesVersion))
		hints = append(hints, "Upgrade git to "+git.RelativeWorktreesVersion+" or later.")
	}
	if len(unsupported.Causes) == 0 {
		return nil
	}
	unsupported.Hint = strings.Join(hints, " ")
	return &OperationError{
		Code:    CodePreconditionFailed,
		Cause:   CauseJJUnavailable,
		Hint:    unsupported.Hint,
		Summary: fmt.Sprintf("%v. %s", unsupported, unsupported.Hint),
		Err:     unsupported,
	}
}

// probeJJ reports what this machine offers jj slots: jjdriver.Probe, kept
// for the process, or the probe a test set (Platform.jjProbe).
func (p *Platform) probeJJ(ctx context.Context) jjdriver.Capabilities {
	if p.jjProbe != nil {
		return p.jjProbe(ctx)
	}
	return jjdriver.Probe(ctx)
}

// checkJJPrimary refuses a jj slot whose primary is not where the manifest
// says (plan phase 6 step 8). The slot's .git and .jj lead to the primary by
// path, so a primary only this process sees there, such as one a container
// that does not mount it would clone, makes a slot that works nowhere else.
// No path is translated:
//   - a git source's cache inside the stack root is reachable wherever the
//     stack is, since the operator's container mounts the stack root at the
//     same path, so a missing one is cloned as for any slot;
//   - one outside it, or a cache_path that is a symlink to nothing, must be
//     there already;
//   - a local source must be a jj repository already: angee never makes one.
func (p *Platform) checkJJPrimary(sourceName string, source manifest.Source, primary string) error {
	_, err := os.Stat(primary)
	switch {
	case err == nil && source.Kind == "local":
		if _, err := os.Lstat(filepath.Join(primary, ".jj")); errors.Is(err, fs.ErrNotExist) {
			hint := "Make it one with `jj git init` in it (add --colocate to keep git working there), or give the slot the mode worktree or clone, which link a local source."
			return &OperationError{Code: CodePreconditionFailed, Source: sourceName, Paths: []string{primary}, Hint: hint,
				Summary: fmt.Sprintf("local source %q at %s is not a jj repository, and angee never makes one. %s", sourceName, primary, hint)}
		} else if err != nil {
			return err
		}
		return nil
	case err == nil:
		return nil
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	// A missing cache inside the stack root is cloned; one a symlink takes
	// out of the stack root is not, nor is a symlink there that leads
	// nowhere, since the clone would land where it leads.
	inside := vcs.PathInside(p.root, primary)
	if _, lstatErr := os.Lstat(primary); source.Kind == "git" && inside && lstatErr != nil {
		return nil
	}
	hint := fmt.Sprintf("Where angee runs in a container, mount %s at that same path; on the host, run `angee source fetch %s` first.", primary, sourceName)
	if source.Kind == "local" {
		hint = fmt.Sprintf("Where angee runs in a container, mount %s at that same path.", primary)
	}
	return &OperationError{Code: CodePreconditionFailed, Source: sourceName, Paths: []string{primary}, Hint: hint,
		Summary: fmt.Sprintf("source %q of a jj slot is not at %s, the path its slots record: a jj slot leads to its primary by that path, so angee does not make one elsewhere. %s", sourceName, primary, hint)}
}

// materializeJJSlot cuts the jj slot ws at dest through destGuard, under its
// primary's lock (plan phase 6 step 3). populated reports that dest holds a
// non-empty directory, which only --sync lets through. It reports kept when
// the slot was there already and is left as it is.
func (p *Platform) materializeJJSlot(ctx context.Context, destGuard *copierx.GuardedPath, sourceName string, source manifest.Source, ws manifest.WorkspaceSource, dest string, target jjSlotTarget, populated, sync bool) (vcs.Driver, vcs.Slot, bool, error) {
	slot := p.workspaceLifecycleSlot(dest, source, ws)
	slot.Name = target.name
	driver := p.slotDriver(slot)
	switch {
	case target.err != nil:
		return nil, slot, false, target.err
	case slot.Name == "":
		return nil, slot, false, fmt.Errorf("jj slot %s has no workspace name", dest)
	}
	insideDest, err := sameOrNestedPath(dest, slot.Primary)
	if err != nil {
		return nil, slot, false, err
	}
	if insideDest {
		return nil, slot, false, fmt.Errorf("workspace source path %s cannot be inside destination %s", slot.Primary, dest)
	}
	if err := p.requireJJSlots(ctx); err != nil {
		return nil, slot, false, err
	}
	if err := p.checkJJPrimary(sourceName, source, slot.Primary); err != nil {
		return nil, slot, false, err
	}
	kept := false
	err = withPrimaryLock(ctx, slot.Primary, func() error {
		var err error
		kept, err = p.addJJSlot(ctx, driver, slot, destGuard, sourceName, source, target.recorded, populated, sync)
		return err
	})
	return driver, slot, kept, err
}

// addJJSlot settles what is at a jj slot's path, and adds the slot, under its
// primary's lock (plan phase 6 step 4). A git source's cache is refreshed and
// colocated with jj first; a local store is never changed. Then, by what the
// primary has under the slot's name (Exists) and what the path holds:
//   - the name is taken and the path holds its checkout (Holds, --sync only):
//     the slot is kept as it is, its working copy and all, and kept is true.
//     It is never forgotten and added again, which would detach it from its
//     working-copy commit;
//   - the name is taken and the path holds nothing, or an empty directory:
//     the name is forgotten and the slot added again when the saved manifest
//     records the slot and no live checkout elsewhere has the name
//     (Elsewhere); otherwise the name is another's, and refused (D13);
//   - the path holds anything else: refused;
//   - otherwise the slot is added.
func (p *Platform) addJJSlot(ctx context.Context, driver vcs.Driver, slot vcs.Slot, dest *copierx.GuardedPath, sourceName string, source manifest.Source, recorded, populated, sync bool) (bool, error) {
	jj, ok := driver.(jjLifecycle)
	if !ok {
		return false, fmt.Errorf("the driver of jj slot %s cannot make jj workspaces", slot.Path)
	}
	if source.Kind == "git" {
		if err := p.materializeSourceLocked(ctx, sourceName, source, true); err != nil {
			return false, err
		}
		if err := jj.EnsurePrimary(ctx, slot.Primary); err != nil {
			return false, err
		}
	}
	taken, err := jj.Exists(ctx, slot)
	if err != nil {
		return false, err
	}
	switch {
	case populated && (!sync || !taken):
		return false, fmt.Errorf("workspace source destination %s already exists and is not empty", slot.Path)
	case populated:
		held, err := jj.Holds(ctx, slot, slot.Path)
		if err != nil {
			return false, err
		}
		if !held {
			return false, fmt.Errorf("workspace source destination %s already exists and is not the checkout of jj workspace %q of %s", slot.Path, slot.Name, slot.Primary)
		}
		logctx.From(ctx).Info(fmt.Sprintf("keeping jj slot %s as it is: it is already jj workspace %q of %s", slot.Path, slot.Name, slot.Primary))
		return true, nil
	case taken:
		if err := p.reclaimJJName(ctx, jj, slot, recorded); err != nil {
			return false, err
		}
		// The slot's checkout is gone: its name is this stack's own, left
		// behind, so forget it and add the slot again.
		if err := jj.Remove(ctx, slot, dest); err != nil {
			return false, err
		}
	}
	if err := jj.Add(ctx, slot, dest, nil); err != nil {
		if errors.Is(err, vcs.ErrNameTaken) {
			return false, errors.Join(jjNameTaken(slot, ""), err)
		}
		return false, err
	}
	return false, nil
}

// reclaimJJName decides whether the taken name of a jj slot whose own path
// holds nothing, or an empty directory, may be forgotten and the slot added
// again. Only when the saved manifest records the slot under that name, and
// jj's record of the name's path is the slot's own or leads nowhere else
// (Elsewhere, which fails closed). A record of another path with nothing at
// it is this stack's own only when the primary lies inside the stack root,
// symlinks resolved, which no other stack's slots use; for a shared primary
// it may be another stack's checkout out of this process's sight, as from a
// container, so it is refused with the forget command for whoever knows.
func (p *Platform) reclaimJJName(ctx context.Context, jj jjLifecycle, slot vcs.Slot, recorded bool) error {
	elsewhere, err := jj.Elsewhere(ctx, slot)
	unresolved := errors.Is(err, jjdriver.ErrRootUnresolved)
	switch {
	case err != nil && !unresolved:
		return err
	case elsewhere != "" || !recorded:
		return jjNameTaken(slot, elsewhere)
	case unresolved && !vcs.PathInside(p.root, slot.Primary):
		// Inside the stack root once symlinks are resolved: a cache_path
		// that a symlink takes to a shared directory is shared.
		return jjNameTaken(slot, "")
	}
	return nil
}

// jjNameTaken is the CONFLICT of a jj slot whose workspace name its primary
// already has for a checkout this stack's manifest does not account for
// (plan D13): another stack of the same name sharing the source, a checkout
// at elsewhere when that is known, or a leftover of an earlier create of this
// workspace. The name is not forgotten, since that would delete the other
// checkout's .git.
func jjNameTaken(slot vcs.Slot, elsewhere string) error {
	where := "a checkout this stack's manifest does not record"
	paths := []string{slot.Primary}
	if elsewhere != "" {
		where = "the checkout at " + elsewhere
		paths = append(paths, elsewhere)
	}
	hint := fmt.Sprintf("Give one of the stacks that share it another name, or, if nothing uses the workspace, forget it with `jj -R %s workspace forget %s` and retry.", shellQuote(slot.Primary), shellQuote(slot.Name))
	return &OperationError{
		Code:    CodeConflict,
		Paths:   paths,
		Hint:    hint,
		Summary: fmt.Sprintf("jj workspace %q of %s, which jj slot %s would be, belongs to %s. %s", slot.Name, slot.Primary, slot.Path, where, hint),
	}
}

// removeJJSlotUnlocked removes a jj slot through dest without its primary's
// lock, for a rollback or destroy that cannot take it, and frees its name,
// which the primary's next prune does not do for jj. The slot's working-copy
// change is read first (HeldChange), and the name forgotten after the
// directory is gone only while it still has that change: with the directory
// gone, the forget changes only jj's own record (jjdriver.Driver.ForgetName),
// so it needs no lock, and a workspace of the same name the lock's holder
// added meanwhile is left alone. A path that is not the name's checkout is
// removed with its name left as it is. The jj calls run on a context of
// their own, since the lock's wait may have used up the caller's.
func removeJJSlotUnlocked(ctx context.Context, driver vcs.Driver, slot vcs.Slot, dest *copierx.GuardedPath) error {
	jj, ok := driver.(jjLifecycle)
	if !ok || slot.Name == "" {
		return dest.RemoveAll()
	}
	jjCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackForgetTimeout)
	defer cancel()
	change, err := jj.HeldChange(jjCtx, slot, slot.Path)
	if err != nil {
		change = ""
		logctx.From(ctx).Warn(fmt.Sprintf("could not tell whether %s is jj workspace %q of %s: %v", slot.Path, slot.Name, slot.Primary, err))
	}
	if err := dest.RemoveAll(); err != nil {
		return err
	}
	if change == "" {
		logctx.From(ctx).Warn(fmt.Sprintf("removed %s without forgetting jj workspace %q of %s, whose checkout it was not", slot.Path, slot.Name, slot.Primary))
		return nil
	}
	if err := jj.ForgetName(jjCtx, slot, change); err != nil {
		return fmt.Errorf("forget jj workspace %q of %s, so its name stays taken until `jj -R %s workspace forget %s`: %w", slot.Name, slot.Primary, shellQuote(slot.Primary), shellQuote(slot.Name), err)
	}
	return nil
}

// destroyedJJSlot names the jj workspace of the slot at slot.Path, for a
// destroy that is about to forget it: the name the manifest computes now
// (jjWorkspaceNames) can differ from the one the slot was made under, as when
// an optional sibling of one primary was skipped at create, or a slot was
// added or removed since. So of the two names this stack could have given
// the slot, the one forgotten is the one whose checkout the path is
// (HeldChange), else the one jj recorded at the slot's own path (Recorded),
// as for a slot whose directory was emptied. Forgetting the latter is safe:
// the driver removes the directory first, so the forget touches only jj's
// record, and no other checkout can be at this stack's own path. With
// neither, the slot is removed from disk only.
func (p *Platform) destroyedJJSlot(ctx context.Context, jj jjLifecycle, stack *manifest.Stack, workspaceName, slotName string, slot vcs.Slot) (vcs.Slot, bool) {
	base := stack.Name + "--" + workspaceName
	candidates := []string{base, base + "--" + slotName}
	for _, name := range candidates {
		candidate := slot
		candidate.Name = name
		if change, err := jj.HeldChange(ctx, candidate, slot.Path); err == nil && change != "" {
			return candidate, true
		}
	}
	for _, name := range candidates {
		candidate := slot
		candidate.Name = name
		if recorded, _, err := jj.Recorded(ctx, candidate); err == nil && recorded != "" && vcs.CanonicalPath(recorded) == vcs.CanonicalPath(slot.Path) {
			return candidate, true
		}
	}
	return slot, false
}

// rollbackForgetTimeout bounds removeJJSlotUnlocked's jj calls.
const rollbackForgetTimeout = 30 * time.Second

// errJJSlotNotYet is what a verb phase 8 of the jj plan has not implemented
// on a jj slot wraps. It matches vcs.ErrUnsupported, as a verb a slot's
// driver does not support does.
var errJJSlotNotYet = fmt.Errorf("not supported on a jj slot yet: %w", vcs.ErrUnsupported)

// jjSlotNotYet refuses verb on the jj slots of workspace named in slots, with
// their paths, before any of them is touched. Phase 8 of the jj plan
// replaces these refusals verb by verb. Like a verb a slot's driver does not
// support, it is the caller's input at fault (INVALID_INPUT); its cause tells
// a client that the slot is a jj slot. Every verb on a slot asks this before
// it runs anything, so a jj slot never gets a git command, and an aggregate
// verb never acts on some slots and skips its jj slots.
func jjSlotNotYet(verb, workspace string, slots, paths []string) *OperationError {
	hint := "Run jj in the slot instead."
	quoted := make([]string, len(slots))
	for i, slot := range slots {
		quoted[i] = fmt.Sprintf("%q", slot)
	}
	noun := "jj slot"
	if len(slots) > 1 {
		noun = "jj slots"
	}
	opErr := &OperationError{
		Code:      CodeInvalidInput,
		Cause:     CauseJJSlotUnsupported,
		Workspace: workspace,
		Paths:     paths,
		Hint:      hint,
		Summary:   fmt.Sprintf("%s is not supported on %s %s yet. %s", verb, noun, strings.Join(quoted, ", "), hint),
		Err:       errJJSlotNotYet,
	}
	if len(slots) == 1 {
		opErr.Slot = slots[0]
	}
	return opErr
}

// refuseJJSlots refuses verb, an aggregate over every slot of workspace, when
// any of its slots is a jj slot: such a verb would otherwise skip it, or run
// git in it.
func (p *Platform) refuseJJSlots(verb, workspaceName string, workspace manifest.Workspace, stack *manifest.Stack) error {
	var slots, paths []string
	for _, slot := range sortedKeys(workspace.Sources) {
		wsSource := workspace.Sources[slot]
		if stack.EffectiveSourceMode(wsSource) != manifest.WorkspaceSourceModeJJ {
			continue
		}
		_, path, _ := p.workspaceSourcePath(workspaceName, slot, wsSource)
		slots = append(slots, slot)
		if path != "" {
			paths = append(paths, path)
		}
	}
	if len(slots) == 0 {
		return nil
	}
	return jjSlotNotYet(verb, workspaceName, slots, paths)
}

// maxNestedShown caps how many nested repositories a destroy refusal names.
const maxNestedShown = 10

// nestedRepositories lists, relative to the jj slot at path, the repositories
// inside it: every .jj directory and every .git (a clone's directory, or the
// file a git worktree or submodule checkout has) below the slot's own. jj
// neither records nor lists such a directory, so the slot's status reads
// clean and pushed over the work it holds, and only the destroy guard, which
// would delete it, looks (owner, 2026-10-29; plan Q10). It walks the slot
// through os.Root, following no link and skipping nothing else, and does not
// descend into the repository metadata it finds. An entry of either name
// counts whatever it is, a link included. The slot's own .jj is skipped, and
// its own .git only when ownGit, a slot colocated with git: a .git at the
// top of a slot that is not is a repository someone made there. skip lists
// the paths, relative to the slot, of the workspace's other slots nested in
// it, which are checkouts the guard reads on their own.
func nestedRepositories(path string, skip []string, ownGit bool) ([]string, error) {
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	var found []string
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		base := entry.Name()
		switch {
		case entry.IsDir() && slices.Contains(skip, name):
			return fs.SkipDir
		case name == "." || base != ".git" && base != ".jj":
			return nil
		case name == ".jj" || name == ".git" && ownGit:
			// The slot's own .jj, and the .git that colocates it.
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		found = append(found, filepath.FromSlash(name))
		if entry.IsDir() {
			return fs.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("%w; make it readable or remove it, since jj does not record what a repository there holds", err)
	}
	return found, nil
}

// nestedSlots lists the paths, relative to the slot at subpath, of the other
// slots of sources that lie inside it, slash-separated as fs.WalkDir names
// them.
func nestedSlots(subpath string, sources map[string]manifest.WorkspaceSource, self string) []string {
	var nested []string
	for slot, ws := range sources {
		if slot == self {
			continue
		}
		other := ws.Subpath
		if other == "" {
			other = slot
		}
		other, err := normalizeWorkspaceSubpath(other)
		if err != nil {
			continue
		}
		switch {
		case subpath == ".":
			nested = append(nested, other)
		case strings.HasPrefix(other, subpath+"/"):
			nested = append(nested, strings.TrimPrefix(other, subpath+"/"))
		}
	}
	return nested
}

// nestedRepositoriesReason says why a destroy keeps a jj slot that holds
// nested repositories, naming up to maxNestedShown of them.
func nestedRepositoriesReason(nested []string) string {
	shown := strings.Join(nested[:min(len(nested), maxNestedShown)], ", ")
	if more := len(nested) - maxNestedShown; more > 0 {
		shown += fmt.Sprintf(" and %d more", more)
	}
	return "repositories inside it that jj does not record: " + shown
}
