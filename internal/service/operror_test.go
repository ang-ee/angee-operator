package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/internal/git"
	"github.com/ang-ee/angee-operator/internal/logctx"
	"github.com/ang-ee/angee-operator/internal/manifest"
)

// git's own words are quoted, not interpreted: its first fatal: or error:
// line with the line before it, where git relays what ssh or the remote said.
// The samples are git's output as captured from the failures they name.
func TestGitErrorLineQuotesGit(t *testing.T) {
	for _, tc := range []struct {
		name     string
		output   string
		wantLine string
	}{
		{"busybox sh without ssh", "ssh -o BatchMode=yes: line 0: ssh: not found\nfatal: Could not read from remote repository.\n\nPlease make sure you have the correct access rights\nand the repository exists.\n", "ssh -o BatchMode=yes: line 0: ssh: not found fatal: Could not read from remote repository."},
		{"rejected key", "Cloning into '/stack/sources/probe'...\ngit@github.com: Permission denied (publickey).\r\nfatal: Could not read from remote repository.\n", "git@github.com: Permission denied (publickey). fatal: Could not read from remote repository."},
		{"https without DNS", "fatal: unable to access 'https://nonexistent.invalid/x.git/': Could not resolve host: nonexistent.invalid\n", "fatal: unable to access 'https://nonexistent.invalid/x.git/': Could not resolve host: nonexistent.invalid"},
		{"advice skipped", "error: could not apply 1a2b3c4... Handle connection refused\nhint: Resolve all conflicts manually\n", "error: could not apply 1a2b3c4... Handle connection refused"},
		{"no fatal line", "warning: something odd\n", "warning: something odd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := &git.CommandError{Args: []string{"fetch"}, Output: tc.output, Err: errors.New("exit status 128")}
			if line := gitErrorLine(err); line != tc.wantLine {
				t.Fatalf("gitErrorLine() = %q, want %q", line, tc.wantLine)
			}
		})
	}
}

// A repair outcome's reason is what failed and git's line, on one line,
// without the operation's title.
func TestRepairReasonUsesSummaryAndGitLine(t *testing.T) {
	err := gitFailure(&git.CommandError{Args: []string{"clone"}, Output: "fatal: repository not found\n", Err: errors.New("exit status 128")},
		gitStep{action: "cloning", object: `source "lib"`, remote: "https://example.com/lib.git", cause: CauseCloneFailed})
	operation{name: "workspace.repair", title: `Repair for workspace "a"`}.annotate(&err)
	if got, want := repairReason(err), `cloning source "lib" (https://example.com/lib.git) failed. git: fatal: repository not found`; got != want {
		t.Fatalf("repairReason() = %q, want %q", got, want)
	}
}

// A remote URL that does not parse still has its credential masked.
func TestGitFailureMasksAnUnparseableRemote(t *testing.T) {
	remote := "https://user:p%zzword@example.com/x.git"
	err := gitFailure(&git.CommandError{Args: []string{"fetch"}, Output: "fatal: unable to access\n", Err: errors.New("exit status 128")},
		gitStep{action: "fetching", object: `source "app"`, remote: remote})
	if opErr := AsOperationError(err); strings.Contains(err.Error(), "p%zzword") || strings.Contains(opErr.Remote, "p%zzword") {
		t.Fatalf("error = %q, remote %q; want the credential masked", err.Error(), opErr.Remote)
	}
}

// Every slot is fetched before any is merged: a slot whose fetch fails
// refuses the sync with the slot that sorts first unchanged.
func TestWorkspaceSyncBaseFetchesEverySlotBeforeMerging(t *testing.T) {
	fixture := newTwoSlotFixture(t)
	seed := filepath.Join(fixture.base, "seed")
	commitFile(t, seed, "upstream.txt")
	runGit(t, seed, "push", "origin", "main")
	runGit(t, filepath.Join(fixture.root, ".cache", "lib"), "remote", "set-url", "origin", filepath.Join(fixture.base, "missing.git"))
	before := slotSnapshot(t, fixture.slotPath("app"))

	_, err := fixture.platform.WorkspaceSyncBase(context.Background(), "feature-a", "merge")
	if opErr := AsOperationError(err); opErr == nil || opErr.Code != CodeGitFailed || opErr.Slot != "lib" {
		t.Fatalf("WorkspaceSyncBase() error = %+v, want lib's fetch to fail", opErr)
	}
	if after := slotSnapshot(t, fixture.slotPath("app")); after != before {
		t.Fatalf("app changed from %q to %q; want no slot merged", before, after)
	}
}

