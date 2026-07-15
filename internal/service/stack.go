package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ang-ee/angee-operator/internal/copierx"
	"github.com/ang-ee/angee-operator/internal/manifest"
)

type StackInitResult struct {
	Template string `json:"template"`
	Root     string `json:"root"`
}

func (p *Platform) StackInit(ctx context.Context, template string, targetPath string, inputs map[string]string, force bool) (StackInitResult, error) {
	if template == "" {
		return StackInitResult{}, &InvalidInputError{Field: "template", Reason: "stack template is required"}
	}
	if targetPath == "" {
		targetPath = p.root
	}
	if !filepath.IsAbs(targetPath) {
		targetPath = filepath.Join(p.root, targetPath)
	}
	templatePath, _, err := p.resolveTemplate(ctx, template, "stack")
	if err != nil {
		return StackInitResult{}, err
	}
	if _, err := copierx.ValidateMetadata(templatePath, "stack"); err != nil {
		return StackInitResult{}, err
	}
	mergedInputs, err := copierx.TemplateInputs(templatePath, copierx.Inputs(inputs))
	if err != nil {
		return StackInitResult{}, err
	}
	preparedRoot := expectedStackRoot(targetPath, mergedInputs)
	if !force {
		nonEmpty, err := pathExistsNonEmpty(preparedRoot)
		if err != nil {
			return StackInitResult{}, err
		}
		if nonEmpty {
			return StackInitResult{}, &ConflictError{
				Kind:   "stack-root",
				Name:   preparedRoot,
				Reason: "already exists and is non-empty; use --force to overwrite or `angee stack update` to update",
			}
		}
	}
	if err := os.MkdirAll(targetPath, 0o755); err != nil {
		return StackInitResult{}, err
	}
	statePath := renderPlanStatePath(preparedRoot, "stack", "")
	plan, documents, err := p.buildStackRenderPlan(ctx, templatePath, targetPath, mergedInputs, statePath, stackPlanOptions{})
	if err != nil {
		return StackInitResult{}, err
	}
	plan.StateRoot = targetPath
	plan.TargetRoot = targetPath
	if len(documents) == 0 {
		return StackInitResult{}, fmt.Errorf("stack template %q rendered no angee.yaml", template)
	}
	prepared, err := copierx.PrepareReconcile(ctx, plan, copierx.ReconcileOptions{Mode: copierx.ReconcileCreate, Overwrite: force})
	if err != nil {
		return StackInitResult{}, err
	}
	defer func() { _ = prepared.Close() }()
	if err := joinRollbackErrors(prepared.VerifyTargetRootPath(), prepared.VerifyStateRootPath); err != nil {
		return StackInitResult{}, err
	}
	renderedDocuments := make(map[string][]byte, len(documents))
	var stack *manifest.Stack
	for _, document := range documents {
		data, ok := prepared.RenderedDocument(document.Path)
		if !ok {
			return StackInitResult{}, fmt.Errorf("stack template did not render %s", document.Path)
		}
		renderedDocuments[document.Path] = data
		if filepath.Clean(filepath.Join(targetPath, filepath.FromSlash(document.Path))) == filepath.Clean(manifest.Path(preparedRoot)) {
			stack, err = decodeStackDocument(data)
			if err != nil {
				return StackInitResult{}, fmt.Errorf("load rendered stack %s: %w", document.Path, err)
			}
		}
	}
	if stack == nil {
		return StackInitResult{}, fmt.Errorf("stack template rendered no manifest for %s", preparedRoot)
	}
	documentExpectations, err := captureRenderedDocumentExpectations(ctx, prepared.OpenTargetPath, renderedDocuments)
	if err != nil {
		return StackInitResult{}, err
	}
	initialized, err := New(preparedRoot)
	if err != nil {
		return StackInitResult{}, err
	}
	if err := joinRollbackErrors(prepared.VerifyTargetRootPath(), prepared.VerifyStateRootPath); err != nil {
		return StackInitResult{}, err
	}
	rollbackResources, closeResources, verifyResources, err := initialized.stageStackResources(ctx, stack, preparedAbsolutePathOpener(prepared, targetPath))
	if err != nil {
		return StackInitResult{}, err
	}
	defer func() { _ = closeResources() }()
	rollbackFiles, err := prepared.ApplyFiles(ctx)
	if err != nil {
		return StackInitResult{}, joinRollbackErrors(err, rollbackResources)
	}
	rollbackDocuments, closeDocuments, verifyDocuments, err := applyRenderedDocuments(ctx, prepared.OpenTargetPath, targetPath, renderedDocuments, nil, nil, documentExpectations, false)
	if err != nil {
		return StackInitResult{}, joinRollbackErrors(err, rollbackFiles, rollbackResources)
	}
	defer func() { _ = closeDocuments() }()
	if err := joinRollbackErrors(prepared.VerifyTargetRootPath(), verifyDocuments, verifyResources); err != nil {
		return StackInitResult{}, joinRollbackErrors(err, rollbackDocuments, rollbackFiles, rollbackResources)
	}
	if err := prepared.SaveState(ctx); err != nil {
		return StackInitResult{}, joinRollbackErrors(err, rollbackDocuments, rollbackFiles, rollbackResources)
	}
	return StackInitResult{Template: template, Root: preparedRoot}, nil
}

func expectedStackRoot(targetPath string, inputs map[string]string) string {
	if angeeRoot := inputs["ANGEE_ROOT"]; angeeRoot != "" {
		return manifest.ResolvePath(targetPath, angeeRoot)
	}
	return targetPath
}

func pathExistsNonEmpty(path string) (bool, error) {
	entries, err := os.ReadDir(path)
	if err == nil {
		return len(entries) > 0, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// resolveChainTemplate resolves a chain entry's template reference the same way
// workspace chains do (resolveWorkspaceChainTemplate): a path relative to the
// chaining stack template's own directory when a `copier.yml` lives there (e.g.
// `../../projects/web`), otherwise through the shared resolver, which handles a
// remote ref, an absolute path, or a `stacks/<name>` template in the configured
// template roots. Refs come from the (semi-trusted) template author, so a relative
// `..` escape is deliberate and permitted.
func (p *Platform) resolveChainTemplate(ctx context.Context, stackTemplatePath, ref string) (string, error) {
	if ref != "" && !filepath.IsAbs(ref) && !isRemoteTemplateRef(ref) {
		candidate := filepath.Clean(filepath.Join(stackTemplatePath, filepath.FromSlash(ref)))
		if _, err := os.Stat(filepath.Join(candidate, "copier.yml")); err == nil {
			return candidate, nil
		}
	}
	path, _, err := p.resolveTemplate(ctx, ref, "stack")
	return path, err
}

func (p *Platform) StackUpdate(ctx context.Context) error {
	_, err := p.StackPrepare(ctx)
	return err
}

func (p *Platform) StackDestroy(ctx context.Context, purge bool) error {
	if err := p.StackDown(ctx); err != nil {
		return err
	}
	for _, name := range []string{"docker-compose.yaml", "process-compose.yaml"} {
		if err := os.Remove(filepath.Join(p.root, name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if purge {
		for _, name := range []string{"workspaces", "sources", "volumes", "run"} {
			if err := os.RemoveAll(filepath.Join(p.root, name)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *Platform) EmptyStack(name string) *manifest.Stack {
	return &manifest.Stack{Version: manifest.VersionCurrent, Kind: manifest.KindStack, Name: name}
}
