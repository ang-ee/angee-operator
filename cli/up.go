package cli

import (
	"fmt"

	"github.com/fyltr/angee/api"
	"github.com/spf13/cobra"
)

var upCmd = &cobra.Command{
	Use:   "up",
	Short: "Start the Angee platform",
	Long: `Ask the operator to reconcile angee.yaml through the selected backend.
Changed services are automatically recreated by the backend when needed.

Example:
  angee up`,
	RunE: runUp,
}

func runUp(cmd *cobra.Command, args []string) error {
	fmt.Printf("\n\033[1mangee up\033[0m\n\n")

	printInfo("Starting stack...")
	var result api.ApplyResult
	if _, err := apiPost("/deploy", api.DeployRequest{}, &result); err != nil {
		return fmt.Errorf("starting stack: %w", err)
	}
	printSuccess("Stack started")

	printPlatformReady()
	return nil
}

func printPlatformReady() {
	fmt.Printf("\n\033[1mPlatform ready:\033[0m\n\n")
	printInfo("UI        →  \033[4mhttp://localhost:3333\033[0m")
	printInfo("API       →  \033[4mhttp://localhost:8000/api\033[0m")
	fmt.Println()
	printInfo("angee ls          View agents and services")
	printInfo("angee agent list  View declared agents")
	fmt.Println()
}
