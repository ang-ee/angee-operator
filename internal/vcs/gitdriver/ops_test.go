package gitdriver

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/internal/git"
	"github.com/ang-ee/angee-operator/internal/vcs"
)

// fakeGitLog is the file, in its HOME, where the fake git records each run.
const fakeGitLog = "fake-git.jsonl"

// fakeGitRun is one run of the fake git: its arguments and its whole
// environment, as it received them.
type fakeGitRun struct {
	Args []string
	Env  []string
}

// The fake git is this test binary linked as "git" on PATH. Run that way with
// a HOME holding fakeGitLog, it records its run there instead of testing, and
// fails every command but ls-files when HOME also holds "fail".
func init() {
	if filepath.Base(os.Args[0]) != "git" {
		return
	}
	home := os.Getenv("HOME")
	log, err := os.OpenFile(filepath.Join(home, fakeGitLog), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return
	}
	line, _ := json.Marshal(fakeGitRun{Args: os.Args[1:], Env: os.Environ()})
	_, _ = log.Write(append(line, '\n'))
	_ = log.Close()
	if _, err := os.Stat(filepath.Join(home, "fail")); err == nil && !slices.Contains(os.Args, "ls-files") {
		os.Exit(1)
	}
	os.Exit(0)
}

// fakeGit puts the fake git first on an otherwise empty PATH, sets every
// variable git.Client.RunIsolated passes on to a known value (GIT_SSH_COMMAND
// only when sshCommand is not empty) plus one it must not pass, and returns
// the HOME the runs are recorded in.
func fakeGit(t *testing.T, sshCommand string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable() error = %v", err)
	}
	bin, home := t.TempDir(), t.TempDir()
	if err := os.Symlink(self, filepath.Join(bin, "git")); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, fakeGitLog), nil, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	for key, value := range map[string]string{
		"PATH": bin, "HOME": home, "USER": "tester", "SSH_AUTH_SOCK": "/tmp/agent.sock", "SSH_AGENT_PID": "42",
		"LANG": "C.UTF-8", "LC_ALL": "C.UTF-8", "GH_TOKEN": "gh-token", "GITHUB_TOKEN": "github-token",
		"GH_HOST": "github.example.com", "GH_CONFIG_DIR": "/tmp/gh-config", "XDG_CONFIG_HOME": "/tmp/xdg-config",
		"GIT_ASKPASS": "/usr/local/bin/askpass", "ANGEE_UNRELATED_SECRET": "must-not-leak", "GIT_SSH_COMMAND": sshCommand,
	} {
		t.Setenv(key, value)
	}
	if sshCommand == "" {
		if err := os.Unsetenv("GIT_SSH_COMMAND"); err != nil {
			t.Fatalf("Unsetenv(GIT_SSH_COMMAND) error = %v", err)
		}
	}
	return home
}

// fakeGitRuns returns the runs recorded in home, and clears them.
func fakeGitRuns(t *testing.T, home string) []fakeGitRun {
	t.Helper()
	path := filepath.Join(home, fakeGitLog)
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open(%s) error = %v", path, err)
	}
	defer func() { _ = file.Close() }()
	var runs []fakeGitRun
	scanner := bufio.NewScanner(file)
	scanner.Buffer(nil, 1<<20)
	for scanner.Scan() {
		var run fakeGitRun
		if err := json.Unmarshal(scanner.Bytes(), &run); err != nil {
			t.Fatalf("fake git record %q: %v", scanner.Text(), err)
		}
		runs = append(runs, run)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
	return runs
}

