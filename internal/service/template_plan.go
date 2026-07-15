package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ang-ee/angee-operator/internal/copierx"
	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/substitute"
)

type stackDocument struct {
	Path     string
	Template string
}

type stackPlanOptions struct {
	InputsAlreadyResolved bool
}

type workspaceRenderPlan struct {
	Plan       copierx.RenderPlan
	Documents  []stackDocument
	Chain      []string
	ChainRoot  string
	Persistent map[string]manifest.PersistPath
	cleanup    []func() error
}

func (p *workspaceRenderPlan) Close() error {
	var result error
	for index := len(p.cleanup) - 1; index >= 0; index-- {
		if err := p.cleanup[index](); err != nil && result == nil {
			result = err
		}
	}
	p.cleanup = nil
	return result
}

func (p *Platform) buildWorkspaceRenderPlan(ctx context.Context, workspacePath, templatePath, templateRef string, metadata copierx.Metadata, inputs map[string]string, workspaceName string, allocations map[string]int, sources map[string]manifest.WorkspaceSource, declaredSources map[string]manifest.Source, statePath string) (workspaceRenderPlan, error) {
	var templateCleanups []func() error
	success := false
	defer func() {
		if success {
			return
		}
		for index := len(templateCleanups) - 1; index >= 0; index-- {
			_ = templateCleanups[index]()
		}
	}()
	renderInputs := copierx.Inputs{}
	for key, value := range inputs {
		renderInputs[key] = value
	}
	renderInputs["workspace_name"] = workspaceName
	for pool, port := range allocations {
		renderInputs["alloc_"+pool] = fmt.Sprint(port)
	}
	mergedOuter, err := copierx.TemplateInputs(templatePath, renderInputs)
	if err != nil {
		return workspaceRenderPlan{}, err
	}
	layers := []copierx.RenderLayer{{Name: "workspace", Template: templatePath, Inputs: mergedOuter}}
	resolvedChain := []string{templateRef}
	documents := map[string]stackDocument{}
	chainRoot := ""
	subCtx := substitute.Context{
		Inputs:        inputs,
		Name:          workspaceName,
		Alloc:         allocations,
		WorkspacePath: workspacePath,
	}
	if metadata.ChainRoot != "" {
		chainRoot, err = substitute.Resolve(metadata.ChainRoot, subCtx)
		if err != nil {
			return workspaceRenderPlan{}, err
		}
	}
	allowedSymlinkParents := make(map[string]*copierx.TrustedRoot)
	for _, source := range sources {
		declared, ok := declaredSources[source.Source]
		if !ok || declared.Kind != "local" || source.Subpath == "" || source.Subpath == "." {
			continue
		}
		target, err := filepath.EvalSymlinks(p.sourcePath(source.Source, declared))
		if err != nil {
			return workspaceRenderPlan{}, fmt.Errorf("resolve local workspace source %q: %w", source.Source, err)
		}
		target, err = filepath.Abs(target)
		if err != nil {
			return workspaceRenderPlan{}, fmt.Errorf("resolve local workspace source %q: %w", source.Source, err)
		}
		trusted, err := copierx.OpenTrustedRoot(filepath.Clean(target))
		if err != nil {
			return workspaceRenderPlan{}, fmt.Errorf("open local workspace source %q: %w", source.Source, err)
		}
		templateCleanups = append(templateCleanups, trusted.Close)
		allowedSymlinkParents[filepath.ToSlash(filepath.Clean(source.Subpath))] = trusted
	}
	for index, entry := range metadata.Chain {
		if entry.Template == "" {
			continue
		}
		templateEntry, err := substitute.Resolve(entry.Template, subCtx)
		if err != nil {
			return workspaceRenderPlan{}, err
		}
		path, ref, cleanup, err := p.resolveWorkspaceChainTemplate(ctx, workspacePath, templateEntry, allowedSymlinkParents)
		if err != nil {
			return workspaceRenderPlan{}, err
		}
		if cleanup != nil {
			templateCleanups = append(templateCleanups, cleanup)
		}
		chainInputs := copierx.Inputs{}
		for key, value := range inputs {
			chainInputs[key] = value
		}
		for key, value := range entry.Inputs {
			resolved, err := substitute.Resolve(value, subCtx)
			if err != nil {
				return workspaceRenderPlan{}, err
			}
			chainInputs[key] = resolved
		}
		destRoot := chainRoot
		if entry.Root != "" {
			destRoot, err = substitute.Resolve(entry.Root, subCtx)
			if err != nil {
				return workspaceRenderPlan{}, err
			}
		}
		if destRoot == "" {
			return workspaceRenderPlan{}, fmt.Errorf("chain entry %q requires a root", entry.Template)
		}
		dest := filepath.Join(workspacePath, filepath.FromSlash(destRoot))
		merged, err := copierx.TemplateInputs(path, chainInputs)
		if err != nil {
			return workspaceRenderPlan{}, err
		}
		resolved, err := copierx.ResolvePathInputs(path, merged, dest, merged["ANGEE_ROOT"])
		if err != nil {
			return workspaceRenderPlan{}, err
		}
		layers = append(layers, copierx.RenderLayer{
			Name:          fmt.Sprintf("chain-%d", index),
			Template:      path,
			StateTemplate: ref,
			DestRoot:      filepath.ToSlash(filepath.Clean(destRoot)),
			Inputs:        resolved,
		})
		resolvedChain = append(resolvedChain, ref)
		if emitsStackManifest(path) {
			documentPath, err := stackDocumentPath(workspacePath, dest, merged)
			if err != nil {
				return workspaceRenderPlan{}, err
			}
			documents[documentPath] = stackDocument{Path: documentPath, Template: path}
		}
	}
	documentPaths := make([]string, 0, len(documents))
	for path := range documents {
		documentPaths = append(documentPaths, path)
	}
	sort.Strings(documentPaths)
	stackDocuments := make([]stackDocument, 0, len(documentPaths))
	for _, path := range documentPaths {
		stackDocuments = append(stackDocuments, documents[path])
	}
	protectedPaths := make([]string, 0, len(metadata.Persist))
	for _, name := range sortedKeys(metadata.Persist) {
		if subpath := metadata.Persist[name].Subpath; subpath != "" {
			protectedPaths = append(protectedPaths, filepath.ToSlash(filepath.Clean(filepath.FromSlash(subpath))))
		}
	}
	success = true
	return workspaceRenderPlan{
		Plan: copierx.RenderPlan{
			Target: workspacePath, TargetRoot: p.root, StateRoot: p.root, StatePath: statePath, Layers: layers, Documents: documentPaths, AllowedSymlinkParents: allowedSymlinkParents, ProtectedPaths: protectedPaths,
		},
		Documents:  stackDocuments,
		Chain:      resolvedChain,
		ChainRoot:  chainRoot,
		Persistent: metadata.Persist,
		cleanup:    templateCleanups,
	}, nil
}

