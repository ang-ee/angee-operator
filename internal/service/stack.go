package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ang-ee/angee-operator/internal/copierx"
	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/substitute"
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
	resolvedInputs, err := copierx.ResolvePathInputs(templatePath, mergedInputs, targetPath, mergedInputs["ANGEE_ROOT"])
	if err != nil {
		return StackInitResult{}, err
	}
	// Render any chained templates (e.g. the project host this stack overlays) into
	// the same target FIRST, so the stack template's own files overlay theirs.
	if err := p.renderStackChain(ctx, templatePath, targetPath, mergedInputs); err != nil {
		return StackInitResult{}, err
	}
	if err := (copierx.LocalRenderer{}).Copy(ctx, copierx.CopyRequest{Template: templatePath, Dest: targetPath, Inputs: resolvedInputs}); err != nil {
		return StackInitResult{}, err
	}
	if _, err := os.Stat(manifest.Path(preparedRoot)); err != nil {
		if angeeRoot, ok := inputs["ANGEE_ROOT"]; ok && angeeRoot != "" {
			candidate := manifest.ResolvePath(targetPath, angeeRoot)
			if _, statErr := os.Stat(manifest.Path(candidate)); statErr == nil {
				preparedRoot = candidate
			}
		} else {
			candidate := filepath.Join(targetPath, ".angee")
			if _, statErr := os.Stat(manifest.Path(candidate)); statErr == nil {
				preparedRoot = candidate
			}
		}
	}
	initialized, err := New(preparedRoot)
	if err != nil {
		return StackInitResult{}, err
	}
	stack, err := initialized.LoadStack()
	if err != nil {
		return StackInitResult{}, err
	}
	if err := initialized.materializeReferencedSources(ctx, stack); err != nil {
		return StackInitResult{}, err
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

// renderStackChain renders every template a stack chains (via `_angee.chain`) into
// the same target BEFORE the stack's own files, so the stack overlays a real host.
// Chain entries share the schema and grammar of workspace chains (renderWorkspaceChain):
// the template ref and each input value are resolved through the project-wide `${...}`
// substitution grammar against the stack's own inputs, and an optional `root`
// (defaulting to `chain_root`) selects a sub-directory. Stack chains overlay in place,
// so an empty root renders the host directly into the target.
//
// Known limitation: a flat overlay (empty root) shares Copier's `.copier-answers.yml`
// with the stack render that follows, which overwrites it, so `angee stack update`
// refreshes the stack layer but not the host layer. Give a host its own `root` to keep
// it independently updatable.
func (p *Platform) renderStackChain(ctx context.Context, stackTemplatePath, targetPath string, stackInputs copierx.Inputs) error {
	metadata, err := copierx.ReadMetadata(stackTemplatePath)
	if err != nil {
		return err
	}
	subCtx := substitute.Context{Inputs: stackInputs}
	chainRoot := metadata.ChainRoot
	if chainRoot != "" {
		if chainRoot, err = substitute.Resolve(chainRoot, subCtx); err != nil {
			return fmt.Errorf("resolve chain_root: %w", err)
		}
	}
	for _, entry := range metadata.Chain {
		if entry.Template == "" {
			continue
		}
		templateRef, err := substitute.Resolve(entry.Template, subCtx)
		if err != nil {
			return fmt.Errorf("chained template %q: %w", entry.Template, err)
		}
		chainTemplate, err := p.resolveChainTemplate(ctx, stackTemplatePath, templateRef)
		if err != nil {
			return fmt.Errorf("resolve chained template %q: %w", entry.Template, err)
		}
		renderInputs := copierx.Inputs{}
		for key, value := range stackInputs {
			renderInputs[key] = value
		}
		for key, value := range entry.Inputs {
			resolved, err := substitute.Resolve(value, subCtx)
			if err != nil {
				return fmt.Errorf("chained template %q input %q: %w", entry.Template, key, err)
			}
			renderInputs[key] = resolved
		}
		destRoot := chainRoot
		if entry.Root != "" {
			if destRoot, err = substitute.Resolve(entry.Root, subCtx); err != nil {
				return fmt.Errorf("chained template %q root: %w", entry.Template, err)
			}
		}
		dest := targetPath
		if destRoot != "" {
			dest = filepath.Join(targetPath, destRoot)
		}
		merged, err := copierx.TemplateInputs(chainTemplate, renderInputs)
		if err != nil {
			return fmt.Errorf("chained template %q inputs: %w", entry.Template, err)
		}
		resolved, err := copierx.ResolvePathInputs(chainTemplate, merged, dest, merged["ANGEE_ROOT"])
		if err != nil {
			return fmt.Errorf("chained template %q inputs: %w", entry.Template, err)
		}
		if err := (copierx.LocalRenderer{}).Copy(ctx, copierx.CopyRequest{Template: chainTemplate, Dest: dest, Inputs: resolved}); err != nil {
			return fmt.Errorf("render chained template %q: %w", entry.Template, err)
		}
	}
	return nil
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
