package cli

import (
	"fmt"

	"github.com/fyltr/angee/api"
	"github.com/spf13/cobra"
)

var planCmd = &cobra.Command{
	Use:   "plan",
	Short: "Preview what 'angee deploy' would change",
	Long:  `Show a diff of what would change if you ran 'angee deploy' right now.`,
	RunE:  runPlan,
}

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show detailed platform status",
	RunE:  runStatus,
}

var downCmd = &cobra.Command{
	Use:   "down",
	Short: "Stop all services and agents",
	Long: `Stop and remove all containers in the stack.

Example:
  angee down`,
	RunE: runDown,
}

func runPlan(cmd *cobra.Command, args []string) error {
	var changeset api.ChangeSet
	if _, err := apiGet("/plan", &changeset); err != nil {
		return err
	}
	if outputJSON {
		return nil
	}

	total := len(changeset.Add) + len(changeset.Update) + len(changeset.Remove)
	if total == 0 {
		fmt.Println("\n  No changes. Platform is up to date.")
		return nil
	}

	fmt.Println()
	for _, s := range changeset.Add {
		fmt.Printf("  \033[32m+ %s\033[0m\n", s)
	}
	for _, s := range changeset.Update {
		fmt.Printf("  \033[33m~ %s\033[0m\n", s)
	}
	for _, s := range changeset.Remove {
		fmt.Printf("  \033[31m- %s\033[0m\n", s)
	}
	fmt.Printf("\n  %d change(s). Run 'angee deploy' to apply.\n\n", total)
	return nil
}

func runStatus(cmd *cobra.Command, args []string) error {
	return runLs(cmd, args)
}

func runDown(cmd *cobra.Command, args []string) error {
	fmt.Printf("\n\033[1mangee down\033[0m\n\n")
	var result api.DownResponse
	if _, err := apiPost("/down", nil, &result); err != nil {
		return fmt.Errorf("stopping stack: %w", err)
	}
	printSuccess("Stack stopped")
	return nil
}
