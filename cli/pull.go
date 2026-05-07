package cli

import (
	"fmt"

	"github.com/fyltr/angee/api"
	"github.com/spf13/cobra"
)

var pullCmd = &cobra.Command{
	Use:   "pull",
	Short: "Pull latest container images",
	Long: `Pull the latest container images for all services and agents defined in
angee.yaml. Does NOT restart — run 'angee restart' after pulling to apply.

Usage:
  angee pull              Pull latest images
  angee pull && angee restart`,
	RunE: runPull,
}

func runPull(cmd *cobra.Command, args []string) error {
	fmt.Printf("\n\033[1mangee pull\033[0m\n\n")

	printInfo("Pulling latest images...")
	var result api.ProvisionResponse
	if _, err := apiPost("/pull", nil, &result); err != nil {
		return fmt.Errorf("pulling images: %w", err)
	}
	printSuccess("Pulled latest images")

	fmt.Println()
	printInfo("Run 'angee restart' to apply updated images")
	fmt.Println()
	return nil
}
