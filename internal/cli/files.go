package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

func fileCommand(stdout, stderr io.Writer, root, operatorURL *string, jsonOutput *bool) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "file",
		Short: "Read and write files under a stack source",
		Long: "Read and write raw UTF-8 text files located under a declared stack source's\n" +
			"root. The <path> is relative to the source root and may contain slashes; the\n" +
			"source is selected with --source.",
		Example: "  angee file get config/app.yaml --source app\n" +
			"  angee file set config/app.yaml --source app --stdin",
	}
	cmd.AddCommand(fileGetCommand(stdout, root, operatorURL, jsonOutput))
	cmd.AddCommand(fileSetCommand(stdout, stderr, root, operatorURL, jsonOutput))
	return cmd
}

func fileGetCommand(stdout io.Writer, root, operatorURL *string, jsonOutput *bool) *cobra.Command {
	var source string
	cmd := &cobra.Command{
		Use:     "get <path>",
		Short:   "Read a file under a stack source",
		Long:    "Print the raw contents of <path> (relative to the source root) to stdout.\nWith --json, emit the path, source, content, and etag as a JSON object.",
		Example: "  angee file get config/app.yaml --source app",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if source == "" {
				return fmt.Errorf("source is empty; pass --source")
			}
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			content, err := platform.FileRead(cmd.Context(), source, args[0])
			if err != nil {
				return err
			}
			if *jsonOutput {
				return writeJSON(stdout, content)
			}
			_, err = fmt.Fprint(stdout, content.Content)
			return err
		},
	}
	cmd.Flags().StringVar(&source, "source", "", "stack source name (required)")
	return cmd
}

func fileSetCommand(stdout, stderr io.Writer, root, operatorURL *string, jsonOutput *bool) *cobra.Command {
	var source string
	var content string
	var fromFile string
	var fromStdin bool
	var etag string
	cmd := &cobra.Command{
		Use:   "set <path>",
		Short: "Write a file under a stack source",
		Long: "Write content to <path> (relative to the source root). Content comes from\n" +
			"--content, --file, or --stdin. Pass --etag for an optimistic-concurrency\n" +
			"precondition; a mismatch is reported as a conflict. With --json, emit the\n" +
			"resulting path, source, and etag as a JSON object.",
		Example: "  angee file set config/app.yaml --source app --stdin\n" +
			"  angee file set config/app.yaml --source app --content 'key: value'",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if source == "" {
				return fmt.Errorf("source is empty; pass --source")
			}
			switch {
			case fromStdin:
				data, err := io.ReadAll(bufio.NewReader(os.Stdin))
				if err != nil {
					return fmt.Errorf("read stdin: %w", err)
				}
				content = string(data)
			case fromFile != "":
				data, err := os.ReadFile(fromFile)
				if err != nil {
					return fmt.Errorf("read %s: %w", fromFile, err)
				}
				content = string(data)
			case !cmd.Flags().Changed("content"):
				return fmt.Errorf("no content; pass --content, --file, or --stdin")
			}
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			ref, err := platform.FileWrite(cmd.Context(), source, args[0], content, etag)
			if err != nil {
				return err
			}
			if *jsonOutput {
				return writeJSON(stdout, ref)
			}
			_, err = fmt.Fprintf(stdout, "file %s set (source=%s etag=%s)\n", ref.Path, ref.Source, ref.Etag)
			return err
		},
	}
	cmd.Flags().StringVar(&source, "source", "", "stack source name (required)")
	cmd.Flags().StringVar(&content, "content", "", "literal file content")
	cmd.Flags().StringVar(&fromFile, "file", "", "read content from a file path")
	cmd.Flags().BoolVar(&fromStdin, "stdin", false, "read content from stdin (overrides --content/--file)")
	cmd.Flags().StringVar(&etag, "etag", "", "compare-and-set precondition; a mismatch is a conflict")
	return cmd
}
