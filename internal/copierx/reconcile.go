package copierx

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	renderStateVersion   = 1
	fingerprintRegular   = "file"
	fingerprintSymlink   = "symlink"
	fingerprintDirectory = "directory"
	fingerprintOther     = "other"
)

type ReconcileMode string

const (
	ReconcileCreate ReconcileMode = "create"
	ReconcileUpdate ReconcileMode = "update"
)

type ChangeKind string

const (
	ChangeAdd    ChangeKind = "add"
	ChangeModify ChangeKind = "modify"
	ChangeDelete ChangeKind = "delete"
	ChangeAdopt  ChangeKind = "adopt"
)

type ConflictReason string

const (
	ConflictLocallyModified    ConflictReason = "locally-modified"
	ConflictUntrackedDifferent ConflictReason = "untracked-different"
	ConflictTypeChanged        ConflictReason = "type-changed"
)

type Change struct {
	Path string     `json:"path"`
	Kind ChangeKind `json:"kind"`
}

type Conflict struct {
	Path   string         `json:"path"`
	Reason ConflictReason `json:"reason"`
}

type ReconcileResult struct {
	Changes   []Change   `json:"changes,omitempty"`
	Conflicts []Conflict `json:"conflicts,omitempty"`
}

type RenderLayer struct {
	Name          string
	Template      string
	StateTemplate string
	DestRoot      string
	Inputs        Inputs
}

type RenderLayerState struct {
	Name        string `json:"name"`
	Template    string `json:"template"`
	DestRoot    string `json:"dest_root,omitempty"`
	AnswersFile string `json:"answers_file,omitempty"`
}

type Fingerprint struct {
	Kind   string      `json:"kind"`
	SHA256 string      `json:"sha256,omitempty"`
	Mode   fs.FileMode `json:"mode,omitempty"`
	Link   string      `json:"link,omitempty"`
}

type RenderState struct {
	Version        int                    `json:"version"`
	Layers         []RenderLayerState     `json:"layers,omitempty"`
	Files          map[string]Fingerprint `json:"files,omitempty"`
	Documents      map[string][]byte      `json:"documents,omitempty"`
	ProtectedPaths []string               `json:"protected_paths,omitempty"`
}

type RenderPlan struct {
	Target                string
	TargetRoot            string
	StateRoot             string
	StatePath             string
	Layers                []RenderLayer
	Documents             []string
	AllowedSymlinkParents map[string]*TrustedRoot
	ProtectedPaths        []string
}

type ReconcileOptions struct {
	Mode      ReconcileMode
	DryRun    bool
	Overwrite bool
}

type PreparedReconcile struct {
	plan             RenderPlan
	options          ReconcileOptions
	scratch          string
	backup           string
	metadataPaths    []string
	protectedPaths   []string
	oldState         RenderState
	newState         RenderState
	result           ReconcileResult
	applyGuards      []*GuardedPath
	applyParentRoots map[string]*os.Root
	pruneGuards      map[string]*GuardedPath
	targetRoot       *TrustedRoot
	stateRoot        *TrustedRoot
	stateGuard       *GuardedPath
	stateTarget      string
	stateRel         string
	stateBefore      []byte
	stateInfo        fs.FileInfo
	stateExisted     bool
	expectedLive     map[string]livePathExpectation
}

type livePathExpectation struct {
	fingerprint Fingerprint
	exists      bool
}

func (p *PreparedReconcile) Close() error {
	if p == nil {
		return nil
	}
	var result error
	if err := p.closeApplyGuards(); err != nil {
		result = err
	}
	if p.targetRoot != nil {
		if err := p.targetRoot.Close(); err != nil && result == nil {
			result = err
		}
		p.targetRoot = nil
	}
	if p.stateRoot != nil {
		if err := p.stateRoot.Close(); err != nil && result == nil {
			result = err
		}
		p.stateRoot = nil
	}
	if p.stateGuard != nil {
		if err := p.stateGuard.Close(); err != nil && result == nil {
			result = err
		}
		p.stateGuard = nil
	}
	if p.scratch != "" {
		if err := os.RemoveAll(p.scratch); result == nil {
			result = err
		}
	}
	if p.backup != "" {
		if err := os.RemoveAll(p.backup); result == nil {
			result = err
		}
	}
	p.scratch = ""
	p.backup = ""
	return result
}

func (p *PreparedReconcile) Result() ReconcileResult {
	if p == nil {
		return ReconcileResult{}
	}
	return p.result
}

func PrepareReconcile(ctx context.Context, plan RenderPlan, opts ReconcileOptions) (*PreparedReconcile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if plan.Target == "" {
		return nil, fmt.Errorf("render target is required")
	}
	scratch, err := os.MkdirTemp("", "angee-reconcile-*")
	if err != nil {
		return nil, fmt.Errorf("create reconciliation scratch dir: %w", err)
	}
	targetCapabilityPath := plan.TargetRoot
	if targetCapabilityPath == "" {
		targetCapabilityPath, err = existingDirectoryAncestor(plan.Target)
		if err != nil {
			_ = os.RemoveAll(scratch)
			return nil, err
		}
	}
	targetRootCapability, err := OpenTrustedRoot(targetCapabilityPath)
	if err != nil {
		_ = os.RemoveAll(scratch)
		return nil, fmt.Errorf("open trusted render root %q: %w", targetCapabilityPath, err)
	}
	oldState := emptyRenderState()
	var stateBefore []byte
	var stateInfo fs.FileInfo
	var stateExisted bool
	var stateRootCapability *TrustedRoot
	var retainedStateGuard *GuardedPath
	stateTarget := ""
	stateRel := ""
	if plan.StatePath != "" {
		stateRoot, resolvedStateRel, stateErr := rootedStatePath(plan.StateRoot, plan.StatePath)
		if stateErr != nil {
			_ = targetRootCapability.Close()
			_ = os.RemoveAll(scratch)
			return nil, stateErr
		}
		stateTarget = stateRoot
		capabilityPath := stateRoot
		_, stateWithinTargetRoot, containErr := relativeContainedPath(targetRootCapability.Path(), stateRoot)
		if containErr != nil {
			_ = targetRootCapability.Close()
			_ = os.RemoveAll(scratch)
			return nil, containErr
		}
		stateEqualsTargetRoot := filepath.Clean(stateRoot) == targetRootCapability.Path()
		rootInfo, inspectErr := os.Lstat(stateRoot)
		rootExists := inspectErr == nil
		if inspectErr != nil && !os.IsNotExist(inspectErr) {
			_ = targetRootCapability.Close()
			_ = os.RemoveAll(scratch)
			return nil, inspectErr
		}
		if rootExists && (rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir()) {
			_ = targetRootCapability.Close()
			_ = os.RemoveAll(scratch)
			return nil, fmt.Errorf("render state root %q is not a real directory", stateRoot)
		}
		if !rootExists && !stateEqualsTargetRoot {
			capabilityPath, stateErr = existingDirectoryAncestor(stateRoot)
			if stateErr != nil {
				_ = targetRootCapability.Close()
				_ = os.RemoveAll(scratch)
				return nil, stateErr
			}
		}
		_, capabilityFromTarget, containErr := relativeContainedPath(targetRootCapability.Path(), capabilityPath)
		capabilityFromTarget = capabilityFromTarget && stateWithinTargetRoot
		if containErr != nil {
			stateErr = containErr
		} else if capabilityFromTarget {
			stateRootCapability, stateErr = targetRootCapability.retainPath(capabilityPath)
		} else {
			stateRootCapability, stateErr = OpenTrustedRoot(capabilityPath)
		}
		if stateErr != nil {
			_ = targetRootCapability.Close()
			_ = os.RemoveAll(scratch)
			return nil, fmt.Errorf("open render state root %q: %w", capabilityPath, stateErr)
		}
		stateRel = filepath.ToSlash(resolvedStateRel)
		stateGuard, guardErr := stateRootCapability.OpenGuardedPath(stateTarget, stateRel, nil)
		if guardErr != nil {
			_ = stateRootCapability.Close()
			_ = targetRootCapability.Close()
			_ = os.RemoveAll(scratch)
			return nil, fmt.Errorf("open render state %q: %w", plan.StatePath, guardErr)
		}
		oldState, stateBefore, stateInfo, stateExisted, stateErr = readRenderStateGuarded(stateGuard, plan.StatePath)
		if rootExists {
			_ = stateGuard.Close()
		} else {
			retainedStateGuard = stateGuard
			_ = stateRootCapability.Close()
			stateRootCapability = nil
		}
		if stateErr != nil {
			if retainedStateGuard != nil {
				_ = retainedStateGuard.Close()
			}
			_ = stateRootCapability.Close()
			_ = targetRootCapability.Close()
			_ = os.RemoveAll(scratch)
			return nil, stateErr
		}
	}
	prepared := &PreparedReconcile{
		plan:         plan,
		options:      opts,
		scratch:      scratch,
		oldState:     oldState,
		targetRoot:   targetRootCapability,
		stateRoot:    stateRootCapability,
		stateGuard:   retainedStateGuard,
		stateTarget:  stateTarget,
		stateRel:     stateRel,
		stateBefore:  stateBefore,
		stateInfo:    stateInfo,
		stateExisted: stateExisted,
		expectedLive: map[string]livePathExpectation{},
		newState: RenderState{
			Version:   renderStateVersion,
			Files:     map[string]Fingerprint{},
			Documents: map[string][]byte{},
		},
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = prepared.Close()
		}
	}()
	currentProtected, err := normalizeProtectedPaths(plan.ProtectedPaths)
	if err != nil {
		return nil, err
	}
	previousProtected, err := normalizeProtectedPaths(oldState.ProtectedPaths)
	if err != nil {
		return nil, fmt.Errorf("render state %q protected paths: %w", plan.StatePath, err)
	}
	prepared.newState.ProtectedPaths = currentProtected
	prepared.protectedPaths = unionSortedPaths(currentProtected, previousProtected, mapKeys(plan.AllowedSymlinkParents))

	metadata := map[string]struct{}{}
	for _, layer := range plan.Layers {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if layer.Template == "" {
			return nil, fmt.Errorf("render layer %q: template is required", layer.Name)
		}
		dest, err := safePlanJoin(scratch, layer.DestRoot)
		if err != nil {
			return nil, fmt.Errorf("render layer %q: %w", layer.Name, err)
		}
		cfg, err := readConfig(layer.Template)
		if err != nil {
			return nil, fmt.Errorf("render layer %q config: %w", layer.Name, err)
		}
		if err := validateRelativePath(cfg.AnswersFile); err != nil {
			return nil, fmt.Errorf("render layer %q answers file: %w", layer.Name, err)
		}
		answerRel := filepath.ToSlash(filepath.Clean(filepath.Join(layer.DestRoot, cfg.AnswersFile)))
		if _, err := safePlanJoin(scratch, answerRel); err != nil {
			return nil, fmt.Errorf("render layer %q answers file: %w", layer.Name, err)
		}
		if err := (LocalRenderer{}).Copy(ctx, CopyRequest{Template: layer.Template, Dest: dest, Inputs: layer.Inputs}); err != nil {
			return nil, fmt.Errorf("render layer %q: %w", layer.Name, err)
		}
		metadata[answerRel] = struct{}{}
		stateTemplate := layer.Template
		if layer.StateTemplate != "" {
			stateTemplate = layer.StateTemplate
		}
		prepared.newState.Layers = append(prepared.newState.Layers, RenderLayerState{
			Name:        layer.Name,
			Template:    stateTemplate,
			DestRoot:    filepath.ToSlash(filepath.Clean(layer.DestRoot)),
			AnswersFile: answerRel,
		})
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	documents := map[string]struct{}{}
	for _, path := range plan.Documents {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rel := filepath.ToSlash(filepath.Clean(path))
		if err := validateRelativePath(rel); err != nil {
			return nil, fmt.Errorf("render document: %w", err)
		}
		documents[rel] = struct{}{}
		renderedPath, err := safePlanJoin(scratch, rel)
		if err != nil {
			return nil, err
		}
		data, err := os.ReadFile(renderedPath)
		if err != nil {
			return nil, fmt.Errorf("read rendered document %q: %w", rel, err)
		}
		prepared.newState.Documents[rel] = data
	}
	for rel := range metadata {
		prepared.metadataPaths = append(prepared.metadataPaths, rel)
	}
	sort.Strings(prepared.metadataPaths)

	stateTargetPath, stateInsideTarget, err := relativeContainedPath(plan.Target, plan.StatePath)
	if err != nil {
		return nil, fmt.Errorf("validate render state path: %w", err)
	}
	var renderedPaths []string
	err = filepath.WalkDir(scratch, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if path == scratch {
			return nil
		}
		rel, err := filepath.Rel(scratch, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if stateInsideTarget && (rel == stateTargetPath || !entry.IsDir() && relativePathContains(rel, stateTargetPath)) {
			return fmt.Errorf("rendered path %q overlaps render state %q", rel, stateTargetPath)
		}
		if !entry.IsDir() && containsPath(prepared.protectedPaths, rel) {
			return fmt.Errorf("rendered path %q replaces a protected root", rel)
		}
		if entry.IsDir() {
			return nil
		}
		if _, ok := metadata[rel]; ok {
			return nil
		}
		if _, ok := documents[rel]; ok {
			return nil
		}
		fingerprint, exists, err := fingerprintPath(path)
		if err != nil {
			return err
		}
		if !exists {
			return nil
		}
		prepared.newState.Files[rel] = fingerprint
		renderedPaths = append(renderedPaths, rel)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk rendered tree: %w", err)
	}
	paths := make(map[string]struct{}, len(renderedPaths)+len(oldState.Files))
	for _, rel := range renderedPaths {
		paths[rel] = struct{}{}
	}
	for rel := range oldState.Files {
		if err := validateRelativePath(rel); err != nil {
			return nil, fmt.Errorf("render state %q: %w", plan.StatePath, err)
		}
		if !containsPath(prepared.protectedPaths, rel) {
			paths[rel] = struct{}{}
		}
	}
	sortedPaths := make([]string, 0, len(paths))
	for rel := range paths {
		sortedPaths = append(sortedPaths, rel)
	}
	sort.Strings(sortedPaths)
	for _, rel := range sortedPaths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		dest, err := prepared.OpenTargetPath(rel)
		if err != nil {
			return nil, err
		}
		current, currentExists, err := fingerprintGuardedPath(dest)
		_ = dest.Close()
		if err != nil {
			return nil, fmt.Errorf("fingerprint current %q: %w", rel, err)
		}
		prepared.expectedLive[rel] = livePathExpectation{fingerprint: current, exists: currentExists}
		old, oldExists := oldState.Files[rel]
		newFingerprint, newExists := prepared.newState.Files[rel]
		overwrite := opts.Overwrite || opts.Mode == ReconcileCreate
		change, conflict := reconcilePath(rel, old, oldExists, current, currentExists, newFingerprint, newExists, overwrite)
		if change != nil {
			prepared.result.Changes = append(prepared.result.Changes, *change)
		}
		if conflict != nil {
			prepared.result.Conflicts = append(prepared.result.Conflicts, *conflict)
		}
	}

	cleanup = false
	return prepared, nil
}

