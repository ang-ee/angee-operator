package service

import (
	"errors"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/api"
)

// publishFixture is a base fixture with a workspace whose slot (on branch
// feature-a) holds a commit of its own, ready to publish.
func publishFixture(t *testing.T, sourceYAML func(remotesFixture) string) (baseFixture, string) {
	t.Helper()
	b := newBaseFixture(t, sourceYAML)
	if _, err := b.p.WorkspaceCreate(t.Context(), api.WorkspaceCreateRequest{Template: b.template, Name: "feature-a"}); err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	slot := filepath.Join(b.p.Root(), "workspaces", "feature-a")
	commitFile(t, slot, "work.txt")
	return b, slot
}

// wantPublishRefused fails unless err is an InvalidInputError on field with
// exactly reason.
func wantPublishRefused(t *testing.T, err error, field, reason string) {
	t.Helper()
	var invalid *InvalidInputError
	if !errors.As(err, &invalid) || invalid.Field != field || invalid.Reason != reason {
		t.Fatalf("WorkspaceSourcePublish() error = %v, want %s refused: %s", err, field, reason)
	}
}

// localBranchExists reports whether the checkout at dir has refs/heads/name.
func localBranchExists(t *testing.T, dir, name string) bool {
	t.Helper()
	return exec.Command("git", "-C", dir, "show-ref", "--verify", "--quiet", "refs/heads/"+name).Run() == nil
}

// Without a remote, publish goes where git itself would push the branch. The
// cache carries the source's push_default as remote.pushDefault, so that is
// where a branch without a pushRemote goes; a branch's own pushRemote comes
// first, as it does for git.
func TestPublishDefaultsToThePushRemoteGitResolves(t *testing.T) {
	threeRemotes := func(other *string) func(remotesFixture) string {
		return func(f remotesFixture) string {
			*other = filepath.Join(f.base, "other.git")
			runGit(t, "", "clone", "--bare", f.upstream, *other)
			return "    remotes:\n      fork: " + f.fork + "\n      other: " + *other + "\n      upstream:\n        url: " + f.upstream + "\n        canonical: true\n    push_default: fork\n"
		}
	}
	t.Run("push_default", func(t *testing.T) {
		var other string
		b, slot := publishFixture(t, threeRemotes(&other))
		if got := strings.TrimSpace(runGitOutput(t, slot, "config", "remote.pushDefault")); got != "fork" {
			t.Fatalf("remote.pushDefault = %q, want the source's push_default", got)
		}
		result, err := b.p.WorkspaceSourcePublish(t.Context(), "feature-a", "app", "", "")
		if err != nil || !result.OK {
			t.Fatalf("WorkspaceSourcePublish() = %#v, %v, want OK", result, err)
		}
		if heads := remoteHeads(t, b.fork); !slices.Contains(heads, "feature-a") {
			t.Fatalf("fork heads = %v, want feature-a published to the push_default", heads)
		}
		if got := strings.TrimSpace(runGitOutput(t, slot, "rev-parse", "--abbrev-ref", "@{upstream}")); got != "fork/feature-a" {
			t.Fatalf("slot upstream = %q, want fork/feature-a", got)
		}
	})
	t.Run("branch pushRemote first", func(t *testing.T) {
		var other string
		b, slot := publishFixture(t, threeRemotes(&other))
		runGit(t, slot, "config", "branch.feature-a.pushRemote", "other")
		if _, err := b.p.WorkspaceSourcePublish(t.Context(), "feature-a", "app", "", ""); err != nil {
			t.Fatalf("WorkspaceSourcePublish() error = %v", err)
		}
		if heads := remoteHeads(t, other); !slices.Contains(heads, "feature-a") {
			t.Fatalf("other heads = %v, want feature-a published to the branch's pushRemote", heads)
		}
		if heads := remoteHeads(t, b.fork); slices.Contains(heads, "feature-a") {
			t.Fatalf("fork heads = %v, want nothing published to the push_default", heads)
		}
	})
}

// A source declared with repo: alone publishes to origin, as before.
func TestPublishFromARepoSourceGoesToOrigin(t *testing.T) {
	b, _ := publishFixture(t, func(f remotesFixture) string { return "    repo: " + f.fork + "\n" })
	if _, err := b.p.WorkspaceSourcePublish(t.Context(), "feature-a", "app", "", ""); err != nil {
		t.Fatalf("WorkspaceSourcePublish() error = %v", err)
	}
	if heads := remoteHeads(t, b.fork); !slices.Contains(heads, "feature-a") {
		t.Fatalf("origin heads = %v, want feature-a published", heads)
	}
}

