package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ang-ee/angee-operator/internal/copierx"
	"github.com/ang-ee/angee-operator/internal/logctx"
	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/vcs"
	"github.com/ang-ee/angee-operator/internal/vcs/jjdriver"
)

// jjLifecycle is what creating and removing a jj slot asks of its driver
// beyond vcs.Driver, which jjdriver.Driver answers. A git primary needs none
// of it, so it is not part of vcs.Driver (plan D2).
type jjLifecycle interface {
	vcs.Driver
	EnsurePrimary(ctx context.Context, primary string) error
	Holds(ctx context.Context, s vcs.Slot, dir string) (bool, error)
	Elsewhere(ctx context.Context, s vcs.Slot) (string, error)
	ForgetName(ctx context.Context, s vcs.Slot) error
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

// recordedJJSlotTarget is the jj workspace of slot of a workspace the stack's
// saved manifest records, which repair cuts again.
func (p *Platform) recordedJJSlotTarget(stack *manifest.Stack, workspaceName, slot string) (jjSlotTarget, error) {
	names, err := p.jjWorkspaceNames(stack, workspaceName, stack.Workspaces[workspaceName].Sources)
	if err != nil {
		return jjSlotTarget{}, err
	}
	return jjSlotTarget{name: names[slot], recorded: names[slot] != ""}, nil
}

// checkJJNamePart refuses a part of a jj workspace name that would let two
// names meet (jjWorkspaceNames).
func checkJJNamePart(what, part string) error {
	if part != "" && !strings.Contains(part, "--") && !strings.HasPrefix(part, "-") && !strings.HasSuffix(part, "-") {
		return nil
	}
	return &InvalidInputError{Field: "name", Reason: fmt.Sprintf(`%s %q cannot name a jj workspace: jj slots are named <stack>--<workspace>, plus --<slot> when a workspace has two of one repository, so each part must be non-empty, hold no "--", and neither start nor end with "-"`, what, part)}
}

// jjSlotMinGit is the oldest git a jj slot runs on: jj's `workspace add
// --colocate` sets extensions.relativeWorktrees on the source cache, which
// older git refuses, for the cache and its git worktree slots alike.
const jjSlotMinGit = "2.48"

const jjSlotMinGitMajor, jjSlotMinGitMinor = 2, 48

var gitVersionNumber = regexp.MustCompile(`(\d+)\.(\d+)`)

// gitVersionAtLeast reports whether a `git --version` line names
// major.minor or later.
func gitVersionAtLeast(versionLine string, major, minor int) bool {
	m := gitVersionNumber.FindStringSubmatch(versionLine)
	if m == nil {
		return false
	}
	gotMajor, err := strconv.Atoi(m[1])
	if err != nil {
		return false
	}
	gotMinor, err := strconv.Atoi(m[2])
	if err != nil {
		return false
	}
	return gotMajor > major || gotMajor == major && gotMinor >= minor
}

// requireJJSlots refuses to create a jj slot on a machine that cannot run
// one: jj without `workspace add --colocate`, git older than jjSlotMinGit,
// or Windows (jjdriver.Probe, cached for the process). The refusal is a
// precondition, cause jj_unavailable, whose hint says what to install, and
// it matches vcs.ErrUnsupported.
func (p *Platform) requireJJSlots(ctx context.Context) error {
	probe := p.jjProbe
	if probe == nil {
		probe = jjdriver.Probe
	}
	caps := probe(ctx)
	unsupported := &jjdriver.UnsupportedError{}
	var hints []string
	var probed *jjdriver.UnsupportedError
	if errors.As(caps.Err(), &probed) {
		unsupported.Causes = append(unsupported.Causes, probed.Causes...)
		hints = append(hints, probed.Hint)
	}
	if caps.GitErr == nil && !gitVersionAtLeast(caps.Git, jjSlotMinGitMajor, jjSlotMinGitMinor) {
		unsupported.Causes = append(unsupported.Causes, fmt.Errorf("%s is older than %s, which jj slots need: jj's workspace add sets extensions.relativeWorktrees on the source cache", caps.Git, jjSlotMinGit))
		hints = append(hints, "Upgrade git to "+jjSlotMinGit+" or later.")
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
	inside, err := sameOrNestedPath(p.root, primary)
	if err != nil {
		return err
	}
	_, dangling := os.Lstat(primary)
	if source.Kind == "git" && inside && dangling != nil {
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
		elsewhere, err := jj.Elsewhere(ctx, slot)
		if err != nil {
			return false, err
		}
		if elsewhere != "" || !recorded {
			return false, jjNameTaken(slot, elsewhere)
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
	hint := fmt.Sprintf("Give one of the stacks that share it another name, or, if nothing uses the workspace, forget it with `jj -R %s workspace forget %s` and retry.", slot.Primary, slot.Name)
	return &OperationError{
		Code:    CodeConflict,
		Paths:   paths,
		Hint:    hint,
		Summary: fmt.Sprintf("jj workspace %q of %s, which jj slot %s would be, belongs to %s. %s", slot.Name, slot.Primary, slot.Path, where, hint),
	}
}

// forgetJJName frees the name of a jj slot that a rollback or destroy removed
// without its primary's lock, which the primary's next prune does not do for
// jj. With the slot's directory gone, the forget changes only jj's own
// record (jjdriver.Driver.ForgetName), so it needs no lock; it runs on a
// context of its own, since the lock's wait may have used up the caller's.
func forgetJJName(ctx context.Context, driver vcs.Driver, slot vcs.Slot) error {
	jj, ok := driver.(jjLifecycle)
	if !ok || slot.Name == "" {
		return nil
	}
	forgetCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackForgetTimeout)
	defer cancel()
	if err := jj.ForgetName(forgetCtx, slot); err != nil {
		return fmt.Errorf("forget jj workspace %q of %s, so its name stays taken until `jj -R %s workspace forget %s`: %w", slot.Name, slot.Primary, slot.Primary, slot.Name, err)
	}
	return nil
}

// rollbackForgetTimeout bounds forgetJJName.
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