func (p *Platform) buildStackRenderPlan(ctx context.Context, templatePath, target string, inputs copierx.Inputs, statePath string, options stackPlanOptions) (copierx.RenderPlan, []stackDocument, error) {
	layers, documents, err := p.buildStackChainLayers(ctx, templatePath, target, inputs, options)
	if err != nil {
		return copierx.RenderPlan{}, nil, err
	}
	resolved := inputs
	if !options.InputsAlreadyResolved {
		resolved, err = copierx.ResolvePathInputs(templatePath, inputs, target, inputs["ANGEE_ROOT"])
		if err != nil {
			return copierx.RenderPlan{}, nil, err
		}
	}
	layers = append(layers, copierx.RenderLayer{Name: "stack", Template: templatePath, Inputs: resolved})
	if emitsStackManifest(templatePath) {
		path, err := stackDocumentPath(target, target, inputs)
		if err != nil {
			return copierx.RenderPlan{}, nil, err
		}
		documents[path] = stackDocument{Path: path, Template: templatePath}
	}

	documentPaths := make([]string, 0, len(documents))
	stackDocuments := make([]stackDocument, 0, len(documents))
	for path := range documents {
		documentPaths = append(documentPaths, path)
	}
	sort.Strings(documentPaths)
	for _, path := range documentPaths {
		stackDocuments = append(stackDocuments, documents[path])
	}
	return copierx.RenderPlan{
		Target:    target,
		StatePath: statePath,
		Layers:    layers,
		Documents: documentPaths,
	}, stackDocuments, nil
}

