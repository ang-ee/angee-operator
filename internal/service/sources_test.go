package service

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/internal/git"
	"github.com/ang-ee/angee-operator/internal/logctx"
	"github.com/ang-ee/angee-operator/internal/manifest"
)

// newUnreachableGitSource clones a one-commit remote into a cache and then
// deletes the remote, so the cache is present and holds `main` but any refresh
// fetch now fails — standing in for a private/SSH source the operator cannot
// authenticate.
func newUnreachableGitSource(t *testing.T) (*Platform, string, manifest.Source) {
	t.Helper()
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	cache := filepath.Join(base, "cache")
	root := filepath.Join(base, ".angee")

	runGit(t, "", "init", "--bare", remote)
	runGit(t, "", "clone", remote, cache)
	runGit(t, cache, "config", "user.email", "test@example.com")
	runGit(t, cache, "config", "user.name", "Test User")
	mustWriteFile(t, filepath.Join(cache, "README.md"), "hello\n")
	runGit(t, cache, "add", "README.md")
	runGit(t, cache, "commit", "-m", "initial")
	runGit(t, cache, "branch", "-M", "main")
	runGit(t, cache, "push", "-u", "origin", "main")

	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll(root) error = %v", err)
	}
	if err := os.RemoveAll(remote); err != nil {
		t.Fatalf("RemoveAll(remote) error = %v", err)
	}
	return &Platform{root: root}, cache, manifest.Source{Kind: "git", Repo: remote, DefaultRef: "main", CachePath: cache}
}

// With bestEffortRefresh, a git source whose cache is already cloned must
// materialize from that cache even when the refresh fetch can no longer reach
// the remote: bring-up and provisioning must not hard-fail on the refresh.
func TestMaterializeSourceBestEffortRefreshKeepsCache(t *testing.T) {
	var stderr bytes.Buffer
	ctx := logctx.With(t.Context(), slog.New(logctx.NewCLIHandler(&stderr, slog.LevelWarn)))
	p, cache, source := newUnreachableGitSource(t)

	if err := p.materializeSource(ctx, "app", source, true); err != nil {
		t.Fatalf("materializeSource(bestEffort) with an unreachable remote and an existing cache = %v, want nil", err)
	}
	wantWarning := "warning: could not refresh source \"app\"; using the existing cache (update it with `angee source pull app`)\n"
	if got := stderr.String(); got != wantWarning {
		t.Fatalf("warning output = %q, want %q", got, wantWarning)
	}
	if !git.New().RefExists(ctx, cache, "main") {
		t.Fatalf("cache lost its main ref after a best-effort refresh")
	}
}

// Without bestEffortRefresh, a failed refresh must surface to the caller: the
// explicit `angee source fetch` / `source pull` verbs route through
// materializeSource and must not silently report success.
func TestMaterializeSourceStrictRefreshSurfacesError(t *testing.T) {
	ctx := context.Background()
	p, _, source := newUnreachableGitSource(t)

	if err := p.materializeSource(ctx, "app", source, false); err == nil {
		t.Fatalf("materializeSource(strict) with an unreachable remote = nil, want the fetch error")
	}
}

// Best-effort covers only the refresh of an existing cache. With no cache yet,
// the clone is the only way to obtain the source, so an unreachable remote must
// still error regardless of the best-effort flag.
func TestMaterializeSourceStillFailsCloneWhenCacheMissing(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	root := filepath.Join(base, ".angee")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll(root) error = %v", err)
	}
	p := &Platform{root: root}
	source := manifest.Source{
		Kind:       "git",
		Repo:       filepath.Join(base, "does-not-exist.git"),
		DefaultRef: "main",
		CachePath:  filepath.Join(base, "cache"),
	}

	if err := p.materializeSource(ctx, "app", source, true); err == nil {
		t.Fatalf("materializeSource() with an unreachable remote and no cache = nil, want a clone error")
	}
}