// Push refuses a detached slot with work of its own before pushing any slot.
func TestWorkspacePushRefusesDetachedSlotBeforePushingAny(t *testing.T) {
	fixture := newRepairFixture(t)
	libRemote := fixture.seedRemote(t, "lib")
	// A slot without a branch is cut on a detached HEAD.
	fixture.declareSlot(t, "lib", &manifest.Source{Kind: "git", Repo: libRemote, DefaultRef: "main", CachePath: ".cache/lib"},
		manifest.WorkspaceSource{Source: "lib", Mode: manifest.WorkspaceSourceModeWorktree, Ref: "main", Subpath: "lib"})
	if _, err := fixture.platform.WorkspaceRepair(context.Background(), "feature-a"); err != nil {
		t.Fatalf("WorkspaceRepair() error = %v", err)
	}
	app := fixture.slotPath("app")
	commitFile(t, app, "app-work.txt")
	lib := fixture.slotPath("lib")
	commitFile(t, lib, "lib-work.txt")
	remoteBefore := strings.TrimSpace(runGitOutput(t, filepath.Join(fixture.base, "remote.git"), "for-each-ref"))

	_, err := fixture.platform.WorkspacePush(context.Background(), "feature-a", "")
	if opErr := AsOperationError(err); opErr == nil || opErr.Cause != CauseDetachedHead || opErr.Slot != "lib" {
		t.Fatalf("WorkspacePush() error = %+v, want detached_head on lib", opErr)
	}
	if after := strings.TrimSpace(runGitOutput(t, filepath.Join(fixture.base, "remote.git"), "for-each-ref")); after != remoteBefore {
		t.Fatalf("app's remote changed from %q to %q; want nothing pushed", remoteBefore, after)
	}
}

// The message names the operation, the target and the remote, says what
// failed and what to do, and ends with git's own line; the whole output is
// the detail.
func TestGitFailureFollowsTheMessageShape(t *testing.T) {
	output := "ssh -o BatchMode=yes: line 0: ssh: not found\nfatal: Could not read from remote repository.\n"
	err := gitFailure(&git.CommandError{Args: []string{"fetch", "--all", "--prune"}, Output: output, Err: errors.New("exit status 128")},
		gitStep{action: "fetching", object: `source "angee-arp"`, remote: "git@github-arpee:ang-ee/angee-arp.git", cause: CauseFetchFailed, slot: "arp", source: "angee-arp"})
	operation{name: "workspace.sync-base", title: `Sync base for workspace "src"`, workspace: "src"}.annotate(&err)

	want := `Sync base for workspace "src": fetching source "angee-arp" (git@github-arpee:ang-ee/angee-arp.git) failed. ` +
		"git: ssh -o BatchMode=yes: line 0: ssh: not found fatal: Could not read from remote repository."
	if err.Error() != want {
		t.Fatalf("message =\n%s\nwant\n%s", err.Error(), want)
	}
	opErr := AsOperationError(err)
	if opErr.Code != CodeGitFailed || opErr.Cause != CauseFetchFailed || opErr.Operation != "workspace.sync-base" ||
		opErr.Workspace != "src" || opErr.Slot != "arp" || opErr.Source != "angee-arp" || opErr.Remote != "git@github-arpee:ang-ee/angee-arp.git" {
		t.Fatalf("error = %+v, want GIT_FAILED/fetch_failed with its context", opErr)
	}
	if opErr.Detail != strings.TrimSpace(output) || opErr.Hint != "" {
		t.Fatalf("detail = %q, hint = %q; want git's output and no hint of angee's", opErr.Detail, opErr.Hint)
	}
}

