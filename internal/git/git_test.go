package git

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/internal/logctx"
)

func TestClientRunTracesCommandWithRedactedArgs(t *testing.T) {
	var logs bytes.Buffer
	ctx := logctx.With(t.Context(), slog.New(logctx.NewCLIHandler(&logs, slog.LevelDebug)))
	client := Client{Bin: "sh"}
	_, err := client.Run(ctx, "", "-c", "exit 0", "--token", "secret", "https://user:password@example.com/repo")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	got := logs.String()
	if !strings.Contains(got, "exec sh -c exit 0 --token *** https://***@example.com/repo") ||
		!strings.Contains(got, "exec finished duration=") {
		t.Fatalf("trace output = %q", got)
	}
	if strings.Contains(got, "secret") || strings.Contains(got, "password") || strings.Contains(got, "user") {
		t.Fatalf("trace output leaked secret data: %q", got)
	}
}

func TestPushRemoteResolution(t *testing.T) {
	isolateGitConfig(t)
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

func TestPushRemoteUsesNativeGitConfigFallbacks(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()

	t.Run("global push default", func(t *testing.T) {
		isolateGitConfig(t)
		repo := filepath.Join(base, "global-config-repo")
		runGit(t, "", "init", repo)
		runGit(t, repo, "config", "user.email", "test@example.com")
		runGit(t, repo, "config", "user.name", "Test User")
		mustWriteFile(t, filepath.Join(repo, "README.md"), "hello\n")
		runGit(t, repo, "add", "README.md")
		runGit(t, repo, "commit", "-m", "initial")
		runGit(t, repo, "branch", "-M", "main")
		runGit(t, repo, "remote", "add", "origin", filepath.Join(base, "origin.git"))
		runGit(t, repo, "remote", "add", "fork", filepath.Join(base, "fork.git"))

		globalConfig := filepath.Join(base, "global.gitconfig")
		mustWriteFile(t, globalConfig, "[remote]\n\tpushDefault = fork\n")
		t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)

		client := New()
		if got := pushRemote(t, client, ctx, repo); got != "fork" {
			t.Fatalf("PushRemote() with global remote.pushDefault = %q, want fork", got)
		}
	})

	t.Run("environment config overrides repo config", func(t *testing.T) {
		isolateGitConfig(t)
		repo := filepath.Join(base, "env-config-repo")
		runGit(t, "", "init", repo)
		runGit(t, repo, "config", "user.email", "test@example.com")
		runGit(t, repo, "config", "user.name", "Test User")
		mustWriteFile(t, filepath.Join(repo, "README.md"), "hello\n")
		runGit(t, repo, "add", "README.md")
		runGit(t, repo, "commit", "-m", "initial")
		runGit(t, repo, "branch", "-M", "main")
		runGit(t, repo, "remote", "add", "origin", filepath.Join(base, "origin.git"))
		runGit(t, repo, "remote", "add", "fork", filepath.Join(base, "fork.git"))
		runGit(t, repo, "config", "remote.pushDefault", "origin")

		t.Setenv("GIT_CONFIG_COUNT", "1")
		t.Setenv("GIT_CONFIG_KEY_0", "remote.pushDefault")
		t.Setenv("GIT_CONFIG_VALUE_0", "fork")

		client := New()
		if got := pushRemote(t, client, ctx, repo); got != "fork" {
			t.Fatalf("PushRemote() with env remote.pushDefault = %q, want fork", got)
		}
	})

	t.Run("worktree branch push remote", func(t *testing.T) {
		isolateGitConfig(t)
		repo := filepath.Join(base, "worktree-config-repo")
		wt := filepath.Join(base, "worktree-config-wt")
		runGit(t, "", "init", repo)
		runGit(t, repo, "config", "user.email", "test@example.com")
		runGit(t, repo, "config", "user.name", "Test User")
		mustWriteFile(t, filepath.Join(repo, "README.md"), "hello\n")
		runGit(t, repo, "add", "README.md")
		runGit(t, repo, "commit", "-m", "initial")
		runGit(t, repo, "branch", "-M", "main")
		runGit(t, repo, "remote", "add", "origin", filepath.Join(base, "origin.git"))
		runGit(t, repo, "remote", "add", "fork", filepath.Join(base, "fork.git"))
		runGit(t, repo, "worktree", "add", "-q", "-b", "workspace/feature", wt)
		runGit(t, wt, "config", "extensions.worktreeConfig", "true")
		runGit(t, wt, "config", "--worktree", "branch.workspace/feature.pushRemote", "fork")

		client := New()
		if got := pushRemote(t, client, ctx, wt); got != "fork" {
			t.Fatalf("PushRemote() with worktree branch pushRemote = %q, want fork", got)
		}
	})
}

