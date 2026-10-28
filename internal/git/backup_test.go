package git

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublishedBackupVerifiesLiveRemoteAndRefusesLocalOnly(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	client := New()
	dir := t.TempDir()
	remote := filepath.Join(t.TempDir(), "remote.git")
	run := func(where string, args ...string) {
		t.Helper()
		if out, err := client.Run(t.Context(), where, args...); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run(dir, "init", "-b", "main")
	run(dir, "config", "user.name", "backup-test")
	run(dir, "config", "user.email", "backup@example.invalid")
	if err := os.WriteFile(filepath.Join(dir, "tracked"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	run(dir, "add", "tracked")
	run(dir, "commit", "-m", "published")
	run(dir, "init", "--bare", remote)
	run(dir, "remote", "add", "origin", remote)
	run(dir, "push", "-u", "origin", "main")
	if remotes, err := client.PublishedBackup(t.Context(), dir, Colocated{}); err != nil || remotes["origin"] != remote {
		t.Fatalf("published=%v err=%v", remotes, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tracked"), []byte("local change"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := client.PublishedBackup(t.Context(), dir, Colocated{}); err == nil {
		t.Fatal("accepted dirty worktree")
	}
	run(dir, "add", "tracked")
	run(dir, "commit", "-m", "local-only")
	if _, err := client.PublishedBackup(t.Context(), dir, Colocated{}); err == nil {
		t.Fatal("accepted unpublished commit")
	}
	run(dir, "push", "origin", "main")
	if _, err := client.PublishedBackup(t.Context(), dir, Colocated{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tracked"), []byte("stashed local work"), 0600); err != nil {
		t.Fatal(err)
	}
	run(dir, "stash", "push", "-m", "private recovery state")
	// A refusal points to the source policy the backup API accepts.
	var refusal *BackupPublicationError
	if _, err := client.PublishedBackup(t.Context(), dir, Colocated{}); !errors.As(err, &refusal) || !strings.HasSuffix(refusal.Reason, "; use the bundle source policy") {
		t.Fatalf("private stash: err=%v, want a refusal naming the bundle source policy", err)
	}
	run(dir, "stash", "drop")
	if err := os.RemoveAll(remote); err != nil {
		t.Fatal(err)
	}
	if _, err := client.PublishedBackup(t.Context(), dir, Colocated{}); err == nil {
		t.Fatal("accepted unavailable advertised remote")
	}
}

// TestPublishedBackupDisregardsExactlyColocatedMetadata gives a published
// checkout what colocated jj keeps in it, without needing jj: an ignored
// worktree-root .jj tree and a refs/jj/ keep ref holding a local-only commit.
// The verdict passes only when told to leave exactly that to the layer, and
// still refuses anything else, and a layer revision no remote advertises.
func TestPublishedBackupDisregardsExactlyColocatedMetadata(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	client := New()
	dir := t.TempDir()
	remote := filepath.Join(t.TempDir(), "remote.git")
	run := func(where string, args ...string) string {
		t.Helper()
		out, err := client.Run(t.Context(), where, args...)
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, contents string) {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	run(dir, "init", "-b", "main")
	run(dir, "config", "user.name", "backup-test")
	run(dir, "config", "user.email", "backup@example.invalid")
	write("tracked", "published")
	run(dir, "add", "tracked")
	run(dir, "commit", "-m", "published")
	run(dir, "init", "--bare", remote)
	run(dir, "remote", "add", "origin", remote)
	run(dir, "push", "-u", "origin", "main")
	write(".jj/.gitignore", "/*\n")
	write(".jj/repo/op_store/operations/0", "jj operation")
	keep := run(dir, "commit-tree", "HEAD^{tree}", "-p", "HEAD", "-m", "hidden jj commit")
	run(dir, "update-ref", "refs/jj/keep/"+keep, keep)
	jj := Colocated{Dirs: []string{".jj"}, Refs: []string{"refs/jj/"}}
	refused := func(name string, colocated Colocated) {
		t.Helper()
		var refusal *BackupPublicationError
		if _, err := client.PublishedBackup(t.Context(), dir, colocated); !errors.As(err, &refusal) {
			t.Fatalf("%s: err=%v, want a publication refusal", name, err)
		}
	}
	refused("plain git verdict over jj metadata", Colocated{})
	refused("jj directory alone", Colocated{Dirs: jj.Dirs})
	if remotes, err := client.PublishedBackup(t.Context(), dir, jj); err != nil || remotes["origin"] != remote {
		t.Fatalf("published=%v err=%v", remotes, err)
	}
	refused("layer revision absent from the remote", Colocated{Dirs: jj.Dirs, Refs: jj.Refs, Revisions: []string{keep}})
	run(dir, "update-ref", "refs/jjx/foo", "HEAD")
	refused("ref beside the jj namespace", jj)
	run(dir, "update-ref", "-d", "refs/jjx/foo")
	write("sub/.jj/.gitignore", "/*\n")
	write("sub/.jj/nested", "not the worktree-root .jj")
	refused("nested .jj", jj)
	if err := os.RemoveAll(filepath.Join(dir, "sub")); err != nil {
		t.Fatal(err)
	}
	write(".git/info/exclude", "build/\n")
	write("build/out", "stray ignored output")
	refused("stray ignored file", jj)
	for _, invalid := range []Colocated{
		{Dirs: []string{"../outside"}}, {Dirs: []string{"."}}, {Dirs: []string{"/abs"}}, {Dirs: []string{"a/../b"}}, {Dirs: []string{"sub/.jj"}},
		{Refs: []string{"refs/heads/"}}, {Refs: []string{"refs/remotes/origin/"}}, {Refs: []string{"refs/"}}, {Refs: []string{"refs/jj"}}, {Refs: []string{"refs/jj/*"}},
	} {
		var refusal *BackupPublicationError
		if _, err := client.PublishedBackup(t.Context(), dir, invalid); err == nil || errors.As(err, &refusal) {
			t.Fatalf("metadata %+v: err=%v, want it rejected as invalid", invalid, err)
		}
	}
}
