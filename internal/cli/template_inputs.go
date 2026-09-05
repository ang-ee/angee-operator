package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/cli/inputform"
	"github.com/ang-ee/angee-operator/internal/service"
	"github.com/spf13/cobra"
)

// loadTemplateInputValues keeps the provenance of each layer so renderer-owned
// defaults never become explicit overrides merely by accepting the form.
func loadTemplateInputValues(cmd *cobra.Command, flags, stack map[string]string) (inputform.Result, error) {
	result := inputform.Result{Values: map[string]string{}, Origins: map[string]inputform.Origin{}}
	merge := func(values map[string]string, origin inputform.Origin) {
		for key, value := range values {
			result.Values[key] = value
			result.Origins[key] = origin
		}
	}
	merge(stack, inputform.OriginStack)
	if cmd.Flags().Lookup("answers") != nil {
		paths, err := cmd.Flags().GetStringArray("answers")
		if err != nil {
			return inputform.Result{}, err
		}
		for _, path := range paths {
			values, err := inputform.LoadAnswersFile(path)
			if err != nil {
				return inputform.Result{}, err
			}
			merge(values, inputform.OriginAnswers)
		}
	}
	merge(flags, inputform.OriginFlag)
	return result, nil
}

func resolveWorkspaceTemplateInputs(cmd *cobra.Command, platform service.API, req api.WorkspaceCreateRequest, mode inputform.Mode, interactive bool) (map[string]string, error) {
	provided, err := loadTemplateInputValues(cmd, req.Inputs, nil)
	if err != nil {
		return nil, err
	}
	req.Inputs = provided.Explicit()
	preflight, err := platform.WorkspaceCreatePreflight(cmd.Context(), req)
	if err != nil {
		return nil, err
	}
	for key, value := range preflight.StackDefaults {
		if _, exists := provided.Values[key]; !exists {
			provided.Values[key] = value
			provided.Origins[key] = inputform.OriginStack
		}
	}
	if filepath.IsAbs(req.Template) || strings.Contains(req.Template, "..") {
		return provided.Explicit(), workspaceInputPreflightError(preflight)
	}
	ref := req.Template
	if !strings.Contains(ref, "/") {
		ref = "workspaces/" + ref
	}
	desc, err := platform.Template(cmd.Context(), ref)
	if err != nil {
		return nil, err
	}
	for _, name := range preflight.MissingRequired {
		if provided.Origins[name] == inputform.OriginStack && strings.TrimSpace(provided.Values[name]) == "" {
			// Preflight treats whitespace-only inherited values as missing.
			provided.Values[name] = ""
		}
	}
	missing, invalid, _ := templateInputProblems(desc.Inputs, provided.Values)
	missing = missing || len(preflight.MissingRequired) > 0
	invalid = invalid || len(preflight.InvalidInputs) > 0
	show := interactive || (mode == inputform.ModeInteractive && (missing || invalid)) || (mode == inputform.ModeScripted && missing)
	req.Inputs, err = resolveCreateTemplateForm(cmd, desc, provided, mode, show,
		"Create workspace "+req.Name+" from "+ref, "Create the workspace?")
	if err != nil {
		return nil, err
	}
	preflight, err = platform.WorkspaceCreatePreflight(cmd.Context(), req)
	if err != nil {
		return nil, err
	}
	return req.Inputs, workspaceInputPreflightError(preflight)
}

func workspaceInputPreflightError(preflight api.WorkspaceCreatePreflightResponse) error {
	var failures []error
	for _, name := range preflight.MissingRequired {
		failures = append(failures, inputform.Validate(api.TemplateInputDescriptor{Name: name, Required: true}, ""))
	}
	for _, failure := range preflight.InvalidInputs {
		failures = append(failures, fmt.Errorf("template input %s: %s", failure.Field, failure.Reason))
	}
	return errors.Join(failures...)
}

func createTemplateFormMode(cmd *cobra.Command, yes, interactive bool) (inputform.Mode, error) {
	if yes && interactive {
		return 0, fmt.Errorf("--interactive cannot be combined with --yes")
	}
	tty := cmd.InOrStdin() == os.Stdin && stdinIsTerminal()
	if interactive && !tty {
		return 0, fmt.Errorf("--interactive requires a terminal; use --input or --answers, or pipe answers without --interactive")
	}
	mode := inputform.DetectMode(yes, tty, os.Getenv)
	if interactive && mode != inputform.ModeInteractive {
		return 0, fmt.Errorf("--interactive requires the terminal form; unset ANGEE_ACCESSIBLE and use TERM other than dumb")
	}
	return mode, nil
}

func resolveCreateTemplateForm(cmd *cobra.Command, desc api.TemplateDescriptor, provided inputform.Result, mode inputform.Mode, show bool, title, confirm string) (map[string]string, error) {
	inputs := append([]api.TemplateInputDescriptor(nil), desc.Inputs...)
	if !show || mode == inputform.ModeDefaults {
		mode = inputform.ModeDefaults
	} else if mode == inputform.ModeScripted {
		// Create commands prompt only for unresolved questions. Preserve origin
		// information by hiding satisfied fields instead of pre-filling defaults.
		for i, input := range inputs {
			value, explicit := provided.Values[input.Name]
			if !explicit {
				value = input.Default
				if input.Multiselect && value == "" {
					value = "[]"
				}
			}
			if provided.Origins[input.Name] == inputform.OriginStack && input.Question && !input.Generated && !input.Immutable {
				if missing, _, _ := templateInputProblems([]api.TemplateInputDescriptor{input}, provided.Values); missing {
					// An empty inherited value is unresolved, so the scripted
					// form must collect it instead of treating it as an answer.
					delete(provided.Values, input.Name)
					delete(provided.Origins, input.Name)
					inputs[i].Default = value
				}
			}
			if inputform.Validate(input, value) == nil || (!input.Required && !explicit && value == "" && len(input.Choices) == 0 && !input.Multiselect) {
				inputs[i].Question = false
			}
		}
	}
	result, err := inputform.Run(cmd.Context(), inputform.Request{
		Title: title, Confirm: confirm, Inputs: inputs,
		Provided: provided.Values, Origins: provided.Origins, Mode: mode,
		In: cmd.InOrStdin(), Out: cmd.OutOrStdout(), Err: cmd.ErrOrStderr(),
	})
	if err != nil {
		return nil, err
	}
	explicit := result.Explicit()
	if _, _, err := templateInputProblems(desc.Inputs, explicit); err != nil {
		return nil, err
	}
	return explicit, nil
}