// A clone slot is a repository of its own, configured with the source's
// remotes and push default when it is cut; publish follows its own config.
func TestPublishFromACloneSlotGoesToItsPushDefault(t *testing.T) {
	b := newBaseFixture(t, remotesFixture.forkAndCanonicalUpstream)
	template := filepath.Join(b.base, ".templates", "workspaces", "release-clone")
	mustMkdirAll(t, filepath.Join(template, "template"))
	mustWriteFile(t, filepath.Join(template, "copier.yml"), `_subdirectory: template
_angee:
  kind: workspace
  name: release-clone
  sources:
    app:
      source: app
      subpath: .
      mode: clone
      branch: feature-c
      ref: release
`)
	if _, err := b.p.WorkspaceCreate(t.Context(), api.WorkspaceCreateRequest{Template: template, Name: "feature-c"}); err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	slot := filepath.Join(b.p.Root(), "workspaces", "feature-c")
	// A clone slot is cut at its ref; the branch to publish is made here.
	runGit(t, slot, "switch", "-c", "feature-c")
	commitFile(t, slot, "work.txt")
	if _, err := b.p.WorkspaceSourcePublish(t.Context(), "feature-c", "app", "", ""); err != nil {
		t.Fatalf("WorkspaceSourcePublish() error = %v", err)
	}
	if heads := remoteHeads(t, b.fork); !slices.Contains(heads, "feature-c") {
		t.Fatalf("fork heads = %v, want feature-c published to the push default", heads)
	}
	if heads := remoteHeads(t, b.upstream); slices.Contains(heads, "feature-c") {
		t.Fatalf("upstream heads = %v, want nothing published there", heads)
	}
}

// A remote the slot does not have is refused before anything is pushed or
// changed, by name or in a URL's form.
func TestPublishRefusesARemoteTheSlotLacks(t *testing.T) {
	b, slot := publishFixture(t, remotesFixture.forkAndCanonicalUpstream)
	head := gitRev(t, slot, "HEAD")
	for _, remote := range []string{"nosuch", "https://example.invalid/x.git"} {
		_, err := b.p.WorkspaceSourcePublish(t.Context(), "feature-a", "app", remote, "")
		wantPublishRefused(t, err, "remote", `source slot "app" has no remote "`+remote+`"`)
	}
	for _, remote := range []string{b.fork, b.upstream} {
		if heads := remoteHeads(t, remote); slices.Contains(heads, "feature-a") {
			t.Fatalf("%s heads = %v, want nothing published", remote, heads)
		}
	}
	if gitRev(t, slot, "HEAD") != head {
		t.Fatal("a refused publish changed the slot")
	}
}

// A push remote the slot's own configuration names but the slot lacks is
// refused as the slot's, not as a remote the caller passed.
func TestPublishNamesAPushRemoteTheSlotLacks(t *testing.T) {
	b, slot := publishFixture(t, remotesFixture.forkAndCanonicalUpstream)
	runGit(t, slot, "config", "remote.pushDefault", "ghost")
	_, err := b.p.WorkspaceSourcePublish(t.Context(), "feature-a", "app", "", "")
	wantPublishRefused(t, err, "remote", `source slot "app" pushes to remote "ghost" by its own configuration, but has no such remote; pass a remote, or fix the slot's push remote`)
}

// With several remotes and nothing in git's push configuration to choose
// between them, publish asks for a remote with git's reason, rather than
// guessing.
func TestPublishWithoutAPushRemoteAsksForOne(t *testing.T) {
	b, slot := publishFixture(t, func(f remotesFixture) string {
		return "    remotes:\n      fork: " + f.fork + "\n      upstream:\n        url: " + f.upstream + "\n        canonical: true\n"
	})
	runGit(t, slot, "switch", "--detach")
	_, err := b.p.WorkspaceSourcePublish(t.Context(), "feature-a", "app", "", "feature-b")
	wantPublishRefused(t, err, "remote", `source slot "app" has no push remote to default to (multiple git remotes configured; set remote.pushDefault); pass a remote, or declare push_default`)
	if localBranchExists(t, slot, "feature-b") {
		t.Fatal("a refused publish created branch feature-b")
	}
}

// A branch that matches none of the target remote's patterns is refused
// before anything is pushed; one that matches is published.
func TestPublishKeepsToTheRemotesBranchPatterns(t *testing.T) {
	for _, tc := range []struct {
		patterns string
		allowed  bool
	}{
		{patterns: `["release/*"]`},
		{patterns: `["release/*", "feature-*"]`, allowed: true},
	} {
		t.Run(tc.patterns, func(t *testing.T) {
			b, _ := publishFixture(t, func(f remotesFixture) string {
				return "    remotes:\n      origin:\n        url: " + f.fork + "\n        branches: " + tc.patterns + "\n      upstream:\n        url: " + f.upstream + "\n        canonical: true\n    push_default: origin\n"
			})
			_, err := b.p.WorkspaceSourcePublish(t.Context(), "feature-a", "app", "", "")
			published := slices.Contains(remoteHeads(t, b.fork), "feature-a")
			if tc.allowed {
				if err != nil || !published {
					t.Fatalf("WorkspaceSourcePublish() error = %v, published = %v, want it published", err, published)
				}
				return
			}
			wantPublishRefused(t, err, "branch", `remote "origin" of source "app" accepts only branches matching release/*; "feature-a" matches none`)
			if published {
				t.Fatal("a refused branch was published")
			}
		})
	}
}

