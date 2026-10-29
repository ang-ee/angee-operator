// Package git provides a hybrid git client.
//
// Read-only queries (status, refs) are implemented with go-git where possible
// so they avoid spawning a process per call. Config, ahead/behind, and write or
// network operations (clone, fetch, pull, push, merge, rebase, worktree add)
// shell out to the git CLI so they inherit the user's credential helpers, SSH
// config, config includes, and upstream git's precedence and graph semantics.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ang-ee/angee-operator/internal/logctx"
	"github.com/bluekeyes/go-gitdiff/gitdiff"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// EndOfOptions ends git's option parsing. Every value a request, template or
// manifest supplies (a ref, branch, remote, repository or path) follows it, so
// git reads the value only as an argument: a ref such as `--exec=<cmd>` is
// refused as a bad revision instead of run as an option. It needs git 2.24.
const EndOfOptions = "--end-of-options"

type Client struct {
	Bin            string
	NonInteractive bool
}

const (
	gitTimeoutEnv     = "ANGEE_GIT_TIMEOUT"
	defaultGitTimeout = 2 * time.Minute
	gitWaitDelay      = 100 * time.Millisecond
)

// TimeoutError reports a git network operation stopped by ANGEE_GIT_TIMEOUT.
type TimeoutError struct {
	Operation string
	Dir       string
	Timeout   time.Duration
}

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("%s in %s timed out after %s", e.Operation, e.Dir, e.Timeout)
}

// IsTimeout reports whether err is a git network timeout.
func IsTimeout(err error) bool {
	var timeoutErr *TimeoutError
	return errors.As(err, &timeoutErr)
}

func New() Client {
	return Client{Bin: "git"}
}

// Run executes an arbitrary git command. Kept as an escape hatch for callers
// that need git operations not exposed by the typed API (e.g. checkout in
// templates.go). Prefer adding a typed method over using Run.
func (c Client) Run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd, trace := c.command(ctx, dir, nil, args)
	out, err := cmd.CombinedOutput()
	trace(out, err)
	if err != nil {
		return out, commandError(args, out, err)
	}
	return out, nil
}

// isolatedEnvKeys are the variables RunIsolated passes on: PATH (for ssh),
// HOME (for ~/.ssh/config and friends), the ssh agent, the locale, and what
// HTTPS credential helpers such as `gh auth git-credential` read their token
// and config from. Without the last, a push over HTTPS fails with "could not
// read Username" on machines that authenticate gh through GH_TOKEN.
var isolatedEnvKeys = []string{
	"PATH", "HOME", "USER", "SSH_AUTH_SOCK", "SSH_AGENT_PID", "GIT_SSH_COMMAND", "LANG", "LC_ALL",
	"GH_TOKEN", "GITHUB_TOKEN", "GH_HOST", "GH_CONFIG_DIR", "XDG_CONFIG_HOME", "GIT_ASKPASS",
}

// IsolatedEnv is the whole environment for a command that runs git on a slot
// without prompting: only isolatedEnvKeys from the process environment, then
// extra, made non-interactive (NonInteractiveEnv). RunIsolated runs git in
// it, and the jj driver runs jj in it, since jj runs git for its fetches and
// pushes and must reach the same ssh agent and credential helpers.
func IsolatedEnv(extra ...string) []string {
	env := make([]string, 0, len(isolatedEnvKeys)+len(extra)+2)
	for _, key := range isolatedEnvKeys {
		if v, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+v)
		}
	}
	env = append(env, "GIT_TERMINAL_PROMPT=0")
	env = append(env, extra...)
	return NonInteractiveEnv(env)
}

// RunIsolated runs git in dir in IsolatedEnv(extraEnv...), so it never
// prompts, whatever c.NonInteractive says. It returns standard output, and
// combined: standard output then standard error, trimmed. A failure is a
// *CommandError whose Output is combined, redacted. Only key names are
// traced, never values.
func (c Client) RunIsolated(ctx context.Context, dir string, extraEnv []string, args ...string) (stdout []byte, combined string, err error) {
	cmd, trace := c.command(ctx, dir, IsolatedEnv(extraEnv...), args)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	runErr := cmd.Run()
	trace(logctx.CombinedOutput(out.Bytes(), errOut.Bytes()), runErr)
	combined = strings.TrimSpace(out.String() + "\n" + errOut.String())
	if runErr != nil {
		return out.Bytes(), combined, commandError(args, []byte(combined), runErr)
	}
	return out.Bytes(), combined, nil
}

