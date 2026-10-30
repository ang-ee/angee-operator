package service

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/fslock"
	gitx "github.com/ang-ee/angee-operator/internal/git"
	"github.com/ang-ee/angee-operator/internal/logctx"
	"github.com/ang-ee/angee-operator/internal/manifest"
	mountx "github.com/ang-ee/angee-operator/internal/mount"
	"github.com/ang-ee/angee-operator/internal/runtime"
	"github.com/ang-ee/angee-operator/internal/secrets"
	"github.com/ang-ee/angee-operator/internal/substitute"
	"github.com/ang-ee/angee-operator/internal/vcs"
)

const (
	backupArchiveDirectory = "backups/archives"
	backupStagingDirectory = "state/backups"
)

// backupTree is one directory a resource captures. A repository tree holds
// version-control metadata, which may hold no symlink; its prefix is no guide,
// since a source, workspace or slot may be named repository.
type backupTree struct {
	scope      string
	root       string
	prefix     string
	exclude    []string
	repository bool
}
type backupResource struct {
	api.BackupResource
	trees       []backupTree
	job         string
	capture     *manifest.BackupCapture
	sourceStore string
	revision    api.BackupSourceEvidence
}

func backupGroups(s api.BackupSelection) map[string]api.BackupGroupSelection {
	return map[string]api.BackupGroupSelection{"databases": s.Databases, "service_state": s.ServiceState, "workspaces": s.Workspaces, "sources": s.Sources.BackupGroupSelection, "storage": s.Storage}
}
func validateBackup(req api.BackupRequest) error {
	if req.Selection.Version != 0 && req.Selection.Version != 1 {
		return &InvalidInputError{Field: "selection.version", Reason: "only version 1 is supported"}
	}
	if req.Version != 1 {
		return &InvalidInputError{Field: "version", Reason: "only backup format version 1 is supported"}
	}
	for name, group := range backupGroups(req.Selection) {
		if group.Mode != "" && group.Mode != "all" && group.Mode != "selected" && group.Mode != "none" {
			return &InvalidInputError{Field: name + ".mode", Reason: "must be all, selected or none"}
		}
		if group.Mode != "selected" && len(group.Names) > 0 {
			return &InvalidInputError{Field: name + ".names", Reason: "requires selected mode"}
		}
	}
	if req.Selection.Sources.Policy != "" && req.Selection.Sources.Policy != "bundle" && req.Selection.Sources.Policy != "fetch_published" {
		return &InvalidInputError{Field: "sources.policy", Reason: "must be bundle or fetch_published"}
	}
	return nil
}
func backupChosen(group api.BackupGroupSelection, name string) bool {
	return !slices.Contains(group.Exclude, name) && (group.Mode == "all" || group.Mode == "selected" && slices.Contains(group.Names, name))
}
func backupRelative(root, candidate string) (string, error) {
	rel, err := filepath.Rel(root, candidate)
	if err != nil || !fs.ValidPath(filepath.ToSlash(rel)) || rel == "." {
		return "", fmt.Errorf("backup path %q must be a contained child of %q", candidate, root)
	}
	return filepath.ToSlash(rel), nil
}
func backupWithin(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
func backupExcluded(candidate string, excludes []string) bool {
	for _, exclude := range excludes {
		if backupWithin(exclude, candidate) {
			return true
		}
	}
	return false
}

// Preview estimates are advisory. Bound filesystem work across the entire
// request; native capability discovery and every selected resource remain visible.
const (
	backupPreviewEstimateEntries         = 100000
	backupPreviewResourceEstimateEntries = 10000
)

var errBackupEstimateIncomplete = errors.New("backup preview estimate scan limit reached")

type backupEstimateBudget struct{ remaining, perResource int }

// backupScanBudget charges enumeration before sorting or applying exclusions.
// One lookahead entry distinguishes an exact fit from an incomplete estimate.
type backupScanBudget struct {
	request   *backupEstimateBudget
	remaining int
}

func (budget *backupScanBudget) available() int {
	return max(0, min(budget.request.remaining, budget.remaining))
}
func (budget *backupScanBudget) consume(count int) error {
	available := budget.available()
	charged := min(count, available)
	budget.remaining -= charged
	budget.request.remaining -= charged
	if count > available {
		return errBackupEstimateIncomplete
	}
	return nil
}

func inspectBackupTrees(ctx context.Context, trees []backupTree, budget *backupEstimateBudget) (*int64, error) {
	// Capability and containment checks are independent of optional byte estimates.
	// Validate every root, even when an earlier component exhausts the scan budget.
	for _, tree := range trees {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		root, err := openBackupTree(tree)
		if err != nil {
			return nil, err
		}
		_ = root.Close()
	}
	if budget == nil {
		return nil, nil
	}
	remaining := budget.remaining
	if budget.perResource > 0 {
		remaining = min(remaining, budget.perResource)
	}
	scan := &backupScanBudget{request: budget, remaining: remaining}
	var total int64
	for _, tree := range trees {
		err := walkBackupTree(ctx, tree, func(_ *os.Root, _ string, info fs.FileInfo, _ string) error {
			if info.Mode().IsRegular() {
				total += info.Size()
			}
			return nil
		}, scan)
		if err != nil {
			return nil, err
		}
	}
	return &total, nil
}

// inspect owns the public interpretation of size estimates and traversal errors.
// A partial estimate is never published as a complete byte count or exclusion.
func (resource *backupResource) inspect(ctx context.Context, budget *backupEstimateBudget) error {
	if resource.Status != "included" || resource.job != "" {
		return nil
	}
	estimate, err := inspectBackupTrees(ctx, resource.trees, budget)
	if canceled := ctx.Err(); canceled != nil {
		return canceled
	}
	if errors.Is(err, errBackupEstimateIncomplete) {
		resource.EstimatedBytes = nil
		resource.Reason = "Size not available from quick discovery. Full contents are checked during backup."
	} else if err != nil {
		resource.Status = "unsupported"
		resource.Reason = backupPublicReason(ctx, err)
	} else {
		resource.EstimatedBytes = estimate
	}
	return nil
}

// BackupPreview discovers stack-owned resources without executing capture jobs.
func (p *Platform) BackupPreview(ctx context.Context, req api.BackupRequest) (api.BackupPreview, error) {
	resources, _, err := p.backupDiscover(ctx, req, &backupEstimateBudget{remaining: backupPreviewEstimateEntries, perResource: backupPreviewResourceEstimateEntries})
	if err != nil {
		return api.BackupPreview{}, err
	}
	preview := api.BackupPreview{Version: 1, Resources: []api.BackupResource{}}
	for _, resource := range resources {
		preview.Resources = append(preview.Resources, resource.BackupResource)
		if resource.Status == "included" {
			if resource.EstimatedBytes == nil {
				preview.HasUnknownSizes = true
			} else {
				preview.EstimatedBytes += *resource.EstimatedBytes
			}
		}
	}
	return preview, nil
}

func (p *Platform) backupDiscover(ctx context.Context, req api.BackupRequest, budget *backupEstimateBudget) ([]backupResource, *manifest.Stack, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if err := validateBackup(req); err != nil {
		return nil, nil, err
	}
	stack, err := p.LoadStack()
	if err != nil {
		return nil, nil, err
	}
	groups := backupGroups(req.Selection)
	// Safety exclusions are global, even when a capture or database is deselected.
	excluded := []string{filepath.Join(p.root, "backups"), filepath.Join(p.root, "state"), filepath.Join(p.root, "run")}
	for name, persist := range stack.Persist {
		// Old manifests called physical PostgreSQL state pgdata. Keep it fail-closed
		// until templates add backup: exclude; never walk through a containing persist.
		if persist.Backup == "exclude" || name == "pgdata" {
			excluded = append(excluded, manifest.ResolvePath(p.root, persist.Subpath))
		}
	}
	capturedPersist := map[string]bool{}
	for _, job := range stack.Jobs {
		if job.Capture != nil {
			for _, name := range job.Capture.ExcludePersist {
				capturedPersist[name] = true
				persist, ok := stack.Persist[name]
				if !ok {
					return nil, nil, fmt.Errorf("capture refers to undeclared persist %q", name)
				}
				excluded = append(excluded, manifest.ResolvePath(p.root, persist.Subpath))
			}
		}
	}
	var result []backupResource
	add := func(group, name, resourcePath string, trees []backupTree, job string, capture *manifest.BackupCapture) error {
		resource := backupResource{BackupResource: api.BackupResource{Group: group, Name: name, Status: "excluded", Path: resourcePath, CaptureJob: job}, trees: trees, job: job, capture: capture}
		if backupChosen(groups[group], name) {
			resource.Status = "included"
			for _, tree := range trees {
				if backupExcluded(tree.root, tree.exclude) {
					resource.Status = "excluded"
					resource.Reason = "physical database state or operator artifact directory is never copied"
					break
				}
			}
			if err := resource.inspect(ctx, budget); err != nil {
				return err
			}
		}
		result = append(result, resource)
		return nil
	}
	for _, name := range sortedKeys(stack.Jobs) {
		capture := stack.Jobs[name].Capture
		if capture == nil {
			continue
		}
		if err := add(capture.Group, capture.Name, "", nil, name, capture); err != nil {
			return nil, nil, err
		}
		backend := p.composeBackend
		if stack.Jobs[name].Runtime == manifest.RuntimeLocal {
			backend = p.procBackend
		}
		if validator, ok := backend.(runtime.JobValidator); ok && backupChosen(groups[capture.Group], capture.Name) {
			if err := validator.ValidateJob(runtime.JobSpec{Name: name, Transient: true}); err != nil {
				resource := &result[len(result)-1]
				resource.Status = "unsupported"
				resource.Reason = backupPublicReason(ctx, err)
			}
		}
	}
	unsupported := func(group, name, reason string) {
		status := "excluded"
		if backupChosen(groups[group], name) {
			status = "unsupported"
		}
		result = append(result, backupResource{BackupResource: api.BackupResource{Group: group, Name: name, Status: status, Reason: reason}})
	}
	for _, name := range sortedKeys(stack.Volumes) {
		unsupported("storage", "volume:"+name, "named volume capture is not declared; configure a trusted logical capture job")
	}
	for _, name := range sortedKeys(stack.Services) {
		if slices.ContainsFunc(result, func(resource backupResource) bool { return resource.Group == "service_state" && resource.Name == name }) {
			continue
		}
		persistent := false
		for _, raw := range stack.Services[name].Mounts {
			mount, err := mountx.Parse(raw)
			if err != nil {
				return nil, nil, err
			}
			if mount.ReadOnly {
				continue
			}
			if mount.Scheme == "volume" {
				persistent = true
				break
			}
			if mount.Scheme == "bind" {
				host := manifest.ResolvePath(p.root, mount.HostPath)
				for persistName, persist := range stack.Persist {
					// Ordinary persisted bind files already belong to storage. A logical
					// capture's exclude_persist owns the state it deliberately supersedes.
					unsafe := persist.Backup == "exclude" || persistName == "pgdata"
					if !unsafe || capturedPersist[persistName] {
						continue
					}
					declared := manifest.ResolvePath(p.root, persist.Subpath)
					if backupWithin(host, declared) || backupWithin(declared, host) {
						persistent = true
						break
					}
				}
			}
			if persistent {
				break
			}
		}
		if persistent {
			unsupported("service_state", name, "service declares persistence but no logical service capture; select its storage paths or declare a trusted capture job")
		}
	}
	for _, name := range sortedKeys(stack.Persist) {
		persist := stack.Persist[name]
		resourcePath := manifest.ResolvePath(p.root, persist.Subpath)
		if _, err := backupRelative(p.root, resourcePath); err != nil {
			return nil, nil, err
		}
		storageExcludes := slices.Clone(excluded)
		for other, spec := range stack.Persist {
			child := manifest.ResolvePath(p.root, spec.Subpath)
			if other != name && child != resourcePath && backupWithin(resourcePath, child) {
				storageExcludes = append(storageExcludes, child)
			}
		}
		if err := add("storage", name, resourcePath, []backupTree{{scope: p.root, root: resourcePath, prefix: "storage/" + name, exclude: storageExcludes}}, "", nil); err != nil {
			return nil, nil, err
		}
	}
	for _, name := range sortedKeys(stack.Workspaces) {
		workspace := stack.Workspaces[name]
		root := filepath.Join(p.root, "workspaces", name)
		exclusions := slices.Clone(excluded)
		for slot, source := range workspace.Sources {
			subpath := source.Subpath
			if subpath == "" {
				subpath = slot
			}
			slotPath := filepath.Join(root, subpath)
			if !backupWithin(root, slotPath) {
				return nil, nil, fmt.Errorf("workspace source %q escapes workspace", slot)
			}
			exclusions = append(exclusions, slotPath)
		}
		if err := add("workspaces", name, root, []backupTree{{scope: p.root, root: root, prefix: "workspaces/" + name, exclude: exclusions}}, "", nil); err != nil {
			return nil, nil, err
		}
	}
	source := func(name, root, declaredStore string) error {
		sourceExcluded := slices.Clone(excluded)
		for _, persist := range stack.Persist {
			candidate := manifest.ResolvePath(p.root, persist.Subpath)
			if candidate != root && !backupWithin(candidate, root) {
				sourceExcluded = append(sourceExcluded, candidate)
			}
		}
		for workspace := range stack.Workspaces {
			candidate := filepath.Join(p.root, "workspaces", workspace)
			if candidate != root && candidate != declaredStore && !backupWithin(candidate, root) {
				sourceExcluded = append(sourceExcluded, candidate)
			}
		}
		for owner, spec := range stack.Sources {
			candidate := p.sourcePath(owner, spec)
			if candidate != root && candidate != declaredStore && !backupWithin(candidate, root) {
				sourceExcluded = append(sourceExcluded, candidate)
			}
		}
		resource := backupResource{BackupResource: api.BackupResource{Group: "sources", Name: name, Path: root, Status: "excluded"}, sourceStore: declaredStore}
		if backupChosen(groups["sources"], name) {
			resource.Status = "included"
			resource.trees, resource.revision, err = p.backupSourceTrees(ctx, name, root, declaredStore, sourceExcluded, req.Selection.Sources.Policy)
			if canceled := ctx.Err(); canceled != nil {
				return canceled
			}
			if err != nil {
				resource.Status = "unsupported"
				resource.Reason = backupPublicReason(ctx, err)
			} else if err := resource.inspect(ctx, budget); err != nil {
				return err
			}
		}
		result = append(result, resource)
		return nil
	}
	for _, name := range sortedKeys(stack.Sources) {
		root := p.sourcePath(name, stack.Sources[name])
		if err := source("stack:"+name, root, root); err != nil {
			return nil, nil, err
		}
	}
	for _, workspace := range sortedKeys(stack.Workspaces) {
		for _, slot := range sortedKeys(stack.Workspaces[workspace].Sources) {
			spec := stack.Workspaces[workspace].Sources[slot]
			owner, ok := stack.Sources[spec.Source]
			if !ok {
				return nil, nil, fmt.Errorf("workspace source %q is undeclared", spec.Source)
			}
			subpath := spec.Subpath
			if subpath == "" {
				subpath = slot
			}
			root := filepath.Join(p.root, "workspaces", workspace, subpath)
			if err := source("workspace:"+workspace+"/"+slot, root, p.sourcePath(spec.Source, owner)); err != nil {
				return nil, nil, err
			}
		}
	}
	for group, selection := range groups {
		for _, name := range append(slices.Clone(selection.Names), selection.Exclude...) {
			if !slices.ContainsFunc(result, func(r backupResource) bool { return r.Group == group && r.Name == name }) {
				return nil, nil, &InvalidInputError{Field: group, Reason: fmt.Sprintf("unknown resource %q", name)}
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	return result, stack, nil
}

// openBackupDirectory pins one real directory below an already confined handle.
// Comparing the opened identity closes the Lstat/OpenRoot substitution window;
// callers retain the handle instead of re-opening its path during recursion.
func openBackupDirectory(parent *os.Root, name string) (*os.Root, fs.FileInfo, error) {
	before, err := parent.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if !before.IsDir() {
		return nil, nil, errors.New("backup directory is not a real directory")
	}
	child, err := parent.OpenRoot(name)
	if err != nil {
		return nil, nil, err
	}
	opened, err := child.Stat(".")
	if err == nil && !os.SameFile(before, opened) {
		err = errors.New("backup directory changed while opening")
	}
	current, currentErr := parent.Lstat(name)
	if err == nil {
		err = currentErr
	}
	if err == nil && (!current.IsDir() || !os.SameFile(before, current)) {
		err = errors.New("backup directory changed while opening")
	}
	if err != nil {
		_ = child.Close()
		return nil, nil, err
	}
	return child, before, nil
}

// openBackupPath checks every directory component from a declared boundary.
// It refuses contained symlinks as well as links escaping the os.Root boundary.
func openBackupPath(scope, target string) (*os.Root, error) {
	root, err := os.OpenRoot(scope)
	if err != nil {
		return nil, err
	}
	relative, err := filepath.Rel(scope, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		_ = root.Close()
		return nil, errors.New("backup tree escapes its owning resource")
	}
	if relative != "." {
		for _, component := range strings.Split(relative, string(filepath.Separator)) {
			child, _, err := openBackupDirectory(root, component)
			_ = root.Close()
			if err != nil {
				return nil, err
			}
			root = child
		}
	}
	return root, nil
}

func openBackupTree(tree backupTree) (*os.Root, error) {
	if backupExcluded(tree.root, tree.exclude) {
		return nil, errors.New("resource belongs to an excluded physical-state, operator-artifact or separately selected source owner")
	}
	return openBackupPath(tree.scope, tree.root)
}

// walkBackupTree never follows directory links, including links within the
// stack. Every ancestor and recursive child is opened through its pinned parent;
// lexical exclusions therefore cannot be bypassed through a contained alias.
//
// A file deleted between its directory's listing and its stat, or its copy
// (visit's errBackupEntryGone), is not part of a live capture and is skipped.
// In a repository tree it is a change, such as a repack deleting the pack it
// replaced after the listing, and fails the walk: skipping it could leave the
// captured store without objects it needs.
func walkBackupTree(ctx context.Context, tree backupTree, visit func(*os.Root, string, fs.FileInfo, string) error, scan *backupScanBudget) error {
	root, err := openBackupTree(tree)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	rootInfo, err := root.Stat(".")
	if err != nil {
		return err
	}
	if scan != nil {
		if err := scan.consume(1); err != nil {
			return err
		}
	}
	if err := visit(root, ".", rootInfo, tree.prefix); err != nil {
		return err
	}
	var walk func(*os.Root, string) error
	walk = func(directory *os.Root, relative string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		file, err := directory.Open(".")
		if err != nil {
			return err
		}
		var entries []fs.DirEntry
		if scan == nil {
			entries, err = file.ReadDir(-1)
		} else {
			// Bound the directory read itself, including excluded entries. A positive
			// ReadDir may return a short batch, so continue until EOF or lookahead fills.
			limit := scan.available() + 1
			for len(entries) < limit {
				if err = ctx.Err(); err != nil {
					break
				}
				var batch []fs.DirEntry
				batch, err = file.ReadDir(limit - len(entries))
				entries = append(entries, batch...)
				if errors.Is(err, io.EOF) {
					err = nil
					break
				}
				if err != nil {
					break
				}
				if len(batch) == 0 {
					err = io.ErrNoProgress
					break
				}
			}
			budgetErr := scan.consume(len(entries))
			if err == nil {
				err = budgetErr
			}
		}
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			name := entry.Name()
			logical := filepath.Join(relative, name)
			if backupExcluded(filepath.Join(tree.root, logical), tree.exclude) {
				continue
			}
			info, err := directory.Lstat(name)
			if errors.Is(err, fs.ErrNotExist) && !tree.repository {
				continue
			}
			if err != nil {
				return err
			}
			archiveName := path.Join(tree.prefix, filepath.ToSlash(logical))
			if info.IsDir() {
				child, identity, err := openBackupDirectory(directory, name)
				if err != nil {
					return err
				}
				if !os.SameFile(info, identity) {
					_ = child.Close()
					return errors.New("backup directory changed before opening")
				}
				err = visit(directory, name, identity, archiveName)
				if err == nil {
					current, statErr := directory.Lstat(name)
					err = statErr
					if err == nil && (!current.IsDir() || !os.SameFile(identity, current)) {
						err = errors.New("backup directory changed before traversal")
					}
				}
				if err == nil {
					err = walk(child, logical)
				}
				_ = child.Close()
				if err != nil {
					return err
				}
				continue
			}
			if tree.repository && info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("repository metadata symlink %q is not portable", logical)
			}
			if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
				return fmt.Errorf("special file %q cannot be backed up", logical)
			}
			if err := visit(directory, name, info, archiveName); err != nil {
				if errors.Is(err, errBackupEntryGone) && !tree.repository {
					continue
				}
				return err
			}
		}
		return nil
	}
	return walk(root, "")
}

func (p *Platform) backupSourceTrees(ctx context.Context, name, root, store string, excluded []string, policy string) ([]backupTree, api.BackupSourceEvidence, error) {
	if backupWithin(p.root, root) {
		relative, err := filepath.Rel(p.root, root)
		if err != nil {
			return nil, api.BackupSourceEvidence{}, err
		}
		guard, err := openBackupPath(p.root, filepath.Join(p.root, filepath.Dir(relative)))
		if err != nil {
			return nil, api.BackupSourceEvidence{}, err
		}
		_ = guard.Close()
	}
	// A declared local source may be a symlink slot to its explicitly declared
	// store. No other link is followed while walking any resource.
	if info, err := os.Lstat(root); err != nil {
		return nil, api.BackupSourceEvidence{}, err
	} else if info.Mode()&os.ModeSymlink != 0 {
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			return nil, api.BackupSourceEvidence{}, err
		}
		declared, err := filepath.EvalSymlinks(store)
		if err != nil || resolved != declared {
			return nil, api.BackupSourceEvidence{}, vcs.BackupRefusal("source symlink does not resolve to its declared store")
		}
		root = resolved
		store = declared
	}
	// Native VCS paths are canonical. Preserve stack-owned exclusions across
	// a canonical stack-root alias (for example macOS /var -> /private/var).
	canonicalStack, err := filepath.EvalSymlinks(p.root)
	if err != nil {
		return nil, api.BackupSourceEvidence{}, err
	}
	excluded = slices.Clone(excluded)
	for _, candidate := range slices.Clone(excluded) {
		if backupWithin(p.root, candidate) {
			relative, err := filepath.Rel(p.root, candidate)
			if err != nil {
				return nil, api.BackupSourceEvidence{}, err
			}
			excluded = append(excluded, filepath.Join(canonicalStack, relative))
		}
	}
	prefix := "sources/" + strings.ReplaceAll(name, ":", "/")
	slot := vcs.Slot{Path: root, Primary: store}
	planner, ok := p.slotDriver(slot).(vcs.BackupPlanner)
	if !ok {
		return nil, api.BackupSourceEvidence{}, vcs.BackupRefusal("source version control cannot plan a capture")
	}
	plan, err := planner.BackupPlan(ctx, slot, policy == "fetch_published", p.backupLayer(slot))
	if err != nil {
		return nil, api.BackupSourceEvidence{}, err
	}
	if policy == "fetch_published" {
		return nil, plan.Evidence, nil
	}
	exclusions := append(slices.Clone(excluded), plan.WorktreeExclude...)
	scope := root
	if backupWithin(p.root, root) {
		scope = p.root
	}
	trees := []backupTree{{scope: scope, root: root, prefix: prefix + "/worktree", exclude: exclusions}}
	resolvedStore, err := filepath.EvalSymlinks(store)
	if err != nil {
		return nil, api.BackupSourceEvidence{}, err
	}
	for _, component := range plan.Components {
		scope := resolvedStore
		if backupWithin(root, component.Root) {
			scope = root
		}
		if backupWithin(p.root, component.Root) {
			scope = p.root
		}
		trees = append(trees, backupTree{scope: scope, root: component.Root, prefix: prefix + "/repository/" + component.Name, exclude: append(slices.Clone(excluded), component.Exclude...), repository: true})
	}
	return trees, plan.Evidence, nil
}

