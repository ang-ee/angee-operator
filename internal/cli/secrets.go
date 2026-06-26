package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ang-ee/angee-operator/internal/query"
	"github.com/spf13/cobra"
)

func secretCommand(stdout, stderr io.Writer, root, operatorURL *string, jsonOutput *bool) *cobra.Command {
	cmd := &cobra.Command{Use: "secret", Short: "Manage secrets"}
	cmd.AddCommand(secretListCommand(stdout, root, operatorURL, jsonOutput))
	cmd.AddCommand(secretGetCommand(stdout, root, operatorURL, jsonOutput))
	cmd.AddCommand(secretRevealCommand(stdout, root, operatorURL))
	cmd.AddCommand(secretSetCommand(stdout, stderr, root, operatorURL))
	cmd.AddCommand(secretDeleteCommand(stdout, root, operatorURL))
	return cmd
}

func secretListCommand(stdout io.Writer, root, operatorURL *string, jsonOutput *bool) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List declared secrets",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			refs, _, err := platform.SecretsList(cmd.Context(), query.Args{})
			if err != nil {
				return err
			}
			if *jsonOutput {
				return writeJSON(stdout, refs)
			}
			for _, ref := range refs {
				marker := "-"
				if ref.HasValue {
					marker = "set"
				}
				if _, err := fmt.Fprintf(stdout, "%s\t%s\t%s\n", ref.Name, marker, ref.EnvVar); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func secretGetCommand(stdout io.Writer, root, operatorURL *string, jsonOutput *bool) *cobra.Command {
	return &cobra.Command{
		Use:   "get <name>",
		Short: "Show secret metadata (never the value)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			ref, err := platform.SecretGet(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if *jsonOutput {
				return writeJSON(stdout, ref)
			}
			rows := [][2]string{
				{"name", ref.Name},
				{"declared", boolStr(ref.Declared)},
				{"has_value", boolStr(ref.HasValue)},
				{"env_var", ref.EnvVar},
			}
			if ref.Required {
				rows = append(rows, [2]string{"required", "true"})
			}
			if ref.Generated {
				rows = append(rows, [2]string{"generated", "true"})
			}
			if ref.Import != "" {
				rows = append(rows, [2]string{"import", ref.Import})
			}
			for _, row := range rows {
				if _, err := fmt.Fprintf(stdout, "%s\t%s\n", row[0], row[1]); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func secretRevealCommand(stdout io.Writer, root, operatorURL *string) *cobra.Command {
	return &cobra.Command{
		Use:   "reveal <name>",
		Short: "Print the secret value (privileged read)",
		Long: `Prints the secret value to stdout, followed by a trailing newline.
Use sparingly; logs and process listings may capture the value. When
piping into commands that don't strip newlines (e.g. building an
Authorization header), trim explicitly:

    angee secret reveal token | tr -d '\n'`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			resp, err := platform.SecretValue(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(stdout, resp.Value)
			return err
		},
	}
}

func secretSetCommand(stdout, stderr io.Writer, root, operatorURL *string) *cobra.Command {
	var value string
	var fromStdin bool
	cmd := &cobra.Command{
		Use:   "set <name>",
		Short: "Set a secret value",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if fromStdin {
				data, err := io.ReadAll(bufio.NewReader(os.Stdin))
				if err != nil {
					return fmt.Errorf("read stdin: %w", err)
				}
				value = strings.TrimRight(string(data), "\n")
			}
			if value == "" {
				return fmt.Errorf("value is empty; pass --value or --stdin")
			}
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			ref, err := platform.SecretSet(cmd.Context(), name, value)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(stdout, "secret %s set (declared=%v env=%s)\n", ref.Name, ref.Declared, ref.EnvVar)
			return err
		},
	}
	cmd.Flags().StringVar(&value, "value", "", "literal value (INSECURE: visible in ps/proc; prefer --stdin)")
	cmd.Flags().BoolVar(&fromStdin, "stdin", false, "read value from stdin (overrides --value)")
	return cmd
}

func secretDeleteCommand(stdout io.Writer, root, operatorURL *string) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <name>",
		Short: "Remove a secret value from the backend",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			if err := platform.SecretDelete(cmd.Context(), args[0]); err != nil {
				return err
			}
			_, err = fmt.Fprintf(stdout, "secret %s deleted\n", args[0])
			return err
		},
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
