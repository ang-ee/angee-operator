package service

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/manifest"
)

// Regression tests for what the review of #116 found.

// useGlobalGitConfig points git at a global config holding text.
func useGlobalGitConfig(t *testing.T, base, text string) {
	t.Helper()
	global := filepath.Join(base, "gitconfig")
	mustWriteFile(t, global, text)
	t.Setenv("GIT_CONFIG_GLOBAL", global)
}

// wrapGit puts a git on PATH that runs script (sh) before the real git; the
// script exits to refuse a call.
func wrapGit(t *testing.T, base, script string) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("LookPath(git) error = %v", err)
	}
	bin := filepath.Join(base, "bin")
	mustMkdirAll(t, bin)
	mustWriteFile(t, filepath.Join(bin, "git"), "#!/bin/sh\n"+script+"\nexec "+realGit+" \"$@\"\n")
	if err := os.Chmod(filepath.Join(bin, "git"), 0o755); err != nil {
		t.Fatalf("Chmod(git) error = %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// stagingLeftovers lists the clone staging directories left beside path.
func stagingLeftovers(t *testing.T, path string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".clone-*"))
	if err != nil {
		t.Fatalf("Glob() error = %v", err)
	}
	return matches
}

// Without --origin, git names a clone's remote after the user's
// clone.defaultRemoteName. A canonical origin would then arrive as an
// undeclared remote the reconcile never blocks, which main tracks.
func TestCloneIgnoresTheUsersDefaultRemoteName(t *testing.T) {
	f := newRemotesFixture(t)
	useGlobalGitConfig(t, f.base, "[clone]\n\tdefaultRemoteName = upstream\n")
	p := f.writeStack(t, "    remotes:\n      origin:\n        url: "+f.upstream+"\n        canonical: true\n      fork: "+f.fork+"\n    push_default: fork\n")

	if err := p.materializeSource(t.Context(), "app", loadSource(t, p, "app"), true); err != nil {
		t.Fatalf("materializeSource() error = %v", err)
	}
	if got := runGitOutput(t, f.cache, "remote"); got != "fork\norigin\n" {
		t.Fatalf("cache remotes = %q, want fork and origin only", got)
	}
	if got := strings.TrimSpace(runGitOutput(t, f.cache, "config", "--get", "branch.main.remote")); got != "origin" {
		t.Fatalf("branch.main.remote = %q, want origin", got)
	}
	if got := strings.TrimSpace(runGitOutput(t, f.cache, "remote", "get-url", "--push", "origin")); got != canonicalPushURL("origin") {
		t.Fatalf("origin push url = %q, want the block", got)
	}
	commitFile(t, f.cache, "leak.txt")
	_ = runGitOutputAllowFailure(t, f.cache, "push", "upstream", "HEAD:refs/heads/leak")
	_ = runGitOutputAllowFailure(t, f.cache, "push", "origin", "HEAD:refs/heads/leak")
	if heads := remoteHeads(t, f.upstream); slices.Contains(heads, "leak") {
		t.Fatalf("canonical remote heads = %v, want no pushed branch", heads)
	}
}

// The template cache reads origin/HEAD, so it must be cloned as origin
// whatever the user's clone.defaultRemoteName says.
func TestTemplateRepoIsClonedAsOrigin(t *testing.T) {
	f := newRemotesFixture(t)
	useGlobalGitConfig(t, f.base, "[clone]\n\tdefaultRemoteName = upstream\n")
	p := f.writeStack(t, "    repo: "+f.upstream+"\n")
	repoDir := filepath.Join(f.base, "templates-cache")
	if err := p.refreshTemplateRepo(t.Context(), f.upstream, repoDir, ""); err != nil {
		t.Fatalf("refreshTemplateRepo() error = %v", err)
	}
	if got := runGitOutput(t, repoDir, "remote"); got != "origin\n" {
		t.Fatalf("template cache remotes = %q, want origin", got)
	}
}

