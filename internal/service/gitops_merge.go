package service

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/git"
	"github.com/ang-ee/angee-operator/internal/logctx"
)

// WorkspaceSourceMerge merges `ref` into the workspace source slot's
// current branch with `--no-ff` (mirrors the safe-default a human would
// pick). On conflict the worktree is left in conflicted state for the
// caller to resolve and the response carries the conflicting paths.
func (p *Platform) WorkspaceSourceMerge(ctx context.Context, workspace, slot, ref string) (api.GitOpResult, error) {
	if strings.TrimSpace(ref) == "" {
		return api.GitOpResult{}, &InvalidInputError{Field: "ref", Reason: "merge ref is required"}
	}
	op := workspaceSlotOperation("workspace.source-merge", "Merge", workspace, slot)
	return p.runWorkspaceGitOp(ctx, op, gitStep{action: fmt.Sprintf("merging %s into", ref)}, workspace, slot, "merge", "--no-ff", "--no-edit", ref)
}

// WorkspaceSourceRebase rebases the current branch onto `ref`. On conflict
// the worktree stays in the rebasing state; callers must
// `workspaceSourceRebaseContinue` or `workspaceSourceRebaseAbort`.
func (p *Platform) WorkspaceSourceRebase(ctx context.Context, workspace, slot, ref string) (api.GitOpResult, error) {
	if strings.TrimSpace(ref) == "" {
		return api.GitOpResult{}, &InvalidInputError{Field: "ref", Reason: "rebase ref is required"}
	}
	op := workspaceSlotOperation("workspace.source-rebase", "Rebase", workspace, slot)
	return p.runWorkspaceGitOp(ctx, op, gitStep{action: fmt.Sprintf("rebasing onto %s", ref)}, workspace, slot, "rebase", ref)
}

// WorkspaceSourceMergeAbort aborts an in-progress merge.
func (p *Platform) WorkspaceSourceMergeAbort(ctx context.Context, workspace, slot string) (api.GitOpResult, error) {
	op := workspaceSlotOperation("workspace.source-merge-abort", "Abort merge", workspace, slot)
	return p.runWorkspaceGitOp(ctx, op, gitStep{action: "aborting the merge in"}, workspace, slot, "merge", "--abort")
}

// WorkspaceSourceRebaseAbort aborts an in-progress rebase.
func (p *Platform) WorkspaceSourceRebaseAbort(ctx context.Context, workspace, slot string) (api.GitOpResult, error) {
	op := workspaceSlotOperation("workspace.source-rebase-abort", "Abort rebase", workspace, slot)
	return p.runWorkspaceGitOp(ctx, op, gitStep{action: "aborting the rebase in"}, workspace, slot, "rebase", "--abort")
}

// WorkspaceSourceRebaseContinue continues an in-progress rebase after the
// caller has resolved conflicts in the worktree.
func (p *Platform) WorkspaceSourceRebaseContinue(ctx context.Context, workspace, slot string) (api.GitOpResult, error) {
	op := workspaceSlotOperation("workspace.source-rebase-continue", "Continue rebase", workspace, slot)
	return p.runWorkspaceGitOp(ctx, op, gitStep{action: "continuing the rebase in"}, workspace, slot, "-c", "core.editor=true", "rebase", "--continue")
}

