package service

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/runtime"
	"github.com/ang-ee/angee-operator/internal/runtime/proccompose"
)

// sshFixture is a stack whose git sources are reached over ssh, at
// git@example.test:acme/<name>.git, through a stand-in for ssh on PATH: it
// records the arguments of each run and serves the repository the remote
// command names from a directory of bare repositories, so a clone, fetch or
// push of a source really goes through GIT_SSH_COMMAND.
type sshFixture struct {
	t          *testing.T
	base, root string
	stack      *manifest.Stack
	platform   *Platform
}

func newSSHFixture(t *testing.T) *sshFixture {
	t.Helper()
	if goruntime.GOOS == "windows" {
		t.Skip("the stand-in for ssh is a shell script")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	base := t.TempDir()
	f := &sshFixture{t: t, base: base, root: filepath.Join(base, "stack")}
	bin := filepath.Join(base, "bin")
	mustMkdirAll(t, bin)
	mustMkdirAll(t, f.root)
	// The script finds its log and its repositories beside its directory, not
	// through the environment: jj passes git only what the operator gave it.
	script := `#!/bin/sh
here=$(dirname "$0")/..
for arg in "$@"; do printf '%s\t' "$arg"; done >> "$here/ssh.log"
printf '\n' >> "$here/ssh.log"
for last in "$@"; do :; done
command=${last%% *}
path=$(printf '%s' "${last#* }" | tr -d "'")
exec git "${command#git-}" "$here/remotes/$path"
`
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(ssh) error = %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	// The operator's own ssh command is the default one.
	t.Setenv("GIT_SSH_COMMAND", "")
	if err := os.Unsetenv("GIT_SSH_COMMAND"); err != nil {
		t.Fatal(err)
	}
	f.stack = &manifest.Stack{Version: manifest.VersionCurrent, Kind: manifest.KindStack, Name: "dev", Sources: map[string]manifest.Source{}, Secrets: map[string]manifest.Secret{}}
	platform, err := New(f.root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	f.platform = platform
	return f
}

// source declares git source name, served at git@example.test:acme/<name>.git
// and holding main; with secret, it names that secret as its SSH key.
func (f *sshFixture) source(name, secret string) {
	f.t.Helper()
	remote, seed := filepath.Join(f.base, "remotes", "acme", name+".git"), filepath.Join(f.base, "seeds", name)
	mustMkdirAll(f.t, filepath.Dir(remote))
	mustMkdirAll(f.t, filepath.Dir(seed))
	runGit(f.t, f.base, "init", "-q", "--bare", "-b", "main", remote)
	runGit(f.t, f.base, "clone", "-q", remote, seed)
	runGit(f.t, seed, "config", "user.email", "test@example.invalid")
	runGit(f.t, seed, "config", "user.name", "test")
	mustWriteFile(f.t, filepath.Join(seed, name+".txt"), name+"\n")
	runGit(f.t, seed, "add", ".")
	runGit(f.t, seed, "commit", "-q", "-m", "base")
	runGit(f.t, seed, "push", "-q", "origin", "HEAD:main")
	f.stack.Sources[name] = manifest.Source{Kind: "git", Repo: "git@example.test:acme/" + name + ".git", DefaultRef: "main", Auth: manifest.SourceAuth{SSHKeySecret: secret}}
}

func (f *sshFixture) save() {
	f.t.Helper()
	if err := manifest.SaveFile(manifest.Path(f.root), f.stack); err != nil {
		f.t.Fatalf("SaveFile() error = %v", err)
	}
}

// setSecret sets a stack secret as `angee secret set` does.
func (f *sshFixture) setSecret(name, value string) {
	f.t.Helper()
	if _, err := f.platform.SecretSet(context.Background(), name, value); err != nil {
		f.t.Fatalf("SecretSet(%s) error = %v", name, err)
	}
}

// runs returns the arguments of each run of the stand-in ssh since the last
// call, and forgets them.
func (f *sshFixture) runs() [][]string {
	f.t.Helper()
	log := filepath.Join(f.base, "ssh.log")
	data, err := os.ReadFile(log)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		f.t.Fatalf("ReadFile(ssh.log) error = %v", err)
	}
	if err := os.Remove(log); err != nil {
		f.t.Fatal(err)
	}
	var runs [][]string
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		runs = append(runs, strings.Split(strings.TrimSuffix(line, "\t"), "\t"))
	}
	return runs
}

// keyed is the arguments ssh gets before the host for keyed source name.
func (f *sshFixture) keyed(name string) []string {
	return []string{
		"-F", "/dev/null",
		"-o", "IdentityFile=" + filepath.Join(f.root, "run", "ssh", name),
		"-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes",
		"-o", "UserKnownHostsFile=" + filepath.Join(f.root, "run", "ssh", "known_hosts") + " ~/.ssh/known_hosts",
		"-o", "StrictHostKeyChecking=accept-new",
	}
}

// assertRuns checks that ssh ran once per given source, in that order, each
// with the arguments its source's key gives, or, for a source in unkeyed,
// with the operator's default ssh command and no key.
func (f *sshFixture) assertRuns(what string, unkeyed []string, sources ...string) {
	f.t.Helper()
	runs := f.runs()
	if len(runs) != len(sources) {
		f.t.Fatalf("%s: ssh ran %d times (%v), want once for each of %v", what, len(runs), runs, sources)
	}
	for i, name := range sources {
		run := runs[i]
		if serves := run[len(run)-1]; !strings.HasSuffix(serves, " 'acme/"+name+".git'") {
			f.t.Fatalf("%s: ssh run %d served %q, want source %s", what, i, serves, name)
		}
		if !slices.Contains(run, "git@example.test") {
			f.t.Fatalf("%s: ssh run %d = %v, want the remote's host", what, i, run)
		}
		want := f.keyed(name)
		if slices.Contains(unkeyed, name) {
			want = []string{"-o", "BatchMode=yes"}
			if slices.Contains(run, "-i") || slices.Contains(run, "-F") || slices.Contains(run, "IdentitiesOnly=yes") || slices.ContainsFunc(run, func(arg string) bool { return strings.HasPrefix(arg, "IdentityFile=") }) {
				f.t.Fatalf("%s: ssh run %d for unkeyed source %s = %v, want no key pinned", what, i, name, run)
			}
		}
		if len(run) < len(want) || !slices.Equal(run[:len(want)], want) {
			f.t.Fatalf("%s: ssh run %d for source %s =\n%v\nwant it to start with\n%v", what, i, name, run, want)
		}
	}
}

