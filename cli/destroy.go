package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

var destroyForce bool

var destroyCmd = &cobra.Command{
	Use:   "destroy",
	Short: "Stop all services and remove ANGEE_ROOT",
	Long: `Bring down all containers and optionally remove the ANGEE_ROOT directory.
This is destructive — all agent workspaces and local config will be deleted.

Examples:
  angee destroy           # stop stack, prompt before removing
  angee destroy --force   # stop stack and remove without prompting`,
	RunE: runDestroy,
}

func init() {
	destroyCmd.Flags().BoolVar(&destroyForce, "force", false, "Skip confirmation prompt")
}

func runDestroy(cmd *cobra.Command, args []string) error {
	path := resolveRoot()

	if err := safeToRemove(path); err != nil {
		return err
	}

	fmt.Printf("\n\033[1mangee destroy\033[0m\n\n")

	printInfo("Stopping stack...")
	if _, err := apiPost("/down", nil, nil); err != nil {
		fmt.Printf("  \033[33m!\033[0m  Could not stop stack (it may not be running): %s\n", err)
	} else {
		printSuccess("Stack stopped")
	}

	if !destroyForce {
		fmt.Printf("\n  Remove ANGEE_ROOT at %s? [y/N] ", path)
		var answer string
		_, _ = fmt.Scanln(&answer)
		if answer != "y" && answer != "Y" {
			printInfo("Aborted. Stack is stopped but ANGEE_ROOT is preserved.")
			return nil
		}
	}

	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("removing ANGEE_ROOT: %w", err)
	}

	printSuccess(fmt.Sprintf("Removed %s", path))
	return nil
}

// safeToRemove blocks `destroy` from nuking obviously-wrong directories.
// We require an angee.yaml inside the resolved path AND refuse "/", $HOME
// and a missing path. Hardens the --force path against typo-CWD disasters.
func safeToRemove(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolving path %s: %w", path, err)
	}
	clean := filepath.Clean(abs)
	if clean == "/" || clean == "." {
		return fmt.Errorf("refusing to remove root path %q", clean)
	}
	if home, err := os.UserHomeDir(); err == nil && filepath.Clean(home) == clean {
		return fmt.Errorf("refusing to remove your home directory %q", clean)
	}
	if _, err := os.Stat(filepath.Join(clean, "angee.yaml")); err != nil {
		return fmt.Errorf(
			"refusing to remove %s: no angee.yaml found there (is this an ANGEE_ROOT?)",
			clean,
		)
	}
	return nil
}
