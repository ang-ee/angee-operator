package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/copierx"
	"github.com/ang-ee/angee-operator/internal/logctx"
	"github.com/ang-ee/angee-operator/internal/manifest"
	mountx "github.com/ang-ee/angee-operator/internal/mount"
	"github.com/ang-ee/angee-operator/internal/ports"
	"github.com/ang-ee/angee-operator/internal/query"
	"github.com/ang-ee/angee-operator/internal/queryfields"
	"github.com/ang-ee/angee-operator/internal/substitute"
	"github.com/ang-ee/angee-operator/internal/vcs"
)

const workspaceSourceStateBranchMismatch = "branch-mismatch"

// validateWorkspaceTemplate reads a workspace template's metadata, checking
// its kind, and refuses one that chains a template (RefuseWorkspaceChain).
func validateWorkspaceTemplate(templatePath, templateRef string) (copierx.Metadata, error) {
	metadata, err := copierx.ValidateMetadata(templatePath, "workspace")
	if err != nil {
		return copierx.Metadata{}, err
	}
	if err := RefuseWorkspaceChain(templateRef, metadata); err != nil {
		return copierx.Metadata{}, err
	}
	return metadata, nil
}

// RefuseWorkspaceChain refuses a workspace template that declares `chain` or
// `chain_root` (#110); `angee doctor` reports a template it refuses. A workspace is files: its rendered template and its
// source slots. A stack is a manifest and a runtime with its own operator, and
// its services and jobs reach a workspace through workspace:// mounts,
// workdir and env. A stack template's own `chain:` is unaffected.
func RefuseWorkspaceChain(templateRef string, metadata copierx.Metadata) error {
	if metadata.Kind != "workspace" || len(metadata.Chain) == 0 && metadata.ChainRoot == "" {
		return nil
	}
	return &InvalidInputError{
		Field:  "template",
		Reason: fmt.Sprintf("workspace template %q declares chain or chain_root, but a workspace no longer renders a stack (ang-ee/angee-operator#110); create the stack as a stack of its own and link its services and jobs to the workspace through workspace:// mounts, workdir or env", templateRef),
	}
}

// refuseRecordedWorkspaceChain refuses to update a workspace whose record
// still carries a chain: one created, before #110, from a template that
// chained a stack. Its files include what that chain rendered (the stack's
// answers file among them), which a render of the workspace template alone
// would overwrite or delete. The record is left as it is; status, logs and
// destroy still work. A record written before #110 always lists the
// workspace's own template first, so only a chain root or a second entry
// marks a chain.
func refuseRecordedWorkspaceChain(name string, workspace manifest.Workspace) error {
	if workspace.Resolved.ChainRoot == "" && len(workspace.Resolved.Chain) <= 1 {
		return nil
	}
	return &InvalidInputError{
		Field:  "workspace",
		Reason: fmt.Sprintf("workspace %q was created from a template that chained a stack (ang-ee/angee-operator#110); destroy it and create it again", name),
	}
}

func (p *Platform) WorkspaceCreate(ctx context.Context, req api.WorkspaceCreateRequest) (result api.WorkspaceRef, retErr error) {
	ctx, release, err := p.beginMutation(ctx, "workspace")
	if err != nil {
		return result, err
	}
	defer release()
	if req.Template == "" {
		return api.WorkspaceRef{}, &InvalidInputError{Field: "template", Reason: "workspace template is required"}
	}
	stackRootGuard, err := copierx.OpenGuardedPath("", filepath.Dir(p.root), filepath.Base(p.root), nil)
	if err != nil {
		return api.WorkspaceRef{}, fmt.Errorf("validate stack root: %w", err)
	}
	defer func() { _ = stackRootGuard.Close() }()
	rootInfo, rootExists, err := stackRootGuard.Lstat()
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	if rootExists && (rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir()) {
		return api.WorkspaceRef{}, fmt.Errorf("stack root %q is not a real directory", p.root)
	}
	if err := stackRootGuard.MkdirAll(0o755); err != nil {
		return api.WorkspaceRef{}, err
	}
	parentTx, stack, err := openParentStackTransaction(p.root, true)
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	defer func() { _ = parentTx.Close() }()
	if stack == nil {
		stack = p.EmptyStack(defaultWorkspaceStackName(p.root))
	}
	templatePath, templateRef, err := p.resolveTemplate(ctx, req.Template, "workspace")
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	metadata, err := validateWorkspaceTemplate(templatePath, templateRef)
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	if err := manifest.Ensure(stack, metadata.Ensure); err != nil {
		return api.WorkspaceRef{}, err
	}
	// The stack's workspace_defaults for this template sit under the request's
	// explicit inputs (and, below, under a declaration's inputs).
	defaults := stackWorkspaceDefaults(stack, templateRef)
	inputs := workspaceInputs(metadata, mergeStringMaps(defaults, req.Inputs))
	name, err := p.workspaceName(metadata, req.Name, inputs)
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	if declared, exists := stack.Workspaces[name]; exists {
		if _, statErr := os.Stat(filepath.Join(p.root, "workspaces", name)); statErr == nil {
			return api.WorkspaceRef{}, &ConflictError{Kind: "workspace", Name: name, Reason: "already exists"}
		}
		// Declared but never materialized (a rendered manifest carries the
		// declaration; `angee dev` owns the cut): adopt the declaration's
		// inputs as defaults under the request's explicit inputs.
		inputs = workspaceInputs(metadata, mergeStringMaps(defaults, declared.Inputs, req.Inputs))
	}
	finishAllocating := logctx.Step(ctx, "allocating ports")
	allocations, err := p.allocateWorkspacePorts(stack, name)
	finishAllocating(err)
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	workspacePath := filepath.Join(p.root, "workspaces", name)
	workspaceGuard, err := copierx.OpenGuardedPath(p.root, p.root, filepath.ToSlash(filepath.Join("workspaces", name)), nil)
	if err != nil {
		return api.WorkspaceRef{}, fmt.Errorf("validate workspace destination: %w", err)
	}
	defer func() { _ = workspaceGuard.Close() }()
	// A pre-existing workspace directory is a leftover from an earlier failed
	// create; rollback below must not delete it (that leftover is what sync is
	// gated to protect), only the worktrees this create itself materializes.
	info, workspacePreexisted, err := workspaceGuard.Lstat()
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	if workspacePreexisted && (info.Mode()&os.ModeSymlink != 0 || !info.IsDir()) {
		return api.WorkspaceRef{}, fmt.Errorf("workspace destination %q is not a real directory", workspacePath)
	}
	if err := workspaceGuard.MkdirAll(0o755); err != nil {
		return api.WorkspaceRef{}, err
	}
	committed := false
	var workspaceSources map[string]manifest.WorkspaceSource
	var sourceCleanup *workspaceSourceCleanup
	var rollbackTemplateFiles func() error
	var rollbackPersistPaths func() error
	var closePersistPaths func() error
	statePath := renderPlanStatePath(p.root, "workspaces", name)
	defer func() {
		if committed {
			return
		}
		// Roll back only what this create materialized: deregister the worktrees
		// it added (which also deletes their working trees), and remove the
		// workspace directory only when this create created it. A create that
		// failed at the "already exists" guard materialized nothing, so its
		// workspaceSources is empty and a pre-existing leftover is left intact.
		// Use a fresh context so cleanup runs even when the request context is
		// what was cancelled.
		removeWorkspace := func() error { return nil }
		if !workspacePreexisted {
			removeWorkspace = workspaceGuard.RemoveAll
		}
		retErr = joinRollbackErrors(retErr, parentTx.Rollback, removeWorkspace)
	}()
	defer func() {
		if sourceCleanup == nil {
			return
		}
		if committed {
			_ = sourceCleanup.Close()
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		retErr = joinRollbackErrors(retErr, func() error { return sourceCleanup.Rollback(cleanupCtx) })
		cancel()
	}()
	finishSources := logctx.Step(ctx, "materializing workspace sources")
	workspaceSources, sourceCleanup, err = p.materializeWorkspaceSources(ctx, stack, name, workspacePath, metadata, inputs, allocations, req.Sync)
	finishSources(err)
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	finishRendering := logctx.Step(ctx, "rendering template")
	renderPlan, err := p.buildWorkspaceRenderPlan(workspacePath, templatePath, metadata, inputs, name, allocations, workspaceSources, stack.Sources, statePath)
	if err != nil {
		finishRendering(err)
		return api.WorkspaceRef{}, err
	}
	defer func() { _ = renderPlan.Close() }()
	prepared, err := copierx.PrepareReconcile(ctx, renderPlan.Plan, copierx.ReconcileOptions{Mode: copierx.ReconcileCreate})
	finishRendering(err)
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	defer func() { _ = prepared.Close() }()
	if err := parentTx.VerifyPreparedRoot(p.root, prepared); err != nil {
		return api.WorkspaceRef{}, fmt.Errorf("verify stack root transaction: %w", err)
	}
	finishApplying := logctx.Step(ctx, "applying rendered files")
	rollbackTemplateFiles, err = prepared.ApplyFiles(ctx)
	finishApplying(err)
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	defer func() {
		if !committed {
			retErr = joinRollbackErrors(retErr, rollbackTemplateFiles)
		}
	}()
	rollbackPersistPaths, closePersistPaths, verifyPersistPaths, err := materializePersistPaths(ctx, prepared.OpenTargetPath, workspacePath, metadata.Persist, renderPlan.Plan.AllowedSymlinkParents)
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	defer func() {
		if !committed {
			retErr = joinRollbackErrors(retErr, rollbackPersistPaths)
		}
		_ = closePersistPaths()
	}()
	workspace := manifest.Workspace{
		Template: templateRef,
		Inputs:   map[string]string(inputs),
		Sources:  workspaceSources,
		Resolved: manifest.WorkspaceResolved{
			Allocations:  copyIntMap(allocations),
			PersistPaths: metadata.Persist,
		},
		TTL: req.TTL,
	}
	if req.TTL != "" {
		duration, err := time.ParseDuration(req.TTL)
		if err != nil {
			return api.WorkspaceRef{}, err
		}
		expires := time.Now().Add(duration).UTC()
		workspace.TTLExpiresAt = &expires
	}
	if stack.Workspaces == nil {
		stack.Workspaces = map[string]manifest.Workspace{}
	}
	stack.Workspaces[name] = workspace
	if err := joinRollbackErrors(prepared.VerifyTargetRootPath(), verifyPersistPaths); err != nil {
		return api.WorkspaceRef{}, err
	}
	finishSaving := logctx.Step(ctx, "saving stack")
	if err := parentTx.Save(stack); err != nil {
		finishSaving(err)
		return api.WorkspaceRef{}, err
	}
	if err := prepared.SaveState(ctx); err != nil {
		err = joinRollbackErrors(err, parentTx.Rollback)
		finishSaving(err)
		return api.WorkspaceRef{}, err
	}
	finishSaving(nil)
	committed = true
	return workspaceRef(name, workspacePath, workspace), nil
}

func defaultWorkspaceStackName(root string) string {
	name := filepath.Base(root)
	if name == ".angee" {
		name = filepath.Base(filepath.Dir(root))
	}
	if name == "." || name == string(filepath.Separator) || name == "" {
		return "workspace"
	}
	return name
}

func (p *Platform) WorkspaceList(ctx context.Context, q query.Args) ([]api.WorkspaceRef, int, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if err := query.Validate(q, queryfields.Workspace); err != nil {
		return nil, 0, invalidQueryError(err)
	}
	stack, err := p.LoadStack()
	if err != nil {
		return nil, 0, err
	}
	refs := make([]api.WorkspaceRef, 0, len(stack.Workspaces))
	for _, name := range sortedKeys(stack.Workspaces) {
		workspace := stack.Workspaces[name]
		refs = append(refs, workspaceRef(name, filepath.Join(p.root, "workspaces", name), workspace))
	}
	page, total := query.Apply(refs, q, queryfields.Workspace)
	return page, total, nil
}

func (p *Platform) WorkspaceGet(ctx context.Context, name string) (api.WorkspaceRef, error) {
	if err := ctx.Err(); err != nil {
		return api.WorkspaceRef{}, err
	}
	stack, err := p.LoadStack()
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	workspace, ok := stack.Workspaces[name]
	if !ok {
		return api.WorkspaceRef{}, &NotFoundError{Kind: "workspace", Name: name}
	}
	return workspaceRef(name, filepath.Join(p.root, "workspaces", name), workspace), nil
}

func (p *Platform) WorkspaceStatus(ctx context.Context, name string) (api.WorkspaceStatusResponse, error) {
	if err := ctx.Err(); err != nil {
		return api.WorkspaceStatusResponse{}, err
	}
	stack, err := p.LoadStack()
	if err != nil {
		return api.WorkspaceStatusResponse{}, err
	}
	workspace, ok := stack.Workspaces[name]
	if !ok {
		return api.WorkspaceStatusResponse{}, &NotFoundError{Kind: "workspace", Name: name}
	}
	return p.workspaceStatus(ctx, name, workspace, stack), nil
}

func (p *Platform) workspaceStatus(ctx context.Context, name string, workspace manifest.Workspace, stack *manifest.Stack) api.WorkspaceStatusResponse {
	path := filepath.Join(p.root, "workspaces", name)
	_, statErr := os.Stat(path)
	exists := statErr == nil
	state := "ready"
	if statErr != nil {
		if os.IsNotExist(statErr) {
			state = "missing"
		} else {
			state = "error"
		}
	}
	expired := workspace.TTLExpiresAt != nil && time.Now().After(*workspace.TTLExpiresAt)
	if expired {
		state = "expired"
	}
	processComposePort, playwrightMCPName, playwrightMCPURL := workspaceRuntimeFacts(name, workspace)
	status := api.WorkspaceStatusResponse{
		Name:               name,
		Path:               path,
		Exists:             exists,
		State:              state,
		Template:           workspace.Template,
		Inputs:             copyStringMap(workspace.Inputs),
		Sources:            []api.WorkspaceSourceStatus{},
		Chain:              []string{},
		Allocations:        copyIntMap(workspace.Resolved.Allocations),
		ProcessComposePort: processComposePort,
		PlaywrightMCPName:  playwrightMCPName,
		PlaywrightMCPURL:   playwrightMCPURL,
		PersistPaths:       workspacePersistPaths(workspace.Resolved.PersistPaths),
		TTL:                workspace.TTL,
		TTLExpiresAt:       workspace.TTLExpiresAt,
		Expired:            expired,
		MountedBy:          workspaceMountedBy(stack, name),
	}
	if statErr != nil && !os.IsNotExist(statErr) {
		status.Error = statErr.Error()
	}
	for _, slot := range sortedKeys(workspace.Sources) {
		sourceStatus := p.workspaceSourceStatus(ctx, name, slot, workspace.Sources[slot], stack)
		status.Sources = append(status.Sources, sourceStatus)
		if sourceStatus.State == workspaceSourceStateBranchMismatch {
			status.State = "discrepancy"
		}
	}
	return status
}

// workspaceSourceStatus reads one slot through its driver: the jj driver for
// a jj slot, which snapshots its working copy first (plan D7) and is never
// branch-mismatch, and the git driver for a git one. A local source's link is
// only ready or missing. Mode is the slot's effective mode.
func (p *Platform) workspaceSourceStatus(ctx context.Context, workspaceName, slot string, wsSource manifest.WorkspaceSource, stack *manifest.Stack) api.WorkspaceSourceStatus {
	wsSource = withEffectiveMode(stack, wsSource)
	jjSlot := wsSource.Mode == manifest.WorkspaceSourceModeJJ
	subpath, path, pathErr := p.workspaceSourcePath(workspaceName, slot, wsSource)
	status := api.WorkspaceSourceStatus{
		Slot:    slot,
		Source:  wsSource.Source,
		Mode:    wsSource.Mode,
		Branch:  wsSource.Branch,
		Ref:     wsSource.Ref,
		Subpath: subpath,
		Path:    path,
		State:   "missing",
		Pushed:  true,
	}
	if pathErr != nil {
		status.State = "error"
		status.Pushed = false
		status.Error = pathErr.Error()
		return status
	}
	source, ok := stack.Sources[wsSource.Source]
	if !ok {
		status.State = "error"
		status.Pushed = false
		status.Error = fmt.Sprintf("source %q is not declared", wsSource.Source)
		return status
	}
	status.Kind = source.Kind
	if _, err := os.Stat(path); err != nil {
		status.Exists = false
		if !os.IsNotExist(err) {
			status.State = "error"
			status.Error = err.Error()
		}
		return status
	}
	status.Exists = true
	if source.Kind != "git" && !jjSlot {
		status.State = "ready"
		return status
	}
	if jjSlot {
		if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
			status.State = "error"
			status.Pushed = false
			status.Error = fmt.Sprintf("%s is a link, not a jj workspace: a jj slot is never linked", path)
			return status
		}
	}
	if !jjSlot && wsSource.Mode != manifest.WorkspaceSourceModeWorktree {
		p.reportCloneSlotRemotes(ctx, workspaceName, slot, path, source, wsSource)
	}
	target := workspaceVCSSlot(path, source, wsSource)
	read, err := p.slotDriver(target).Status(ctx, target)
	setSlotStatus(&status, read, err)
	return status
}

