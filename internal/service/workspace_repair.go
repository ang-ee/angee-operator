package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/logctx"
	"github.com/ang-ee/angee-operator/internal/manifest"
)

// WorkspaceRepair brings the source slots of an existing workspace back in
// line with its manifest. A declared slot whose path is missing (typically one
// added to `workspaces.<name>.sources` after the workspace was created, or one
// deleted from disk) is materialized the way WorkspaceCreate cuts it; for a
// git slot an empty directory at the path counts as missing. Anything else at
// a slot's path is left alone and only reported: ok, or needing attention when
// the slot is dirty, diverged, on the wrong branch or unreadable, or the path
// holds something that is not the slot. Repairing a repaired workspace changes
// nothing.
//
// Slots are repaired independently. One that cannot be materialized is rolled
// back, so nothing is left at its path, and reported as failed; the others are
// still repaired. Once cancelled, the remaining slots are reported as not
// attempted. Whenever the repair runs, the result lists every slot and the
// error is a *WorkspaceRepairError naming the failed slots, or nil. Any other
// error means the repair could not start, and the result is empty.
func (p *Platform) WorkspaceRepair(ctx context.Context, name string) (api.WorkspaceRepairResult, error) {
	ctx, release, err := p.beginMutation(ctx, "workspace")
	if err != nil {
		return api.WorkspaceRepairResult{}, err
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return api.WorkspaceRepairResult{}, err
	}
	stack, err := p.LoadStack()
	if err != nil {
		return api.WorkspaceRepairResult{}, err
	}
	workspace, ok := stack.Workspaces[name]
	if !ok {
		return api.WorkspaceRepairResult{}, &NotFoundError{Kind: "workspace", Name: name}
	}
	workspacePath := filepath.Join(p.root, "workspaces", name)
	// Slots live in the rendered workspace. Cutting them into a bare directory
	// would make an unrendered workspace look materialized, and its template
	// would then never be rendered.
	info, err := os.Lstat(workspacePath)
	if os.IsNotExist(err) {
		return api.WorkspaceRepairResult{}, &ConflictError{Kind: "workspace", Name: name, Reason: fmt.Sprintf("its directory %s does not exist, so it has no slots to repair; materialize it with `angee workspace create %s --template %s`", workspacePath, name, workspace.Template)}
	}
	if err != nil {
		return api.WorkspaceRepairResult{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return api.WorkspaceRepairResult{}, &ConflictError{Kind: "workspace", Name: name, Reason: fmt.Sprintf("%s is not a real directory", workspacePath)}
	}

	result := api.WorkspaceRepairResult{Workspace: name, Path: workspacePath, Slots: []api.WorkspaceRepairSlot{}}
	run := &workspaceRepairRun{claimed: map[string]string{}}
	for _, slot := range workspaceRepairOrder(workspace.Sources) {
		result.Slots = append(result.Slots, p.repairWorkspaceSource(ctx, run, name, slot, workspace.Sources[slot], stack))
	}
	sort.Slice(result.Slots, func(i, j int) bool { return result.Slots[i].Slot < result.Slots[j].Slot })
	repairErr := WorkspaceRepairFailure(result)
	result.OK = repairErr == nil
	return result, repairErr
}

// workspaceRepairRun is what one repair has learned about the paths it went
// through so far.
type workspaceRepairRun struct {
	// claimed maps each slot path seen to the slot that owns it.
	claimed map[string]string
	// vacant lists missing slots that could not be materialized. A missing
	// slot nested inside one is not attempted: cutting it would leave a
	// plain directory where the failed slot belongs.
	vacant []workspaceSlotPath
}

// workspaceRepairOrder orders slots so that one nested inside another's path
// is handled after it: shallower subpaths first, then by slot name.
func workspaceRepairOrder(sources map[string]manifest.WorkspaceSource) []string {
	depth := func(slot string) int {
		subpath := sources[slot].Subpath
		if subpath == "" {
			subpath = slot
		}
		clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(subpath)))
		if clean == "." {
			return 0
		}
		return strings.Count(clean, "/") + 1
	}
	slots := sortedKeys(sources)
	sort.SliceStable(slots, func(i, j int) bool { return depth(slots[i]) < depth(slots[j]) })
	return slots
}