// command builds the git command for args in dir, and its trace. A nil env
// inherits the environment, made non-interactive when c.NonInteractive is
// set; otherwise env is the whole environment.
func (c Client) command(ctx context.Context, dir string, env, args []string) (*exec.Cmd, func([]byte, error)) {
	bin := c.Bin
	if bin == "" {
		bin = "git"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.WaitDelay = gitWaitDelay
	if dir != "" {
		cmd.Dir = dir
	}
	if env != nil {
		cmd.Env = env
		return cmd, logctx.TraceExec(ctx, bin, args, dir, slog.Any("env", logctx.EnvKeys(env)))
	}
	if c.NonInteractive {
		cmd.Env = NonInteractiveEnv(os.Environ())
	}
	return cmd, logctx.TraceExec(ctx, bin, args, dir)
}

// commandError wraps a failed git command. Callers return it to CLI and API
// clients, so credentials in a remote URL are kept out of it.
func commandError(args []string, output []byte, err error) *CommandError {
	return &CommandError{Args: logctx.RedactArgs(args), Output: logctx.RedactText(string(output)), Err: err}
}

// CommandError is a git command that failed to start or exited non-zero. Args
// and Output are redacted, so the error can be returned to clients; Output
// is kept separately so callers can classify the failure and quote it.
type CommandError struct {
	Args   []string
	Output string
	Err    error
}

func (e *CommandError) Error() string {
	return fmt.Sprintf("git %v: %v: %s", e.Args, e.Err, e.Output)
}

func (e *CommandError) Unwrap() error { return e.Err }

// DirtyPaths lists the paths `git status` reports as changed, staged or
// untracked in the checkout at dir, in its order. A rename lists its new path.
func (c Client) DirtyPaths(ctx context.Context, dir string) ([]string, error) {
	out, err := c.Run(ctx, dir, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	var paths []string
	entries := strings.Split(string(out), "\x00")
	for i := 0; i < len(entries); i++ {
		entry := entries[i]
		if len(entry) < 4 {
			continue
		}
		paths = append(paths, entry[3:])
		// A rename or copy, staged or in the worktree, is followed by its
		// original path.
		if strings.ContainsAny(entry[:2], "RC") {
			i++
		}
	}
	return paths, nil
}

// Diff returns the parsed unified diff of the checkout at dir: the working tree
// against HEAD when ref is empty, else the committed range HEAD..ref.
func (c Client) Diff(ctx context.Context, dir, ref string) ([]*gitdiff.File, error) {
	args := []string{"diff", "--no-color", "--no-ext-diff"}
	// Use the two-argument form for a ref: `git diff <ref>` would compare the
	// working tree against ref and leak uncommitted edits into a committed
	// diff.
	if ref != "" {
		args = append(args, EndOfOptions, "HEAD", ref, "--")
	}
	cmd, trace := c.command(ctx, dir, nil, args)
	// stdout is parsed, so stderr is captured apart from it.
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	trace(logctx.CombinedOutput(stdout.Bytes(), stderr.Bytes()), err)
	if err != nil {
		return nil, commandError(args, stderr.Bytes(), err)
	}
	files, _, err := gitdiff.Parse(stdout)
	if err != nil {
		return nil, fmt.Errorf("parse git diff in %s: %w", dir, err)
	}
	return files, nil
}

func (c Client) runText(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := c.Run(ctx, dir, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// runStdout runs git in dir and returns its standard output alone, for a
// caller that parses it, so a warning git prints is not read as output. A
// failure carries standard error. git reads the repository at dir whatever
// the process environment says (repositoryEnvKeys), and an output of more
// than limit bytes stops git and fails, so a pathological history cannot be
// read into memory.
func (c Client) runStdout(ctx context.Context, dir string, limit int, args ...string) ([]byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	env := slices.DeleteFunc(os.Environ(), func(entry string) bool {
		key, _, _ := strings.Cut(entry, "=")
		return slices.Contains(repositoryEnvKeys, key)
	})
	if c.NonInteractive {
		env = NonInteractiveEnv(env)
	}
	cmd, trace := c.command(ctx, dir, env, args)
	stdout := &cappedBuffer{limit: limit, cancel: cancel}
	var stderr bytes.Buffer
	cmd.Stdout = stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	trace(logctx.CombinedOutput(stdout.Bytes(), stderr.Bytes()), err)
	if stdout.over {
		return nil, commandError(args, stderr.Bytes(), fmt.Errorf("output exceeds %d bytes", limit))
	}
	if err != nil {
		return nil, commandError(args, stderr.Bytes(), err)
	}
	return stdout.Bytes(), nil
}

// repositoryEnvKeys are the variables that point git at another repository,
// work tree, index or object store than the one in its directory. runStdout
// leaves them out; the ones that only bound the search for a repository
// (GIT_CEILING_DIRECTORIES) stay.
var repositoryEnvKeys = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE",
}

// stdoutLimit bounds what runStdout reads from one git command.
const stdoutLimit = 32 << 20

// cappedBuffer holds a command's output up to limit bytes; a write past it
// marks the buffer over and cancels the command instead. The buffer is a
// field, not embedded, so io.Copy cannot bypass Write through its ReadFrom.
type cappedBuffer struct {
	buf    bytes.Buffer
	limit  int
	cancel context.CancelFunc
	over   bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > b.limit {
		b.over = true
		b.cancel()
		return 0, errors.New("output limit reached")
	}
	return b.buf.Write(p)
}

// Bytes is the output held so far.
func (b *cappedBuffer) Bytes() []byte { return b.buf.Bytes() }

// NonInteractiveEnv returns env configured so git and SSH cannot prompt. An
// existing GIT_SSH_COMMAND is preserved verbatim.
func NonInteractiveEnv(env []string) []string {
	env = setEnv(env, "GIT_TERMINAL_PROMPT", "0")
	if !envContains(env, "GIT_SSH_COMMAND") {
		env = append(env, "GIT_SSH_COMMAND=ssh -o BatchMode=yes")
	}
	return env
}

func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	result := make([]string, 0, len(env)+1)
	set := false
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			if !set {
				result = append(result, prefix+value)
				set = true
			}
			continue
		}
		result = append(result, entry)
	}
	if !set {
		result = append(result, prefix+value)
	}
	return result
}

func envContains(env []string, key string) bool {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return true
		}
	}
	return false
}

func networkTimeout() (time.Duration, error) {
	raw, ok := os.LookupEnv(gitTimeoutEnv)
	if !ok || strings.TrimSpace(raw) == "" {
		return defaultGitTimeout, nil
	}
	timeout, err := time.ParseDuration(raw)
	if err != nil || timeout < 0 {
		return 0, fmt.Errorf("invalid %s %q: expected a non-negative Go duration", gitTimeoutEnv, raw)
	}
	return timeout, nil
}

// RunNetworkOperation runs fn under the ANGEE_GIT_TIMEOUT deadline and turns
// expiry of that deadline into a descriptive TimeoutError. A caller deadline
// or cancellation remains the caller's error.
func RunNetworkOperation(ctx context.Context, operation, dir string, fn func(context.Context) error) error {
	timedCtx, cancel, timeout, err := networkContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	err = fn(timedCtx)
	return networkResult(ctx, timedCtx, operation, dir, timeout, err)
}

