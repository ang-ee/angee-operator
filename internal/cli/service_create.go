package cli

import (
	"fmt"
	"io"

	"github.com/ang-ee/angee-operator/api"
	"github.com/spf13/cobra"
)

func serviceCreateCommand(stdout io.Writer, root, operatorURL *string, jsonOutput *bool) *cobra.Command {
	var req api.ServiceCreateRequest
	var inputValues []string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Render a service from a template into the stack",
		Long: `Render a Copier template with _angee.kind: service into the outer
stack as a single service entry, bound to a workspace.

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
			state, err := platform.ServiceCreate(cmd.Context(), req)
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
	cmd.Flags().StringVar(&req.Name, "name", "", "override resolved service name")
	cmd.Flags().BoolVar(&req.Start, "start", false, "start the service after create")
	_ = cmd.MarkFlagRequired("template")
	_ = cmd.MarkFlagRequired("workspace")
	return cmd
}