// The remote is masked where it enters the error, and the detail keeps the
// last 4 KiB of git's output (redacted where git ran, as git.Run does).
func TestGitFailureRedactsAndCapsDetail(t *testing.T) {
	long := strings.Repeat("remote: progress line\n", 400) + "fatal: Authentication failed for 'https://user:s3cret@example.com/x.git/'\n"
	err := gitFailure(&git.CommandError{Args: []string{"push"}, Output: logctx.RedactText(long), Err: errors.New("exit status 128")},
		gitStep{action: "pushing", object: `source "app"`, remote: "https://user:s3cret@example.com/x.git", cause: CausePushFailed})
	opErr := AsOperationError(err)
	if strings.Contains(err.Error(), "s3cret") || strings.Contains(opErr.Remote, "s3cret") || strings.Contains(opErr.Detail, "s3cret") {
		t.Fatalf("error leaks the credential: %q / %q / %q", err.Error(), opErr.Remote, opErr.Detail)
	}
	if opErr.Cause != CausePushFailed || opErr.Remote != "https://***@example.com/x.git" {
		t.Fatalf("cause %q, remote %q; want push_failed and a masked remote", opErr.Cause, opErr.Remote)
	}
	if len(opErr.Detail) > maxErrorDetail || !strings.HasSuffix(opErr.Detail, "https://***@example.com/x.git/'") {
		t.Fatalf("detail is %d bytes ending %q, want at most %d ending with git's last line", len(opErr.Detail), opErr.Detail[max(0, len(opErr.Detail)-60):], maxErrorDetail)
	}
}

// A legacy error keeps its message and gains a code; anything else is
// INTERNAL with the operation's title; an operation error wrapped in text is
// returned as itself, so the title starts the message.
func TestAnnotateClassifiesEveryError(t *testing.T) {
	op := operation{name: "source.pull", title: `Pull for source "app"`, source: "app"}
	for _, tc := range []struct {
		name        string
		err         error
		wantCode    string
		wantMessage string
	}{
		{"not found", &NotFoundError{Kind: "source", Name: "app"}, CodeNotFound, `source "app" is not declared`},
		{"invalid input", &InvalidInputError{Field: "ref", Reason: "ref is required"}, CodeInvalidInput, "ref: ref is required"},
		{"conflict", &ConflictError{Kind: "stack", Name: "x", Reason: "busy"}, CodeConflict, "stack x conflicts: busy"},
		{"internal", errors.New("disk full"), CodeInternal, `Pull for source "app": disk full`},
		{"deadline", fmt.Errorf("wait: %w", context.DeadlineExceeded), CodeTimeout, `Pull for source "app": wait: context deadline exceeded`},
		{"wrapped operation error", fmt.Errorf("outer: %w", &OperationError{Code: CodePreconditionFailed, Summary: "it is dirty"}), CodePreconditionFailed, `Pull for source "app": it is dirty`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.err
			op.annotate(&err)
			opErr, ok := err.(*OperationError)
			if !ok {
				t.Fatalf("annotate() = %T, want *OperationError", err)
			}
			if opErr.Code != tc.wantCode || err.Error() != tc.wantMessage || opErr.Operation != "source.pull" || opErr.Source != "app" {
				t.Fatalf("annotate() = %+v (%q), want code %s, message %q, operation and source set", opErr, err.Error(), tc.wantCode, tc.wantMessage)
			}
			if !errors.Is(err, tc.err) && !errors.As(tc.err, new(*OperationError)) {
				t.Fatalf("annotate() lost the original error")
			}
		})
	}
}

// Sync-base checks every slot before touching any: a dirty slot refuses the
// whole sync, naming its changed files, and the clean slot that sorts first
// is not synced.
func TestWorkspaceSyncBaseRefusesDirtySlotBeforeSyncingAny(t *testing.T) {
	ctx := context.Background()
	fixture := newTwoSlotFixture(t)
	// Move app's base on, so syncing app would change it.
	seed := filepath.Join(fixture.base, "seed")
	commitFile(t, seed, "upstream.txt")
	runGit(t, seed, "push", "origin", "main")
	mustWriteFile(t, filepath.Join(fixture.slotPath("lib"), "README.md"), "edited\n")
	mustWriteFile(t, filepath.Join(fixture.slotPath("lib"), "new file.txt"), "new\n")
	before := slotSnapshot(t, fixture.slotPath("app"))

	_, err := fixture.platform.WorkspaceSyncBase(ctx, "feature-a", "merge")
	opErr := AsOperationError(err)
	if opErr == nil || opErr.Code != CodePreconditionFailed || opErr.Cause != CauseUncommittedChanges || opErr.Slot != "lib" ||
		!slices.Equal(opErr.Paths, []string{"README.md", "new file.txt"}) {
		t.Fatalf("WorkspaceSyncBase() error = %+v, want uncommitted_changes on lib listing its files", opErr)
	}
	want := `Sync base for workspace "feature-a": source slot "lib" has uncommitted changes in README.md, new file.txt. Commit or restore the changes, then retry.`
	if err.Error() != want {
		t.Fatalf("message = %q, want %q", err.Error(), want)
	}
	if after := slotSnapshot(t, fixture.slotPath("app")); after != before {
		t.Fatalf("app changed from %q to %q; want no slot synced", before, after)
	}
}

