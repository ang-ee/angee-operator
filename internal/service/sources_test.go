package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ang-ee/angee-operator/internal/git"
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
	ctx := context.Background()
	p, cache, source := newUnreachableGitSource(t)

	if err := p.materializeSource(ctx, "app", source, true); err != nil {
		t.Fatalf("materializeSource(bestEffort) with an unreachable remote and an existing cache = %v, want nil", err)
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
