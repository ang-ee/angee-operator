package service

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/copierx"
)

// StackTemplateInputs returns the descriptor and recorded answers used by a
// stack template update, including stacks rendered into a parent directory.
func (p *Platform) StackTemplateInputs(ctx context.Context) (api.TemplateInputsResponse, error) {
	origin, err := p.resolveStackTemplateOrigin(ctx)
	if err != nil {
		return api.TemplateInputsResponse{}, err
	}
	return describeRecordedTemplateInputs("stack", origin.Ref, origin.Path, origin.Answers)
}

// WorkspaceTemplateInputs returns inherited stack defaults overlaid with the
// workspace's persisted answers for its current template.
func (p *Platform) WorkspaceTemplateInputs(ctx context.Context, name string) (api.TemplateInputsResponse, error) {
	if err := ctx.Err(); err != nil {
		return api.TemplateInputsResponse{}, err
	}
	stack, err := p.LoadStack()
	if err != nil {
		return api.TemplateInputsResponse{}, err
	}
	workspace, ok := stack.Workspaces[name]
	if !ok {
		return api.TemplateInputsResponse{}, &NotFoundError{Kind: "workspace", Name: name}
	}
	path, ref, err := p.resolveTemplate(ctx, workspace.Template, "workspace")
	if err != nil {
		return api.TemplateInputsResponse{}, err
	}
	recorded := mergeStringMaps(stackWorkspaceDefaults(stack, ref), workspace.Inputs)
	return describeRecordedTemplateInputs("workspace/"+name, ref, path, recorded)
}

// ServiceTemplateInputs returns the same render-state origin and answers used
// by ServiceUpdateFromTemplate, with support for legacy Copier answers files.
func (p *Platform) ServiceTemplateInputs(ctx context.Context, name string) (api.TemplateInputsResponse, error) {
	if err := ctx.Err(); err != nil {
		return api.TemplateInputsResponse{}, err
	}
	if name == "" {
		return api.TemplateInputsResponse{}, &InvalidInputError{Field: "name", Reason: "service name is required"}
	}
	if !serviceNamePattern.MatchString(name) {
		return api.TemplateInputsResponse{}, &InvalidInputError{Field: "name", Reason: "service name must match " + serviceNamePattern.String()}
	}
	stack, err := p.LoadStack()
	if err != nil {
		return api.TemplateInputsResponse{}, err
	}
	if _, ok := stack.Services[name]; !ok {
		return api.TemplateInputsResponse{}, &NotFoundError{Kind: "service", Name: name}
	}
	state, hasState, err := copierx.ReadRenderStateRooted(p.root, renderPlanStatePath(p.root, "services", name))
	if err != nil {
		return api.TemplateInputsResponse{}, err
	}
	ref, answersPath, err := serviceTemplateOrigin(filepath.Join(p.root, "services", name), state, hasState)
	if err != nil {
		return api.TemplateInputsResponse{}, err
	}
	sourceFromAnswers, answers, err := readTemplateAnswers(answersPath)
	if err != nil {
		return api.TemplateInputsResponse{}, fmt.Errorf("read service template answers %q: %w", answersPath, err)
	}
	if ref == "" {
		ref = sourceFromAnswers
	}
	if ref == "" {
		return api.TemplateInputsResponse{}, &InvalidInputError{Field: "template", Reason: "service template origin is missing"}
	}
	path, _, err := p.resolveTemplate(ctx, ref, "service")
	if err != nil {
		return api.TemplateInputsResponse{}, err
	}
	return describeRecordedTemplateInputs("service/"+name, ref, path, answers)
}

// describeRecordedTemplateInputs pairs the descriptor with the recorded
// answers a client may see: only declared inputs, never secrets or generated
// values (a service answers file records every render variable, so filtering
// by the descriptor is what keeps tokens off the wire). Secret questions are
// reported as unrecorded so the form asks for them again; left blank, the
// re-render falls back to whatever the answers file or template default
// holds for them.
func describeRecordedTemplateInputs(target, ref, path string, recorded map[string]string) (api.TemplateInputsResponse, error) {
	descriptor, err := templateDescriptor(ref, path)
	if err != nil {
		return api.TemplateInputsResponse{}, err
	}
	result := api.TemplateInputsResponse{
		Target: target, Template: descriptor, Recorded: make(map[string]string),
	}
	for _, input := range descriptor.Inputs {
		if input.Secret {
			if input.Question {
				result.Unrecorded = append(result.Unrecorded, input.Name)
			}
			continue
		}
		if input.Generated {
			continue
		}
		if value, ok := recorded[input.Name]; ok {
			result.Recorded[input.Name] = value
		}
	}
	return result, nil
}

// validateImmutableTemplateInputs checks only explicit update overrides. Secret
// answers absent from the previous render cannot be compared and remain locked.
func validateImmutableTemplateInputs(ref, path string, recorded, provided map[string]string) error {
	if len(provided) == 0 {
		return nil
	}
	descriptor, err := templateDescriptor(ref, path)
	if err != nil {
		return err
	}
	for _, input := range descriptor.Inputs {
		value, hasProvided := provided[input.Name]
		if !input.Immutable || !hasProvided {
			continue
		}
		old, hadRecorded := recorded[input.Name]
		if hadRecorded && value == old {
			continue
		}
		reason := fmt.Sprintf("template input %s is immutable; it was rendered as %q", input.Name, old)
		if input.Secret {
			reason = fmt.Sprintf("template input %s is immutable; it cannot change after the first render", input.Name)
		}
		return &InvalidInputError{Field: input.Name, Reason: reason}
	}
	return nil
}
