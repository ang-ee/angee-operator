package service

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

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

func writeRenderedDocument(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".angee-document-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Chmod(0o644); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

type renderedDocumentBackup struct {
	path    string
	data    []byte
	mode    fs.FileMode
	existed bool
}

func applyRenderedDocuments(target string, documents map[string][]byte, dryRun bool) (func() error, error) {
	if dryRun {
		return func() error { return nil }, nil
	}
	paths := make([]string, 0, len(documents))
	for path := range documents {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	journal := make([]renderedDocumentBackup, 0, len(paths))
	rolledBack := false
	rollback := func() error {
		if rolledBack {
			return nil
		}
		rolledBack = true
		var result error
		for index := len(journal) - 1; index >= 0; index-- {
			entry := journal[index]
			if entry.existed {
				if err := writeRenderedDocument(entry.path, entry.data); err != nil && result == nil {
					result = err
				}
				if err := os.Chmod(entry.path, entry.mode.Perm()); err != nil && result == nil {
					result = err
				}
			} else if err := os.Remove(entry.path); err != nil && !os.IsNotExist(err) && result == nil {
				result = err
			}
		}
		return result
	}
	for _, rel := range paths {
		if filepath.IsAbs(rel) {
			_ = rollback()
			return nil, fmt.Errorf("rendered document path %q must be relative", rel)
		}
		dest := filepath.Clean(filepath.Join(target, filepath.FromSlash(rel)))
		within, err := filepath.Rel(target, dest)
		if err != nil || within == ".." || len(within) >= 3 && within[:3] == ".."+string(filepath.Separator) {
			_ = rollback()
			return nil, fmt.Errorf("rendered document path %q escapes target", rel)
		}
		entry := renderedDocumentBackup{path: dest}
		if info, err := os.Stat(dest); err == nil {
			entry.data, err = os.ReadFile(dest)
			if err != nil {
				_ = rollback()
				return nil, err
			}
			entry.mode = info.Mode()
			entry.existed = true
		} else if !os.IsNotExist(err) {
			_ = rollback()
			return nil, err
		}
		journal = append(journal, entry)
		if err := writeRenderedDocument(dest, documents[rel]); err != nil {
			_ = rollback()
			return nil, err
		}
	}
	return rollback, nil
}
