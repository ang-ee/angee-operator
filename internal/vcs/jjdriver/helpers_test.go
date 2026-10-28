package jjdriver

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/internal/git"
	"github.com/ang-ee/angee-operator/internal/vcs"
)

// requireJJ skips the test when this machine cannot run jj slots, or fails it
// when ANGEE_REQUIRE_JJ=1 (CI's jj job), so a missing jj or capability
// cannot pass for a green run there.
func requireJJ(t *testing.T) {
	t.Helper()
	err := Probe(context.Background()).Err()
	if err == nil {
		return
	}
	if os.Getenv("ANGEE_REQUIRE_JJ") == "1" {
		t.Fatalf("ANGEE_REQUIRE_JJ=1, but %v", err)
	}
	t.Skipf("%v", err)
}

// isolate gives the test a HOME of its own. jj keeps its user config there,
// and, since 0.45, a record and the config of every repository it loads too
// (under $XDG_CONFIG_HOME/jj/repos), so neither the developer's jj or git
// config reaches the test, and nothing the test runs writes to the
// developer's. The driver passes HOME, XDG_CONFIG_HOME and JJ_CONFIG on to
// jj, so it reads the empty JJ_CONFIG too and has no identity; the test's own
// jj and git commands also get an identity, empty global git config, and no
// system git config. Call it before anything runs jj.
func isolate(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	config := filepath.Join(home, "jj-config.toml")
	gitConfig := filepath.Join(home, ".gitconfig")
	for _, path := range []string{config, gitConfig} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", path, err)
		}
	}
	for key, value := range map[string]string{
		"HOME":                home,
		"XDG_CONFIG_HOME":     filepath.Join(home, ".config"),
		"JJ_CONFIG":           config,
		"JJ_USER":             "test",
		"JJ_EMAIL":            "test@example.invalid",
		"GIT_CONFIG_GLOBAL":   gitConfig,
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME":     "test",
		"GIT_AUTHOR_EMAIL":    "test@example.invalid",
		"GIT_COMMITTER_NAME":  "test",
		"GIT_COMMITTER_EMAIL": "test@example.invalid",
	} {
		t.Setenv(key, value)
	}
}

// fixture is a primary with one jj slot, laid out as phase 6 will create
// them: a bare remote with one commit on main, a clone of it colocated with
// jj as the primary, and a secondary jj workspace colocated with git at main
// on that remote. seed is a plain git clone that moves the remote. The
// repository does not set snapshot.auto-update-stale: the driver passes it.
type fixture struct {
	t       *testing.T
	remote  string
	seed    string
	primary string
	slot    string
	name    string
	// remoteName is what the primary calls the remote (origin by default).
	remoteName string
	// seeds, when set, gives each jj command the next JJ_RANDOMNESS_SEED, so
	// change ids repeat from run to run without colliding within one.
	seeds *int
}

type fixtureOption func(*fixtureConfig)

type fixtureConfig struct {
	name          string
	remoteName    string
	deterministic bool
}

// deterministic makes jj's ids repeat from run to run: jj's own test
// settings, a seed per command, and fixed dates for jj and git.
func deterministic(c *fixtureConfig) { c.deterministic = true }

// named names the slot's workspace.
func named(name string) fixtureOption { return func(c *fixtureConfig) { c.name = name } }

// cloneRemote names the primary's remote.
func cloneRemote(name string) fixtureOption { return func(c *fixtureConfig) { c.remoteName = name } }

