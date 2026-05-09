package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPushRemoteResolution(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	runGit(t, "", "init", repo)
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test User")
	mustWriteFile(t, filepath.Join(repo, "README.md"), "hello\n")
	runGit(t, repo, "add", "README.md")
	runGit(t, repo, "commit", "-m", "initial")
	runGit(t, repo, "branch", "-M", "main")
	runGit(t, repo, "remote", "add", "origin", filepath.Join(base, "origin.git"))
	runGit(t, repo, "remote", "add", "fork", filepath.Join(base, "fork.git"))

	client := New()

	runGit(t, repo, "config", "branch.main.pushRemote", "fork")
	if got := pushRemote(t, client, ctx, repo); got != "fork" {
		t.Fatalf("PushRemote() with branch pushRemote = %q, want fork", got)
	}

	runGit(t, repo, "config", "--unset", "branch.main.pushRemote")
	runGit(t, repo, "config", "remote.pushDefault", "fork")
	if got := pushRemote(t, client, ctx, repo); got != "fork" {
		t.Fatalf("PushRemote() with remote.pushDefault = %q, want fork", got)
	}

	runGit(t, repo, "config", "--unset", "remote.pushDefault")
	runGit(t, repo, "config", "branch.main.remote", "fork")
	if got := pushRemote(t, client, ctx, repo); got != "fork" {
		t.Fatalf("PushRemote() with branch remote = %q, want fork", got)
	}

	runGit(t, repo, "config", "--unset", "branch.main.remote")
	if got := pushRemote(t, client, ctx, repo); got != "origin" {
		t.Fatalf("PushRemote() with origin fallback = %q, want origin", got)
	}

	runGit(t, repo, "remote", "remove", "origin")
	if got := pushRemote(t, client, ctx, repo); got != "fork" {
		t.Fatalf("PushRemote() with sole remote = %q, want fork", got)
	}

	runGit(t, repo, "remote", "add", "upstream", filepath.Join(base, "upstream.git"))
	if _, err := client.PushRemote(ctx, repo); err == nil || !strings.Contains(err.Error(), "multiple git remotes") {
		t.Fatalf("PushRemote() with ambiguous remotes error = %v, want multiple remotes error", err)
	}
}

func pushRemote(t *testing.T, client Client, ctx context.Context, repo string) string {
	t.Helper()
	remote, err := client.PushRemote(ctx, repo)
	if err != nil {
		t.Fatalf("PushRemote() error = %v", err)
	}
	return remote
}

func mustWriteFile(t *testing.T, path string, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v error = %v: %s", args, err, out)
	}
}