// newMovedGitSource seeds two bare remotes, where the new one holds one more
// commit than the old one, and clones the cache from the old one. The stack's
// angee.yaml names the new one as the source's repo, as after the manifest's
// repo changed (or the cache was cloned by a template that used another URL).
func newMovedGitSource(t *testing.T) (p *Platform, cache, oldRemote, newRemote, newHead string) {
	t.Helper()
	base := t.TempDir()
	oldRemote = filepath.Join(base, "old.git")
	newRemote = filepath.Join(base, "new.git")
	work := filepath.Join(base, "work")
	cache = filepath.Join(base, "cache")
	root := filepath.Join(base, "stack")

	runGit(t, "", "init", "--bare", oldRemote)
	runGit(t, "", "init", "--bare", newRemote)
	runGit(t, "", "clone", oldRemote, work)
	runGit(t, work, "config", "user.email", "test@example.com")
	runGit(t, work, "config", "user.name", "Test User")
	mustWriteFile(t, filepath.Join(work, "README.md"), "hello\n")
	runGit(t, work, "add", "README.md")
	runGit(t, work, "commit", "-m", "initial")
	runGit(t, work, "branch", "-M", "main")
	runGit(t, work, "push", "origin", "main")
	runGit(t, "", "clone", "--branch", "main", oldRemote, cache)
	runGit(t, work, "commit", "--allow-empty", "-m", "only in the new remote")
	runGit(t, work, "push", newRemote, "main")
	newHead = strings.TrimSpace(runGitOutput(t, work, "rev-parse", "HEAD"))

	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll(root) error = %v", err)
	}
	mustWriteFile(t, filepath.Join(root, "angee.yaml"), "version: 1\nkind: stack\nname: moved-source\nsources:\n  app:\n    kind: git\n    repo: "+newRemote+"\n    cache_path: "+cache+"\n    default_ref: main\n")
	platform, err := New(root)
	if err != nil {
		t.Fatalf("New(root) error = %v", err)
	}
	return platform, cache, oldRemote, newRemote, newHead
}

func warnLogContext(t *testing.T, stderr *bytes.Buffer) context.Context {
	return logctx.With(t.Context(), slog.New(logctx.NewCLIHandler(stderr, slog.LevelWarn)))
}

// The repo in angee.yaml was used only for the first clone, so a cache cloned
// from another URL kept fetching from it. `source pull` must fetch from the
// manifest's repo and leave the cache's origin pointing at it.
func TestSourcePullFollowsManifestRepo(t *testing.T) {
	var stderr bytes.Buffer
	ctx := warnLogContext(t, &stderr)
	p, cache, oldRemote, newRemote, newHead := newMovedGitSource(t)

	if _, err := p.SourcePull(ctx, "app"); err != nil {
		t.Fatalf("SourcePull() error = %v", err)
	}
	if got := strings.TrimSpace(runGitOutput(t, cache, "config", "--get", "remote.origin.url")); got != newRemote {
		t.Fatalf("cache origin = %q, want the manifest repo %q", got, newRemote)
	}
	if got := strings.TrimSpace(runGitOutput(t, cache, "rev-parse", "HEAD")); got != newHead {
		t.Fatalf("cache HEAD = %s, want the new remote's main %s", got, newHead)
	}
	wantWarning := "warning: source \"app\" cache had origin " + oldRemote + "; set origin to " + newRemote + " from angee.yaml\n"
	if got := stderr.String(); got != wantWarning {
		t.Fatalf("warning output = %q, want %q", got, wantWarning)
	}

	// Once origin matches, a second pull changes nothing and says nothing.
	stderr.Reset()
	if _, err := p.SourcePull(ctx, "app"); err != nil {
		t.Fatalf("second SourcePull() error = %v", err)
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("second pull warning output = %q, want none", got)
	}
}