// BackupStart creates an idempotent asynchronous operation using the existing
// JobRun receipt and mutation lease. Included unsupported resources fail closed.
func (p *Platform) BackupStart(ctx context.Context, req api.BackupRequest) (api.JobRunOperation, error) {
	if err := validateBackup(req); err != nil {
		return api.JobRunOperation{}, err
	}
	if !operationIDPattern.MatchString(req.RequestID) {
		return api.JobRunOperation{}, &InvalidInputError{Field: "request_id", Reason: "must be 1..128 letters, digits, underscores or hyphens"}
	}
	data, _ := json.Marshal(req.Selection)
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	p.backupMu.Lock()
	defer p.backupMu.Unlock()
	existing, err := p.JobRunGet(ctx, req.RequestID)
	if err == nil {
		return backupReceiptFor(existing, digest)
	}
	var missing *NotFoundError
	if !errors.As(err, &missing) {
		return api.JobRunOperation{}, err
	}
	ctx, release, err := p.beginMutation(ctx, "backup")
	if err != nil {
		return api.JobRunOperation{}, err
	}
	lockCtx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	rootLock := fslock.RootLock(p.root)
	lockErr := rootLock.Lock(lockCtx)
	cancel()
	if lockErr != nil {
		release()
		if errors.Is(lockErr, context.DeadlineExceeded) {
			return api.JobRunOperation{}, mutationBusyError("another stack mutation is active")
		}
		return api.JobRunOperation{}, lockErr
	}
	handedOff := false
	defer func() {
		if !handedOff {
			_ = rootLock.Unlock()
		}
	}()
	if existing, err := p.readJobReceipt(ctx, req.RequestID, true); err == nil {
		release()
		return backupReceiptFor(existing, digest)
	} else {
		var missing *NotFoundError
		if !errors.As(err, &missing) {
			release()
			return api.JobRunOperation{}, err
		}
	}
	resources, stack, err := p.backupDiscover(ctx, req, nil)
	if err != nil {
		release()
		return api.JobRunOperation{}, err
	}
	for _, resource := range resources {
		if resource.Status == "unsupported" {
			release()
			return api.JobRunOperation{}, &InvalidInputError{Field: resource.Group + ":" + resource.Name, Reason: resource.Reason}
		}
	}

	op := api.JobRunOperation{ID: req.RequestID, RootJob: "backup", RequestDigest: digest, Status: api.JobRunPending, StartedAt: time.Now().UTC(), Nodes: []api.JobRunNode{{Name: "capture", Kind: "backup", Status: api.JobRunPending}, {Name: "verify", Kind: "backup", Status: api.JobRunPending}}}
	if err = p.persistJobReceipt(op); err != nil {
		release()
		return op, err
	}
	p.jobRunsMu.Lock()
	p.rememberBackupRun(cloneJobRun(op))
	p.jobRunsMu.Unlock()
	operationCtx := context.WithoutCancel(ctx)
	if p.detachedContext != nil {
		operationCtx = p.detachedContext
	}
	handedOff = true
	go func() {
		defer func() { _ = rootLock.Unlock() }()
		defer release()
		operationCtx, cancel, _ := jobRunContext(operationCtx)
		defer cancel()
		err := p.executeBackup(operationCtx, req, resources, stack)
		if err != nil {
			failed, _ := p.JobRunGet(operationCtx, req.RequestID)
			func(o *api.JobRunOperation) {
				now := time.Now().UTC()
				o.Status = api.JobRunFailed
				o.EndedAt = &now
				o.Error = boundedJobError(err.Error())
				o.ErrorCode = CodeJobFailed
				o.Artifact = nil
				for i := range o.Nodes {
					if o.Nodes[i].Status == api.JobRunPending || o.Nodes[i].Status == api.JobRunRunning {
						o.Nodes[i].Status = api.JobRunFailed
					}
				}
			}(&failed)
			if persistErr := p.persistJobReceipt(failed); persistErr != nil {
				failed.Error = "failed to retain backup failure receipt: " + persistErr.Error()
			}
			p.updateJobRun(req.RequestID, func(o *api.JobRunOperation) { *o = failed })
		}
	}()
	return cloneJobRun(op), nil
}