func (p *PreparedReconcile) RenderedDocument(path string) ([]byte, bool) {
	if p == nil {
		return nil, false
	}
	data, ok := p.newState.Documents[filepath.ToSlash(filepath.Clean(path))]
	return append([]byte(nil), data...), ok
}

func (p *PreparedReconcile) PreviousDocument(path string) ([]byte, bool) {
	if p == nil {
		return nil, false
	}
	data, ok := p.oldState.Documents[filepath.ToSlash(filepath.Clean(path))]
	return append([]byte(nil), data...), ok
}

// OpenTargetPath derives a guarded destination from the exact target-root
// capability retained during PrepareReconcile. Callers use it for special
// template-owned files that are merged outside ApplyFiles.
func (p *PreparedReconcile) OpenTargetPath(path string) (*GuardedPath, error) {
	if p == nil || p.targetRoot == nil {
		return nil, fmt.Errorf("render target capability is not available")
	}
	if p.stateRoot != nil && filepath.Clean(p.stateRoot.Path()) == filepath.Clean(p.stateTarget) {
		destination, err := safePlanJoin(p.plan.Target, path)
		if err != nil {
			return nil, err
		}
		stateRel, contained, err := relativeContainedPath(p.stateTarget, destination)
		if err != nil {
			return nil, err
		}
		if contained && stateRel != "." {
			allowed, err := allowedPathsRelativeTo(p.plan.Target, p.stateTarget, p.plan.AllowedSymlinkParents)
			if err != nil {
				return nil, err
			}
			return p.stateRoot.OpenGuardedPath(p.stateTarget, stateRel, allowed)
		}
	}
	return p.targetRoot.OpenGuardedPath(p.plan.Target, path, p.plan.AllowedSymlinkParents)
}

func allowedPathsRelativeTo(target, root string, allowed map[string]*TrustedRoot) (map[string]*TrustedRoot, error) {
	shifted := map[string]*TrustedRoot{}
	for path, trusted := range allowed {
		destination, err := safePlanJoin(target, path)
		if err != nil {
			return nil, err
		}
		rel, contained, err := relativeContainedPath(root, destination)
		if err != nil {
			return nil, err
		}
		if contained && rel != "." {
			shifted[filepath.ToSlash(rel)] = trusted
		}
	}
	return shifted, nil
}

// VerifyTargetRootPath confirms the public target-root pathname still names
// the directory capability captured during PrepareReconcile.
func (p *PreparedReconcile) VerifyTargetRootPath() error {
	if p == nil || p.targetRoot == nil {
		return fmt.Errorf("render target capability is not available")
	}
	return p.targetRoot.VerifyPath(p.targetRoot.Path())
}

// VerifyStateRootPath confirms an existing public state-root pathname still
// names the capability retained during PrepareReconcile. A state root that was
// absent at preparation remains guarded by stateGuard instead.
func (p *PreparedReconcile) VerifyStateRootPath() error {
	if p == nil || p.plan.StatePath == "" {
		return nil
	}
	if p.stateRoot == nil {
		if p.stateGuard != nil {
			return nil
		}
		return fmt.Errorf("render state capability is not available")
	}
	return p.stateRoot.VerifyPath(p.stateTarget)
}

// VerifyRootCapability confirms root is represented by the same retained
// directory in both this reconciliation and another guarded transaction.
func (p *PreparedReconcile) VerifyRootCapability(root string, other *GuardedPath) error {
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	abs = filepath.Clean(abs)
	for _, trusted := range []*TrustedRoot{p.stateRoot, p.targetRoot} {
		if trusted != nil && trusted.Path() == abs {
			return other.VerifyParentTrustedRoot(trusted)
		}
	}
	return fmt.Errorf("prepared reconciliation retains no root capability for %q", root)
}

type applyJournalEntry struct {
	path    string
	backup  string
	existed bool
	dest    *GuardedPath
}

func (p *PreparedReconcile) closeApplyGuards() error {
	var result error
	for _, guard := range p.applyGuards {
		if err := guard.Close(); err != nil && result == nil {
			result = err
		}
	}
	p.applyGuards = nil
	for key, root := range p.applyParentRoots {
		if err := root.Close(); err != nil && result == nil {
			result = fmt.Errorf("close retained apply parent %q: %w", key, err)
		}
	}
	p.applyParentRoots = nil
	p.pruneGuards = nil
	return result
}

func applyParentKey(path string) string {
	return filepath.ToSlash(filepath.Dir(filepath.Clean(filepath.FromSlash(path))))
}

func (p *PreparedReconcile) openApplyPath(path string) (*GuardedPath, string, error) {
	key := applyParentKey(path)
	if root := p.applyParentRoots[key]; root != nil {
		guard := &GuardedPath{parent: root, leaf: filepath.Base(filepath.Clean(filepath.FromSlash(path)))}
		if err := guard.captureEntry(); err != nil {
			return nil, "", err
		}
		return guard, key, nil
	}
	guard, err := p.OpenTargetPath(path)
	return guard, key, err
}