// Two private repositories on one host each have a deploy key of their own:
// every clone, fetch, pull and push of a source runs ssh with that source's
// key file and no other, and a source that names no key keeps the operator's
// own ssh command. The key file is the secret, with a final newline, readable
// by its owner only, and is rewritten only when the secret changes.
func TestKeyedSourcesEachUseTheirOwnKey(t *testing.T) {
	ctx := context.Background()
	f := newSSHFixture(t)
	f.source("a", "deploy-key-a")
	f.source("b", "deploy-key-b")
	f.source("open", "")
	f.stack.Secrets["deploy-key-a"] = manifest.Secret{Required: true}
	f.save()
	// As `angee secret set --stdin` stores a key: without its last newline.
	f.setSecret("deploy-key-a", "-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----")
	f.setSecret("deploy-key-b", "-----BEGIN OPENSSH PRIVATE KEY-----\nBBBB\n-----END OPENSSH PRIVATE KEY-----\n")

	stack, err := f.platform.LoadStack()
	if err != nil {
		t.Fatalf("LoadStack() error = %v", err)
	}
	// What the first `angee up` does with the stack's sources.
	if err := f.platform.materializeReferencedSources(ctx, stack); err != nil {
		t.Fatalf("materializeReferencedSources() error = %v", err)
	}
	f.assertRuns("first clone", []string{"open"}, "a", "b", "open")
	for _, name := range []string{"a", "b", "open"} {
		if _, err := os.Stat(filepath.Join(f.root, "sources", name, name+".txt")); err != nil {
			t.Fatalf("source %s was not cloned: %v", name, err)
		}
	}

	sshDir := filepath.Join(f.root, "run", "ssh")
	if info, err := os.Stat(sshDir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("%s mode = %v, %v; want 0700", sshDir, info.Mode(), err)
	}
	for name, want := range map[string]string{
		"a": "-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----\n",
		"b": "-----BEGIN OPENSSH PRIVATE KEY-----\nBBBB\n-----END OPENSSH PRIVATE KEY-----\n",
	} {
		file := filepath.Join(sshDir, name)
		info, err := os.Lstat(file)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Fatalf("key file %s mode = %v, %v; want a regular file of mode 0600", file, info, err)
		}
		if got, err := os.ReadFile(file); err != nil || string(got) != want {
			t.Fatalf("key file %s = %q, %v; want the secret with one final newline", file, got, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(sshDir, "open")); !os.IsNotExist(err) {
		t.Fatalf("a key file was written for a source that names no key: %v", err)
	}
	entries, err := os.ReadDir(sshDir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("%s holds %v, %v; want the two key files alone", sshDir, entries, err)
	}

	// A refresh on the next bring-up, and the explicit source verbs.
	before, err := os.Lstat(filepath.Join(sshDir, "a"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.platform.materializeReferencedSources(ctx, stack); err != nil {
		t.Fatalf("materializeReferencedSources() again error = %v", err)
	}
	f.assertRuns("refresh", []string{"open"}, "a", "b", "open")
	if _, err := f.platform.SourceFetch(ctx, "b"); err != nil {
		t.Fatalf("SourceFetch(b) error = %v", err)
	}
	f.assertRuns("source fetch", nil, "b")
	if _, err := f.platform.SourcePull(ctx, "a"); err != nil {
		t.Fatalf("SourcePull(a) error = %v", err)
	}
	// A pull fetches the cache, then pulls it.
	f.assertRuns("source pull", nil, "a", "a")
	if _, err := f.platform.SourcePush(ctx, "a", "main"); err != nil {
		t.Fatalf("SourcePush(a) error = %v", err)
	}
	f.assertRuns("source push", nil, "a")
	if _, err := f.platform.SourceFetch(ctx, "open"); err != nil {
		t.Fatalf("SourceFetch(open) error = %v", err)
	}
	f.assertRuns("unkeyed source fetch", []string{"open"}, "open")
	// Reads run no ssh.
	if _, err := f.platform.SourceStatus(ctx, "a"); err != nil {
		t.Fatalf("SourceStatus(a) error = %v", err)
	}
	if runs := f.runs(); len(runs) != 0 {
		t.Fatalf("a status ran ssh: %v", runs)
	}
	after, err := os.Lstat(filepath.Join(sshDir, "a"))
	if err != nil || !os.SameFile(before, after) || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("key file a was rewritten though its secret did not change (%v)", err)
	}

	// A key set again is the one the next fetch uses.
	f.setSecret("deploy-key-a", "-----BEGIN OPENSSH PRIVATE KEY-----\nROTATED\n-----END OPENSSH PRIVATE KEY-----")
	if _, err := f.platform.SourceFetch(ctx, "a"); err != nil {
		t.Fatalf("SourceFetch(a) after a new key error = %v", err)
	}
	f.assertRuns("fetch with a new key", nil, "a")
	if got, err := os.ReadFile(filepath.Join(sshDir, "a")); err != nil || !strings.Contains(string(got), "ROTATED\n") || !strings.HasSuffix(string(got), "KEY-----\n") {
		t.Fatalf("key file a after a new key = %q, %v", got, err)
	}
	if info, err := os.Lstat(filepath.Join(sshDir, "a")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file a after a new key has mode %v, %v", info.Mode(), err)
	}
	// A key file opened up by hand is closed again.
	if err := os.Chmod(filepath.Join(sshDir, "a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sshDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.platform.ensureSourceKey(ctx, stack, "a"); err != nil {
		t.Fatalf("ensureSourceKey() error = %v", err)
	}
	if info, err := os.Lstat(filepath.Join(sshDir, "a")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file a mode = %v, %v; want 0600 again", info.Mode(), err)
	}
	if info, err := os.Stat(sshDir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("%s mode = %v, %v; want 0700 again", sshDir, info.Mode(), err)
	}
}

// A keyed source whose key is not set is refused before git runs: nothing is
// cloned, for that source or for the ones before it, and the refusal names
// the source and the secret.
func TestKeyedSourceWithoutItsKeyFailsBeforeAnyClone(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		secret manifest.Secret
		set    string
	}{
		{name: "an undeclared secret"},
		{name: "a required secret", secret: manifest.Secret{Required: true}},
		{name: "a blank secret", set: " \n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSSHFixture(t)
			f.source("a-open", "")
			f.source("keyed", "deploy-key-keyed")
			if tc.secret != (manifest.Secret{}) {
				f.stack.Secrets["deploy-key-keyed"] = tc.secret
			}
			f.save()
			if tc.set != "" {
				f.setSecret("deploy-key-keyed", tc.set)
			}
			stack, err := f.platform.LoadStack()
			if err != nil {
				t.Fatalf("LoadStack() error = %v", err)
			}
			const want = `source "keyed" authenticates with the SSH key in secret "deploy-key-keyed", which is not set. Set it to the private key with ` + "`angee secret set deploy-key-keyed --stdin < <key file>`."
			check := func(what string, err error) {
				t.Helper()
				opErr := AsOperationError(err)
				if opErr == nil || opErr.Code != CodePreconditionFailed || opErr.Cause != CauseSourceKeyMissing || opErr.Source != "keyed" || !strings.HasSuffix(err.Error(), want) {
					t.Fatalf("%s error = %v, want a missing key of source keyed (%s)", what, err, want)
				}
				if runs := f.runs(); len(runs) != 0 {
					t.Fatalf("%s ran ssh before it refused: %v", what, runs)
				}
				if entries, _ := os.ReadDir(filepath.Join(f.root, "sources")); len(entries) != 0 {
					t.Fatalf("%s cloned %v before it refused", what, entries)
				}
				if _, err := os.Lstat(filepath.Join(f.root, "run", "ssh", "keyed")); !os.IsNotExist(err) {
					t.Fatalf("%s wrote a key file: %v", what, err)
				}
			}
			check("materializeReferencedSources()", f.platform.materializeReferencedSources(ctx, stack))
			_, err = f.platform.StackPrepare(ctx)
			check("StackPrepare()", err)
			_, err = f.platform.SourceFetch(ctx, "keyed")
			check("SourceFetch()", err)
			_, err = f.platform.SourcePull(ctx, "keyed")
			check("SourcePull()", err)
		})
	}
}

// A bring-up leaves in run/ssh only the key files of the stack's keyed
// sources: the key of a source that is gone, or that names no key any more,
// is removed, and so is a staging file a crash left long ago. The
// known-hosts file stays.
func TestBringUpRemovesKeyFilesNoSourceHas(t *testing.T) {
	ctx := context.Background()
	f := newSSHFixture(t)
	f.source("a", "deploy-key-a")
	f.source("b", "deploy-key-b")
	f.save()
	f.setSecret("deploy-key-a", "KEY A")
	f.setSecret("deploy-key-b", "KEY B")
	stack, err := f.platform.LoadStack()
	if err != nil {
		t.Fatalf("LoadStack() error = %v", err)
	}
	if err := f.platform.materializeReferencedSources(ctx, stack); err != nil {
		t.Fatalf("materializeReferencedSources() error = %v", err)
	}
	sshDir := filepath.Join(f.root, "run", "ssh")
	old := filepath.Join(sshDir, ".b.new-123")
	for path, content := range map[string]string{
		filepath.Join(sshDir, "known_hosts"):     "example.test ssh-ed25519 AAAA\n",
		filepath.Join(sshDir, "known_hosts.old"): "\n",
		filepath.Join(sshDir, "removed-source"):  "OLD KEY\n",
		filepath.Join(sshDir, ".a.new-456"):      "being written\n",
		old:                                      "left by a crash\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	longAgo := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(old, longAgo, longAgo); err != nil {
		t.Fatal(err)
	}
	// Source b names no key any more.
	b := stack.Sources["b"]
	b.Auth = manifest.SourceAuth{}
	stack.Sources["b"] = b
	if err := f.platform.materializeReferencedSources(ctx, stack); err != nil {
		t.Fatalf("materializeReferencedSources() error = %v", err)
	}
	entries, err := os.ReadDir(sshDir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if want := []string{".a.new-456", "a", "known_hosts", "known_hosts.old"}; !slices.Equal(names, want) {
		t.Fatalf("%s holds %v, want %v", sshDir, names, want)
	}
}

// A key the stack imports from the environment is taken from it, as any
// imported secret is.
func TestKeyedSourceTakesAnImportedKey(t *testing.T) {
	ctx := context.Background()
	f := newSSHFixture(t)
	f.source("a", "deploy-key-a")
	f.stack.Secrets["deploy-key-a"] = manifest.Secret{Required: true, Import: "env:DEPLOY_KEY_A"}
	f.save()
	t.Setenv("DEPLOY_KEY_A", "IMPORTED KEY")
	if _, err := f.platform.SourceFetch(ctx, "a"); err != nil {
		t.Fatalf("SourceFetch() error = %v", err)
	}
	f.assertRuns("clone with an imported key", nil, "a")
	if got, err := os.ReadFile(filepath.Join(f.root, "run", "ssh", "a")); err != nil || string(got) != "IMPORTED KEY\n" {
		t.Fatalf("key file = %q, %v; want the imported key", got, err)
	}
}

// The slots of a workspace reach their source's remote as its cache does:
// cutting a worktree or a clone of a keyed source, and fetching, pushing and
// syncing the slot, run ssh with the source's key.
func TestWorkspaceSlotsUseTheirSourcesKey(t *testing.T) {
	ctx := context.Background()
	f := newSSHFixture(t)
	f.source("a", "deploy-key-a")
	f.source("b", "deploy-key-b")
	f.source("open", "")
	f.save()
	f.setSecret("deploy-key-a", "KEY A")
	f.setSecret("deploy-key-b", "KEY B")
	template := f.template("slots", `    a:
      mode: worktree
      branch: "work/${name | slug}"
    b:
      mode: clone
      ref: main
    open:
      mode: worktree
      branch: "work/${name | slug}"
`)
	created, err := f.platform.WorkspaceCreate(ctx, api.WorkspaceCreateRequest{Template: template, Name: "w"})
	if err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	// A worktree slot clones its source's cache; a clone slot clones the
	// remote itself.
	f.assertRuns("create", []string{"open"}, "a", "b", "open")
	for _, name := range []string{"a", "b"} {
		if got, err := os.ReadFile(filepath.Join(f.root, "run", "ssh", name)); err != nil || string(got) != "KEY "+strings.ToUpper(name)+"\n" {
			t.Fatalf("key file %s = %q, %v", name, got, err)
		}
	}

	for _, slot := range []string{"a", "b"} {
		if _, err := f.platform.WorkspaceSourceFetch(ctx, "w", slot); err != nil {
			t.Fatalf("WorkspaceSourceFetch(%s) error = %v", slot, err)
		}
		f.assertRuns("slot fetch "+slot, nil, slot)
	}
	if _, err := f.platform.WorkspaceSourceFetch(ctx, "w", "open"); err != nil {
		t.Fatalf("WorkspaceSourceFetch(open) error = %v", err)
	}
	f.assertRuns("unkeyed slot fetch", []string{"open"}, "open")

	// A commit in the worktree slot, published and pushed.
	checkout := filepath.Join(created.Path, "a")
	runGit(t, checkout, "config", "user.email", "test@example.invalid")
	runGit(t, checkout, "config", "user.name", "test")
	commitFile(t, checkout, "more.txt")
	if _, err := f.platform.WorkspaceSourcePublish(ctx, "w", "a", "", ""); err != nil {
		t.Fatalf("WorkspaceSourcePublish() error = %v", err)
	}
	f.assertRuns("publish", nil, "a")
	commitFile(t, checkout, "again.txt")
	if _, err := f.platform.WorkspaceSourcePush(ctx, "w", "a", ""); err != nil {
		t.Fatalf("WorkspaceSourcePush() error = %v", err)
	}
	f.assertRuns("slot push", nil, "a")
	if heads := remoteHeads(t, filepath.Join(f.base, "remotes", "acme", "a.git")); !slices.Contains(heads, "work/w") {
		t.Fatalf("remote heads = %v, want the published branch", heads)
	}
	if _, err := f.platform.WorkspaceSyncBase(ctx, "w", ""); err != nil {
		t.Fatalf("WorkspaceSyncBase() error = %v", err)
	}
	// sync-base fetches each worktree slot.
	f.assertRuns("sync-base", []string{"open"}, "a", "open")

	// A status of the workspace runs no ssh, and needs no key.
	if err := os.RemoveAll(filepath.Join(f.root, "run", "ssh")); err != nil {
		t.Fatal(err)
	}
	status, err := f.platform.WorkspaceStatus(ctx, "w")
	if err != nil || len(status.Sources) != 3 {
		t.Fatalf("WorkspaceStatus() = %+v, %v", status.Sources, err)
	}
	if runs := f.runs(); len(runs) != 0 {
		t.Fatalf("a status ran ssh: %v", runs)
	}
	// The next verb that reaches the remote writes the key again.
	if _, err := f.platform.WorkspaceSourceFetch(ctx, "w", "a"); err != nil {
		t.Fatalf("WorkspaceSourceFetch() after the keys were removed error = %v", err)
	}
	f.assertRuns("slot fetch after the keys were removed", nil, "a")
}

// A workspace is not cut from a keyed source that has to be cloned and whose
// key is not set: no slot is left. A slot verb that reaches the remote
// refuses the same way; one that does not still runs, and so does the
// cutting of a slot from a cache that is there.
func TestWorkspaceSlotOfAKeyedSourceWithoutItsKey(t *testing.T) {
	ctx := context.Background()
	f := newSSHFixture(t)
	f.source("a", "deploy-key-a")
	f.source("open", "")
	f.save()
	template := f.template("slots", `    open:
      mode: worktree
      branch: "work/${name | slug}"
    a:
      mode: worktree
      branch: "work/${name | slug}"
`)
	missing := func(what string, err error) {
		t.Helper()
		opErr := AsOperationError(err)
		if opErr == nil || opErr.Cause != CauseSourceKeyMissing || opErr.Source != "a" || !strings.Contains(err.Error(), `secret "deploy-key-a", which is not set`) {
			t.Fatalf("%s error = %v, want the missing key of source a", what, err)
		}
		if runs := f.runs(); len(runs) != 0 {
			t.Fatalf("%s ran ssh before it refused: %v", what, runs)
		}
	}
	_, err := f.platform.WorkspaceCreate(ctx, api.WorkspaceCreateRequest{Template: template, Name: "w"})
	missing("WorkspaceCreate()", err)
	if _, err := os.Stat(filepath.Join(f.root, "workspaces", "w")); !os.IsNotExist(err) {
		t.Fatalf("workspace directory after a refused create: %v", err)
	}

	f.setSecret("deploy-key-a", "KEY A")
	if _, err := f.platform.WorkspaceCreate(ctx, api.WorkspaceCreateRequest{Template: template, Name: "w"}); err != nil {
		t.Fatalf("WorkspaceCreate() with the key error = %v", err)
	}
	f.runs()
	if err := f.platform.SecretDelete(ctx, "deploy-key-a"); err != nil {
		t.Fatalf("SecretDelete() error = %v", err)
	}
	_, err = f.platform.WorkspaceSourceFetch(ctx, "w", "a")
	missing("WorkspaceSourceFetch()", err)
	// The key file the deleted secret left is gone with it.
	if _, err := os.Lstat(filepath.Join(f.root, "run", "ssh", "a")); !os.IsNotExist(err) {
		t.Fatalf("key file of a deleted secret: %v, want it removed", err)
	}
	_, err = f.platform.WorkspaceSourcePull(ctx, "w", "a")
	missing("WorkspaceSourcePull()", err)
	_, err = f.platform.WorkspaceSyncBase(ctx, "w", "")
	missing("WorkspaceSyncBase()", err)
	_, err = f.platform.WorkspacePush(ctx, "w", "")
	missing("WorkspacePush()", err)
	if _, err := f.platform.WorkspaceSourceDiff(ctx, "w", "a", ""); err != nil {
		t.Fatalf("WorkspaceSourceDiff() error = %v, want a read to need no key", err)
	}
	if _, err := f.platform.WorkspaceSourceFetch(ctx, "w", "open"); err != nil {
		t.Fatalf("WorkspaceSourceFetch(open) error = %v, want an unkeyed slot unaffected", err)
	}
	f.runs()
	// A new workspace is still cut, from the cache the source has: its
	// refresh is skipped with a warning, as one that fails is.
	var stderr bytes.Buffer
	second, err := f.platform.WorkspaceCreate(warnLogContext(t, &stderr), api.WorkspaceCreateRequest{Template: template, Name: "w2"})
	if err != nil {
		t.Fatalf("WorkspaceCreate() from an existing cache without the key error = %v", err)
	}
	f.assertRuns("create from an existing cache", []string{"open"}, "open")
	if !strings.Contains(stderr.String(), `could not refresh source "a"; using the existing cache`) {
		t.Fatalf("warnings = %q, want the skipped refresh of a", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(second.Path, "a", "a.txt")); err != nil {
		t.Fatalf("slot a of the second workspace was not cut from the cache: %v", err)
	}
	if err := f.platform.WorkspaceDestroy(ctx, "w2", false); err != nil {
		t.Fatalf("WorkspaceDestroy(w2) error = %v", err)
	}
	if err := f.platform.WorkspaceDestroy(ctx, "w", false); err != nil {
		t.Fatalf("WorkspaceDestroy() error = %v, want a destroy to need no key", err)
	}
}

// A jj slot reaches its source's remote through the git that jj runs, which
// gets the source's ssh command from jj's environment.
func TestJJSlotUsesItsSourcesKey(t *testing.T) {
	ctx := context.Background()
	isolateJJ(t)
	requireJJSlotsHere(t)
	f := newSSHFixture(t)
	f.source("a", "deploy-key-a")
	f.source("open", "")
	f.save()
	f.setSecret("deploy-key-a", "KEY A")
	template := f.template("jj", `    a:
      mode: jj
      branch: "work/${name | slug}"
    open:
      mode: jj
      branch: "work/${name | slug}"
`)
	created, err := f.platform.WorkspaceCreate(ctx, api.WorkspaceCreateRequest{Template: template, Name: "w"})
	if err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	f.assertRuns("create", []string{"open"}, "a", "open")
	if _, err := f.platform.WorkspaceSourceFetch(ctx, "w", "a"); err != nil {
		t.Fatalf("WorkspaceSourceFetch(a) error = %v", err)
	}
	f.assertRuns("jj slot fetch", nil, "a")
	if _, err := f.platform.WorkspaceSourceFetch(ctx, "w", "open"); err != nil {
		t.Fatalf("WorkspaceSourceFetch(open) error = %v", err)
	}
	f.assertRuns("unkeyed jj slot fetch", []string{"open"}, "open")

	// A commit in the jj slot, published through `jj git push`.
	checkout := filepath.Join(created.Path, "a")
	mustWriteFile(t, filepath.Join(checkout, "more.txt"), "more\n")
	if out, err := exec.Command("jj", "-R", checkout, "commit", "-m", "more").CombinedOutput(); err != nil {
		t.Fatalf("jj commit: %v: %s", err, out)
	}
	if _, err := f.platform.WorkspaceSourcePublish(ctx, "w", "a", "", ""); err != nil {
		t.Fatalf("WorkspaceSourcePublish() error = %v", err)
	}
	if runs := f.runs(); len(runs) == 0 {
		t.Fatal("a publish of a jj slot ran no ssh")
	} else {
		for i, run := range runs {
			if want := f.keyed("a"); len(run) < len(want) || !slices.Equal(run[:len(want)], want) {
				t.Fatalf("publish: ssh run %d =\n%v\nwant it to start with\n%v", i, run, want)
			}
		}
	}
	if heads := remoteHeads(t, filepath.Join(f.base, "remotes", "acme", "a.git")); !slices.Contains(heads, "work/w") {
		t.Fatalf("remote heads = %v, want the published branch", heads)
	}
	if _, err := f.platform.WorkspaceSyncBase(ctx, "w", ""); err != nil {
		t.Fatalf("WorkspaceSyncBase() error = %v", err)
	}
	f.assertRuns("sync-base", []string{"open"}, "a", "open")
}

// template writes workspace template name whose `_angee.sources` are the
// given lines, and returns its path.
func (f *sshFixture) template(name, sources string) string {
	f.t.Helper()
	root := filepath.Join(f.base, ".templates", "workspaces", name)
	mustMkdirAll(f.t, filepath.Join(root, "template"))
	mustWriteFile(f.t, filepath.Join(root, "copier.yml"), "_subdirectory: template\n_templates_suffix: .jinja\n_answers_file: .copier-answers.yml\n_angee:\n  kind: workspace\n  name: "+name+"\n  sources:\n"+sources)
	mustWriteFile(f.t, filepath.Join(root, "template", "README.md.jinja"), "workspace {{ workspace_name }}\n")
	return root
}

// A keyed source cannot take the name of the files its key file sits beside.
func TestKeyedSourceNames(t *testing.T) {
	for _, name := range []string{"known_hosts", "Known_Hosts", "known_hosts.old", ".", ".."} {
		err := manifest.ValidateSource(name, manifest.Source{Kind: "git", Repo: "git@example.test:acme/a.git", Auth: manifest.SourceAuth{SSHKeySecret: "key"}})
		if err == nil || !strings.Contains(err.Error(), "a source with auth.ssh_key_secret has its key file named after it") {
			t.Fatalf("ValidateSource(%q) error = %v", name, err)
		}
	}
	p, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := manifest.Source{Kind: "git", Repo: "git@example.test:acme/a.git", Auth: manifest.SourceAuth{SSHKeySecret: "key"}}
	want := "ssh -F /dev/null -o IdentityFile=" + filepath.Join(p.Root(), "run", "ssh", "angee-arp") + " -o IdentitiesOnly=yes -o BatchMode=yes -o 'UserKnownHostsFile=" + filepath.Join(p.Root(), "run", "ssh", "known_hosts") + " ~/.ssh/known_hosts' -o StrictHostKeyChecking=accept-new"
	if got := p.sourceSSHCommand("angee-arp", source); got != want {
		t.Fatalf("sourceSSHCommand() =\n%s\nwant\n%s", got, want)
	}
	// A name that is no plain file name is escaped.
	if got := p.sourceKeyFile("team/app"); got != filepath.Join(p.Root(), "run", "ssh", "team%2Fapp") {
		t.Fatalf("sourceKeyFile(team/app) = %s", got)
	}
	if got := p.sourceSSHCommand("angee-arp", manifest.Source{Kind: "git", Repo: "git@example.test:acme/a.git"}); got != "" {
		t.Fatalf("sourceSSHCommand(unkeyed) = %q, want none", got)
	}
}

// A stack on OpenBao has its resolved secrets written to run/secrets.env for
// the runtimes. A value that holds line breaks, a certificate say, stays one
// entry there and reads back whole; a one-line value is written as it always
// was.
func TestRuntimeSecretsEnvKeepsAMultiLineValueOneEntry(t *testing.T) {
	const pem = "-----BEGIN CERTIFICATE-----\nb3BlbnNzaC1rZXk=\n-----END CERTIFICATE-----\n"
	content := runtimeSecretsEnv(map[string]string{"tls-cert": pem, "db-password": `p"a ss=w0rd`})
	want := "ANGEE_SECRET_DB_PASSWORD=p\"a ss=w0rd\n" +
		`ANGEE_SECRET_TLS_CERT="-----BEGIN CERTIFICATE-----\nb3BlbnNzaC1rZXk=\n-----END CERTIFICATE-----\n"` + "\n"
	if string(content) != want {
		t.Fatalf("runtimeSecretsEnv() =\n%s\nwant\n%s", content, want)
	}
	path := filepath.Join(t.TempDir(), "secrets.env")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := runtime.ReadEnvFile(path)
	if err != nil || !slices.Equal(env, []string{`ANGEE_SECRET_DB_PASSWORD=p"a ss=w0rd`, "ANGEE_SECRET_TLS_CERT=" + pem}) {
		t.Fatalf("ReadEnvFile() = %q, %v; want both values whole", env, err)
	}
}

// A source's key is the operator's alone. It is not among the secrets the
// stack's runtime is made with, whatever its declaration says, so it is
// neither compiled into a service nor written to run/secrets.env; and the
// env file the runtimes export to what they run is a copy of the env-file
// backend's without it, the other secrets as they are.
func TestRuntimesAreNotGivenSourceKeys(t *testing.T) {
	ctx := context.Background()
	f := newSSHFixture(t)
	f.source("a", "deploy-key-a")
	f.stack.Secrets["deploy-key-a"] = manifest.Secret{Required: true}
	f.stack.Secrets["db-password"] = manifest.Secret{Required: true}
	f.stack.Services = map[string]manifest.Service{
		"web": {Runtime: manifest.RuntimeLocal, Command: []string{"sleep", "60"}, Env: map[string]string{"DB_PASSWORD": "${secret.db-password}"}},
	}
	f.save()
	const key = "-----BEGIN OPENSSH PRIVATE KEY-----\nKEYBODY\n-----END OPENSSH PRIVATE KEY-----"
	f.setSecret("deploy-key-a", key)
	f.setSecret("db-password", `hunter 2"`)
	stack, err := f.platform.LoadStack()
	if err != nil {
		t.Fatalf("LoadStack() error = %v", err)
	}

	stored := filepath.Join(f.root, ".env")
	if data, err := os.ReadFile(stored); err != nil || !strings.Contains(string(data), "KEYBODY") {
		t.Fatalf("the env-file backend's file = %q, %v; want it to hold the key, which it stores", data, err)
	}
	envFile := f.platform.runtimeEnvFile(stack)
	if want := filepath.Join(f.root, "run", "runtime.env"); envFile != want {
		t.Fatalf("runtimeEnvFile() = %s, want the copy %s", envFile, want)
	}
	if info, err := os.Lstat(envFile); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("%s mode = %v, %v; want 0600", envFile, info, err)
	}
	// The backend's file is the `.env` process-compose would load from the
	// stack root itself, so the copy also tells it not to.
	env, err := runtime.ReadEnvFile(envFile)
	if err != nil || !slices.Equal(env, []string{`ANGEE_SECRET_DB_PASSWORD=hunter 2"`, "PC_DISABLE_DOTENV=1"}) {
		t.Fatalf("what the runtimes export = %q, %v; want the other secret, and no dotenv loading", env, err)
	}
	if data, _ := os.ReadFile(envFile); strings.Contains(string(data), "KEYBODY") || strings.Contains(string(data), "DEPLOY_KEY") || !strings.HasPrefix(string(data), "# Written by angee") {
		t.Fatalf("%s holds the key, or does not say what it is:\n%s", envFile, data)
	}
	// A secret set since is in the copy the next time the runtimes are given it.
	f.setSecret("db-password", "changed")
	if env, err := runtime.ReadEnvFile(f.platform.runtimeEnvFile(stack)); err != nil || !slices.Equal(env, []string{"ANGEE_SECRET_DB_PASSWORD=changed", "PC_DISABLE_DOTENV=1"}) {
		t.Fatalf("what the runtimes export after a secret changed = %q, %v", env, err)
	}

	compiled, resolved, err := f.platform.compileStackArtifacts(ctx, stack)
	if err != nil {
		t.Fatalf("compileStackArtifacts() error = %v", err)
	}
	if _, has := resolved["deploy-key-a"]; has || resolved["db-password"] != "changed" || len(resolved) != 1 {
		t.Fatalf("the runtime's secrets = %v, want db-password alone", sortedKeys(resolved))
	}
	if _, has := compiled.SecretEnvVars["deploy-key-a"]; has {
		t.Fatalf("the compiled stack names the key's variable: %v", compiled.SecretEnvVars)
	}
	if content := runtimeSecretsEnv(resolved); strings.Contains(string(content), "DEPLOY_KEY") {
		t.Fatalf("run/secrets.env would hold the key:\n%s", content)
	}
	// A key that is not set does not stop the runtime being compiled: it is
	// asked for where the source's remote is reached.
	if err := f.platform.SecretDelete(ctx, "deploy-key-a"); err != nil {
		t.Fatalf("SecretDelete() error = %v", err)
	}
	if _, _, err := f.platform.compileStackArtifacts(ctx, stack); err != nil {
		t.Fatalf("compileStackArtifacts() without the key error = %v", err)
	}
	// A service cannot be given the key.
	web := stack.Services["web"]
	web.Env = map[string]string{"KEY": "${secret.deploy-key-a}"}
	stack.Services["web"] = web
	if _, _, err := f.platform.compileStackArtifacts(ctx, stack); err == nil || !strings.Contains(err.Error(), `secret "deploy-key-a" is not resolved`) {
		t.Fatalf("compileStackArtifacts() with a service that refers to the key error = %v", err)
	}

	// A stack whose sources name no key reads the env file itself, and a
	// bring-up removes the copy an earlier manifest left.
	a := stack.Sources["a"]
	a.Auth = manifest.SourceAuth{}
	stack.Sources["a"] = a
	if got := f.platform.runtimeEnvFile(stack); got != stored {
		t.Fatalf("runtimeEnvFile() without keyed sources = %s, want %s", got, stored)
	}
	if err := f.platform.writeRuntimeEnv(stack, nil); err != nil {
		t.Fatalf("writeRuntimeEnv() error = %v", err)
	}
	if _, err := os.Lstat(envFile); !os.IsNotExist(err) {
		t.Fatalf("%s after a bring-up without keyed sources: %v, want it removed", envFile, err)
	}
	// A file of that name that is not the copy is not angee's to remove.
	mustWriteFile(t, envFile, "MINE=1\n")
	if err := f.platform.writeRuntimeEnv(stack, nil); err != nil {
		t.Fatalf("writeRuntimeEnv() error = %v", err)
	}
	if data, err := os.ReadFile(envFile); err != nil || string(data) != "MINE=1\n" {
		t.Fatalf("%s = %q, %v; want a file angee did not write left alone", envFile, data, err)
	}
}

// With a real process-compose: a local service runs with the stack's other
// secrets in its environment, as before, and without the key of a source.
func TestLocalServiceEnvironmentHasNoSourceKey(t *testing.T) {
	if testing.Short() {
		t.Skip("native supervisor integration")
	}
	if _, err := exec.LookPath("process-compose"); err != nil {
		t.Skip("process-compose unavailable")
	}
	backend := proccompose.NewBackend()
	f := newSSHFixture(t)
	t.Setenv("PC_LOG_FILE", filepath.Join(f.base, "supervisor.log"))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	dump, script := filepath.Join(f.base, "environment"), filepath.Join(f.base, "dump.sh")
	mustWriteFile(t, script, "env > \"$1.part\" && mv \"$1.part\" \"$1\"\nexec sleep 60\n")
	f.source("a", "deploy-key-a")
	f.stack.Ports = map[string]manifest.Port{"process_compose": {Value: port}}
	f.stack.Secrets["deploy-key-a"] = manifest.Secret{Required: true}
	f.stack.Secrets["db-password"] = manifest.Secret{Required: true}
	f.stack.Services = map[string]manifest.Service{
		"web": {Runtime: manifest.RuntimeLocal, Command: []string{"sh", script, dump}, StopGracePeriod: "2s"},
	}
	f.save()
	f.setSecret("deploy-key-a", "-----BEGIN OPENSSH PRIVATE KEY-----\nKEYBODY\n-----END OPENSSH PRIVATE KEY-----")
	f.setSecret("db-password", "hunter2")
	p, err := NewWithBackends(f.root, stubStatusBackend{}, backend)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = p.StackDown(ctx)
	})
	// The detached bring-up `angee dev` makes: local services included.
	if err := p.StackDev(t.Context(), false); err != nil {
		t.Fatalf("StackDev() error = %v", err)
	}
	// The source was cloned with its key on the way up.
	f.assertRuns("bring-up", nil, "a")
	var environment []byte
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if environment, err = os.ReadFile(dump); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the local service did not write its environment: %v", err)
		}
	}
	if !strings.Contains(string(environment), "ANGEE_SECRET_DB_PASSWORD=hunter2\n") {
		t.Fatalf("the local service's environment lacks the stack's other secret:\n%s", environment)
	}
	if strings.Contains(string(environment), "DEPLOY_KEY") || strings.Contains(string(environment), "KEYBODY") || strings.Contains(string(environment), "PRIVATE KEY") {
		t.Fatalf("the local service's environment holds the source's key:\n%s", environment)
	}
}

