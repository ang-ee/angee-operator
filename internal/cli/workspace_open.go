package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/spf13/cobra"
)

// editorChoice names a supported editor opener.
type editorChoice string

const (
	editorVSCode    editorChoice = "vscode"
	editorIDEA      editorChoice = "idea"
	editorGHDesktop editorChoice = "gh-desktop"
)

// validateEditorChoice rejects values that aren't a known editorChoice.
func validateEditorChoice(s string) error {
	switch editorChoice(s) {
	case editorVSCode, editorIDEA, editorGHDesktop:
		return nil
	default:
		return fmt.Errorf("--with: unknown editor %q (want vscode | idea | gh-desktop)", s)
	}
}

// openCommands returns the shell command(s) that open the workspace in the chosen editor.
//
//   - workspacePath:      absolute path to the workspace dir (e.g. <host>/.angee/workspaces/<name>)
//   - codeWorkspaceFile:  absolute path to <name>.code-workspace if it exists, "" otherwise
//   - repos:              names of materialised worktree subdirs under workspacePath (each contains .git)
//   - goos:               runtime.GOOS at decision time (parameterised for testability)
//
// VSCode prefers the .code-workspace file (multi-root). If absent, it falls back to opening the
// workspace directory and lets VS Code's "Open Folder" + git.openRepositoryInParentFolders pick
// up each child repo.
//
// IDEA opens the workspace directory; JetBrains IDEs auto-detect per-folder Git roots.
//
// GitHub Desktop has no concept of a multi-root workspace, so we register each materialised
// worktree as its own entry. Currently macOS-only via `open -a "GitHub Desktop"` — Linux and
// Windows would need different invocations.
func openCommands(editor editorChoice, workspacePath, codeWorkspaceFile string, repos []string, goos string) ([][]string, error) {
	switch editor {
	case editorVSCode:
		target := codeWorkspaceFile
		if target == "" {
			target = workspacePath
		}
		return [][]string{{"code", target}}, nil
	case editorIDEA:
		return [][]string{{"idea", workspacePath}}, nil
	case editorGHDesktop:
		if goos != "darwin" {
			return nil, fmt.Errorf("--with=gh-desktop is currently macOS-only (got GOOS=%s)", goos)
		}
		if len(repos) == 0 {
			return nil, fmt.Errorf("no materialised worktrees under %s", workspacePath)
		}
		cmds := make([][]string, 0, len(repos))
		for _, repo := range repos {
			cmds = append(cmds, []string{"open", "-a", "GitHub Desktop", filepath.Join(workspacePath, repo)})
		}
		return cmds, nil
	default:
		return nil, fmt.Errorf("unknown editor %q (want vscode|idea|gh-desktop)", editor)
	}
}

// materialisedWorktrees returns the names of immediate subdirs of workspacePath that look
// like git worktrees (each contains a .git file or directory). Symlinks are followed.
// Result is sorted for deterministic output.
func materialisedWorktrees(workspacePath string) ([]string, error) {
	entries, err := os.ReadDir(workspacePath)
	if err != nil {
		return nil, fmt.Errorf("read workspace dir: %w", err)
	}
	var repos []string
	for _, e := range entries {
		// resolve symlinks; a sibling like django-zed-rebac may be a symlink to a real dir
		full := filepath.Join(workspacePath, e.Name())
		info, err := os.Stat(full)
		if err != nil || !info.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(full, ".git")); err != nil {
			continue
		}
		repos = append(repos, e.Name())
	}
	slices.Sort(repos)
	return repos, nil
}

func ensureWorkspaceOpenLocal(operatorURL *string) error {
	if operatorURL != nil && *operatorURL != "" {
		return fmt.Errorf("workspace open requires a local ANGEE_ROOT; remote operator mode via --operator/ANGEE_OPERATOR_URL is not supported")
	}
	return nil
}

func startDetachedCommand(c []string) error {
	proc := exec.Command(c[0], c[1:]...)
	if err := proc.Start(); err != nil {
		return err
	}
	_ = proc.Process.Release()
	return nil
}

func launchOpenCommands(
	cmds [][]string,
	stderr io.Writer,
	lookPath func(string) (string, error),
	start func([]string) error,
) error {
	launched := 0
	for _, c := range cmds {
		if _, err := lookPath(c[0]); err != nil {
			fmt.Fprintf(stderr, "binary %q not found on PATH; skip: %s\n", c[0], strings.Join(c, " "))
			continue
		}
		if err := start(c); err != nil {
			return fmt.Errorf("launch %s: %w", strings.Join(c, " "), err)
		}
		launched++
	}
	if launched == 0 {
		return fmt.Errorf("no editor command launched; no requested editor binaries were found on PATH")
	}
	return nil
}

func workspaceOpenCommand(stdout io.Writer, root, operatorURL *string) *cobra.Command {
	var editor string
	var printOnly bool
	cmd := &cobra.Command{
		Use:   "open <name>",
		Short: "Open a workspace in your editor (vscode | idea | gh-desktop)",
		Long: "Opens the workspace's <name>.code-workspace file (multi-root view of all materialised\n" +
			"worktrees) in the chosen editor. Falls back to opening the workspace directory if the\n" +
			"file is absent (older workspaces created before the template emitted it).\n" +
			"\n" +
			"Examples:\n" +
			"  angee workspace open mypr\n" +
			"  angee workspace open mypr --with idea\n" +
			"  angee workspace open mypr --with gh-desktop --print",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			stderr := cmd.ErrOrStderr()
			// Validate --with up-front so a typo doesn't trigger a platform RPC.
			if err := validateEditorChoice(editor); err != nil {
				return err
			}
			if err := ensureWorkspaceOpenLocal(operatorURL); err != nil {
				return err
			}
			platform, err := localPlatform(root, operatorURL)
			if err != nil {
				return err
			}
			ref, err := platform.WorkspaceGet(cmd.Context(), args[0])
			if err != nil {
				return err
			}

			cwFile := filepath.Join(ref.Path, ref.Name+".code-workspace")
			if _, err := os.Stat(cwFile); err != nil {
				cwFile = ""
			}

			repos, err := materialisedWorktrees(ref.Path)
			if err != nil {
				return err
			}

			cmds, err := openCommands(editorChoice(editor), ref.Path, cwFile, repos, runtime.GOOS)
			if err != nil {
				return err
			}

			if printOnly {
				for _, c := range cmds {
					if _, err := fmt.Fprintln(stdout, strings.Join(c, " ")); err != nil {
						return err
					}
				}
				return nil
			}

			return launchOpenCommands(cmds, stderr, exec.LookPath, startDetachedCommand)
		},
	}
	cmd.Flags().StringVar(&editor, "with", "vscode", "editor: vscode | idea | gh-desktop")
	cmd.Flags().BoolVar(&printOnly, "print", false, "print the command(s) that would run, don't execute")
	return cmd
}
