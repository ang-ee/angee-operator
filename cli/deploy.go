package cli

import (
	"fmt"

	"github.com/fyltr/angee/api"
	"github.com/spf13/cobra"
)

var deployMessage string

var deployCmd = &cobra.Command{
	Use:   "deploy",
	Short: "Deploy the current angee.yaml to the runtime",
	Long: `Compile angee.yaml and apply it to the running platform.

This is equivalent to asking the admin agent to deploy, but bypasses
the agent for direct operator interaction.`,
	RunE: runDeploy,
}

func init() {
	deployCmd.Flags().StringVarP(&deployMessage, "message", "m", "", "Commit message for this deploy")
}

func runDeploy(cmd *cobra.Command, args []string) error {
	var result api.ApplyResult
	if _, err := apiPost("/deploy", api.DeployRequest{Message: deployMessage}, &result); err != nil {
		return fmt.Errorf("deploying: %w", err)
	}

	if outputJSON {
		return nil // already printed by apiPost
	}

	printSuccess("Deployed")
	if len(result.ServicesStarted) > 0 {
		printInfo(fmt.Sprintf("Started: %v", result.ServicesStarted))
	}
	if len(result.ServicesUpdated) > 0 {
		printInfo(fmt.Sprintf("Updated: %v", result.ServicesUpdated))
	}
	if len(result.ServicesRemoved) > 0 {
		printInfo(fmt.Sprintf("Removed: %v", result.ServicesRemoved))
	}
	return nil
}
