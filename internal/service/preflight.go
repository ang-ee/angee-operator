package service

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/fyltr/angee/api"
	"github.com/fyltr/angee/internal/copierx"
)

// WorkspaceCreatePreflight validates a WorkspaceCreateRequest against the
// resolved template's input declarations without materialising the
// workspace. The response carries enough detail for clients to surface
// per-field validation failures and avoid partial state on input-shape
// mismatches.
//
// The check covers: required-but-missing inputs, presence of `_angee.inputs`
// declarations, and best-effort type validation (boolean / int currently —
// other types are passed through as strings).
func (p *Platform) WorkspaceCreatePreflight(ctx context.Context, req api.WorkspaceCreateRequest) (api.WorkspaceCreatePreflightResponse, error) {
	if req.Template == "" {
		return api.WorkspaceCreatePreflightResponse{}, &InvalidInputError{Field: "template", Reason: "workspace template is required"}
	}
	templatePath, templateRef, err := p.resolveTemplate(ctx, req.Template, "workspace")
	if err != nil {
		return api.WorkspaceCreatePreflightResponse{}, err
	}
	metadata, err := copierx.ValidateMetadata(templatePath, "workspace")
	if err != nil {
		return api.WorkspaceCreatePreflightResponse{}, err
	}
	questions, _, err := copierx.TemplateQuestions(templatePath)
	if err != nil {
		return api.WorkspaceCreatePreflightResponse{}, err
	}

	defs := map[string]copierx.Input{}
	for name, def := range metadata.Inputs {
		defs[name] = def
	}
	for name, def := range questions {
		defs[name] = def
	}

	effective := workspaceInputs(metadata, req.Inputs)
	missing := []string{}
	invalid := []api.PreflightFailure{}

	for name, def := range defs {
		if !def.Required {
			continue
		}
		value, ok := effective[name]
		if !ok || strings.TrimSpace(value) == "" {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)

	for name, value := range req.Inputs {
		def, declared := defs[name]
		if !declared {
			continue
		}
		if def.Type == "" {
			continue
		}
		if reason := validateInputType(def.Type, value); reason != "" {
			invalid = append(invalid, api.PreflightFailure{Field: name, Reason: reason})
		}
	}
	sort.Slice(invalid, func(i, j int) bool { return invalid[i].Field < invalid[j].Field })

	return api.WorkspaceCreatePreflightResponse{
		OK:               len(missing) == 0 && len(invalid) == 0,
		Template:         req.Template,
		ResolvedTemplate: templateRef,
		EffectiveInputs:  effective,
		MissingRequired:  missing,
		InvalidInputs:    invalid,
	}, nil
}

func validateInputType(declared, value string) string {
	switch strings.ToLower(declared) {
	case "bool", "boolean":
		switch strings.ToLower(value) {
		case "true", "false", "1", "0", "yes", "no", "y", "n", "":
			return ""
		default:
			return fmt.Sprintf("not a boolean: %q", value)
		}
	case "int", "integer":
		for _, r := range value {
			if r == '-' || r == '+' {
				continue
			}
			if r < '0' || r > '9' {
				return fmt.Sprintf("not an integer: %q", value)
			}
		}
		return ""
	}
	return ""
}