// setSlotStatus fills status with what the slot's driver read, read and err
// being what vcs.Driver.Status returned. It is the one mapping from a
// driver's read to the API: every DTO that reports a slot or a source's
// cache (SourceState, GitOpsLink) is made from the WorkspaceSourceStatus it
// fills. A read that failed makes the slot an error and not pushed, and
// keeps what the driver read before it failed, as a jj slot on a missing base
// keeps its counts. The jj fields are copied as they are, so they are zero
// for a git slot.
func setSlotStatus(status *api.WorkspaceSourceStatus, read vcs.Status, err error) {
	status.CurrentRef = read.CurrentRef
	status.Dirty = read.Dirty
	status.Upstream = read.Upstream
	status.Ahead = read.Ahead
	status.Behind = read.Behind
	status.JJSlotState = api.JJSlotState{
		ChangeID:       read.ChangeID,
		BaseBehind:     read.BaseBehind,
		Conflicted:     read.Conflicted,
		ConflictPaths:  read.ConflictPaths,
		UntrackedPaths: read.UntrackedPaths,
	}
	if err != nil {
		status.State = "error"
		status.Pushed = false
		status.Error = err.Error()
		return
	}
	status.State = workspaceSourceStateOf(read)
	status.Pushed = read.Pushed
	status.UnpushedReason = read.UnpushedReason
}

// workspaceSourceStateOf names the state a slot's driver status puts it in.
// Ahead and behind describe the slot against its upstream or base; whether
// the slot holds work of its own is Pushed, which can be true while it is
// ahead (after sync-base, its new commits are the base's).
func workspaceSourceStateOf(read vcs.Status) string {
	switch {
	case read.MismatchReason != "":
		return workspaceSourceStateBranchMismatch
	case read.Dirty:
		return "dirty"
	case read.Ahead > 0 && read.Behind > 0:
		return "diverged"
	case read.Ahead > 0:
		return "ahead"
	case read.Behind > 0:
		return "behind"
	default:
		return "clean"
	}
}

func (p *Platform) WorkspaceDestroy(ctx context.Context, name string, purge bool) error {
	ctx, release, err := p.beginMutation(ctx, "workspace")
	if err != nil {
		return err
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return err
	}
	parentTx, stack, err := openParentStackTransaction(p.root, false)
	if err != nil {
		return err
	}
	defer func() { _ = parentTx.Close() }()
	workspace, ok := stack.Workspaces[name]
	if !ok {
		return &NotFoundError{Kind: "workspace", Name: name}
	}
	var workspaceGuard *copierx.GuardedPath
	if purge {
		workspaceGuard, err = copierx.OpenGuardedPath(p.root, p.root, filepath.ToSlash(filepath.Join("workspaces", name)), nil)
		if err != nil {
			return fmt.Errorf("validate workspace destination: %w", err)
		}
		defer func() { _ = workspaceGuard.Close() }()
	}
	if err := p.ensureWorkspaceGitSourcesOnExpectedBranches(ctx, name, workspace, stack); err != nil {
		return err
	}
	if err := p.ensureWorkspaceGitSourcesPushed(ctx, name, workspace, stack); err != nil {
		return err
	}
	delete(stack.Workspaces, name)
	releaseWorkspacePorts(stack, name)
	if err := parentTx.Save(stack); err != nil {
		return err
	}
	if purge {
		workspacePath := filepath.Join(p.root, "workspaces", name)
		// Deregister the worktrees first so purging the directory does not
		// strand "missing but already registered" entries in the shared caches.
		p.removeWorkspaceSources(ctx, stack, name, workspacePath, workspace.Sources)
		return workspaceGuard.RemoveAll()
	}
	return nil
}

func (p *Platform) ensureWorkspaceGitSourcesOnExpectedBranches(ctx context.Context, workspaceName string, workspace manifest.Workspace, stack *manifest.Stack) error {
	for _, slot := range sortedKeys(workspace.Sources) {
		wsSource := workspace.Sources[slot]
		source, ok := stack.Sources[wsSource.Source]
		if !ok {
			return fmt.Errorf("workspace %q source %q references undeclared source %q", workspaceName, slot, wsSource.Source)
		}
		if err := p.ensureWorkspaceGitSourceOnExpectedBranch(ctx, workspaceName, slot, source, wsSource); err != nil {
			return err
		}
	}
	return nil
}