// A stack comes up again from the caches it has: a source whose key is gone
// keeps its cache, unrefreshed, with a warning, whether or not its secret is
// declared required, while the other sources are refreshed. What needs the
// source's remote still says the key is not set.
func TestBringUpKeepsTheCacheOfASourceWhoseKeyIsGone(t *testing.T) {
	for _, declared := range []bool{false, true} {
		name := "an undeclared secret"
		if declared {
			name = "a required secret"
		}
		t.Run(name, func(t *testing.T) {
			f := newSSHFixture(t)
			f.source("a", "deploy-key-a")
			f.source("open", "")
			if declared {
				f.stack.Secrets["deploy-key-a"] = manifest.Secret{Required: true}
			}
			f.save()
			f.setSecret("deploy-key-a", "KEY A")
			if _, err := f.platform.StackPrepare(t.Context()); err != nil {
				t.Fatalf("StackPrepare() error = %v", err)
			}
			f.assertRuns("first bring-up", []string{"open"}, "a", "open")
			head := strings.TrimSpace(runGitOutput(t, filepath.Join(f.root, "sources", "a"), "rev-parse", "HEAD"))

			if err := f.platform.SecretDelete(t.Context(), "deploy-key-a"); err != nil {
				t.Fatalf("SecretDelete() error = %v", err)
			}
			var stderr bytes.Buffer
			if _, err := f.platform.StackPrepare(warnLogContext(t, &stderr)); err != nil {
				t.Fatalf("StackPrepare() without the key error = %v, want the stack prepared from the caches it has", err)
			}
			f.assertRuns("bring-up without the key", []string{"open"}, "open")
			want := `could not refresh source "a"; using the existing cache: source "a" authenticates with the SSH key in secret "deploy-key-a", which is not set.`
			if !strings.Contains(stderr.String(), want) {
				t.Fatalf("warnings = %q, want %q", stderr.String(), want)
			}
			if got := strings.TrimSpace(runGitOutput(t, filepath.Join(f.root, "sources", "a"), "rev-parse", "HEAD")); got != head {
				t.Fatalf("cache of a is at %s, want it left at %s", got, head)
			}
			if _, err := os.Lstat(filepath.Join(f.root, "run", "ssh", "a")); !os.IsNotExist(err) {
				t.Fatalf("key file of a deleted secret: %v, want it removed", err)
			}

			_, err := f.platform.SourceFetch(t.Context(), "a")
			if opErr := AsOperationError(err); opErr == nil || opErr.Cause != CauseSourceKeyMissing {
				t.Fatalf("SourceFetch() without the key error = %v, want the missing key", err)
			}
			if runs := f.runs(); len(runs) != 0 {
				t.Fatalf("a refused fetch ran ssh: %v", runs)
			}
		})
	}
}

