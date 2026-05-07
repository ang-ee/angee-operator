// Package git wraps git CLI operations for ANGEE_ROOT management.
package git

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// refRE constrains user-supplied git refs (SHAs, branches, tags) to
// characters that can never be misinterpreted as a flag. Without this,
// a value like `--upload-pack=evil` flowing from an HTTP request into
// `git revert <ref>` would smuggle a flag into git's argv.
//
// The git-check-ref-format rules are stricter than this; we accept the
// subset that's universally safe and reject everything else with a clear
// error message rather than handing argv to git and hoping.
var refRE = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// validateRef returns an error if ref contains anything outside refRE,
// is empty, or starts with `-` (defence in depth).
func validateRef(ref string) error {
	if ref == "" {
		return fmt.Errorf("empty ref")
	}
	if strings.HasPrefix(ref, "-") {
		return fmt.Errorf("ref must not start with '-': %q", ref)
	}
	if !refRE.MatchString(ref) {
		return fmt.Errorf("ref %q contains characters outside %s", ref, refRE.String())
	}
	return nil
}

// Repo wraps git operations for a single repository.
type Repo struct {
	Path string
}

// New creates a Repo for the given path.
func New(path string) *Repo {
	return &Repo{Path: path}
}

// run executes a git subcommand with the given context. Callers without a
// caller-bound context can pass context.Background() — methods that flow
// from HTTP handlers (Revert, ResetHard, Log) accept ctx so a client
// disconnect cancels the underlying git process instead of leaking it.
func (r *Repo) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = r.Path
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// Init initializes a new git repository at r.Path.
func (r *Repo) Init() error {
	if err := os.MkdirAll(r.Path, 0755); err != nil {
		return fmt.Errorf("creating directory: %w", err)
	}
	_, err := r.run(context.Background(), "init", "-b", "main")
	if err != nil {
		// Try without -b for older git versions
		_, err = r.run(context.Background(), "init")
	}
	return err
}

// ConfigureUser sets the local git user for commits.
func (r *Repo) ConfigureUser(name, email string) error {
	if _, err := r.run(context.Background(), "config", "user.name", name); err != nil {
		return err
	}
	_, err := r.run(context.Background(), "config", "user.email", email)
	return err
}

// Add stages the given paths (or "." for all).
func (r *Repo) Add(paths ...string) error {
	args := append([]string{"add"}, paths...)
	_, err := r.run(context.Background(), args...)
	return err
}

// CommitInfo holds a single git log entry.
type CommitInfo struct {
	SHA     string
	Author  string
	Date    string
	Message string
}

// Commit creates a new commit. Returns the new SHA.
func (r *Repo) Commit(message string) (string, error) {
	_, err := r.run(context.Background(), "commit", "-m", message)
	if err != nil {
		return "", err
	}
	return r.CurrentSHA()
}

// CurrentSHA returns the SHA of the current HEAD commit.
func (r *Repo) CurrentSHA() (string, error) {
	return r.run(context.Background(), "rev-parse", "HEAD")
}

// CurrentBranch returns the name of the current branch.
func (r *Repo) CurrentBranch() (string, error) {
	return r.run(context.Background(), "rev-parse", "--abbrev-ref", "HEAD")
}

// Log returns the last n commit log entries.
func (r *Repo) Log(n int) ([]CommitInfo, error) {
	return r.LogCtx(context.Background(), n)
}

// LogCtx returns the last n commit log entries; ctx cancellation kills git.
func (r *Repo) LogCtx(ctx context.Context, n int) ([]CommitInfo, error) {
	out, err := r.run(ctx,
		"log",
		fmt.Sprintf("-%d", n),
		"--pretty=format:%H|%an|%ad|%s",
		"--date=short",
	)
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	var commits []CommitInfo
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "|", 4)
		if len(parts) != 4 {
			continue
		}
		commits = append(commits, CommitInfo{
			SHA:     parts[0],
			Author:  parts[1],
			Date:    parts[2],
			Message: parts[3],
		})
	}
	return commits, nil
}

// Diff returns the unstaged diff of the repository.
func (r *Repo) Diff() (string, error) {
	return r.run(context.Background(), "diff")
}