// Every way of pushing from a cache leaves the canonical remote unchanged,
// and SourcePush is refused.
func TestCanonicalBlockRefusesEveryPushForm(t *testing.T) {
	f := newRemotesFixture(t)
	p := f.writeStack(t, "    remotes:\n      upstream:\n        url: "+f.upstream+"\n        canonical: true\n")
	if err := p.materializeSource(t.Context(), "app", loadSource(t, p, "app"), true); err != nil {
		t.Fatalf("materializeSource() error = %v", err)
	}
	commitFile(t, f.cache, "x.txt")
	runGit(t, f.cache, "branch", "extra")
	runGit(t, f.cache, "tag", "v-test")
	before := runGitOutput(t, "", "ls-remote", f.upstream)
	for _, args := range [][]string{
		{"push"},
		{"push", "--all"},
		{"push", "--all", "upstream"},
		{"push", "--mirror", "upstream"},
		{"push", "--tags", "upstream"},
		{"push", "--force", "upstream", "main"},
		{"push", "--repo=upstream"},
		{"push", "-u", "upstream", "extra"},
	} {
		_ = runGitOutputAllowFailure(t, f.cache, args...)
	}
	if _, err := p.SourcePush(t.Context(), "app", "extra"); err == nil {
		t.Fatal("SourcePush() error = nil, want the push to the canonical remote refused")
	}
	if after := runGitOutput(t, "", "ls-remote", f.upstream); after != before {
		t.Fatalf("canonical remote changed:\nbefore %s\nafter %s", before, after)
	}
}

// baseFixture gives the canonical upstream and the fork each a `release`
// branch at a different commit, and a stack whose angee.yaml declares the
// source and a worktree workspace template on `release`.
type baseFixture struct {
	remotesFixture
	p                       *Platform
	template                string
	upstreamRelease, forked string
}

func newBaseFixture(t *testing.T, sourceYAML func(remotesFixture) string) baseFixture {
	t.Helper()
	b := baseFixture{remotesFixture: newRemotesFixture(t)}
	b.upstreamRelease = commitOnBranch(t, b.base, b.upstream, "release", "upstream.txt")
	b.forked = commitOnBranch(t, b.base, b.fork, "release", "fork.txt")
	b.p = b.writeStack(t, sourceYAML(b.remotesFixture))
	b.template = filepath.Join(b.base, ".templates", "workspaces", "release")
	mustMkdirAll(t, filepath.Join(b.template, "template"))
	mustWriteFile(t, filepath.Join(b.template, "copier.yml"), `_subdirectory: template
_angee:
  kind: workspace
  name: release
  sources:
    app:
      source: app
      subpath: .
      mode: worktree
      branch: feature-a
      ref: release
`)
	// Clone, then fetch every declared remote, so both release branches are
	// known to the cache.
	for range 2 {
		if _, err := b.p.SourceFetch(t.Context(), "app"); err != nil {
			t.Fatalf("SourceFetch() error = %v", err)
		}
	}
	return b
}

// commitOnBranch adds a commit on branch (from main when the remote lacks it)
// in the bare remote and returns it.
func commitOnBranch(t *testing.T, base, remote, branch, file string) string {
	t.Helper()
	work, err := os.MkdirTemp(base, "work-")
	if err != nil {
		t.Fatalf("MkdirTemp() error = %v", err)
	}
	runGit(t, "", "clone", remote, work)
	start := "origin/main"
	if exec.Command("git", "-C", work, "rev-parse", "--verify", "--quiet", "origin/"+branch).Run() == nil {
		start = "origin/" + branch
	}
	runGit(t, work, "switch", "-C", branch, start)
	commitFile(t, work, file)
	runGit(t, work, "push", "origin", branch)
	return strings.TrimSpace(runGitOutput(t, work, "rev-parse", "HEAD"))
}

// With origin a fork and a canonical upstream, the base comes from the
// canonical remote in all three places that resolve it: a new worktree slot's
// start point, the slot's status, and sync-base.
func TestBaseComesFromTheCanonicalRemote(t *testing.T) {
	b := newBaseFixture(t, remotesFixture.forkAndCanonicalUpstream)
	b.assertBaseFromUpstream(t)
}

