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
// a HOME holding fakeGitLog, it records its run there instead of testing. It
// fails every command but ls-files and var when HOME also holds "fail".
// `var GIT_<ROLE>_IDENT` prints fakeIdent, or fails as git does for a role
// with no identity when HOME holds "no-<ROLE>", and for one whose configured
// name is invalid when it holds "invalid-<ROLE>".
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
	if i := slices.Index(os.Args, "var"); i >= 0 && i+1 < len(os.Args) {
		role := strings.TrimSuffix(strings.TrimPrefix(os.Args[i+1], "GIT_"), "_IDENT")
		if _, err := os.Stat(filepath.Join(home, "no-"+role)); err == nil {
			_, _ = os.Stderr.WriteString("fatal: no email was given and auto-detection is disabled\n")
			os.Exit(128)
		}
		if _, err := os.Stat(filepath.Join(home, "invalid-"+role)); err == nil {
			_, _ = os.Stderr.WriteString("fatal: name consists only of disallowed characters: <>\n")
			os.Exit(128)
		}
		_, _ = os.Stdout.WriteString(fakeIdent + " 1793222148 -0400\n")
		os.Exit(0)
	}
	if _, err := os.Stat(filepath.Join(home, "fail")); err == nil && !slices.Contains(os.Args, "ls-files") {
		os.Exit(1)
	}
	os.Exit(0)
}

// identityProbes are the runs commitIdentity makes before a merge or rebase.
var identityProbes = [][]string{
	{"-c", "user.useConfigOnly=true", "var", "GIT_AUTHOR_IDENT"},
	{"-c", "user.useConfigOnly=true", "var", "GIT_COMMITTER_IDENT"},
}

// fakeIdent is the identity the fake git resolves for every role, and
// fakeIdentEnv what a merge or rebase then commits with.
const fakeIdent = "Fake User <fake@example.com>"

var fakeIdentEnv = []string{
	"GIT_AUTHOR_NAME=Fake User", "GIT_AUTHOR_EMAIL=fake@example.com",
	"GIT_COMMITTER_NAME=Fake User", "GIT_COMMITTER_EMAIL=fake@example.com",
}

// sameEnv reports whether two environments hold the same entries, in any
// order, as exec dedups a repeated key into the later one's place.
func sameEnv(a, b []string) bool {
	return slices.Equal(slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b)))
}

// probeEnv is the environment the identity probes run in, given the one an
// operation without them gets: the same, in the C locale.
func probeEnv(env []string) []string {
	env = slices.DeleteFunc(slices.Clone(env), func(e string) bool { return strings.HasPrefix(e, "LC_ALL=") })
	return append(env, "LC_ALL=C")
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
	unsetIdentityEnv(t)
	return home
}

