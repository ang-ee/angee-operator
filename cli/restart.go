package cli

import (
	"fmt"

	"github.com/fyltr/angee/api"
	"github.com/spf13/cobra"
)

var restartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart all services and agents",
	Long: `Stop and restart all services and agents. Recompiles angee.yaml,
stops the stack, then starts it fresh. Containers using updated images
(from 'angee pull') will be recreated automatically.

Usage:
  angee restart
  angee pull && angee restart`,
	RunE: runRestart,
}

func runRestart(cmd *cobra.Command, args []string) error {
	fmt.Printf("\n\033[1mangee restart\033[0m\n\n")

	printInfo("Stopping stack...")
	var down api.DownResponse
	if _, err := apiPost("/down", nil, &down); err != nil {
		return fmt.Errorf("stopping stack: %w", err)
	}
	printSuccess("Stack stopped")

	printInfo("Starting stack...")
	var deploy api.ApplyResult
	if _, err := apiPost("/deploy", api.DeployRequest{}, &deploy); err != nil {
		return fmt.Errorf("starting stack: %w", err)
	}
	printSuccess("Stack restarted")

	fmt.Println()
	return nil
}