func (p *PreparedReconcile) retainApplyParent(key string, guard *GuardedPath) error {
	if p.applyParentRoots == nil {
		p.applyParentRoots = map[string]*os.Root{}
	}
	if p.applyParentRoots[key] != nil {
		return nil
	}
	root, err := guard.detachParentRoot()
	if err != nil {
		return err
	}
	p.applyParentRoots[key] = root
	return nil
}

func (p *PreparedReconcile) retainEmptyParentCandidates(path string) error {
	parent := filepath.Dir(filepath.Clean(filepath.FromSlash(path)))
	if parent == "." || parent == "" {
		return nil
	}
	if p.pruneGuards == nil {
		p.pruneGuards = map[string]*GuardedPath{}
	}
	for current := parent; current != "." && current != ""; current = filepath.Dir(current) {
		rel := filepath.ToSlash(current)
		if containsPath(p.protectedPaths, rel) {
			continue
		}
		if fingerprint, managed := p.newState.Files[rel]; managed && fingerprint.Kind == fingerprintDirectory {
			continue
		}
		if _, retained := p.pruneGuards[rel]; retained {
			continue
		}
		guard, err := p.OpenTargetPath(rel)
		if err != nil {
			return err
		}
		info, exists, err := guard.Lstat()
		if err != nil {
			_ = guard.Close()
			return err
		}
		if !exists || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			_ = guard.Close()
			continue
		}
		p.pruneGuards[rel] = guard
		p.applyGuards = append(p.applyGuards, guard)
	}
	return nil
}

func (p *PreparedReconcile) pruneEmptyParents() {
	paths := make([]string, 0, len(p.pruneGuards))
	for path := range p.pruneGuards {
		paths = append(paths, path)
	}
	sort.Slice(paths, func(i, j int) bool {
		leftDepth := strings.Count(paths[i], "/")
		rightDepth := strings.Count(paths[j], "/")
		if leftDepth != rightDepth {
			return leftDepth > rightDepth
		}
		return paths[i] > paths[j]
	})
	for _, path := range paths {
		_ = p.pruneGuards[path].Remove()
	}
}

func (p *PreparedReconcile) ApplyFiles(ctx context.Context) (func() error, error) {
	if p == nil {
		return nil, fmt.Errorf("prepared reconciliation is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(p.result.Conflicts) != 0 {
		return nil, fmt.Errorf("template reconciliation has %d conflict(s)", len(p.result.Conflicts))
	}
	if p.options.DryRun {
		return func() error { return nil }, nil
	}
	if p.backup != "" {
		return nil, fmt.Errorf("template reconciliation has already been applied")
	}
	backup, err := os.MkdirTemp("", "angee-reconcile-backup-*")
	if err != nil {
		return nil, fmt.Errorf("create reconciliation backup: %w", err)
	}
	p.backup = backup

	type operation struct {
		path   string
		delete bool
	}
	operations := make([]operation, 0, len(p.result.Changes)+len(p.metadataPaths))
	for _, change := range p.result.Changes {
		if change.Kind == ChangeAdopt {
			continue
		}
		operations = append(operations, operation{path: change.Path, delete: change.Kind == ChangeDelete})
	}
	for _, path := range p.metadataPaths {
		operations = append(operations, operation{path: path})
	}
	sort.SliceStable(operations, func(i, j int) bool {
		if operations[i].delete != operations[j].delete {
			return operations[i].delete
		}
		if operations[i].delete {
			leftDepth := strings.Count(filepath.ToSlash(operations[i].path), "/")
			rightDepth := strings.Count(filepath.ToSlash(operations[j].path), "/")
			if leftDepth != rightDepth {
				return leftDepth > rightDepth
			}
		}
		return operations[i].path < operations[j].path
	})

	journal := make([]applyJournalEntry, 0, len(operations))
	rolledBack := false
	rollback := func() error {
		if rolledBack {
			return nil
		}
		rolledBack = true
		defer func() { _ = p.closeApplyGuards() }()
		var result error
		for i := len(journal) - 1; i >= 0; i-- {
			entry := journal[i]
			if err := entry.dest.RemoveAll(); err != nil && result == nil {
				result = err
			}
			if entry.existed {
				if err := copyEntryToGuarded(context.Background(), entry.backup, entry.dest); err != nil && result == nil {
					result = err
				}
			} else {
				if err := entry.dest.RemoveMissingParents(); err != nil && result == nil {
					result = err
				}
			}
		}
		return result
	}
	fail := func(primary error) (func() error, error) {
		return nil, errors.Join(primary, rollback())
	}

	for index, operation := range operations {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		dest, parentKey, err := p.openApplyPath(operation.path)
		if err != nil {
			return fail(err)
		}
		entry := applyJournalEntry{path: operation.path, backup: filepath.Join(backup, fmt.Sprintf("%06d", index)), dest: dest}
		p.applyGuards = append(p.applyGuards, dest)
		current, exists, err := fingerprintGuardedPath(dest)
		if err != nil {
			return fail(fmt.Errorf("backup destination %q: %w", operation.path, err))
		}
		if expected, ok := p.expectedLive[operation.path]; ok && (exists != expected.exists || exists && current != expected.fingerprint) {
			return fail(fmt.Errorf("destination %q changed after template reconciliation was prepared", operation.path))
		}
		if exists {
			entry.existed = true
			if err := copyGuardedEntry(ctx, dest, entry.backup); err != nil {
				return fail(fmt.Errorf("backup destination %q: %w", operation.path, err))
			}
		}
		journal = append(journal, entry)
		if operation.delete {
			if err := p.retainEmptyParentCandidates(operation.path); err != nil {
				return fail(fmt.Errorf("retain empty parent candidates for %q: %w", operation.path, err))
			}
		}

		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		if operation.delete {
			if err := dest.RemoveAll(); err != nil {
				return fail(fmt.Errorf("delete rendered path %q: %w", operation.path, err))
			}
			if err := p.retainApplyParent(parentKey, dest); err != nil {
				return fail(fmt.Errorf("retain apply parent for %q: %w", operation.path, err))
			}
			continue
		}
		source, err := safePlanJoin(p.scratch, operation.path)
		if err != nil {
			return fail(err)
		}
		if err := copyEntryToGuarded(ctx, source, dest); err != nil {
			return fail(fmt.Errorf("install rendered path %q: %w", operation.path, err))
		}
		if err := p.retainApplyParent(parentKey, dest); err != nil {
			return fail(fmt.Errorf("retain apply parent for %q: %w", operation.path, err))
		}
	}
	return rollback, nil
}

func (p *PreparedReconcile) SaveState(ctx context.Context) error {
	if p == nil {
		return fmt.Errorf("prepared reconciliation is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(p.result.Conflicts) != 0 {
		return fmt.Errorf("template reconciliation has %d conflict(s)", len(p.result.Conflicts))
	}
	if p.options.DryRun || p.plan.StatePath == "" {
		return nil
	}
	data, err := json.MarshalIndent(p.newState, "", "  ")
	if err != nil {
		return fmt.Errorf("encode render state: %w", err)
	}
	data = append(data, '\n')
	if p.stateRoot == nil && p.stateGuard == nil {
		return fmt.Errorf("render state capability is not available")
	}
	state := p.stateGuard
	closeState := false
	if state == nil {
		state, err = p.stateRoot.OpenGuardedPath(p.stateTarget, p.stateRel, nil)
		if err != nil {
			return fmt.Errorf("open retained render state %q: %w", p.plan.StatePath, err)
		}
		closeState = true
	}
	if closeState {
		defer func() { _ = state.Close() }()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	current, currentInfo, currentExists, err := state.ReadRegularFile()
	if err != nil {
		return fmt.Errorf("revalidate render state %q: %w", p.plan.StatePath, err)
	}
	if currentExists != p.stateExisted || currentExists && (!bytes.Equal(current, p.stateBefore) || p.stateInfo == nil || !os.SameFile(p.stateInfo, currentInfo)) {
		return fmt.Errorf("render state %q changed after template reconciliation was prepared", p.plan.StatePath)
	}
	if err := state.WriteFile(data, 0o644); err != nil {
		_ = state.RemoveMissingParents()
		return fmt.Errorf("replace render state %q: %w", p.plan.StatePath, err)
	}
	// The state rename is the commit point. A close error cannot safely turn a
	// committed transaction back into a failure, because callers would then
	// roll the files back while the new state remains persisted.
	p.pruneEmptyParents()
	_ = p.closeApplyGuards()
	return nil
}

func safePlanJoin(root, rel string) (string, error) {
	if rel == "" || rel == "." {
		return root, nil
	}
	if err := validateRelativePath(rel); err != nil {
		return "", err
	}
	return filepath.Join(root, filepath.Clean(rel)), nil
}

func validateRelativePath(path string) error {
	if path == "" || filepath.IsAbs(path) {
		return fmt.Errorf("path %q must be a non-empty relative path", path)
	}
	clean := filepath.Clean(filepath.FromSlash(path))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path %q escapes render target", path)
	}
	return nil
}

func ReadRenderState(path string) (RenderState, bool, error) {
	return ReadRenderStateRooted(filepath.Dir(path), path)
}

func emptyRenderState() RenderState {
	return RenderState{
		Version:   renderStateVersion,
		Files:     map[string]Fingerprint{},
		Documents: map[string][]byte{},
	}
}

func RemoveRenderStateRooted(stateRoot, path string) error {
	rootPath, rel, err := rootedStatePath(stateRoot, path)
	if err != nil {
		return err
	}
	state, err := OpenGuardedPath("", rootPath, filepath.ToSlash(rel), nil)
	if err != nil {
		return fmt.Errorf("open render state %q: %w", path, err)
	}
	defer func() { _ = state.Close() }()
	if err := state.Remove(); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove render state %q: %w", path, err)
	}
	return nil
}

func ReadRenderStateRooted(stateRoot, path string) (RenderState, bool, error) {
	empty := emptyRenderState()
	if path == "" {
		return empty, false, nil
	}
	rootPath, rel, err := rootedStatePath(stateRoot, path)
	if err != nil {
		return RenderState{}, false, err
	}
	statePath, err := OpenGuardedPath("", rootPath, filepath.ToSlash(rel), nil)
	if err != nil {
		return RenderState{}, false, fmt.Errorf("open render state %q: %w", path, err)
	}
	defer func() { _ = statePath.Close() }()
	state, _, _, exists, err := readRenderStateGuarded(statePath, path)
	return state, exists, err
}

func readRenderStateGuarded(statePath *GuardedPath, path string) (RenderState, []byte, fs.FileInfo, bool, error) {
	empty := emptyRenderState()
	data, info, exists, err := statePath.ReadRegularFile()
	if err != nil {
		return RenderState{}, nil, nil, false, fmt.Errorf("read render state %q: %w", path, err)
	}
	if !exists {
		return empty, nil, nil, false, nil
	}
	var state RenderState
	if err := json.Unmarshal(data, &state); err != nil {
		return RenderState{}, nil, nil, false, fmt.Errorf("decode render state %q: %w", path, err)
	}
	if state.Version != renderStateVersion {
		return RenderState{}, nil, nil, false, fmt.Errorf("decode render state %q: unsupported version %d", path, state.Version)
	}
	if state.Files == nil {
		state.Files = map[string]Fingerprint{}
	}
	if state.Documents == nil {
		state.Documents = map[string][]byte{}
	}
	return state, append([]byte(nil), data...), info, true, nil
}

func rootedStatePath(stateRoot, path string) (string, string, error) {
	if path == "" {
		return "", "", fmt.Errorf("render state path is required")
	}
	if stateRoot == "" {
		stateRoot = filepath.Dir(path)
	}
	root, err := filepath.Abs(stateRoot)
	if err != nil {
		return "", "", fmt.Errorf("resolve render state root %q: %w", stateRoot, err)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", "", fmt.Errorf("resolve render state path %q: %w", path, err)
	}
	rel, err := filepath.Rel(root, absPath)
	if err != nil {
		return "", "", fmt.Errorf("resolve render state path %q: %w", path, err)
	}
	if err := validateRelativePath(rel); err != nil {
		return "", "", fmt.Errorf("render state path %q: %w", path, err)
	}
	rootInfo, err := os.Lstat(root)
	if err == nil {
		if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
			return "", "", fmt.Errorf("render state root %q is not a real directory", root)
		}
		rootHandle, openErr := openVerifiedRoot(root)
		if openErr != nil {
			return "", "", fmt.Errorf("open render state root %q: %w", root, openErr)
		}
		defer func() { _ = rootHandle.Close() }()
		if err := validateRootParents(rootHandle, rel); err != nil {
			return "", "", fmt.Errorf("render state path %q: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return "", "", fmt.Errorf("inspect render state root %q: %w", root, err)
	}
	return root, rel, nil
}

func createRootedTemp(root *os.Root, dir string) (string, *os.File, error) {
	for range 100 {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", nil, err
		}
		name := filepath.Join(dir, ".template-state-"+hex.EncodeToString(random[:]))
		file, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return name, file, nil
		}
		if !os.IsExist(err) {
			return "", nil, err
		}
	}
	return "", nil, fmt.Errorf("could not allocate a unique temporary file")
}