// WorkspaceSourcePublish pushes the worktree's branch to the named remote
// (default "origin") under the named branch (default: the current branch),
// setting upstream tracking if not already configured. Useful for
// publishing a workspace branch for the first time so an external
// reviewer can open a PR against it.
//
// For a git source, two cases differ from a plain push. Without an explicit
// branch, a slot with no upstream and no commits beyond its base has nothing
// to publish and is left alone (naming the branch publishes it anyway). A slot
// on a detached HEAD gets the named branch created at HEAD first; if the push
// then fails, the slot stays on that branch and a retry pushes it.
func (p *Platform) WorkspaceSourcePublish(ctx context.Context, workspace, slot, remote, branch string) (result api.GitOpResult, err error) {
	defer workspaceSlotOperation("workspace.source-publish", "Publish", workspace, slot).annotate(&err)
	ctx, release, err := p.beginMutation(ctx, "workspace source")
	if err != nil {
		return api.GitOpResult{}, err
	}
	defer release()
	_, wsSource, source, path, err := p.workspaceSourceTarget(ctx, workspace, slot)
	if err != nil {
		return api.GitOpResult{}, err
	}
	if remote == "" {
		remote = "origin"
	}
	client := p.gitClient()
	currentBranch, onBranch, err := client.CurrentBranch(ctx, path)
	if err != nil {
		return api.GitOpResult{}, fmt.Errorf("resolve HEAD branch in %s: %w", path, err)
	}
	explicit := branch != ""
	if branch == "" {
		branch = wsSource.Branch
	}
	if branch == "" && onBranch {
		branch = currentBranch
	}
	isGit := source.Kind == "git"
	// The count is of HEAD, so it only speaks for the push when HEAD is what
	// would be pushed.
	if isGit && !explicit && (!onBranch || branch == currentBranch) {
		_, hasUpstream, err := client.Upstream(ctx, path)
		if err != nil {
			return api.GitOpResult{}, err
		}
		if !hasUpstream {
			if ahead, base, known := workspaceGitSourceCommitsBeyondBase(ctx, client, path, source, wsSource); known && ahead == 0 {
				return api.GitOpResult{OK: true, ConflictFiles: []string{}, Message: fmt.Sprintf("nothing to publish: no commits beyond %s; pass a branch to publish anyway", base)}, nil
			}
		}
	}
	if branch == "" {
		// Detached HEAD has no branch name to fall back on, and publishing
		// it as "HEAD" would create refs/heads/HEAD on the remote, so the
		// caller must name the branch.
		return api.GitOpResult{}, &InvalidInputError{Field: "branch", Reason: "worktree is in detached HEAD; pass an explicit branch"}
	}
	if isGit && !onBranch {
		// A slot cut without a branch starts detached; publishing names its
		// branch, so create it at HEAD for the push below to track. An
		// existing branch of that name is not reused: it may point elsewhere.
		if client.RefExists(ctx, path, "refs/heads/"+branch) {
			return api.GitOpResult{}, &InvalidInputError{Field: "branch", Reason: fmt.Sprintf("worktree is in detached HEAD and branch %q already exists; switch to it or pass another branch", branch)}
		}
		if _, err := client.Run(ctx, path, "switch", "-c", branch); err != nil {
			return api.GitOpResult{}, fmt.Errorf("create branch %q in %s: %w", branch, path, err)
		}
	}
	remoteURL, _, _ := client.RemoteURL(ctx, path, remote)
	step := gitStep{action: fmt.Sprintf("pushing branch %q of", branch), object: fmt.Sprintf("source slot %q", slot), remote: remoteURL, cause: CausePushFailed, slot: slot, source: wsSource.Source}
	// The push is a network operation like any other: bound it with
	// ANGEE_GIT_TIMEOUT so a stalled remote fails instead of hanging.
	err = git.RunNetworkOperation(ctx, "git push --set-upstream "+remote+" "+branch, path, func(ctx context.Context) error {
		var runErr error
		result, runErr = runGitOpAt(ctx, path, "push", "--set-upstream", remote, branch)
		return runErr
	})
	return result, gitFailure(err, step)
}

