package git

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
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

// PushRemoteFor resolves in git's order for the branch it is given, on a
// checkout that is not on it: a jj slot's detached HEAD.
func TestPushRemoteForANamedBranch(t *testing.T) {
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
	runGit(t, repo, "switch", "--detach")
	runGit(t, repo, "remote", "add", "origin", filepath.Join(base, "origin.git"))
	runGit(t, repo, "remote", "add", "fork", filepath.Join(base, "fork.git"))
	runGit(t, repo, "remote", "add", "mirror", filepath.Join(base, "mirror.git"))
	runGit(t, repo, "config", "remote.pushDefault", "mirror")
	runGit(t, repo, "config", "branch.feature.pushRemote", "fork")
	client := New()

	for branch, want := range map[string]string{"feature": "fork", "other": "mirror", "": "mirror"} {
		got, err := client.PushRemoteFor(ctx, repo, branch)
		if err != nil || got != want {
			t.Fatalf("PushRemoteFor(%q) = %q, %v; want %q", branch, got, err, want)
		}
	}
	if got := pushRemote(t, client, ctx, repo); got != "mirror" {
		t.Fatalf("PushRemote() on a detached HEAD = %q, want mirror", got)
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

// SyncBaseRef's order: git's own tracking of the local branch, then the base
// remote (origin when none is given), then the other remotes by name, never
// by the order git happens to list them in.
func TestSyncBaseRefOrder(t *testing.T) {
	isolateGitConfig(t)
	ctx := context.Background()
	base := t.TempDir()
	seed := filepath.Join(base, "seed")
	runGit(t, "", "init", seed)
	runGit(t, seed, "config", "user.email", "test@example.com")
	runGit(t, seed, "config", "user.name", "Test User")
	runGit(t, seed, "commit", "--allow-empty", "-m", "initial")
	runGit(t, seed, "branch", "-M", "main")
	remotes := map[string]string{}
	for _, name := range []string{"upstream", "origin", "afork", "zfork"} {
		remotes[name] = filepath.Join(base, name+".git")
		runGit(t, "", "init", "--bare", remotes[name])
		runGit(t, seed, "commit", "--allow-empty", "-m", "release on "+name)
		runGit(t, seed, "push", remotes[name], "main", "HEAD:refs/heads/release")
	}
	repo := filepath.Join(base, "repo")
	runGit(t, "", "clone", "--origin", "upstream", remotes["upstream"], repo)
	for _, name := range []string{"origin", "zfork", "afork"} {
		runGit(t, repo, "remote", "add", name, remotes[name])
	}
	runGit(t, repo, "fetch", "--all")
	client := New()
	resolve := func(ref, baseRemote string) string {
		t.Helper()
		got, err := client.SyncBaseRef(ctx, repo, ref, baseRemote)
		if err != nil {
			t.Fatalf("SyncBaseRef(%q, %q) error = %v", ref, baseRemote, err)
		}
		return got
	}

	if got := resolve("release", "upstream"); got != "upstream/release" {
		t.Fatalf("SyncBaseRef(release, upstream) = %q, want the base remote's", got)
	}
	if got := resolve("release", ""); got != "origin/release" {
		t.Fatalf("SyncBaseRef(release, none) = %q, want origin's", got)
	}
	if got := resolve("upstream/release", "origin"); got != "upstream/release" {
		t.Fatalf("SyncBaseRef(upstream/release) = %q, want it as given", got)
	}
	runGit(t, repo, "branch", "--track", "release", "zfork/release")
	if got := resolve("release", "upstream"); got != "zfork/release" {
		t.Fatalf("SyncBaseRef(release) with release tracking zfork = %q, want git's own tracking", got)
	}
	runGit(t, repo, "branch", "-D", "release")

	runGit(t, repo, "remote", "remove", "origin")
	runGit(t, repo, "remote", "remove", "upstream")
	for range 20 {
		if got := resolve("release", "upstream"); got != "afork/release" {
			t.Fatalf("SyncBaseRef(release) without the base remote = %q, want the first remote by name", got)
		}
	}
}

// CloneRef names the remote it clones from when asked, whatever the user's
// clone.defaultRemoteName says, and leaves git's default otherwise.
func TestCloneRefNamesTheRemote(t *testing.T) {
	isolateGitConfig(t)
	ctx := context.Background()
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	runGit(t, "", "init", "--bare", remote)
	client := New()

	named := filepath.Join(base, "named")
	if err := client.CloneRef(ctx, remote, named, "", "upstream"); err != nil {
		t.Fatalf("CloneRef(upstream) error = %v", err)
	}
	if got := configValue(t, named, "remote.upstream.url"); got != remote {
		t.Fatalf("remote.upstream.url = %q, want %q", got, remote)
	}
	if urls, err := client.remoteURLs(ctx, named, "origin"); err != nil || len(urls) != 0 {
		t.Fatalf("origin URLs = (%q, %v), want none", urls, err)
	}
	plain := filepath.Join(base, "plain")
	if err := client.CloneRef(ctx, remote, plain, "", ""); err != nil {
		t.Fatalf("CloneRef(default) error = %v", err)
	}
	if got := configValue(t, plain, "remote.origin.url"); got != remote {
		t.Fatalf("remote.origin.url = %q, want %q", got, remote)
	}

	runGit(t, "", "config", "--global", "clone.defaultRemoteName", "upstream")
	asOrigin := filepath.Join(base, "as-origin")
	if err := client.CloneRef(ctx, remote, asOrigin, "", "origin"); err != nil {
		t.Fatalf("CloneRef(origin) error = %v", err)
	}
	if got := configValue(t, asOrigin, "remote.origin.url"); got != remote {
		t.Fatalf("remote.origin.url under clone.defaultRemoteName = %q, want %q", got, remote)
	}
}

// The push URL and push default setters: SetPushURL leaves one push URL however
// many there were, UnsetPushURL removes them all, and neither touches the fetch
// URL.
func TestPushURLAndPushDefault(t *testing.T) {
	isolateGitConfig(t)
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "repo")
	runGit(t, "", "init", repo)
	runGit(t, repo, "remote", "add", "upstream", "https://example.com/up.git")
	client := New()

	if urls, err := client.PushURLs(ctx, repo, "upstream"); err != nil || len(urls) != 0 {
		t.Fatalf("PushURLs(none) = (%q, %v), want none", urls, err)
	}
	runGit(t, repo, "config", "--add", "remote.upstream.pushurl", "https://example.com/a.git")
	runGit(t, repo, "config", "--add", "remote.upstream.pushurl", "https://example.com/b.git")
	if err := client.SetPushURL(ctx, repo, "upstream", "angee-fetch-only://upstream"); err != nil {
		t.Fatalf("SetPushURL() error = %v", err)
	}
	if urls, err := client.PushURLs(ctx, repo, "upstream"); err != nil || len(urls) != 1 || urls[0] != "angee-fetch-only://upstream" {
		t.Fatalf("PushURLs(set) = (%q, %v), want the one guard URL", urls, err)
	}
	if got := configValue(t, repo, "remote.upstream.url"); got != "https://example.com/up.git" {
		t.Fatalf("remote.upstream.url = %q, want it unchanged", got)
	}
	if err := client.UnsetPushURL(ctx, repo, "upstream"); err != nil {
		t.Fatalf("UnsetPushURL() error = %v", err)
	}
	if urls, err := client.PushURLs(ctx, repo, "upstream"); err != nil || len(urls) != 0 {
		t.Fatalf("PushURLs(unset) = (%q, %v), want none", urls, err)
	}

	if err := client.SetPushDefault(ctx, repo, "upstream"); err != nil {
		t.Fatalf("SetPushDefault() error = %v", err)
	}
	if got := configValue(t, repo, "remote.pushDefault"); got != "upstream" {
		t.Fatalf("remote.pushDefault = %q, want upstream", got)
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
	got, err := client.SyncBaseRef(ctx, repo, "release/2026-05", "")
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

// A ref is read as a revision only: one that looks like an option is refused
// by git rather than run as one (--output would write a file on the host).
func TestDiffReadsRefAsRevisionOnly(t *testing.T) {
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "repo")
	runGit(t, "", "init", "--initial-branch=main", repo)
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test User")
	mustWriteFile(t, filepath.Join(repo, "README.md"), "hello\n")
	runGit(t, repo, "add", "README.md")
	runGit(t, repo, "commit", "-m", "initial")
	mustWriteFile(t, filepath.Join(repo, "README.md"), "hello again\n")
	runGit(t, repo, "commit", "-am", "second")

	client := New()
	files, err := client.Diff(ctx, repo, "HEAD~1")
	if err != nil || len(files) != 1 || files[0].NewName != "README.md" {
		t.Fatalf("Diff(HEAD~1) = %v, %v, want README.md", files, err)
	}
	written := filepath.Join(t.TempDir(), "written.diff")
	if _, err := client.Diff(ctx, repo, "--output="+written); err == nil {
		t.Fatal("Diff() accepted an option as its ref")
	}
	if _, err := os.Stat(written); !os.IsNotExist(err) {
		t.Fatalf("Diff() with an option as ref wrote %s (stat error = %v)", written, err)
	}
}

// LinkedWorktree tells a worktree added from a repository from a repository of
// its own, also when the checkout is reached from a subdirectory through a
// symlink, where git prints the two directories in different forms.
func TestLinkedWorktree(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	runGit(t, "", "init", "--initial-branch=main", repo)
	mustWriteFile(t, filepath.Join(repo, "README.md"), "readme\n")
	if err := os.Mkdir(filepath.Join(repo, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(repo, "sub", "file.txt"), "file\n")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "-c", "user.email=test@example.com", "-c", "user.name=Test User", "commit", "-m", "init")
	worktree := filepath.Join(base, "worktree")
	runGit(t, repo, "worktree", "add", "--detach", worktree)
	link := filepath.Join(base, "link")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}

	client := New()
	for _, tc := range []struct {
		dir  string
		want bool
	}{
		{dir: repo, want: false},
		{dir: filepath.Join(link, "sub"), want: false},
		{dir: worktree, want: true},
		{dir: filepath.Join(worktree, "sub"), want: true},
	} {
		got, err := client.LinkedWorktree(ctx, tc.dir)
		if err != nil || got != tc.want {
			t.Fatalf("LinkedWorktree(%s) = %v, %v, want %v", tc.dir, got, err, tc.want)
		}
	}
}

func TestVersionAtLeast(t *testing.T) {
	for _, tc := range []struct {
		line, floor string
		want        bool
	}{
		{"git version 2.48.0", RelativeWorktreesVersion, true},
		{"git version 2.50.1 (Apple Git-155)", RelativeWorktreesVersion, true},
		{"git version 2.48.0.windows.1", RelativeWorktreesVersion, true},
		{"git version 3.0.0", RelativeWorktreesVersion, true},
		{"git version 2.47.2", RelativeWorktreesVersion, false},
		{"git version 2.100.0", RelativeWorktreesVersion, true},
		{"git version 1.99.0", RelativeWorktreesVersion, false},
		{"git version 2.42.0", "2.42", true},
		{"git version 2.41.9", "2.42", false},
		{"", RelativeWorktreesVersion, false},
		{"git version unknown", RelativeWorktreesVersion, false},
		{"git version 2.50.1", "", false},
	} {
		if got := VersionAtLeast(tc.line, tc.floor); got != tc.want {
			t.Errorf("VersionAtLeast(%q, %q) = %v, want %v", tc.line, tc.floor, got, tc.want)
		}
	}
}

// commitAt commits what is staged in dir with message, as committed at when.
func commitAt(t *testing.T, dir, when, message string) {
	t.Helper()
	t.Setenv("GIT_AUTHOR_DATE", when)
	t.Setenv("GIT_COMMITTER_DATE", when)
	runGit(t, dir, "-c", "user.email=test@example.com", "-c", "user.name=Test User", "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", message)
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		t.Fatalf("git %v in %s error = %v", args, dir, err)
	}
	return strings.TrimSpace(string(out))
}