func relativeContainedPath(root, path string) (string, bool, error) {
	if path == "" {
		return "", false, nil
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", false, err
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", false, err
	}
	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil {
		return "", false, err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false, nil
	}
	return filepath.ToSlash(filepath.Clean(rel)), true, nil
}

func relativePathContains(parent, child string) bool {
	parent = strings.TrimSuffix(filepath.ToSlash(filepath.Clean(parent)), "/")
	child = filepath.ToSlash(filepath.Clean(child))
	return parent != "." && strings.HasPrefix(child, parent+"/")
}

func fingerprintPath(path string) (Fingerprint, bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return Fingerprint{}, false, nil
	}
	if err != nil {
		return Fingerprint{}, false, err
	}
	mode := info.Mode()
	switch {
	case mode.IsRegular():
		data, err := os.ReadFile(path)
		if err != nil {
			return Fingerprint{}, false, err
		}
		sum := sha256.Sum256(data)
		return Fingerprint{Kind: fingerprintRegular, SHA256: hex.EncodeToString(sum[:]), Mode: mode.Perm()}, true, nil
	case mode&os.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return Fingerprint{}, false, err
		}
		return Fingerprint{Kind: fingerprintSymlink, Link: target}, true, nil
	case mode.IsDir():
		return Fingerprint{Kind: fingerprintDirectory, Mode: mode.Perm()}, true, nil
	default:
		return Fingerprint{Kind: fingerprintOther, Mode: mode}, true, nil
	}
}

func fingerprintGuardedPath(path *GuardedPath) (Fingerprint, bool, error) {
	info, exists, err := path.Lstat()
	if err != nil || !exists {
		return Fingerprint{}, exists, err
	}
	mode := info.Mode()
	switch {
	case mode.IsRegular():
		data, verified, _, err := path.ReadRegularFile()
		if err != nil {
			return Fingerprint{}, false, err
		}
		sum := sha256.Sum256(data)
		return Fingerprint{Kind: fingerprintRegular, SHA256: hex.EncodeToString(sum[:]), Mode: verified.Mode().Perm()}, true, nil
	case mode&os.ModeSymlink != 0:
		target, err := readRootedSymlinkExpected(path.parent, path.leaf, path.entry)
		if err != nil {
			return Fingerprint{}, false, err
		}
		return Fingerprint{Kind: fingerprintSymlink, Link: target}, true, nil
	case mode.IsDir():
		return Fingerprint{Kind: fingerprintDirectory, Mode: mode.Perm()}, true, nil
	default:
		return Fingerprint{Kind: fingerprintOther, Mode: mode}, true, nil
	}
}

func reconcilePath(path string, old Fingerprint, oldExists bool, current Fingerprint, currentExists bool, newFingerprint Fingerprint, newExists bool, overwrite bool) (*Change, *Conflict) {
	if !oldExists {
		switch {
		case !newExists:
			return nil, nil
		case !currentExists:
			return &Change{Path: path, Kind: ChangeAdd}, nil
		case current == newFingerprint:
			return &Change{Path: path, Kind: ChangeAdopt}, nil
		case overwrite:
			return &Change{Path: path, Kind: ChangeModify}, nil
		default:
			reason := ConflictUntrackedDifferent
			if current.Kind != newFingerprint.Kind {
				reason = ConflictTypeChanged
			}
			return nil, &Conflict{Path: path, Reason: reason}
		}
	}

	if !newExists {
		switch {
		case !currentExists:
			return nil, nil
		case current == old || overwrite:
			return &Change{Path: path, Kind: ChangeDelete}, nil
		default:
			return nil, &Conflict{Path: path, Reason: ConflictLocallyModified}
		}
	}

	switch {
	case !currentExists:
		if overwrite {
			return &Change{Path: path, Kind: ChangeAdd}, nil
		}
		return nil, &Conflict{Path: path, Reason: ConflictLocallyModified}
	case current == newFingerprint:
		if old == newFingerprint {
			return nil, nil
		}
		return &Change{Path: path, Kind: ChangeAdopt}, nil
	case current == old:
		return &Change{Path: path, Kind: ChangeModify}, nil
	case overwrite:
		return &Change{Path: path, Kind: ChangeModify}, nil
	default:
		reason := ConflictLocallyModified
		if current.Kind != newFingerprint.Kind {
			reason = ConflictTypeChanged
		}
		return nil, &Conflict{Path: path, Reason: reason}
	}
}

// TrustedRoot retains the directory identity approved for a local workspace
// Source. Callers can verify its pathname without reopening that pathname for
// subsequent reads or writes.
type TrustedRoot struct {
	root     *os.Root
	path     string
	identity fs.FileInfo
}

// OpenTrustedRoot opens path once and retains its directory identity.
func OpenTrustedRoot(path string) (*TrustedRoot, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	abs = filepath.Clean(abs)
	root, err := openVerifiedRoot(abs)
	if err != nil {
		return nil, err
	}
	identity, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	return &TrustedRoot{root: root, path: abs, identity: identity}, nil
}

// Path returns the stable pathname recorded for this trusted root.
func (r *TrustedRoot) Path() string {
	if r == nil {
		return ""
	}
	return r.path
}