func (c Client) runNetwork(ctx context.Context, commandDir, reportDir, operation string, args ...string) ([]byte, error) {
	var out []byte
	err := RunNetworkOperation(ctx, operation, reportDir, func(timedCtx context.Context) error {
		var err error
		out, err = c.Run(timedCtx, commandDir, args...)
		return err
	})
	return out, err
}

func networkContext(ctx context.Context) (context.Context, context.CancelFunc, time.Duration, error) {
	timeout, err := networkTimeout()
	if err != nil {
		return nil, nil, 0, err
	}
	if timeout == 0 {
		return ctx, func() {}, 0, nil
	}
	timedCtx, cancel := context.WithTimeout(ctx, timeout)
	return timedCtx, cancel, timeout, nil
}

func networkResult(parent, timedCtx context.Context, operation, dir string, timeout time.Duration, err error) error {
	if timeout > 0 && timedCtx.Err() == context.DeadlineExceeded && parent.Err() == nil {
		return &TimeoutError{Operation: operation, Dir: dir, Timeout: timeout}
	}
	return err
}

// open returns the go-git repository at dir. Discovers the repo via parent
// walk so worktrees and bare-checked-out directories work.
func openRepo(dir string) (*gogit.Repository, error) {
	repo, err := gogit.PlainOpenWithOptions(dir, &gogit.PlainOpenOptions{DetectDotGit: true, EnableDotGitCommonDir: true})
	if err != nil {
		return nil, fmt.Errorf("open git repo at %s: %w", dir, err)
	}
	return repo, nil
}

// --- Network / write operations: shell out to git CLI for auth correctness ---

func (c Client) Clone(ctx context.Context, repo, dest string, args ...string) error {
	cmdArgs := append([]string{"clone"}, args...)
	cmdArgs = append(cmdArgs, EndOfOptions, repo, dest)
	operationArgs := append([]string{"git", "clone"}, args...)
	operationArgs = append(operationArgs, logctx.RedactURL(repo))
	_, err := c.runNetwork(ctx, "", dest, strings.Join(operationArgs, " "), cmdArgs...)
	return err
}

// CloneRef clones repo into dest with ref checked out (the remote's default
// branch when empty), naming the remote remote. Callers name it, origin
// included: without --origin git takes the name from the user's
// clone.defaultRemoteName, which the caller does not know.
func (c Client) CloneRef(ctx context.Context, repo, dest, ref, remote string) error {
	args := []string{}
	if remote != "" {
		args = append(args, "--origin", remote)
	}
	if ref != "" {
		args = append(args, "--branch", ref)
	}
	return c.Clone(ctx, repo, dest, args...)
}

func (c Client) Fetch(ctx context.Context, dir string) error {
	_, err := c.runNetwork(ctx, dir, dir, "git fetch --all --prune", "fetch", "--all", "--prune")
	return err
}

func (c Client) Merge(ctx context.Context, dir, ref string) error {
	_, err := c.Run(ctx, dir, "merge", "--no-edit", EndOfOptions, ref)
	return err
}

func (c Client) Rebase(ctx context.Context, dir, ref string) error {
	_, err := c.Run(ctx, dir, "rebase", EndOfOptions, ref)
	return err
}

// worktreeAdd is `git worktree add <args>` with absolute links: git writes the
// new worktree's .git file, and the repository's gitdir file for it, as
// absolute paths, whatever the user's worktree.useRelativePaths (git 2.48+)
// says. Older git ignores the key. A worktree added in one directory and
// installed in another (the git driver stages every worktree slot) then
// still leads to its repository, and the repository's format is left as it
// was: relative links make git set extensions.relativeWorktrees, which git
// before 2.48 and go-git refuse to open.
func worktreeAdd(args ...string) []string {
	return append([]string{"-c", "worktree.useRelativePaths=false", "worktree", "add"}, args...)
}

// WorktreeAdd adds a worktree at dest with ref checked out. Its links are
// absolute (worktreeAdd).
func (c Client) WorktreeAdd(ctx context.Context, repoDir, dest, ref string) error {
	args := worktreeAdd(EndOfOptions, dest)
	if ref != "" {
		args = append(args, ref)
	}
	_, err := c.Run(ctx, repoDir, args...)
	return err
}

// WorktreeAddDetached adds a worktree at dest on a detached HEAD at ref, so no
// branch is created or checked out. An empty ref detaches at repoDir's HEAD.
// Its links are absolute (worktreeAdd).
func (c Client) WorktreeAddDetached(ctx context.Context, repoDir, dest, ref string) error {
	args := worktreeAdd("--detach", EndOfOptions, dest)
	if ref != "" {
		args = append(args, ref)
	}
	_, err := c.Run(ctx, repoDir, args...)
	return err
}

// WorktreeAddBranch adds a worktree at dest on a new branch cut from ref. With
// track false the new branch gets no upstream even when ref is a
// remote-tracking branch, which git would otherwise set. Its links are
// absolute (worktreeAdd).
func (c Client) WorktreeAddBranch(ctx context.Context, repoDir, dest, branch, ref string, track bool) error {
	args := worktreeAdd()
	if !track {
		args = append(args, "--no-track")
	}
	if branch != "" {
		args = append(args, "-b", branch)
	}
	args = append(args, EndOfOptions, dest)
	if ref != "" {
		args = append(args, ref)
	}
	_, err := c.Run(ctx, repoDir, args...)
	return err
}