// A declared slot missing on disk is PRECONDITION_FAILED with cause
// slot_missing, and still the legacy ConflictError underneath.
func TestWorkspaceSyncBaseMissingSlotIsAPrecondition(t *testing.T) {
	fixture := newRepairFixture(t)
	libRemote := fixture.seedRemote(t, "lib")
	fixture.declareSlot(t, "lib", &manifest.Source{Kind: "git", Repo: libRemote, DefaultRef: "main", CachePath: ".cache/lib"},
		manifest.WorkspaceSource{Source: "lib", Mode: manifest.WorkspaceSourceModeWorktree, Branch: "feature-a", Ref: "main", Subpath: "lib"})

	_, err := fixture.platform.WorkspaceSyncBase(context.Background(), "feature-a", "merge")
	assertMissingSlotError(t, err, "feature-a", `source slot "lib"`)
	opErr := AsOperationError(err)
	if opErr.Code != CodePreconditionFailed || opErr.Cause != CauseSlotMissing || opErr.Slot != "lib" ||
		opErr.Hint != "Run `angee workspace repair feature-a`." || !strings.HasPrefix(err.Error(), `Sync base for workspace "feature-a": source slot "lib" is missing`) {
		t.Fatalf("WorkspaceSyncBase() error = %+v (%q), want slot_missing on lib", opErr, err.Error())
	}
}

// An SSH remote with no ssh to run fails the fetch, and the message carries
// what git and the shell said; angee does not diagnose SSH.
func TestWorkspaceSyncBasePassesGitsWordsThrough(t *testing.T) {
	fixture := newRepairFixture(t)
	runGit(t, filepath.Join(fixture.root, ".cache", "app"), "remote", "set-url", "origin", "git@github-deploy:org/app.git")
	pathWithoutSSH(t)

	_, err := fixture.platform.WorkspaceSyncBase(context.Background(), "feature-a", "merge")
	opErr := AsOperationError(err)
	if opErr == nil || opErr.Code != CodeGitFailed || opErr.Cause != CauseFetchFailed || opErr.Remote != "git@github-deploy:org/app.git" || opErr.Hint != "" {
		t.Fatalf("WorkspaceSyncBase() error = %+v, want fetch_failed naming the SSH remote", opErr)
	}
	if want := `Sync base for workspace "feature-a": fetching source "app" (git@github-deploy:org/app.git) failed. git: `; !strings.HasPrefix(err.Error(), want) || !strings.Contains(opErr.GitLine, "ssh") {
		t.Fatalf("message = %q, want it to start %q", err.Error(), want)
	}
}

// A remote that cannot be reached fails the fetch with host_unreachable and
// git's line, after the preflight passed.
func TestWorkspaceSyncBaseReportsUnreachableRemote(t *testing.T) {
	fixture := newRepairFixture(t)
	runGit(t, filepath.Join(fixture.root, ".cache", "app"), "remote", "set-url", "origin", "https://nonexistent.invalid/app.git")
	t.Setenv("ANGEE_GIT_TIMEOUT", "30s")

	_, err := fixture.platform.WorkspaceSyncBase(context.Background(), "feature-a", "merge")
	opErr := AsOperationError(err)
	if opErr == nil || opErr.Code != CodeGitFailed || opErr.Cause != CauseFetchFailed || opErr.Slot != "app" || opErr.Source != "app" ||
		opErr.Remote != "https://nonexistent.invalid/app.git" || !strings.Contains(opErr.GitLine, "nonexistent.invalid") || opErr.Detail == "" {
		t.Fatalf("WorkspaceSyncBase() error = %+v, want fetch_failed with git's line", opErr)
	}
	if want := `Sync base for workspace "feature-a": fetching source "app" (https://nonexistent.invalid/app.git) failed. git: fatal: unable to access`; !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("message = %q, want it to start %q", err.Error(), want)
	}
}