func (p *Platform) ensureWorkspaceGitSourceOnExpectedBranch(ctx context.Context, workspaceName, slot string, source manifest.Source, wsSource manifest.WorkspaceSource) error {
	if source.Kind != "git" || !workspaceSourceRequiresBranch(wsSource) {
		return nil
	}
	_, path, err := p.workspaceSourcePath(workspaceName, slot, wsSource)
	if err != nil {
		return fmt.Errorf("workspace %q source %q: %w", workspaceName, slot, err)
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	driver, target := p.workspaceSlotDriver(path, source, wsSource)
	reason, err := driver.BranchMismatch(ctx, target)
	if err != nil {
		return err
	}
	if reason != "" {
		// Only a git worktree slot must stay on a branch
		// (workspaceSourceRequiresBranch). The verbs that still ask here
		// move to Driver.Ready, which names the fix itself, with the rest of
		// phase 8 of the jj plan.
		return branchMismatch(workspaceName, slot, wsSource.Source, path, &vcs.BranchMismatchError{Reason: reason, Fix: fmt.Sprintf("git -C %s switch %s", path, wsSource.Branch)})
	}
	return nil
}

// branchMismatch is the branch_mismatch refusal of a workspace slot at path,
// with the fix its driver names.
func branchMismatch(workspaceName, slot, source, path string, mismatch *vcs.BranchMismatchError) error {
	hint := "Put it back on its branch, then retry."
	if mismatch.Fix != "" {
		hint = fmt.Sprintf("Switch it back with `%s`, then retry.", mismatch.Fix)
	}
	return &OperationError{
		Code:      CodePreconditionFailed,
		Cause:     CauseBranchMismatch,
		Workspace: workspaceName,
		Slot:      slot,
		Source:    source,
		Paths:     []string{path},
		Hint:      hint,
		Summary:   fmt.Sprintf("workspace %q source %q has branch mismatch: %s at %s. %s", workspaceName, slot, mismatch.Reason, path, hint),
	}
}

// requireSlotReady refuses verb on a workspace slot when its driver says the
// slot is not ready for it (vcs.Driver.Ready), in the service's words: a
// branch mismatch with the fix the driver names, uncommitted changes, or
// untracked paths only the slot holds.
func requireSlotReady(ctx context.Context, driver vcs.Driver, target vcs.Slot, verb vcs.Verb, workspaceName, slot, source string) error {
	err := driver.Ready(ctx, target, verb)
	var mismatch *vcs.BranchMismatchError
	var uncommitted *vcs.UncommittedError
	var untracked *vcs.UntrackedPathsError
	switch {
	case errors.As(err, &mismatch):
		return branchMismatch(workspaceName, slot, source, target.Path, mismatch)
	case errors.As(err, &uncommitted):
		return uncommittedChanges(uncommitted.Paths, fmt.Sprintf("source slot %q", slot), slot, source)
	case errors.As(err, &untracked):
		hint := "Track them (`jj file track`), move them out of the slot, or ignore them, then retry."
		return &OperationError{
			Code:      CodePreconditionFailed,
			Cause:     CauseUntrackedPaths,
			Workspace: workspaceName,
			Slot:      slot,
			Source:    source,
			Paths:     untracked.Paths[:min(len(untracked.Paths), maxErrorPaths)],
			Hint:      hint,
			Summary: fmt.Sprintf("source slot %q holds files jj does not track, only it has, and a rebase could keep over the base's own: %s. %s",
				slot, vcs.NamePaths(untracked.Paths, maxMessagePaths), hint),
			Err: err,
		}
	}
	return err
}

// workspaceSourceRequiresBranch reports whether a slot must stay on its
// manifest branch: a worktree slot that names one.
func workspaceSourceRequiresBranch(wsSource manifest.WorkspaceSource) bool {
	return wsSource.Mode == manifest.WorkspaceSourceModeWorktree && wsSource.Branch != ""
}

// ensureWorkspaceGitSourcesPushed refuses to let a workspace go while a slot
// holds work nothing else does, as its driver's status reports it, or, in a
// jj slot, while a repository jj does not record is nested in it
// (nestedRepositories).
func (p *Platform) ensureWorkspaceGitSourcesPushed(ctx context.Context, workspaceName string, workspace manifest.Workspace, stack *manifest.Stack) error {
	unpushed := []string{}
	for _, slot := range sortedKeys(workspace.Sources) {
		wsSource := workspace.Sources[slot]
		source, ok := stack.Sources[wsSource.Source]
		if !ok {
			return fmt.Errorf("workspace %q source %q references undeclared source %q", workspaceName, slot, wsSource.Source)
		}
		wsSource = withEffectiveMode(stack, wsSource)
		if source.Kind != "git" && wsSource.Mode != manifest.WorkspaceSourceModeJJ {
			continue
		}
		_, path, err := p.workspaceSourcePath(workspaceName, slot, wsSource)
		if err != nil {
			return fmt.Errorf("workspace %q source %q: %w", workspaceName, slot, err)
		}
		if _, err := os.Stat(path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if wsSource.Mode == manifest.WorkspaceSourceModeJJ {
			// A link at a jj slot's path, left from when the slot linked a
			// local source, and an empty directory, which repair takes for a
			// missing slot, hold nothing of their own: destroy removes only
			// them.
			info, err := os.Lstat(path)
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				continue
			}
			if empty, err := emptyDirectory(path); err != nil {
				return fmt.Errorf("workspace %q source %q: %w", workspaceName, slot, err)
			} else if empty {
				continue
			}
		}
		// A jj slot's status snapshots its working copy first, so an edit no
		// jj command has recorded yet counts as uncommitted (plan D7).
		target := workspaceVCSSlot(path, source, wsSource)
		read, err := p.slotDriver(target).Status(ctx, target)
		// A slot whose base no longer resolves, such as a deleted branch, can
		// still be let go, when the driver decided Pushed without counting.
		// Any other failure, a jj slot's missing base included until its
		// driver reports one this way, refuses the destroy.
		var countErr *vcs.CountError
		if err != nil && !errors.As(err, &countErr) {
			return fmt.Errorf("workspace %q source %q: %w", workspaceName, slot, err)
		}
		if !read.Pushed {
			unpushed = append(unpushed, fmt.Sprintf("%s (%s)", slot, read.UnpushedReason))
			continue
		}
		// jj records no repository nested in a jj slot, such as a clone an
		// agent made in it, so its status says nothing of their work.
		if wsSource.Mode == manifest.WorkspaceSourceModeJJ {
			subpath, _, _ := p.workspaceSourcePath(workspaceName, slot, wsSource)
			_, colocatedErr := os.Lstat(filepath.Join(p.sourcePath(wsSource.Source, source), ".git"))
			nested, err := nestedRepositories(path, nestedSlots(subpath, workspace.Sources, slot), colocatedErr == nil)
			if err != nil {
				return fmt.Errorf("workspace %q source %q: look for repositories inside it: %w", workspaceName, slot, err)
			}
			if len(nested) > 0 {
				unpushed = append(unpushed, fmt.Sprintf("%s (%s)", slot, nestedRepositoriesReason(nested)))
			}
		}
	}
	if len(unpushed) > 0 {
		return fmt.Errorf("workspace %q has sources that have not been pushed: %s", workspaceName, strings.Join(unpushed, ", "))
	}
	return nil
}

func (p *Platform) WorkspaceUpdate(ctx context.Context, name string, req api.WorkspaceUpdateRequest) (api.WorkspaceRef, error) {
	ctx, release, err := p.beginMutation(ctx, "workspace")
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return api.WorkspaceRef{}, err
	}
	parentTx, stack, err := openParentStackTransaction(p.root, false)
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	defer func() { _ = parentTx.Close() }()
	workspace, ok := stack.Workspaces[name]
	if !ok {
		return api.WorkspaceRef{}, &NotFoundError{Kind: "workspace", Name: name}
	}
	if err := refuseRecordedWorkspaceChain(name, workspace); err != nil {
		return api.WorkspaceRef{}, err
	}
	templatePath, templateRef, err := p.resolveTemplate(ctx, workspace.Template, "workspace")
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	metadata, err := validateWorkspaceTemplate(templatePath, templateRef)
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	if err := manifest.Ensure(stack, metadata.Ensure); err != nil {
		return api.WorkspaceRef{}, err
	}
	recorded := mergeStringMaps(stackWorkspaceDefaults(stack, templateRef), workspace.Inputs)
	if err := validateImmutableTemplateInputs(templateRef, templatePath, recorded, req.Inputs); err != nil {
		return api.WorkspaceRef{}, err
	}
	workspace.Inputs = recorded
	if req.Inputs != nil {
		if workspace.Inputs == nil {
			workspace.Inputs = map[string]string{}
		}
		for key, value := range req.Inputs {
			workspace.Inputs[key] = value
		}
	}
	workspace.Inputs = workspaceInputs(metadata, workspace.Inputs)
	if req.TTL != "" {
		duration, err := time.ParseDuration(req.TTL)
		if err != nil {
			return api.WorkspaceRef{}, err
		}
		expires := time.Now().Add(duration).UTC()
		workspace.TTL = req.TTL
		workspace.TTLExpiresAt = &expires
	}
	workspacePath := filepath.Join(p.root, "workspaces", name)
	statePath := renderPlanStatePath(p.root, "workspaces", name)
	renderPlan, err := p.buildWorkspaceRenderPlan(workspacePath, templatePath, metadata, workspace.Inputs, name, workspace.Resolved.Allocations, workspace.Sources, stack.Sources, statePath)
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	defer func() { _ = renderPlan.Close() }()
	prepared, err := copierx.PrepareReconcile(ctx, renderPlan.Plan, copierx.ReconcileOptions{
		Mode: copierx.ReconcileUpdate, Overwrite: req.Overwrite,
	})
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	defer func() { _ = prepared.Close() }()
	if err := parentTx.VerifyPreparedRoot(p.root, prepared); err != nil {
		return api.WorkspaceRef{}, fmt.Errorf("verify stack root transaction: %w", err)
	}
	if conflicts := prepared.Result().Conflicts; len(conflicts) != 0 {
		paths := make([]string, 0, len(conflicts))
		for _, conflict := range conflicts {
			paths = append(paths, conflict.Path)
		}
		return api.WorkspaceRef{}, &ConflictError{Kind: "workspace-template", Name: name, Reason: fmt.Sprintf("locally modified paths: %s; use --overwrite to replace", strings.Join(paths, ", "))}
	}
	rollbackFiles, err := prepared.ApplyFiles(ctx)
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	rollbackPersistPaths, closePersistPaths, verifyPersistPaths, err := materializePersistPaths(ctx, prepared.OpenTargetPath, workspacePath, metadata.Persist, renderPlan.Plan.AllowedSymlinkParents)
	if err != nil {
		return api.WorkspaceRef{}, joinRollbackErrors(err, rollbackFiles)
	}
	defer func() { _ = closePersistPaths() }()
	workspace.Template = templateRef
	workspace.Resolved.PersistPaths = metadata.Persist
	stack.Workspaces[name] = workspace
	if err := joinRollbackErrors(prepared.VerifyTargetRootPath(), verifyPersistPaths); err != nil {
		return api.WorkspaceRef{}, joinRollbackErrors(err, rollbackPersistPaths, rollbackFiles)
	}
	if err := parentTx.Save(stack); err != nil {
		return api.WorkspaceRef{}, joinRollbackErrors(err, rollbackPersistPaths, rollbackFiles)
	}
	if err := prepared.SaveState(ctx); err != nil {
		return api.WorkspaceRef{}, joinRollbackErrors(err, parentTx.Rollback, rollbackPersistPaths, rollbackFiles)
	}
	return workspaceRef(name, workspacePath, workspace), nil
}

// WorkspaceLogs is deprecated (#110): a workspace is files and has no logs of
// its own. It answers a declared workspace with a closed channel, as it always
// did for a workspace without a chained stack, and is removed in a later
// release. A stack's services and jobs have their own logs.
func (p *Platform) WorkspaceLogs(ctx context.Context, name string, follow bool) (<-chan string, error) {
	return p.WorkspaceLogsLimited(ctx, name, follow, 0)
}

// WorkspaceLogsLimited is deprecated with WorkspaceLogs.
func (p *Platform) WorkspaceLogsLimited(ctx context.Context, name string, _ bool, _ int) (<-chan string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stack, err := p.LoadStack()
	if err != nil {
		return nil, err
	}
	if _, ok := stack.Workspaces[name]; !ok {
		return nil, &NotFoundError{Kind: "workspace", Name: name}
	}
	ch := make(chan string)
	close(ch)
	return ch, nil
}

func releaseWorkspacePorts(stack *manifest.Stack, workspaceName string) {
	for poolName, leases := range stack.PortLeases {
		kept := leases[:0]
		for _, lease := range leases {
			if strings.HasPrefix(lease.Owner, "workspace/"+workspaceName+"/") {
				continue
			}
			kept = append(kept, lease)
		}
		if len(kept) == 0 {
			delete(stack.PortLeases, poolName)
			continue
		}
		stack.PortLeases[poolName] = kept
	}
}

type materializedPersistPath struct {
	path    string
	dest    *copierx.GuardedPath
	created bool
	trusted *copierx.TrustedRoot
}

func materializePersistPaths(ctx context.Context, openPath guardedPathOpener, publicRoot string, persist map[string]manifest.PersistPath, allowedSymlinkParents map[string]*copierx.TrustedRoot) (func() error, func() error, func() error, error) {
	created := []materializedPersistPath{}
	closeGuards := func() error {
		var result error
		for _, path := range created {
			if err := path.dest.Close(); err != nil && result == nil {
				result = err
			}
		}
		return result
	}
	rolledBack := false
	rollback := func() error {
		if rolledBack {
			return nil
		}
		rolledBack = true
		defer func() { _ = closeGuards() }()
		var result error
		for index := len(created) - 1; index >= 0; index-- {
			if !created[index].created {
				continue
			}
			err := created[index].dest.Remove()
			if err != nil && !os.IsNotExist(err) && result == nil {
				result = err
			}
			if err := created[index].dest.RemoveMissingParents(); err != nil && result == nil {
				result = err
			}
		}
		return result
	}
	fail := func(primary error, cleanup ...func() error) (func() error, func() error, func() error, error) {
		cleanup = append(cleanup, rollback)
		return nil, nil, nil, joinRollbackErrors(primary, cleanup...)
	}
	for _, key := range sortedKeys(persist) {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		entry := persist[key]
		if entry.Subpath == "" {
			continue
		}
		clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(entry.Subpath)))
		if trusted, isSourceRoot := allowedSymlinkParents[clean]; isSourceRoot {
			link, err := openPath(clean)
			if err != nil {
				return fail(fmt.Errorf("persist %q: %w", key, err))
			}
			info, exists, err := link.Lstat()
			if err != nil || !exists || info.Mode()&os.ModeSymlink == 0 {
				if err == nil {
					err = fmt.Errorf("declared Source persist root %q is not a symlink", clean)
				}
				return fail(fmt.Errorf("persist %q: %w", key, err), link.Close)
			}
			probe, err := openPath(filepath.ToSlash(filepath.Join(filepath.FromSlash(clean), ".angee-persist-probe")))
			if err != nil {
				return fail(fmt.Errorf("persist %q: %w", key, err), link.Close)
			}
			_ = probe.Close()
			created = append(created, materializedPersistPath{path: clean, dest: link, trusted: trusted})
			continue
		}
		dir, err := openPath(clean)
		if err != nil {
			// A persist dir owned by a service container (postgres pgdata is
			// 0700 uid 999) cannot be OPENED by the privilege-dropped operator.
			// It only needs to exist — nothing is written on this path — so a
			// no-follow stat is enough to accept it and skip creation.
			if errors.Is(err, os.ErrPermission) {
				full := filepath.Join(publicRoot, filepath.FromSlash(clean))
				if info, lerr := os.Lstat(full); lerr == nil && info.IsDir() {
					continue
				}
			}
			return fail(fmt.Errorf("persist %q: %w", key, err))
		}
		info, exists, err := dir.Lstat()
		if err != nil {
			return fail(fmt.Errorf("persist %q: %w", key, err), dir.Close)
		}
		if exists {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return fail(fmt.Errorf("persist %q path %q is not a real directory", key, clean), dir.Close)
			}
			created = append(created, materializedPersistPath{path: clean, dest: dir})
			continue
		}
		if err := dir.MkdirAll(0o755); err != nil {
			return fail(fmt.Errorf("persist %q: %w", key, err), dir.Close)
		}
		created = append(created, materializedPersistPath{path: clean, dest: dir, created: true})
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	verify := func() error {
		var result error
		for _, path := range created {
			publicPath := filepath.Join(publicRoot, filepath.FromSlash(path.path))
			result = errors.Join(result, path.dest.VerifyPathEntryIdentity(publicPath))
			if path.trusted != nil {
				result = errors.Join(result, path.trusted.VerifyPath(publicPath))
			}
		}
		return result
	}
	return rollback, closeGuards, verify, nil
}