func (p *Platform) executeBackup(ctx context.Context, req api.BackupRequest, resources []backupResource, stack *manifest.Stack) error {
	id := req.RequestID
	backend, err := secrets.FromManifest(p.root, stack.SecretsBackend, substitute.SecretEnvName)
	if err != nil {
		return err
	}
	originalSecrets, err := secrets.ReadDeclarations(ctx, backend, stack.Secrets)
	if err != nil {
		return err
	}
	p.updateJobRun(id, func(o *api.JobRunOperation) {
		o.Status = api.JobRunRunning
		o.CurrentStep = "capture"
		o.Nodes[0].Status = api.JobRunRunning
	})
	op, _ := p.JobRunGet(ctx, id)
	if err := p.persistJobReceipt(op); err != nil {
		return err
	}
	root, err := os.OpenRoot(p.root)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	staging := backupStagingDirectory + "/" + id
	if err = root.MkdirAll(staging, 0700); err != nil {
		return err
	}
	defer func() { _ = root.RemoveAll(staging) }()
	if err = root.MkdirAll(backupArchiveDirectory, 0700); err != nil {
		return err
	}
	temporary := staging + "/archive.zip"
	output, err := root.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer output.Close()
	writer := zip.NewWriter(output)
	manifestData := api.BackupManifest{Version: 1, Stack: stack.Name, Selection: req.Selection, Entries: []api.BackupEntry{}, Resources: []api.BackupResource{}, SourceRefs: map[string]api.BackupSourceEvidence{}, Recovery: []string{"recovery/angee.yaml retains the stack declarations and source references", "recovery/secrets.env retains original application encryption keys; never regenerate SECRET_KEY for captured encrypted fields", "source worktree payloads retain configuration, lockfiles and migration files", "repository/git is a self-contained Git common directory; restore worktree with captured source_refs revision", "repository/jj-repo plus jj-workspace retain shared JJ history; remap repo and store/git_target to captured sibling components before use", "no archive command is executable restore input; cold restore requires explicit trusted CLI validation", "files are captured live; independent database dumps are per-database consistent, not a cross-service transaction"}}
	archive := &backupArchive{ctx: ctx, writer: writer, manifest: &manifestData, names: map[string]bool{}}
	for _, resource := range resources {
		manifestData.Resources = append(manifestData.Resources, resource.BackupResource)
		if resource.Status != "included" {
			continue
		}
		trees := resource.trees
		if resource.job != "" {
			dir := staging + "/captures/" + resource.job
			if err = root.MkdirAll(dir, 0700); err != nil {
				return err
			}
			inputs := map[string]string{"backup_output": dir, "backup_output_absolute": filepath.Join(p.root, dir)}
			projected, err := jobOperationStack(stack, resource.job, false)
			if err != nil {
				return err
			}
			compiled, err := compileCapture(projected, p.root, originalSecrets, resource.job, inputs)
			if err != nil {
				return err
			}
			for _, dependency := range stackNodeDependencies(stack, resource.job) {
				ready, err := p.dependencySatisfied(ctx, stack, compiled, dependency)
				if err != nil {
					return err
				}
				if !ready {
					return fmt.Errorf("capture prerequisite %q is not ready", dependency)
				}
			}
			out, runErr := p.runJob(ctx, stack, compiled, resource.job, false)
			p.updateJobRun(id, func(o *api.JobRunOperation) {
				o.Output = appendCaptureOutput(o.Output, resource.Group+"/"+resource.Name, out)
			})
			if runErr != nil {
				p.updateJobRun(id, func(o *api.JobRunOperation) { o.Nodes[0].Message = boundedJobError(runErr.Error()) })
				return fmt.Errorf("capture %q: %w", resource.Name, runErr)
			}
			file := dir + "/" + resource.capture.Output
			info, err := root.Lstat(file)
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() || info.Size() == 0 {
				return fmt.Errorf("capture %q must emit a non-empty regular file", resource.Name)
			}
			if err = archive.add(root, file, info, "captures/"+resource.Group+"/"+resource.Name+"/"+resource.capture.Output); err != nil {
				return err
			}
		}
		for _, tree := range trees {
			if err = walkBackupTree(ctx, tree, archive.add, nil); err != nil {
				return err
			}
		}
		if resource.Group == "sources" {

			manifestData.SourceRefs[resource.Name] = resource.revision
		}
	}
	// Preserve the actual recovery key material without putting any value into
	// receipts or manifest JSON. Env-file and OpenBao already own these paths.
	for _, item := range []struct{ source, destination string }{{"angee.yaml", "recovery/angee.yaml"}, {p.runtimeEnvFile(stack), "recovery/secrets.env"}} {
		name := item.source
		if filepath.IsAbs(name) {
			name, err = backupRelative(p.root, name)
			if err != nil {
				return err
			}
		}
		info, err := root.Lstat(name)
		if errors.Is(err, os.ErrNotExist) && item.destination == "recovery/secrets.env" {
			return errors.New("original secret material is unavailable; exact encrypted-data recovery cannot be guaranteed")
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("recovery file %q must be regular", name)
		}
		if err = archive.add(root, name, info, item.destination); err != nil {
			return err
		}
	}
	payload, err := json.Marshal(manifestData)
	if err != nil {
		return err
	}
	entry, err := writer.Create("manifest.json")
	if err != nil {
		return err
	}
	if _, err = entry.Write(payload); err != nil {
		return err
	}

	if err = writer.Close(); err != nil {
		return err
	}
	if err = output.Sync(); err != nil {
		return err
	}
	p.updateJobRun(id, func(o *api.JobRunOperation) {
		o.CurrentStep = "verify"
		o.Nodes[0].Status = api.JobRunSucceeded
		o.Nodes[1].Status = api.JobRunRunning
	})
	info, err := output.Stat()
	if err != nil {
		return err
	}
	if err = verifyBackupZIP(ctx, output, info.Size(), manifestData, payload); err != nil {
		return err
	}
	if _, err = output.Seek(0, io.SeekStart); err != nil {
		return err
	}
	hash := sha256.New()
	if _, err = io.Copy(hash, &backupContextReader{ctx: ctx, reader: output}); err != nil {
		return err
	}
	for _, resource := range resources {
		if resource.Status != "included" || resource.Group != "sources" {
			continue
		}
		if err := p.verifyBackupSource(ctx, resource, req.Selection.Sources.Policy); err != nil {
			return err
		}
	}
	destination := backupArchiveDirectory + "/" + id + ".zip"
	if _, err = root.Lstat(destination); err == nil {
		return errors.New("backup archive already exists without a completed receipt")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err = root.Rename(temporary, destination); err != nil {
		return err
	}
	artifact := &api.BackupArtifact{Path: destination, SizeBytes: info.Size(), SHA256: hex.EncodeToString(hash.Sum(nil)), ManifestVersion: 1, Manifest: manifestData}
	completed, _ := p.JobRunGet(ctx, id)
	completed.Status = api.JobRunSucceeded
	now := time.Now().UTC()
	completed.EndedAt = &now
	completed.Artifact = artifact
	completed.CurrentStep = ""
	completed.Nodes[1].Status = api.JobRunSucceeded
	if err = p.persistJobReceipt(completed); err != nil {
		_ = root.Remove(destination)
		return err
	}
	p.jobRunsMu.Lock()
	p.jobRuns[id] = cloneJobRun(completed)
	p.jobRunsMu.Unlock()
	return nil
}

// appendCaptureOutput adds one capture's output to a backup receipt's, each
// line labelled with its resource (group/name), as docker compose labels
// services' lines. The output is redacted before it is labelled, since labels
// would split a registered multi-line value, and the receipt keeps the last
// whole lines within the job output bound, so a cut never leaves a line
// unlabelled.
func appendCaptureOutput(previous, label string, out []byte) string {
	text := runtime.PrefixLines(logctx.RedactText(string(out)), label, 0)
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return runtime.LimitLogBytes(previous+text, maxPersistedJobOutput)
}

// backupArchive writes a backup's ZIP entries and declares each, with its size,
// mode and SHA256, in the archive's manifest.
type backupArchive struct {
	ctx      context.Context
	writer   *zip.Writer
	manifest *api.BackupManifest
	names    map[string]bool
}

// errBackupEntryGone is add's error for an entry deleted before it was opened,
// when nothing of it has been written. A walk skips such an entry outside
// repository trees (walkBackupTree); an entry the backup requires, such as a
// capture's output or a recovery file, fails the backup.
var errBackupEntryGone = errors.New("was removed before it was copied")

// add copies one entry into the archive. The entry is opened before its header
// is written, so an entry that is gone by then leaves nothing in the archive.
func (a *backupArchive) add(source *os.Root, name string, info fs.FileInfo, archiveName string) error {
	if !fs.ValidPath(archiveName) || a.names[archiveName] {
		return fmt.Errorf("invalid or duplicate archive entry %q", archiveName)
	}
	var content io.Reader
	var file *os.File
	var before fs.FileInfo
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := source.Readlink(name)
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("archive entry %q %w", archiveName, errBackupEntryGone)
		}
		if err != nil {
			return err
		}
		content = strings.NewReader(target)
	} else if !info.IsDir() {
		var err error
		file, err = source.Open(name)
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("archive entry %q %w", archiveName, errBackupEntryGone)
		}
		if err != nil {
			return err
		}
		defer file.Close()
		before, err = file.Stat()
		if err != nil {
			return err
		}
		if !os.SameFile(info, before) || !before.Mode().IsRegular() {
			return fmt.Errorf("file changed before capture: %s", archiveName)
		}
		content = &backupContextReader{ctx: a.ctx, reader: file}
	}
	a.names[archiveName] = true
	header, err := zip.FileInfoHeader(info)
	if err != nil {
		return err
	}
	header.Name = archiveName
	header.Method = zip.Deflate
	if info.IsDir() {
		header.Name += "/"
		header.Method = zip.Store
	}
	destination, err := a.writer.CreateHeader(header)
	if err != nil {
		return err
	}
	hash := sha256.New()
	var size int64
	if content != nil {
		size, err = io.Copy(io.MultiWriter(destination, hash), content)
		if err != nil {
			return err
		}
	}
	if file != nil {
		after, err := file.Stat()
		if err != nil {
			return err
		}
		current, err := source.Lstat(name)
		if err != nil {
			return err
		}
		if size != info.Size() || !os.SameFile(before, current) || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
			return fmt.Errorf("file changed during capture: %s", archiveName)
		}
	}
	a.manifest.Entries = append(a.manifest.Entries, api.BackupEntry{Path: archiveName, SizeBytes: size, SHA256: hex.EncodeToString(hash.Sum(nil)), Mode: uint32(info.Mode())})
	return nil
}

