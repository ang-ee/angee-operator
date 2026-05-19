package service

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"github.com/fyltr/angee/api"
)

// WorkspaceSourceMerge merges `ref` into the workspace source slot's
// current branch with `--no-ff` (mirrors the safe-default a human would
// pick). On conflict the worktree is left in conflicted state for the
// caller to resolve and the response carries the conflicting paths.
func (p *Platform) WorkspaceSourceMerge(ctx context.Context, workspace, slot, ref string) (api.GitOpResult, error) {
	if strings.TrimSpace(ref) == "" {
		return api.GitOpResult{}, &InvalidInputError{Field: "ref", Reason: "merge ref is required"}
	}
	return p.runWorkspaceGitOp(ctx, workspace, slot, "merge", "--no-ff", "--no-edit", ref)
}

// WorkspaceSourceRebase rebases the current branch onto `ref`. On conflict
// the worktree stays in the rebasing state; callers must
// `workspaceSourceRebaseContinue` or `workspaceSourceRebaseAbort`.
func (p *Platform) WorkspaceSourceRebase(ctx context.Context, workspace, slot, ref string) (api.GitOpResult, error) {
	if strings.TrimSpace(ref) == "" {
		return api.GitOpResult{}, &InvalidInputError{Field: "ref", Reason: "rebase ref is required"}
	}
	return p.runWorkspaceGitOp(ctx, workspace, slot, "rebase", ref)
}

// WorkspaceSourceMergeAbort aborts an in-progress merge.
func (p *Platform) WorkspaceSourceMergeAbort(ctx context.Context, workspace, slot string) (api.GitOpResult, error) {
	return p.runWorkspaceGitOp(ctx, workspace, slot, "merge", "--abort")
}

// WorkspaceSourceRebaseAbort aborts an in-progress rebase.
func (p *Platform) WorkspaceSourceRebaseAbort(ctx context.Context, workspace, slot string) (api.GitOpResult, error) {
	return p.runWorkspaceGitOp(ctx, workspace, slot, "rebase", "--abort")
}

// WorkspaceSourceRebaseContinue continues an in-progress rebase after the
// caller has resolved conflicts in the worktree.
func (p *Platform) WorkspaceSourceRebaseContinue(ctx context.Context, workspace, slot string) (api.GitOpResult, error) {
	return p.runWorkspaceGitOp(ctx, workspace, slot, "-c", "core.editor=true", "rebase", "--continue")
}

// WorkspaceSourcePublish pushes the worktree's branch to the named remote
// (default "origin") under the named branch (default: the current branch),
// setting upstream tracking if not already configured. Useful for
// publishing a workspace branch for the first time so an external
// reviewer can open a PR against it.
func (p *Platform) WorkspaceSourcePublish(ctx context.Context, workspace, slot, remote, branch string) (api.GitOpResult, error) {
	stack, wsSource, _, path, err := p.workspaceSourceTarget(ctx, workspace, slot)
	if err != nil {
		return api.GitOpResult{}, err
	}
	_ = stack
	if remote == "" {
		remote = "origin"
	}
	if branch == "" {
		branch = wsSource.Branch
	}
	if branch == "" {
		// Fall back to whatever HEAD currently points at.
		out, err := runGitCapture(ctx, path, "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil {
			return api.GitOpResult{}, fmt.Errorf("resolve HEAD branch in %s: %w", path, err)
		}
		branch = strings.TrimSpace(out)
	}
	return runGitOpAt(ctx, path, "push", "--set-upstream", remote, branch)
}

func (p *Platform) runWorkspaceGitOp(ctx context.Context, workspace, slot string, args ...string) (api.GitOpResult, error) {
	_, _, _, path, err := p.workspaceSourceTarget(ctx, workspace, slot)
	if err != nil {
		return api.GitOpResult{}, err
	}
	return runGitOpAt(ctx, path, args...)
}

func runGitOpAt(ctx context.Context, workdir string, args ...string) (api.GitOpResult, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = workdir
	cmd.Env = gitOpEnv()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	runErr := cmd.Run()
	combined := strings.TrimSpace(stdout.String() + "\n" + stderr.String())

	result := api.GitOpResult{
		Message:       combined,
		ConflictFiles: []string{},
	}
	if runErr == nil {
		result.OK = true
		return result, nil
	}
	// On failure, enumerate conflicted paths via `git ls-files -u`. This is
	// safe even when no conflict is in flight (returns an empty set).
	conflictOut, _ := runGitCapture(ctx, workdir, "ls-files", "-u")
	files := parseConflictedPaths(conflictOut)
	result.ConflictFiles = files
	result.Conflicted = len(files) > 0
	if result.Conflicted {
		return result, nil
	}
	// Non-conflict failure surfaces as a typed error so callers can
	// distinguish "merge produced conflicts" (handled) from "git refused
	// to start the merge at all" (unexpected).
	return result, fmt.Errorf("git %s in %s: %w", strings.Join(args, " "), workdir, runErr)
}

func runGitCapture(ctx context.Context, workdir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = workdir
	cmd.Env = gitOpEnv()
	out, err := cmd.Output()
	return string(out), err
}

func parseConflictedPaths(lsFilesOutput string) []string {
	if strings.TrimSpace(lsFilesOutput) == "" {
		return []string{}
	}
	seen := map[string]struct{}{}
	for line := range strings.SplitSeq(lsFilesOutput, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// `git ls-files -u` emits `<mode> <sha> <stage>\t<path>` per stage entry.
		tab := strings.IndexByte(line, '\t')
		if tab < 0 {
			continue
		}
		seen[strings.TrimSpace(line[tab+1:])] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func gitOpEnv() []string {
	// Avoid commit-time prompts during merge --no-edit and rebase --continue;
	// pin a deterministic identity for the merge/rebase commit so the
	// operator never has to depend on the host's git config.
	return []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=angee",
		"GIT_AUTHOR_EMAIL=angee@example.invalid",
		"GIT_COMMITTER_NAME=angee",
		"GIT_COMMITTER_EMAIL=angee@example.invalid",
	}
}