// Log lists HEAD's history newest committer time first, merges with both
// parents and messages whole, up to its limit; a file named HEAD does not
// make HEAD ambiguous, and a repository with no commit yet has none, while
// a HEAD naming a missing commit is an error. The repository's
// i18n.logOutputEncoding and its replace refs change nothing it reads.
func TestLog(t *testing.T) {
	isolateGitConfig(t)
	ctx := context.Background()
	base := t.TempDir()
	// git does not look for a repository in the ceiling, only below it.
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(base))
	repo := filepath.Join(base, "repo")
	runGit(t, "", "init", "-q", "--initial-branch=main", repo)
	mustWriteFile(t, filepath.Join(repo, "HEAD"), "a file named HEAD\n")
	runGit(t, repo, "add", "HEAD")
	commitAt(t, repo, "2026-01-01T00:00:00Z", "first\n\nwith a body")
	runGit(t, repo, "branch", "topic")
	commitAt(t, repo, "2026-01-03T00:00:00Z", "second, café")
	runGit(t, repo, "checkout", "-q", "topic")
	commitAt(t, repo, "2026-01-02T00:00:00Z", "topic")
	runGit(t, repo, "checkout", "-q", "main")
	t.Setenv("GIT_COMMITTER_DATE", "2026-01-04T00:00:00Z")
	runGit(t, repo, "-c", "user.email=test@example.com", "-c", "user.name=Test User", "merge", "-q", "--no-ff", "-m", "merge topic", "topic")
	id := func(rev string) string { return gitOutput(t, repo, "rev-parse", rev) }

	client := New()
	commits, err := client.Log(ctx, repo, 10)
	if err != nil {
		t.Fatalf("Log() error = %v", err)
	}
	var hashes, messages []string
	for _, c := range commits {
		hashes = append(hashes, c.Hash)
		messages = append(messages, c.Message)
	}
	if want := []string{id("main"), id("main^1"), id("topic"), id("topic^")}; !slices.Equal(hashes, want) {
		t.Fatalf("Log() hashes = %q, want merge, second, topic, first: %q", hashes, want)
	}
	if want := []string{"merge topic\n", "second, café\n", "topic\n", "first\n\nwith a body\n"}; !slices.Equal(messages, want) {
		t.Fatalf("Log() messages = %q, want %q", messages, want)
	}
	merge, first := commits[0], commits[3]
	if want := []string{id("main^1"), id("main^2")}; !slices.Equal(merge.Parents, want) {
		t.Fatalf("merge parents = %q, want %q", merge.Parents, want)
	}
	if first.Parents == nil || len(first.Parents) != 0 {
		t.Fatalf("root commit parents = %#v, want empty", first.Parents)
	}
	if want := time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC); !merge.Committed.Equal(want) {
		t.Fatalf("merge committed at %s, want %s", merge.Committed, want)
	}
	if merge.AuthorName != "Test User" || merge.AuthorEmail != "test@example.com" {
		t.Fatalf("merge author = %q <%q>, want Test User <test@example.com>", merge.AuthorName, merge.AuthorEmail)
	}

	if limited, err := client.Log(ctx, repo, 2); err != nil || len(limited) != 2 || limited[1].Hash != id("main^1") {
		t.Fatalf("Log(2) = %+v, %v; want merge and second", limited, err)
	}
	if none, err := client.Log(ctx, repo, 0); err != nil || none != nil {
		t.Fatalf("Log(0) = %+v, %v; want nothing run", none, err)
	}
	empty := filepath.Join(base, "empty")
	runGit(t, "", "init", "-q", empty)
	if none, err := client.Log(ctx, empty, 10); err != nil || len(none) != 0 {
		t.Fatalf("Log(empty repository) = %+v, %v; want no commits", none, err)
	}
	if _, err := client.Log(ctx, base, 10); err == nil {
		t.Fatal("Log(not a repository) error = nil")
	}
	broken := filepath.Join(base, "broken")
	runGit(t, "", "init", "-q", "--initial-branch=main", broken)
	mustWriteFile(t, filepath.Join(broken, ".git", "refs", "heads", "main"), strings.Repeat("0123456789", 4)+"\n")
	if got, err := client.Log(ctx, broken, 10); err == nil {
		t.Fatalf("Log(HEAD names a missing commit) = %+v, want an error", got)
	}

	runGit(t, repo, "config", "i18n.logOutputEncoding", "ISO-8859-1")
	runGit(t, repo, "replace", id("main^1"), id("topic"))
	if again, err := client.Log(ctx, repo, 10); err != nil || !reflect.DeepEqual(again, commits) {
		t.Fatalf("Log() under i18n.logOutputEncoding and a replace ref =\n%+v, %v\nwant\n%+v", again, err, commits)
	}
}