// DiffCached returns the staged diff.
func (r *Repo) DiffCached() (string, error) {
	return r.run(context.Background(), "diff", "--cached")
}

// Status returns the short git status output.
func (r *Repo) Status() (string, error) {
	return r.run(context.Background(), "status", "--short")
}

// HasChanges returns true if there are uncommitted changes.
func (r *Repo) HasChanges() (bool, error) {
	out, err := r.Status()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// Checkout switches to the given branch, creating it if create is true.
//
// `git checkout -b NAME` rejects a `--` separator before NAME, so we rely
// on validateRef alone to keep the branch name out of git's flag namespace.
func (r *Repo) Checkout(branch string, create bool) error {
	if err := validateRef(branch); err != nil {
		return err
	}
	args := []string{"checkout"}
	if create {
		args = append(args, "-b")
	}
	args = append(args, branch)
	_, err := r.run(context.Background(), args...)
	return err
}

// Revert creates a revert commit for the given SHA.
func (r *Repo) Revert(sha string) error {
	return r.RevertCtx(context.Background(), sha)
}

// RevertCtx creates a revert commit; ctx cancellation kills git.
// SHA is validated against refRE so it can't smuggle a git flag.
func (r *Repo) RevertCtx(ctx context.Context, sha string) error {
	if err := validateRef(sha); err != nil {
		return err
	}
	_, err := r.run(ctx, "revert", "--no-edit", "--", sha)
	return err
}

// ResetHard resets to a given ref (use with care).
func (r *Repo) ResetHard(ref string) error {
	return r.ResetHardCtx(context.Background(), ref)
}

// ResetHardCtx resets to ref; ctx cancellation kills git.
// Ref is validated against refRE so it can't smuggle a git flag.
func (r *Repo) ResetHardCtx(ctx context.Context, ref string) error {
	if err := validateRef(ref); err != nil {
		return err
	}
	// `git reset --hard` doesn't accept the `--` positional separator
	// (it's not a pathspec command), so we rely on validateRef alone.
	_, err := r.run(ctx, "reset", "--hard", ref)
	return err
}

// Clone clones a git repository into dest. If branch is non-empty, only that
// branch is cloned (--branch). The dest directory must not already exist.
//
// Per-protocol config restricts which transports git will follow, limiting
// drive-by exposure if `url` ever flows from an untrusted source. These
// `--config` values apply only to this single invocation.
func Clone(url, dest, branch string) error {
	return CloneCtx(context.Background(), url, dest, branch)
}

// CloneCtx is Clone with a context for cancellation.
func CloneCtx(ctx context.Context, url, dest, branch string) error {
	args := []string{
		"-c", "protocol.https.allow=always",
		"-c", "protocol.file.allow=user",
		"-c", "protocol.http.allow=user",
		"-c", "protocol.git.allow=never",
		"-c", "protocol.ssh.allow=user",
		"clone",
	}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	args = append(args, "--", url, dest)

	cmd := exec.CommandContext(ctx, "git", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git clone %s: %w: %s", url, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// SyncCtx fetches updates for an existing clone, optionally checks out ref,
// and fast-forwards the current branch when attached to one.
func (r *Repo) SyncCtx(ctx context.Context, ref string) error {
	if _, err := r.run(ctx, "fetch", "--all", "--tags", "--prune"); err != nil {
		return err
	}
	if ref != "" && ref != "current" {
		if err := validateRef(ref); err != nil {
			return err
		}
		if _, err := r.run(ctx, "checkout", ref); err != nil {
			return err
		}
	}
	branch, err := r.run(ctx, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return err
	}
	if branch == "HEAD" {
		return nil
	}
	_, err = r.run(ctx, "pull", "--ff-only")
	return err
}

// IsRepo returns true if the path is a git repository.
func IsRepo(path string) bool {
	cmd := exec.Command("git", "-C", path, "rev-parse", "--git-dir")
	return cmd.Run() == nil
}

// HasInitialCommit returns true if the repo has at least one commit.
func (r *Repo) HasInitialCommit() bool {
	_, err := r.run(context.Background(), "rev-parse", "HEAD")
	return err == nil
}
