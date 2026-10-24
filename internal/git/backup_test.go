package git

import (
	"os"
	"os/exec"
	"path/filepath"
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
	if remotes, err := client.PublishedBackup(t.Context(), dir); err != nil || remotes["origin"] != remote {
		t.Fatalf("published=%v err=%v", remotes, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tracked"), []byte("local change"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := client.PublishedBackup(t.Context(), dir); err == nil {
		t.Fatal("accepted dirty worktree")
	}
	run(dir, "add", "tracked")
	run(dir, "commit", "-m", "local-only")
	if _, err := client.PublishedBackup(t.Context(), dir); err == nil {
		t.Fatal("accepted unpublished commit")
	}
	run(dir, "push", "origin", "main")
	if _, err := client.PublishedBackup(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tracked"), []byte("stashed local work"), 0600); err != nil {
		t.Fatal(err)
	}
	run(dir, "stash", "push", "-m", "private recovery state")
	if _, err := client.PublishedBackup(t.Context(), dir); err == nil {
		t.Fatal("accepted private stash")
	}
	run(dir, "stash", "drop")
	if err := os.RemoveAll(remote); err != nil {
		t.Fatal(err)
	}
	if _, err := client.PublishedBackup(t.Context(), dir); err == nil {
		t.Fatal("accepted unavailable advertised remote")
	}
}