// `source fetch` follows the manifest's repo too and leaves every remote
// other than origin alone. A worktree slot shares the cache's config, so it
// follows without being touched.
func TestSourceFetchFollowsManifestRepoAndKeepsOtherRemotes(t *testing.T) {
	ctx := context.Background()
	p, cache, oldRemote, newRemote, newHead := newMovedGitSource(t)
	runGit(t, cache, "remote", "add", "upstream", oldRemote)
	slot := filepath.Join(t.TempDir(), "slot")
	runGit(t, cache, "worktree", "add", "--detach", slot, "main")

	if _, err := p.SourceFetch(ctx, "app"); err != nil {
		t.Fatalf("SourceFetch() error = %v", err)
	}
	if got := strings.TrimSpace(runGitOutput(t, cache, "config", "--get", "remote.origin.url")); got != newRemote {
		t.Fatalf("cache origin = %q, want the manifest repo %q", got, newRemote)
	}
	if got := strings.TrimSpace(runGitOutput(t, cache, "rev-parse", "refs/remotes/origin/main")); got != newHead {
		t.Fatalf("origin/main = %s, want the new remote's main %s", got, newHead)
	}
	if got := strings.TrimSpace(runGitOutput(t, cache, "config", "--get", "remote.upstream.url")); got != oldRemote {
		t.Fatalf("upstream remote = %q, want it left at %q", got, oldRemote)
	}
	if got := strings.TrimSpace(runGitOutput(t, slot, "remote", "get-url", "origin")); got != newRemote {
		t.Fatalf("worktree slot origin = %q, want the manifest repo %q", got, newRemote)
	}
}

// A cache with no origin remote gets one, so the refresh has something to
// fetch from.
func TestMaterializeSourceAddsMissingOrigin(t *testing.T) {
	var stderr bytes.Buffer
	ctx := warnLogContext(t, &stderr)
	p, cache, _, newRemote, newHead := newMovedGitSource(t)
	runGit(t, cache, "remote", "remove", "origin")
	stack, err := p.LoadStack()
	if err != nil {
		t.Fatalf("LoadStack() error = %v", err)
	}

	if err := p.materializeSource(ctx, "app", stack.Sources["app"], false); err != nil {
		t.Fatalf("materializeSource() error = %v", err)
	}
	if got := strings.TrimSpace(runGitOutput(t, cache, "rev-parse", "refs/remotes/origin/main")); got != newHead {
		t.Fatalf("origin/main = %s, want the new remote's main %s", got, newHead)
	}
	wantWarning := "warning: source \"app\" cache had no origin remote; set origin to " + newRemote + " from angee.yaml\n"
	if got := stderr.String(); got != wantWarning {
		t.Fatalf("warning output = %q, want %q", got, wantWarning)
	}
}

// Bring-up and workspace provisioning refresh best-effort. The cache may be
// shared with other stacks or be the user's own clone, so a mismatched origin
// is reported, not rewritten, and the refresh carries on against it.
func TestMaterializeSourceBestEffortWarnsInsteadOfRewritingOrigin(t *testing.T) {
	var stderr bytes.Buffer
	ctx := warnLogContext(t, &stderr)
	p, cache, oldRemote, newRemote, _ := newMovedGitSource(t)
	stack, err := p.LoadStack()
	if err != nil {
		t.Fatalf("LoadStack() error = %v", err)
	}

	configBefore, err := os.ReadFile(filepath.Join(cache, ".git", "config"))
	if err != nil {
		t.Fatalf("ReadFile(config) error = %v", err)
	}

	if err := p.materializeSource(ctx, "app", stack.Sources["app"], true); err != nil {
		t.Fatalf("materializeSource(bestEffort) error = %v", err)
	}
	configAfter, err := os.ReadFile(filepath.Join(cache, ".git", "config"))
	if err != nil {
		t.Fatalf("ReadFile(config) error = %v", err)
	}
	if !bytes.Equal(configBefore, configAfter) {
		t.Fatalf("best-effort refresh rewrote the cache config:\nbefore:\n%s\nafter:\n%s", configBefore, configAfter)
	}
	if got := strings.TrimSpace(runGitOutput(t, cache, "config", "--get", "remote.origin.url")); got != oldRemote {
		t.Fatalf("cache origin = %q, want it left at %q", got, oldRemote)
	}
	wantWarning := "warning: source \"app\" cache origin is " + oldRemote + ", but angee.yaml declares " + newRemote + "; run `angee source fetch app` to switch it\n"
	if got := stderr.String(); got != wantWarning {
		t.Fatalf("warning output = %q, want %q", got, wantWarning)
	}
}