func isolateGitConfig(t *testing.T) {
	t.Helper()
	globalConfig := filepath.Join(t.TempDir(), "global.gitconfig")
	mustWriteFile(t, globalConfig, "")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

func TestSyncBaseRefPrefersRemoteForSlashBranchNames(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	repo := filepath.Join(base, "repo")
	runGit(t, "", "init", "--bare", remote)
	runGit(t, "", "clone", remote, repo)
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test User")
	mustWriteFile(t, filepath.Join(repo, "README.md"), "hello\n")
	runGit(t, repo, "add", "README.md")
	runGit(t, repo, "commit", "-m", "initial")
	runGit(t, repo, "branch", "-M", "main")
	runGit(t, repo, "push", "-u", "origin", "main")
	runGit(t, repo, "switch", "-c", "release/2026-05")
	mustWriteFile(t, filepath.Join(repo, "release.txt"), "release\n")
	runGit(t, repo, "add", "release.txt")
	runGit(t, repo, "commit", "-m", "release branch")
	runGit(t, repo, "push", "-u", "origin", "release/2026-05")
	runGit(t, repo, "switch", "main")
	runGit(t, repo, "branch", "-D", "release/2026-05")

	client := New()
	if err := client.Fetch(ctx, repo); err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	got, err := client.SyncBaseRef(ctx, repo, "release/2026-05")
	if err != nil {
		t.Fatalf("SyncBaseRef() error = %v", err)
	}
	if got != "origin/release/2026-05" {
		t.Fatalf("SyncBaseRef() = %q, want origin/release/2026-05", got)
	}
}

func TestReadOnlyQueriesFallbackForWorktreeConfigExtension(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	wt := filepath.Join(base, "wt")
	runGit(t, "", "init", "-q", repo)
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test User")
	mustWriteFile(t, filepath.Join(repo, "file.txt"), "hello\n")
	runGit(t, repo, "add", "file.txt")
	runGit(t, repo, "commit", "-q", "-m", "initial")

	client := New()
	ref, err := client.CurrentRef(ctx, repo)
	if err != nil {
		t.Fatalf("base CurrentRef() error = %v", err)
	}
	runGit(t, repo, "worktree", "add", "-q", "-b", "workspace/feature", wt)
	runGit(t, wt, "config", "extensions.worktreeConfig", "true")

	current, err := client.CurrentRef(ctx, wt)
	if err != nil {
		t.Fatalf("CurrentRef() error = %v", err)
	}
	if current != "workspace/feature" {
		t.Fatalf("CurrentRef() = %q, want workspace/feature", current)
	}
	dirty, err := client.Dirty(ctx, wt)
	if err != nil {
		t.Fatalf("Dirty() error = %v", err)
	}
	if dirty {
		t.Fatal("Dirty() = true, want false")
	}
	ahead, behind, err := client.AheadBehind(ctx, wt, ref)
	if err != nil {
		t.Fatalf("AheadBehind() error = %v", err)
	}
	if ahead != 0 || behind != 0 {
		t.Fatalf("AheadBehind() = (%d, %d), want (0, 0)", ahead, behind)
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