func workspaceRef(name, path string, ws manifest.Workspace) api.WorkspaceRef {
	processComposePort, playwrightMCPName, playwrightMCPURL := workspaceRuntimeFacts(name, ws)
	return api.WorkspaceRef{
		Name:               name,
		Path:               path,
		Template:           ws.Template,
		Allocations:        copyIntMap(ws.Resolved.Allocations),
		ProcessComposePort: processComposePort,
		PlaywrightMCPName:  playwrightMCPName,
		PlaywrightMCPURL:   playwrightMCPURL,
		TTL:                ws.TTL,
		TTLExpiresAt:       ws.TTLExpiresAt,
	}
}

func workspaceRuntimeFacts(name string, ws manifest.Workspace) (int, string, string) {
	processComposePort := firstPositiveAllocation(ws.Resolved.Allocations, "process_compose", "custom")
	if processComposePort == 0 {
		processComposePort = firstPositiveInputPort(ws.Inputs, "process_compose_port")
	}
	playwrightPort := firstPositiveAllocation(ws.Resolved.Allocations, "playwright_mcp", "playwright", "browser_mcp", "mcp")
	if playwrightPort == 0 {
		playwrightPort = firstPositiveInputPort(ws.Inputs, "playwright_mcp_port", "playwright_port")
	}
	if playwrightPort == 0 {
		return processComposePort, "", ""
	}
	mcpName := "playwright-" + name
	mcpURL := fmt.Sprintf("http://127.0.0.1:%d/mcp", playwrightPort)
	return processComposePort, mcpName, mcpURL
}

func firstPositiveAllocation(alloc map[string]int, keys ...string) int {
	for _, key := range keys {
		if value := alloc[key]; value > 0 {
			return value
		}
	}
	return 0
}

func firstPositiveInputPort(inputs map[string]string, keys ...string) int {
	for _, key := range keys {
		value, err := strconv.Atoi(strings.TrimSpace(inputs[key]))
		if err == nil && value > 0 {
			return value
		}
	}
	return 0
}

func copyIntMap(in map[string]int) map[string]int {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]int, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func copyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func workspacePersistPaths(in map[string]manifest.PersistPath) map[string]api.WorkspacePersistPath {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]api.WorkspacePersistPath, len(in))
	for key, value := range in {
		out[key] = api.WorkspacePersistPath{Subpath: value.Subpath, Scope: value.Scope}
	}
	return out
}

func workspaceMountedBy(stack *manifest.Stack, workspaceName string) []api.WorkspaceMountRef {
	refs := []api.WorkspaceMountRef{}
	for _, name := range sortedKeys(stack.Services) {
		service := stack.Services[name]
		refs = appendWorkspaceRunnableRefs(refs, "service", name, workspaceName, service.Mounts, service.Workdir, service.Env)
	}
	for _, name := range sortedKeys(stack.Jobs) {
		job := stack.Jobs[name]
		refs = appendWorkspaceRunnableRefs(refs, "job", name, workspaceName, job.Mounts, job.Workdir, job.Env)
	}
	return refs
}

func appendWorkspaceRunnableRefs(refs []api.WorkspaceMountRef, kind, name, workspaceName string, mounts manifest.StringList, workdir string, env map[string]string) []api.WorkspaceMountRef {
	for _, raw := range mounts {
		if mountReferencesWorkspace(raw, workspaceName) {
			refs = append(refs, api.WorkspaceMountRef{Kind: kind, Name: name, Field: "mounts", Value: raw})
		}
	}
	if workspaceURIReferences(workdir, workspaceName) {
		refs = append(refs, api.WorkspaceMountRef{Kind: kind, Name: name, Field: "workdir", Value: workdir})
	}
	for _, key := range sortedKeys(env) {
		value := env[key]
		if workspaceStringReferences(value, workspaceName) {
			refs = append(refs, api.WorkspaceMountRef{Kind: kind, Name: name, Field: "env." + key, Value: value})
		}
	}
	return refs
}

func mountReferencesWorkspace(raw, workspaceName string) bool {
	if !strings.Contains(raw, "://") {
		return false
	}
	mount, err := mountx.Parse(raw)
	return err == nil && mount.Scheme == "workspace" && mount.Name == workspaceName
}

func workspaceURIReferences(raw, workspaceName string) bool {
	if !strings.Contains(raw, "://") {
		return false
	}
	scheme, rest, _ := strings.Cut(raw, "://")
	if scheme != "workspace" {
		return false
	}
	name, _, _ := strings.Cut(rest, "/")
	return name == workspaceName
}

func workspaceStringReferences(value, workspaceName string) bool {
	return strings.Contains(value, "${workspace."+workspaceName+".") || strings.Contains(value, "workspace://"+workspaceName)
}

func (p *Platform) WorkspaceGitStatus(ctx context.Context, name string) ([]api.SourceState, error) {
	stack, err := p.LoadStack()
	if err != nil {
		return nil, err
	}
	workspace, ok := stack.Workspaces[name]
	if !ok {
		return nil, &NotFoundError{Kind: "workspace", Name: name}
	}
	states := []api.SourceState{}
	for _, slot := range sortedKeys(workspace.Sources) {
		wsSource := workspace.Sources[slot]
		_, ok := stack.Sources[wsSource.Source]
		if !ok {
			return nil, fmt.Errorf("workspace %q source %q references undeclared source %q", name, slot, wsSource.Source)
		}
		state, err := p.workspaceSourceState(ctx, name, slot, stack, wsSource)
		if err != nil {
			return nil, err
		}
		states = append(states, state)
	}
	return states, nil
}

func (p *Platform) WorkspacePush(ctx context.Context, name, ref string) (states []api.SourceState, err error) {
	defer operation{name: "workspace.push", title: fmt.Sprintf("Push workspace %q", name), workspace: name}.annotate(&err)
	ctx, release, err := p.beginMutation(ctx, "workspace")
	if err != nil {
		return nil, err
	}
	defer release()
	stack, err := p.LoadStack()
	if err != nil {
		return nil, err
	}
	workspace, ok := stack.Workspaces[name]
	if !ok {
		return nil, &NotFoundError{Kind: "workspace", Name: name}
	}
	if err := p.refuseJJSlots("workspace push", name, workspace); err != nil {
		return nil, err
	}
	// Check the slots are materialized first: the branch check reads git in
	// each slot's path and would read an enclosing repository's branch.
	targets, err := p.workspaceGitSlots(name, workspace, stack, manifest.WorkspaceSourceModeWorktree, manifest.WorkspaceSourceModeClone)
	if err != nil {
		return nil, err
	}
	if err := p.ensureWorkspaceGitSourcesOnExpectedBranches(ctx, name, workspace, stack); err != nil {
		return nil, err
	}
	plans := make([]workspaceGitPush, len(targets))
	for i, target := range targets {
		driver, slot := p.workspaceSlotDriver(target.path, target.source, target.wsSource)
		if err := requireCleanSlot(ctx, driver, slot, fmt.Sprintf("source slot %q", target.slot), target.slot, target.wsSource.Source); err != nil {
			return nil, err
		}
		if plans[i], err = planWorkspaceGitPush(ctx, driver, slot, name, target.slot, target.wsSource, ref); err != nil {
			return nil, err
		}
	}
	states = []api.SourceState{}
	for i, target := range targets {
		if err := plans[i].run(ctx); err != nil {
			return nil, err
		}
		state, err := p.workspaceSourceState(ctx, name, target.slot, stack, target.wsSource)
		if err != nil {
			return nil, err
		}
		states = append(states, state)
	}
	return states, nil
}

// workspaceSlotPath names a workspace source slot, where it lives on disk, the
// source it materializes, and its effective mode.
type workspaceSlotPath struct {
	slot   string
	path   string
	source manifest.Source
	mode   string
}

// workspaceGitSlot is a git slot an aggregate workspace verb acts on.
type workspaceGitSlot struct {
	workspaceSlotPath
	wsSource manifest.WorkspaceSource
}

// workspaceGitSlots returns the workspace's slots whose manifest names one of
// modes, in slot order: the slots an aggregate verb walks. A slot that names
// no mode is never one of them, whatever its effective mode (owner,
// 2026-10-29, plan Q7), so a work-state `.work` slot, a jj slot under `vcs:
// jj`, stays out of every aggregate. A slot of a `kind: git` source is a
// version-controlled checkout, and so is a jj slot of a `kind: local` store.
// Every one of them must be materialized: the verb fails before touching any
// slot when one is not.
func (p *Platform) workspaceGitSlots(workspaceName string, workspace manifest.Workspace, stack *manifest.Stack, modes ...string) ([]workspaceGitSlot, error) {
	var targets []workspaceGitSlot
	var paths []workspaceSlotPath
	for _, slot := range sortedKeys(workspace.Sources) {
		wsSource := workspace.Sources[slot]
		if !slices.Contains(modes, wsSource.Mode) {
			continue
		}
		source, ok := stack.Sources[wsSource.Source]
		if !ok || source.Kind != "git" && wsSource.Mode != manifest.WorkspaceSourceModeJJ {
			continue
		}
		_, path, err := p.workspaceSourcePath(workspaceName, slot, wsSource)
		if err != nil {
			return nil, fmt.Errorf("workspace %q source %q: %w", workspaceName, slot, err)
		}
		target := workspaceGitSlot{workspaceSlotPath: workspaceSlotPath{slot: slot, path: path, source: source, mode: wsSource.Mode}, wsSource: wsSource}
		targets = append(targets, target)
		paths = append(paths, target.workspaceSlotPath)
	}
	if err := requireWorkspaceSlotsOnDisk(workspaceName, paths); err != nil {
		return nil, err
	}
	return targets, nil
}

// workspaceSlotDisk is what a slot's path holds.
type workspaceSlotDisk int

const (
	// workspaceSlotAbsent: nothing is at the path, or an empty directory is
	// where a git checkout belongs. Repair materializes the slot there.
	workspaceSlotAbsent workspaceSlotDisk = iota
	// workspaceSlotMaterialized: the slot is on disk.
	workspaceSlotMaterialized
	// workspaceSlotDangling: the path is a link whose target is gone.
	workspaceSlotDangling
	// workspaceSlotNotCheckout: a git or jj slot's path holds something that
	// is not its own checkout. Git or jj run there would act on an enclosing
	// repository, such as the project an ANGEE_ROOT lives in.
	workspaceSlotNotCheckout
)

