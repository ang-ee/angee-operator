package jjdriver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/internal/git"
	"github.com/ang-ee/angee-operator/internal/vcs"
	"github.com/ang-ee/angee-operator/internal/vcs/gitdriver"
)

// requireRelativeWorktrees is requireJJ for the git jj slots need: it skips
// the test when git is older than git.RelativeWorktreesVersion, or fails it
// when ANGEE_REQUIRE_JJ=1.
func requireRelativeWorktrees(t *testing.T) {
	t.Helper()
	version, err := git.New().Version(context.Background())
	if err == nil && git.VersionAtLeast(version, git.RelativeWorktreesVersion) {
		return
	}
	reason := fmt.Sprintf("jj slots need git %s or later, not %q (%v)", git.RelativeWorktreesVersion, version, err)
	if os.Getenv("ANGEE_REQUIRE_JJ") == "1" {
		t.Fatalf("ANGEE_REQUIRE_JJ=1, but %s", reason)
	}
	t.Skip(reason)
}

// gitConfig reads key from the primary's git config, which every slot of it
// shares, and "" when it is unset.
func (f *fixture) gitConfig(key string) string {
	f.t.Helper()
	out, err := exec.Command("git", "-C", f.primary, "config", "--get", key).Output()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return ""
	}
	if err != nil {
		f.t.Fatalf("git config --get %s error = %v", key, err)
	}
	return strings.TrimSpace(string(out))
}

// TestAddChangesTheCachesGitFormat pins what Add does to the git config the
// primary shares with every slot of it. `jj workspace add --colocate` adds
// the slot's git worktree with relative links, so git sets
// core.repositoryformatversion=1 and extensions.relativeworktrees=true on
// the cache, which git before git.RelativeWorktreesVersion and go-git refuse
// to open. The git driver keeps adding its worktree slots with absolute
// links, and a git worktree slot of the same cache keeps working: one made
// before Add, and one made after it. Removing one prunes neither the other
// nor the jj slot.
func TestAddChangesTheCachesGitFormat(t *testing.T) {
	requireRelativeWorktrees(t)
	f := newPrimary(t)
	ctx := context.Background()
	gd := gitdriver.New(git.New())
	gitSlot := func(name string) vcs.Slot {
		return vcs.Slot{
			Path:       filepath.Join(f.root, "workspaces", name, "src"),
			Mode:       vcs.ModeWorktree,
			Primary:    f.primary,
			Branch:     "workspace/" + name,
			BaseRef:    "main",
			BaseRemote: f.remoteName,
		}
	}

	before := gitSlot("before")
	if err := gd.Add(ctx, before, f.dest(before), nil); err != nil {
		t.Fatalf("git driver Add(before) error = %v", err)
	}
	if format, ext := f.gitConfig("core.repositoryformatversion"), f.gitConfig("extensions.relativeworktrees"); format != "0" || ext != "" {
		t.Fatalf("before the jj slot: core.repositoryformatversion = %q, extensions.relativeworktrees = %q; want 0 and unset", format, ext)
	}

	s := f.slotSpec("workspaces/jj/src", "stack--jj", "workspace/jj")
	if err := f.add(s); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if format, ext := f.gitConfig("core.repositoryformatversion"), f.gitConfig("extensions.relativeworktrees"); format != "1" || ext != "true" {
		t.Fatalf("after the jj slot: core.repositoryformatversion = %q, extensions.relativeworktrees = %q; want 1 and true", format, ext)
	}
	if link := gitdirOf(t, s.Path); filepath.IsAbs(link) {
		t.Errorf("the jj slot's .git = %q, want the relative link that sets the extension", link)
	}

	after := gitSlot("after")
	if err := gd.Add(ctx, after, f.dest(after), nil); err != nil {
		t.Fatalf("git driver Add(after) error = %v", err)
	}
	for _, slot := range []vcs.Slot{before, after} {
		name := filepath.Base(filepath.Dir(slot.Path))
		if link := gitdirOf(t, slot.Path); !filepath.IsAbs(link) {
			t.Errorf("%s: .git = %q, want an absolute link", name, link)
		}
		if got := f.gitOut(slot.Path, "status", "--porcelain"); got != "" {
			t.Errorf("%s: git status = %q, want clean", name, got)
		}
		status, err := gd.Status(ctx, slot)
		if err != nil {
			t.Fatalf("%s: git driver Status() error = %v", name, err)
		}
		if status.CurrentRef != slot.Branch || status.MismatchReason != "" || status.Dirty || status.Ahead != 0 || status.Behind != 0 {
			t.Errorf("%s: git driver Status() = %+v, want clean on %s at its base", name, status, slot.Branch)
		}
	}
	if commits, err := git.New().Log(ctx, f.primary, 5); err != nil || len(commits) != 1 {
		t.Errorf("Log(the cache) = %+v, %v; want its one commit", commits, err)
	}

	if err := gd.Remove(ctx, before, f.dest(before)); err != nil {
		t.Fatalf("git driver Remove(before) error = %v", err)
	}
	regs := f.registrations()
	if len(regs) != 2 || strings.Contains(strings.Join(regs, "\n"), "prunable") {
		t.Fatalf("git worktrees besides the primary = %q, want the jj slot and after, neither prunable", regs)
	}
	if got := f.gitOut(s.Path, "rev-parse", "--is-inside-work-tree"); got != "true" {
		t.Errorf("the jj slot after the prune: git rev-parse --is-inside-work-tree = %q, want true", got)
	}
}

// gitdirOf is the link in the .git file of the worktree at dir.
func gitdirOf(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, ".git"))
	if err != nil {
		t.Fatalf("ReadFile(%s/.git) error = %v", dir, err)
	}
	link, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir: ")
	if !ok {
		t.Fatalf("%s/.git = %q, want a gitdir link", dir, data)
	}
	return link
}
