package service

import (
	"context"
	"os"
	"path/filepath"

	"github.com/fyltr/angee/internal/copierx"
	"github.com/fyltr/angee/internal/manifest"
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

func (p *Platform) StackTemplateQuestions(ctx context.Context, template string) (map[string]copierx.Input, copierx.Inputs, error) {
	templatePath, _, err := p.resolveTemplate(ctx, template, "stack")
	if err != nil {
		return nil, nil, err
	}
	if _, err := copierx.ValidateMetadata(templatePath, "stack"); err != nil {
		return nil, nil, err
	}
	return copierx.TemplateQuestions(templatePath)
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