// WorktreeRemove deregisters the worktree at dest and deletes its working
// tree. It targets only dest, so sibling worktrees of repoDir are left intact;
// --force lets git drop the worktree even when it holds uncommitted changes. It
// is used to reclaim a populated leftover worktree from a failed create before
// re-adding the path, and to roll back worktrees a failed create materialized.
// The doubled --force drops the worktree even when it is locked, and on current
// git even when its working tree has already gone missing, since reclaim and
// rollback always mean to discard the leftover. The missing-registration
// recovery in materializeWorkspaceSource still routes through WorktreePrune, the
// portable recovery for older git that refuses to remove a missing working tree.
func (c Client) WorktreeRemove(ctx context.Context, repoDir, dest string) error {
	_, err := c.Run(ctx, repoDir, "worktree", "remove", "--force", "--force", dest)
	return err
}

// WorktreePrune clears administrative entries for worktrees whose working tree
// has gone missing, so one can be re-added at a path git still reports as
// "missing but already registered". It is git's canonical recovery for a
// prunable registration (WorktreeRemove rejects a missing working tree). It
// acts on every prunable entry in repoDir, so callers invoke it only when an
// add actually conflicts, not preemptively.
func (c Client) WorktreePrune(ctx context.Context, repoDir string) error {
	_, err := c.Run(ctx, repoDir, "worktree", "prune")
	return err
}

// WorktreeRegistered reports whether repoDir has a worktree registered at dest.
func (c Client) WorktreeRegistered(ctx context.Context, repoDir, dest string) (bool, error) {
	out, err := c.runText(ctx, repoDir, "worktree", "list", "--porcelain")
	if err != nil {
		return false, err
	}
	want, err := canonicalWorktreePath(dest)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "worktree ") {
			continue
		}
		path, err := canonicalWorktreePath(strings.TrimPrefix(line, "worktree "))
		if err == nil && path == want {
			return true, nil
		}
	}
	return false, nil
}

// Worktree is one worktree a repository lists, its main one included: where
// it is checked out, the commit at its HEAD, and the branch it has checked
// out, by its short name ("" when HEAD is detached).
type Worktree struct {
	Path   string
	Head   string
	Branch string
}

// Worktrees lists the worktrees of the repository at dir (`git worktree list
// --porcelain -z`, so no path is misread), in git's order, its main worktree
// first.
func (c Client) Worktrees(ctx context.Context, dir string) ([]Worktree, error) {
	out, err := c.runStdout(ctx, dir, stdoutLimit, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	return parseWorktrees(string(out)), nil
}

// parseWorktrees reads `git worktree list --porcelain -z`: each attribute
// ends in a NUL, and an empty one ends a worktree.
func parseWorktrees(out string) []Worktree {
	var worktrees []Worktree
	var current *Worktree
	for _, field := range strings.Split(out, "\x00") {
		key, value, _ := strings.Cut(field, " ")
		switch {
		case field == "":
			current = nil
		case key == "worktree":
			worktrees = append(worktrees, Worktree{Path: value})
			current = &worktrees[len(worktrees)-1]
		case current == nil:
		case key == "HEAD":
			current.Head = value
		case key == "branch":
			current.Branch = strings.TrimPrefix(value, "refs/heads/")
		}
	}
	return worktrees
}

func canonicalWorktreePath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(resolved), nil
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return filepath.Clean(abs), nil
	}
	return filepath.Clean(filepath.Join(parent, filepath.Base(abs))), nil
}

// CheckoutDetached detaches the checkout at dir at ref. ref is resolved to a
// commit first and the commit ID is checked out: before git 2.44, checkout
// does not honour --end-of-options, so the ID is what keeps an option-shaped
// ref from being read as an option.
func (c Client) CheckoutDetached(ctx context.Context, dir, ref string) error {
	commit, err := c.runText(ctx, dir, "rev-parse", "--verify", EndOfOptions, ref+"^{commit}")
	if err != nil {
		return err
	}
	_, err = c.Run(ctx, dir, "checkout", "--detach", commit)
	return err
}

func (c Client) Pull(ctx context.Context, dir string) error {
	_, err := c.runNetwork(ctx, dir, dir, "git pull --ff-only", "pull", "--ff-only")
	return err
}

func (c Client) Push(ctx context.Context, dir string, ref string) error {
	timedCtx, cancel, timeout, err := networkContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	args := []string{"push"}
	if ref != "" {
		remote, err := c.pushRemote(timedCtx, dir)
		if err != nil {
			return networkResult(ctx, timedCtx, "git push", dir, timeout, err)
		}
		args = append(args, EndOfOptions, remote, ref)
	}
	_, err = c.Run(timedCtx, dir, args...)
	return networkResult(ctx, timedCtx, "git "+strings.Join(args, " "), dir, timeout, err)
}

func (c Client) PushSetUpstream(ctx context.Context, dir string, ref string) error {
	if ref == "" {
		return c.Push(ctx, dir, ref)
	}
	timedCtx, cancel, timeout, err := networkContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	remote, err := c.pushRemote(timedCtx, dir)
	if err != nil {
		return networkResult(ctx, timedCtx, "git push -u", dir, timeout, err)
	}
	args := []string{"push", "-u", EndOfOptions, remote, ref}
	_, err = c.Run(timedCtx, dir, args...)
	return networkResult(ctx, timedCtx, "git "+strings.Join(args, " "), dir, timeout, err)
}

// --- Read-only queries: go-git ---

func (c Client) RefExists(ctx context.Context, dir, ref string) bool {
	if strings.TrimSpace(ref) == "" {
		return false
	}
	repo, err := openRepo(dir)
	if err != nil {
		_, err := c.Run(ctx, dir, "rev-parse", "--verify", "--quiet", EndOfOptions, ref+"^{commit}")
		return err == nil
	}
	hash, err := repo.ResolveRevision(plumbing.Revision(ref))
	if err != nil || hash == nil {
		return false
	}
	if _, err := repo.CommitObject(*hash); err != nil {
		return false
	}
	return true
}