// Log and RefTargets read the repository in their directory even when the
// process environment points git at another one.
func TestCLIReadsIgnoreRepositoryOverrides(t *testing.T) {
	isolateGitConfig(t)
	ctx := context.Background()
	base := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(base))
	repo, other := filepath.Join(base, "repo"), filepath.Join(base, "other")
	for _, dir := range []string{repo, other} {
		runGit(t, "", "init", "-q", "--initial-branch=main", dir)
		commitAt(t, dir, "2026-01-01T00:00:00Z", filepath.Base(dir))
	}
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	commits, err := New().Log(ctx, repo, 10)
	if err != nil || len(commits) != 1 || commits[0].Message != "repo\n" {
		t.Fatalf("Log(repo) under GIT_DIR=other = %+v, %v; want repo's commit", commits, err)
	}
	targets, err := New().RefTargets(ctx, repo)
	if err != nil || !slices.Contains(targets[commits[0].Hash], "refs/heads/main") {
		t.Fatalf("RefTargets(repo) under GIT_DIR=other = %v, %v; want repo's main", targets, err)
	}
}

// A command whose output outgrows runStdout's limit is stopped and fails
// rather than being read whole.
func TestRunStdoutStopsAtItsLimit(t *testing.T) {
	isolateGitConfig(t)
	base := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(base))
	repo := filepath.Join(base, "repo")
	runGit(t, "", "init", "-q", "--initial-branch=main", repo)
	commitAt(t, repo, "2026-01-01T00:00:00Z", strings.Repeat("a long message ", 1000))
	_, err := New().runStdout(context.Background(), repo, 1024, "log", "--format=%B")
	if err == nil || !strings.Contains(err.Error(), "output exceeds 1024 bytes") {
		t.Fatalf("runStdout() past its limit error = %v, want the limit named", err)
	}
	if out, err := New().runStdout(context.Background(), repo, stdoutLimit, "log", "--format=%B"); err != nil || len(out) < 15000 {
		t.Fatalf("runStdout() within its limit = %d bytes, %v", len(out), err)
	}
}

