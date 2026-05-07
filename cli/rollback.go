package cli

import (
	"fmt"

	"github.com/fyltr/angee/api"
	"github.com/spf13/cobra"
)

var rollbackCmd = &cobra.Command{
	Use:   "rollback [sha|HEAD~N]",
	Short: "Roll back to a previous deployment",
	Long: `Roll back ANGEE_ROOT to a previous git commit and redeploy.

Examples:
  angee rollback HEAD~1         # previous deploy
  angee rollback abc123def      # specific commit SHA`,
	Args: cobra.ExactArgs(1),
	RunE: runRollback,
}

func runRollback(cmd *cobra.Command, args []string) error {
	var result api.RollbackResponse
	if _, err := apiPost("/rollback", api.RollbackRequest{SHA: args[0]}, &result); err != nil {
		return fmt.Errorf("rollback: %w", err)
	}

	if outputJSON {
		return nil
	}

	printSuccess(fmt.Sprintf("Rolled back to %s", result.RolledBackTo))
	return nil
}