func (p *Platform) buildStackChainLayers(ctx context.Context, stackTemplatePath, target string, stackInputs copierx.Inputs, options stackPlanOptions) ([]copierx.RenderLayer, map[string]stackDocument, error) {
	metadata, err := copierx.ReadMetadata(stackTemplatePath)
	if err != nil {
		return nil, nil, err
	}
	subCtx := substitute.Context{Inputs: stackInputs}
	chainRoot := metadata.ChainRoot
	if chainRoot != "" {
		if chainRoot, err = substitute.Resolve(chainRoot, subCtx); err != nil {
			return nil, nil, fmt.Errorf("resolve chain_root: %w", err)
		}
	}
	layers := make([]copierx.RenderLayer, 0, len(metadata.Chain))
	documents := map[string]stackDocument{}
	for index, entry := range metadata.Chain {
		if entry.Template == "" {
			continue
		}
		templateRef, err := substitute.Resolve(entry.Template, subCtx)
		if err != nil {
			return nil, nil, fmt.Errorf("chained template %q: %w", entry.Template, err)
		}
		chainTemplate, err := p.resolveChainTemplate(ctx, stackTemplatePath, templateRef)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve chained template %q: %w", entry.Template, err)
		}
		renderInputs := copierx.Inputs{}
		for key, value := range stackInputs {
			renderInputs[key] = value
		}
		for key, value := range entry.Inputs {
			resolved, err := substitute.Resolve(value, subCtx)
			if err != nil {
				return nil, nil, fmt.Errorf("chained template %q input %q: %w", entry.Template, key, err)
			}
			renderInputs[key] = resolved
		}
		destRoot := chainRoot
		if entry.Root != "" {
			if destRoot, err = substitute.Resolve(entry.Root, subCtx); err != nil {
				return nil, nil, fmt.Errorf("chained template %q root: %w", entry.Template, err)
			}
		}
		merged, err := copierx.TemplateInputs(chainTemplate, renderInputs)
		if err != nil {
			return nil, nil, fmt.Errorf("chained template %q inputs: %w", entry.Template, err)
		}
		dest := filepath.Join(target, filepath.FromSlash(destRoot))
		resolved := merged
		if !options.InputsAlreadyResolved {
			resolved, err = copierx.ResolvePathInputs(chainTemplate, merged, dest, merged["ANGEE_ROOT"])
			if err != nil {
				return nil, nil, fmt.Errorf("chained template %q inputs: %w", entry.Template, err)
			}
		}
		layers = append(layers, copierx.RenderLayer{
			Name:     fmt.Sprintf("chain-%d", index),
			Template: chainTemplate,
			DestRoot: filepath.ToSlash(filepath.Clean(destRoot)),
			Inputs:   resolved,
		})
		if emitsStackManifest(chainTemplate) {
			path, err := stackDocumentPath(target, dest, merged)
			if err != nil {
				return nil, nil, fmt.Errorf("chained template %q manifest: %w", entry.Template, err)
			}
			documents[path] = stackDocument{Path: path, Template: chainTemplate}
		}
	}
	return layers, documents, nil
}

func stackDocumentPath(planTarget, layerTarget string, inputs copierx.Inputs) (string, error) {
	path := manifest.Path(expectedStackRoot(layerTarget, inputs))
	rel, err := filepath.Rel(planTarget, path)
	if err != nil {
		return "", err
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || len(rel) >= 3 && rel[:3] == "../" {
		return "", fmt.Errorf("rendered manifest %q escapes target %q", path, planTarget)
	}
	return rel, nil
}

func emitsStackManifest(templatePath string) bool {
	configRoot := filepath.Join(templatePath, "template")
	found := false
	_ = filepath.WalkDir(configRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || found || entry.IsDir() {
			return nil
		}
		name := entry.Name()
		if name == "angee.yaml" || name == "angee.yaml.jinja" {
			found = true
		}
		return nil
	})
	if found {
		return true
	}
	// Templates may set a non-default _subdirectory. A complete walk is cheap
	// relative to rendering and keeps document detection independent of it.
	_ = filepath.WalkDir(templatePath, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || found || entry.IsDir() {
			return nil
		}
		name := entry.Name()
		if name == "angee.yaml" || name == "angee.yaml.jinja" {
			found = true
		}
		return nil
	})
	return found
}

func renderPlanStatePath(root, kind, name string) string {
	if name == "" {
		return filepath.Join(root, "run", "template-state", kind+".json")
	}
	return filepath.Join(root, "run", "template-state", kind, name+".json")
}

type parentStackTransaction struct {
	guard      *copierx.GuardedPath
	before     []byte
	mode       fs.FileMode
	existed    bool
	written    bool
	rolledBack bool
}