// A commit object whose message holds a NUL, which git commit refuses but
// other tools can write, still parses as one commit: git prints a message only
// up to its NUL, so the NUL ending each record stays unambiguous.
func TestLogReadsAMessageHoldingANUL(t *testing.T) {
	isolateGitConfig(t)
	base := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(base))
	repo := filepath.Join(base, "repo")
	runGit(t, "", "init", "-q", "--initial-branch=main", repo)
	commitAt(t, repo, "2026-01-01T00:00:00Z", "first")
	object := fmt.Sprintf("tree %s\nparent %s\nauthor A <a@example.com> 1767225600 +0000\ncommitter A <a@example.com> 1767225600 +0000\n\nbefore\x00after\nmore\n",
		gitOutput(t, repo, "rev-parse", "HEAD^{tree}"), gitOutput(t, repo, "rev-parse", "HEAD"))
	cmd := exec.Command("git", "hash-object", "-t", "commit", "-w", "--literally", "--stdin")
	cmd.Dir, cmd.Stdin = repo, strings.NewReader(object)
	id, err := cmd.Output()
	if err != nil {
		t.Fatalf("hash-object error = %v", err)
	}
	runGit(t, repo, "update-ref", "HEAD", strings.TrimSpace(string(id)))
	commits, err := New().Log(context.Background(), repo, 10)
	if err != nil || len(commits) != 2 || commits[0].Message != "before" || commits[1].Message != "first\n" {
		t.Fatalf("Log() = %+v, %v; want the NUL commit's message cut at its NUL, then first", commits, err)
	}
}