// A remote the caller names is judged by its own patterns, not the default
// remote's, and wins over the push default.
func TestPublishToANamedRemoteKeepsToItsOwnPatterns(t *testing.T) {
	var other string
	b, _ := publishFixture(t, func(f remotesFixture) string {
		other = filepath.Join(f.base, "other.git")
		runGit(t, "", "clone", "--bare", f.upstream, other)
		return "    remotes:\n      origin:\n        url: " + f.fork + "\n        branches: [\"release/*\"]\n      other:\n        url: " + other + "\n        branches: [\"feature-*\"]\n      upstream:\n        url: " + f.upstream + "\n        canonical: true\n    push_default: origin\n"
	})
	_, err := b.p.WorkspaceSourcePublish(t.Context(), "feature-a", "app", "origin", "")
	wantPublishRefused(t, err, "branch", `remote "origin" of source "app" accepts only branches matching release/*; "feature-a" matches none`)
	if _, err := b.p.WorkspaceSourcePublish(t.Context(), "feature-a", "app", "other", ""); err != nil {
		t.Fatalf("WorkspaceSourcePublish(other) error = %v", err)
	}
	if heads := remoteHeads(t, other); !slices.Contains(heads, "feature-a") {
		t.Fatalf("other heads = %v, want feature-a published to the named remote", heads)
	}
	if heads := remoteHeads(t, b.fork); slices.Contains(heads, "feature-a") {
		t.Fatalf("origin heads = %v, want nothing published to the push default", heads)
	}
}

// Publishing to the canonical remote is refused before anything changes; on
// a detached slot, no branch is created. git's push block on that remote
// still refuses a push made outside angee.
func TestPublishRefusesTheCanonicalRemote(t *testing.T) {
	b, slot := publishFixture(t, remotesFixture.forkAndCanonicalUpstream)
	const reason = `remote "upstream" of source "app" is canonical (fetch-only) in angee.yaml; publish to another remote`
	_, err := b.p.WorkspaceSourcePublish(t.Context(), "feature-a", "app", "upstream", "")
	wantPublishRefused(t, err, "remote", reason)
	runGit(t, slot, "switch", "--detach")
	_, err = b.p.WorkspaceSourcePublish(t.Context(), "feature-a", "app", "upstream", "feature-b")
	wantPublishRefused(t, err, "remote", reason)
	if localBranchExists(t, slot, "feature-b") {
		t.Fatal("a refused publish created branch feature-b")
	}
	if heads := remoteHeads(t, b.upstream); slices.Contains(heads, "feature-a") || slices.Contains(heads, "feature-b") {
		t.Fatalf("upstream heads = %v, want nothing published", heads)
	}
	if out, err := exec.Command("git", "-C", slot, "push", "upstream", "HEAD:refs/heads/leak").CombinedOutput(); err == nil {
		t.Fatalf("git push upstream succeeded (%s), want the push block to refuse it", out)
	}
}

// A refused branch on a detached slot is never created: the remote's rules
// are checked before the branch the publish would name.
func TestPublishRefusedOnADetachedSlotCreatesNoBranch(t *testing.T) {
	b, slot := publishFixture(t, func(f remotesFixture) string {
		return "    remotes:\n      origin:\n        url: " + f.fork + "\n        branches: [\"topic/*\"]\n      upstream:\n        url: " + f.upstream + "\n        canonical: true\n    push_default: origin\n"
	})
	runGit(t, slot, "switch", "--detach")
	_, err := b.p.WorkspaceSourcePublish(t.Context(), "feature-a", "app", "", "feature-b")
	wantPublishRefused(t, err, "branch", `remote "origin" of source "app" accepts only branches matching topic/*; "feature-b" matches none`)
	if localBranchExists(t, slot, "feature-b") {
		t.Fatal("a refused publish created branch feature-b")
	}
	if _, err := b.p.WorkspaceSourcePublish(t.Context(), "feature-a", "app", "", "topic/one"); err != nil {
		t.Fatalf("WorkspaceSourcePublish(topic/one) error = %v", err)
	}
	if !localBranchExists(t, slot, "topic/one") || !slices.Contains(remoteHeads(t, b.fork), "topic/one") {
		t.Fatal("an allowed publish from a detached slot did not create and push its branch")
	}
}
