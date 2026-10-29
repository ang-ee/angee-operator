package jjdriver

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// The fake jj and git are this test binary linked under those names on PATH.
// Run that way, it records its run in $HOME/fake-runs.jsonl, sleeps when
// $HOME/sleep exists, fails when $HOME/fail does or when one of its arguments
// is a line of $HOME/fail-on, prints the version and help text files in
// $HOME hold, and for `git worktree list` $HOME/git-worktree-list when it
// exists, and exits instead of testing.
func init() {
	tool := filepath.Base(os.Args[0])
	if tool != "jj" && tool != "git" {
		return
	}
	home := os.Getenv("HOME")
	dir, _ := os.Getwd()
	line, _ := json.Marshal(fakeRun{Tool: tool, Args: os.Args[1:], Env: os.Environ(), Dir: dir})
	if log, err := os.OpenFile(filepath.Join(home, "fake-runs.jsonl"), os.O_APPEND|os.O_WRONLY, 0); err == nil {
		_, _ = log.Write(append(line, '\n'))
		_ = log.Close()
	}
	if _, err := os.Stat(filepath.Join(home, "sleep")); err == nil {
		time.Sleep(30 * time.Second)
	}
	if _, err := os.Stat(filepath.Join(home, "fail")); err == nil || failsOn(home, os.Args[1:]) {
		_, _ = os.Stderr.WriteString("Error: fake failure for https://user:secret@example.com/repo\n")
		os.Exit(1)
	}
	reply := ""
	switch {
	case slices.Contains(os.Args, "--version"):
		reply = tool + "-version"
	case slices.Contains(os.Args, "--help"):
		reply = tool + "-help"
	case tool == "git" && slices.Contains(os.Args, "worktree") && slices.Contains(os.Args, "list"):
		// The worktrees $HOME/git-worktree-list lists, none without it.
		if _, err := os.Stat(filepath.Join(home, "git-worktree-list")); err == nil {
			reply = "git-worktree-list"
		}
	}
	if reply != "" {
		out, err := os.ReadFile(filepath.Join(home, reply))
		if err != nil {
			os.Exit(1)
		}
		_, _ = os.Stdout.Write(out)
	}
	os.Exit(0)
}

// failsOn reports whether one of args is a line of $HOME/fail-on, so a test
// can fail one command (`forget`, say) and let the others run.
func failsOn(home string, args []string) bool {
	failOn, err := os.ReadFile(filepath.Join(home, "fail-on"))
	if err != nil {
		return false
	}
	return slices.ContainsFunc(strings.Split(string(failOn), "\n"), func(line string) bool {
		return line != "" && slices.Contains(args, line)
	})
}

// fakeRun is one run of a fake tool, as it received it.
type fakeRun struct {
	Tool string
	Args []string
	Env  []string
	Dir  string
}

// fakeTools puts the fake jj (unless replies has no "jj-version") and git on
// an otherwise empty PATH, writes replies (file name to content) into a fresh
// HOME, sets every variable git.IsolatedEnv passes on to a known value plus
// one it must not pass, and returns the HOME.
func fakeTools(t *testing.T, replies map[string]string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake tools are symlinks")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable() error = %v", err)
	}
	bin, home := t.TempDir(), t.TempDir()
	tools := []string{"git"}
	if _, ok := replies["jj-version"]; ok {
		tools = append(tools, "jj")
	}
	for _, tool := range tools {
		if err := os.Symlink(self, filepath.Join(bin, tool)); err != nil {
			t.Fatalf("Symlink() error = %v", err)
		}
	}
	replies["fake-runs.jsonl"] = ""
	for name, content := range replies {
		if err := os.WriteFile(filepath.Join(home, name), []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", name, err)
		}
	}
	for key, value := range map[string]string{
		"PATH": bin, "HOME": home, "USER": "tester", "SSH_AUTH_SOCK": "/tmp/agent.sock", "LANG": "C.UTF-8",
		"GH_TOKEN": "gh-token", "XDG_CONFIG_HOME": filepath.Join(home, ".config"),
		"ANGEE_UNRELATED_SECRET": "must-not-leak",
	} {
		t.Setenv(key, value)
	}
	return home
}

// fakeRuns reads the runs the fake tools recorded in home.
func fakeRuns(t *testing.T, home string) []fakeRun {
	t.Helper()
	file, err := os.Open(filepath.Join(home, "fake-runs.jsonl"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer file.Close()
	var runs []fakeRun
	scanner := bufio.NewScanner(file)
	scanner.Buffer(nil, 1<<20)
	for scanner.Scan() {
		var run fakeRun
		if err := json.Unmarshal(scanner.Bytes(), &run); err != nil {
			t.Fatalf("decode fake run: %v", err)
		}
		runs = append(runs, run)
	}
	return runs
}