// inspectWorkspaceSlot reports what is at a slot's path. mode is the slot's
// effective mode: a jj slot's checkout has a .jj, and a git one a .git.
func inspectWorkspaceSlot(path string, source manifest.Source, mode string) (workspaceSlotDisk, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return workspaceSlotAbsent, nil
	}
	if err != nil {
		return 0, err
	}
	isLink := info.Mode()&os.ModeSymlink != 0
	if isLink && mode == manifest.WorkspaceSourceModeJJ {
		// A jj slot is made in place, never linked: a link is what a local
		// source left from before its slots became jj slots.
		return workspaceSlotNotCheckout, nil
	}
	if isLink {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return workspaceSlotDangling, nil
		} else if err != nil {
			return 0, err
		}
	}
	marker := ".git"
	switch {
	case mode == manifest.WorkspaceSourceModeJJ:
		marker = ".jj"
	case source.Kind != "git":
		return workspaceSlotMaterialized, nil
	}
	if _, err := os.Lstat(filepath.Join(path, marker)); err == nil {
		return workspaceSlotMaterialized, nil
	} else if !os.IsNotExist(err) {
		return 0, err
	}
	if !isLink && info.IsDir() {
		empty, err := emptyDirectory(path)
		if err != nil {
			return 0, err
		}
		if empty {
			return workspaceSlotAbsent, nil
		}
	}
	return workspaceSlotNotCheckout, nil
}

func emptyDirectory(path string) (bool, error) {
	dir, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer func() { _ = dir.Close() }()
	if _, err := dir.Readdirnames(1); errors.Is(err, io.EOF) {
		return true, nil
	} else if err != nil {
		return false, err
	}
	return false, nil
}

