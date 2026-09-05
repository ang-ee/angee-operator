package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/cli/inputform"
	"github.com/ang-ee/angee-operator/internal/service"
	"github.com/spf13/cobra"
)

func serviceCreateCommand(stdout io.Writer, root, operatorURL *string, jsonOutput *bool) *cobra.Command {
	var req api.ServiceCreateRequest
	var inputValues []string
	var yes, interactive bool
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Render a service from a template into the stack",
		Long: `Render a Copier template with _angee.kind: service into the outer
stack as a single service entry, bound to a workspace.

The form appears when required inputs are missing or with --interactive.
Use --yes to accept defaults without prompting. Piped stdin uses line prompts
for missing required inputs.

Example:
  angee service create --template ./templates/agents/claude-code \
    --workspace my-pa --input auth_mode=api_key`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			inputs, err := parseKeyValues(inputValues)
			if err != nil {
				return err
			}
			req.Inputs = inputs
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			state, err := createServiceFromTemplate(cmd, platform, req, yes, interactive)
			if err != nil {
				return err
			}
			if *jsonOutput {
				return writeJSON(stdout, state)
			}
			_, err = fmt.Fprintf(stdout, "service %s created (runtime=%s status=%s)\n", state.Name, state.Runtime, state.Status)
			return err
		},
	}
	cmd.Flags().StringVar(&req.Template, "template", "", "template ref or path (required)")
	cmd.Flags().StringVar(&req.Workspace, "workspace", "", "target workspace name (required)")
	cmd.Flags().StringArrayVar(&inputValues, "input", nil, "template input K=V (repeatable)")
	cmd.Flags().StringArray("answers", nil, "template answers YAML file (repeatable; later files override earlier ones)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "accept template defaults and run non-interactively (no form)")
	cmd.Flags().BoolVarP(&interactive, "interactive", "i", false, "review template inputs in the interactive form")
	cmd.Flags().StringVar(&req.Name, "name", "", "override resolved service name")
	cmd.Flags().BoolVar(&req.Start, "start", false, "start the service after create")
	_ = cmd.MarkFlagRequired("template")
	_ = cmd.MarkFlagRequired("workspace")
	return cmd
}

func createServiceFromTemplate(cmd *cobra.Command, platform service.API, req api.ServiceCreateRequest, yes, interactive bool) (api.ServiceState, error) {
	mode, err := createTemplateFormMode(cmd, yes, interactive)
	if err != nil {
		return api.ServiceState{}, err
	}
	provided, err := loadTemplateInputValues(cmd, req.Inputs, nil)
	if err != nil {
		return api.ServiceState{}, err
	}
	if filepath.IsAbs(req.Template) || strings.Contains(req.Template, "..") {
		req.Inputs = provided.Explicit()
		return platform.ServiceCreate(cmd.Context(), req)
	}
	ref := req.Template
	if !strings.Contains(ref, "/") {
		ref = "services/" + ref
	}
	desc, err := platform.Template(cmd.Context(), ref)
	if err != nil {
		return api.ServiceState{}, err
	}
	missing, invalid, _ := templateInputProblems(desc.Inputs, provided.Values)
	show := interactive || (mode == inputform.ModeInteractive && (missing || invalid)) || (mode == inputform.ModeScripted && missing)
	req.Inputs, err = resolveCreateTemplateForm(cmd, desc, provided, mode, show, "Create service from "+ref, "Create the service?")
	if err != nil {
		return api.ServiceState{}, err
	}
	return platform.ServiceCreate(cmd.Context(), req)
}

// templateInputProblems checks effective values without collecting defaults into
// the explicit inputs sent to the platform. Generated values belong to rendering.
func templateInputProblems(inputs []api.TemplateInputDescriptor, provided map[string]string) (missing, invalid bool, err error) {
	var failures []error
	for _, desc := range inputs {
		value, explicit := provided[desc.Name]
		if !explicit {
			if desc.Generated {
				continue
			}
			value = desc.Default
			if desc.Multiselect && value == "" {
				value = "[]"
			}
		}
		if value == "" && !explicit && !desc.Required && len(desc.Choices) == 0 && !desc.Multiselect {
			continue
		}
		validationErr := inputform.Validate(desc, value)
		if validationErr == nil {
			continue
		}
		failures = append(failures, validationErr)
		empty := value == ""
		if desc.Multiselect {
			var selections []json.RawMessage
			if json.Unmarshal([]byte(value), &selections) == nil && selections != nil && len(selections) == 0 {
				empty = true
			}
		}
		if desc.Required && empty {
			missing = true
		} else {
			invalid = true
		}
	}
	return missing, invalid, errors.Join(failures...)
}