// repairWorkspaceSource repairs one slot and reports what it did.
func (p *Platform) repairWorkspaceSource(ctx context.Context, run *workspaceRepairRun, workspaceName, slot string, wsSource manifest.WorkspaceSource, stack *manifest.Stack) api.WorkspaceRepairSlot {
	outcome := api.WorkspaceRepairSlot{Slot: slot, Source: wsSource.Source}
	finish := func(action, reason string) api.WorkspaceRepairSlot {
		outcome.Action, outcome.Reason = action, reason
		return outcome
	}
	// vacate fails a slot whose path stays empty.
	vacate := func(reason string) api.WorkspaceRepairSlot {
		run.vacant = append(run.vacant, workspaceSlotPath{slot: slot, path: outcome.Path})
		return finish(api.WorkspaceRepairFailed, reason)
	}
	_, path, err := p.workspaceSourcePath(workspaceName, slot, wsSource)
	if err != nil {
		return finish(api.WorkspaceRepairFailed, repairReason(err))
	}
	outcome.Path = path
	if err := ctx.Err(); err != nil {
		return finish(api.WorkspaceRepairFailed, "not attempted: "+repairReason(err))
	}
	if owner, taken := run.claimed[path]; taken {
		return finish(api.WorkspaceRepairFailed, fmt.Sprintf("shares its path with slot %q", owner))
	}
	run.claimed[path] = slot
	source, declared := stack.Sources[wsSource.Source]
	disk, err := inspectWorkspaceSlot(path, source)
	if err != nil {
		return finish(api.WorkspaceRepairFailed, repairReason(err))
	}
	switch disk {
	case workspaceSlotMaterialized:
		// The status reports an undeclared source as an error, so such a slot
		// needs attention instead of failing a repair that does not touch it.
		status := p.workspaceSourceStatus(ctx, workspaceName, slot, wsSource, stack)
		outcome.Status = &status
		return finish(existingWorkspaceSlotOutcome(status))
	case workspaceSlotDangling:
		return finish(api.WorkspaceRepairNeedsAttention, fmt.Sprintf("%s is a link whose target is missing; remove it and repair again", path))
	case workspaceSlotNotCheckout:
		return finish(api.WorkspaceRepairNeedsAttention, fmt.Sprintf("%s exists but is not a git checkout; move it aside and repair again", path))
	}
	if !declared {
		return vacate(fmt.Sprintf("source %q is not declared", wsSource.Source))
	}
	for _, parent := range run.vacant {
		if nested, err := sameOrNestedPath(parent.path, path); err == nil && nested {
			return vacate(fmt.Sprintf("not attempted: it lies inside slot %q, which failed", parent.slot))
		}
	}
	done := logctx.Step(ctx, "materializing source slot "+slot)
	err = p.repairMissingWorkspaceSlot(ctx, workspaceName, slot, source, wsSource, path)
	done(err)
	status := p.workspaceSourceStatus(ctx, workspaceName, slot, wsSource, stack)
	outcome.Status = &status
	if err != nil {
		return vacate(repairReason(err))
	}
	return finish(api.WorkspaceRepairCreated, describeWorkspaceSlotCut(source, wsSource))
}

// repairReason renders an error as a slot outcome's reason: on one line, with
// credentials in remote URLs redacted, since git errors quote both their
// command line and its output. An operation error gives what failed and git's
// line, without the operation's title, which the repair result already names.
func repairReason(err error) string {
	text := err.Error()
	var opErr *OperationError
	if errors.As(err, &opErr) && opErr != nil && opErr.Summary != "" {
		text = opErr.Summary
		if opErr.GitLine != "" {
			text += " git: " + opErr.GitLine
		}
	}
	return strings.Join(strings.Fields(logctx.RedactText(text)), " ")
}