// requireWorkspaceSlotsOnDisk is the check every slot verb makes before it
// touches a slot. A slot declared in the manifest but not materialized (one
// added to an existing workspace, say) fails the verb with a message naming it
// and pointing at `workspace repair`, rather than as a raw git chdir error or,
// for a directory that is not the slot's checkout, a git command run in an
// enclosing repository.
func requireWorkspaceSlotsOnDisk(workspaceName string, slots []workspaceSlotPath) error {
	var problems, slotNames, paths []string
	for _, slot := range slots {
		disk, err := inspectWorkspaceSlot(slot.path, slot.source, slot.mode)
		if err != nil {
			return fmt.Errorf("workspace %q source %q: %w", workspaceName, slot.slot, err)
		}
		if disk != workspaceSlotMaterialized {
			slotNames = append(slotNames, slot.slot)
			paths = append(paths, slot.path)
		}
		switch disk {
		case workspaceSlotAbsent:
			problems = append(problems, fmt.Sprintf("source slot %q is missing at %s", slot.slot, slot.path))
		case workspaceSlotDangling:
			problems = append(problems, fmt.Sprintf("source slot %q at %s is a link whose target is missing; remove it", slot.slot, slot.path))
		case workspaceSlotNotCheckout:
			checkout := "a git checkout"
			if slot.mode == manifest.WorkspaceSourceModeJJ {
				checkout = "a jj workspace"
			}
			problems = append(problems, fmt.Sprintf("source slot %q at %s is not %s; move it aside", slot.slot, slot.path, checkout))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	pronoun := "it"
	if len(problems) > 1 {
		pronoun = "them"
	}
	reason := fmt.Sprintf("%s; run `angee workspace repair %s` to materialize %s", strings.Join(problems, "; "), workspaceName, pronoun)
	opErr := &OperationError{
		Code:      CodePreconditionFailed,
		Cause:     CauseSlotMissing,
		Workspace: workspaceName,
		Paths:     paths,
		Hint:      fmt.Sprintf("Run `angee workspace repair %s`.", workspaceName),
		Summary:   reason,
	}
	if len(slotNames) == 1 {
		opErr.Slot = slotNames[0]
	}
	return opErr
}

// workspaceGitPush is a push of one clean git slot, as its driver planned it
// (vcs.Driver.PlanPush): an explicit ref as given, else the slot's branch to
// its upstream or to a new one. A slot with nothing of its own is left alone
// (skip says why), since the push would only create a remote branch with no
// work on it.
type workspaceGitPush struct {
	driver vcs.Driver
	target vcs.Slot
	plan   vcs.PushPlan
	skip   string
	step   gitStep
}

// planWorkspaceGitPush decides a slot's push without pushing, so an
// aggregate push can refuse before any slot is pushed: a slot on a detached
// HEAD with commits of its own has no branch to push.
func planWorkspaceGitPush(ctx context.Context, driver vcs.Driver, target vcs.Slot, workspaceName, slot string, wsSource manifest.WorkspaceSource, ref string) (workspaceGitPush, error) {
	push := workspaceGitPush{driver: driver, target: target, step: gitStep{action: "pushing", object: fmt.Sprintf("source slot %q", slot), remote: driver.PushURL(ctx, target, ""), cause: CausePushFailed, slot: slot, source: wsSource.Source}}
	plan, err := driver.PlanPush(ctx, target, ref, wsSource.Branch)
	if errors.Is(err, vcs.ErrDetachedWithWork) {
		return push, detachedHeadPushError(workspaceName, slot, wsSource)
	} else if err != nil {
		return push, err
	}
	push.plan = plan
	if plan.Skip {
		push.skip = fmt.Sprintf("workspace %q source %q has no commits beyond %s; not pushing it", workspaceName, slot, target.BaseRef)
	}
	return push, nil
}

// run carries out the push the plan decided.
func (push workspaceGitPush) run(ctx context.Context) error {
	if push.skip != "" {
		logctx.From(ctx).Warn(push.skip)
		return nil
	}
	return gitFailure(push.driver.Push(ctx, push.target, push.plan), push.step)
}

func detachedHeadPushError(workspaceName, slot string, wsSource manifest.WorkspaceSource) error {
	publish := fmt.Sprintf("angee workspace source publish %s %s --branch <name>", workspaceName, slot)
	return &OperationError{
		Code:      CodePreconditionFailed,
		Cause:     CauseDetachedHead,
		Workspace: workspaceName,
		Slot:      slot,
		Source:    wsSource.Source,
		Hint:      fmt.Sprintf("Publish it with `%s`.", publish),
		Summary:   fmt.Sprintf("source slot %q is on a detached HEAD, so there is no branch to push; publish it with `%s`", slot, publish),
	}
}

// WorkspaceSyncBase brings each slot's base into it, through the slot's
// driver: the slots whose manifest names a mode that syncs (a git worktree,
// a jj slot); a slot with no mode is skipped, even a jj slot under `vcs: jj`
// (owner, 2026-10-29, plan Q7). With no method each slot is synced its
// driver's way, a git worktree by merging and a jj slot by rebasing (plan
// D11); a method a slot's driver cannot sync by, such as a merge of a jj
// slot, refuses the whole sync before any slot is read.
//
// Every slot is checked, fetched, has its base resolved and, when it is to
// be rebased, what the rebase would rewrite checked (vcs.Driver.CheckRebase,
// plan D12) before any is synced, so a refusal those checks make leaves no
// slot changed. A slot's sync can still fail once earlier ones are synced:
// its rebase checks again just before it runs, and git or jj can fail it.
// That error names the slots already synced. A jj slot's conflict is recorded
// in its commits and reported on its state, read from its status
// (conflicted, conflict_paths), and the sync goes on to the next slot; a git
// slot that stops on conflicts ends the sync with merge_conflict, as before.
// What jj warned of while it synced a slot, and a failure to read what the
// sync left, are on that slot's state (error), and the sync goes on.
// A local source's jj store is the user's, so angee never fetches it: such a
// slot is rebased onto its base as the store has it.
func (p *Platform) WorkspaceSyncBase(ctx context.Context, name, method string) (states []api.SourceState, err error) {
	defer operation{name: "workspace.sync-base", title: fmt.Sprintf("Sync base for workspace %q", name), workspace: name}.annotate(&err)
	ctx, release, err := p.beginMutation(ctx, "workspace")
	if err != nil {
		return nil, err
	}
	defer release()
	stack, err := p.LoadStack()
	if err != nil {
		return nil, err
	}
	workspace, ok := stack.Workspaces[name]
	if !ok {
		return nil, &NotFoundError{Kind: "workspace", Name: name}
	}
	method, ok = normalizeWorkspaceSyncBaseMethod(method)
	if !ok {
		return nil, &InvalidInputError{Field: "method", Reason: fmt.Sprintf("workspace sync-base method must be %q or %q", vcs.SyncMerge, vcs.SyncRebase)}
	}
	// Check the slots are materialized first: a driver's checks read each
	// slot's path, and git would read an enclosing repository there.
	targets, err := p.workspaceGitSlots(name, workspace, stack, manifest.WorkspaceSourceModeWorktree, manifest.WorkspaceSourceModeJJ)
	if err != nil {
		return nil, err
	}
	// Each slot's driver resolves the method for it, before any slot is read.
	drivers := make([]vcs.Driver, len(targets))
	slots := make([]vcs.Slot, len(targets))
	methods := make([]string, len(targets))
	jjSlots := false
	for i, target := range targets {
		drivers[i], slots[i] = p.workspaceSlotDriver(target.path, target.source, target.wsSource)
		if methods[i], err = drivers[i].SyncMethod(method); err != nil {
			return nil, syncMethodRefused(err, name, target.slot, target.wsSource.Source, method)
		}
		jjSlots = jjSlots || slots[i].Mode == vcs.ModeJJ
	}
	// A machine that cannot run jj slots says so, rather than fail a fetch.
	if jjSlots {
		if err := p.requireJJSlots(ctx); err != nil {
			return nil, err
		}
	}
	// Every slot must be able to sync before any is touched, so a refusal
	// never leaves the workspace half-synced. What a slot needs for it is its
	// driver's to say (vcs.Driver.Ready).
	for i, target := range targets {
		if err := requireSlotReady(ctx, drivers[i], slots[i], vcs.VerbSyncBase, name, target.slot, target.wsSource.Source); err != nil {
			return nil, err
		}
		if slots[i].BaseRef == "" {
			return nil, &OperationError{
				Code:    CodePreconditionFailed,
				Slot:    target.slot,
				Source:  target.wsSource.Source,
				Hint:    fmt.Sprintf("Set `ref` on the slot or `default_ref` on source %q.", target.wsSource.Source),
				Summary: fmt.Sprintf("source slot %q has no base ref to sync from. Set `ref` on the slot or `default_ref` on source %q.", target.slot, target.wsSource.Source),
			}
		}
	}
	// Fetch every slot and resolve its base before syncing any, so a remote
	// that fails or lacks the base refuses the sync with no slot changed.
	syncRefs := make([]string, len(targets))
	for i, target := range targets {
		if target.source.Kind == "git" {
			fetch := gitStep{action: "fetching", object: fmt.Sprintf("source %q", target.wsSource.Source), remote: drivers[i].FetchURL(ctx, slots[i]), cause: CauseFetchFailed, slot: target.slot, source: target.wsSource.Source}
			if err := drivers[i].Fetch(ctx, slots[i]); err != nil {
				return nil, gitFailure(err, fetch)
			}
		}
		syncRef, err := drivers[i].ResolveBase(ctx, slots[i])
		if errors.Is(err, vcs.ErrBaseNotFound) {
			hint := fmt.Sprintf("Check the base ref %q of source slot %q.", slots[i].BaseRef, target.slot)
			return nil, &OperationError{Code: CodePreconditionFailed, Slot: target.slot, Source: target.wsSource.Source, Hint: hint, Summary: fmt.Sprintf("source slot %q: %v. %s", target.slot, err, hint), Err: err}
		} else if err != nil {
			return nil, gitFailure(err, gitStep{action: "resolving the base of", object: fmt.Sprintf("source slot %q", target.slot), slot: target.slot, source: target.wsSource.Source})
		}
		syncRefs[i] = syncRef
	}
	// Check what each rebase would rewrite before rebasing any (plan D12).
	steps := make([]gitStep, len(targets))
	for i, target := range targets {
		steps[i] = gitStep{action: fmt.Sprintf("merging %s into", syncRefs[i]), object: fmt.Sprintf("source slot %q", target.slot), slot: target.slot, source: target.wsSource.Source}
		if methods[i] != vcs.SyncRebase {
			continue
		}
		steps[i].action = fmt.Sprintf("rebasing onto %s", syncRefs[i])
		if err := drivers[i].CheckRebase(ctx, slots[i], syncRefs[i]); err != nil {
			return nil, syncFailure(err, steps[i], name)
		}
	}
	states = []api.SourceState{}
	var synced []string
	for i, target := range targets {
		result, syncErr := drivers[i].SyncBase(ctx, slots[i], syncRefs[i], methods[i])
		if syncErr != nil && !result.OK {
			return nil, afterSynced(syncFailure(syncErr, steps[i], name), synced)
		}
		if result.Conflicted && !result.OK {
			// The slot stopped mid-merge or mid-rebase (git).
			return nil, afterSynced(syncConflict(result, steps[i]), synced)
		}
		synced = append(synced, target.slot)
		state, err := p.workspaceSourceState(ctx, name, target.slot, stack, target.wsSource)
		if err != nil {
			return nil, err
		}
		// A conflict recorded in the slot's commits (jj) reaches its state
		// through its status, as conflicted and conflict_paths. The slot
		// synced, but what jj warned of, or a failure to read what the sync
		// left, is on its state, and the sync goes on.
		var warned *vcs.Warning
		switch {
		case syncErr == nil:
		case errors.As(syncErr, &warned):
			state.Error = joinNonEmpty(state.Error, fmt.Sprintf("synced, but jj warned while %s: %s", steps[i].action, warned.Text))
		default:
			state.Error = joinNonEmpty(state.Error, fmt.Sprintf("synced, but reading what %s left failed: %v", steps[i].action, syncErr))
		}
		states = append(states, state)
	}
	return states, nil
}

// joinNonEmpty joins the non-empty of a and b with "; ".
func joinNonEmpty(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "; " + b
}

// afterSynced makes err, the failure of a slot's sync, name the slots synced
// before it, which it leaves synced: in the message, before the remedy, and
// in the detail. err is an *OperationError, as syncFailure and syncConflict
// return; anything else passes through.
func afterSynced(err error, synced []string) error {
	var opErr *OperationError
	if len(synced) == 0 || !errors.As(err, &opErr) {
		return err
	}
	quoted := make([]string, len(synced))
	for i, slot := range synced {
		quoted[i] = fmt.Sprintf("%q", slot)
	}
	noun := "source slot"
	if len(synced) > 1 {
		noun = "source slots"
	}
	note := fmt.Sprintf("%s %s synced before it, and stay synced.", noun, strings.Join(quoted, ", "))
	if opErr.Summary == "" {
		opErr.Summary = opErr.Error()
	}
	opErr.Summary += " " + note
	opErr.Detail = joinNonEmpty(note, opErr.Detail)
	return opErr
}

// syncFailure is a slot's sync failure in the service's words: a rebase that
// would rewrite what is not the slot's to rewrite is its refusal
// (rebaseRefusal), and anything else the failure of step.
func syncFailure(err error, step gitStep, workspaceName string) error {
	if refusal := rebaseRefusal(err, workspaceName, step.slot, step.source); refusal != nil {
		return refusal
	}
	return gitFailure(err, step)
}

// syncMethodRefused is the refusal of a sync-base method a slot's driver
// cannot sync by (vcs.Driver.SyncMethod), such as a merge of a jj slot (plan
// D11): rebasing instead of the merge asked for would be worse than
// refusing, so no slot is touched.
func syncMethodRefused(err error, workspaceName, slot, source, method string) error {
	if !errors.Is(err, vcs.ErrUnsupported) {
		return err
	}
	hint := "Sync by rebase, or pass no method: each slot is then synced its own way, a git slot by merging and a jj slot by rebasing."
	return &OperationError{
		Code:      CodeInvalidInput,
		Workspace: workspaceName,
		Slot:      slot,
		Source:    source,
		Hint:      hint,
		Summary:   fmt.Sprintf("source slot %q cannot be synced by %s (%v). %s", slot, method, err, hint),
		Err:       err,
	}
}

// normalizeWorkspaceSyncBaseMethod reads a requested sync-base method. No
// method is vcs.SyncDefault, which each slot's driver resolves to its own
// natural sync (vcs.Driver.SyncMethod; git: a merge).
func normalizeWorkspaceSyncBaseMethod(method string) (string, bool) {
	switch method = strings.ToLower(strings.TrimSpace(method)); method {
	case vcs.SyncDefault, vcs.SyncMerge, vcs.SyncRebase:
		return method, true
	default:
		return "", false
	}
}

func workspaceSourceBaseRef(source manifest.Source, wsSource manifest.WorkspaceSource) string {
	if wsSource.Ref != "" {
		return wsSource.Ref
	}
	return source.DefaultRef
}

func (p *Platform) workspaceSourceState(ctx context.Context, workspaceName, slot string, stack *manifest.Stack, wsSource manifest.WorkspaceSource) (api.SourceState, error) {
	return sourceStateFromWorkspaceStatus(p.workspaceSourceStatus(ctx, workspaceName, slot, wsSource, stack)), nil
}

func sourceStateFromWorkspaceStatus(status api.WorkspaceSourceStatus) api.SourceState {
	return api.SourceState{
		Name:           status.Source,
		Slot:           status.Slot,
		Kind:           status.Kind,
		Path:           status.Path,
		Exists:         status.Exists,
		State:          status.State,
		Branch:         status.Branch,
		Ref:            status.Ref,
		CurrentRef:     status.CurrentRef,
		Dirty:          status.Dirty,
		Upstream:       status.Upstream,
		Ahead:          status.Ahead,
		Behind:         status.Behind,
		Pushed:         status.Pushed,
		UnpushedReason: status.UnpushedReason,
		Error:          status.Error,
		JJSlotState:    status.JJSlotState,
	}
}

// workspaceSourceCleanupEntry is one slot a create or repair cut: the guard on
// its path and, for a git or jj slot, the slot as its driver was handed it,
// so a rollback removes it through the same driver.
type workspaceSourceCleanupEntry struct {
	dest   *copierx.GuardedPath
	driver vcs.Driver // nil for a local source's link
	slot   vcs.Slot
	// kept marks a jj slot --sync found in place and left as it was, which a
	// rollback leaves too.
	kept bool
	// emptyDir, when set, is the mode of the empty directory that was at the
	// path before a jj slot was made in it, which a rollback puts back: jj
	// makes the slot in place, so removing the slot removes that directory.
	emptyDir fs.FileMode
}

type workspaceSourceCleanup struct {
	entries []workspaceSourceCleanupEntry
}

func (c *workspaceSourceCleanup) Close() error {
	if c == nil {
		return nil
	}
	var result error
	for _, entry := range c.entries {
		if err := entry.dest.Close(); err != nil && result == nil {
			result = err
		}
	}
	c.entries = nil
	return result
}

// Rollback removes the slots in the reverse of the order they were cut, and
// the parent directories their guards created. A slot that was kept stays,
// and an empty directory a jj slot was made in is put back.
func (c *workspaceSourceCleanup) Rollback(ctx context.Context) error {
	if c == nil {
		return nil
	}
	var result error
	for index := len(c.entries) - 1; index >= 0; index-- {
		entry := c.entries[index]
		if entry.kept {
			continue
		}
		if err := removeWorkspaceSlot(ctx, entry.driver, entry.slot, entry.dest); err != nil && result == nil {
			result = err
		}
		if err := restoreEmptyDir(entry.dest, entry.emptyDir); err != nil && result == nil {
			result = err
		}
		if err := entry.dest.RemoveMissingParents(); err != nil && result == nil {
			result = err
		}
	}
	if err := c.Close(); err != nil && result == nil {
		result = err
	}
	return result
}

// removeWorkspaceSlot removes the slot at dest: a git or jj slot through its
// driver, under its primary's lock when it has one, and a local source's
// link (no driver) by removing the link.
//
// Only what the primary keeps of the slot needs its lock, not the slot's own
// path. When the lock cannot be taken (another process holds it past the
// rollback's deadline or ANGEE_LOCK_TIMEOUT), the slot is still removed
// through dest, and the primary keeps its git registration until its next
// prune. A jj slot's name is not pruned, so it is forgotten once the
// directory is gone, which needs no lock (removeJJSlotUnlocked); otherwise
// the name would stay taken and refuse the next create of the slot.
func removeWorkspaceSlot(ctx context.Context, driver vcs.Driver, slot vcs.Slot, dest *copierx.GuardedPath) error {
	switch {
	case driver == nil:
		return dest.RemoveAll()
	case slot.Primary == "":
		return driver.Remove(ctx, slot, dest)
	}
	err := withPrimaryLock(ctx, slot.Primary, func() error { return driver.Remove(ctx, slot, dest) })
	if !errors.Is(err, errPrimaryLockNotHeld) {
		return err
	}
	logctx.From(ctx).Warn(fmt.Sprintf("removing %s without deregistering it from source cache %s (%v); the cache drops the registration when it is next pruned", slot.Path, slot.Primary, err))
	if slot.Mode == vcs.ModeJJ {
		return removeJJSlotUnlocked(ctx, driver, slot, dest)
	}
	return dest.RemoveAll()
}

// slotCleanupTimeout bounds the removal of one slot by a destroy, which runs
// on a context the request cannot cancel.
const slotCleanupTimeout = 30 * time.Second

// restoreEmptyDir puts back the empty directory a jj slot was made in, once
// the slot is removed, with the permissions of mode, the directory's mode as
// Lstat read it; 0 means there was none.
func restoreEmptyDir(dest *copierx.GuardedPath, mode fs.FileMode) error {
	if mode == 0 {
		return nil
	}
	return dest.MkdirAll(mode)
}

func (p *Platform) materializeWorkspaceSources(ctx context.Context, stack *manifest.Stack, workspaceName, workspacePath string, metadata copierx.Metadata, inputs map[string]string, alloc map[string]int, sync bool) (map[string]manifest.WorkspaceSource, *workspaceSourceCleanup, error) {
	result := map[string]manifest.WorkspaceSource{}
	cleanup := &workspaceSourceCleanup{}
	items := []workspaceSourceMaterialization{}
	for _, slot := range sortedKeys(metadata.Sources) {
		spec := metadata.Sources[slot]
		// The slot's source name is input-substitutable (e.g. an opt-in
		// work-state slot names its source through an input; empty + optional
		// skips the slot below).
		sourceName, err := substitute.Resolve(spec.Source, substitute.Context{Inputs: inputs, Name: workspaceName, Alloc: alloc})
		if err != nil {
			return nil, cleanup, fmt.Errorf("workspace source %q name: %w", slot, err)
		}
		if sourceName == "" {
			sourceName = slot
		}
		source, ok := stack.Sources[sourceName]
		if !ok {
			var err error
			source, err = resolveWorkspaceTemplateSource(spec, inputs, workspaceName, alloc)
			if err != nil {
				return nil, cleanup, err
			}
			if source.Kind != "" {
				if err := manifest.ValidateSource(sourceName, source); err != nil {
					return nil, cleanup, fmt.Errorf("workspace source %q: %w", slot, err)
				}
				if stack.Sources == nil {
					stack.Sources = map[string]manifest.Source{}
				}
				stack.Sources[sourceName] = source
				ok = true
			}
		}
		if !ok {
			if spec.Optional {
				continue
			}
			return nil, cleanup, fmt.Errorf("workspace source %q references undeclared source %q", slot, sourceName)
		}
		resolved, err := p.resolveWorkspaceSource(spec, sourceName, inputs, workspaceName, alloc)
		if err != nil {
			return nil, cleanup, fmt.Errorf("workspace %q source %q: %w", workspaceName, slot, err)
		}
		if err := checkWorkspaceSlotBase(workspaceName, slot, source, resolved, stack.EffectiveSourceMode(resolved)); err != nil {
			return nil, cleanup, err
		}
		if resolved.Subpath == "" {
			resolved.Subpath = slot
		}
		resolved.Subpath, err = normalizeWorkspaceSubpath(resolved.Subpath)
		if err != nil {
			return nil, cleanup, fmt.Errorf("workspace source %q subpath: %w", slot, err)
		}
		items = append(items, workspaceSourceMaterialization{
			slot:       slot,
			sourceName: sourceName,
			source:     source,
			resolved:   resolved,
			optional:   spec.Optional,
		})
	}
	orderedItems, err := orderWorkspaceSourceMaterializations(items)
	if err != nil {
		return nil, cleanup, err
	}
	jjTargets, err := p.jjSlotTargets(ctx, stack, workspaceName, items)
	if err != nil {
		return nil, cleanup, err
	}
	for _, item := range orderedItems {
		dest := filepath.Join(workspacePath, filepath.FromSlash(item.resolved.Subpath))
		finish := logctx.Step(ctx, "materializing source "+item.sourceName)
		// The record keeps the mode as written; the slot is cut in the mode
		// it is checked out in.
		if err := p.materializeWorkspaceSlot(ctx, cleanup, item.source, withEffectiveMode(stack, item.resolved), dest, sync, jjTargets[item.slot]); err != nil {
			if item.optional {
				// An optional source that cannot be materialized is skipped by
				// design, so the phase completes normally rather than as a failure.
				logctx.From(ctx).Warn(fmt.Sprintf("skipping optional source slot %q: %v", item.slot, err))
				finish(nil)
				continue
			}
			finish(err)
			// Return the sources materialized so far alongside the error so the
			// caller's rollback can undo them; this is the single cleanup
			// site for a failed create.
			return result, cleanup, err
		}
		finish(nil)
		result[item.slot] = item.resolved
	}
	return result, cleanup, nil
}

// checkWorkspaceSlotBase rejects a slot that has nothing to start from, before
// anything is materialized. A worktree without a branch is detached at its
// base; with no base it would detach at whatever the cache has checked out,
// and nothing would record where its work starts. A jj slot starts on its
// branch's bookmark only when that exists, so it always needs a base. mode is
// the slot's effective mode (manifest.Stack.EffectiveSourceMode).
func checkWorkspaceSlotBase(workspaceName, slot string, source manifest.Source, ws manifest.WorkspaceSource, mode string) error {
	noBase := ws.Ref == "" && source.DefaultRef == ""
	if mode == manifest.WorkspaceSourceModeJJ && noBase {
		return fmt.Errorf("workspace %q source %q: a jj slot needs a ref or the source's default_ref to start from", workspaceName, slot)
	}
	if source.Kind == "git" && ws.Mode == manifest.WorkspaceSourceModeWorktree && ws.Branch == "" && noBase {
		return fmt.Errorf("workspace %q source %q: a worktree without a branch needs a ref or the source's default_ref to start from", workspaceName, slot)
	}
	return nil
}

// materializeWorkspaceSlot cuts one source slot at dest and records what it
// created in cleanup, so the caller can roll it back with the rest of its
// work. It is the one step that materializes a slot, for WorkspaceCreate and
// WorkspaceRepair alike. ws's mode is the effective one, and jj is the jj
// workspace a jj slot is made as. On failure it records nothing;
// materializeWorkspaceSource removes what it staged or installed at dest
// before returning.
func (p *Platform) materializeWorkspaceSlot(ctx context.Context, cleanup *workspaceSourceCleanup, source manifest.Source, ws manifest.WorkspaceSource, dest string, sync bool, jj jjSlotTarget) error {
	entry, err := p.materializeWorkspaceSource(ctx, ws.Source, source, ws, dest, sync, jj)
	if err != nil {
		return err
	}
	cleanup.entries = append(cleanup.entries, *entry)
	return nil
}

// removeWorkspaceSources removes only source destinations materialized for the
// workspace: git and jj slots through their drivers, which also deregister
// linked worktrees from their caches and forget jj workspaces, and local
// links directly. Every destination is removed through a stack-rooted
// capability; per-source cleanup is best effort. A jj slot's workspace is
// forgotten only when the checkout at its path is that workspace
// (destroyedJJSlot), so a name another checkout now has, such as another
// stack's of the same name, is never forgotten; otherwise the slot is removed
// from disk only, with a warning. Destroy calls it once the manifest no
// longer records the workspace, so each slot is removed on a context of its
// own, as a rollback's is: a request cancelled now would otherwise fail the
// read that tells whose a jj slot's name is, and leave the name taken.
func (p *Platform) removeWorkspaceSources(ctx context.Context, stack *manifest.Stack, workspaceName, workspacePath string, sources map[string]manifest.WorkspaceSource) {
	ctx = context.WithoutCancel(ctx)
	type materializedSource struct {
		dest   string
		driver vcs.Driver // nil for a local source's link
		slot   vcs.Slot
	}
	var materialized []materializedSource
	for slot, ws := range sources {
		subpath := ws.Subpath
		if subpath == "" {
			subpath = slot
		}
		item := materializedSource{dest: filepath.Join(workspacePath, filepath.FromSlash(subpath))}
		ws = withEffectiveMode(stack, ws)
		source, ok := stack.Sources[ws.Source]
		switch {
		case !ok:
		case ws.Mode == manifest.WorkspaceSourceModeJJ:
			jjSlot := p.workspaceLifecycleSlot(item.dest, source, ws)
			driver := p.slotDriver(jjSlot)
			jj, isJJ := driver.(jjLifecycle)
			if !isJJ {
				break
			}
			slotCtx, cancel := context.WithTimeout(ctx, slotCleanupTimeout)
			named, held := p.destroyedJJSlot(slotCtx, jj, stack, workspaceName, slot, jjSlot)
			cancel()
			if held {
				item.slot, item.driver = named, driver
			} else {
				logctx.From(ctx).Warn(fmt.Sprintf("removing jj slot %s without forgetting a jj workspace: it is no checkout of %s this stack made", item.dest, jjSlot.Primary))
			}
		case source.Kind == "git":
			item.slot = p.workspaceLifecycleSlot(item.dest, source, ws)
			item.driver = p.slotDriver(item.slot)
		}
		materialized = append(materialized, item)
	}
	// Remove deepest paths first so a source rooted at the workspace does not
	// erase nested destinations before their own drivers remove them.
	sort.Slice(materialized, func(i, j int) bool { return len(materialized[i].dest) > len(materialized[j].dest) })
	for _, item := range materialized {
		rel, err := filepath.Rel(p.root, item.dest)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		dest, err := copierx.OpenGuardedPath(p.root, p.root, filepath.ToSlash(rel), nil)
		if err != nil {
			logctx.From(ctx).Warn(fmt.Sprintf("leaving source slot %s for the workspace's removal: %v", item.dest, err))
			continue
		}
		slotCtx, cancel := context.WithTimeout(ctx, slotCleanupTimeout)
		if err := removeWorkspaceSlot(slotCtx, item.driver, item.slot, dest); err != nil {
			logctx.From(ctx).Warn(fmt.Sprintf("removing source slot %s: %v", item.dest, err))
		}
		cancel()
		_ = dest.Close()
	}
}

type workspaceSourceMaterialization struct {
	slot       string
	sourceName string
	source     manifest.Source
	resolved   manifest.WorkspaceSource
	optional   bool
}

func resolveWorkspaceTemplateSource(spec copierx.TemplateSource, inputs map[string]string, workspaceName string, alloc map[string]int) (manifest.Source, error) {
	ctx := substitute.Context{Inputs: inputs, Name: workspaceName, Alloc: alloc}
	kind, err := substitute.Resolve(spec.Kind, ctx)
	if err != nil {
		return manifest.Source{}, err
	}
	repo, err := substitute.Resolve(spec.Repo, ctx)
	if err != nil {
		return manifest.Source{}, err
	}
	url, err := substitute.Resolve(spec.URL, ctx)
	if err != nil {
		return manifest.Source{}, err
	}
	path, err := substitute.Resolve(spec.Path, ctx)
	if err != nil {
		return manifest.Source{}, err
	}
	defaultRef, err := substitute.Resolve(spec.DefaultRef, ctx)
	if err != nil {
		return manifest.Source{}, err
	}
	cachePath, err := substitute.Resolve(spec.CachePath, ctx)
	if err != nil {
		return manifest.Source{}, err
	}
	pushDefault, err := substitute.Resolve(spec.PushDefault, ctx)
	if err != nil {
		return manifest.Source{}, err
	}
	var remotes map[string]manifest.SourceRemote
	for name, declared := range spec.Remotes {
		remote := manifest.SourceRemote{Canonical: declared.Canonical}
		if remote.URL, err = substitute.Resolve(declared.URL, ctx); err != nil {
			return manifest.Source{}, fmt.Errorf("remote %q url: %w", name, err)
		}
		for _, pattern := range declared.Branches {
			resolved, err := substitute.Resolve(pattern, ctx)
			if err != nil {
				return manifest.Source{}, fmt.Errorf("remote %q branches: %w", name, err)
			}
			remote.Branches = append(remote.Branches, resolved)
		}
		if remotes == nil {
			remotes = map[string]manifest.SourceRemote{}
		}
		remotes[name] = remote
	}
	if kind == "" {
		switch {
		case repo != "" || len(remotes) > 0:
			kind = "git"
		case path != "":
			kind = "local"
		}
	}
	return manifest.Source{Kind: kind, Repo: repo, Remotes: remotes, PushDefault: pushDefault, URL: url, Path: path, DefaultRef: defaultRef, CachePath: cachePath}, nil
}

func (p *Platform) resolveWorkspaceSource(spec copierx.TemplateSource, sourceName string, inputs map[string]string, workspaceName string, alloc map[string]int) (manifest.WorkspaceSource, error) {
	ctx := substitute.Context{Inputs: inputs, Name: workspaceName, Alloc: alloc}
	branch, err := substitute.Resolve(spec.Branch, ctx)
	if err != nil {
		return manifest.WorkspaceSource{}, err
	}
	ref, err := substitute.Resolve(spec.Ref, ctx)
	if err != nil {
		return manifest.WorkspaceSource{}, err
	}
	subpath, err := substitute.Resolve(spec.Subpath, ctx)
	if err != nil {
		return manifest.WorkspaceSource{}, err
	}
	// The mode is substitutable so a stack can choose it through an input
	// without forking the template; it is validated once resolved, before any
	// slot is materialized.
	mode, err := substitute.Resolve(spec.Mode, ctx)
	if err != nil {
		return manifest.WorkspaceSource{}, err
	}
	if err := manifest.ValidateWorkspaceSourceMode(mode); err != nil {
		return manifest.WorkspaceSource{}, err
	}
	return manifest.WorkspaceSource{Source: sourceName, Mode: mode, Branch: branch, Ref: ref, Subpath: subpath}, nil
}

func (p *Platform) materializeWorkspaceSource(ctx context.Context, sourceName string, source manifest.Source, ws manifest.WorkspaceSource, dest string, sync bool, jj jjSlotTarget) (*workspaceSourceCleanupEntry, error) {
	rel, err := filepath.Rel(p.root, dest)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("workspace source destination %q escapes stack root", dest)
	}
	destGuard, err := copierx.OpenGuardedPath(p.root, p.root, filepath.ToSlash(rel), nil)
	if err != nil {
		return nil, fmt.Errorf("validate workspace source destination %s: %w", dest, err)
	}
	retainGuard := false
	defer func() {
		if !retainGuard {
			_ = destGuard.Close()
		}
	}()
	complete := func(driver vcs.Driver, slot vcs.Slot) (*workspaceSourceCleanupEntry, error) {
		retainGuard = true
		return &workspaceSourceCleanupEntry{dest: destGuard, driver: driver, slot: slot}, nil
	}
	jjSlot := ws.Mode == manifest.WorkspaceSourceModeJJ
	destExists, destEmpty := false, false
	var destMode fs.FileMode
	if info, exists, err := destGuard.Lstat(); err != nil {
		return nil, err
	} else if exists {
		destExists, destMode = true, info.Mode()
		if (source.Kind != "git" && !jjSlot) || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, fmt.Errorf("workspace source destination %s already exists", dest)
		}
		empty, err := destGuard.IsEmptyDirectory()
		if err != nil {
			return nil, err
		}
		destEmpty = empty
		// A non-empty leftover only blocks the create when we are not asked to
		// reconcile it; with sync, a worktree source reclaims the path below,
		// and a jj source keeps its own checkout there.
		if !empty && (!sync || (ws.Mode != manifest.WorkspaceSourceModeWorktree && !jjSlot)) {
			return nil, fmt.Errorf("workspace source destination %s already exists and is not empty", dest)
		}
	}
	switch {
	case jjSlot:
		driver, slot, kept, err := p.materializeJJSlot(ctx, destGuard, sourceName, source, ws, dest, jj, destExists && !destEmpty, sync)
		if !destEmpty {
			destMode = 0
		}
		if err != nil {
			// jj makes a slot in place, so a failed add removed the empty
			// directory it was made in; put it back, as a failed git add
			// leaves it.
			return nil, errors.Join(err, restoreEmptyDir(destGuard, destMode))
		}
		entry, _ := complete(driver, slot)
		entry.kept, entry.emptyDir = kept, destMode
		return entry, nil
	case source.Kind == "git":
		slot := p.workspaceLifecycleSlot(dest, source, ws)
		driver := p.slotDriver(slot)
		if slot.Mode == vcs.ModeWorktree {
			insideDest, err := sameOrNestedPath(dest, slot.Primary)
			if err != nil {
				return nil, err
			}
			if insideDest {
				return nil, fmt.Errorf("workspace source cache path %s cannot be inside destination %s", slot.Primary, dest)
			}
			if err := withPrimaryLock(ctx, slot.Primary, func() error {
				return p.addWorktreeSlot(ctx, driver, slot, destGuard, sourceName, source, destExists && !destEmpty)
			}); err != nil {
				return nil, err
			}
			return complete(driver, slot)
		}
		// A clone slot is a repository of its own, so it gets the source's
		// remotes as the cache does, the canonical push block included, before
		// it is installed. Fresh mode only writes, so the holder needs only the
		// source for errors.
		configure := func(ctx context.Context, checkout string) error {
			return syncSourceRemotes(ctx, p.gitClient(), remoteHolder{source: sourceName}, checkout, source, remoteSyncFresh)
		}
		if err := driver.Add(ctx, slot, destGuard, configure); err != nil {
			return nil, err
		}
		return complete(driver, slot)
	case source.Kind == "local":
		target, err := workspaceLocalSymlinkTarget(p.sourcePath(sourceName, source), dest)
		if err != nil {
			return nil, err
		}
		if err := destGuard.Symlink(target); err != nil {
			return nil, err
		}
		return complete(nil, vcs.Slot{})
	default:
		return nil, fmt.Errorf("workspace source kind %q is not implemented", source.Kind)
	}
}