func openParentStackTransaction(root string, allowMissing bool) (*parentStackTransaction, *manifest.Stack, error) {
	guard, err := copierx.OpenGuardedPath("", root, "angee.yaml", nil)
	if err != nil {
		return nil, nil, err
	}
	data, info, exists, err := guard.ReadRegularFile()
	if err != nil {
		_ = guard.Close()
		return nil, nil, err
	}
	tx := &parentStackTransaction{guard: guard, before: append([]byte(nil), data...), existed: exists}
	if exists {
		tx.mode = info.Mode()
		stack, err := decodeStackDocument(data)
		if err != nil {
			_ = guard.Close()
			return nil, nil, err
		}
		return tx, stack, nil
	}
	if !allowMissing {
		_ = guard.Close()
		return nil, nil, fmt.Errorf("stack manifest %q does not exist", manifest.Path(root))
	}
	return tx, nil, nil
}

func (t *parentStackTransaction) Save(stack *manifest.Stack) error {
	if t == nil || t.guard == nil {
		return fmt.Errorf("parent stack transaction is closed")
	}
	current, info, exists, err := t.guard.ReadRegularFile()
	if err != nil {
		return err
	}
	if exists != t.existed || exists && (!bytes.Equal(current, t.before) || info.Mode().Perm() != t.mode.Perm()) {
		return fmt.Errorf("parent stack manifest changed during template reconciliation")
	}
	data, err := manifest.Marshal(stack)
	if err != nil {
		return err
	}
	if err := t.guard.WriteFile(data, 0o644); err != nil {
		return err
	}
	t.written = true
	return nil
}

func (t *parentStackTransaction) Rollback() error {
	if t == nil || t.guard == nil || !t.written || t.rolledBack {
		return nil
	}
	t.rolledBack = true
	if t.existed {
		return t.guard.WriteFile(t.before, t.mode.Perm())
	}
	if err := t.guard.RemoveAll(); err != nil {
		return err
	}
	return t.guard.RemoveMissingParents()
}

func (t *parentStackTransaction) VerifyRootPath(root string) error {
	if t == nil || t.guard == nil {
		return fmt.Errorf("parent stack transaction is closed")
	}
	return t.guard.VerifyParentPathIdentity(root)
}

func (t *parentStackTransaction) VerifyPreparedRoot(root string, prepared *copierx.PreparedReconcile) error {
	if t == nil || t.guard == nil {
		return fmt.Errorf("parent stack transaction is closed")
	}
	return prepared.VerifyRootCapability(root, t.guard)
}

func (t *parentStackTransaction) Close() error {
	if t == nil || t.guard == nil {
		return nil
	}
	guard := t.guard
	t.guard = nil
	return guard.Close()
}

type guardedPathOpener func(string) (*copierx.GuardedPath, error)

func joinRollbackErrors(primary error, rollbacks ...func() error) error {
	errs := []error{primary}
	for _, rollback := range rollbacks {
		if rollback != nil {
			errs = append(errs, rollback())
		}
	}
	return errors.Join(errs...)
}

func targetPathOpener(targetRoot, target string, allowedSymlinkParents map[string]*copierx.TrustedRoot) guardedPathOpener {
	return func(rel string) (*copierx.GuardedPath, error) {
		return copierx.OpenGuardedPath(targetRoot, target, rel, allowedSymlinkParents)
	}
}

func preparedAbsolutePathOpener(prepared *copierx.PreparedReconcile, target string) func(string) (*copierx.GuardedPath, error) {
	return func(path string) (*copierx.GuardedPath, error) {
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		rel, err := filepath.Rel(target, abs)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return prepared.OpenTargetPath(filepath.ToSlash(rel))
		}
		return openAbsoluteGuardedPath(abs)
	}
}

func readStackDocumentExpectation(openPath guardedPathOpener, rel string) (*manifest.Stack, renderedDocumentExpectation, error) {
	path, err := openPath(rel)
	if err != nil {
		return nil, renderedDocumentExpectation{}, err
	}
	defer func() { _ = path.Close() }()
	data, info, exists, err := path.ReadRegularFile()
	expectation := renderedDocumentExpectation{Data: data, Info: info, Exists: exists}
	if err != nil || !exists {
		return nil, expectation, err
	}
	stack, err := decodeStackDocument(data)
	if err != nil {
		return nil, expectation, err
	}
	return stack, expectation, nil
}

func readStackDocument(openPath guardedPathOpener, rel string) (*manifest.Stack, []byte, bool, error) {
	stack, expectation, err := readStackDocumentExpectation(openPath, rel)
	return stack, expectation.Data, expectation.Exists, err
}

