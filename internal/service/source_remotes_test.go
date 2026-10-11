package service

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/copierx"
	"github.com/ang-ee/angee-operator/internal/manifest"
)

// remotesFixture is a canonical upstream holding main and a fork of it, with a
// stack root whose angee.yaml the test writes.
type remotesFixture struct {
	base, upstream, fork, cache, root string
	upstreamHead                      string
}

func newRemotesFixture(t *testing.T) remotesFixture {
	t.Helper()
	base := t.TempDir()
	f := remotesFixture{
		base:     base,
		upstream: filepath.Join(base, "upstream.git"),
		fork:     filepath.Join(base, "fork.git"),
		cache:    filepath.Join(base, "cache"),
		root:     filepath.Join(base, "stack"),
	}
	seedWorktreeRemote(t, base, f.upstream)
	runGit(t, "", "clone", "--bare", f.upstream, f.fork)
	f.upstreamHead = strings.TrimSpace(runGitOutput(t, "", "--git-dir", f.upstream, "rev-parse", "main"))
	if err := os.MkdirAll(f.root, 0o755); err != nil {
		t.Fatalf("MkdirAll(root) error = %v", err)
	}
	return f
}

// writeStack writes angee.yaml with one git source "app" whose source lines
// (remotes, push_default) are sourceYAML, indented four spaces.
func (f remotesFixture) writeStack(t *testing.T, sourceYAML string) *Platform {
	t.Helper()
	mustWriteFile(t, filepath.Join(f.root, "angee.yaml"), "version: 1\nkind: stack\nname: remotes\nsources:\n  app:\n    kind: git\n    cache_path: "+f.cache+"\n    default_ref: main\n"+sourceYAML)
	p, err := New(f.root)
	if err != nil {
		t.Fatalf("New(root) error = %v", err)
	}
	return p
}

func (f remotesFixture) forkAndCanonicalUpstream() string {
	return "    remotes:\n      origin: " + f.fork + "\n      upstream:\n        url: " + f.upstream + "\n        canonical: true\n    push_default: origin\n"
}

func loadSource(t *testing.T, p *Platform, name string) manifest.Source {
	t.Helper()
	stack, err := p.LoadStack()
	if err != nil {
		t.Fatalf("LoadStack() error = %v", err)
	}
	return stack.Sources[name]
}

func mustMkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", dir, err)
	}
}

func gitConfigBytes(t *testing.T, repo string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repo, ".git", "config"))
	if err != nil {
		t.Fatalf("ReadFile(%s config) error = %v", repo, err)
	}
	return data
}

// assertCanonicalBlocked checks that repo fetches from the canonical upstream
// but a real push to it fails and leaves it unchanged.
func assertCanonicalBlocked(t *testing.T, f remotesFixture, repo string) {
	t.Helper()
	if got := strings.TrimSpace(runGitOutput(t, repo, "config", "--get", "remote.upstream.url")); got != f.upstream {
		t.Fatalf("%s upstream url = %q, want %q", repo, got, f.upstream)
	}
	if got := strings.TrimSpace(runGitOutput(t, repo, "remote", "get-url", "--push", "upstream")); got != canonicalPushURL("upstream") {
		t.Fatalf("%s upstream push url = %q, want %q", repo, got, canonicalPushURL("upstream"))
	}
	out := runGitOutputAllowFailure(t, repo, "push", "upstream", "HEAD:refs/heads/pushed-to-canonical")
	if !strings.Contains(out, "angee-fetch-only") {
		t.Fatalf("push to the canonical remote output = %q, want it refused by the fetch-only push URL", out)
	}
	if heads := remoteHeads(t, f.upstream); slices.Contains(heads, "pushed-to-canonical") {
		t.Fatalf("canonical remote heads = %v, want no pushed branch", heads)
	}
}