// addWorktreeSlot cuts a worktree slot under its cache's primary lock: it
// refreshes the cache, settles what an earlier create left at the slot path,
// and has the driver add the worktree. A worktree the cache still lists at
// the path is a leftover, deregistered with whatever is left of its checkout
// (populated reaches here only with --sync). A populated directory the cache
// does not list is not a leftover, and is refused.
func (p *Platform) addWorktreeSlot(ctx context.Context, driver vcs.Driver, slot vcs.Slot, dest *copierx.GuardedPath, sourceName string, source manifest.Source, populated bool) error {
	if err := p.materializeSourceLocked(ctx, sourceName, source, true); err != nil {
		return err
	}
	registered, err := driver.Exists(ctx, slot)
	if err != nil {
		return err
	}
	if registered {
		if err := driver.Remove(ctx, slot, dest); err != nil {
			return err
		}
	} else if populated {
		return fmt.Errorf("workspace source destination %s already exists and is not empty", slot.Path)
	}
	return driver.Add(ctx, slot, dest, nil)
}

func normalizeWorkspaceSubpath(subpath string) (string, error) {
	subpath = strings.TrimSpace(subpath)
	if subpath == "" {
		return "", fmt.Errorf("is required")
	}
	clean := filepath.Clean(filepath.FromSlash(subpath))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%q escapes the workspace root", subpath)
	}
	return filepath.ToSlash(clean), nil
}