// VerifyPath confirms path still identifies the retained directory.
func (r *TrustedRoot) VerifyPath(path string) error {
	if r == nil || r.root == nil {
		return fmt.Errorf("trusted root capability is closed")
	}
	retained, err := r.root.Stat(".")
	if err != nil {
		return err
	}
	if !os.SameFile(r.identity, retained) {
		return fmt.Errorf("trusted root %q changed identity", r.path)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	candidate, err := openVerifiedRoot(resolved)
	if err != nil {
		return err
	}
	defer func() { _ = candidate.Close() }()
	actual, err := candidate.Stat(".")
	if err != nil {
		return err
	}
	if !os.SameFile(r.identity, actual) {
		return fmt.Errorf("path %q no longer identifies trusted root %q", path, r.path)
	}
	return nil
}

func (r *TrustedRoot) clone() (*os.Root, error) {
	if err := r.VerifyPath(r.path); err != nil {
		return nil, err
	}
	return r.cloneRetained()
}

func (r *TrustedRoot) cloneRetained() (*os.Root, error) {
	if r == nil || r.root == nil {
		return nil, fmt.Errorf("trusted root capability is closed")
	}
	retained, err := r.root.Stat(".")
	if err != nil {
		return nil, err
	}
	if !os.SameFile(r.identity, retained) {
		return nil, fmt.Errorf("trusted root %q changed identity", r.path)
	}
	clone, err := r.root.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	identity, err := clone.Stat(".")
	if err != nil {
		_ = clone.Close()
		return nil, err
	}
	if !os.SameFile(r.identity, identity) {
		_ = clone.Close()
		return nil, fmt.Errorf("trusted root %q changed while cloning capability", r.path)
	}
	return clone, nil
}

// Retain creates an independently owned capability for the same retained
// directory without reopening its public pathname.
func (r *TrustedRoot) Retain() (*TrustedRoot, error) {
	clone, err := r.cloneRetained()
	if err != nil {
		return nil, err
	}
	identity, err := clone.Stat(".")
	if err != nil {
		_ = clone.Close()
		return nil, err
	}
	return &TrustedRoot{root: clone, path: r.path, identity: identity}, nil
}

func (r *TrustedRoot) retainPath(path string) (*TrustedRoot, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	abs = filepath.Clean(abs)
	rel, contained, err := relativeContainedPath(r.path, abs)
	if err != nil {
		return nil, err
	}
	if !contained {
		return nil, fmt.Errorf("path %q escapes trusted root %q", abs, r.path)
	}
	if rel == "." {
		return r.Retain()
	}
	current, err := r.cloneRetained()
	if err != nil {
		return nil, err
	}
	for _, part := range splitPathParts(rel) {
		info, err := current.Lstat(part)
		if err != nil {
			_ = current.Close()
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			_ = current.Close()
			return nil, fmt.Errorf("trusted subroot %q is not a real directory", abs)
		}
		child, err := openRootedSubroot(current, part)
		_ = current.Close()
		if err != nil {
			return nil, err
		}
		current = child
	}
	identity, err := current.Stat(".")
	if err != nil {
		_ = current.Close()
		return nil, err
	}
	return &TrustedRoot{root: current, path: abs, identity: identity}, nil
}

// Close releases the retained source capability.
func (r *TrustedRoot) Close() error {
	if r == nil || r.root == nil {
		return nil
	}
	root := r.root
	r.root = nil
	return root.Close()
}

type guardedCreatedDirectory struct {
	parent   *os.Root
	name     string
	identity fs.FileInfo
}

// GuardedPath is a destination capability anchored at the deepest verified
// parent directory. Mutating methods never re-traverse the original pathname.
type GuardedPath struct {
	roots          []*os.Root
	parent         *os.Root
	leaf           string
	pendingParents []string
	createdParents []guardedCreatedDirectory
	cleanupEntries []string
	entry          fs.FileInfo
	exists         bool
	entryRoot      *os.Root
}

func openVerifiedRoot(path string) (*os.Root, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return nil, fmt.Errorf("root %q is not a real directory", path)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	after, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	if !os.SameFile(before, after) {
		_ = root.Close()
		return nil, fmt.Errorf("root %q changed while it was being opened", path)
	}
	return root, nil
}

// OpenGuardedPath opens a rooted destination capability. It rejects symlinks
// in the render target path, routes declared Source descendants through their
// retained roots, and anchors at the deepest verified destination parent.
func OpenGuardedPath(targetRoot, target, rel string, allowedSymlinkParents map[string]*TrustedRoot) (*GuardedPath, error) {
	return openGuardedPath(targetRoot, target, rel, allowedSymlinkParents, nil)
}

// OpenGuardedPath derives a destination guard from this retained root without
// reopening the root pathname. It is used to commit state into the exact root
// captured while the render transaction was prepared.
func (r *TrustedRoot) OpenGuardedPath(target, rel string, allowedSymlinkParents map[string]*TrustedRoot) (*GuardedPath, error) {
	if r == nil {
		return nil, fmt.Errorf("trusted root capability is nil")
	}
	return openGuardedPath(r.path, target, rel, allowedSymlinkParents, r)
}

func openGuardedPath(targetRoot, target, rel string, allowedSymlinkParents map[string]*TrustedRoot, retainedRoot *TrustedRoot) (*GuardedPath, error) {
	if err := validateRelativePath(rel); err != nil {
		return nil, err
	}
	if targetRoot == "" {
		var err error
		targetRoot, err = existingDirectoryAncestor(target)
		if err != nil {
			return nil, err
		}
	}
	targetRoot, err := filepath.Abs(targetRoot)
	if err != nil {
		return nil, err
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return nil, err
	}
	targetRel, contained, err := relativeContainedPath(targetRoot, target)
	if err != nil {
		return nil, err
	}
	if !contained {
		return nil, fmt.Errorf("render target %q escapes trusted root %q", target, targetRoot)
	}
	allowed, err := normalizedAllowedSymlinkParents(allowedSymlinkParents)
	if err != nil {
		return nil, err
	}
	var root *os.Root
	if retainedRoot != nil {
		root, err = retainedRoot.cloneRetained()
	} else {
		root, err = openVerifiedRoot(targetRoot)
	}
	if err != nil {
		return nil, fmt.Errorf("open trusted render root %q: %w", targetRoot, err)
	}
	nativeRel := filepath.Clean(filepath.FromSlash(rel))
	targetParts := splitPathParts(filepath.FromSlash(targetRel))
	relParts := splitPathParts(nativeRel)
	parentParts := append(append([]string(nil), targetParts...), relParts[:len(relParts)-1]...)
	guard := &GuardedPath{roots: []*os.Root{root}, parent: root, leaf: relParts[len(relParts)-1]}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = guard.Close()
		}
	}()

	for index, part := range parentParts {
		relIndex := index - len(targetParts)
		currentRel := ""
		if relIndex >= 0 {
			currentRel = filepath.Join(relParts[:relIndex+1]...)
		}
		expectedRoot, expectedSymlink := allowed[currentRel]
		entry, statErr := guard.parent.Lstat(part)
		if os.IsNotExist(statErr) {
			if expectedSymlink {
				return nil, fmt.Errorf("declared symlink parent %q is missing", filepath.Join(target, currentRel))
			}
			operationParent := filepath.Join(relParts[:len(relParts)-1]...)
			for allowedPath := range allowed {
				onOperationPath := allowedPath == operationParent || strings.HasPrefix(operationParent, allowedPath+string(filepath.Separator))
				belowMissingParent := relIndex < 0 || allowedPath == currentRel || strings.HasPrefix(allowedPath, currentRel+string(filepath.Separator))
				if onOperationPath && belowMissingParent {
					return nil, fmt.Errorf("declared symlink parent %q is beneath missing destination parent %q", filepath.Join(target, allowedPath), filepath.Join(target, currentRel))
				}
			}
			guard.pendingParents = append([]string(nil), parentParts[index:]...)
			break
		}
		if statErr != nil {
			return nil, fmt.Errorf("inspect destination parent %q: %w", filepath.Join(target, currentRel), statErr)
		}
		var child *os.Root
		if expectedSymlink {
			if entry.Mode()&os.ModeSymlink == 0 {
				return nil, fmt.Errorf("declared symlink parent %q is not a symlink", filepath.Join(target, currentRel))
			}
			link, err := readRootedSymlink(guard.parent, part)
			if err != nil {
				return nil, fmt.Errorf("read destination parent %q: %w", filepath.Join(target, currentRel), err)
			}
			actualTarget := link
			if !filepath.IsAbs(actualTarget) {
				actualTarget = filepath.Join(filepath.Dir(filepath.Join(target, currentRel)), actualTarget)
			}
			if err := expectedRoot.VerifyPath(filepath.Clean(actualTarget)); err != nil {
				return nil, fmt.Errorf("verify destination parent %q: %w", filepath.Join(target, currentRel), err)
			}
			child, err = expectedRoot.clone()
			if err != nil {
				return nil, fmt.Errorf("open expected Source root %q: %w", expectedRoot.Path(), err)
			}
		} else {
			if entry.Mode()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf("destination parent %q is an undeclared symlink", filepath.Join(target, currentRel))
			}
			if !entry.IsDir() {
				return nil, fmt.Errorf("destination parent %q is not a directory", filepath.Join(target, currentRel))
			}
			child, err = openRootedSubroot(guard.parent, part)
			if err != nil {
				return nil, err
			}
		}
		_ = guard.parent.Close()
		guard.roots[len(guard.roots)-1] = child
		guard.parent = child
	}
	if len(guard.pendingParents) == 0 {
		if err := guard.captureEntry(); err != nil {
			return nil, err
		}
	}
	closeOnError = false
	return guard, nil
}

func normalizedAllowedSymlinkParents(values map[string]*TrustedRoot) (map[string]*TrustedRoot, error) {
	allowed := make(map[string]*TrustedRoot, len(values))
	for path, target := range values {
		clean := filepath.Clean(filepath.FromSlash(path))
		if err := validateRelativePath(clean); err != nil {
			return nil, fmt.Errorf("allowed symlink parent %q: %w", path, err)
		}
		if target == nil || target.root == nil {
			return nil, fmt.Errorf("allowed symlink parent %q target capability is closed", path)
		}
		allowed[clean] = target
	}
	return allowed, nil
}

func splitPathParts(path string) []string {
	clean := filepath.Clean(path)
	if clean == "." || clean == "" {
		return nil
	}
	return strings.Split(clean, string(filepath.Separator))
}

func existingDirectoryAncestor(path string) (string, error) {
	current, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	for {
		info, err := os.Lstat(current)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return "", fmt.Errorf("trusted render ancestor %q is not a real directory", current)
			}
			return current, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("render target %q has no existing directory ancestor", path)
		}
		current = parent
	}
}

func validateRootParents(root *os.Root, rel string) error {
	parts := strings.Split(filepath.Clean(rel), string(filepath.Separator))
	current := ""
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		info, err := root.Lstat(current)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("destination parent %q is not a real directory", current)
		}
	}
	return nil
}

// Close releases the rooted destination capability.
func (p *GuardedPath) Close() error {
	if p == nil {
		return nil
	}
	var result error
	if p.parent != nil {
		for _, name := range p.cleanupEntries {
			if err := p.parent.RemoveAll(name); err != nil && !os.IsNotExist(err) && result == nil {
				result = err
			}
		}
	}
	p.cleanupEntries = nil
	if p.entryRoot != nil {
		if err := p.entryRoot.Close(); err != nil {
			result = err
		}
		p.entryRoot = nil
	}
	for index := len(p.roots) - 1; index >= 0; index-- {
		if p.roots[index] == nil {
			continue
		}
		if err := p.roots[index].Close(); err != nil && result == nil {
			result = err
		}
	}
	p.roots = nil
	p.parent = nil
	return result
}

