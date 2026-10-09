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
	"time"

	"github.com/ang-ee/angee-operator/internal/logctx"
)

func TestClientRunNonInteractiveEnvironment(t *testing.T) {
	readEnv := func(t *testing.T, client Client) string {
		t.Helper()
		out, err := client.Run(t.Context(), "", "-c", `printf '%s|%s' "${GIT_TERMINAL_PROMPT-unset}" "${GIT_SSH_COMMAND-unset}"`)
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		return string(out)
	}

	t.Run("interactive preserves environment", func(t *testing.T) {
		t.Setenv("GIT_TERMINAL_PROMPT", "ask")
		t.Setenv("GIT_SSH_COMMAND", "custom-ssh")
		if got := readEnv(t, Client{Bin: "sh"}); got != "ask|custom-ssh" {
			t.Fatalf("environment = %q, want inherited values", got)
		}
	})

	t.Run("non-interactive adds defaults", func(t *testing.T) {
		unsetEnv(t, "GIT_TERMINAL_PROMPT")
		unsetEnv(t, "GIT_SSH_COMMAND")
		if got := readEnv(t, Client{Bin: "sh", NonInteractive: true}); got != "0|ssh -o BatchMode=yes" {
			t.Fatalf("environment = %q, want non-interactive defaults", got)
		}
	})

	t.Run("non-interactive preserves ssh command", func(t *testing.T) {
		t.Setenv("GIT_TERMINAL_PROMPT", "ask")
		t.Setenv("GIT_SSH_COMMAND", "ssh-wrapper --flag")
		if got := readEnv(t, Client{Bin: "sh", NonInteractive: true}); got != "0|ssh-wrapper --flag" {
			t.Fatalf("environment = %q, want prompt disabled and SSH command preserved", got)
		}
	})
}

func TestNetworkOperationTimeout(t *testing.T) {
	binDir := t.TempDir()
	gitPath := filepath.Join(binDir, "git")
	if err := os.WriteFile(gitPath, []byte("#!/bin/sh\nsleep 5 &\nwait\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(fake git) error = %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ANGEE_GIT_TIMEOUT", "20ms")

	dir := t.TempDir()
	started := time.Now()
	err := New().Fetch(t.Context(), dir)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Fetch() took %s, want bounded failure", elapsed)
	}
	want := "git fetch --all --prune in " + dir + " timed out after 20ms"
	if err == nil || err.Error() != want {
		t.Fatalf("Fetch() error = %v, want %q", err, want)
	}
	if !IsTimeout(err) {
		t.Fatalf("IsTimeout(%v) = false", err)
	}

	err = New().Push(t.Context(), dir, "main")
	want = "git push in " + dir + " timed out after 20ms"
	if err == nil || err.Error() != want {
		t.Fatalf("Push() error = %v, want %q", err, want)
	}
}

func TestCloneTimeoutRedactsRemoteURL(t *testing.T) {
	binDir := t.TempDir()
	gitPath := filepath.Join(binDir, "git")
	if err := os.WriteFile(gitPath, []byte("#!/bin/sh\nsleep 5 &\nwait\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(fake git) error = %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ANGEE_GIT_TIMEOUT", "20ms")

	dest := filepath.Join(t.TempDir(), "repo")
	err := New().Clone(t.Context(), "https://user:secret@example.com/repo.git", dest)
	want := "git clone https://***@example.com/repo.git in " + dest + " timed out after 20ms"
	if err == nil || err.Error() != want {
		t.Fatalf("Clone() error = %v, want %q", err, want)
	}
	if strings.Contains(err.Error(), "user") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("Clone() error leaked credentials: %v", err)
	}
}

func unsetEnv(t *testing.T, key string) {
	t.Helper()
	old, existed := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("Unsetenv(%s) error = %v", key, err)
	}
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(key, old)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

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

