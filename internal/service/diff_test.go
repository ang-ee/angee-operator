package service

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/git"
	"github.com/ang-ee/angee-operator/internal/manifest"
	gogit "github.com/go-git/go-git/v5"
)

func TestSourceDiffReturnsUncommittedChanges(t *testing.T) {
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	cache := filepath.Join(base, "cache")
	root := filepath.Join(base, ".angee")

	mustGit(t, "", "init", "--bare", remote)
	mustGit(t, "", "clone", remote, cache)
	mustGit(t, cache, "config", "user.email", "test@example.com")
	mustGit(t, cache, "config", "user.name", "Test User")
	must(t, os.WriteFile(filepath.Join(cache, "README.md"), []byte("hello\n"), 0o644))
	mustGit(t, cache, "add", "README.md")
	mustGit(t, cache, "commit", "-m", "initial")
	mustGit(t, cache, "branch", "-M", "main")
	mustGit(t, cache, "push", "-u", "origin", "main")
	// Introduce an uncommitted modification.
	must(t, os.WriteFile(filepath.Join(cache, "README.md"), []byte("hello\nworld\n"), 0o644))

	must(t, os.MkdirAll(root, 0o755))
	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "test",
		Sources: map[string]manifest.Source{
			"app": {Kind: "git", Repo: remote, DefaultRef: "main", CachePath: cache},
		},
	}
	must(t, manifest.SaveFile(manifest.Path(root), stack))

	p, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	files, err := p.SourceDiff(context.Background(), "app", "")
	if err != nil {
		t.Fatalf("SourceDiff() error = %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("files = %d, want 1: %+v", len(files), files)
	}
	if files[0].NewPath != "README.md" {
		t.Fatalf("NewPath = %q, want README.md", files[0].NewPath)
	}
	if len(files[0].Hunks) == 0 {
		t.Fatalf("expected at least one hunk")
	}
	if !strings.Contains(files[0].Hunks[0].Body, "world") {
		t.Fatalf("hunk body missing change marker: %q", files[0].Hunks[0].Body)
	}
}

func TestSourceDiffRejectsNonGitSource(t *testing.T) {
	root := t.TempDir()
	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "test",
		Sources: map[string]manifest.Source{
			"data": {Kind: "local", Path: "data"},
		},
	}
	must(t, manifest.SaveFile(manifest.Path(root), stack))
	p, _ := New(root)
	_, err := p.SourceDiff(context.Background(), "data", "")
	if err == nil {
		t.Fatal("SourceDiff(local) error = nil, want type error")
	}
}

// isolateGitConfig gives git an empty global config and no system config, so
// none of the developer's (signing, encodings) reaches the test.
func isolateGitConfig(t *testing.T) {
	t.Helper()
	useGlobalGitConfig(t, t.TempDir(), "")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

// newCommitsStack is a stack whose one git source, app, has a cache with two
// commits, first and second, on main, pushed to its remote.
func newCommitsStack(t *testing.T) (root, cache string) {
	t.Helper()
	isolateGitConfig(t)
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	cache = filepath.Join(base, "cache")
	root = filepath.Join(base, ".angee")

	mustGit(t, "", "init", "--bare", remote)
	mustGit(t, "", "clone", remote, cache)
	mustGit(t, cache, "config", "user.email", "test@example.com")
	mustGit(t, cache, "config", "user.name", "Test User")
	must(t, os.WriteFile(filepath.Join(cache, "a.txt"), []byte("a\n"), 0o644))
	mustGit(t, cache, "add", "a.txt")
	mustGit(t, cache, "commit", "-m", "first")
	must(t, os.WriteFile(filepath.Join(cache, "b.txt"), []byte("b\n"), 0o644))
	mustGit(t, cache, "add", "b.txt")
	mustGit(t, cache, "commit", "-m", "second")
	mustGit(t, cache, "branch", "-M", "main")
	mustGit(t, cache, "push", "-u", "origin", "main")

	must(t, os.MkdirAll(root, 0o755))
	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "test",
		Sources: map[string]manifest.Source{
			"app": {Kind: "git", Repo: remote, DefaultRef: "main", CachePath: cache},
		},
	}
	must(t, manifest.SaveFile(manifest.Path(root), stack))
	return root, cache
}