func (p *GuardedPath) openedRoot() (*os.Root, error) {
	if p == nil || p.parent == nil {
		return nil, fmt.Errorf("guarded destination capability is closed")
	}
	return p.parent, nil
}

// detachParentRoot transfers ownership of the deepest parent capability to a
// transaction-level pool. The GuardedPath keeps using it but will not close it.
func (p *GuardedPath) detachParentRoot() (*os.Root, error) {
	if p == nil || p.parent == nil {
		return nil, fmt.Errorf("guarded destination capability is closed")
	}
	for index, root := range p.roots {
		if root != p.parent {
			continue
		}
		p.roots = append(p.roots[:index], p.roots[index+1:]...)
		return root, nil
	}
	return nil, fmt.Errorf("guarded destination parent capability is not owned")
}

func (p *GuardedPath) captureEntry() error {
	root, err := p.openedRoot()
	if err != nil {
		return err
	}
	info, err := root.Lstat(p.leaf)
	if os.IsNotExist(err) {
		p.exists = false
		p.entry = nil
		if p.entryRoot != nil {
			_ = p.entryRoot.Close()
			p.entryRoot = nil
		}
		return nil
	}
	if err != nil {
		return err
	}
	var entryRoot *os.Root
	if info.Mode()&os.ModeSymlink == 0 && info.IsDir() {
		entryRoot, err = openRootedSubroot(root, p.leaf)
		if err != nil {
			return err
		}
	}
	if p.entryRoot != nil {
		_ = p.entryRoot.Close()
	}
	p.entryRoot = entryRoot
	p.entry = info
	p.exists = true
	return nil
}

func (p *GuardedPath) verifyEntry() error {
	root, err := p.openedRoot()
	if err != nil {
		return err
	}
	current, err := root.Lstat(p.leaf)
	if !p.exists {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		return fmt.Errorf("guarded destination %q appeared after capability creation", p.leaf)
	}
	if err != nil {
		return err
	}
	if !sameGuardedEntryIdentity(p.entry, current) {
		return fmt.Errorf("guarded destination %q changed identity", p.leaf)
	}
	if p.entryRoot != nil {
		retained, err := p.entryRoot.Stat(".")
		if err != nil {
			return err
		}
		if !os.SameFile(p.entry, retained) {
			return fmt.Errorf("guarded destination directory %q changed identity", p.leaf)
		}
	}
	return nil
}

func (p *GuardedPath) setEntryAbsent() {
	if p.entryRoot != nil {
		_ = p.entryRoot.Close()
		p.entryRoot = nil
	}
	p.entry = nil
	p.exists = false
}

// moveExpectedAside atomically detaches the final entry from its public name,
// then verifies that the moved inode is the one retained by this capability.
// A swapped entry is quarantined under the private aside name and rejected.
// It is deliberately not renamed back: a pathname-based restore could race
// with and overwrite another concurrent replacement of the public name.
func (p *GuardedPath) moveExpectedAside(prefix string) (string, bool, error) {
	if len(p.pendingParents) != 0 {
		return "", false, nil
	}
	if err := p.verifyEntry(); err != nil {
		return "", false, err
	}
	if !p.exists {
		return "", false, nil
	}
	aside, err := unusedRootedName(p.parent, prefix)
	if err != nil {
		return "", false, err
	}
	if err := p.parent.Rename(p.leaf, aside); err != nil {
		return "", false, err
	}
	moved, err := p.parent.Lstat(aside)
	if err != nil || !sameGuardedEntryIdentity(p.entry, moved) {
		if err != nil {
			return "", false, fmt.Errorf("inspect quarantined guarded destination %q: %w", aside, err)
		}
		return "", false, fmt.Errorf("guarded destination %q changed before mutation; unexpected entry quarantined as %q", p.leaf, aside)
	}
	p.setEntryAbsent()
	return aside, true, nil
}