// A missing cache is cloned from the canonical remote under its own name and
// gets the other declared remotes, a push block on the canonical one and the
// push default, all without a warning. Worktree slots share that config.
func TestMaterializeSourceClonesFromCanonicalAndBlocksPushes(t *testing.T) {
	var stderr bytes.Buffer
	ctx := warnLogContext(t, &stderr)
	f := newRemotesFixture(t)
	p := f.writeStack(t, f.forkAndCanonicalUpstream())

	if err := p.materializeSource(ctx, "app", loadSource(t, p, "app"), true); err != nil {
		t.Fatalf("materializeSource() error = %v", err)
	}
	if got := strings.TrimSpace(runGitOutput(t, f.cache, "rev-parse", "HEAD")); got != f.upstreamHead {
		t.Fatalf("cache HEAD = %s, want upstream's main %s", got, f.upstreamHead)
	}
	if got := strings.TrimSpace(runGitOutput(t, f.cache, "config", "--get", "remote.origin.url")); got != f.fork {
		t.Fatalf("cache origin url = %q, want the fork %q", got, f.fork)
	}
	if got := strings.TrimSpace(runGitOutput(t, f.cache, "config", "--get", "remote.pushDefault")); got != "origin" {
		t.Fatalf("cache remote.pushDefault = %q, want origin", got)
	}
	// main tracks the canonical remote it was cloned from, not the fork.
	if got := strings.TrimSpace(runGitOutput(t, f.cache, "config", "--get", "branch.main.remote")); got != "upstream" {
		t.Fatalf("cache branch.main.remote = %q, want upstream", got)
	}
	if remote, err := p.gitClient().PushRemote(ctx, f.cache); err != nil || remote != "origin" {
		t.Fatalf("PushRemote() = (%q, %v), want origin", remote, err)
	}
	commitFile(t, f.cache, "local.txt")
	assertCanonicalBlocked(t, f, f.cache)

	slot := filepath.Join(f.base, "slot")
	runGit(t, f.cache, "worktree", "add", "--detach", slot, "main")
	assertCanonicalBlocked(t, f, slot)
	if got := stderr.String(); got != "" {
		t.Fatalf("warning output = %q, want none for a fresh clone", got)
	}
}

// A source declared with repo: is cloned as before: origin, no push URL, no
// push default.
func TestMaterializeSourceRepoFormClonesOrigin(t *testing.T) {
	f := newRemotesFixture(t)
	p := f.writeStack(t, "    repo: "+f.upstream+"\n")
	if err := p.materializeSource(t.Context(), "app", loadSource(t, p, "app"), true); err != nil {
		t.Fatalf("materializeSource() error = %v", err)
	}
	if got := runGitOutput(t, f.cache, "remote"); got != "origin\n" {
		t.Fatalf("cache remotes = %q, want origin only", got)
	}
	if got := runGitOutputAllowFailure(t, f.cache, "config", "--get-regexp", `^remote\.(pushdefault|origin\.pushurl)$`); got != "" {
		t.Fatalf("cache push config = %q, want none", got)
	}
}

// startFromOldStyleCache clones the cache the way a repo:-only manifest did
// (origin pointing at the upstream) and adds a remote of the user's own.
func startFromOldStyleCache(t *testing.T, f remotesFixture) {
	t.Helper()
	mine := filepath.Join(f.base, "mine.git")
	runGit(t, "", "init", "--bare", mine)
	runGit(t, "", "clone", "--branch", "main", f.upstream, f.cache)
	runGit(t, f.cache, "remote", "add", "mine", mine)
	runGit(t, f.cache, "config", "remote.mine.pushurl", "https://example.com/mine-push.git")
}