// removeInterruptedBackup removes what a backup that never completed left in
// the stack root: its staging directory, which its own cleanup never removed
// and which may hold a plaintext dump and a partial archive with the recovery
// secrets, and an archive it published before writing the receipt that would
// have completed it. The caller holds the root lock, so no backup is running,
// and id is a checked operation ID, so neither path names another backup's.
func removeInterruptedBackup(root *os.Root, id string) error {
	if err := root.RemoveAll(backupStagingDirectory + "/" + id); err != nil {
		return err
	}
	if err := root.Remove(backupArchiveDirectory + "/" + id + ".zip"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

type backupContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *backupContextReader) Read(data []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(data)
}

func verifyBackupZIP(ctx context.Context, reader io.ReaderAt, size int64, manifest api.BackupManifest, payload []byte) error {
	archive, err := zip.NewReader(reader, size)
	if err != nil {
		return err
	}
	expected := map[string]api.BackupEntry{}
	for _, entry := range manifest.Entries {
		expected[entry.Path] = entry
	}
	seen := map[string]bool{}
	for _, entry := range archive.File {
		canonical := strings.TrimSuffix(entry.Name, "/")
		if !fs.ValidPath(canonical) || seen[canonical] {
			return errors.New("invalid or duplicate ZIP entry")
		}
		seen[canonical] = true
		content, err := entry.Open()
		if err != nil {
			return err
		}
		if entry.Name == "manifest.json" {
			data, err := io.ReadAll(io.LimitReader(content, int64(len(payload))+1))
			closeErr := content.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
			if !bytes.Equal(data, payload) {
				return errors.New("archive manifest verification failed")
			}
			continue
		}
		declared, ok := expected[canonical]
		if !ok {
			_ = content.Close()
			return fmt.Errorf("undeclared ZIP entry %q", entry.Name)
		}
		if uint32(entry.Mode()) != declared.Mode || entry.FileInfo().IsDir() != strings.HasSuffix(entry.Name, "/") {
			_ = content.Close()
			return fmt.Errorf("archive mode verification failed for %q", entry.Name)
		}
		hash := sha256.New()
		count, err := io.Copy(hash, &backupContextReader{ctx: ctx, reader: io.LimitReader(content, declared.SizeBytes+1)})
		closeErr := content.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if count != declared.SizeBytes || hex.EncodeToString(hash.Sum(nil)) != declared.SHA256 {
			return fmt.Errorf("archive verification failed for %q", entry.Name)
		}
	}
	if len(seen) != len(expected)+1 || !seen["manifest.json"] {
		return errors.New("archive is missing declared entries")
	}
	return nil
}

// BackupArchive opens only a completed artifact through the confined stack root.
// The caller owns the returned file. No path supplied by the transport is opened.
func (p *Platform) BackupArchive(ctx context.Context, id string) (io.ReadCloser, api.BackupArtifact, error) {
	op, err := p.JobRunGet(ctx, id)
	if err != nil {
		return nil, api.BackupArtifact{}, err
	}
	if op.Status != api.JobRunSucceeded || op.Artifact == nil {
		return nil, api.BackupArtifact{}, &ConflictError{Kind: "backup", Name: id, Reason: "artifact is not complete"}
	}
	root, err := os.OpenRoot(p.root)
	if err != nil {
		return nil, api.BackupArtifact{}, err
	}
	defer func() { _ = root.Close() }()
	expected := backupArchiveDirectory + "/" + id + ".zip"
	if op.Artifact.Path != expected {
		return nil, api.BackupArtifact{}, errors.New("backup receipt path mismatch")
	}
	info, err := root.Lstat(expected)
	if err != nil {
		return nil, api.BackupArtifact{}, err
	}
	if !info.Mode().IsRegular() {
		return nil, api.BackupArtifact{}, errors.New("backup archive is not regular")
	}
	file, err := root.Open(expected)
	return file, *op.Artifact, err
}

// IsBackupReceipt reports whether op is a backup's receipt rather than a job
// run's, by its nodes (kind "backup"), not its root job's name: a stack may
// declare a job called backup. A backup receipt's artifact names every archived
// path and the digest of the captured secrets, so the operator shows it only
// to the admin bearer that /backups requires.
func IsBackupReceipt(op api.JobRunOperation) bool {
	return len(op.Nodes) > 0 && op.Nodes[0].Kind == "backup"
}

func backupReceiptFor(existing api.JobRunOperation, digest string) (api.JobRunOperation, error) {
	if existing.RootJob != "backup" || existing.RequestDigest != digest {
		return api.JobRunOperation{}, &OperationError{Code: CodeConflict, Cause: CauseBackupRequestConflict, Title: "Backup request conflicts", Summary: "request_id already belongs to a different selection", Hint: "Start a new workflow run for the changed selection.", Err: &ConflictError{Kind: "backup request", Name: existing.ID, Reason: "request_id already belongs to a different selection"}}
	}
	return existing, nil
}

func (p *Platform) verifyBackupSource(ctx context.Context, resource backupResource, policy string) error {
	trees, evidence, err := p.backupSourceTrees(ctx, resource.Name, resource.Path, resource.sourceStore, nil, policy)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(evidence, resource.revision) || len(trees) != len(resource.trees) {
		return fmt.Errorf("source %q changed during capture", resource.Name)
	}
	for i, tree := range trees {
		previous := resource.trees[i]
		if tree.root != previous.root || tree.scope != previous.scope || tree.prefix != previous.prefix {
			return fmt.Errorf("source %q metadata location changed during capture", resource.Name)
		}
	}
	return nil
}

func backupPublicReason(ctx context.Context, err error) string {
	var publication *gitx.BackupPublicationError
	if errors.As(err, &publication) {
		return publication.Reason
	}
	var jobCapability *runtime.JobCapabilityError
	if errors.As(err, &jobCapability) {
		return jobCapability.Reason
	}
	var capability *vcs.BackupCapabilityError
	if errors.As(err, &capability) {
		return capability.Reason
	}
	logctx.From(ctx).WarnContext(ctx, "backup resource discovery failed", "error", logctx.RedactText(err.Error()))
	if errors.Is(err, os.ErrNotExist) {
		return "declared resource is not materialized"
	}
	if errors.Is(err, os.ErrPermission) {
		return "operator cannot read the declared resource"
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "resource discovery was interrupted"
	}
	return "resource cannot be captured safely; inspect operator diagnostics"
}