// RefTargets keys each ref by what it points at: a commit for branches,
// remote-tracking branches and lightweight tags, the tag object for an
// annotated tag. A symbolic ref (origin/HEAD) is left out.
func TestRefTargets(t *testing.T) {
	isolateGitConfig(t)
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	repo := filepath.Join(base, "repo")
	runGit(t, "", "init", "-q", "--bare", "--initial-branch=main", remote)
	runGit(t, "", "clone", "-q", remote, repo)
	commitAt(t, repo, "2026-01-01T00:00:00Z", "first")
	runGit(t, repo, "push", "-q", "origin", "main")
	runGit(t, repo, "remote", "set-head", "origin", "main")
	runGit(t, repo, "branch", "other")
	runGit(t, repo, "tag", "light")
	runGit(t, repo, "-c", "user.email=test@example.com", "-c", "user.name=Test User", "tag", "-a", "-m", "annotated", "annotated")
	if got := gitOutput(t, repo, "symbolic-ref", "refs/remotes/origin/HEAD"); got != "refs/remotes/origin/main" {
		t.Fatalf("origin/HEAD = %q, want a symbolic ref to origin/main", got)
	}

	targets, err := New().RefTargets(context.Background(), repo)
	if err != nil {
		t.Fatalf("RefTargets() error = %v", err)
	}
	want := map[string][]string{
		gitOutput(t, repo, "rev-parse", "main"):      {"refs/heads/main", "refs/heads/other", "refs/remotes/origin/main", "refs/tags/light"},
		gitOutput(t, repo, "rev-parse", "annotated"): {"refs/tags/annotated"},
	}
	if !reflect.DeepEqual(targets, want) {
		t.Fatalf("RefTargets() = %q, want %q", targets, want)
	}
}

