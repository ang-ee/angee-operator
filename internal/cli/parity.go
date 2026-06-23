package cli

// CLI parity for the operations that previously lived only on the REST
// + GraphQL surfaces. Each command in this file dispatches through the
// shared service.API (local Platform or remote operator), matching
// the same routing convention as the older subcommands.

import (
	"context"
	"fmt"
	"io"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/platformclient"
	"github.com/ang-ee/angee-operator/internal/query"
	"github.com/ang-ee/angee-operator/internal/service"
	"github.com/spf13/cobra"
)

// gitopsCommand bundles read-only GitOps views: the cross-source ×
// workspace-slot topology snapshot.
func gitopsCommand(stdout io.Writer, root, operatorURL *string, jsonOutput *bool) *cobra.Command {
	cmd := &cobra.Command{Use: "gitops", Short: "Inspect git-source / workspace-slot topology"}
	cmd.AddCommand(gitopsTopologyCommand(stdout, root, operatorURL, jsonOutput))
	return cmd
}

func gitopsTopologyCommand(stdout io.Writer, root, operatorURL *string, jsonOutput *bool) *cobra.Command {
	var withCommits int
	cmd := &cobra.Command{
		Use:   "topology",
		Short: "Print the GitOps topology snapshot",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			topo, err := platform.GitOpsTopologyWithCommits(cmd.Context(), withCommits)
			if err != nil {
				return err
			}
			if *jsonOutput {
				return writeJSON(stdout, topo)
			}
			_, err = fmt.Fprintf(stdout, "stack=%s sources=%d workspaces=%d clean=%d dirty=%d ahead=%d behind=%d diverged=%d\n",
				topo.Name, topo.Summary.Sources, topo.Summary.Workspaces,
				topo.Summary.Clean, topo.Summary.Dirty, topo.Summary.Ahead, topo.Summary.Behind, topo.Summary.Diverged,
			)
			return err
		},
	}
	cmd.Flags().IntVar(&withCommits, "with-commits", 0, "include up to N recent commits per git source (0 = skip)")
	return cmd
}

// templateCommand exposes the template introspection queries
// (`templates` / `template(ref)`) over the CLI.
func templateCommand(stdout io.Writer, root, operatorURL *string, jsonOutput *bool) *cobra.Command {
	cmd := &cobra.Command{Use: "template", Short: "Inspect discoverable Copier templates"}
	cmd.AddCommand(templateListCommand(stdout, root, operatorURL, jsonOutput))
	cmd.AddCommand(templateGetCommand(stdout, root, operatorURL, jsonOutput))
	return cmd
}