func TestEnsureRemoteURL(t *testing.T) {
	isolateGitConfig(t)
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "repo")
	runGit(t, "", "init", repo)
	client := New()
	const declared = "https://github.com/acme/app.git"

	// No origin yet: it is added, with the default fetch refspec.
	previous, changed, err := client.EnsureRemoteURL(ctx, repo, "origin", declared)
	if err != nil || !changed || previous != "" {
		t.Fatalf("EnsureRemoteURL(add) = (%q, %v, %v), want (\"\", true, nil)", previous, changed, err)
	}
	if got := configValue(t, repo, "remote.origin.fetch"); got != "+refs/heads/*:refs/remotes/origin/*" {
		t.Fatalf("remote.origin.fetch = %q, want the default refspec", got)
	}

	// An insteadOf rewrite makes `git remote get-url` differ from the declared
	// URL; the configured value still matches, so nothing changes.
	runGit(t, repo, "config", "url.git@github.com:.insteadOf", "https://github.com/")
	if previous, changed, err := client.EnsureRemoteURL(ctx, repo, "origin", declared); err != nil || changed || previous != declared {
		t.Fatalf("EnsureRemoteURL(same, rewritten) = (%q, %v, %v), want (%q, false, nil)", previous, changed, err, declared)
	}

	// A different URL replaces origin's and leaves other remotes alone.
	runGit(t, repo, "remote", "add", "upstream", "https://example.com/upstream.git")
	const moved = "git@github-deploy:acme/app.git"
	if previous, changed, err := client.EnsureRemoteURL(ctx, repo, "origin", moved); err != nil || !changed || previous != declared {
		t.Fatalf("EnsureRemoteURL(set) = (%q, %v, %v), want (%q, true, nil)", previous, changed, err, declared)
	}
	if got := configValue(t, repo, "remote.origin.url"); got != moved {
		t.Fatalf("remote.origin.url = %q, want %q", got, moved)
	}
	if got := configValue(t, repo, "remote.upstream.url"); got != "https://example.com/upstream.git" {
		t.Fatalf("remote.upstream.url = %q, want it unchanged", got)
	}
}

// A missing remote URL is not an error, but an unreadable config is: it must
// not pass for "no origin" and lead to a misleading `git remote add` failure.
func TestRemoteURLDistinguishesMissingFromUnreadable(t *testing.T) {
	isolateGitConfig(t)
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "repo")
	runGit(t, "", "init", repo)
	client := New()

	if value, ok, err := client.RemoteURL(ctx, repo, "origin"); err != nil || ok || value != "" {
		t.Fatalf("RemoteURL(missing) = (%q, %v, %v), want (\"\", false, nil)", value, ok, err)
	}
	mustWriteFile(t, filepath.Join(repo, ".git", "config"), "[core\nnot a config\n")
	if _, _, err := client.RemoteURL(ctx, repo, "origin"); err == nil {
		t.Fatal("RemoteURL(unreadable config) error = nil, want an error")
	}
}

// Run's error reaches CLI and API clients, so credentials in a URL argument
// or in git's output must not appear in it.
func TestClientRunErrorRedactsCredentials(t *testing.T) {
	// A stand-in git that echoes its arguments to stderr and fails, so both
	// the argument list and the output carry the credential.
	fakeGit := filepath.Join(t.TempDir(), "git")
	mustWriteFile(t, fakeGit, "#!/bin/sh\necho \"fatal: $*\" >&2\nexit 128\n")
	if err := os.Chmod(fakeGit, 0o755); err != nil {
		t.Fatalf("Chmod(fake git) error = %v", err)
	}
	_, err := Client{Bin: fakeGit}.Run(context.Background(), t.TempDir(), "remote", "add", "--", "origin", "https://angee:hunter22@example.com/app.git")
	if err == nil {
		t.Fatal("Run() error = nil")
	}
	if strings.Contains(err.Error(), "hunter22") {
		t.Fatalf("Run() error = %q, want the credential redacted", err)
	}
	if strings.Count(err.Error(), "https://***@example.com/app.git") != 2 {
		t.Fatalf("Run() error = %q, want the redacted URL in both the arguments and the output", err)
	}
}

// git fetches from a remote's first URL while `git config --get` returns the
// last; RemoteURL reports the first, and EnsureRemoteURL refuses to rewrite a
// remote with several URLs unless the first already matches.
func TestRemoteWithSeveralURLs(t *testing.T) {
	isolateGitConfig(t)
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "repo")
	runGit(t, "", "init", repo)
	runGit(t, repo, "remote", "add", "origin", "https://example.com/first.git")
	runGit(t, repo, "config", "--add", "remote.origin.url", "https://example.com/second.git")
	client := New()

	if value, ok, err := client.RemoteURL(ctx, repo, "origin"); err != nil || !ok || value != "https://example.com/first.git" {
		t.Fatalf("RemoteURL() = (%q, %v, %v), want the first URL", value, ok, err)
	}
	if _, changed, err := client.EnsureRemoteURL(ctx, repo, "origin", "https://example.com/first.git"); err != nil || changed {
		t.Fatalf("EnsureRemoteURL(first) = (changed %v, %v), want an unchanged no-op", changed, err)
	}
	if _, _, err := client.EnsureRemoteURL(ctx, repo, "origin", "https://example.com/other.git"); err == nil || !strings.Contains(err.Error(), "has 2 URLs") {
		t.Fatalf("EnsureRemoteURL(other) error = %v, want a refusal naming the URL count", err)
	}
}