// repairMissingWorkspaceSlot materializes one declared slot whose path is
// missing, through the same step WorkspaceCreate uses, and owns that slot's
// cleanup: if the cut fails, or the new slot is not on its branch, whatever was
// created is rolled back through workspaceSourceCleanup as a failed create
// would, so nothing is left at the path and no other slot is affected.
func (p *Platform) repairMissingWorkspaceSlot(ctx context.Context, workspaceName, slot string, source manifest.Source, ws manifest.WorkspaceSource, dest string) (retErr error) {
	if err := checkWorkspaceSlotBase(workspaceName, slot, source, ws); err != nil {
		return err
	}
	cleanup := &workspaceSourceCleanup{gitClient: p.gitClient()}
	defer func() {
		if retErr == nil {
			_ = cleanup.Close()
			return
		}
		// A fresh context, so the rollback runs even when the request's was
		// what got cancelled.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		retErr = joinRollbackErrors(retErr, func() error { return cleanup.Rollback(cleanupCtx) })
	}()
	if err := p.materializeWorkspaceSlot(ctx, cleanup, source, ws, dest, false); err != nil {
		return err
	}
	// Every git verb refuses a slot off its branch, so do not leave one behind.
	return p.ensureWorkspaceGitSourceOnExpectedBranch(ctx, workspaceName, slot, source, ws)
}

// existingWorkspaceSlotOutcome classifies a slot repair found on disk from its
// status. The branch check is the one the git verbs enforce
// (workspaceGitBranchMismatchReason, which the status applies), reported here
// instead of failing the repair.
func existingWorkspaceSlotOutcome(status api.WorkspaceSourceStatus) (action, reason string) {
	switch status.State {
	case "clean", "ready":
		return api.WorkspaceRepairOK, status.State
	case "ahead":
		return api.WorkspaceRepairOK, "ahead: " + status.UnpushedReason
	case "behind":
		return api.WorkspaceRepairOK, fmt.Sprintf("behind by %d commit(s)", status.Behind)
	case workspaceSourceStateBranchMismatch:
		return api.WorkspaceRepairNeedsAttention, "branch mismatch: " + status.UnpushedReason
	case "dirty":
		return api.WorkspaceRepairNeedsAttention, "uncommitted changes"
	case "diverged":
		return api.WorkspaceRepairNeedsAttention, fmt.Sprintf("diverged: %s and %d behind", status.UnpushedReason, status.Behind)
	default:
		if status.Error != "" {
			return api.WorkspaceRepairNeedsAttention, repairReason(errors.New(status.Error))
		}
		return api.WorkspaceRepairNeedsAttention, "state " + status.State
	}
}

// describeWorkspaceSlotCut says how a slot repair created was materialized.
func describeWorkspaceSlotCut(source manifest.Source, ws manifest.WorkspaceSource) string {
	base := workspaceSourceBaseRef(source, ws)
	if base == "" {
		base = "the cache's HEAD"
	}
	switch {
	case source.Kind != "git":
		return fmt.Sprintf("linked to source %q", ws.Source)
	case ws.Mode == manifest.WorkspaceSourceModeWorktree && ws.Branch != "":
		return fmt.Sprintf("worktree on branch %q, base %s", ws.Branch, base)
	case ws.Mode == manifest.WorkspaceSourceModeWorktree:
		return "worktree detached at " + base
	default:
		return "cloned at " + base
	}
}

// WorkspaceRepairError reports the slots a repair could not materialize. The
// repair still returns its result with every slot's outcome; this error makes
// the failure visible to callers that only look at errors, such as the CLI's
// exit status.
type WorkspaceRepairError struct {
	Workspace string
	// Failed holds the failed slots' outcomes, in slot order.
	Failed []api.WorkspaceRepairSlot
}

func (e *WorkspaceRepairError) Error() string {
	parts := make([]string, 0, len(e.Failed))
	for _, slot := range e.Failed {
		parts = append(parts, fmt.Sprintf("%q (%s)", slot.Slot, slot.Reason))
	}
	return fmt.Sprintf("workspace %q repair could not materialize source slot(s) %s", e.Workspace, strings.Join(parts, "; "))
}

// WorkspaceRepairFailure returns the error a repair result implies: a
// *WorkspaceRepairError naming its failed slots, or nil when none failed.
// Transports that carry the result instead of the error rebuild it with this,
// so a remote repair fails the way a local one does.
func WorkspaceRepairFailure(result api.WorkspaceRepairResult) error {
	var failed []api.WorkspaceRepairSlot
	for _, slot := range result.Slots {
		if slot.Action == api.WorkspaceRepairFailed {
			failed = append(failed, slot)
		}
	}
	if len(failed) == 0 {
		return nil
	}
	return &WorkspaceRepairError{Workspace: result.Workspace, Failed: failed}
}