func (p *GuardedPath) restoreAside(aside string) error {
	if _, err := p.parent.Lstat(p.leaf); err == nil {
		return fmt.Errorf("cannot restore guarded destination %q because its name is occupied", p.leaf)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := p.parent.Rename(aside, p.leaf); err != nil {
		return err
	}
	return p.captureEntry()
}

func (p *GuardedPath) ensureParent(ctx context.Context) error {
	if _, err := p.openedRoot(); err != nil {
		return err
	}
	for _, part := range p.pendingParents {
		if err := ctx.Err(); err != nil {
			return err
		}
		parent := p.parent
		if err := parent.Mkdir(part, 0o755); err != nil {
			return fmt.Errorf("create guarded destination parent %q: %w", part, err)
		}
		child, err := openRootedSubroot(parent, part)
		if err != nil {
			_ = parent.Remove(part)
			return err
		}
		identity, err := child.Stat(".")
		if err != nil {
			_ = child.Close()
			_ = parent.Remove(part)
			return err
		}
		p.createdParents = append(p.createdParents, guardedCreatedDirectory{parent: parent, name: part, identity: identity})
		p.roots = append(p.roots, child)
		p.parent = child
	}
	p.pendingParents = nil
	return nil
}

func (p *GuardedPath) removeCreatedParents() error {
	var result error
	for index := len(p.createdParents) - 1; index >= 0; index-- {
		entry := p.createdParents[index]
		current, err := entry.parent.Lstat(entry.name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			if result == nil {
				result = err
			}
			break
		}
		if !os.SameFile(entry.identity, current) {
			if result == nil {
				result = fmt.Errorf("created destination parent %q changed identity", entry.name)
			}
			break
		}
		if err := entry.parent.Remove(entry.name); err != nil && !os.IsNotExist(err) {
			if result == nil {
				result = err
			}
			break
		}
	}
	return result
}

// Lstat returns the destination entry without following its final symlink.
func (p *GuardedPath) Lstat() (fs.FileInfo, bool, error) {
	if len(p.pendingParents) != 0 {
		return nil, false, nil
	}
	if err := p.verifyEntry(); err != nil {
		return nil, false, err
	}
	if !p.exists {
		return nil, false, nil
	}
	return p.entry, true, nil
}

// ReadRegularFile opens a regular destination exactly once, verifies that the
// opened handle is the entry observed by the no-follow stat, and reads it.
func (p *GuardedPath) ReadRegularFile() ([]byte, fs.FileInfo, bool, error) {
	if len(p.pendingParents) != 0 {
		return nil, nil, false, nil
	}
	root, err := p.openedRoot()
	if err != nil {
		return nil, nil, false, err
	}
	if err := p.verifyEntry(); err != nil {
		return nil, nil, false, err
	}
	if !p.exists {
		return nil, nil, false, nil
	}
	return readRootedRegularFileExpected(context.Background(), root, p.leaf, p.entry)
}

// WriteFile atomically replaces a regular destination file.
func (p *GuardedPath) WriteFile(data []byte, mode fs.FileMode) error {
	if err := p.ensureParent(context.Background()); err != nil {
		return err
	}
	root := p.parent
	if err := p.verifyEntry(); err != nil {
		return err
	}
	tempName, temp, err := createRootedTemp(root, ".")
	if err != nil {
		return err
	}
	defer func() { _ = root.Remove(tempName) }()
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Chmod(mode.Perm()); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	aside, moved, err := p.moveExpectedAside(".angee-previous-")
	if err != nil {
		return err
	}
	if err := root.Rename(tempName, p.leaf); err != nil {
		if moved {
			err = errors.Join(err, p.restoreAside(aside))
		}
		return err
	}
	if err := p.captureEntry(); err != nil {
		_ = root.RemoveAll(p.leaf)
		if moved {
			err = errors.Join(err, p.restoreAside(aside))
		}
		return err
	}
	if moved {
		// Installing the new entry is the mutation commit point. Failure to
		// remove the detached old entry must not make callers roll back other
		// files while this new entry remains live.
		p.cleanupEntries = append(p.cleanupEntries, aside)
		if err := root.RemoveAll(aside); err == nil || os.IsNotExist(err) {
			p.cleanupEntries = p.cleanupEntries[:len(p.cleanupEntries)-1]
		}
	}
	return nil
}

// MkdirAll creates the destination directory within its rooted capability.
func (p *GuardedPath) MkdirAll(mode fs.FileMode) error {
	if err := p.ensureParent(context.Background()); err != nil {
		return err
	}
	if err := p.verifyEntry(); err != nil {
		return err
	}
	if p.exists {
		if p.entry.Mode()&os.ModeSymlink != 0 || !p.entry.IsDir() {
			return fmt.Errorf("guarded destination %q is not a real directory", p.leaf)
		}
		return nil
	}
	if err := p.parent.Mkdir(p.leaf, mode.Perm()); err != nil {
		return err
	}
	return p.captureEntry()
}

// RemoveAll removes the destination without leaving its rooted capability.
func (p *GuardedPath) RemoveAll() error {
	if len(p.pendingParents) != 0 {
		return nil
	}
	if err := p.verifyEntry(); err != nil {
		return err
	}
	if !p.exists {
		return nil
	}
	aside, moved, err := p.moveExpectedAside(".angee-remove-")
	if err != nil || !moved {
		return err
	}
	if err := p.parent.RemoveAll(aside); err != nil {
		return errors.Join(err, p.restoreAside(aside))
	}
	return nil
}

// Remove removes an empty destination entry.
func (p *GuardedPath) Remove() error {
	if len(p.pendingParents) != 0 {
		return os.ErrNotExist
	}
	if err := p.verifyEntry(); err != nil {
		return err
	}
	if !p.exists {
		return os.ErrNotExist
	}
	aside, moved, err := p.moveExpectedAside(".angee-remove-")
	if err != nil {
		return err
	}
	if !moved {
		return os.ErrNotExist
	}
	if err := p.parent.Remove(aside); err != nil {
		return errors.Join(err, p.restoreAside(aside))
	}
	return nil
}

// RemoveMissingParents removes, deepest first, parent directories that were
// absent when this capability was opened. Non-empty parents are preserved.
func (p *GuardedPath) RemoveMissingParents() error {
	return p.removeCreatedParents()
}

// IsEmptyDirectory reports whether the guarded destination is an empty real directory.
func (p *GuardedPath) IsEmptyDirectory() (bool, error) {
	if len(p.pendingParents) != 0 || !p.exists {
		return false, os.ErrNotExist
	}
	if err := p.verifyEntry(); err != nil {
		return false, err
	}
	if p.entryRoot == nil {
		return false, fmt.Errorf("guarded destination %q is not a real directory", p.leaf)
	}
	dir, _, err := openRootedDirectory(p.entryRoot, ".")
	if err != nil {
		return false, err
	}
	defer func() { _ = dir.Close() }()
	entries, err := dir.ReadDir(1)
	if err != nil && err != io.EOF {
		return false, err
	}
	return len(entries) == 0, nil
}

// HasRealDirectory reports whether rel names a real directory beneath this
// guarded destination directory without re-traversing its public pathname.
func (p *GuardedPath) HasRealDirectory(rel string) (bool, error) {
	if err := validateRelativePath(rel); err != nil {
		return false, err
	}
	if len(p.pendingParents) != 0 || !p.exists {
		return false, nil
	}
	if err := p.verifyEntry(); err != nil {
		return false, err
	}
	if p.entryRoot == nil {
		return false, fmt.Errorf("guarded destination %q is not a real directory", p.leaf)
	}
	info, err := p.entryRoot.Lstat(filepath.FromSlash(rel))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return info.Mode()&os.ModeSymlink == 0 && info.IsDir(), nil
}

// RetainRealSubdirectory derives a TrustedRoot for a real directory below this
// guarded destination without reopening the destination's public pathname.
// publicPath is recorded only for the later success-path identity check.
func (p *GuardedPath) RetainRealSubdirectory(rel, publicPath string) (*TrustedRoot, error) {
	if err := validateRelativePath(rel); err != nil {
		return nil, err
	}
	if len(p.pendingParents) != 0 || !p.exists {
		return nil, os.ErrNotExist
	}
	if err := p.verifyEntry(); err != nil {
		return nil, err
	}
	if p.entryRoot == nil {
		return nil, fmt.Errorf("guarded destination %q is not a real directory", p.leaf)
	}
	current, err := p.entryRoot.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	for _, part := range splitPathParts(filepath.FromSlash(rel)) {
		info, err := current.Lstat(part)
		if err != nil {
			_ = current.Close()
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			_ = current.Close()
			return nil, fmt.Errorf("guarded subdirectory %q is not a real directory", rel)
		}
		child, err := openRootedSubroot(current, part)
		_ = current.Close()
		if err != nil {
			return nil, err
		}
		current = child
	}
	identity, err := current.Stat(".")
	if err != nil {
		_ = current.Close()
		return nil, err
	}
	abs, err := filepath.Abs(publicPath)
	if err != nil {
		_ = current.Close()
		return nil, err
	}
	return &TrustedRoot{root: current, path: filepath.Clean(abs), identity: identity}, nil
}

// Symlink creates the guarded destination symlink without leaving its root.
func (p *GuardedPath) Symlink(target string) error {
	if err := p.ensureParent(context.Background()); err != nil {
		return err
	}
	if err := p.verifyEntry(); err != nil {
		return err
	}
	if p.exists {
		return fmt.Errorf("guarded destination %q already exists", p.leaf)
	}
	if err := p.parent.Symlink(target, p.leaf); err != nil {
		return err
	}
	return p.captureEntry()
}

// ReplaceFrom replaces the guarded destination with a filesystem tree copied
// from source. Destination mutation remains confined to the rooted capability.
func (p *GuardedPath) ReplaceFrom(ctx context.Context, source string) (result error) {
	createdBefore := len(p.createdParents)
	if err := p.ensureParent(ctx); err != nil {
		_ = p.removeCreatedParents()
		return err
	}
	cleanupParents := func() {
		if len(p.createdParents) > createdBefore {
			_ = p.removeCreatedParents()
		}
	}
	if err := p.verifyEntry(); err != nil {
		cleanupParents()
		return err
	}
	tempName, err := unusedRootedName(p.parent, ".angee-stage-")
	if err != nil {
		cleanupParents()
		return err
	}
	defer func() { _ = p.parent.RemoveAll(tempName) }()
	cleanupFailure := func() {
		_ = p.parent.RemoveAll(tempName)
		cleanupParents()
	}
	if err := copyEntryToRoot(ctx, source, p.parent, tempName); err != nil {
		cleanupFailure()
		return err
	}
	if err := ctx.Err(); err != nil {
		cleanupFailure()
		return err
	}
	aside, moved, err := p.moveExpectedAside(".angee-previous-")
	if err != nil {
		cleanupFailure()
		return err
	}
	if err := p.parent.Rename(tempName, p.leaf); err != nil {
		if moved {
			err = errors.Join(err, p.restoreAside(aside))
		}
		cleanupFailure()
		return err
	}
	if err := p.captureEntry(); err != nil {
		_ = p.parent.RemoveAll(p.leaf)
		if moved {
			err = errors.Join(err, p.restoreAside(aside))
		}
		cleanupFailure()
		return err
	}
	if moved {
		p.cleanupEntries = append(p.cleanupEntries, aside)
		if err := p.parent.RemoveAll(aside); err == nil || os.IsNotExist(err) {
			p.cleanupEntries = p.cleanupEntries[:len(p.cleanupEntries)-1]
		}
	}
	return nil
}

// SnapshotDirectory copies a guarded real directory to a private temporary
// directory and returns the snapshot plus an idempotent cleanup function.
// Symlinks are rejected so the snapshot cannot gain new escape semantics when
// moved away from its original parent tree.
func (p *GuardedPath) SnapshotDirectory(ctx context.Context) (string, func() error, error) {
	if len(p.pendingParents) != 0 || !p.exists {
		return "", nil, os.ErrNotExist
	}
	if err := p.verifyEntry(); err != nil {
		return "", nil, err
	}
	if p.entryRoot == nil {
		return "", nil, fmt.Errorf("guarded destination %q is not a real directory", p.leaf)
	}
	tempRoot, err := os.MkdirTemp("", "angee-template-snapshot-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() error { return os.RemoveAll(tempRoot) }
	destination := filepath.Join(tempRoot, "template")
	if err := copySnapshotEntryAt(ctx, p.entryRoot, ".", destination); err != nil {
		_ = cleanup()
		return "", nil, err
	}
	return destination, cleanup, nil
}

// VerifyPathIdentity confirms that an unavoidable pathname-based integration
// still resolves to the exact entry represented by this guarded capability.
func (p *GuardedPath) VerifyPathIdentity(path string) error {
	if err := p.VerifyPathEntryIdentity(path); err != nil {
		return err
	}
	if p.entry.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("path %q identifies a symlink, not a real guarded destination", path)
	}
	return nil
}

// VerifyPathEntryIdentity confirms that a public path still names the exact
// final entry retained by this capability. Unlike VerifyPathIdentity it also
// supports a final symlink, which is needed to validate read-only local Source
// declarations without following a mutable pathname during commit.
func (p *GuardedPath) VerifyPathEntryIdentity(path string) error {
	if len(p.pendingParents) != 0 || !p.exists {
		return fmt.Errorf("guarded destination has no installed identity")
	}
	if err := p.verifyEntry(); err != nil {
		return err
	}
	actual, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !sameGuardedEntryIdentity(p.entry, actual) {
		return fmt.Errorf("path %q no longer identifies its guarded destination", path)
	}
	return nil
}

func sameGuardedEntryIdentity(expected, actual fs.FileInfo) bool {
	return expected.Mode().Type() == actual.Mode().Type() && os.SameFile(expected, actual)
}

// VerifyPathAbsent confirms both the retained destination capability and its
// unavoidable public pathname remain absent after a guarded deletion.
func (p *GuardedPath) VerifyPathAbsent(path string) error {
	if len(p.pendingParents) != 0 {
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			return nil
		} else if err != nil {
			return err
		}
		return fmt.Errorf("path %q appeared after guarded deletion", path)
	}
	if err := p.verifyEntry(); err != nil {
		return err
	}
	if p.exists {
		return fmt.Errorf("guarded destination still has an installed identity")
	}
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	return fmt.Errorf("path %q appeared after guarded deletion", path)
}

// VerifyParentPathIdentity confirms path still names the retained parent
// directory used by this guarded destination.
func (p *GuardedPath) VerifyParentPathIdentity(path string) error {
	root, err := p.openedRoot()
	if err != nil {
		return err
	}
	retained, err := root.Stat(".")
	if err != nil {
		return err
	}
	actual, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if actual.Mode()&os.ModeSymlink != 0 || !actual.IsDir() || !os.SameFile(retained, actual) {
		return fmt.Errorf("path %q no longer identifies the guarded destination parent", path)
	}
	return nil
}

// VerifyParentTrustedRoot compares two retained capabilities directly without
// consulting a public pathname, preventing rename ABA between acquisitions.
func (p *GuardedPath) VerifyParentTrustedRoot(trusted *TrustedRoot) error {
	root, err := p.openedRoot()
	if err != nil {
		return err
	}
	if trusted == nil || trusted.root == nil {
		return fmt.Errorf("trusted root capability is not available")
	}
	parentInfo, err := root.Stat(".")
	if err != nil {
		return err
	}
	trustedInfo, err := trusted.root.Stat(".")
	if err != nil {
		return err
	}
	if !os.SameFile(parentInfo, trustedInfo) {
		return fmt.Errorf("guarded destination parent and prepared root are different directories")
	}
	return nil
}

func readRootedRegularFileContext(ctx context.Context, root *os.Root, rel string) ([]byte, fs.FileInfo, bool, error) {
	return readRootedRegularFileExpected(ctx, root, rel, nil)
}