// SyncBaseRef returns the ref at dir that the base ref is synced from: ref
// itself when it is a full or remote-qualified ref; otherwise, in order, the
// remote branch the local branch ref tracks (git's own <ref>@{upstream}),
// <baseRemote>/<ref>, <remote>/<ref> for the other remotes in name order, and
// the local ref. baseRemote is the remote the base comes from, the source's
// clone remote; empty means origin.
func (c Client) SyncBaseRef(ctx context.Context, dir, ref, baseRemote string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", fmt.Errorf("base ref is empty")
	}
	remotes, err := c.Remotes(ctx, dir)
	if err != nil {
		return "", err
	}
	slices.Sort(remotes)
	if baseRemote == "" {
		baseRemote = "origin"
	}
	candidates := []string{}
	if strings.HasPrefix(ref, "refs/") || remoteQualifiedRef(ref, remotes) {
		candidates = append(candidates, ref)
	} else {
		if upstream, ok := c.trackedRemoteBranch(ctx, dir, ref); ok {
			candidates = append(candidates, upstream)
		}
		if slices.Contains(remotes, baseRemote) {
			candidates = append(candidates, baseRemote+"/"+ref)
		}
		for _, remote := range remotes {
			if remote != baseRemote {
				candidates = append(candidates, remote+"/"+ref)
			}
		}
		candidates = append(candidates, ref)
	}
	seen := map[string]bool{}
	for _, candidate := range candidates {
		if seen[candidate] {
			continue
		}
		seen[candidate] = true
		if c.RefExists(ctx, dir, candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("base ref %q was not found after fetch", ref)
}

// trackedRemoteBranch returns the remote branch the local branch branch tracks,
// as <remote>/<branch>, and false when it tracks none or a local branch, or no
// local branch has that name. git reads <name>@{upstream} only with the short
// branch name, not refs/heads/<name>.
func (c Client) trackedRemoteBranch(ctx context.Context, dir, branch string) (string, bool) {
	out, err := c.runText(ctx, dir, "rev-parse", "--verify", "--quiet", "--symbolic-full-name", EndOfOptions, branch+"@{upstream}")
	if err != nil {
		return "", false
	}
	upstream, ok := strings.CutPrefix(out, "refs/remotes/")
	return upstream, ok && upstream != ""
}

// SetUpstream makes the local branch branch track upstream, a remote-tracking
// ref; pass it in full (refs/remotes/origin/main), since a local branch can
// have the short name.
func (c Client) SetUpstream(ctx context.Context, dir, branch, upstream string) error {
	if _, err := c.Run(ctx, dir, "branch", "--set-upstream-to", upstream, EndOfOptions, branch); err != nil {
		return fmt.Errorf("set the upstream of %s to %s: %w", branch, upstream, err)
	}
	return nil
}

// BranchUpstream returns the remote and the merge ref the local branch branch
// tracks (branch.<name>.remote and .merge), empty when unset.
func (c Client) BranchUpstream(ctx context.Context, dir, branch string) (remote, merge string, err error) {
	remotes, err := c.configValues(ctx, dir, "branch."+branch+".remote")
	if err != nil || len(remotes) == 0 {
		return "", "", err
	}
	merges, err := c.configValues(ctx, dir, "branch."+branch+".merge")
	if err != nil || len(merges) == 0 {
		return remotes[0], "", err
	}
	return remotes[0], merges[0], nil
}

func remoteQualifiedRef(ref string, remotes []string) bool {
	for _, remote := range remotes {
		if strings.HasPrefix(ref, remote+"/") {
			return true
		}
	}
	return false
}

func (c Client) CurrentRef(ctx context.Context, dir string) (string, error) {
	repo, err := openRepo(dir)
	if err != nil {
		return c.currentRefCLI(ctx, dir)
	}
	head, err := repo.Head()
	if err != nil {
		return c.currentRefCLI(ctx, dir)
	}
	if head.Name().IsBranch() {
		return head.Name().Short(), nil
	}
	return shortHash(head.Hash().String()), nil
}

func (c Client) CurrentBranch(ctx context.Context, dir string) (string, bool, error) {
	repo, err := openRepo(dir)
	if err != nil {
		return c.currentBranchCLI(ctx, dir)
	}
	head, err := repo.Head()
	if err != nil {
		// Detached or no commits yet: not an error for this query.
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return "", false, nil
		}
		return c.currentBranchCLI(ctx, dir)
	}
	if !head.Name().IsBranch() {
		return "", false, nil
	}
	return head.Name().Short(), true, nil
}

func (c Client) Upstream(ctx context.Context, dir string) (string, bool, error) {
	branch, ok, err := c.CurrentBranch(ctx, dir)
	if err != nil {
		return "", false, err
	}
	if !ok {
		return "", false, nil
	}
	repo, err := openRepo(dir)
	if err != nil {
		return c.upstreamCLI(ctx, dir)
	}
	cfg, err := repo.Config()
	if err != nil {
		return c.upstreamCLI(ctx, dir)
	}
	br, ok := cfg.Branches[branch]
	if !ok || br.Remote == "" || br.Merge == "" {
		return "", false, nil
	}
	mergeShort := strings.TrimPrefix(string(br.Merge), "refs/heads/")
	return br.Remote + "/" + mergeShort, true, nil
}

// CountNotOn counts the commits reachable from HEAD in dir that none of bases
// reach.
func (c Client) CountNotOn(ctx context.Context, dir string, bases ...string) (int, error) {
	return c.countHead(ctx, dir, append([]string{"--not", EndOfOptions}, bases...)...)
}