func configValue(t *testing.T, dir, key string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "config", "--get", key).Output()
	if err != nil {
		t.Fatalf("git config --get %s error = %v", key, err)
	}
	return strings.TrimSpace(string(out))
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

// Values the client passes to git follow --end-of-options, so one shaped like
// an option is refused as an argument instead of run: a rebase ref of
// --exec=<cmd> would run the command for every commit replayed, a count base
// of --output=<file> would write the file, and a worktree ref of --detach
// would quietly change what is checked out.
func TestClientReadsValuesAsArgumentsOnly(t *testing.T) {
	t.Setenv("LC_ALL", "C") // git's messages are matched below
	ctx := context.Background()
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	repo := filepath.Join(base, "repo")
	runGit(t, "", "init", "--bare", "--initial-branch=main", remote)
	runGit(t, "", "clone", remote, repo)
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test User")
	runGit(t, repo, "switch", "-c", "main")
	mustWriteFile(t, filepath.Join(repo, "README.md"), "hello\n")
	runGit(t, repo, "add", "README.md")
	runGit(t, repo, "commit", "-m", "initial")
	runGit(t, repo, "push", "-u", "origin", "main")
	mustWriteFile(t, filepath.Join(repo, "change.txt"), "change\n")
	runGit(t, repo, "add", "change.txt")
	runGit(t, repo, "commit", "-m", "change")
	client := New()

	executed := filepath.Join(base, "executed")
	if err := client.Rebase(ctx, repo, "--exec=touch "+executed); err == nil {
		t.Error("Rebase() accepted an option as its ref")
	}
	if _, err := os.Stat(executed); !os.IsNotExist(err) {
		t.Errorf("Rebase() ran --exec (stat error = %v)", err)
	}
	if err := client.Merge(ctx, repo, "--exec=x"); err == nil || !strings.Contains(err.Error(), "not something we can merge") {
		t.Errorf("Merge() error = %v, want the option refused as a revision", err)
	}
	written := filepath.Join(base, "written")
	if _, _, err := client.AheadBehind(ctx, repo, "--output="+written); err == nil {
		t.Error("AheadBehind() accepted an option as its base")
	}
	if matches, _ := filepath.Glob(written + "*"); len(matches) != 0 {
		t.Errorf("AheadBehind() wrote %v", matches)
	}
	if err := client.WorktreeAdd(ctx, repo, filepath.Join(base, "worktree"), "--detach"); err == nil {
		t.Error("WorktreeAdd() accepted an option as its ref")
	}
	if err := client.Push(ctx, repo, "--mirror"); err == nil {
		t.Error("Push() accepted an option as its ref")
	}
	if err := client.PushSetUpstream(ctx, repo, "--mirror"); err == nil {
		t.Error("PushSetUpstream() accepted an option as its ref")
	}
	if err := client.WorktreeAddDetached(ctx, repo, filepath.Join(base, "detached"), "--lock"); err == nil {
		t.Error("WorktreeAddDetached() accepted an option as its ref")
	}
	if err := client.WorktreeAddBranch(ctx, repo, filepath.Join(base, "branch"), "other", "--lock", false); err == nil {
		t.Error("WorktreeAddBranch() accepted an option as its ref")
	}
	if _, err := client.CountNotOn(ctx, repo, "--output="+written); err == nil {
		t.Error("CountNotOn() accepted an option as a base")
	}
	if matches, _ := filepath.Glob(written + "*"); len(matches) != 0 {
		t.Errorf("CountNotOn() wrote %v", matches)
	}
	// checkout before git 2.44 does not honour --end-of-options, so the ref is
	// resolved to a commit first.
	if err := client.CheckoutDetached(ctx, repo, "--force"); err == nil {
		t.Error("CheckoutDetached() accepted an option as its ref")
	}
	if err := client.CheckoutDetached(ctx, repo, "origin/main"); err != nil {
		t.Errorf("CheckoutDetached(origin/main) error = %v", err)
	}
	if branch, onBranch, err := client.CurrentBranch(ctx, repo); err != nil || onBranch {
		t.Errorf("CurrentBranch() after CheckoutDetached = %q, %v, %v, want detached", branch, onBranch, err)
	}
	if held, err := client.CountNotOnAnyRef(ctx, repo); err != nil || held != 0 {
		t.Errorf("CountNotOnAnyRef() at origin/main = %d, %v, want 0", held, err)
	}
}
