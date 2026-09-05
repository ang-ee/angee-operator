package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/cli/inputform"
	"github.com/ang-ee/angee-operator/internal/service"
	"github.com/spf13/cobra"
)

// templateUpdateForm keeps terminal detection and form execution together. A
// context-scoped implementation lets command tests drive the form without a TTY
// or process-wide mutable hooks.
type templateUpdateForm struct {
	detectMode func(*cobra.Command, bool) inputform.Mode
	run        func(context.Context, inputform.Request) (inputform.Result, error)
}

type templateUpdateFormKey struct{}

func updateForm(cmd *cobra.Command) templateUpdateForm {
	if form, ok := cmd.Context().Value(templateUpdateFormKey{}).(templateUpdateForm); ok {
		return form
	}
	return templateUpdateForm{
		detectMode: func(cmd *cobra.Command, yes bool) inputform.Mode {
			return inputform.DetectMode(yes, cmd.InOrStdin() == os.Stdin && stdinIsTerminal(), os.Getenv)
		},
		run: inputform.Run,
	}
}

// requireUpdateInteractiveMode rejects -i where the form cannot run: the
// update commands have no defaults mode, so only the terminal matters.
func requireUpdateInteractiveMode(cmd *cobra.Command, interactive bool) error {
	if interactive && updateForm(cmd).detectMode(cmd, false) != inputform.ModeInteractive {
		return fmt.Errorf("--interactive requires a terminal; pass --input/--answers instead")
	}
	return nil
}

func resolveUpdateTemplateInputs(cmd *cobra.Command, flags map[string]string, interactive bool, kind, name string, fetch func(context.Context) (api.TemplateInputsResponse, error)) (map[string]string, error) {
	provided, err := loadTemplateInputValues(cmd, flags, nil)
	if err != nil {
		return nil, err
	}
	if !interactive {
		return provided.Explicit(), nil
	}
	response, err := fetch(cmd.Context())
	if err != nil {
		return nil, err
	}
	for key, value := range response.Recorded {
		if _, exists := provided.Values[key]; !exists {
			provided.Values[key] = value
			provided.Origins[key] = inputform.OriginRecorded
		}
	}
	inputs := append([]api.TemplateInputDescriptor(nil), response.Template.Inputs...)
	unrecorded := make(map[string]bool, len(response.Unrecorded))
	for _, name := range response.Unrecorded {
		unrecorded[name] = true
	}
	for i := range inputs {
		if inputs[i].Secret && unrecorded[inputs[i].Name] {
			inputs[i].Default = ""
			inputs[i].Help = strings.TrimSpace(inputs[i].Help + "\n(not recorded)")
		}
	}
	title := "Update " + kind
	if name != "" {
		title += " " + name
	}
	title += " from " + response.Template.Ref
	result, err := updateForm(cmd).run(cmd.Context(), inputform.Request{
		Title: title, Confirm: "Re-render the " + kind + "?", Inputs: inputs,
		Provided: provided.Values, Origins: provided.Origins, Mode: inputform.ModeInteractive,
		In: cmd.InOrStdin(), Out: cmd.OutOrStdout(), Err: cmd.ErrOrStderr(),
	})
	if err != nil {
		return nil, err
	}
	explicit := result.Explicit()
	if len(explicit) == 0 {
		// Keep the form's commentary out of machine-readable update results.
		if _, err := fmt.Fprintln(cmd.ErrOrStderr(), "no input changes"); err != nil {
			return nil, err
		}
	}
	return explicit, nil
}

func updateWorkspaceFromTemplate(cmd *cobra.Command, platform service.API, name string, req api.WorkspaceUpdateRequest, interactive bool) (api.WorkspaceRef, error) {
	inputs, err := resolveUpdateTemplateInputs(cmd, req.Inputs, interactive, "workspace", name,
		func(ctx context.Context) (api.TemplateInputsResponse, error) {
			return platform.WorkspaceTemplateInputs(ctx, name)
		})
	if err != nil {
		return api.WorkspaceRef{}, err
	}
	req.Inputs = inputs
	return platform.WorkspaceUpdate(cmd.Context(), name, req)
}
