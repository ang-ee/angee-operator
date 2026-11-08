//go:build !windows

package restic

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRealLockKindAndUnlockStaleLock(t *testing.T) {
	client := requireRestic(t)
	repository := newRealRepository(t, client, "locks")
	if _, err := client.BackupStdin(t.Context(), BackupStdinOptions{Repository: repository, Filename: "stack/seed"}, strings.NewReader("seed")); err != nil {
		t.Fatal(err)
	}
	pruneCmd := startPausedExclusivePrune(t, client, repository)
	client.metadataTimeout, client.metadataRetryLock = 10*time.Second, time.Second
	_, metadataLockErr := client.KeyAdd(t.Context(), KeyAddOptions{Repository: repository, NewPassword: "metadata-lock-password"})
	if !IsKind(metadataLockErr, Locked) {
		t.Fatalf("locking metadata operation error = %v, want Locked", metadataLockErr)
	}
	if err := pruneCmd.Process.Signal(syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	if err := pruneCmd.Wait(); err != nil {
		t.Fatalf("exclusive prune: %v", err)
	}

	passwordRead, passwordWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdinRead, stdinWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(client.Binary, "--json", "backup", "--stdin", "--stdin-filename", "stack/slow.dump")
	cmd.Env = allowedEnvironment([]string{
		"RESTIC_REPOSITORY=" + repository.URL,
		"RESTIC_PASSWORD_FILE=/dev/fd/3",
		"RESTIC_CACHE_DIR=" + repository.CacheDir,
	})
	cmd.ExtraFiles = []*os.File{passwordRead}
	cmd.Stdin = stdinRead
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdinWrite.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	_ = passwordRead.Close()
	_ = stdinRead.Close()
	_, _ = io.WriteString(passwordWrite, repository.Password+"\n")
	_ = passwordWrite.Close()
	_, _ = io.WriteString(stdinWrite, strings.Repeat("x", 64<<10))

	status := make(chan []byte, 1)
	go func() {
		line, _ := bufio.NewReader(stdout).ReadBytes('\n')
		status <- line
	}()
	select {
	case line := <-status:
		if !bytes.Contains(line, []byte(`"message_type":"status"`)) {
			_ = cmd.Process.Kill()
			_ = stdinWrite.Close()
			_ = cmd.Wait()
			t.Fatalf("first backup event = %q, stderr %q", line, stderr.String())
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		_ = stdinWrite.Close()
		_ = cmd.Wait()
		t.Fatalf("restic emitted no status event; stderr %q", stderr.String())
	}
	if _, err := client.ForgetDryRunPolicy(t.Context(), ForgetDryRunOptions{
		Repository: repository, Policy: RetentionPolicy{KeepLast: 1},
	}); err != nil {
		t.Fatalf("no-lock ForgetDryRunPolicy under held lock: %v", err)
	}

	lockCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	_, lockErr := client.run(lockCtx, invocation{repository: &repository, args: []string{"prune"}})
	cancel()
	if !IsKind(lockErr, Locked) {
		t.Fatalf("exclusive operation error = %v, want Locked", lockErr)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = stdinWrite.Close()
	_ = cmd.Wait()
	staleLocks, err := client.run(t.Context(), invocation{repository: &repository, args: []string{"list", "locks"}, metadata: true, noLock: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(staleLocks.Stdout)) == "" {
		t.Fatal("SIGKILL left no stale lock to exercise Unlock")
	}

	if _, err := client.Unlock(t.Context(), repository); err != nil {
		t.Fatalf("Unlock(): %v", err)
	}
	locks, err := client.run(t.Context(), invocation{repository: &repository, args: []string{"--no-lock", "list", "locks"}, metadata: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(locks.Stdout)) != "" {
		t.Fatalf("locks remain after Unlock: %q", locks.Stdout)
	}
}

func startPausedExclusivePrune(t *testing.T, c *Client, repository Repository) *exec.Cmd {
	t.Helper()
	passwordRead, passwordWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(c.Binary, "prune", "--dry-run")
	cmd.Env = allowedEnvironment([]string{
		"RESTIC_REPOSITORY=" + repository.URL,
		"RESTIC_PASSWORD_FILE=/dev/fd/3",
		"RESTIC_CACHE_DIR=" + repository.CacheDir,
	})
	cmd.ExtraFiles = []*os.File{passwordRead}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGCONT)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	_ = passwordRead.Close()
	_, _ = io.WriteString(passwordWrite, repository.Password+"\n")
	_ = passwordWrite.Close()
	lineReady := make(chan []byte, 1)
	go func() {
		line, _ := bufio.NewReader(stdout).ReadBytes('\n')
		lineReady <- line
	}()
	select {
	case line := <-lineReady:
		if !bytes.Contains(line, []byte("loading all snapshots")) {
			t.Fatalf("first prune output = %q, stderr %q", line, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("prune emitted no progress; stderr %q", stderr.String())
	}
	if err := cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatalf("pause prune: %v", err)
	}
	return cmd
}