// The explicit verbs write the declared remotes, say what changed, and change
// nothing on a second run. A remote the manifest does not declare is left as
// it was.
func TestSourceFetchReconcilesDeclaredRemotesIdempotently(t *testing.T) {
	var stderr bytes.Buffer
	ctx := warnLogContext(t, &stderr)
	f := newRemotesFixture(t)
	startFromOldStyleCache(t, f)
	p := f.writeStack(t, f.forkAndCanonicalUpstream())

	if _, err := p.SourceFetch(ctx, "app"); err != nil {
		t.Fatalf("SourceFetch() error = %v", err)
	}
	wantWarnings := []string{
		"warning: source \"app\" cache had origin " + f.upstream + "; set origin to " + f.fork + " from angee.yaml",
		"warning: source \"app\" cache had no upstream remote; set upstream to " + f.upstream + " from angee.yaml",
		"warning: source \"app\" cache could push to upstream; blocked pushes to it, since angee.yaml marks it canonical (fetch-only)",
		"warning: source \"app\" cache pushed to no configured remote by default; set push_default origin from angee.yaml",
		"warning: source \"app\" cache branch main tracked origin/main; set it to track upstream/main from angee.yaml",
	}
	if got := strings.Split(strings.TrimSuffix(stderr.String(), "\n"), "\n"); !reflect.DeepEqual(got, wantWarnings) {
		t.Fatalf("warnings = %q, want %q", got, wantWarnings)
	}
	assertCanonicalBlocked(t, f, f.cache)
	if got := strings.TrimSpace(runGitOutput(t, f.cache, "rev-parse", "--abbrev-ref", "main@{upstream}")); got != "upstream/main" {
		t.Fatalf("main tracks %q, want upstream/main", got)
	}
	if got := strings.TrimSpace(runGitOutput(t, f.cache, "rev-parse", "refs/remotes/upstream/main")); got != f.upstreamHead {
		t.Fatalf("upstream/main = %s, want %s: the fetch should reach every declared remote", got, f.upstreamHead)
	}
	if got := strings.TrimSpace(runGitOutput(t, f.cache, "config", "--get", "remote.mine.url")); got != filepath.Join(f.base, "mine.git") {
		t.Fatalf("mine url = %q, want it left alone", got)
	}
	if got := strings.TrimSpace(runGitOutput(t, f.cache, "config", "--get", "remote.mine.pushurl")); got != "https://example.com/mine-push.git" {
		t.Fatalf("mine pushurl = %q, want it left alone", got)
	}

	before := gitConfigBytes(t, f.cache)
	stderr.Reset()
	if _, err := p.SourceFetch(ctx, "app"); err != nil {
		t.Fatalf("SourceFetch(again) error = %v", err)
	}
	if after := gitConfigBytes(t, f.cache); !bytes.Equal(before, after) {
		t.Fatalf("second SourceFetch changed the config:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("second SourceFetch warnings = %q, want none", got)
	}
}

// Bring-up only reports what differs: the cache may be shared or the user's
// own, so its config stays byte for byte as it was. A canonical remote that is
// missing is reported once, as missing, not again as pushable.
func TestMaterializeSourceBestEffortReportsRemotesWithoutWriting(t *testing.T) {
	var stderr bytes.Buffer
	ctx := warnLogContext(t, &stderr)
	f := newRemotesFixture(t)
	startFromOldStyleCache(t, f)
	p := f.writeStack(t, f.forkAndCanonicalUpstream())
	before := gitConfigBytes(t, f.cache)

	if err := p.materializeSource(ctx, "app", loadSource(t, p, "app"), true); err != nil {
		t.Fatalf("materializeSource(bestEffort) error = %v", err)
	}
	if after := gitConfigBytes(t, f.cache); !bytes.Equal(before, after) {
		t.Fatalf("best-effort refresh rewrote the cache config:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	wantWarnings := []string{
		"warning: source \"app\" cache origin is " + f.upstream + ", but angee.yaml declares " + f.fork + "; run `angee source fetch app` to switch it",
		"warning: source \"app\" cache has no upstream remote, but angee.yaml declares " + f.upstream + "; run `angee source fetch app` to switch it",
		"warning: source \"app\" cache pushes to no configured remote by default, but angee.yaml declares push_default origin; run `angee source fetch app` to switch it",
		"warning: source \"app\" cache branch main tracks origin/main, but angee.yaml takes it from upstream/main; run `angee source fetch app` to switch it",
	}
	if got := strings.Split(strings.TrimSuffix(stderr.String(), "\n"), "\n"); !reflect.DeepEqual(got, wantWarnings) {
		t.Fatalf("warnings = %q, want %q", got, wantWarnings)
	}
}

// A remote the manifest no longer marks canonical loses the operator's push
// block; a push URL the user set on another remote stays.
func TestSourceFetchUnblocksARemoteNoLongerCanonical(t *testing.T) {
	f := newRemotesFixture(t)
	runGit(t, "", "clone", "--origin", "upstream", "--branch", "main", f.upstream, f.cache)
	runGit(t, f.cache, "config", "remote.upstream.pushurl", canonicalPushURL("upstream"))
	runGit(t, f.cache, "remote", "add", "origin", f.fork)
	runGit(t, f.cache, "config", "remote.origin.pushurl", "https://example.com/push.git")
	p := f.writeStack(t, "    remotes:\n      origin: "+f.fork+"\n      upstream: "+f.upstream+"\n")

	if _, err := p.SourceFetch(t.Context(), "app"); err != nil {
		t.Fatalf("SourceFetch() error = %v", err)
	}
	if got := runGitOutputAllowFailure(t, f.cache, "config", "--get-all", "remote.upstream.pushurl"); got != "" {
		t.Fatalf("upstream pushurl = %q, want the block removed", got)
	}
	if got := strings.TrimSpace(runGitOutput(t, f.cache, "config", "--get", "remote.origin.pushurl")); got != "https://example.com/push.git" {
		t.Fatalf("origin pushurl = %q, want the user's left alone", got)
	}
}

// A clone that cannot be given its remotes is removed, so it cannot be left
// able to push to the canonical remote it came from.
func TestMaterializeSourceRemovesACloneItCannotConfigure(t *testing.T) {
	f := newRemotesFixture(t)
	p := f.writeStack(t, f.forkAndCanonicalUpstream())
	// A git that clones but refuses `git remote add`, so the reconcile fails
	// right after the clone, while the canonical remote is still pushable.
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("LookPath(git) error = %v", err)
	}
	bin := filepath.Join(f.base, "bin")
	mustMkdirAll(t, bin)
	mustWriteFile(t, filepath.Join(bin, "git"), "#!/bin/sh\nif [ \"$1\" = remote ] && [ \"$2\" = add ]; then echo 'refused by the test' >&2; exit 1; fi\nexec "+realGit+" \"$@\"\n")
	if err := os.Chmod(filepath.Join(bin, "git"), 0o755); err != nil {
		t.Fatalf("Chmod(git) error = %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	err = p.materializeSource(t.Context(), "app", loadSource(t, p, "app"), true)
	if err == nil || !strings.Contains(err.Error(), "refused by the test") {
		t.Fatalf("materializeSource() error = %v, want the refused remote add", err)
	}
	if _, err := os.Stat(f.cache); !os.IsNotExist(err) {
		t.Fatalf("Stat(cache) error = %v, want the unconfigured clone removed", err)
	}
	if left := stagingLeftovers(t, f.cache); len(left) != 0 {
		t.Fatalf("staging directories left = %v", left)
	}
}

// A workspace template's inline source carries its remotes into angee.yaml,
// and a clone slot gets them as the cache does, push block included.
func TestCloneSlotGetsTheSourceRemotes(t *testing.T) {
	ctx := context.Background()
	f := newRemotesFixture(t)
	root := filepath.Join(f.base, ".angee")
	templateRoot := filepath.Join(f.base, ".templates", "workspaces", "slot")
	mustMkdirAll(t, filepath.Join(templateRoot, "template"))
	mustWriteFile(t, filepath.Join(templateRoot, "copier.yml"), `_subdirectory: template
_templates_suffix: .jinja
_answers_file: .copier-answers.yml
_angee:
  kind: workspace
  name: slot
  sources:
    app:
      remotes:
        origin:
          url: `+f.fork+`
          branches: ["feature-*"]
        upstream:
          url: `+f.upstream+`
          canonical: true
      push_default: origin
      cache_path: .cache/app
      default_ref: main
      subpath: .
      mode: clone
      branch: feature-a
      ref: main
`)
	platform, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := platform.WorkspaceCreate(ctx, api.WorkspaceCreateRequest{Template: templateRoot, Name: "feature-a"}); err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	source := loadSource(t, platform, "app")
	wantRemotes := map[string]manifest.SourceRemote{
		"origin":   {URL: f.fork, Branches: []string{"feature-*"}},
		"upstream": {URL: f.upstream, Canonical: true},
	}
	if source.Kind != "git" || !reflect.DeepEqual(source.Remotes, wantRemotes) || source.PushDefault != "origin" {
		t.Fatalf("saved source = %#v, want kind git, remotes %#v and push_default origin", source, wantRemotes)
	}
	slot := filepath.Join(root, "workspaces", "feature-a")
	commitFile(t, slot, "work.txt")
	assertCanonicalBlocked(t, f, slot)
	if got := strings.TrimSpace(runGitOutput(t, slot, "config", "--get", "remote.origin.url")); got != f.fork {
		t.Fatalf("slot origin url = %q, want the fork %q", got, f.fork)
	}
	if got := strings.TrimSpace(runGitOutput(t, slot, "config", "--get", "remote.pushDefault")); got != "origin" {
		t.Fatalf("slot remote.pushDefault = %q, want origin", got)
	}
}

// An inline template source with invalid remotes is refused before anything
// is cloned.
func TestWorkspaceCreateRefusesInvalidTemplateRemotes(t *testing.T) {
	f := newRemotesFixture(t)
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
      push_default: upstream
      cache_path: .cache/app
      subpath: .
      mode: clone
      ref: main
`)
	platform, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	_, err = platform.WorkspaceCreate(t.Context(), api.WorkspaceCreateRequest{Template: templateRoot, Name: "feature-a"})
	// The prefix shows the refusal came from resolving the template source,
	// before any clone, not from saving angee.yaml afterwards.
	if err == nil || !strings.Contains(err.Error(), `workspace source "app": source "app": push_default "upstream" names no declared remote`) {
		t.Fatalf("WorkspaceCreate() error = %v, want the push_default refusal from resolving the template source", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".cache", "app")); !os.IsNotExist(err) {
		t.Fatalf("Stat(cache) error = %v, want no clone", err)
	}
}

func TestResolveWorkspaceTemplateSourceSubstitutesRemotes(t *testing.T) {
	spec := copierx.TemplateSource{
		Remotes: map[string]manifest.SourceRemote{
			"origin":   {URL: "git@github.com:${inputs.owner}/app.git"},
			"upstream": {URL: "https://github.com/acme/app.git", Canonical: true, Branches: []string{"${inputs.prefix}/*"}},
		},
		PushDefault: "${inputs.push}",
		CachePath:   ".cache/app",
	}
	source, err := resolveWorkspaceTemplateSource(spec, map[string]string{"owner": "me", "prefix": "agent", "push": "origin"}, "ws", nil)
	if err != nil {
		t.Fatalf("resolveWorkspaceTemplateSource() error = %v", err)
	}
	want := manifest.Source{
		Kind: "git",
		Remotes: map[string]manifest.SourceRemote{
			"origin":   {URL: "git@github.com:me/app.git"},
			"upstream": {URL: "https://github.com/acme/app.git", Canonical: true, Branches: []string{"agent/*"}},
		},
		PushDefault: "origin",
		CachePath:   ".cache/app",
	}
	if !reflect.DeepEqual(source, want) {
		t.Fatalf("resolveWorkspaceTemplateSource() = %#v, want %#v", source, want)
	}
}