func readRootedRegularFileExpected(ctx context.Context, root *os.Root, rel string, expected fs.FileInfo) ([]byte, fs.FileInfo, bool, error) {
	before, err := root.Lstat(rel)
	if os.IsNotExist(err) {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, err
	}
	if !before.Mode().IsRegular() {
		return nil, before, true, fmt.Errorf("destination %q is not a regular file", rel)
	}
	if expected != nil && !os.SameFile(expected, before) {
		return nil, before, true, fmt.Errorf("destination %q changed before it was read", rel)
	}
	file, err := root.Open(rel)
	if err != nil {
		return nil, nil, true, err
	}
	defer func() { _ = file.Close() }()
	after, err := file.Stat()
	if err != nil {
		return nil, nil, true, err
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return nil, nil, true, fmt.Errorf("destination %q changed while it was being opened", rel)
	}
	data, err := io.ReadAll(&contextReader{ctx: ctx, reader: file})
	if err != nil {
		return nil, nil, true, err
	}
	return data, after, true, nil
}

func readRootedSymlink(root *os.Root, rel string) (string, error) {
	return readRootedSymlinkExpected(root, rel, nil)
}

func readRootedSymlinkExpected(root *os.Root, rel string, expected fs.FileInfo) (string, error) {
	before, err := root.Lstat(rel)
	if err != nil {
		return "", err
	}
	if before.Mode()&os.ModeSymlink == 0 {
		return "", fmt.Errorf("destination %q is not a symlink", rel)
	}
	if expected != nil && !os.SameFile(expected, before) {
		return "", fmt.Errorf("destination symlink %q changed before it was read", rel)
	}
	target, err := root.Readlink(rel)
	if err != nil {
		return "", err
	}
	after, err := root.Lstat(rel)
	if err != nil {
		return "", err
	}
	if after.Mode()&os.ModeSymlink == 0 || !os.SameFile(before, after) {
		return "", fmt.Errorf("destination symlink %q changed while it was being read", rel)
	}
	return target, nil
}

func openRootedDirectory(root *os.Root, rel string) (*os.File, fs.FileInfo, error) {
	before, err := root.Lstat(rel)
	if err != nil {
		return nil, nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return nil, nil, fmt.Errorf("destination %q is not a real directory", rel)
	}
	dir, err := root.Open(rel)
	if err != nil {
		return nil, nil, err
	}
	after, err := dir.Stat()
	if err != nil {
		_ = dir.Close()
		return nil, nil, err
	}
	if !after.IsDir() || !os.SameFile(before, after) {
		_ = dir.Close()
		return nil, nil, fmt.Errorf("destination directory %q changed while it was being opened", rel)
	}
	return dir, after, nil
}

func openRootedSubroot(root *os.Root, rel string) (*os.Root, error) {
	before, err := root.Lstat(rel)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return nil, fmt.Errorf("destination %q is not a real directory", rel)
	}
	subroot, err := root.OpenRoot(rel)
	if err != nil {
		return nil, err
	}
	after, err := subroot.Stat(".")
	if err != nil {
		_ = subroot.Close()
		return nil, err
	}
	if !after.IsDir() || !os.SameFile(before, after) {
		_ = subroot.Close()
		return nil, fmt.Errorf("destination directory %q changed while it was being opened", rel)
	}
	return subroot, nil
}

func chmodRootedDirectory(root *os.Root, rel string, mode fs.FileMode) error {
	dir, _, err := openRootedDirectory(root, rel)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Chmod(mode.Perm())
}

func chmodVerifiedDirectory(path string, mode fs.FileMode) error {
	before, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return fmt.Errorf("backup destination %q is not a real directory", path)
	}
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	after, err := dir.Stat()
	if err != nil {
		return err
	}
	if !after.IsDir() || !os.SameFile(before, after) {
		return fmt.Errorf("backup destination %q changed while it was being opened", path)
	}
	return dir.Chmod(mode.Perm())
}

func writeFileExact(path string, data []byte, mode fs.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Chmod(mode.Perm()); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func copyGuardedEntry(ctx context.Context, source *GuardedPath, dest string) error {
	if len(source.pendingParents) != 0 || !source.exists {
		return os.ErrNotExist
	}
	if err := source.verifyEntry(); err != nil {
		return err
	}
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	if source.entryRoot != nil {
		return copyGuardedEntryAt(ctx, source.entryRoot, ".", dest)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	switch {
	case source.entry.Mode()&os.ModeSymlink != 0:
		target, err := readRootedSymlinkExpected(source.parent, source.leaf, source.entry)
		if err != nil {
			return err
		}
		return os.Symlink(target, dest)
	case source.entry.Mode().IsRegular():
		data, verified, _, err := readRootedRegularFileExpected(ctx, source.parent, source.leaf, source.entry)
		if err != nil {
			return err
		}
		return writeFileExact(dest, data, verified.Mode().Perm())
	default:
		return fmt.Errorf("unsupported filesystem entry mode %s", source.entry.Mode())
	}
}

func copySnapshotEntryAt(ctx context.Context, root *os.Root, rel, dest string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := root.Lstat(rel)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("template snapshot contains unsupported symlink %q", rel)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	if info.IsDir() {
		if err := os.Mkdir(dest, 0o700); err != nil && !os.IsExist(err) {
			return err
		}
		subroot, err := openRootedSubroot(root, rel)
		if err != nil {
			return err
		}
		defer func() { _ = subroot.Close() }()
		verified, err := subroot.Stat(".")
		if err != nil {
			return err
		}
		dir, _, err := openRootedDirectory(subroot, ".")
		if err != nil {
			return err
		}
		entries, readErr := dir.ReadDir(-1)
		closeErr := dir.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		for _, entry := range entries {
			if err := copySnapshotEntryAt(ctx, subroot, entry.Name(), filepath.Join(dest, entry.Name())); err != nil {
				return err
			}
		}
		return chmodVerifiedDirectory(dest, verified.Mode().Perm())
	}
	if info.Mode().IsRegular() {
		data, verified, _, err := readRootedRegularFileContext(ctx, root, rel)
		if err != nil {
			return err
		}
		return writeFileExact(dest, data, verified.Mode().Perm())
	}
	return fmt.Errorf("template snapshot contains unsupported entry %q with mode %s", rel, info.Mode())
}

func copyGuardedEntryAt(ctx context.Context, root *os.Root, rel, dest string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := root.Lstat(rel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := readRootedSymlink(root, rel)
		if err != nil {
			return err
		}
		return os.Symlink(target, dest)
	case info.IsDir():
		if err := os.Mkdir(dest, 0o700); err != nil && !os.IsExist(err) {
			return err
		}
		subroot, err := openRootedSubroot(root, rel)
		if err != nil {
			return err
		}
		defer func() { _ = subroot.Close() }()
		verified, err := subroot.Stat(".")
		if err != nil {
			return err
		}
		dir, _, err := openRootedDirectory(subroot, ".")
		if err != nil {
			return err
		}
		entries, readErr := dir.ReadDir(-1)
		closeErr := dir.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		for _, entry := range entries {
			if err := copyGuardedEntryAt(ctx, subroot, entry.Name(), filepath.Join(dest, entry.Name())); err != nil {
				return err
			}
		}
		return chmodVerifiedDirectory(dest, verified.Mode().Perm())
	case info.Mode().IsRegular():
		data, verified, _, err := readRootedRegularFileContext(ctx, root, rel)
		if err != nil {
			return err
		}
		return writeFileExact(dest, data, verified.Mode().Perm())
	default:
		return fmt.Errorf("unsupported filesystem entry mode %s", info.Mode())
	}
}

func copyEntryToGuarded(ctx context.Context, source string, dest *GuardedPath) error {
	return dest.ReplaceFrom(ctx, source)
}

func copyEntryToRoot(ctx context.Context, source string, root *os.Root, rel string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if err := root.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		return err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(source)
		if err != nil {
			return err
		}
		return root.Symlink(target, rel)
	case info.IsDir():
		if err := root.Mkdir(rel, 0o700); err != nil && !os.IsExist(err) {
			return err
		}
		entries, err := os.ReadDir(source)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := copyEntryToRoot(ctx, filepath.Join(source, entry.Name()), root, filepath.Join(rel, entry.Name())); err != nil {
				return err
			}
		}
		return chmodRootedDirectory(root, rel, info.Mode().Perm())
	case info.Mode().IsRegular():
		input, err := os.Open(source)
		if err != nil {
			return err
		}
		defer func() { _ = input.Close() }()
		tempName, temp, err := createRootedTemp(root, filepath.Dir(rel))
		if err != nil {
			return err
		}
		defer func() { _ = root.Remove(tempName) }()
		if _, err := io.Copy(temp, &contextReader{ctx: ctx, reader: input}); err != nil {
			_ = temp.Close()
			return err
		}
		if err := temp.Chmod(info.Mode().Perm()); err != nil {
			_ = temp.Close()
			return err
		}
		if err := temp.Close(); err != nil {
			return err
		}
		return root.Rename(tempName, rel)
	default:
		return fmt.Errorf("unsupported filesystem entry mode %s", info.Mode())
	}
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

func unusedRootedName(root *os.Root, prefix string) (string, error) {
	for attempt := 0; attempt < 100; attempt++ {
		var random [8]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", err
		}
		name := fmt.Sprintf("%s%x", prefix, random[:])
		if _, err := root.Lstat(name); os.IsNotExist(err) {
			return name, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("could not allocate rooted staging name")
}

func normalizeProtectedPaths(paths []string) ([]string, error) {
	set := map[string]struct{}{}
	for _, path := range paths {
		rel := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
		if rel == "." {
			continue
		}
		if err := validateRelativePath(rel); err != nil {
			return nil, fmt.Errorf("protected path %q: %w", path, err)
		}
		set[rel] = struct{}{}
	}
	result := make([]string, 0, len(set))
	for path := range set {
		result = append(result, path)
	}
	sort.Strings(result)
	return result, nil
}

func unionSortedPaths(groups ...[]string) []string {
	set := map[string]struct{}{}
	for _, group := range groups {
		for _, path := range group {
			set[filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))] = struct{}{}
		}
	}
	result := make([]string, 0, len(set))
	for path := range set {
		result = append(result, path)
	}
	sort.Strings(result)
	return result
}

func mapKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

func containsPath(paths []string, want string) bool {
	want = filepath.ToSlash(filepath.Clean(filepath.FromSlash(want)))
	index := sort.SearchStrings(paths, want)
	return index < len(paths) && paths[index] == want
}
