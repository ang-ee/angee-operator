package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/fyltr/angee/api"
	"github.com/fyltr/angee/internal/copierx"
)

// Templates discovers every template under the stack root's
// `.templates/<kindPlural>/<name>` and `templates/<kindPlural>/<name>`
// directories and returns a descriptor per template. The result is sorted
// by ref for deterministic output.
//
// Templates that fail to parse are skipped silently — `Template(ref)` is
// the right place to surface parse errors against a specific ref.
func (p *Platform) Templates(ctx context.Context) ([]api.TemplateDescriptor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	roots := []string{
		filepath.Join(p.root, ".templates"),
		filepath.Join(p.root, "templates"),
	}
	seen := map[string]struct{}{}
	descriptors := []api.TemplateDescriptor{}
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, kindEntry := range entries {
			if !kindEntry.IsDir() {
				continue
			}
			kindDir := filepath.Join(root, kindEntry.Name())
			templateEntries, err := os.ReadDir(kindDir)
			if err != nil {
				return nil, err
			}
			for _, te := range templateEntries {
				if !te.IsDir() {
					continue
				}
				templatePath := filepath.Join(kindDir, te.Name())
				if _, err := os.Stat(filepath.Join(templatePath, "copier.yml")); err != nil {
					continue
				}
				ref := fmt.Sprintf("%s/%s", kindEntry.Name(), te.Name())
				if _, dup := seen[ref]; dup {
					continue
				}
				seen[ref] = struct{}{}
				desc, err := templateDescriptor(ref, templatePath)
				if err != nil {
					continue
				}
				descriptors = append(descriptors, desc)
			}
		}
	}
	sort.Slice(descriptors, func(i, j int) bool { return descriptors[i].Ref < descriptors[j].Ref })
	return descriptors, nil
}

// Template returns the descriptor for the template identified by ref.
// ref is a relative path under `<root>/.templates/<kind>/<name>` or
// `<root>/templates/<kind>/<name>` (e.g. `workspaces/dev-pr`), or a
// remote reference accepted by resolveTemplate. Kind is inferred from
// the first path segment when the ref is relative.
//
// Absolute filesystem paths and refs containing `..` segments are
// rejected: this is an introspection surface reachable from REST and
// GraphQL with only the admin bearer, and the CLI's local-path
// `--template /abs/path` flow does not go through here (it calls
// WorkspaceCreate which uses resolveTemplate directly).
func (p *Platform) Template(ctx context.Context, ref string) (api.TemplateDescriptor, error) {
	if ref == "" {
		return api.TemplateDescriptor{}, &InvalidInputError{Field: "ref", Reason: "template ref is required"}
	}
	if !isRemoteTemplateRef(ref) {
		if filepath.IsAbs(ref) {
			return api.TemplateDescriptor{}, &InvalidInputError{Field: "ref", Reason: "absolute paths are not allowed; use a relative ref under the stack root"}
		}
		if strings.Contains(ref, "..") {
			return api.TemplateDescriptor{}, &InvalidInputError{Field: "ref", Reason: "ref must not contain `..` segments"}
		}
	}
	kind := templateKindFromRef(ref)
	templatePath, resolvedRef, err := p.resolveTemplate(ctx, ref, kind)
	if err != nil {
		return api.TemplateDescriptor{}, err
	}
	return templateDescriptor(resolvedRef, templatePath)
}

func templateDescriptor(ref, templatePath string) (api.TemplateDescriptor, error) {
	metadata, err := copierx.ReadMetadata(templatePath)
	if err != nil {
		return api.TemplateDescriptor{}, err
	}
	questions, defaults, err := copierx.TemplateQuestions(templatePath)
	if err != nil {
		return api.TemplateDescriptor{}, err
	}
	defs := map[string]copierx.Input{}
	for name, def := range metadata.Inputs {
		defs[name] = def
	}
	for name, def := range questions {
		defs[name] = def
	}
	inputs := make([]api.TemplateInputDescriptor, 0, len(defs))
	for name, def := range defs {
		desc := api.TemplateInputDescriptor{
			Name:      name,
			Type:      def.Type,
			Required:  def.Required,
			Immutable: def.Immutable,
			Generated: def.Generated,
		}
		if v, ok := defaults[name]; ok {
			desc.Default = v
		} else if def.Default != nil {
			desc.Default = fmt.Sprint(def.Default)
		}
		inputs = append(inputs, desc)
	}
	sort.Slice(inputs, func(i, j int) bool { return inputs[i].Name < inputs[j].Name })
	return api.TemplateDescriptor{
		Ref:    ref,
		Kind:   metadata.Kind,
		Name:   metadata.Name,
		Path:   templatePath,
		Inputs: inputs,
	}, nil
}

// templateKindFromRef extracts the kind segment from a relative ref like
// `workspaces/dev-pr` and returns the singular form that resolveTemplate
// expects. Only the plural kinds we actually recognise are mapped; any
// other first segment returns an empty string so resolveTemplate's
// "not found" path takes over with a meaningful error.
func templateKindFromRef(ref string) string {
	if filepath.IsAbs(ref) || isRemoteTemplateRef(ref) {
		return ""
	}
	parts := strings.SplitN(ref, "/", 2)
	if len(parts) == 0 {
		return ""
	}
	switch parts[0] {
	case "workspaces":
		return "workspace"
	case "stacks":
		return "stack"
	default:
		return ""
	}
}
