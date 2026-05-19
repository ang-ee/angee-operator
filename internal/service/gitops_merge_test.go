package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fyltr/angee/internal/manifest"
)

// gitMergeFixture builds a stack with one git source (`app`) and one
// workspace (`feature-a`) whose worktree is on a branch divergent from
// `main`. Returns (root, workspaceName, workspaceSourcePath).
func gitMergeFixture(t *testing.T) (string, string, string) {
	t.Helper()
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	cache := filepath.Join(base, "cache")
	root := filepath.Join(base, ".angee")
	workspaceName := "feature-a"
	workspaceSourcePath := filepath.Join(root, "workspaces", workspaceName, "app")

	mustGit(t, "", "init", "--bare", remote)
	mustGit(t, "", "clone", remote, cache)
	mustGit(t, cache, "config", "user.email", "test@example.com")
	mustGit(t, cache, "config", "user.name", "Test User")
	must(t, os.WriteFile(filepath.Join(cache, "README.md"), []byte("hello\n"), 0o644))
	mustGit(t, cache, "add", "README.md")
	mustGit(t, cache, "commit", "-m", "initial")
	mustGit(t, cache, "branch", "-M", "main")
	mustGit(t, cache, "push", "-u", "origin", "main")

	must(t, os.MkdirAll(root, 0o755))
	mustGit(t, cache, "worktree", "add", "-b", workspaceName, workspaceSourcePath, "main")
	mustGit(t, workspaceSourcePath, "config", "user.email", "test@example.com")
	mustGit(t, workspaceSourcePath, "config", "user.name", "Test User")

	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "test",
		Sources: map[string]manifest.Source{
			"app": {Kind: "git", Repo: remote, DefaultRef: "main", CachePath: cache},
		},
		Workspaces: map[string]manifest.Workspace{
			workspaceName: {
				Template: "workspaces/dev-pr",
				Sources: map[string]manifest.WorkspaceSource{
					"app": {Source: "app", Mode: "worktree", Branch: workspaceName, Ref: "main", Subpath: "app"},
				},
			},
		},
	}
	must(t, manifest.SaveFile(manifest.Path(root), stack))
	return root, workspaceName, workspaceSourcePath
}

func TestWorkspaceSourceMergeReportsConflict(t *testing.T) {
	root, ws, worktree := gitMergeFixture(t)

	// Create a diverging change on `main` (via the cache directory which is the
	// other worktree of the same repo) and a conflicting change on the
	// workspace branch.
	cache := filepath.Dir(worktree) // workspaces/<name>/
	cache = filepath.Dir(cache)     // workspaces
	cache = filepath.Dir(cache)     // root
	cache = filepath.Join(filepath.Dir(cache), "cache")
	must(t, os.WriteFile(filepath.Join(cache, "README.md"), []byte("hello from main\n"), 0o644))
	mustGit(t, cache, "add", "README.md")
	mustGit(t, cache, "commit", "-m", "main update")

	must(t, os.WriteFile(filepath.Join(worktree, "README.md"), []byte("hello from feature\n"), 0o644))
	mustGit(t, worktree, "add", "README.md")
	mustGit(t, worktree, "commit", "-m", "feature update")

	p, _ := New(root)
	result, err := p.WorkspaceSourceMerge(context.Background(), ws, "app", "main")
	if err != nil {
		t.Fatalf("Merge() error = %v (result=%+v)", err, result)
	}
	if !result.Conflicted {
		t.Fatalf("Conflicted = false, want true. Message: %s", result.Message)
	}
	if len(result.ConflictFiles) == 0 || result.ConflictFiles[0] != "README.md" {
		t.Fatalf("ConflictFiles = %v, want [README.md]", result.ConflictFiles)
	}
	if result.OK {
		t.Fatalf("OK = true on conflict")
	}

	// Abort restores the worktree.
	aborted, err := p.WorkspaceSourceMergeAbort(context.Background(), ws, "app")
	if err != nil {
		t.Fatalf("MergeAbort() error = %v", err)
	}
	if !aborted.OK {
		t.Fatalf("aborted.OK = false. Message: %s", aborted.Message)
	}
}

func TestWorkspaceSourceMergeFastForward(t *testing.T) {
	root, ws, worktree := gitMergeFixture(t)
	cache := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(worktree)))), "cache")

	// Add a commit on main that the feature branch doesn't yet have.
	must(t, os.WriteFile(filepath.Join(cache, "extra.txt"), []byte("extra\n"), 0o644))
	mustGit(t, cache, "add", "extra.txt")
	mustGit(t, cache, "commit", "-m", "extra")

	p, _ := New(root)
	result, err := p.WorkspaceSourceMerge(context.Background(), ws, "app", "main")
	if err != nil {
		t.Fatalf("Merge() error = %v\nmessage=%s", err, result.Message)
	}
	if !result.OK || result.Conflicted {
		t.Fatalf("expected ok merge, got %+v", result)
	}
}

func TestWorkspaceSourceRebaseMissingRef(t *testing.T) {
	root, ws, _ := gitMergeFixture(t)
	p, _ := New(root)
	_, err := p.WorkspaceSourceRebase(context.Background(), ws, "app", "")
	if err == nil {
		t.Fatal("Rebase(empty ref) error = nil, want InvalidInput")
	}
	if !strings.Contains(err.Error(), "ref") {
		t.Fatalf("error = %v, want mentioning 'ref'", err)
	}
}