// A repository with extensions.relativeWorktrees, which `jj workspace add
// --colocate` sets on a source's cache, is one go-git refuses to open. The
// readers that try go-git first answer for the cache and for its worktrees
// what they answered before the extension was set, through the git CLI, and
// the CLI-only readers read it too.
func TestReadOnlyQueriesFallbackForRelativeWorktreesExtension(t *testing.T) {
	isolateGitConfig(t)
	ctx := context.Background()
	client := New()
	if version, err := client.Version(ctx); err != nil || !VersionAtLeast(version, RelativeWorktreesVersion) {
		t.Skipf("git %q (%v) has no relative worktrees", version, err)
	}
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	cache := filepath.Join(base, "cache")
	slot := filepath.Join(base, "slot")
	runGit(t, "", "init", "-q", "--bare", "--initial-branch=main", remote)
	runGit(t, "", "clone", "-q", remote, cache)
	commitAt(t, cache, "2026-01-01T00:00:00Z", "first")
	runGit(t, cache, "push", "-q", "-u", "origin", "main")
	if err := client.WorktreeAddBranch(ctx, cache, slot, "workspace/slot", "main", false); err != nil {
		t.Fatalf("WorktreeAddBranch() error = %v", err)
	}

	read := func(dir string) string {
		t.Helper()
		ref, refErr := client.CurrentRef(ctx, dir)
		branch, onBranch, branchErr := client.CurrentBranch(ctx, dir)
		upstream, hasUpstream, upstreamErr := client.Upstream(ctx, dir)
		remotes, remotesErr := client.Remotes(ctx, dir)
		return fmt.Sprintf("ref=%s,%v branch=%s,%v,%v upstream=%s,%v,%v remotes=%q,%v main=%v origin/main=%v missing=%v",
			ref, refErr, branch, onBranch, branchErr, upstream, hasUpstream, upstreamErr, remotes, remotesErr,
			client.RefExists(ctx, dir, "main"), client.RefExists(ctx, dir, "origin/main"), client.RefExists(ctx, dir, "missing"))
	}
	before := map[string]string{cache: read(cache), slot: read(slot)}

	runGit(t, cache, "-c", "worktree.useRelativePaths=true", "worktree", "add", "-q", "--detach", filepath.Join(base, "relative"))
	if got := configValue(t, cache, "extensions.relativeWorktrees"); got != "true" {
		t.Fatalf("extensions.relativeWorktrees = %q, want true", got)
	}
	if got := configValue(t, cache, "core.repositoryformatversion"); got != "1" {
		t.Fatalf("core.repositoryformatversion = %q, want 1", got)
	}
	for dir, want := range before {
		if got := read(dir); got != want {
			t.Errorf("after the extension, %s reads\n%s\nwant\n%s", filepath.Base(dir), got, want)
		}
	}
	if commits, err := client.Log(ctx, cache, 10); err != nil || len(commits) != 1 || commits[0].Message != "first\n" {
		t.Fatalf("Log(cache) = %+v, %v; want the one commit", commits, err)
	}
	if targets, err := client.RefTargets(ctx, cache); err != nil || len(targets) != 1 {
		t.Fatalf("RefTargets(cache) = %q, %v; want main's commit", targets, err)
	}
}

