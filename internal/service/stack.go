package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

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

var chainInputRef = regexp.MustCompile(`{{\s*(\w+)\s*}}`)

// renderStackChain renders every template a stack chains (via `_angee.chain`) into
// the same target BEFORE the stack's own files, so the stack overlays a real host.
// A chained entry's inputs are Copier `{{ input }}` references resolved against the
// stack's own inputs.
func (p *Platform) renderStackChain(ctx context.Context, stackTemplatePath, targetPath string, stackInputs copierx.Inputs) error {
	metadata, err := copierx.ReadMetadata(stackTemplatePath)
	if err != nil {
		return err
	}
	for _, entry := range metadata.Chain {
		if entry.Template == "" {
			continue
		}
		chainTemplate, err := p.resolveChainTemplate(ctx, stackTemplatePath, entry.Template)
		if err != nil {
			return fmt.Errorf("resolve chained template %q: %w", entry.Template, err)
		}
		entryInputs := resolveChainInputs(entry.Inputs, stackInputs)
		merged, err := copierx.TemplateInputs(chainTemplate, entryInputs)
		if err != nil {
			return err
		}
		resolved, err := copierx.ResolvePathInputs(chainTemplate, merged, targetPath, stackInputs["ANGEE_ROOT"])
		if err != nil {
			return err
		}
		if err := (copierx.LocalRenderer{}).Copy(ctx, copierx.CopyRequest{Template: chainTemplate, Dest: targetPath, Inputs: resolved}); err != nil {
			return fmt.Errorf("render chained template %q: %w", entry.Template, err)
		}
	}
	return nil
}

// resolveChainTemplate resolves a chain entry's template reference: a remote ref
// through the git resolver, an absolute path as-is, otherwise a path relative to
// the chaining stack template's own directory (e.g. `../../projects/web`).
func (p *Platform) resolveChainTemplate(ctx context.Context, stackTemplatePath, ref string) (string, error) {
	if isRemoteTemplateRef(ref) {
		path, _, err := p.resolveRemoteTemplate(ctx, ref, "project")
		return path, err
	}
	if filepath.IsAbs(ref) {
		return ref, nil
	}
	return filepath.Clean(filepath.Join(stackTemplatePath, ref)), nil
}

// resolveChainInputs renders a chain entry's `{{ input }}` values against the
// chaining stack's resolved inputs; unknown references are left verbatim.
func resolveChainInputs(entryInputs map[string]string, stackInputs copierx.Inputs) copierx.Inputs {
	out := make(copierx.Inputs, len(entryInputs))
	for key, value := range entryInputs {
		out[key] = chainInputRef.ReplaceAllStringFunc(value, func(match string) string {
			name := chainInputRef.FindStringSubmatch(match)[1]
			if resolved, ok := stackInputs[name]; ok {
				return resolved
			}
			return match
		})
	}
	return out
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