// Without an origin, the base still comes from the canonical remote rather
// than from whichever remote git lists first.
func TestBaseComesFromTheCanonicalRemoteWithoutOrigin(t *testing.T) {
	b := newBaseFixture(t, func(f remotesFixture) string {
		return "    remotes:\n      fork: " + f.fork + "\n      upstream:\n        url: " + f.upstream + "\n        canonical: true\n    push_default: fork\n"
	})
	b.assertBaseFromUpstream(t)
}

func (b baseFixture) assertBaseFromUpstream(t *testing.T) {
	t.Helper()
	ctx := t.Context()
	if _, err := b.p.WorkspaceCreate(ctx, api.WorkspaceCreateRequest{Template: b.template, Name: "feature-a"}); err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	slot := filepath.Join(b.p.Root(), "workspaces", "feature-a")
	if got := strings.TrimSpace(runGitOutput(t, slot, "rev-parse", "HEAD")); got != b.upstreamRelease {
		t.Fatalf("new slot HEAD = %s, want upstream's release %s (fork's is %s)", got, b.upstreamRelease, b.forked)
	}
	status, err := b.p.WorkspaceStatus(ctx, "feature-a")
	if err != nil {
		t.Fatalf("WorkspaceStatus() error = %v", err)
	}
	if source := status.Sources[0]; !source.Pushed || source.Ahead != 0 || source.Behind != 0 {
		t.Fatalf("slot status = %#v, want pushed and level with upstream's release", source)
	}

	next := commitOnBranch(t, b.base, b.upstream, "release", "upstream-next.txt")
	if _, err := b.p.WorkspaceSyncBase(ctx, "feature-a", "merge"); err != nil {
		t.Fatalf("WorkspaceSyncBase() error = %v", err)
	}
	if !isAncestor(t, slot, next) {
		t.Fatalf("slot does not hold upstream's new release commit %s after sync-base", next)
	}
	if isAncestor(t, slot, b.forked) {
		t.Fatalf("slot holds the fork's release commit %s after sync-base", b.forked)
	}
}

func isAncestor(t *testing.T, dir, commit string) bool {
	t.Helper()
	cmd := exec.Command("git", "merge-base", "--is-ancestor", commit, "HEAD")
	cmd.Dir = dir
	return cmd.Run() == nil
}

// A cache cloned under repo: tracks origin; once remotes: names origin a fork,
// `source pull` makes main track the canonical remote and pulls from it.
func TestSourcePullOnAMigratedCacheFollowsTheCanonicalRemote(t *testing.T) {
	var stderr bytes.Buffer
	ctx := warnLogContext(t, &stderr)
	f := newRemotesFixture(t)
	runGit(t, "", "clone", "--branch", "main", f.upstream, f.cache)
	newHead := commitOnBranch(t, f.base, f.upstream, "main", "new-upstream.txt")
	p := f.writeStack(t, f.forkAndCanonicalUpstream())

	if _, err := p.SourcePull(ctx, "app"); err != nil {
		t.Fatalf("SourcePull() error = %v", err)
	}
	if got := strings.TrimSpace(runGitOutput(t, f.cache, "rev-parse", "main")); got != newHead {
		t.Fatalf("cache main = %s, want the canonical upstream's %s", got, newHead)
	}
	if got := strings.TrimSpace(runGitOutput(t, f.cache, "rev-parse", "--abbrev-ref", "main@{upstream}")); got != "upstream/main" {
		t.Fatalf("main tracks %q, want upstream/main", got)
	}
	if want := "warning: source \"app\" cache branch main tracked origin/main; set it to track upstream/main from angee.yaml\n"; !strings.Contains(stderr.String(), want) {
		t.Fatalf("warnings = %q, want %q", stderr.String(), want)
	}
}