// CountNotOnAnyRef counts the commits reachable from HEAD in dir that no
// branch, remote-tracking branch or tag reaches: what only a detached HEAD
// holds.
func (c Client) CountNotOnAnyRef(ctx context.Context, dir string) (int, error) {
	return c.countHead(ctx, dir, "--not", "--branches", "--remotes", "--tags")
}

// CountNotOnRemotesOrTags counts the commits reachable from HEAD in dir that
// no remote-tracking branch or tag reaches. git keeps fetched tags with local
// ones, so a remote's tag on a release commit counts as holding it.
func (c Client) CountNotOnRemotesOrTags(ctx context.Context, dir string) (int, error) {
	return c.countHead(ctx, dir, "--not", "--remotes", "--tags")
}

// LinkedWorktree reports whether the checkout at dir is a linked worktree,
// whose branches and tags live in the repository it was added from and
// outlive it, rather than a repository of its own.
func (c Client) LinkedWorktree(ctx context.Context, dir string) (bool, error) {
	out, err := c.runText(ctx, dir, "rev-parse", "--git-dir", "--git-common-dir")
	if err != nil {
		return false, err
	}
	gitDir, commonDir, ok := strings.Cut(out, "\n")
	if !ok {
		return false, fmt.Errorf("git rev-parse --git-dir --git-common-dir in %s: unexpected output %q", dir, out)
	}
	// git prints either path relative to dir or absolute, and older versions
	// mix the two, so compare where they lead.
	same, err := samePath(gitPathIn(dir, gitDir), gitPathIn(dir, commonDir))
	if err != nil {
		return false, err
	}
	return !same, nil
}

func gitPathIn(dir, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(dir, path)
}

func samePath(a, b string) (bool, error) {
	a, err := filepath.EvalSymlinks(a)
	if err != nil {
		return false, err
	}
	b, err = filepath.EvalSymlinks(b)
	if err != nil {
		return false, err
	}
	return a == b, nil
}

func (c Client) countHead(ctx context.Context, dir string, args ...string) (int, error) {
	out, err := c.runText(ctx, dir, append([]string{"rev-list", "--count", "HEAD"}, args...)...)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(out))
}

func (c Client) AheadCount(ctx context.Context, dir, base string) (int, error) {
	ahead, _, err := c.AheadBehind(ctx, dir, base)
	return ahead, err
}

func (c Client) AheadBehind(ctx context.Context, dir, base string) (ahead int, behind int, err error) {
	return c.aheadBehindCLI(ctx, dir, base)
}

// LogCommit is a commit as Log lists it.
type LogCommit struct {
	Hash        string
	Parents     []string
	AuthorName  string
	AuthorEmail string
	Committed   time.Time // the committer's time
	Message     string
}

// logFormat prints a commit's fields a line each, the message last, since it
// may span lines. With -z a NUL ends each commit, which no field holds: git
// prints a message only up to a NUL it contains.
const logFormat = "%H%n%P%n%ct%n%an%n%ae%n%B"

// Log lists up to limit commits reachable from HEAD in the repository at dir,
// in git log's default order, newest committer time first. A HEAD with no
// commit yet has none. It runs the git CLI, so it reads a repository go-git
// refuses to open, such as a cache jj has added a slot to
// (RelativeWorktreesVersion). Names and messages are UTF-8, converted from
// the encoding a commit declares, and replace refs are not followed, as
// go-git follows none.
func (c Client) Log(ctx context.Context, dir string, limit int) ([]LogCommit, error) {
	if limit <= 0 {
		return nil, nil
	}
	// The -- keeps a file named HEAD from making HEAD ambiguous.
	out, err := c.runStdout(ctx, dir, stdoutLimit, "--no-replace-objects", "log", "-z", "--no-show-signature", "--encoding=UTF-8",
		"--max-count="+strconv.Itoa(limit), "--format="+logFormat, "HEAD", "--")
	if err != nil {
		// rev-parse exits 1 for a HEAD that names nothing, an unborn branch,
		// but 0 for one that names a missing object, and 128 outside a
		// repository: only the first is an empty history.
		var exitErr *exec.ExitError
		if _, headErr := c.runStdout(ctx, dir, stdoutLimit, "rev-parse", "--verify", "--quiet", "HEAD"); errors.As(headErr, &exitErr) && exitErr.ExitCode() == 1 {
			return nil, nil
		}
		return nil, err
	}
	var commits []LogCommit
	for _, record := range strings.Split(string(out), "\x00") {
		if record == "" {
			continue
		}
		fields := strings.SplitN(record, "\n", 6)
		if len(fields) != 6 {
			return nil, fmt.Errorf("git log in %s: unexpected record %q", dir, record)
		}
		seconds, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("git log in %s: committer time %q: %w", dir, fields[2], err)
		}
		commits = append(commits, LogCommit{
			Hash:        fields[0],
			Parents:     strings.Fields(fields[1]),
			Committed:   time.Unix(seconds, 0),
			AuthorName:  fields[3],
			AuthorEmail: fields[4],
			Message:     fields[5],
		})
	}
	return commits, nil
}

// RefTargets maps each object a ref of the repository at dir points at to the
// full names of those refs, sorted. A symbolic ref (refs/remotes/origin/HEAD)
// is left out, and an annotated tag is keyed by its tag object, not by the
// commit it tags. Like Log, it runs the git CLI.
func (c Client) RefTargets(ctx context.Context, dir string) (map[string][]string, error) {
	out, err := c.runStdout(ctx, dir, stdoutLimit, "for-each-ref", "--format=%(objectname) %(refname) %(symref)")
	if err != nil {
		return nil, err
	}
	targets := map[string][]string{}
	for _, line := range strings.Split(string(out), "\n") {
		// A symbolic ref has a third field, its target's name.
		if fields := strings.Fields(line); len(fields) == 2 {
			targets[fields[0]] = append(targets[fields[0]], fields[1])
		}
	}
	for _, names := range targets {
		slices.Sort(names)
	}
	return targets, nil
}