// A merge that stops on conflicts is merge_conflict, read from the index
// (git ls-files -u), with the conflicted files.
func TestWorkspaceSyncBaseReportsMergeConflict(t *testing.T) {
	fixture := newRepairFixture(t)
	seed := filepath.Join(fixture.base, "seed")
	mustWriteFile(t, filepath.Join(seed, "README.md"), "upstream change\n")
	runGit(t, seed, "commit", "-am", "upstream readme")
	runGit(t, seed, "push", "origin", "main")
	app := fixture.slotPath("app")
	mustWriteFile(t, filepath.Join(app, "README.md"), "slot change\n")
	runGit(t, app, "-c", "user.email=test@example.com", "-c", "user.name=Test User", "commit", "-am", "slot readme")

	_, err := fixture.platform.WorkspaceSyncBase(context.Background(), "feature-a", "merge")
	opErr := AsOperationError(err)
	if opErr == nil || opErr.Code != CodeGitFailed || opErr.Cause != CauseMergeConflict || !slices.Equal(opErr.Paths, []string{"README.md"}) {
		t.Fatalf("WorkspaceSyncBase() error = %+v, want merge_conflict on README.md", opErr)
	}
	if want := `Sync base for workspace "feature-a": merging origin/main into source slot "app" stopped on conflicts in README.md.`; !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("message = %q, want it to start %q", err.Error(), want)
	}
}

// A job run whose dependent fails names the dependent and why on its
// receipt, with code JOB_FAILED and cause dependency_failed, instead of "one
// or more dependents failed". A node blocked by that failure is not repeated.
func TestJobRunNamesTheFailedDependent(t *testing.T) {
	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "jobs",
		Jobs: map[string]manifest.Job{
			"root":  {Runtime: manifest.RuntimeLocal, Command: []string{"root"}},
			"child": {Runtime: manifest.RuntimeLocal, Command: []string{"child"}, DependsOn: []string{"root"}},
		},
		Services: map[string]manifest.Service{
			"web": {Runtime: manifest.RuntimeLocal, Command: []string{"web"}, DependsOn: []string{"child"}},
		},
	}
	backend := &recordingJobBackend{failJobs: map[string]error{"child": errors.New("exit status 3\nlast output line")}}
	p, id := newJobOperationPlatform(t, stack, backend, "root", true)

	p.executeJobRun(context.Background(), id, func() {})

	op, err := p.JobRunGet(context.Background(), id)
	if err != nil {
		t.Fatalf("JobRunGet: %v", err)
	}
	if op.ErrorCode != CodeJobFailed || op.ErrorCause != CauseDependencyFailed || op.Error != `job "child" failed: exit status 3` {
		t.Fatalf("receipt error = %q (%s/%s), want the failed dependent named with JOB_FAILED/dependency_failed", op.Error, op.ErrorCode, op.ErrorCause)
	}
}

// newTwoSlotFixture is the repair fixture with a second worktree slot, "lib",
// materialized by repair.
func newTwoSlotFixture(t *testing.T) *repairFixture {
	t.Helper()
	fixture := newRepairFixture(t)
	libRemote := fixture.seedRemote(t, "lib")
	fixture.declareSlot(t, "lib", &manifest.Source{Kind: "git", Repo: libRemote, DefaultRef: "main", CachePath: ".cache/lib"},
		manifest.WorkspaceSource{Source: "lib", Mode: manifest.WorkspaceSourceModeWorktree, Branch: "feature-a", Ref: "main", Subpath: "lib"})
	if _, err := fixture.platform.WorkspaceRepair(context.Background(), "feature-a"); err != nil {
		t.Fatalf("WorkspaceRepair() error = %v", err)
	}
	return fixture
}

// pathWithoutSSH leaves only git on PATH, and no GIT_SSH_COMMAND.
func pathWithoutSSH(t *testing.T) {
	t.Helper()
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("LookPath(git) error = %v", err)
	}
	bin := t.TempDir()
	if err := os.Symlink(gitBin, filepath.Join(bin, "git")); err != nil {
		t.Fatalf("Symlink(git) error = %v", err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("GIT_SSH_COMMAND", "")
	if err := os.Unsetenv("GIT_SSH_COMMAND"); err != nil {
		t.Fatalf("Unsetenv(GIT_SSH_COMMAND) error = %v", err)
	}
}