// runWorkspaceGitOp runs a local git operation in a slot. step names the
// action for a failure's message; its object and slot are filled in here.
func (p *Platform) runWorkspaceGitOp(ctx context.Context, op operation, step gitStep, workspace, slot string, args ...string) (result api.GitOpResult, err error) {
	defer op.annotate(&err)
	ctx, release, err := p.beginMutation(ctx, "workspace source")
	if err != nil {
		return api.GitOpResult{}, err
	}
	defer release()
	_, wsSource, _, path, err := p.workspaceSourceTarget(ctx, workspace, slot)
	if err != nil {
		return api.GitOpResult{}, err
	}
	step.object = fmt.Sprintf("source slot %q", slot)
	step.slot = slot
	step.source = wsSource.Source
	result, err = runGitOpAt(ctx, path, args...)
	return result, gitFailure(err, step)
}

// gitOpWaitDelay bounds how long a cancelled git operation may linger in Wait.
const gitOpWaitDelay = 100 * time.Millisecond

func runGitOpAt(ctx context.Context, workdir string, args ...string) (api.GitOpResult, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = workdir
	// Bound the wait after cancellation so an ssh child holding the pipes
	// cannot keep a cancelled operation blocked in Wait.
	cmd.WaitDelay = gitOpWaitDelay
	env := gitOpEnv()
	cmd.Env = env
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	trace := logctx.TraceExec(ctx, "git", args, workdir, slog.Any("env", logctx.EnvKeys(env)))
	runErr := cmd.Run()
	trace(combineCapturedOutput(stdout.Bytes(), stderr.Bytes()), runErr)
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
	// to start the merge at all" (unexpected). It carries git's output so
	// the failure can be classified and quoted.
	return result, &git.CommandError{Args: logctx.RedactArgs(args), Output: logctx.RedactText(combined), Err: runErr}
}

func combineCapturedOutput(stdout, stderr []byte) []byte {
	if len(stdout) == 0 {
		return stderr
	}
	if len(stderr) == 0 {
		return stdout
	}
	return append(append([]byte(nil), stdout...), stderr...)
}

func runGitCapture(ctx context.Context, workdir string, args ...string) (string, error) {
	// Force core.quotepath=false so non-ASCII paths come back as raw UTF-8
	// rather than `\NNN`-escaped strings. Callers parse the output (e.g.
	// ls-files -u for conflict files) and need stable bytes.
	full := append([]string{"-c", "core.quotepath=false"}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = workdir
	cmd.WaitDelay = gitOpWaitDelay
	env := gitOpEnv()
	cmd.Env = env
	trace := logctx.TraceExec(ctx, "git", full, workdir, slog.Any("env", logctx.EnvKeys(env)))
	out, err := cmd.Output()
	trace(out, err)
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

// gitOpEnv builds the child environment for git invocations. We pin a
// deterministic identity for merge/rebase commits and silence prompts,
// but we still inherit the small slice of env vars git relies on to
// reach external helpers — most notably PATH (for `ssh`) and HOME (for
// `~/.ssh/config` and friends). Wiping these would break
// `workspaceSourcePublish` against any non-`file://` remote.
func gitOpEnv() []string {
	inherit := []string{"PATH", "HOME", "USER", "SSH_AUTH_SOCK", "SSH_AGENT_PID", "GIT_SSH_COMMAND", "LANG", "LC_ALL"}
	// HTTPS credential helpers such as `gh auth git-credential` read their
	// token and config from the environment; without these a push over HTTPS
	// fails with "could not read Username" on machines that authenticate gh
	// through GH_TOKEN. Only key names are traced, never values.
	inherit = append(inherit, "GH_TOKEN", "GITHUB_TOKEN", "GH_HOST", "GH_CONFIG_DIR", "XDG_CONFIG_HOME", "GIT_ASKPASS")
	env := make([]string, 0, len(inherit)+5)
	for _, key := range inherit {
		if v, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+v)
		}
	}
	env = append(env,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=angee",
		"GIT_AUTHOR_EMAIL=angee@example.invalid",
		"GIT_COMMITTER_NAME=angee",
		"GIT_COMMITTER_EMAIL=angee@example.invalid",
	)
	return git.NonInteractiveEnv(env)
}