func (c Client) Config(ctx context.Context, dir, key string) (string, bool, error) {
	return c.configCLI(ctx, dir, key)
}

// Version returns what `git --version` prints, such as
// "git version 2.50.1 (Apple Git-155)".
func (c Client) Version(ctx context.Context) (string, error) {
	return c.runText(ctx, "", "--version")
}

// RelativeWorktreesVersion is the oldest git that opens a repository with
// extensions.relativeWorktrees set. `jj workspace add --colocate` sets it on
// the repository it adds a workspace to, since it adds the workspace's git
// worktree with relative links, and an older git refuses that repository (a
// source's cache) and every git worktree slot of it. So jj slots need it.
const RelativeWorktreesVersion = "2.48"

var versionNumber = regexp.MustCompile(`(\d+)\.(\d+)`)

// VersionAtLeast reports whether versionLine, what `git --version` prints,
// names git floor or later; floor is "<major>.<minor>", such as
// RelativeWorktreesVersion. A line or a floor that names no version is not.
func VersionAtLeast(versionLine, floor string) bool {
	have, ok := majorMinor(versionLine)
	if !ok {
		return false
	}
	want, ok := majorMinor(floor)
	return ok && slices.Compare(have, want) >= 0
}

// majorMinor reads the first "<major>.<minor>" in s.
func majorMinor(s string) ([]int, bool) {
	m := versionNumber.FindStringSubmatch(s)
	if m == nil {
		return nil, false
	}
	major, err := strconv.Atoi(m[1])
	if err != nil {
		return nil, false
	}
	minor, err := strconv.Atoi(m[2])
	if err != nil {
		return nil, false
	}
	return []int{major, minor}, true
}

func (c Client) configCLI(ctx context.Context, dir, key string) (string, bool, error) {
	value, err := c.runText(ctx, dir, "config", "--get", key)
	if err != nil || value == "" {
		return "", false, nil
	}
	return value, true, nil
}

// RemoteURL returns the URL remote fetches from in the repository at dir (the
// first of its configured URLs), and false when it has none. It reads the
// configured value rather than `git remote get-url`, which applies
// url.<base>.insteadOf rewrites and so would not match a declared URL under
// such a rewrite.
func (c Client) RemoteURL(ctx context.Context, dir, remote string) (string, bool, error) {
	urls, err := c.remoteURLs(ctx, dir, remote)
	if err != nil || len(urls) == 0 {
		return "", false, err
	}
	return urls[0], true, nil
}

func (c Client) remoteURLs(ctx context.Context, dir, remote string) ([]string, error) {
	return c.configValues(ctx, dir, "remote."+remote+".url")
}

// configValues returns every value of key in the repository at dir, and none
// when the key is unset.
func (c Client) configValues(ctx context.Context, dir, key string) ([]string, error) {
	value, err := c.runText(ctx, dir, "config", "--get-all", EndOfOptions, key)
	if err != nil {
		// `git config` exits 1 for a missing key; anything else (an
		// unreadable config, a cancelled context) is an error.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && ctx.Err() == nil {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", key, err)
	}
	if value == "" {
		return nil, nil
	}
	return strings.Split(value, "\n"), nil
}

// PushURLs returns remote's configured push URLs (remote.<name>.pushurl), and
// none when pushes go to its fetch URL.
func (c Client) PushURLs(ctx context.Context, dir, remote string) ([]string, error) {
	return c.configValues(ctx, dir, "remote."+remote+".pushurl")
}

// SetPushURL makes rawURL remote's only push URL.
func (c Client) SetPushURL(ctx context.Context, dir, remote, rawURL string) error {
	if _, err := c.Run(ctx, dir, "config", "--replace-all", EndOfOptions, "remote."+remote+".pushurl", rawURL); err != nil {
		return fmt.Errorf("set remote.%s.pushurl: %w", remote, err)
	}
	return nil
}

// UnsetPushURL removes remote's push URLs, so pushes go to its fetch URL.
func (c Client) UnsetPushURL(ctx context.Context, dir, remote string) error {
	if _, err := c.Run(ctx, dir, "config", "--unset-all", EndOfOptions, "remote."+remote+".pushurl"); err != nil {
		return fmt.Errorf("unset remote.%s.pushurl: %w", remote, err)
	}
	return nil
}

// SetPushDefault sets remote.pushDefault, the remote a push goes to when the
// branch names no pushRemote.
func (c Client) SetPushDefault(ctx context.Context, dir, remote string) error {
	if _, err := c.Run(ctx, dir, "config", EndOfOptions, "remote.pushDefault", remote); err != nil {
		return fmt.Errorf("set remote.pushDefault: %w", err)
	}
	return nil
}

// EnsureRemoteURL makes rawURL the URL remote fetches from in the repository
// at dir, adding the remote when it has no URL. It returns remote's URL before
// the call (empty when it had none) and whether it changed anything. A remote
// with several URLs is refused rather than rewritten.
func (c Client) EnsureRemoteURL(ctx context.Context, dir, remote, rawURL string) (string, bool, error) {
	urls, err := c.remoteURLs(ctx, dir, remote)
	if err != nil {
		return "", false, err
	}
	current := ""
	if len(urls) > 0 {
		current = urls[0]
	}
	ok := current != ""
	if ok && current == rawURL {
		return current, false, nil
	}
	if len(urls) > 1 {
		return current, false, fmt.Errorf("remote %s has %d URLs; set the one to fetch from with `git remote set-url`", remote, len(urls))
	}
	verb := "add"
	if ok {
		verb = "set-url"
	}
	if _, err := c.Run(ctx, dir, "remote", verb, "--", remote, rawURL); err != nil {
		return current, false, fmt.Errorf("set remote.%s.url: %w", remote, err)
	}
	return current, true, nil
}

