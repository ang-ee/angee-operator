package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/copierx"
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
	questions, questionDefaults, err := copierx.TemplateQuestions(templatePath)
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

	// The host stack's workspace_defaults for this template sit under the
	// request's inputs, exactly as WorkspaceCreate layers them. A missing
	// manifest is fine (a workspace can be cut with no host stack).
	var stackDefaults map[string]string
	if stack, err := p.LoadStack(); err == nil {
		stackDefaults = stackWorkspaceDefaults(stack, templateRef)
	} else if !os.IsNotExist(err) {
		return api.WorkspaceCreatePreflightResponse{}, err
	}
	provided := mergeStringMaps(stackDefaults, req.Inputs)
	// Questions override metadata declarations, including their defaults. Use
	// Copier's parsed default strings before layering stack and request inputs.
	effective := workspaceInputs(copierx.Metadata{Inputs: defs}, mergeStringMaps(questionDefaults, provided))
	missing := []string{}
	invalid := []api.PreflightFailure{}

	for name, def := range defs {
		if !def.Required {
			continue
		}
		value, ok := effective[name]
		if !ok && def.Generated {
			continue
		}
		if !ok || strings.TrimSpace(value) == "" {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)

	for name, value := range provided {
		def, declared := defs[name]
		if !declared {
			continue
		}
		if reason := validatePreflightInputType(def, value); reason != "" {
			invalid = append(invalid, api.PreflightFailure{Field: name, Reason: reason})
		}
	}
	sort.Slice(invalid, func(i, j int) bool { return invalid[i].Field < invalid[j].Field })

	return api.WorkspaceCreatePreflightResponse{
		OK:               len(missing) == 0 && len(invalid) == 0,
		Template:         req.Template,
		ResolvedTemplate: templateRef,
		EffectiveInputs:  effective,
		StackDefaults:    stackDefaults,
		MissingRequired:  missing,
		InvalidInputs:    invalid,
	}, nil
}

func validatePreflightInputType(def copierx.Input, value string) string {
	if !def.Multiselect {
		return validateInputType(def.Type, value)
	}
	var selections []*string
	if err := json.Unmarshal([]byte(value), &selections); err != nil || selections == nil {
		return "must be a JSON array of strings"
	}
	for _, selection := range selections {
		if selection == nil {
			return "must be a JSON array of strings"
		}
		if reason := validateInputType(def.Type, *selection); reason != "" {
			return reason
		}
	}
	return ""
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
		if strings.TrimSpace(value) == "" {
			return ""
		}
		if _, err := strconv.ParseInt(value, 10, 64); err != nil {
			return fmt.Sprintf("not an integer: %q", value)
		}
		return ""
	}
	return ""
}