// A source that is both marked for a slot and keyed, as a repository given
// to `angee init` by its ssh URL is rendered: the first bring-up clones its
// cache and cuts its slot in the declared workspace, with its key throughout.
func TestFirstBringUpCutsTheSlotOfAMarkedKeyedSource(t *testing.T) {
	f := newSSHFixture(t)
	f.source("lib", "deploy-key-lib")
	f.source("open", "")
	for _, name := range []string{"lib", "open"} {
		source := f.stack.Sources[name]
		source.Slot = true
		f.stack.Sources[name] = source
	}
	f.stack.Secrets["deploy-key-lib"] = manifest.Secret{Required: true}
	template := filepath.Join(f.base, ".templates", "workspaces", "src")
	mustMkdirAll(t, filepath.Join(template, "template"))
	mustWriteFile(t, filepath.Join(template, "copier.yml"), "_subdirectory: template\n_templates_suffix: .jinja\n_answers_file: .copier-answers.yml\n_angee:\n  kind: workspace\n  name: src\n  source_slot:\n    mode: worktree\n    branch: \"dev/${name | slug}\"\n")
	mustWriteFile(t, filepath.Join(template, "template", "README.md.jinja"), "workspace {{ workspace_name }}\n")
	f.stack.Workspaces = map[string]manifest.Workspace{"src": {Template: template}}
	f.save()
	f.setSecret("deploy-key-lib", "KEY LIB")

	if _, err := f.platform.StackPrepare(t.Context()); err != nil {
		t.Fatalf("StackPrepare() error = %v", err)
	}
	// Each cache is cloned, then refreshed when its slot is cut.
	f.assertRuns("first bring-up", []string{"open"}, "lib", "open", "lib", "open")
	stack, err := f.platform.LoadStack()
	if err != nil {
		t.Fatalf("LoadStack() error = %v", err)
	}
	slots := stack.Workspaces["src"].Sources
	for _, name := range []string{"lib", "open"} {
		if want := (manifest.WorkspaceSource{Source: name, Mode: "worktree", Branch: "dev/src", Subpath: name}); slots[name] != want {
			t.Fatalf("recorded slot %s = %+v, want %+v", name, slots[name], want)
		}
		if _, err := os.Stat(filepath.Join(f.root, "workspaces", "src", name, name+".txt")); err != nil {
			t.Fatalf("slot %s is not checked out: %v", name, err)
		}
	}
	if len(slots) != 2 {
		t.Fatalf("recorded slots = %v, want the two marked sources'", sortedKeys(slots))
	}
	if _, err := f.platform.WorkspaceSourceFetch(t.Context(), "src", "lib"); err != nil {
		t.Fatalf("WorkspaceSourceFetch() error = %v", err)
	}
	f.assertRuns("slot fetch", nil, "lib")
}

