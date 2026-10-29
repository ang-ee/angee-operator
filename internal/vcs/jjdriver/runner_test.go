package jjdriver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/internal/git"
	"github.com/ang-ee/angee-operator/internal/vcs"
)

// TestRunnerRunsJJInTheSlot pins how every jj run starts: jj from PATH,
// --no-pager --color never and snapshot.auto-update-stale (D8), `-R .` in the
// slot, --ignore-working-copy for a read after a snapshot, with the
// snapshot's operation once it is known, and git's isolated environment plus a no-op editor and
// the operator's JJ_CONFIG, JJ_USER and JJ_EMAIL, with nothing else from the
// operator's environment and no identity of angee's: jj commits as the
// operator's JJ_USER and JJ_EMAIL when set, else as their jj config says.
func TestRunnerRunsJJInTheSlot(t *testing.T) {
	home := fakeTools(t, map[string]string{"jj-version": "jj 0.0.0\n"})
	t.Setenv("JJ_CONFIG", "/tmp/jj-config.toml")
	t.Setenv("JJ_USER", "operator")
	t.Setenv("JJ_EMAIL", "operator@example.invalid")
	slot := t.TempDir()
	ctx := context.Background()
	if _, err := (runner{}).snapshot(ctx, slot, "log", "-r", "@"); err != nil {
		t.Fatalf("snapshot() error = %v", err)
	}
	if _, err := (runner{}).latest(ctx, slot, "op", "log"); err != nil {
		t.Fatalf("latest() error = %v", err)
	}
	if _, err := (runner{}).read(ctx, slot, "0123abcd", "log", "-r", "@"); err != nil {
		t.Fatalf("read() error = %v", err)
	}
	runs := fakeRuns(t, home)
	if len(runs) != 3 {
		t.Fatalf("fake jj ran %d times, want 3", len(runs))
	}
	global := []string{"--no-pager", "--color", "never", "--config", "snapshot.auto-update-stale=true"}
	wantArgs := [][]string{
		append(slices.Clone(global), "-R", ".", "log", "-r", "@"),
		append(slices.Clone(global), "-R", ".", "--ignore-working-copy", "op", "log"),
		append(slices.Clone(global), "-R", ".", "--ignore-working-copy", "--at-op", "0123abcd", "log", "-r", "@"),
	}
	realSlot, err := filepath.EvalSymlinks(slot)
	if err != nil {
		t.Fatalf("EvalSymlinks() error = %v", err)
	}
	for i, run := range runs {
		if run.Tool != "jj" || !slices.Equal(run.Args, wantArgs[i]) {
			t.Errorf("run %d = %s %q, want jj %q", i, run.Tool, run.Args, wantArgs[i])
		}
		if dir, _ := filepath.EvalSymlinks(run.Dir); dir != realSlot {
			t.Errorf("run %d ran in %s, want the slot %s", i, run.Dir, realSlot)
		}
		for _, entry := range []string{
			"JJ_EDITOR=true", "JJ_CONFIG=/tmp/jj-config.toml", "JJ_USER=operator", "JJ_EMAIL=operator@example.invalid",
			"GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=ssh -o BatchMode=yes",
			"HOME=" + home, "GH_TOKEN=gh-token", "SSH_AUTH_SOCK=/tmp/agent.sock",
		} {
			if !slices.Contains(run.Env, entry) {
				t.Errorf("run %d environment lacks %s", i, entry)
			}
		}
		for _, entry := range run.Env {
			if entry == "ANGEE_UNRELATED_SECRET=must-not-leak" || strings.Contains(entry, "angee@example.invalid") {
				t.Errorf("run %d environment carries %s", i, entry)
			}
		}
	}
}

// TestRunnerRefusesAnEmptySlot never runs jj in the process's working
// directory: a slot command with no slot, a read without its snapshot's
// operation, and a command with no directory are refused before jj runs.
func TestRunnerRefusesAnEmptySlot(t *testing.T) {
	home := fakeTools(t, map[string]string{"jj-version": "jj 0.0.0\n"})
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"snapshot":      func() error { _, err := (runner{}).snapshot(ctx, "", "log"); return err },
		"latest":        func() error { _, err := (runner{}).latest(ctx, "", "op", "log"); return err },
		"read":          func() error { _, err := (runner{}).read(ctx, "", "0123abcd", "log"); return err },
		"read, no op":   func() error { _, err := (runner{}).read(ctx, t.TempDir(), "", "log"); return err },
		"run":           func() error { _, err := (runner{}).run(ctx, "", []string{"--version"}); return err },
		"Status":        func() error { _, err := New(git.New()).Status(ctx, vcs.Slot{BaseRef: "main"}); return err },
		"Diff":          func() error { _, err := New(git.New()).Diff(ctx, vcs.Slot{}, ""); return err },
		"network fetch": func() error { _, err := (runner{}).network(ctx, "jj git fetch", "", "git", "fetch"); return err },
	} {
		if err := call(); err == nil {
			t.Errorf("%s with no slot: error = nil, want a refusal", name)
		}
	}
	if runs := fakeRuns(t, home); len(runs) != 0 {
		t.Fatalf("jj ran %d times, want none: %v", len(runs), runs)
	}
}

// TestRunnerFailure returns jj's standard error, redacted, in a
// *CommandError.
func TestRunnerFailure(t *testing.T) {
	fakeTools(t, map[string]string{"jj-version": "jj 0.0.0\n", "fail": ""})
	_, err := (runner{}).snapshot(context.Background(), t.TempDir(), "git", "fetch")
	var command *CommandError
	if !errors.As(err, &command) {
		t.Fatalf("snapshot() error = %v, want a *CommandError", err)
	}
	if want := "Error: fake failure for https://***@example.com/repo"; command.Stderr != want {
		t.Fatalf("CommandError.Stderr = %q, want %q", command.Stderr, want)
	}
}

// TestRunnerNetworkTimeout bounds a network command by ANGEE_GIT_TIMEOUT, as
// every git network operation is.
func TestRunnerNetworkTimeout(t *testing.T) {
	home := fakeTools(t, map[string]string{"jj-version": "jj 0.0.0\n"})
	if err := os.WriteFile(filepath.Join(home, "sleep"), nil, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	t.Setenv("ANGEE_GIT_TIMEOUT", "200ms")
	_, err := (runner{}).network(context.Background(), "jj git fetch", t.TempDir(), "git", "fetch")
	if !git.IsTimeout(err) {
		t.Fatalf("network() error = %v, want a *git.TimeoutError", err)
	}
}