// topologyCommits reads the stack's topology with up to five commits per
// source and returns app's.
func topologyCommits(t *testing.T, root string) []api.CommitRef {
	t.Helper()
	p, _ := New(root)
	resp, err := p.GitOpsTopologyWithCommits(context.Background(), 5)
	if err != nil {
		t.Fatalf("GitOpsTopologyWithCommits() error = %v", err)
	}
	if len(resp.Sources) != 1 {
		t.Fatalf("sources = %d, want 1", len(resp.Sources))
	}
	return resp.Sources[0].Commits
}

func TestGitOpsTopologyWithCommitsPopulatesPerSource(t *testing.T) {
	root, _ := newCommitsStack(t)
	commits := topologyCommits(t, root)
	if len(commits) != 2 {
		t.Fatalf("commits = %d, want 2: %+v", len(commits), commits)
	}
	// Newest first by committer time.
	if commits[0].Summary != "second" || commits[1].Summary != "first" {
		t.Fatalf("ordering = %v, want [second, first]", []string{commits[0].Summary, commits[1].Summary})
	}
	if len(commits[1].Parents) != 0 {
		t.Fatalf("first commit parents = %v, want []", commits[1].Parents)
	}
	if len(commits[0].Parents) != 1 || commits[0].Parents[0] == "" {
		t.Fatalf("second commit parents = %v, want [<sha>]", commits[0].Parents)
	}
}

// Adding a jj slot to a source's cache sets extensions.relativeWorktrees on
// it (`jj workspace add --colocate` adds the slot's git worktree with
// relative links), and go-git refuses to open a repository with that
// extension. The topology reads the cache's commits with the git CLI
// instead of leaving them out.
func TestGitOpsTopologyReadsTheCommitsOfACacheGoGitRefuses(t *testing.T) {
	if out, err := exec.Command("git", "--version").Output(); err != nil || !git.VersionAtLeast(string(out), git.RelativeWorktreesVersion) {
		t.Skipf("git %q (%v) has no relative worktrees", out, err)
	}
	root, cache := newCommitsStack(t)
	want := topologyCommits(t, root)
	// What jj's add does to the cache's config.
	mustGit(t, cache, "-c", "worktree.useRelativePaths=true", "worktree", "add", "--detach", filepath.Join(filepath.Dir(cache), "slot"))
	if out, err := exec.Command("git", "-C", cache, "config", "--get", "extensions.relativeWorktrees").Output(); err != nil || strings.TrimSpace(string(out)) != "true" {
		t.Fatalf("extensions.relativeWorktrees = %q (%v), want true", out, err)
	}
	if _, err := gogit.PlainOpen(cache); err == nil {
		t.Fatal("go-git opens a cache with extensions.relativeWorktrees, so this test no longer reaches the git CLI fallback")
	}

	commits := topologyCommits(t, root)
	if len(commits) != 2 || commits[0].Summary != "second" || commits[1].Summary != "first" {
		t.Fatalf("commits = %+v, want second and first", commits)
	}
	if refs := []string{"refs/heads/main", "refs/remotes/origin/main"}; !slices.Equal(commits[0].Refs, refs) {
		t.Fatalf("second's refs = %q, want %q", commits[0].Refs, refs)
	}
	if !reflect.DeepEqual(commits, want) {
		t.Fatalf("commits through the git CLI =\n%+v\nwant what go-git read before the extension:\n%+v", commits, want)
	}
}