// A missing slot of a keyed source is repaired with the source's key: a
// worktree slot from its cache, refreshed, and a clone slot from the remote.
// Without the key a worktree slot is still cut from the cache there is, and a
// clone slot, which needs the remote, is not.
func TestRepairCutsTheSlotsOfAKeyedSource(t *testing.T) {
	ctx := context.Background()
	f := newSSHFixture(t)
	f.source("a", "deploy-key-a")
	f.source("b", "deploy-key-b")
	f.save()
	f.setSecret("deploy-key-a", "KEY A")
	f.setSecret("deploy-key-b", "KEY B")
	template := f.template("slots", `    a:
      mode: worktree
      branch: "work/${name | slug}"
    b:
      mode: clone
      ref: main
`)
	created, err := f.platform.WorkspaceCreate(ctx, api.WorkspaceCreateRequest{Template: template, Name: "w"})
	if err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	f.runs()
	remove := func() {
		t.Helper()
		runGit(t, filepath.Join(f.root, "sources", "a"), "worktree", "remove", "--force", filepath.Join(created.Path, "a"))
		for _, slot := range []string{"a", "b"} {
			if err := os.RemoveAll(filepath.Join(created.Path, slot)); err != nil {
				t.Fatal(err)
			}
		}
	}
	actions := func(result api.WorkspaceRepairResult) map[string]string {
		out := map[string]string{}
		for _, slot := range result.Slots {
			out[slot.Slot] = slot.Action + ": " + slot.Reason
		}
		return out
	}

	remove()
	result, err := f.platform.WorkspaceRepair(ctx, "w")
	if err != nil || !result.OK {
		t.Fatalf("WorkspaceRepair() = %v, %v; want both slots cut", actions(result), err)
	}
	f.assertRuns("repair", nil, "a", "b")
	for _, slot := range []string{"a", "b"} {
		if _, err := os.Stat(filepath.Join(created.Path, slot, slot+".txt")); err != nil {
			t.Fatalf("repaired slot %s is not checked out: %v", slot, err)
		}
	}

	remove()
	for _, secret := range []string{"deploy-key-a", "deploy-key-b"} {
		if err := f.platform.SecretDelete(ctx, secret); err != nil {
			t.Fatalf("SecretDelete() error = %v", err)
		}
	}
	var stderr bytes.Buffer
	result, err = f.platform.WorkspaceRepair(warnLogContext(t, &stderr), "w")
	got := actions(result)
	if err == nil || result.OK || !strings.HasPrefix(got["a"], api.WorkspaceRepairCreated) ||
		!strings.HasPrefix(got["b"], api.WorkspaceRepairFailed) || !strings.Contains(got["b"], `secret "deploy-key-b", which is not set`) {
		t.Fatalf("WorkspaceRepair() without the keys = %v, %v; want a cut from its cache and b refused for its key", got, err)
	}
	if runs := f.runs(); len(runs) != 0 {
		t.Fatalf("a repair without the keys ran ssh: %v", runs)
	}
	if !strings.Contains(stderr.String(), `could not refresh source "a"; using the existing cache`) {
		t.Fatalf("warnings = %q, want the skipped refresh of a", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(created.Path, "a", "a.txt")); err != nil {
		t.Fatalf("slot a was not cut from its cache: %v", err)
	}
	if _, err := os.Stat(filepath.Join(created.Path, "b")); !os.IsNotExist(err) {
		t.Fatalf("slot b after a refused repair: %v, want nothing left", err)
	}
}
