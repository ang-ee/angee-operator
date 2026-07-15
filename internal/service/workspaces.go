package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/copierx"
	"github.com/ang-ee/angee-operator/internal/git"
	"github.com/ang-ee/angee-operator/internal/manifest"
	mountx "github.com/ang-ee/angee-operator/internal/mount"
	"github.com/ang-ee/angee-operator/internal/ports"
	"github.com/ang-ee/angee-operator/internal/query"
	"github.com/ang-ee/angee-operator/internal/queryfields"
	"github.com/ang-ee/angee-operator/internal/substitute"
)

const (
	workspaceSourceStateBranchMismatch = "branch-mismatch"
	workspaceSyncBaseMerge             = "merge"
	workspaceSyncBaseRebase            = "rebase"
)

func (p *Platform) WorkspaceCreate(ctx context.Context, req api.WorkspaceCreateRequest) (api.WorkspaceRef, error) {
	if req.Template == "" {
		return api.WorkspaceRef{}, &InvalidInputError{Field: "template", Reason: "workspace template is required"}
	}
	stack, err := p.loadOrCreateWorkspaceStack()
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	templatePath, templateRef, err := p.resolveTemplate(ctx, req.Template, "workspace")
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	metadata, err := copierx.ValidateMetadata(templatePath, "workspace")
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	if err := manifest.Ensure(stack, metadata.Ensure); err != nil {
		return api.WorkspaceRef{}, err
	}
	inputs := workspaceInputs(metadata, req.Inputs)
	name, err := p.workspaceName(metadata, req.Name, inputs)
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	if _, exists := stack.Workspaces[name]; exists {
		return api.WorkspaceRef{}, &ConflictError{Kind: "workspace", Name: name, Reason: "already exists"}
	}
	allocations, err := p.allocateWorkspacePorts(stack, name)
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	workspacePath := filepath.Join(p.root, "workspaces", name)
	// A pre-existing workspace directory is a leftover from an earlier failed
	// create; rollback below must not delete it (that leftover is what sync is
	// gated to protect), only the worktrees this create itself materializes.
	_, statErr := os.Stat(workspacePath)
	workspacePreexisted := statErr == nil
	if err := os.MkdirAll(workspacePath, 0o755); err != nil {
		return api.WorkspaceRef{}, err
	}
	committed := false
	var workspaceSources map[string]manifest.WorkspaceSource
	var rollbackTemplateFiles func() error
	var rollbackTemplateDocuments func() error
	statePath := renderPlanStatePath(p.root, "workspaces", name)
	defer func() {
		if committed {
			return
		}
		if rollbackTemplateDocuments != nil {
			_ = rollbackTemplateDocuments()
		}
		if rollbackTemplateFiles != nil {
			_ = rollbackTemplateFiles()
		}
		_ = os.Remove(statePath)
		// Roll back only what this create materialized: deregister the worktrees
		// it added (which also deletes their working trees), and remove the
		// workspace directory only when this create created it. A create that
		// failed at the "already exists" guard materialized nothing, so its
		// workspaceSources is empty and a pre-existing leftover is left intact.
		// Use a fresh context so cleanup runs even when the request context is
		// what was cancelled.
		p.removeWorkspaceWorktrees(context.Background(), stack, workspacePath, workspaceSources)
		if !workspacePreexisted {
			_ = os.RemoveAll(workspacePath)
		}
	}()
	workspaceSources, err = p.materializeWorkspaceSources(ctx, stack, name, workspacePath, metadata, inputs, allocations, req.Sync)
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	renderPlan, err := p.buildWorkspaceRenderPlan(ctx, workspacePath, templatePath, templateRef, metadata, inputs, name, allocations, workspaceSources, statePath)
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	prepared, err := copierx.PrepareReconcile(ctx, renderPlan.Plan, copierx.ReconcileOptions{Mode: copierx.ReconcileCreate})
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	defer func() { _ = prepared.Close() }()
	rollbackTemplateFiles, err = prepared.ApplyFiles()
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	renderedDocuments := make(map[string][]byte, len(renderPlan.Documents))
	for _, document := range renderPlan.Documents {
		data, ok := prepared.RenderedDocument(document.Path)
		if !ok {
			return api.WorkspaceRef{}, fmt.Errorf("workspace chain did not render %s", document.Path)
		}
		if _, err := decodeStackDocument(data); err != nil {
			return api.WorkspaceRef{}, fmt.Errorf("load rendered workspace stack %s: %w", document.Path, err)
		}
		renderedDocuments[document.Path] = data
	}
	rollbackTemplateDocuments, err = applyRenderedDocuments(workspacePath, renderedDocuments, false)
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	if err := materializePersistPaths(workspacePath, metadata.Persist); err != nil {
		return api.WorkspaceRef{}, err
	}
	workspace := manifest.Workspace{
		Template: templateRef,
		Inputs:   map[string]string(inputs),
		Sources:  workspaceSources,
		Resolved: manifest.WorkspaceResolved{
			Chain:        renderPlan.Chain,
			ChainRoot:    renderPlan.ChainRoot,
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
	if err := manifest.SaveFile(manifest.Path(p.root), stack); err != nil {
		return api.WorkspaceRef{}, err
	}
	if err := prepared.SaveState(); err != nil {
		delete(stack.Workspaces, name)
		_ = manifest.SaveFile(manifest.Path(p.root), stack)
		return api.WorkspaceRef{}, err
	}
	committed = true
	return workspaceRef(name, workspacePath, workspace), nil
}

func (p *Platform) loadOrCreateWorkspaceStack() (*manifest.Stack, error) {
	stack, err := p.LoadStack()
	if err == nil {
		return stack, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	return p.EmptyStack(defaultWorkspaceStackName(p.root)), nil
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
		Chain:              append([]string{}, workspace.Resolved.Chain...),
		ChainRoot:          workspace.Resolved.ChainRoot,
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
	if workspace.Resolved.ChainRoot != "" {
		innerRoot := filepath.Join(path, workspace.Resolved.ChainRoot)
		if _, err := os.Stat(manifest.Path(innerRoot)); err != nil {
			status.InnerError = err.Error()
		} else {
			inner, err := New(innerRoot)
			if err != nil {
				status.InnerError = err.Error()
			} else if innerStatus, err := inner.StackStatus(ctx); err != nil {
				status.InnerError = err.Error()
			} else {
				status.InnerStack = &innerStatus
			}
		}
	}
	return status
}

func (p *Platform) workspaceSourceStatus(ctx context.Context, workspaceName, slot string, wsSource manifest.WorkspaceSource, stack *manifest.Stack) api.WorkspaceSourceStatus {
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
	if source.Kind != "git" {
		status.State = "ready"
		return status
	}
	client := git.New()
	currentRef, err := client.CurrentRef(ctx, path)
	if err != nil {
		status.State = "error"
		status.Pushed = false
		status.Error = err.Error()
		return status
	}
	status.CurrentRef = currentRef
	dirty, err := client.Dirty(ctx, path)
	if err != nil {
		status.State = "error"
		status.Pushed = false
		status.Error = err.Error()
		return status
	}
	status.Dirty = dirty
	if reason := workspaceGitBranchMismatchReason(currentRef, wsSource); reason != "" {
		status.State = workspaceSourceStateBranchMismatch
		status.Pushed = false
		status.UnpushedReason = reason
		return status
	}
	if dirty {
		status.State = "dirty"
		status.Pushed = false
		status.UnpushedReason = "uncommitted changes"
		return status
	}
	base, hasUpstream, err := client.Upstream(ctx, path)
	if err != nil {
		status.State = "error"
		status.Pushed = false
		status.Error = err.Error()
		return status
	}
	if hasUpstream {
		status.Upstream = base
	}
	if base == "" {
		base = wsSource.Ref
	}
	if base == "" {
		base = source.DefaultRef
	}
	if base == "" {
		status.State = "clean"
		return status
	}
	ahead, behind, err := client.AheadBehind(ctx, path, base)
	if err != nil {
		status.State = "error"
		status.Pushed = false
		status.Error = err.Error()
		return status
	}
	status.Ahead = ahead
	status.Behind = behind
	switch {
	case ahead > 0 && behind > 0:
		status.State = "diverged"
		status.Pushed = false
		status.UnpushedReason = fmt.Sprintf("%d commit(s) ahead of %s", ahead, base)
	case ahead > 0:
		status.State = "ahead"
		status.Pushed = false
		if hasUpstream {
			status.UnpushedReason = fmt.Sprintf("%d commit(s) ahead of %s", ahead, base)
		} else {
			status.UnpushedReason = fmt.Sprintf("%d commit(s) ahead of base ref %s with no upstream", ahead, base)
		}
	case behind > 0:
		status.State = "behind"
	default:
		status.State = "clean"
	}
	return status
}

func (p *Platform) WorkspaceDestroy(ctx context.Context, name string, purge bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	stack, err := p.LoadStack()
	if err != nil {
		return err
	}
	workspace, ok := stack.Workspaces[name]
	if !ok {
		return &NotFoundError{Kind: "workspace", Name: name}
	}
	if err := p.ensureWorkspaceGitSourcesOnExpectedBranches(ctx, name, workspace, stack); err != nil {
		return err
	}
	if err := p.ensureWorkspaceGitSourcesPushed(ctx, name, workspace, stack); err != nil {
		return err
	}
	delete(stack.Workspaces, name)
	releaseWorkspacePorts(stack, name)
	if err := manifest.SaveFile(manifest.Path(p.root), stack); err != nil {
		return err
	}
	if purge {
		workspacePath := filepath.Join(p.root, "workspaces", name)
		// Deregister the worktrees first so purging the directory does not
		// strand "missing but already registered" entries in the shared caches.
		p.removeWorkspaceWorktrees(ctx, stack, workspacePath, workspace.Sources)
		return os.RemoveAll(workspacePath)
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
	currentRef, err := git.New().CurrentRef(ctx, path)
	if err != nil {
		return err
	}
	if reason := workspaceGitBranchMismatchReason(currentRef, wsSource); reason != "" {
		return fmt.Errorf("workspace %q source %q has branch mismatch: %s at %s", workspaceName, slot, reason, path)
	}
	return nil
}

func workspaceSourceRequiresBranch(wsSource manifest.WorkspaceSource) bool {
	return wsSource.Mode == "worktree" && wsSource.Branch != ""
}

func workspaceGitBranchMismatchReason(currentRef string, wsSource manifest.WorkspaceSource) string {
	if !workspaceSourceRequiresBranch(wsSource) || currentRef == wsSource.Branch {
		return ""
	}
	return fmt.Sprintf("current branch/ref %q, expected workspace branch %q", currentRef, wsSource.Branch)
}

func (p *Platform) ensureWorkspaceGitSourcesPushed(ctx context.Context, workspaceName string, workspace manifest.Workspace, stack *manifest.Stack) error {
	client := git.New()
	unpushed := []string{}
	for _, slot := range sortedKeys(workspace.Sources) {
		wsSource := workspace.Sources[slot]
		source, ok := stack.Sources[wsSource.Source]
		if !ok {
			return fmt.Errorf("workspace %q source %q references undeclared source %q", workspaceName, slot, wsSource.Source)
		}
		if source.Kind != "git" {
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
		reason, err := workspaceGitSourceUnpushedReason(ctx, client, path, source, wsSource)
		if err != nil {
			return fmt.Errorf("workspace %q source %q: %w", workspaceName, slot, err)
		}
		if reason != "" {
			unpushed = append(unpushed, fmt.Sprintf("%s (%s)", slot, reason))
		}
	}
	if len(unpushed) > 0 {
		return fmt.Errorf("workspace %q has git sources that have not been pushed: %s", workspaceName, strings.Join(unpushed, ", "))
	}
	return nil
}

func workspaceGitSourceUnpushedReason(ctx context.Context, client git.Client, path string, source manifest.Source, wsSource manifest.WorkspaceSource) (string, error) {
	dirty, err := client.Dirty(ctx, path)
	if err != nil {
		return "", err
	}
	if dirty {
		return "uncommitted changes", nil
	}
	base, hasUpstream, err := client.Upstream(ctx, path)
	if err != nil {
		return "", err
	}
	if base == "" {
		base = wsSource.Ref
	}
	if base == "" {
		base = source.DefaultRef
	}
	if base == "" {
		return "", nil
	}
	ahead, err := client.AheadCount(ctx, path, base)
	if err != nil {
		return "", err
	}
	if ahead == 0 {
		return "", nil
	}
	if hasUpstream {
		return fmt.Sprintf("%d commit(s) ahead of %s", ahead, base), nil
	}
	return fmt.Sprintf("%d commit(s) ahead of base ref %s with no upstream", ahead, base), nil
}

func (p *Platform) WorkspaceUpdate(ctx context.Context, name string, req api.WorkspaceUpdateRequest) (api.WorkspaceRef, error) {
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
	parentBefore, err := manifest.Marshal(stack)
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	templatePath, templateRef, err := p.resolveTemplate(ctx, workspace.Template, "workspace")
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	metadata, err := copierx.ValidateMetadata(templatePath, "workspace")
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	if err := manifest.Ensure(stack, metadata.Ensure); err != nil {
		return api.WorkspaceRef{}, err
	}
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
	renderPlan, err := p.buildWorkspaceRenderPlan(ctx, workspacePath, templatePath, templateRef, metadata, workspace.Inputs, name, workspace.Resolved.Allocations, workspace.Sources, statePath)
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	prepared, err := copierx.PrepareReconcile(ctx, renderPlan.Plan, copierx.ReconcileOptions{
		Mode: copierx.ReconcileUpdate, Overwrite: req.Overwrite,
	})
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	defer func() { _ = prepared.Close() }()
	if conflicts := prepared.Result().Conflicts; len(conflicts) != 0 {
		paths := make([]string, 0, len(conflicts))
		for _, conflict := range conflicts {
			paths = append(paths, conflict.Path)
		}
		return api.WorkspaceRef{}, &ConflictError{Kind: "workspace-template", Name: name, Reason: fmt.Sprintf("locally modified paths: %s; use --overwrite to replace", strings.Join(paths, ", "))}
	}
	mergedDocuments := make(map[string][]byte, len(renderPlan.Documents))
	for _, document := range renderPlan.Documents {
		rendered, ok := prepared.RenderedDocument(document.Path)
		if !ok {
			return api.WorkspaceRef{}, fmt.Errorf("workspace chain did not render %s", document.Path)
		}
		theirs, err := decodeStackDocument(rendered)
		if err != nil {
			return api.WorkspaceRef{}, fmt.Errorf("load rendered workspace stack %s: %w", document.Path, err)
		}
		destination := filepath.Join(workspacePath, filepath.FromSlash(document.Path))
		merged := theirs
		if current, loadErr := manifest.LoadFile(destination); loadErr == nil {
			merged = mergeStackFromTemplate(current, theirs, true)
		} else if !os.IsNotExist(loadErr) {
			return api.WorkspaceRef{}, fmt.Errorf("load workspace stack %s: %w", document.Path, loadErr)
		}
		documentMetadata, err := copierx.ReadMetadata(document.Template)
		if err != nil {
			return api.WorkspaceRef{}, err
		}
		if err := manifest.Ensure(merged, documentMetadata.Ensure); err != nil {
			return api.WorkspaceRef{}, err
		}
		mergedDocuments[document.Path], err = manifest.Marshal(merged)
		if err != nil {
			return api.WorkspaceRef{}, err
		}
	}
	rollbackFiles, err := prepared.ApplyFiles()
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	rollbackDocuments, err := applyRenderedDocuments(workspacePath, mergedDocuments, false)
	if err != nil {
		_ = rollbackFiles()
		return api.WorkspaceRef{}, err
	}
	if err := materializePersistPaths(workspacePath, metadata.Persist); err != nil {
		_ = rollbackDocuments()
		_ = rollbackFiles()
		return api.WorkspaceRef{}, err
	}
	workspace.Template = templateRef
	workspace.Resolved.Chain = renderPlan.Chain
	workspace.Resolved.ChainRoot = renderPlan.ChainRoot
	workspace.Resolved.PersistPaths = metadata.Persist
	stack.Workspaces[name] = workspace
	if err := manifest.SaveFile(manifest.Path(p.root), stack); err != nil {
		_ = rollbackDocuments()
		_ = rollbackFiles()
		return api.WorkspaceRef{}, err
	}
	if err := prepared.SaveState(); err != nil {
		_ = writeRenderedDocument(manifest.Path(p.root), parentBefore)
		_ = rollbackDocuments()
		_ = rollbackFiles()
		return api.WorkspaceRef{}, err
	}
	return workspaceRef(name, workspacePath, workspace), nil
}

func (p *Platform) WorkspaceLogs(ctx context.Context, name string, follow bool) (<-chan string, error) {
	return p.WorkspaceLogsLimited(ctx, name, follow, 0)
}

func (p *Platform) WorkspaceLogsLimited(ctx context.Context, name string, follow bool, maxBytes int) (<-chan string, error) {
	stack, err := p.LoadStack()
	if err != nil {
		return nil, err
	}
	workspace, ok := stack.Workspaces[name]
	if !ok {
		return nil, &NotFoundError{Kind: "workspace", Name: name}
	}
	if workspace.Resolved.ChainRoot == "" {
		ch := make(chan string)
		close(ch)
		return ch, nil
	}
	inner, err := New(filepath.Join(p.root, "workspaces", name, workspace.Resolved.ChainRoot))
	if err != nil {
		return nil, err
	}
	return inner.StackLogsLimited(ctx, nil, follow, maxBytes)
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

func materializePersistPaths(workspacePath string, persist map[string]manifest.PersistPath) error {
	for _, key := range sortedKeys(persist) {
		entry := persist[key]
		if entry.Subpath == "" {
			continue
		}
		dir := filepath.Join(workspacePath, filepath.FromSlash(entry.Subpath))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("persist %q: %w", key, err)
		}
	}
	return nil
}

func workspaceRef(name, path string, ws manifest.Workspace) api.WorkspaceRef {
	processComposePort, playwrightMCPName, playwrightMCPURL := workspaceRuntimeFacts(name, ws)
	return api.WorkspaceRef{
		Name:               name,
		Path:               path,
		Template:           ws.Template,
		ChainRoot:          ws.Resolved.ChainRoot,
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

func (p *Platform) WorkspacePush(ctx context.Context, name, ref string) ([]api.SourceState, error) {
	stack, err := p.LoadStack()
	if err != nil {
		return nil, err
	}
	workspace, ok := stack.Workspaces[name]
	if !ok {
		return nil, &NotFoundError{Kind: "workspace", Name: name}
	}
	if err := p.ensureWorkspaceGitSourcesOnExpectedBranches(ctx, name, workspace, stack); err != nil {
		return nil, err
	}
	client := git.New()
	states := []api.SourceState{}
	for _, slot := range sortedKeys(workspace.Sources) {
		wsSource := workspace.Sources[slot]
		if wsSource.Mode != "worktree" && wsSource.Mode != "clone" {
			continue
		}
		source, ok := stack.Sources[wsSource.Source]
		if !ok || source.Kind != "git" {
			continue
		}
		_, path, err := p.workspaceSourcePath(name, slot, wsSource)
		if err != nil {
			return nil, fmt.Errorf("workspace %q source %q: %w", name, slot, err)
		}
		dirty, err := client.Dirty(ctx, path)
		if err != nil {
			return nil, err
		}
		if dirty {
			return nil, fmt.Errorf("workspace %q source %q has uncommitted changes", name, slot)
		}
		pushRef := ref
		if pushRef == "" {
			pushRef = wsSource.Branch
		}
		if ref == "" {
			_, hasUpstream, upstreamErr := client.Upstream(ctx, path)
			if upstreamErr != nil {
				return nil, upstreamErr
			}
			if hasUpstream {
				err = client.Push(ctx, path, "")
			} else if pushRef != "" && wsSource.Branch != "" {
				err = client.PushSetUpstream(ctx, path, pushRef)
			} else {
				err = client.Push(ctx, path, pushRef)
			}
		} else {
			err = client.Push(ctx, path, pushRef)
		}
		if err != nil {
			return nil, err
		}
		state, err := p.workspaceSourceState(ctx, name, slot, stack, wsSource)
		if err != nil {
			return nil, err
		}
		states = append(states, state)
	}
	return states, nil
}

func (p *Platform) WorkspaceSyncBase(ctx context.Context, name, method string) ([]api.SourceState, error) {
	stack, err := p.LoadStack()
	if err != nil {
		return nil, err
	}
	workspace, ok := stack.Workspaces[name]
	if !ok {
		return nil, &NotFoundError{Kind: "workspace", Name: name}
	}
	method = normalizeWorkspaceSyncBaseMethod(method)
	if method == "" {
		return nil, &InvalidInputError{Field: "method", Reason: fmt.Sprintf("workspace sync-base method must be %q or %q", workspaceSyncBaseMerge, workspaceSyncBaseRebase)}
	}
	if err := p.ensureWorkspaceGitSourcesOnExpectedBranches(ctx, name, workspace, stack); err != nil {
		return nil, err
	}
	client := git.New()
	states := []api.SourceState{}
	for _, slot := range sortedKeys(workspace.Sources) {
		wsSource := workspace.Sources[slot]
		if wsSource.Mode != "worktree" {
			continue
		}
		source, ok := stack.Sources[wsSource.Source]
		if !ok || source.Kind != "git" {
			continue
		}
		_, path, err := p.workspaceSourcePath(name, slot, wsSource)
		if err != nil {
			return nil, fmt.Errorf("workspace %q source %q: %w", name, slot, err)
		}
		dirty, err := client.Dirty(ctx, path)
		if err != nil {
			return nil, err
		}
		if dirty {
			return nil, fmt.Errorf("workspace %q source %q has uncommitted changes", name, slot)
		}
		baseRef := workspaceSourceBaseRef(source, wsSource)
		if baseRef == "" {
			return nil, fmt.Errorf("workspace %q source %q has no base ref", name, slot)
		}
		if err := client.Fetch(ctx, path); err != nil {
			return nil, err
		}
		syncRef, err := client.SyncBaseRef(ctx, path, baseRef)
		if err != nil {
			return nil, err
		}
		switch method {
		case workspaceSyncBaseRebase:
			err = client.Rebase(ctx, path, syncRef)
		default:
			err = client.Merge(ctx, path, syncRef)
		}
		if err != nil {
			return nil, err
		}
		state, err := p.workspaceSourceState(ctx, name, slot, stack, wsSource)
		if err != nil {
			return nil, err
		}
		states = append(states, state)
	}
	return states, nil
}

func normalizeWorkspaceSyncBaseMethod(method string) string {
	switch strings.ToLower(strings.TrimSpace(method)) {
	case "", workspaceSyncBaseMerge:
		return workspaceSyncBaseMerge
	case workspaceSyncBaseRebase:
		return workspaceSyncBaseRebase
	default:
		return ""
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
	}
}

func (p *Platform) materializeWorkspaceSources(ctx context.Context, stack *manifest.Stack, workspaceName, workspacePath string, metadata copierx.Metadata, inputs map[string]string, alloc map[string]int, sync bool) (map[string]manifest.WorkspaceSource, error) {
	result := map[string]manifest.WorkspaceSource{}
	items := []workspaceSourceMaterialization{}
	for _, slot := range sortedKeys(metadata.Sources) {
		spec := metadata.Sources[slot]
		sourceName := spec.Source
		if sourceName == "" {
			sourceName = slot
		}
		source, ok := stack.Sources[sourceName]
		if !ok {
			var err error
			source, err = resolveWorkspaceTemplateSource(spec, inputs, workspaceName, alloc)
			if err != nil {
				return nil, err
			}
			if source.Kind != "" {
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
			return nil, fmt.Errorf("workspace source %q references undeclared source %q", slot, sourceName)
		}
		resolved, err := p.resolveWorkspaceSource(spec, sourceName, inputs, workspaceName, alloc)
		if err != nil {
			return nil, err
		}
		if resolved.Subpath == "" {
			resolved.Subpath = slot
		}
		resolved.Subpath, err = normalizeWorkspaceSubpath(resolved.Subpath)
		if err != nil {
			return nil, fmt.Errorf("workspace source %q subpath: %w", slot, err)
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
		return nil, err
	}
	for _, item := range orderedItems {
		dest := filepath.Join(workspacePath, filepath.FromSlash(item.resolved.Subpath))
		if err := p.materializeWorkspaceSource(ctx, item.sourceName, item.source, item.resolved, dest, sync); err != nil {
			if item.optional {
				continue
			}
			// Return the worktrees materialized so far alongside the error so
			// the caller's rollback can undo them; this is the single cleanup
			// site for a failed create.
			return result, err
		}
		result[item.slot] = item.resolved
	}
	return result, nil
}

// removeWorkspaceWorktrees deregisters every git worktree materialized for the
// given workspace sources, deleting each worktree's working tree. It targets
// only those destination paths, so sibling worktrees sharing the same source
// caches are left intact. It is best-effort cleanup for failed or rolled-back
// creates, so per-source errors are ignored.
func (p *Platform) removeWorkspaceWorktrees(ctx context.Context, stack *manifest.Stack, workspacePath string, sources map[string]manifest.WorkspaceSource) {
	type worktree struct{ cachePath, dest string }
	var worktrees []worktree
	for _, ws := range sources {
		if ws.Mode != "worktree" {
			continue
		}
		source, ok := stack.Sources[ws.Source]
		if !ok || source.Kind != "git" {
			continue
		}
		worktrees = append(worktrees, worktree{
			cachePath: p.sourcePath(ws.Source, source),
			dest:      filepath.Join(workspacePath, filepath.FromSlash(ws.Subpath)),
		})
	}
	// Remove deepest path first so a parent worktree's forced delete cannot wipe
	// a nested worktree's directory before that nested one is deregistered.
	sort.Slice(worktrees, func(i, j int) bool { return len(worktrees[i].dest) > len(worktrees[j].dest) })
	client := git.New()
	for _, w := range worktrees {
		_ = client.WorktreeRemove(ctx, w.cachePath, w.dest)
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
	if kind == "" {
		switch {
		case repo != "":
			kind = "git"
		case path != "":
			kind = "local"
		}
	}
	return manifest.Source{Kind: kind, Repo: repo, URL: url, Path: path, DefaultRef: defaultRef, CachePath: cachePath}, nil
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
	return manifest.WorkspaceSource{Source: sourceName, Mode: spec.Mode, Branch: branch, Ref: ref, Subpath: subpath}, nil
}

func (p *Platform) materializeWorkspaceSource(ctx context.Context, sourceName string, source manifest.Source, ws manifest.WorkspaceSource, dest string, sync bool) error {
	destExists, destEmpty := false, false
	if info, err := os.Stat(dest); err == nil {
		destExists = true
		if source.Kind != "git" || !info.IsDir() {
			return fmt.Errorf("workspace source destination %s already exists", dest)
		}
		empty, err := isEmptyDir(dest)
		if err != nil {
			return err
		}
		destEmpty = empty
		// A non-empty leftover only blocks the create when we are not asked to
		// reconcile it; with sync, a worktree source reclaims the path below.
		if !empty && (!sync || ws.Mode != "worktree") {
			return fmt.Errorf("workspace source destination %s already exists and is not empty", dest)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	switch source.Kind {
	case "git":
		if ws.Mode == "worktree" {
			cachePath := p.sourcePath(sourceName, source)
			insideDest, err := sameOrNestedPath(dest, cachePath)
			if err != nil {
				return err
			}
			if insideDest {
				return fmt.Errorf("workspace source cache path %s cannot be inside destination %s", cachePath, dest)
			}
			if err := p.materializeSource(ctx, sourceName, source); err != nil {
				return err
			}
			client := git.New()
			ref := ws.Ref
			if ref == "" {
				ref = source.DefaultRef
			}
			useExisting := ws.Branch != "" && client.RefExists(ctx, cachePath, "refs/heads/"+ws.Branch)
			if useExisting {
				fmt.Fprintf(os.Stderr, "warning: branch %q already exists in %s; checking it out into worktree without creating a new branch\n", ws.Branch, cachePath)
			}
			addWorktree := func() error {
				if useExisting {
					return client.WorktreeAdd(ctx, cachePath, dest, ws.Branch)
				}
				return client.WorktreeAddBranch(ctx, cachePath, dest, ws.Branch, ref)
			}
			if destExists && !destEmpty {
				// A populated worktree left by a create that failed after
				// materializing it. Reaching here implies sync (the guard above
				// rejects a non-empty destination otherwise). Hand it back to
				// git before re-adding; remove targets only dest, so sibling
				// worktrees sharing this cache are untouched. If dest is not a
				// reclaimable worktree (e.g. unrelated files), keep the clear
				// "already exists" refusal instead of leaking a raw git error,
				// and leave its contents in place.
				if err := client.WorktreeRemove(ctx, cachePath, dest); err != nil {
					return fmt.Errorf("workspace source destination %s already exists and is not empty", dest)
				}
				return addWorktree()
			}
			// The destination is absent or an empty directory, so the add
			// normally succeeds. If it does not, an earlier failed create may
			// have left a stale "missing but already registered" worktree for
			// this path. That registration has no working tree to lose, so it is
			// always safe to clear (no sync required); prune is git's recovery
			// for it. Prune only here — when an add actually conflicts — so the
			// common create never risks a sibling whose dir is briefly absent.
			if err := addWorktree(); err != nil {
				if pruneErr := client.WorktreePrune(ctx, cachePath); pruneErr != nil {
					return err
				}
				return addWorktree()
			}
			return nil
		}
		ref := ws.Ref
		if ref == "" {
			ref = source.DefaultRef
		}
		return git.New().CloneRef(ctx, source.Repo, dest, ref)
	case "local":
		target, err := workspaceLocalSymlinkTarget(p.sourcePath(sourceName, source), dest)
		if err != nil {
			return err
		}
		return os.Symlink(target, dest)
	default:
		return fmt.Errorf("workspace source kind %q is not implemented", source.Kind)
	}
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

func isEmptyDir(path string) (bool, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return false, err
	}
	return len(entries) == 0, nil
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

func (p *Platform) resolveWorkspaceChainTemplate(ctx context.Context, workspacePath, ref string) (string, string, error) {
	if ref != "" && !filepath.IsAbs(ref) && !isRemoteTemplateRef(ref) {
		candidate := filepath.Join(workspacePath, filepath.FromSlash(ref))
		if _, err := os.Stat(filepath.Join(candidate, "copier.yml")); err == nil {
			return candidate, ref, nil
		}
	}
	return p.resolveTemplate(ctx, ref, "stack")
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

func (p *Platform) resolveTemplate(ctx context.Context, ref, kind string) (string, string, error) {
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
	family := kind + "s"
	kindRef := ref
	if !strings.Contains(ref, "/") {
		kindRef = family + "/" + ref
	}
	if !strings.HasPrefix(kindRef, family+"/") {
		return "", "", fmt.Errorf("template %q does not match kind %q", ref, kind)
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
	return "", "", fmt.Errorf("template %q was not found", ref)
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