func templateListCommand(stdout io.Writer, root, operatorURL *string, jsonOutput *bool) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List templates under <root>/(.)templates/<kind>/<name>",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			descs, _, err := platform.Templates(cmd.Context(), query.Args{})
			if err != nil {
				return err
			}
			if *jsonOutput {
				return writeJSON(stdout, descs)
			}
			for _, d := range descs {
				if _, err := fmt.Fprintf(stdout, "%s\t%s\t%s\n", d.Ref, d.Kind, d.Path); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func templateGetCommand(stdout io.Writer, root, operatorURL *string, jsonOutput *bool) *cobra.Command {
	return &cobra.Command{
		Use:   "get <ref>",
		Short: "Show one template's descriptor + input schema",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			desc, err := platform.Template(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if *jsonOutput {
				return writeJSON(stdout, desc)
			}
			fmt.Fprintf(stdout, "ref\t%s\nkind\t%s\nname\t%s\npath\t%s\n", desc.Ref, desc.Kind, desc.Name, desc.Path)
			for _, in := range desc.Inputs {
				fmt.Fprintf(stdout, "input.%s\trequired=%v type=%s default=%q\n", in.Name, in.Required, in.Type, in.Default)
			}
			return nil
		},
	}
}

// tokenCommand exposes per-actor scoped JWT minting on the CLI.
func tokenCommand(stdout io.Writer, root, operatorURL *string, jsonOutput *bool) *cobra.Command {
	cmd := &cobra.Command{Use: "token", Short: "Manage operator connection tokens"}
	cmd.AddCommand(tokenMintCommand(stdout, root, operatorURL, jsonOutput))
	return cmd
}

func tokenMintCommand(stdout io.Writer, root, operatorURL *string, jsonOutput *bool) *cobra.Command {
	var ttl string
	cmd := &cobra.Command{
		Use:   "mint <actor>",
		Short: "Mint a connection token scoped to <actor>",
		Long: `Mints an HS256 JWT carrying sub=<actor> and signed by the operator's
JWT signing key. TTL defaults to 1h, capped at 24h. The minted token
is printed to stdout; capture it carefully.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Token minting is operator-package state, not a Platform
			// method — the CLI can only mint over the remote operator
			// (or by adopting the operator's signing material locally
			// via separate config). Route through the remote path
			// explicitly.
			if operatorURL == nil || *operatorURL == "" {
				return fmt.Errorf("token mint requires --operator URL")
			}
			remote := platformclient.New(*operatorURL)
			resp, err := remote.MintConnectionToken(cmd.Context(), api.MintConnectionTokenRequest{Actor: args[0], TTL: ttl})
			if err != nil {
				return err
			}
			if *jsonOutput {
				return writeJSON(stdout, resp)
			}
			_, err = fmt.Fprintln(stdout, resp.Token)
			return err
		},
	}
	cmd.Flags().StringVar(&ttl, "ttl", "", "duration (e.g. 30m, 2h); default 1h, cap 24h")
	return cmd
}

// sourceDiffCommand prints the unified diff for one top-level source.
func sourceDiffCommand(stdout io.Writer, root, operatorURL *string, jsonOutput *bool) *cobra.Command {
	var ref string
	cmd := &cobra.Command{
		Use:   "diff <name>",
		Short: "Show the unified diff for one git source",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			files, err := platform.SourceDiff(cmd.Context(), args[0], ref)
			if err != nil {
				return err
			}
			if *jsonOutput {
				return writeJSON(stdout, files)
			}
			return renderDiff(stdout, files)
		},
	}
	cmd.Flags().StringVar(&ref, "ref", "", "diff target ref (empty = working tree vs HEAD)")
	return cmd
}

// workspacePreflightCommand validates a WorkspaceCreateRequest against
// the resolved template's input declarations without materialising.
func workspacePreflightCommand(stdout io.Writer, root, operatorURL *string, jsonOutput *bool) *cobra.Command {
	var req api.WorkspaceCreateRequest
	var inputValues []string
	cmd := &cobra.Command{
		Use:   "preflight",
		Short: "Validate a workspace-create input set without rendering",
		Args:  cobra.NoArgs,
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
			resp, err := platform.WorkspaceCreatePreflight(cmd.Context(), req)
			if err != nil {
				return err
			}
			if *jsonOutput {
				return writeJSON(stdout, resp)
			}
			fmt.Fprintf(stdout, "ok\t%v\ntemplate\t%s\nresolved_template\t%s\n", resp.OK, resp.Template, resp.ResolvedTemplate)
			for _, name := range sortedStrings(resp.MissingRequired) {
				fmt.Fprintf(stdout, "missing\t%s\n", name)
			}
			for _, fail := range resp.InvalidInputs {
				fmt.Fprintf(stdout, "invalid\t%s\t%s\n", fail.Field, fail.Reason)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&req.Template, "template", "", "template ref or path (required)")
	cmd.Flags().StringVar(&req.Name, "name", "", "workspace name override")
	cmd.Flags().StringVar(&req.TTL, "ttl", "", "workspace TTL")
	cmd.Flags().StringArrayVar(&inputValues, "input", nil, "template input K=V (repeatable)")
	_ = cmd.MarkFlagRequired("template")
	return cmd
}

// workspaceSourceCommand bundles every per-workspace-source-slot
// operation under one subcommand tree.
func workspaceSourceCommand(stdout io.Writer, root, operatorURL *string, jsonOutput *bool) *cobra.Command {
	cmd := &cobra.Command{Use: "source", Short: "Per-workspace source-slot git operations"}
	cmd.AddCommand(slotStateCommand("fetch", "Fetch a workspace source slot from upstream", stdout, root, operatorURL, jsonOutput,
		func(p service.API, ctx context.Context, ws, slot string) (any, error) {
			return p.WorkspaceSourceFetch(ctx, ws, slot)
		}))
	cmd.AddCommand(slotStateCommand("pull", "Pull a workspace source slot from upstream (fast-forward)", stdout, root, operatorURL, jsonOutput,
		func(p service.API, ctx context.Context, ws, slot string) (any, error) {
			return p.WorkspaceSourcePull(ctx, ws, slot)
		}))
	cmd.AddCommand(workspaceSlotPushCommand(stdout, root, operatorURL, jsonOutput))
	cmd.AddCommand(workspaceSlotDiffCommand(stdout, root, operatorURL, jsonOutput))
	cmd.AddCommand(workspaceSlotMergeCommand(stdout, root, operatorURL, jsonOutput))
	cmd.AddCommand(workspaceSlotRebaseCommand(stdout, root, operatorURL, jsonOutput))
	cmd.AddCommand(slotGitOpCommand("merge-abort", "Abort an in-progress merge", stdout, root, operatorURL, jsonOutput,
		func(p service.API, ctx context.Context, ws, slot string) (api.GitOpResult, error) {
			return p.WorkspaceSourceMergeAbort(ctx, ws, slot)
		}))
	cmd.AddCommand(slotGitOpCommand("rebase-abort", "Abort an in-progress rebase", stdout, root, operatorURL, jsonOutput,
		func(p service.API, ctx context.Context, ws, slot string) (api.GitOpResult, error) {
			return p.WorkspaceSourceRebaseAbort(ctx, ws, slot)
		}))
	cmd.AddCommand(slotGitOpCommand("rebase-continue", "Continue an in-progress rebase after resolving conflicts", stdout, root, operatorURL, jsonOutput,
		func(p service.API, ctx context.Context, ws, slot string) (api.GitOpResult, error) {
			return p.WorkspaceSourceRebaseContinue(ctx, ws, slot)
		}))
	cmd.AddCommand(workspaceSlotPublishCommand(stdout, root, operatorURL, jsonOutput))
	return cmd
}

// slotStateCommand registers a `workspace source <op> <workspace> <slot>`
// subcommand that returns a WorkspaceSourceStatus. Used for fetch / pull.
func slotStateCommand(name, short string, stdout io.Writer, root, operatorURL *string, jsonOutput *bool, fn func(p service.API, ctx context.Context, ws, slot string) (any, error)) *cobra.Command {
	return &cobra.Command{
		Use:   name + " <workspace> <slot>",
		Short: short,
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			state, err := fn(platform, cmd.Context(), args[0], args[1])
			if err != nil {
				return err
			}
			if *jsonOutput {
				return writeJSON(stdout, state)
			}
			ws := state.(api.WorkspaceSourceStatus)
			_, err = fmt.Fprintf(stdout, "workspace=%s slot=%s state=%s branch=%s ahead=%d behind=%d dirty=%v\n",
				args[0], args[1], ws.State, ws.Branch, ws.Ahead, ws.Behind, ws.Dirty)
			return err
		},
	}
}

func workspaceSlotPushCommand(stdout io.Writer, root, operatorURL *string, jsonOutput *bool) *cobra.Command {
	var ref string
	cmd := &cobra.Command{
		Use:   "push <workspace> <slot>",
		Short: "Push a workspace source slot",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			state, err := platform.WorkspaceSourcePush(cmd.Context(), args[0], args[1], ref)
			if err != nil {
				return err
			}
			if *jsonOutput {
				return writeJSON(stdout, state)
			}
			_, err = fmt.Fprintf(stdout, "workspace=%s slot=%s pushed=%v state=%s\n", args[0], args[1], state.Pushed, state.State)
			return err
		},
	}
	cmd.Flags().StringVar(&ref, "ref", "", "remote ref (default: workspace branch)")
	return cmd
}

func workspaceSlotDiffCommand(stdout io.Writer, root, operatorURL *string, jsonOutput *bool) *cobra.Command {
	var ref string
	cmd := &cobra.Command{
		Use:   "diff <workspace> <slot>",
		Short: "Show the unified diff for one workspace source slot",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			files, err := platform.WorkspaceSourceDiff(cmd.Context(), args[0], args[1], ref)
			if err != nil {
				return err
			}
			if *jsonOutput {
				return writeJSON(stdout, files)
			}
			return renderDiff(stdout, files)
		},
	}
	cmd.Flags().StringVar(&ref, "ref", "", "diff target ref (empty = working tree vs HEAD)")
	return cmd
}

func workspaceSlotMergeCommand(stdout io.Writer, root, operatorURL *string, jsonOutput *bool) *cobra.Command {
	return &cobra.Command{
		Use:   "merge <workspace> <slot> <ref>",
		Short: "Merge <ref> into a workspace source slot",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			result, err := platform.WorkspaceSourceMerge(cmd.Context(), args[0], args[1], args[2])
			if err != nil {
				return err
			}
			return printGitOpResult(stdout, jsonOutput, result)
		},
	}
}

func workspaceSlotRebaseCommand(stdout io.Writer, root, operatorURL *string, jsonOutput *bool) *cobra.Command {
	return &cobra.Command{
		Use:   "rebase <workspace> <slot> <ref>",
		Short: "Rebase a workspace source slot onto <ref>",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			result, err := platform.WorkspaceSourceRebase(cmd.Context(), args[0], args[1], args[2])
			if err != nil {
				return err
			}
			return printGitOpResult(stdout, jsonOutput, result)
		},
	}
}

func workspaceSlotPublishCommand(stdout io.Writer, root, operatorURL *string, jsonOutput *bool) *cobra.Command {
	var remote, branch string
	cmd := &cobra.Command{
		Use:   "publish <workspace> <slot>",
		Short: "Push a workspace source slot to a remote (set upstream)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			result, err := platform.WorkspaceSourcePublish(cmd.Context(), args[0], args[1], remote, branch)
			if err != nil {
				return err
			}
			return printGitOpResult(stdout, jsonOutput, result)
		},
	}
	cmd.Flags().StringVar(&remote, "remote", "", "remote name (default: origin)")
	cmd.Flags().StringVar(&branch, "branch", "", "branch name (default: workspace source manifest branch)")
	return cmd
}

func slotGitOpCommand(name, short string, stdout io.Writer, root, operatorURL *string, jsonOutput *bool, fn func(p service.API, ctx context.Context, ws, slot string) (api.GitOpResult, error)) *cobra.Command {
	return &cobra.Command{
		Use:   name + " <workspace> <slot>",
		Short: short,
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			result, err := fn(platform, cmd.Context(), args[0], args[1])
			if err != nil {
				return err
			}
			return printGitOpResult(stdout, jsonOutput, result)
		},
	}
}

func printGitOpResult(stdout io.Writer, jsonOutput *bool, result api.GitOpResult) error {
	if *jsonOutput {
		return writeJSON(stdout, result)
	}
	if _, err := fmt.Fprintf(stdout, "ok=%v conflicted=%v\n", result.OK, result.Conflicted); err != nil {
		return err
	}
	for _, f := range result.ConflictFiles {
		if _, err := fmt.Fprintf(stdout, "conflict\t%s\n", f); err != nil {
			return err
		}
	}
	if result.Message != "" {
		_, err := fmt.Fprintln(stdout, result.Message)
		return err
	}
	return nil
}

func renderDiff(stdout io.Writer, files []api.DiffFile) error {
	for _, f := range files {
		header := f.NewPath
		if header == "" {
			header = f.OldPath
		}
		if _, err := fmt.Fprintf(stdout, "--- %s\n+++ %s\n", f.OldPath, header); err != nil {
			return err
		}
		for _, h := range f.Hunks {
			if _, err := fmt.Fprintf(stdout, "@@ -%d,%d +%d,%d @@ %s\n", h.OldStart, h.OldLines, h.NewStart, h.NewLines, h.Header); err != nil {
				return err
			}
			if _, err := io.WriteString(stdout, h.Body); err != nil {
				return err
			}
		}
	}
	return nil
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	// Simple insertion sort to avoid importing sort here.
	for i := 1; i < len(out); i++ {
		j := i
		for j > 0 && out[j-1] > out[j] {
			out[j-1], out[j] = out[j], out[j-1]
			j--
		}
	}
	return out
}