// unsetIdentityEnv clears git.IdentityEnvKeys, EMAIL and GIT_CONFIG_COUNT for the
// test, so commitIdentity sees no identity from the environment the tests run
// in.
func unsetIdentityEnv(t *testing.T) {
	t.Helper()
	for _, key := range append(slices.Clone(git.IdentityEnvKeys), "EMAIL", "GIT_CONFIG_COUNT") {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("Unsetenv(%s) error = %v", key, err)
		}
	}
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
// with prompts disabled; GIT_SSH_COMMAND is kept when set, else batch-mode
// ssh. A merge or rebase first asks git for each role's identity, in the C
// locale, and commits as exactly what git resolved. Syncing a base runs as the
// user, with their whole environment.
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
			)
			if ssh == "" {
				want = append(want, "GIT_SSH_COMMAND=ssh -o BatchMode=yes")
			}

			for _, client := range []git.Client{git.New(), {Bin: "git", NonInteractive: true}} {
				d := New(client)
				s := vcs.Slot{Path: t.TempDir()}
				for _, tc := range []struct {
					run     func() error
					args    []string
					commits bool
				}{
					{func() error { _, err := d.Merge(ctx, s, "main"); return err }, []string{"merge", "--no-ff", "--no-edit", "--end-of-options", "main"}, true},
					{func() error { _, err := d.Rebase(ctx, s, "main"); return err }, []string{"rebase", "--end-of-options", "main"}, true},
					{func() error { _, err := d.MergeAbort(ctx, s); return err }, []string{"merge", "--abort"}, false},
					{func() error { _, err := d.RebaseAbort(ctx, s); return err }, []string{"rebase", "--abort"}, false},
					{func() error { _, err := d.RebaseContinue(ctx, s); return err }, []string{"-c", "core.editor=true", "rebase", "--continue"}, true},
					{func() error {
						_, err := d.Publish(ctx, s, vcs.PublishPlan{Remote: "fork", Branch: "feature"})
						return err
					}, []string{"push", "--set-upstream", "--end-of-options", "fork", "refs/heads/feature:refs/heads/feature"}, false},
				} {
					if err := tc.run(); err != nil {
						t.Fatalf("%v error = %v", tc.args, err)
					}
					wantArgs := [][]string{tc.args}
					if tc.commits {
						wantArgs = append(slices.Clone(identityProbes), tc.args)
					}
					runs := fakeGitRuns(t, home)
					if len(runs) != len(wantArgs) {
						t.Fatalf("%v ran git %#v,\nwant %q", tc.args, runs, wantArgs)
					}
					for i, run := range runs {
						wantEnv := want
						if tc.commits && i < len(identityProbes) {
							wantEnv = probeEnv(want)
						} else if tc.commits {
							wantEnv = append(slices.Clone(want), fakeIdentEnv...)
						}
						if !slices.Equal(run.Args, wantArgs[i]) || !sameEnv(run.Env, wantEnv) {
							t.Fatalf("%v run %d = %#v,\nwant args %q and env %q", tc.args, i, run, wantArgs[i], wantEnv)
						}
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
				if len(runs) != 4 || !slices.Equal(runs[3].Args, []string{"-c", "core.quotepath=false", "ls-files", "-u"}) || !sameEnv(runs[3].Env, want) {
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

// A merge or rebase commits as exactly the identity each probe resolved. Only
// a role git reports no identity for commits as angee's placeholder; one whose
// configured identity git refuses gets none, so the operation reports git's
// refusal rather than commit as angee.
func TestMergeAndRebaseCommitAsAngeeOnlyWithoutAConfiguredIdentity(t *testing.T) {
	ctx := context.Background()
	angeeAuthor := []string{"GIT_AUTHOR_NAME=angee", "GIT_AUTHOR_EMAIL=angee@example.invalid"}
	angeeCommitter := []string{"GIT_COMMITTER_NAME=angee", "GIT_COMMITTER_EMAIL=angee@example.invalid"}
	fakeAuthor, fakeCommitter := fakeIdentEnv[:2], fakeIdentEnv[2:]
	for _, tc := range []struct {
		markers []string
		want    []string
	}{
		{nil, fakeIdentEnv},
		{[]string{"no-AUTHOR"}, append(slices.Clone(angeeAuthor), fakeCommitter...)},
		{[]string{"no-COMMITTER"}, append(slices.Clone(fakeAuthor), angeeCommitter...)},
		{[]string{"no-AUTHOR", "no-COMMITTER"}, append(slices.Clone(angeeAuthor), angeeCommitter...)},
		{[]string{"invalid-AUTHOR"}, fakeCommitter},
	} {
		t.Run(strings.Join(tc.markers, "+"), func(t *testing.T) {
			home := fakeGit(t, "")
			for _, marker := range tc.markers {
				if err := os.WriteFile(filepath.Join(home, marker), nil, 0o644); err != nil {
					t.Fatalf("WriteFile(%s) error = %v", marker, err)
				}
			}
			d := New(git.New())
			s := vcs.Slot{Path: t.TempDir()}
			for name, run := range map[string]func() error{
				"Merge":          func() error { _, err := d.Merge(ctx, s, "main"); return err },
				"Rebase":         func() error { _, err := d.Rebase(ctx, s, "main"); return err },
				"RebaseContinue": func() error { _, err := d.RebaseContinue(ctx, s); return err },
			} {
				if err := run(); err != nil {
					t.Fatalf("%s() error = %v", name, err)
				}
				runs := fakeGitRuns(t, home)
				if len(runs) != 3 {
					t.Fatalf("%s() ran git %#v, want two identity probes and the operation", name, runs)
				}
				var got []string
				for _, entry := range runs[2].Env {
					if strings.HasPrefix(entry, "GIT_AUTHOR_") || strings.HasPrefix(entry, "GIT_COMMITTER_") {
						got = append(got, entry)
					}
				}
				if !slices.Equal(got, tc.want) {
					t.Fatalf("%s() committed with identity %q, want %q", name, got, tc.want)
				}
			}
		})
	}
}

// The probes get the operator's GIT_CONFIG_COUNT entries for identity keys
// only, renumbered in order; the others, which can carry tokens, and the
// operation itself get none.
func TestIdentityProbesTakeOnlyIdentityConfigEntries(t *testing.T) {
	home := fakeGit(t, "")
	for key, value := range map[string]string{
		"GIT_CONFIG_COUNT":   "4",
		"GIT_CONFIG_KEY_0":   "http.extraHeader",
		"GIT_CONFIG_VALUE_0": "Authorization: Bearer must-not-leak",
		"GIT_CONFIG_KEY_1":   "user.name",
		"GIT_CONFIG_VALUE_1": "Cfg User",
		"GIT_CONFIG_KEY_2":   "core.sshCommand",
		"GIT_CONFIG_VALUE_2": "ssh -i must-not-leak",
		"GIT_CONFIG_KEY_3":   "User.Email",
		"GIT_CONFIG_VALUE_3": "cfg@example.com",
		"GIT_CONFIG_KEY_4":   "user.name",
		"GIT_CONFIG_VALUE_4": "Beyond the count",
	} {
		t.Setenv(key, value)
	}
	if _, err := New(git.New()).Merge(context.Background(), vcs.Slot{Path: t.TempDir()}, "main"); err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
	runs := fakeGitRuns(t, home)
	if len(runs) != 3 {
		t.Fatalf("Merge() ran git %#v, want two identity probes and the merge", runs)
	}
	config := func(env []string) []string {
		var entries []string
		for _, entry := range env {
			if strings.HasPrefix(entry, "GIT_CONFIG_") && !strings.HasPrefix(entry, "GIT_CONFIG_NOSYSTEM") {
				entries = append(entries, entry)
			}
		}
		slices.Sort(entries)
		return entries
	}
	want := []string{"GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=user.name", "GIT_CONFIG_KEY_1=User.Email", "GIT_CONFIG_VALUE_0=Cfg User", "GIT_CONFIG_VALUE_1=cfg@example.com"}
	for i, run := range runs[:2] {
		if got := config(run.Env); !slices.Equal(got, want) {
			t.Fatalf("probe %d config entries = %q, want %q", i, got, want)
		}
	}
	if got := config(runs[2].Env); len(got) != 0 {
		t.Fatalf("merge config entries = %q, want none", got)
	}
}

// With real git, a merge through the driver commits as the identity git
// resolves from the operator's environment and config files, the way
// sync-base's merge does (EMAIL included), only with none as angee's
// placeholder, and not at all with an invalid one.
func TestMergeCommitsAsTheIdentityGitResolves(t *testing.T) {
	ctx := context.Background()
	dir, remote := clone(t)
	advanceRemote(t, dir, remote, "main")
	// Only what each case provides is left: no slot identity, no HOME or
	// XDG config, no system config.
	run(t, dir, "config", "--unset", "user.name")
	run(t, dir, "config", "--unset", "user.email")
	before := gitOutput(t, dir, "rev-parse", "HEAD")
	writeConfig := func(t *testing.T, path, contents string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", path, err)
		}
	}
	userConfig := func(t *testing.T, path, name string) {
		t.Helper()
		writeConfig(t, path, "[user]\n\tname = "+name+"\n\temail = user@example.com\n")
	}
	for _, tc := range []struct {
		name    string
		setup   func(t *testing.T, home string)
		want    string
		wantErr string
	}{
		{"nothing configured", func(*testing.T, string) {}, "angee <angee@example.invalid>|angee <angee@example.invalid>", ""},
		{"HOME's gitconfig", func(t *testing.T, home string) { userConfig(t, filepath.Join(home, ".gitconfig"), "Home User") },
			"Home User <user@example.com>|Home User <user@example.com>", ""},
		{"GIT_CONFIG_GLOBAL", func(t *testing.T, home string) {
			path := filepath.Join(t.TempDir(), "gitconfig")
			userConfig(t, path, "Global User")
			t.Setenv("GIT_CONFIG_GLOBAL", path)
		}, "Global User <user@example.com>|Global User <user@example.com>", ""},
		{"identity variables", func(t *testing.T, _ string) {
			t.Setenv("GIT_AUTHOR_NAME", "Env Author")
			t.Setenv("GIT_AUTHOR_EMAIL", "author@example.com")
			t.Setenv("GIT_COMMITTER_NAME", "Env Committer")
			t.Setenv("GIT_COMMITTER_EMAIL", "committer@example.com")
		}, "Env Author <author@example.com>|Env Committer <committer@example.com>", ""},
		{"author variables only", func(t *testing.T, _ string) {
			t.Setenv("GIT_AUTHOR_NAME", "Env Author")
			t.Setenv("GIT_AUTHOR_EMAIL", "author@example.com")
		}, "Env Author <author@example.com>|angee <angee@example.invalid>", ""},
		{"EMAIL for a configured name", func(t *testing.T, home string) {
			writeConfig(t, filepath.Join(home, ".gitconfig"), "[user]\n\tname = Home User\n")
			t.Setenv("EMAIL", "env@example.com")
		}, "Home User <env@example.com>|Home User <env@example.com>", ""},
		{"identity config entries", func(t *testing.T, _ string) {
			t.Setenv("GIT_CONFIG_COUNT", "3")
			t.Setenv("GIT_CONFIG_KEY_0", "http.extraHeader")
			t.Setenv("GIT_CONFIG_VALUE_0", "Authorization: Bearer must-not-leak")
			t.Setenv("GIT_CONFIG_KEY_1", "user.name")
			t.Setenv("GIT_CONFIG_VALUE_1", "Cfg User")
			t.Setenv("GIT_CONFIG_KEY_2", "user.email")
			t.Setenv("GIT_CONFIG_VALUE_2", "cfg@example.com")
		}, "Cfg User <cfg@example.com>|Cfg User <cfg@example.com>", ""},
		{"an invalid configured name", func(t *testing.T, home string) {
			writeConfig(t, filepath.Join(home, ".gitconfig"), "[user]\n\tname = <>\n\temail = user@example.com\n")
		}, "", "disallowed characters"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unsetIdentityEnv(t)
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
			t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
			tc.setup(t, home)
			run(t, dir, "reset", "--hard", before)

			result, err := New(git.New()).Merge(ctx, vcs.Slot{Path: dir}, "origin/main")
			if tc.wantErr != "" {
				if err == nil || result.OK || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Merge() = %#v, %v, want git's refusal %q", result, err, tc.wantErr)
				}
				if got := gitOutput(t, dir, "rev-parse", "HEAD"); got != before {
					t.Fatalf("HEAD after a refused merge = %s, want %s", got, before)
				}
				return
			}
			if err != nil || !result.OK {
				t.Fatalf("Merge() = %#v, %v, want OK", result, err)
			}
			if got := gitOutput(t, dir, "log", "-1", "--format=%an <%ae>|%cn <%ce>"); got != tc.want {
				t.Fatalf("merge commit identity = %q, want %q", got, tc.want)
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