// The operations that may stop on conflicts run git with exactly the
// arguments and environment they had before they moved to git.Client: the
// environment holds only what git needs to reach ssh and credential helpers,
// prompts disabled, and angee's identity; GIT_SSH_COMMAND is kept when set,
// else batch-mode ssh. Syncing a base runs as the user, with their whole
// environment.
func TestOperationsRunGitWithTheirArgumentsAndEnvironment(t *testing.T) {
	ctx := context.Background()
	for _, ssh := range []string{"ssh-wrapper --flag", ""} {
		t.Run("GIT_SSH_COMMAND="+ssh, func(t *testing.T) {
			home := fakeGit(t, ssh)
			bin := os.Getenv("PATH")
			want := []string{
				"PATH=" + bin, "HOME=" + home, "USER=tester", "SSH_AUTH_SOCK=/tmp/agent.sock", "SSH_AGENT_PID=42",
			}
			if ssh != "" {
				want = append(want, "GIT_SSH_COMMAND="+ssh)
			}
			want = append(want,
				"LANG=C.UTF-8", "LC_ALL=C.UTF-8", "GH_TOKEN=gh-token", "GITHUB_TOKEN=github-token", "GH_HOST=github.example.com",
				"GH_CONFIG_DIR=/tmp/gh-config", "XDG_CONFIG_HOME=/tmp/xdg-config", "GIT_ASKPASS=/usr/local/bin/askpass",
				"GIT_TERMINAL_PROMPT=0",
				"GIT_AUTHOR_NAME=angee", "GIT_AUTHOR_EMAIL=angee@example.invalid",
				"GIT_COMMITTER_NAME=angee", "GIT_COMMITTER_EMAIL=angee@example.invalid",
			)
			if ssh == "" {
				want = append(want, "GIT_SSH_COMMAND=ssh -o BatchMode=yes")
			}

			for _, client := range []git.Client{git.New(), {Bin: "git", NonInteractive: true}} {
				d := New(client)
				s := vcs.Slot{Path: t.TempDir()}
				for _, tc := range []struct {
					run  func() error
					args []string
				}{
					{func() error { _, err := d.Merge(ctx, s, "main"); return err }, []string{"merge", "--no-ff", "--no-edit", "--end-of-options", "main"}},
					{func() error { _, err := d.Rebase(ctx, s, "main"); return err }, []string{"rebase", "--end-of-options", "main"}},
					{func() error { _, err := d.MergeAbort(ctx, s); return err }, []string{"merge", "--abort"}},
					{func() error { _, err := d.RebaseAbort(ctx, s); return err }, []string{"rebase", "--abort"}},
					{func() error { _, err := d.RebaseContinue(ctx, s); return err }, []string{"-c", "core.editor=true", "rebase", "--continue"}},
					{func() error {
						_, err := d.Publish(ctx, s, vcs.PublishPlan{Remote: "fork", Branch: "feature"})
						return err
					}, []string{"push", "--set-upstream", "--end-of-options", "fork", "refs/heads/feature:refs/heads/feature"}},
				} {
					if err := tc.run(); err != nil {
						t.Fatalf("%v error = %v", tc.args, err)
					}
					runs := fakeGitRuns(t, home)
					if len(runs) != 1 || !slices.Equal(runs[0].Args, tc.args) || !slices.Equal(runs[0].Env, want) {
						t.Fatalf("%v ran git %#v,\nwant args %q and env %q", tc.args, runs, tc.args, want)
					}
				}

				// A failed operation lists the conflicts with the same
				// environment.
				if err := os.WriteFile(filepath.Join(home, "fail"), nil, 0o644); err != nil {
					t.Fatalf("WriteFile(fail) error = %v", err)
				}
				if _, err := d.Merge(ctx, s, "main"); err == nil {
					t.Fatal("Merge() of a failing git error = nil")
				}
				runs := fakeGitRuns(t, home)
				if len(runs) != 2 || !slices.Equal(runs[1].Args, []string{"-c", "core.quotepath=false", "ls-files", "-u"}) || !slices.Equal(runs[1].Env, want) {
					t.Fatalf("failed Merge() ran git %#v, want ls-files -u with the operation's environment", runs)
				}

				// Syncing runs git.Client's merge, as the user.
				if _, err := d.SyncBase(ctx, s, "origin/main", vcs.SyncMerge); err == nil {
					t.Fatal("SyncBase() of a failing git error = nil")
				}
				runs = fakeGitRuns(t, home)
				if len(runs) != 2 || !slices.Equal(runs[0].Args, []string{"merge", "--no-edit", "--end-of-options", "origin/main"}) ||
					!slices.Contains(runs[0].Env, "ANGEE_UNRELATED_SECRET=must-not-leak") || slices.ContainsFunc(runs[0].Env, func(e string) bool { return strings.HasPrefix(e, "GIT_COMMITTER_NAME=") }) {
					t.Fatalf("SyncBase() ran git %#v, want git.Client's merge with the user's environment", runs)
				}
				if err := os.Remove(filepath.Join(home, "fail")); err != nil {
					t.Fatalf("Remove(fail) error = %v", err)
				}
			}
		})
	}
}

// git ls-files -u lists one line per stage of each conflicted path; each
// path is reported once, sorted, with its name as git printed it.
func TestParseConflictedPaths(t *testing.T) {
	out := "100644 1111111111111111111111111111111111111111 1\tb.txt\n" +
		"100644 2222222222222222222222222222222222222222 2\tb.txt\n" +
		"100644 3333333333333333333333333333333333333333 3\tb.txt\n" +
		"100644 4444444444444444444444444444444444444444 2\trésumé with space.md\n" +
		"not a stage line\n\n"
	if got, want := parseConflictedPaths(out), []string{"b.txt", "résumé with space.md"}; !slices.Equal(got, want) {
		t.Fatalf("parseConflictedPaths() = %q, want %q", got, want)
	}
	if got := parseConflictedPaths(" \n"); got == nil || len(got) != 0 {
		t.Fatalf("parseConflictedPaths(blank) = %#v, want an empty, non-nil list", got)
	}
}

// A fetch failure is named by the remote the slot's base comes from, else by
// origin, else by the checkout's only remote; with several and neither, by
// none.
func TestFetchURL(t *testing.T) {
	ctx := context.Background()
	dir, remote := clone(t)
	upstream := "https://example.com/upstream.git"
	run(t, dir, "remote", "add", "upstream", upstream)
	d := New(git.New())
	for _, tc := range []struct {
		name, baseRemote, prepare, want string
	}{
		{name: "the base remote", baseRemote: "upstream", want: upstream},
		{name: "origin without a base remote", want: remote},
		{name: "origin for a base remote that is not configured", baseRemote: "gone", want: remote},
		{name: "the base remote without origin", baseRemote: "upstream", prepare: "rename", want: upstream},
		{name: "none of several without origin", want: ""},
		{name: "the only remote", prepare: "remove", want: remote},
	} {
		t.Run(tc.name, func(t *testing.T) {
			switch tc.prepare {
			case "rename":
				run(t, dir, "remote", "rename", "origin", "fork")
			case "remove":
				run(t, dir, "remote", "remove", "upstream")
			}
			if got := d.FetchURL(ctx, vcs.Slot{Path: dir, BaseRemote: tc.baseRemote}); got != tc.want {
				t.Fatalf("FetchURL(BaseRemote %q) = %q, want %q", tc.baseRemote, got, tc.want)
			}
		})
	}
}