// Worktrees lists every worktree, the main one first, with its HEAD and the
// branch it has checked out, none when detached, from any of them; a path
// holding a line break is read whole.
func TestWorktrees(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	runGit(t, "", "init", "--initial-branch=main", repo)
	mustWriteFile(t, filepath.Join(repo, "README.md"), "readme\n")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "-c", "user.email=test@example.com", "-c", "user.name=Test User", "commit", "-m", "init")
	head := gitOutput(t, repo, "rev-parse", "HEAD")
	topic, detached := filepath.Join(base, "topic\nline"), filepath.Join(base, "detached")
	runGit(t, repo, "worktree", "add", "-b", "feature/x", topic)
	runGit(t, repo, "worktree", "add", "--detach", detached)

	got, err := New().Worktrees(ctx, detached)
	if err != nil {
		t.Fatalf("Worktrees() error = %v", err)
	}
	resolve := func(path string) string {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			t.Fatalf("EvalSymlinks(%q) error = %v", path, err)
		}
		return resolved
	}
	// git lists the linked worktrees by the names it keeps them under.
	want := []Worktree{
		{Path: resolve(repo), Head: head, Branch: "main"},
		{Path: resolve(detached), Head: head},
		{Path: resolve(topic), Head: head, Branch: "feature/x"},
	}
	for i := range got {
		got[i].Path = resolve(got[i].Path)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Worktrees() = %#v, want %#v", got, want)
	}
}

// ChangedPaths lists changes to tracked files alone, and OtherPaths what git
// does not track, the tops of untracked directories as one entry each, with
// or without the ignored ones.
func TestChangedAndOtherPaths(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	runGit(t, repo, "init", "-q", "--initial-branch=main")
	runGit(t, repo, "config", "user.email", "test@example.invalid")
	runGit(t, repo, "config", "user.name", "test")
	if err := os.MkdirAll(filepath.Join(repo, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(repo, ".gitignore"), "*.log\nnode_modules/\n")
	mustWriteFile(t, filepath.Join(repo, "src", "a.txt"), "a\n")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-q", "-m", "base")
	for name, content := range map[string]string{"src/a.txt": "changed\n", "src/debug.log": "log\n", "notes.txt": "n\n"} {
		mustWriteFile(t, filepath.Join(repo, name), content)
	}
	if err := os.MkdirAll(filepath.Join(repo, "node_modules", "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(repo, "node_modules", "pkg", "i.js"), "x\n")
	client := New()
	if got, err := client.ChangedPaths(ctx, repo); err != nil || !slices.Equal(got, []string{"src/a.txt"}) {
		t.Errorf("ChangedPaths() = %q, %v, want src/a.txt alone", got, err)
	}
	if got, err := client.OtherPaths(ctx, repo, true); err != nil || !slices.Equal(got, []string{"node_modules/", "notes.txt", "src/debug.log"}) {
		t.Errorf("OtherPaths(ignored) = %q, %v", got, err)
	}
	if got, err := client.OtherPaths(ctx, repo, false); err != nil || !slices.Equal(got, []string{"notes.txt"}) {
		t.Errorf("OtherPaths() = %q, %v", got, err)
	}
	head, err := client.Commit(ctx, repo, "HEAD")
	if err != nil || len(head) != 40 {
		t.Errorf("Commit(HEAD) = %q, %v", head, err)
	}
}