func (p *Platform) workspaceSourcePath(workspaceName, slot string, wsSource manifest.WorkspaceSource) (string, string, error) {
	subpath := wsSource.Subpath
	if subpath == "" {
		subpath = slot
	}
	normalized, err := normalizeWorkspaceSubpath(subpath)
	if err != nil {
		return subpath, "", fmt.Errorf("invalid subpath: %w", err)
	}
	return normalized, filepath.Join(p.root, "workspaces", workspaceName, filepath.FromSlash(normalized)), nil
}

func orderWorkspaceSourceMaterializations(items []workspaceSourceMaterialization) ([]workspaceSourceMaterialization, error) {
	ordered := make([]workspaceSourceMaterialization, 0, len(items))
	rootCount := 0
	for _, item := range items {
		if item.resolved.Subpath == "." {
			if item.source.Kind != "git" {
				return nil, fmt.Errorf("workspace source %q uses subpath %q, which is only supported for git sources", item.slot, item.resolved.Subpath)
			}
			rootCount++
			ordered = append(ordered, item)
		}
	}
	if rootCount > 1 {
		return nil, fmt.Errorf("only one workspace source can use subpath %q", ".")
	}
	for _, item := range items {
		if item.resolved.Subpath != "." {
			ordered = append(ordered, item)
		}
	}
	return ordered, nil
}

func sameOrNestedPath(parent, child string) (bool, error) {
	absParent, err := filepath.Abs(parent)
	if err != nil {
		return false, err
	}
	absChild, err := filepath.Abs(child)
	if err != nil {
		return false, err
	}
	rel, err := filepath.Rel(absParent, absChild)
	if err != nil {
		return false, err
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))), nil
}

func workspaceLocalSymlinkTarget(sourcePath, dest string) (string, error) {
	target, err := filepath.Rel(filepath.Dir(dest), sourcePath)
	if err != nil {
		return "", fmt.Errorf("local source symlink target: %w", err)
	}
	return target, nil
}

func (p *Platform) allocateWorkspacePorts(stack *manifest.Stack, workspaceName string) (map[string]int, error) {
	alloc := map[string]int{}
	if len(stack.Operator.PortPool) == 0 {
		return alloc, nil
	}
	pools, err := ports.FromManifest(stack.Operator.PortPool, stack.PortLeases)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if stack.PortLeases == nil {
		stack.PortLeases = map[string][]manifest.PortLease{}
	}
	for _, name := range sortedKeys(pools) {
		owner := "workspace/" + workspaceName + "/" + name
		port, err := pools[name].AllocateAvailable(owner, p.portUnavailable)
		if err != nil {
			return nil, err
		}
		alloc[name] = port
		leases := stack.PortLeases[name]
		found := false
		for i := range leases {
			if leases[i].Owner == owner {
				leases[i].Port = port
				found = true
			}
		}
		if !found {
			leases = append(leases, manifest.PortLease{Port: port, Owner: owner, CreatedAt: now})
		}
		stack.PortLeases[name] = leases
	}
	return alloc, nil
}

func workspaceInputs(metadata copierx.Metadata, provided map[string]string) map[string]string {
	inputs := map[string]string{}
	for key, spec := range metadata.Inputs {
		if spec.Default != nil {
			inputs[key] = fmt.Sprint(spec.Default)
		}
	}
	for key, value := range provided {
		inputs[key] = value
	}
	return inputs
}

// stackWorkspaceDefaults returns the stack's `workspace_defaults` inputs for
// templateRef, the ref resolveTemplate reports (e.g. "workspaces/src"). Keys
// match the way `--template` does: a bare name means workspaces/<name>. Keys
// that normalize to the same ref layer in sorted order. Nil when the stack
// declares nothing for the template.
func stackWorkspaceDefaults(stack *manifest.Stack, templateRef string) map[string]string {
	if stack == nil || templateRef == "" {
		return nil
	}
	want := normalizeWorkspaceTemplateRef(templateRef)
	var inputs map[string]string
	for _, key := range sortedKeys(stack.WorkspaceDefaults) {
		if normalizeWorkspaceTemplateRef(key) != want {
			continue
		}
		inputs = mergeStringMaps(inputs, stack.WorkspaceDefaults[key].Inputs)
	}
	return inputs
}

// normalizeWorkspaceTemplateRef mirrors resolveTemplate's local-ref rule: a
// bare name resolves under the workspaces family. Every other form (a family
// path, an absolute path, a registry or remote ref) compares verbatim.
func normalizeWorkspaceTemplateRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref != "" && !strings.Contains(ref, "/") {
		return "workspaces/" + ref
	}
	return ref
}

// mergeStringMaps layers maps left to right (later keys win). Nil layers are
// skipped; the result is nil when every layer is empty.
func mergeStringMaps(layers ...map[string]string) map[string]string {
	var out map[string]string
	for _, layer := range layers {
		for key, value := range layer {
			if out == nil {
				out = map[string]string{}
			}
			out[key] = value
		}
	}
	return out
}

func (p *Platform) workspaceName(metadata copierx.Metadata, explicit string, inputs map[string]string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	pattern := metadata.InstanceNaming.Pattern
	if pattern == "" {
		pattern = metadata.InstanceNaming.Fallback
	}
	if pattern == "" {
		pattern = "${inputs.name | slug}"
	}
	name, err := substitute.Resolve(pattern, substitute.Context{Inputs: inputs})
	if err != nil {
		return "", err
	}
	if max := metadata.InstanceNaming.MaxLength; max > 0 && len(name) > max {
		name = name[:max]
		name = strings.Trim(name, "-_")
	}
	if name == "" {
		return "", fmt.Errorf("workspace name resolved empty")
	}
	return name, nil
}

func (p *Platform) resolveTemplate(ctx context.Context, ref, kind string) (path, activeRef string, retErr error) {
	finish := logctx.Step(ctx, "resolving template "+logctx.RedactURL(ref))
	defer func() { finish(retErr) }()
	if ref == "" {
		return "", "", fmt.Errorf("template reference is empty")
	}
	if isRemoteTemplateRef(ref) {
		return p.resolveRemoteTemplate(ctx, ref, kind)
	}
	if filepath.IsAbs(ref) {
		if _, err := os.Stat(ref); err != nil {
			return "", "", err
		}
		return ref, ref, nil
	}
	if base, pin := splitTemplateRefPin(ref); pin != "" || strings.Contains(base, "//") {
		// A pinned ref or an explicit owner/repo//subpath never resolves
		// locally — it names a repository state, so it goes straight to the
		// registry resolver.
		return p.resolveRegistryTemplate(ctx, ref, kind)
	}
	family := kind + "s"
	kindRef := ref
	if !strings.Contains(ref, "/") {
		kindRef = family + "/" + ref
	}
	if !strings.HasPrefix(kindRef, family+"/") {
		return "", "", templateKindError(ref, kind)
	}
	candidates := []string{
		filepath.Join(p.root, ".templates", kindRef),
		filepath.Join(p.root, "templates", kindRef),
		filepath.Join(p.root, kindRef),
		filepath.Join(p.root, ref),
	}
	candidates = append(candidates, ancestorTemplatePaths(p.root, kindRef)...)
	if cwd, err := os.Getwd(); err == nil && cwd != p.root {
		candidates = append(candidates,
			filepath.Join(cwd, ".templates", kindRef),
			filepath.Join(cwd, "templates", kindRef),
		)
		candidates = append(candidates, ancestorTemplatePaths(cwd, kindRef)...)
	}
	seen := map[string]bool{}
	for _, candidate := range candidates {
		if seen[candidate] {
			continue
		}
		seen[candidate] = true
		if _, err := os.Stat(filepath.Join(candidate, "copier.yml")); err == nil {
			return candidate, kindRef, nil
		}
	}
	// Every local candidate missed: fall through to the template registry so a
	// clean machine resolves the conventional names remotely.
	path, activeRef, err := p.resolveRegistryTemplate(ctx, ref, kind)
	if err != nil {
		return "", "", registryFallbackError(ref, err)
	}
	return path, activeRef, nil
}

// ancestorTemplatePaths walks up from start (exclusive) and returns
// "<ancestor>/.templates/<kindRef>" for each ancestor up to the
// filesystem root, capped at 32 levels of nesting as a safety net.
//
// This lets `angee` find templates declared at a monorepo's root from
// subdirectories — e.g. running from `<repo>/examples/foo/` finds
// `<repo>/.templates/stacks/dev`. The first existing match wins,
// preserving the legacy "p.root then cwd" precedence by virtue of the
// caller-supplied ordering.
func ancestorTemplatePaths(start, kindRef string) []string {
	paths := []string{}
	dir := start
	for i := 0; i < 32; i++ {
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		paths = append(paths, filepath.Join(parent, ".templates", kindRef))
		dir = parent
	}
	return paths
}