func readGuardedStackDocument(targetRoot, target, rel string, allowedSymlinkParents map[string]*copierx.TrustedRoot) (*manifest.Stack, []byte, bool, error) {
	return readStackDocument(targetPathOpener(targetRoot, target, allowedSymlinkParents), rel)
}

type renderedDocumentBackup struct {
	path    string
	data    []byte
	mode    fs.FileMode
	existed bool
	dest    *copierx.GuardedPath
}

type renderedDocumentExpectation struct {
	Data   []byte
	Info   fs.FileInfo
	Exists bool
}

func captureRenderedDocumentExpectations(ctx context.Context, openPath guardedPathOpener, documents map[string][]byte) (map[string]renderedDocumentExpectation, error) {
	expectations := make(map[string]renderedDocumentExpectation, len(documents))
	for _, rel := range sortedKeys(documents) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		dest, err := openPath(rel)
		if err != nil {
			return nil, err
		}
		data, info, exists, err := dest.ReadRegularFile()
		_ = dest.Close()
		if err != nil {
			return nil, err
		}
		expectations[rel] = renderedDocumentExpectation{Data: data, Info: info, Exists: exists}
	}
	return expectations, nil
}

func applyRenderedDocuments(ctx context.Context, openPath guardedPathOpener, publicTarget string, documents map[string][]byte, deletions map[string]bool, modes map[string]fs.FileMode, expectations map[string]renderedDocumentExpectation, dryRun bool) (func() error, func() error, func() error, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, err
	}
	if dryRun {
		noop := func() error { return nil }
		return noop, noop, noop, nil
	}
	paths := make([]string, 0, len(documents))
	seen := make(map[string]bool, len(documents))
	for path := range documents {
		paths = append(paths, path)
		seen[path] = true
	}
	for path, deleted := range deletions {
		if deleted && !seen[path] {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	journal := make([]renderedDocumentBackup, 0, len(paths))
	closeGuards := func() error {
		var result error
		for index := range journal {
			if err := journal[index].dest.Close(); err != nil && result == nil {
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
		for index := len(journal) - 1; index >= 0; index-- {
			entry := journal[index]
			if entry.existed {
				if err := entry.dest.WriteFile(entry.data, entry.mode.Perm()); err != nil && result == nil {
					result = err
				}
			} else {
				if err := entry.dest.RemoveAll(); err != nil && !os.IsNotExist(err) && result == nil {
					result = err
				}
				if err := entry.dest.RemoveMissingParents(); err != nil && result == nil {
					result = err
				}
			}
		}
		return result
	}
	fail := func(primary error) (func() error, func() error, func() error, error) {
		return nil, nil, nil, joinRollbackErrors(primary, rollback)
	}
	for _, rel := range paths {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		if filepath.IsAbs(rel) {
			return fail(fmt.Errorf("rendered document path %q must be relative", rel))
		}
		dest, err := openPath(rel)
		if err != nil {
			return fail(fmt.Errorf("rendered document path %q: %w", rel, err))
		}
		entry := renderedDocumentBackup{path: rel, dest: dest}
		data, info, exists, err := dest.ReadRegularFile()
		if err == nil && exists {
			entry.data = data
			entry.mode = info.Mode()
			entry.existed = true
		} else if err != nil {
			return fail(errors.Join(err, dest.Close()))
		}
		if expected, ok := expectations[rel]; ok && (exists != expected.Exists || exists && (!bytes.Equal(data, expected.Data) || expected.Info != nil && !os.SameFile(expected.Info, info))) {
			return fail(errors.Join(fmt.Errorf("rendered document %q changed after its merge was prepared", rel), dest.Close()))
		}
		if err := ctx.Err(); err != nil {
			return fail(errors.Join(err, dest.Close()))
		}
		journal = append(journal, entry)
		if deletions[rel] {
			if err := dest.RemoveAll(); err != nil {
				return fail(err)
			}
			continue
		}
		mode := fs.FileMode(0o644)
		if configured := modes[rel]; configured != 0 {
			mode = configured
		}
		if err := dest.WriteFile(documents[rel], mode); err != nil {
			return fail(err)
		}
	}
	verifyPublicPaths := func() error {
		var result error
		for _, entry := range journal {
			path := filepath.Join(publicTarget, filepath.FromSlash(entry.path))
			var err error
			if deletions[entry.path] {
				err = entry.dest.VerifyPathAbsent(path)
			} else {
				err = entry.dest.VerifyPathIdentity(path)
			}
			if err != nil {
				result = errors.Join(result, err)
			}
		}
		return result
	}
	return rollback, closeGuards, verifyPublicPaths, nil
}