func newFixture(t *testing.T, options ...fixtureOption) *fixture {
	t.Helper()
	isolate(t)
	requireJJ(t)
	config := fixtureConfig{name: "stack--ws", remoteName: "origin"}
	for _, option := range options {
		option(&config)
	}
	root := t.TempDir()
	f := &fixture{
		t:          t,
		remote:     filepath.Join(root, "remote.git"),
		seed:       filepath.Join(root, "seed"),
		primary:    filepath.Join(root, "primary"),
		slot:       filepath.Join(root, "slot"),
		name:       config.name,
		remoteName: config.remoteName,
	}
	if config.deterministic {
		const when = "2026-10-05T12:00:00+00:00"
		for key, value := range map[string]string{
			"JJ_TIMESTAMP":       when,
			"JJ_OP_TIMESTAMP":    when,
			"JJ_OP_HOSTNAME":     "angee-test",
			"JJ_OP_USERNAME":     "angee-test",
			"GIT_AUTHOR_DATE":    when,
			"GIT_COMMITTER_DATE": when,
		} {
			t.Setenv(key, value)
		}
		f.seeds = new(int)
	}
	f.git(root, "init", "-q", "--bare", "-b", "main", f.remote)
	f.git(root, "clone", "-q", f.remote, f.seed)
	f.write(f.seed, "f.txt", "one\n")
	f.git(f.seed, "add", ".")
	f.git(f.seed, "commit", "-q", "-m", "base")
	f.git(f.seed, "push", "-q", "origin", "HEAD:main")
	f.git(root, "clone", "-q", "-o", f.remoteName, f.remote, f.primary)
	f.jj(f.primary, "git", "init", "--colocate")
	f.jj(f.primary, "workspace", "add", "--colocate", "--name", f.name, "-r", f.base().String(), f.slot)
	return f
}

// base is main on the primary's remote, the slot's base.
func (f *fixture) base() revset { return remoteBookmark("main", f.remoteName) }

// slotAt is the vcs.Slot for the fixture's slot on branch, based on main.
func (f *fixture) slotAt(branch string) vcs.Slot {
	return vcs.Slot{Path: f.slot, Branch: branch, BaseRef: "main", BaseRemote: f.remoteName}
}

// driver is the driver under test, asking the git on PATH for push remotes.
func driver() Driver { return New(git.New()) }

// commit records name with content as a commit in dir with message, leaving
// an empty working-copy commit on top, as an agent's `jj commit` does.
func (f *fixture) commit(dir, name, content, message string) {
	f.t.Helper()
	f.write(dir, name, content)
	f.jj(dir, "commit", "-m", message)
}

// advanceBase pushes a commit setting name to content on main from seed, and
// fetches it into the primary.
func (f *fixture) advanceBase(name, content string) {
	f.t.Helper()
	f.write(f.seed, name, content)
	f.git(f.seed, "add", ".")
	f.git(f.seed, "commit", "-q", "-m", "upstream "+name)
	f.git(f.seed, "push", "-q", "origin", "HEAD:main")
	f.jj(f.primary, "git", "fetch")
}

// shortID is jj's short commit id of r, read in dir without snapshotting.
func (f *fixture) shortID(dir string, r revset) string {
	f.t.Helper()
	return strings.TrimSpace(f.jj(dir, "--ignore-working-copy", "log", "--no-graph", "-r", r.String(), "-T", `commit_id.short() ++ "\n"`))
}

// changeID is jj's change id of r, read in dir without snapshotting.
func (f *fixture) changeID(dir string, r revset) string {
	f.t.Helper()
	return strings.TrimSpace(f.jj(dir, "--ignore-working-copy", "log", "--no-graph", "-r", r.String(), "-T", `change_id ++ "\n"`))
}

func (f *fixture) write(dir, name, content string) {
	f.t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		f.t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}

// jj runs jj in dir with the test's environment and returns its standard
// output, failing the test when it fails.
func (f *fixture) jj(dir string, args ...string) string {
	f.t.Helper()
	var env []string
	if f.seeds != nil {
		*f.seeds++
		env = append(os.Environ(), "JJ_RANDOMNESS_SEED="+strconv.Itoa(*f.seeds))
	}
	return runTool(f.t, dir, env, "jj", append([]string{"--no-pager", "--color", "never"}, args...)...)
}

// jjFails runs jj in dir as jj does, expecting it to fail, and returns its
// standard error.
func (f *fixture) jjFails(dir string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command("jj", append([]string{"--no-pager", "--color", "never"}, args...)...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil {
		f.t.Fatalf("jj %q in %s succeeded, want it to fail", args, dir)
	}
	return stderr.String()
}

func (f *fixture) git(dir string, args ...string) {
	f.t.Helper()
	runTool(f.t, dir, nil, "git", args...)
}

// runTool runs name in dir, in env, else in the test's environment.
func runTool(t *testing.T, dir string, env []string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %q in %s: %v\n%s%s", name, args, dir, err, stdout.String(), stderr.String())
	}
	return stdout.String()
}