// A cache whose config cannot be read is a failed best-effort refresh: warn
// and keep the cache, as for a failed fetch.
func TestMaterializeSourceBestEffortKeepsCacheWhenOriginUnreadable(t *testing.T) {
	var stderr bytes.Buffer
	ctx := warnLogContext(t, &stderr)
	p, cache, _, _, _ := newMovedGitSource(t)
	stack, err := p.LoadStack()
	if err != nil {
		t.Fatalf("LoadStack() error = %v", err)
	}
	mustWriteFile(t, filepath.Join(cache, ".git", "config"), "[core\nnot a config\n")

	if err := p.materializeSource(ctx, "app", stack.Sources["app"], true); err != nil {
		t.Fatalf("materializeSource(bestEffort) error = %v, want nil", err)
	}
	wantWarning := "warning: could not refresh source \"app\"; using the existing cache (update it with `angee source pull app`)\n"
	if got := stderr.String(); got != wantWarning {
		t.Fatalf("warning output = %q, want %q", got, wantWarning)
	}
}

// When origin cannot be updated, the explicit verbs fail and say so instead of
// fetching from the old remote.
func TestSourceFetchReportsFailedOriginUpdate(t *testing.T) {
	p, cache, oldRemote, _, _ := newMovedGitSource(t)
	// A held config lock makes `git remote set-url` fail while reads succeed.
	mustWriteFile(t, filepath.Join(cache, ".git", "config.lock"), "")

	_, err := p.SourceFetch(t.Context(), "app")
	if err == nil || !strings.Contains(err.Error(), `source "app": set remote.origin.url`) {
		t.Fatalf("SourceFetch() error = %v, want a set-origin failure", err)
	}
	if got := strings.TrimSpace(runGitOutput(t, cache, "config", "--get", "remote.origin.url")); got != oldRemote {
		t.Fatalf("cache origin = %q, want it left at %q", got, oldRemote)
	}
	// A fresh clone has no FETCH_HEAD; one now would mean the old remote was
	// fetched after all.
	if _, err := os.Stat(filepath.Join(cache, ".git", "FETCH_HEAD")); !os.IsNotExist(err) {
		t.Fatalf("Stat(FETCH_HEAD) error = %v, want no fetch after the failed origin update", err)
	}
}

// When the two URLs differ only in credentials, the redacted warning says so
// instead of printing the same URL twice.
func TestRedactedURLAgainstNotesCredentialOnlyDifference(t *testing.T) {
	if got, want := redactedURLAgainst("https://a:one@example.com/app.git", "https://a:two@example.com/app.git"), "https://***@example.com/app.git (different credentials)"; got != want {
		t.Fatalf("redactedURLAgainst(credentials differ) = %q, want %q", got, want)
	}
	if got, want := redactedURLAgainst("https://example.com/old.git", "https://example.com/new.git"), "https://example.com/old.git"; got != want {
		t.Fatalf("redactedURLAgainst(different URL) = %q, want %q", got, want)
	}
}

func TestRelativeRepoPath(t *testing.T) {
	for repo, want := range map[string]bool{
		"../app":                                 true,
		"sources/app":                            true,
		"app":                                    true,
		"./dir:name":                             true,
		"host:repo.git":                          false,
		"/srv/git/app.git":                       false,
		"https://github.com/acme/app.git":        false,
		"file:///srv/git/app.git":                false,
		"git@github.com:acme/app.git":            false,
		"git@github-arpee:/ang-ee/angee-arp.git": false,
	} {
		if got := relativeRepoPath(repo); got != want {
			t.Errorf("relativeRepoPath(%q) = %v, want %v", repo, got, want)
		}
	}

	// A relative repo is left out of origin reconciliation entirely.
	p, cache, oldRemote, _, _ := newMovedGitSource(t)
	if err := syncSourceOrigin(context.Background(), p.gitClient(), "app", cache, "../new.git", true); err != nil {
		t.Fatalf("syncSourceOrigin(relative) error = %v", err)
	}
	if got := strings.TrimSpace(runGitOutput(t, cache, "config", "--get", "remote.origin.url")); got != oldRemote {
		t.Fatalf("cache origin = %q, want it left at %q", got, oldRemote)
	}
}

// A cancelled or timed-out context must abort even the best-effort path rather
// than masking the cancellation and proceeding against the stale cache.
func TestMaterializeSourceBestEffortDoesNotMaskContextCancellation(t *testing.T) {
	p, _, source := newUnreachableGitSource(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := p.materializeSource(ctx, "app", source, true); err == nil {
		t.Fatalf("materializeSource(bestEffort) with a cancelled context = nil, want the context error")
	}
}
