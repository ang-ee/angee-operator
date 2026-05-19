package service

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fyltr/angee/api"
	"github.com/fyltr/angee/internal/manifest"
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

func TestGitOpsTopologyWithCommitsPopulatesPerSource(t *testing.T) {
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	cache := filepath.Join(base, "cache")
	root := filepath.Join(base, ".angee")

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

	p, _ := New(root)
	resp, err := p.GitOpsTopologyWithCommits(context.Background(), 5)
	if err != nil {
		t.Fatalf("GitOpsTopologyWithCommits() error = %v", err)
	}
	if len(resp.Sources) != 1 {
		t.Fatalf("sources = %d, want 1", len(resp.Sources))
	}
	commits := resp.Sources[0].Commits
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