func (c Client) Remotes(ctx context.Context, dir string) ([]string, error) {
	repo, err := openRepo(dir)
	if err != nil {
		return c.remotesCLI(ctx, dir)
	}
	remotes, err := repo.Remotes()
	if err != nil {
		return c.remotesCLI(ctx, dir)
	}
	names := make([]string, 0, len(remotes))
	for _, r := range remotes {
		names = append(names, r.Config().Name)
	}
	return names, nil
}

func (c Client) PushRemote(ctx context.Context, dir string) (string, error) {
	timedCtx, cancel, timeout, err := networkContext(ctx)
	if err != nil {
		return "", err
	}
	defer cancel()
	remote, err := c.pushRemote(timedCtx, dir)
	return remote, networkResult(ctx, timedCtx, "git resolve push remote", dir, timeout, err)
}

// PushRemoteFor is PushRemote for a push of branch, whatever the checkout at
// dir is on: a jj slot's HEAD is detached, but its bookmark is pushed as a
// branch. An empty branch resolves as a detached HEAD does.
func (c Client) PushRemoteFor(ctx context.Context, dir, branch string) (string, error) {
	timedCtx, cancel, timeout, err := networkContext(ctx)
	if err != nil {
		return "", err
	}
	defer cancel()
	remote, err := c.pushRemoteFor(timedCtx, dir, branch, branch != "")
	return remote, networkResult(ctx, timedCtx, "git resolve push remote", dir, timeout, err)
}

func (c Client) pushRemote(ctx context.Context, dir string) (string, error) {
	branch, hasBranch, err := c.CurrentBranch(ctx, dir)
	if err != nil {
		return "", err
	}
	return c.pushRemoteFor(ctx, dir, branch, hasBranch)
}

func (c Client) pushRemoteFor(ctx context.Context, dir, branch string, hasBranch bool) (string, error) {
	if hasBranch {
		if remote, ok, err := c.Config(ctx, dir, "branch."+branch+".pushRemote"); err != nil {
			return "", err
		} else if ok {
			return remote, nil
		}
	}
	if remote, ok, err := c.Config(ctx, dir, "remote.pushDefault"); err != nil {
		return "", err
	} else if ok {
		return remote, nil
	}
	if hasBranch {
		if remote, ok, err := c.Config(ctx, dir, "branch."+branch+".remote"); err != nil {
			return "", err
		} else if ok {
			return remote, nil
		}
	}
	remotes, err := c.Remotes(ctx, dir)
	if err != nil {
		return "", err
	}
	for _, remote := range remotes {
		if remote == "origin" {
			return remote, nil
		}
	}
	if len(remotes) == 1 {
		return remotes[0], nil
	}
	if len(remotes) == 0 {
		return "", &NoPushRemoteError{Reason: "no git remotes configured"}
	}
	if hasBranch {
		return "", &NoPushRemoteError{Reason: fmt.Sprintf("multiple git remotes configured; set branch.%s.pushRemote or remote.pushDefault", branch)}
	}
	return "", &NoPushRemoteError{Reason: "multiple git remotes configured; set remote.pushDefault"}
}

// NoPushRemoteError reports a checkout for which git's push configuration
// names no remote and no single remote stands in: none is configured, or
// several are and neither the branch's pushRemote nor remote.pushDefault
// picks one. Reason says which, and what to set.
type NoPushRemoteError struct {
	Reason string
}

func (e *NoPushRemoteError) Error() string { return e.Reason }

// Dirty reports whether the checkout at dir has uncommitted changes, as git
// status sees them (DirtyPaths).
func (c Client) Dirty(ctx context.Context, dir string) (bool, error) {
	paths, err := c.DirtyPaths(ctx, dir)
	return len(paths) > 0, err
}

func (c Client) currentRefCLI(ctx context.Context, dir string) (string, error) {
	branch, err := c.runText(ctx, dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err == nil && branch != "" {
		return branch, nil
	}
	hash, err := c.runText(ctx, dir, "rev-parse", "--short", "HEAD")
	if err != nil {
		return "", err
	}
	return hash, nil
}

func (c Client) currentBranchCLI(ctx context.Context, dir string) (string, bool, error) {
	branch, err := c.runText(ctx, dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err == nil && branch != "" {
		return branch, true, nil
	}
	if _, repoErr := c.runText(ctx, dir, "rev-parse", "--git-dir"); repoErr != nil {
		return "", false, err
	}
	return "", false, nil
}

func (c Client) upstreamCLI(ctx context.Context, dir string) (string, bool, error) {
	_, ok, err := c.CurrentBranch(ctx, dir)
	if err != nil || !ok {
		return "", false, err
	}
	upstream, err := c.runText(ctx, dir, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}")
	if err != nil || upstream == "" {
		return "", false, nil
	}
	return upstream, true, nil
}

func (c Client) aheadBehindCLI(ctx context.Context, dir, base string) (int, int, error) {
	out, err := c.runText(ctx, dir, "rev-list", "--left-right", "--count", EndOfOptions, base+"...HEAD")
	if err != nil {
		return 0, 0, err
	}
	fields := strings.Fields(out)
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("git rev-list count returned %q", out)
	}
	behind, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, 0, fmt.Errorf("parse behind count %q: %w", fields[0], err)
	}
	ahead, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, fmt.Errorf("parse ahead count %q: %w", fields[1], err)
	}
	return ahead, behind, nil
}

func (c Client) remotesCLI(ctx context.Context, dir string) ([]string, error) {
	out, err := c.runText(ctx, dir, "remote")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return strings.Fields(out), nil
}

func shortHash(h string) string {
	if len(h) > 7 {
		return h[:7]
	}
	return h
}