// The git CLI fallback reads what the go-git walk reads: the same commits in
// the same order, with the same parents, refs, times, summaries and authors,
// across a merge, tags, a symbolic ref and a non-ASCII author. Each commit
// has a committer time of its own: the two order commits with the same time
// each their own way.
func TestSourceCommitsCLIMatchesGoGit(t *testing.T) {
	isolateGitConfig(t)
	ctx := context.Background()
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	cache := filepath.Join(base, "cache")
	mustGit(t, "", "init", "-q", "--bare", "--initial-branch=main", remote)
	mustGit(t, "", "clone", "-q", remote, cache)
	mustGit(t, cache, "config", "user.email", "test@example.com")
	mustGit(t, cache, "config", "user.name", "Tëst Üser")
	commit := func(when, message string) {
		t.Helper()
		t.Setenv("GIT_AUTHOR_DATE", when)
		t.Setenv("GIT_COMMITTER_DATE", when)
		mustGit(t, cache, "commit", "-q", "--allow-empty", "-m", message)
	}
	commit("2026-01-01T00:00:00+02:00", "first\n\nwith a body")
	mustGit(t, cache, "branch", "-M", "main")
	mustGit(t, cache, "branch", "topic")
	commit("2026-01-03T00:00:00Z", "second")
	mustGit(t, cache, "checkout", "-q", "topic")
	commit("2026-01-02T00:00:00Z", "topic")
	mustGit(t, cache, "checkout", "-q", "main")
	t.Setenv("GIT_COMMITTER_DATE", "2026-01-04T00:00:00Z")
	mustGit(t, cache, "merge", "-q", "--no-ff", "-m", "merge topic", "topic")
	mustGit(t, cache, "push", "-q", "origin", "main")
	mustGit(t, cache, "remote", "set-head", "origin", "main")
	mustGit(t, cache, "tag", "light", "main~1")
	mustGit(t, cache, "tag", "-a", "-m", "annotated", "annotated", "main")

	p, _ := New(filepath.Join(base, ".angee"))
	for _, limit := range []int{1, 3, 10} {
		want, err := p.sourceCommits(ctx, cache, limit)
		if err != nil {
			t.Fatalf("sourceCommits(%d) error = %v", limit, err)
		}
		if n := min(limit, 4); len(want) != n {
			t.Fatalf("sourceCommits(%d) = %d commits, want %d", limit, len(want), n)
		}
		got, err := p.sourceCommitsCLI(ctx, cache, limit)
		if err != nil {
			t.Fatalf("sourceCommitsCLI(%d) error = %v", limit, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("sourceCommitsCLI(%d) =\n%+v\nwant what go-git reads:\n%+v", limit, got, want)
		}
	}
}

// A directory that is not a repository has no commits, even inside one: the
// fallback is for a repository go-git refuses, and git would read the one
// above.
func TestSourceCommitsOfADirectoryThatIsNotARepository(t *testing.T) {
	_, cache := newCommitsStack(t)
	dir := filepath.Join(cache, "sub")
	must(t, os.Mkdir(dir, 0o755))
	p, _ := New(t.TempDir())
	if commits, err := p.sourceCommits(context.Background(), dir, 5); err != nil || commits != nil {
		t.Fatalf("sourceCommits(not a repository) = %+v, %v; want none", commits, err)
	}
}

func TestGitOpsTopologyDefaultZeroSkipsCommits(t *testing.T) {
	root := t.TempDir()
	stack := &manifest.Stack{Version: manifest.VersionCurrent, Kind: manifest.KindStack, Name: "test"}
	must(t, manifest.SaveFile(manifest.Path(root), stack))
	p, _ := New(root)
	resp, err := p.GitOpsTopology(context.Background())
	if err != nil {
		t.Fatalf("GitOpsTopology() error = %v", err)
	}
	for _, src := range resp.Sources {
		if len(src.Commits) != 0 {
			t.Fatalf("source %s carries %d commits without opt-in", src.Name, len(src.Commits))
		}
	}
	// Confirm api.SourceState zero value omits commits in JSON.
	_ = api.SourceState{}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("setup error: %v", err)
	}
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	gitArgs := append([]string{"-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}, args...)
	cmd := exec.Command("git", gitArgs...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s failed: %v\n%s", args, dir, err, out)
	}
}