// The repo: form leaves tracking and push URLs alone: another stack may share
// the cache under remotes:, with a canonical block this stack must not lift.
func TestRepoFormLeavesTrackingAndPushURLsAlone(t *testing.T) {
	var stderr bytes.Buffer
	ctx := warnLogContext(t, &stderr)
	f := newRemotesFixture(t)
	runGit(t, "", "clone", "--branch", "main", f.upstream, f.cache)
	runGit(t, f.cache, "config", "remote.origin.pushurl", canonicalPushURL("origin"))
	runGit(t, f.cache, "remote", "add", "mine", f.fork)
	runGit(t, f.cache, "fetch", "mine")
	runGit(t, f.cache, "branch", "--set-upstream-to", "mine/main", "main")
	p := f.writeStack(t, "    repo: "+f.upstream+"\n")
	before := gitConfigBytes(t, f.cache)

	if err := p.materializeSource(ctx, "app", loadSource(t, p, "app"), true); err != nil {
		t.Fatalf("materializeSource(bestEffort) error = %v", err)
	}
	if _, err := p.SourceFetch(ctx, "app"); err != nil {
		t.Fatalf("SourceFetch() error = %v", err)
	}
	if after := gitConfigBytes(t, f.cache); !bytes.Equal(before, after) {
		t.Fatalf("repo:-form reconcile changed the config:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("warnings = %q, want none", got)
	}
}

// A new cache appears at its path only once it is configured: the reconcile
// runs in a staging directory beside it, then the clone is renamed into place.
func TestMaterializeSourceConfiguresTheCloneBeforeItAppears(t *testing.T) {
	f := newRemotesFixture(t)
	p := f.writeStack(t, f.forkAndCanonicalUpstream())
	t.Setenv("ANGEE_TEST_CACHE", f.cache)
	wrapGit(t, f.base, `case "$1 $2" in "remote add"|"config --replace-all") if [ -e "$ANGEE_TEST_CACHE" ]; then echo "configured after the cache appeared" >&2; exit 1; fi;; esac`)

	if err := p.materializeSource(t.Context(), "app", loadSource(t, p, "app"), true); err != nil {
		t.Fatalf("materializeSource() error = %v", err)
	}
	if got := runGitOutput(t, f.cache, "remote"); got != "origin\nupstream\n" {
		t.Fatalf("cache remotes = %q, want origin and upstream", got)
	}
	if got := strings.TrimSpace(runGitOutput(t, f.cache, "remote", "get-url", "--push", "upstream")); got != canonicalPushURL("upstream") {
		t.Fatalf("upstream push url = %q, want the block", got)
	}
	if left := stagingLeftovers(t, f.cache); len(left) != 0 {
		t.Fatalf("staging directories left = %v", left)
	}
}

// A path that exists and is not a cache is someone else's: a clone that cannot
// be moved there fails, and leaves the path and nothing else behind.
func TestMaterializeSourceLeavesAnOccupiedPathAlone(t *testing.T) {
	f := newRemotesFixture(t)
	p := f.writeStack(t, f.forkAndCanonicalUpstream())
	mustMkdirAll(t, f.cache)
	mustWriteFile(t, filepath.Join(f.cache, "other.txt"), "someone else's\n")

	err := p.materializeSource(t.Context(), "app", loadSource(t, p, "app"), true)
	if err == nil || !strings.Contains(err.Error(), "cache path "+f.cache+" is not empty and holds no git checkout") {
		t.Fatalf("materializeSource() error = %v, want the occupied path refused", err)
	}
	if data, err := os.ReadFile(filepath.Join(f.cache, "other.txt")); err != nil || string(data) != "someone else's\n" {
		t.Fatalf("ReadFile(other.txt) = (%q, %v), want it untouched", data, err)
	}
	if left := stagingLeftovers(t, f.cache); len(left) != 0 {
		t.Fatalf("staging directories left = %v", left)
	}
}

// A clone slot is configured only when it is created. When angee.yaml marks
// one of its remotes canonical later, reading the slot (status, repair) says
// so, and changes nothing.
func TestReadingACloneSlotReportsANewCanonicalRemote(t *testing.T) {
	// A git slot with no mode is a clone too.
	for name, modeLine := range map[string]string{"mode clone": "      mode: clone\n", "no mode": ""} {
		t.Run(name, func(t *testing.T) {
			assertCloneSlotReportsANewCanonicalRemote(t, modeLine)
		})
	}
}

// createCloneSlotWorkspace creates workspace feature-a from a template whose
// one slot, app, is a git clone at the workspace root with an inline source
// (origin the fork, upstream not canonical); modeLine is the slot's mode line,
// or "" for none.
func createCloneSlotWorkspace(t *testing.T, ctx context.Context, f remotesFixture, modeLine string) (*Platform, string) {
	t.Helper()
	root := filepath.Join(f.base, ".angee")
	templateRoot := filepath.Join(f.base, ".templates", "workspaces", "slot")
	mustMkdirAll(t, filepath.Join(templateRoot, "template"))
	mustWriteFile(t, filepath.Join(templateRoot, "copier.yml"), `_subdirectory: template
_angee:
  kind: workspace
  name: slot
  sources:
    app:
      remotes:
        origin: `+f.fork+`
        upstream: `+f.upstream+`
      cache_path: .cache/app
      default_ref: main
      subpath: .
`+modeLine+`      branch: feature-a
      ref: main
`)
	p, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := p.WorkspaceCreate(ctx, api.WorkspaceCreateRequest{Template: templateRoot, Name: "feature-a"}); err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	return p, filepath.Join(root, "workspaces", "feature-a")
}

func assertCloneSlotReportsANewCanonicalRemote(t *testing.T, modeLine string) {
	var stderr bytes.Buffer
	ctx := warnLogContext(t, &stderr)
	f := newRemotesFixture(t)
	p, slot := createCloneSlotWorkspace(t, ctx, f, modeLine)
	root := p.Root()
	if _, err := os.Stat(filepath.Join(slot, ".git", "config")); err != nil {
		t.Fatalf("slot is not a clone of its own: %v", err)
	}
	stack, err := p.LoadStack()
	if err != nil {
		t.Fatalf("LoadStack() error = %v", err)
	}
	source := stack.Sources["app"]
	source.Remotes["upstream"] = manifest.SourceRemote{URL: f.upstream, Canonical: true}
	stack.Sources["app"] = source
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile() error = %v", err)
	}
	before := gitConfigBytes(t, slot)
	stderr.Reset()

	// Marking upstream canonical also makes it the clone remote, which main
	// should now track.
	want := "warning: workspace \"feature-a\" clone slot \"app\" can push to upstream, which angee.yaml marks canonical (fetch-only); use git in " + slot + " to block pushes to it\n" +
		"warning: workspace \"feature-a\" clone slot \"app\" branch main tracks origin/main, but angee.yaml takes it from upstream/main; use git in " + slot + " to switch it\n"
	if _, err := p.WorkspaceStatus(ctx, "feature-a"); err != nil {
		t.Fatalf("WorkspaceStatus() error = %v", err)
	}
	if got := stderr.String(); got != want {
		t.Fatalf("status warnings = %q, want %q", got, want)
	}
	stderr.Reset()
	if _, err := p.WorkspaceRepair(ctx, "feature-a"); err != nil {
		t.Fatalf("WorkspaceRepair() error = %v", err)
	}
	if got := stderr.String(); !strings.Contains(got, want) {
		t.Fatalf("repair warnings = %q, want %q", got, want)
	}
	if after := gitConfigBytes(t, slot); !bytes.Equal(before, after) {
		t.Fatalf("reading the slot changed its config:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// A clone slot whose .git is gone is not checked: git would read the remotes
// of the repository around it.
func TestReadingACloneSlotWithoutGitIsNotChecked(t *testing.T) {
	var stderr bytes.Buffer
	ctx := warnLogContext(t, &stderr)
	f := newRemotesFixture(t)
	p, slot := createCloneSlotWorkspace(t, ctx, f, "")
	runGit(t, "", "init", p.Root())
	runGit(t, p.Root(), "remote", "add", "origin", "https://example.com/enclosing.git")
	if err := os.RemoveAll(filepath.Join(slot, ".git")); err != nil {
		t.Fatalf("RemoveAll(.git) error = %v", err)
	}
	stderr.Reset()

	if _, err := p.WorkspaceStatus(ctx, "feature-a"); err != nil {
		t.Fatalf("WorkspaceStatus() error = %v", err)
	}
	if got := stderr.String(); strings.Contains(got, "clone slot") {
		t.Fatalf("status warnings = %q, want none about the slot's remotes", got)
	}
}

// A new cache has the permissions a plain git clone gives, not those of the
// 0700 temporary directory it is staged in, so other users (a container
// mounting the source, a worktree slot's .git pointing into the cache) can
// read it.
func TestNewCacheHasThePermissionsOfAPlainClone(t *testing.T) {
	f := newRemotesFixture(t)
	p := f.writeStack(t, f.forkAndCanonicalUpstream())
	if err := p.materializeSource(t.Context(), "app", loadSource(t, p, "app"), true); err != nil {
		t.Fatalf("materializeSource() error = %v", err)
	}
	plain := filepath.Join(f.base, "plain")
	runGit(t, "", "clone", f.upstream, plain)
	for _, rel := range []string{".", ".git"} {
		got, err := os.Stat(filepath.Join(f.cache, rel))
		if err != nil {
			t.Fatalf("Stat(cache %s) error = %v", rel, err)
		}
		want, err := os.Stat(filepath.Join(plain, rel))
		if err != nil {
			t.Fatalf("Stat(plain %s) error = %v", rel, err)
		}
		if got.Mode().Perm() != want.Mode().Perm() {
			t.Fatalf("cache %s mode = %v, want a plain clone's %v", rel, got.Mode().Perm(), want.Mode().Perm())
		}
	}
}

// A cache_path that is a symlink to an empty directory is cloned into the
// target, as git clone does, and the symlink stays.
func TestSymlinkedCachePathIsClonedIntoItsTarget(t *testing.T) {
	f := newRemotesFixture(t)
	target := filepath.Join(f.base, "cache-target")
	mustMkdirAll(t, target)
	if err := os.Symlink(target, f.cache); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	p := f.writeStack(t, f.forkAndCanonicalUpstream())

	if err := p.materializeSource(t.Context(), "app", loadSource(t, p, "app"), true); err != nil {
		t.Fatalf("materializeSource() error = %v", err)
	}
	if info, err := os.Lstat(f.cache); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("Lstat(cache) = (%v, %v), want the symlink kept", info, err)
	}
	if got := strings.TrimSpace(runGitOutput(t, target, "remote", "get-url", "--push", "upstream")); got != canonicalPushURL("upstream") {
		t.Fatalf("target upstream push url = %q, want the block", got)
	}
}

// A cache_path that is a symlink to nothing is refused, as git clone refuses
// it; renaming the staged clone onto it would replace the symlink itself.
func TestDanglingSymlinkedCachePathIsRefused(t *testing.T) {
	f := newRemotesFixture(t)
	if err := os.Symlink(filepath.Join(f.base, "missing"), f.cache); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	p := f.writeStack(t, f.forkAndCanonicalUpstream())

	if err := p.materializeSource(t.Context(), "app", loadSource(t, p, "app"), true); err == nil || !strings.Contains(err.Error(), "cache path "+f.cache) {
		t.Fatalf("materializeSource() error = %v, want the dangling cache path refused", err)
	}
	if info, err := os.Lstat(f.cache); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("Lstat(cache) = (%v, %v), want the symlink kept", info, err)
	}
}

// A cache_path that is an existing empty directory (a mount point, say) is
// cloned in place and configured; a failed configuration leaves it empty.
func TestEmptyCachePathIsClonedInPlace(t *testing.T) {
	f := newRemotesFixture(t)
	mustMkdirAll(t, f.cache)
	p := f.writeStack(t, f.forkAndCanonicalUpstream())
	if err := p.materializeSource(t.Context(), "app", loadSource(t, p, "app"), true); err != nil {
		t.Fatalf("materializeSource() error = %v", err)
	}
	if got := strings.TrimSpace(runGitOutput(t, f.cache, "remote", "get-url", "--push", "upstream")); got != canonicalPushURL("upstream") {
		t.Fatalf("upstream push url = %q, want the block", got)
	}
	if left := stagingLeftovers(t, f.cache); len(left) != 0 {
		t.Fatalf("staging directories left = %v", left)
	}

	g := newRemotesFixture(t)
	mustMkdirAll(t, g.cache)
	q := g.writeStack(t, g.forkAndCanonicalUpstream())
	wrapGit(t, g.base, `if [ "$1" = remote ] && [ "$2" = add ]; then echo 'refused by the test' >&2; exit 1; fi`)
	if err := q.materializeSource(t.Context(), "app", loadSource(t, q, "app"), true); err == nil || !strings.Contains(err.Error(), "refused by the test") {
		t.Fatalf("materializeSource() error = %v, want the refused remote add", err)
	}
	if empty, err := emptyDirectory(g.cache); err != nil || !empty {
		t.Fatalf("emptyDirectory(cache) = (%v, %v), want the directory kept and empty", empty, err)
	}
}

// Staging directories an interrupted clone left are removed by the next one.
// A directory that only starts with the staging prefix, without the digits
// os.MkdirTemp appends, is someone else's and stays.
func TestMaterializeSourceRemovesStaleCloneStages(t *testing.T) {
	f := newRemotesFixture(t)
	prefix := cloneStagePrefix(filepath.Base(f.cache))
	stale := filepath.Join(f.base, prefix+"1234567")
	mustMkdirAll(t, filepath.Join(stale, "clone"))
	mustWriteFile(t, filepath.Join(stale, "clone", "half.txt"), "")
	sibling := filepath.Join(f.base, prefix+"mine")
	mustMkdirAll(t, sibling)
	mustWriteFile(t, filepath.Join(sibling, "keep.txt"), "")
	p := f.writeStack(t, f.forkAndCanonicalUpstream())

	if err := p.materializeSource(t.Context(), "app", loadSource(t, p, "app"), true); err != nil {
		t.Fatalf("materializeSource() error = %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("Stat(stale stage) error = %v, want it removed", err)
	}
	if _, err := os.Stat(filepath.Join(sibling, "keep.txt")); err != nil {
		t.Fatalf("Stat(sibling) error = %v, want it kept", err)
	}
}

// A clone into an empty cache path that fails leaves what is in the path: it
// may be another writer's, which started between the emptiness check and the
// clone.
func TestFailedInPlaceCloneKeepsAnotherWritersFiles(t *testing.T) {
	f := newRemotesFixture(t)
	mustMkdirAll(t, f.cache)
	p := f.writeStack(t, f.forkAndCanonicalUpstream())
	t.Setenv("ANGEE_TEST_CACHE", f.cache)
	wrapGit(t, f.base, `if [ "$1" = clone ]; then mkdir -p "$ANGEE_TEST_CACHE/.git" && echo other > "$ANGEE_TEST_CACHE/.git/HEAD-of-the-other-clone"; fi`)

	if err := p.materializeSource(t.Context(), "app", loadSource(t, p, "app"), true); err == nil {
		t.Fatal("materializeSource() error = nil, want the clone into a non-empty path to fail")
	}
	if _, err := os.Stat(filepath.Join(f.cache, ".git", "HEAD-of-the-other-clone")); err != nil {
		t.Fatalf("Stat(the other writer's file) error = %v, want it kept", err)
	}
}

// The tracking target is passed as a full ref, so a local branch named like
// the remote branch does not make it ambiguous.
func TestAdoptTracksTheRemoteBranchBesideALocalNamesake(t *testing.T) {
	f := newRemotesFixture(t)
	runGit(t, "", "clone", "--branch", "main", f.upstream, f.cache)
	runGit(t, f.cache, "branch", "upstream/main")
	p := f.writeStack(t, f.forkAndCanonicalUpstream())

	// The adopt path that `source fetch` runs; its status read afterwards is a
	// separate matter (it counts against the short upstream name).
	if err := p.materializeSource(t.Context(), "app", loadSource(t, p, "app"), false); err != nil {
		t.Fatalf("materializeSource(adopt) error = %v", err)
	}
	if got := strings.TrimSpace(runGitOutput(t, f.cache, "config", "--get", "branch.main.remote")); got != "upstream" {
		t.Fatalf("branch.main.remote = %q, want upstream", got)
	}
	if got := strings.TrimSpace(runGitOutput(t, f.cache, "config", "--get", "branch.main.merge")); got != "refs/heads/main" {
		t.Fatalf("branch.main.merge = %q, want refs/heads/main", got)
	}
}
