package jjdriver

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/internal/git"
)

// unsetEnv clears keys for the test.
func unsetEnv(t *testing.T, keys ...string) {
	t.Helper()
	for _, key := range keys {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("Unsetenv(%s) error = %v", key, err)
		}
	}
}

// identityVars are the identity entries of env.
func identityVars(env []string) []string {
	var vars []string
	for _, entry := range env {
		if strings.HasPrefix(entry, "JJ_USER=") || strings.HasPrefix(entry, "JJ_EMAIL=") {
			vars = append(vars, entry)
		}
	}
	return vars
}

// TestIdentityFallbackRuns pins, through a fake jj and git, when a jj run
// gets an identity of its own: only a run that may record commits (a
// snapshot, a command in the primary), and only what jj's config leaves
// empty, as one `jj -R . --ignore-working-copy config list` reads it, filled
// from `git var GIT_AUTHOR_IDENT` in the colocated slot. It is resolved once
// per runner. The operator's own JJ_USER and JJ_EMAIL need no probe.
func TestIdentityFallbackRuns(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		replies  map[string]string
		operator bool
		probes   []string
		want     []string
	}{
		{name: "no jj identity", replies: map[string]string{"git-var": "Git User <git@example.com> 1793222148 -0400\n", "jj-config-user": "user.email\t\"\"\nuser.name\t\"\"\n"},
			probes: []string{"jj config list", "git var GIT_AUTHOR_IDENT"},
			want:   []string{"JJ_USER=Git User", "JJ_EMAIL=git@example.com"}},
		{name: "a jj name only", replies: map[string]string{"git-var": "Git User <git@example.com> 1793222148 -0400\n", "jj-config-user": "user.email\t\"\"\nuser.name\t\"JJ User\"\n"},
			probes: []string{"jj config list", "git var GIT_AUTHOR_IDENT"},
			want:   []string{"JJ_EMAIL=git@example.com"}},
		{name: "a jj identity", replies: map[string]string{"jj-config-user": "user.email\t\"jj@example.com\"\nuser.name\t\"JJ User\"\n"},
			probes: []string{"jj config list"}},
		{name: "the operator's", operator: true},
		{name: "none from git either", probes: []string{"jj config list", "git var GIT_AUTHOR_IDENT"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			replies := map[string]string{"jj-version": "jj 0.0.0\n"}
			for name, content := range tc.replies {
				replies[name] = content
			}
			home := fakeTools(t, replies)
			if !tc.operator {
				unsetEnv(t, "JJ_USER", "JJ_EMAIL")
			}
			unsetEnv(t, append(slices.Clone(git.IdentityEnvKeys), "EMAIL")...)
			slot := t.TempDir()
			for _, dir := range []string{".jj", ".git"} {
				if err := os.Mkdir(filepath.Join(slot, dir), 0o755); err != nil {
					t.Fatalf("Mkdir() error = %v", err)
				}
			}
			d := New(git.New())
			for _, run := range []func() error{
				func() error { _, err := d.jj.latest(ctx, slot, "op", "log"); return err },
				func() error { _, err := d.jj.snapshot(ctx, slot, "status"); return err },
				func() error { _, err := d.jj.inPrimary(ctx, slot, "workspace", "add"); return err },
				func() error { _, err := d.jj.store(ctx, slot, "workspace", "list"); return err },
			} {
				if err := run(); err != nil {
					t.Fatalf("run error = %v", err)
				}
			}
			runs := fakeRuns(t, home)
			var probes []string
			var commands []fakeRun
			for _, run := range runs {
				switch {
				case slices.Contains(run.Args, "config"):
					if want := []string{"-R", ".", "--ignore-working-copy", "config", "list", "--include-defaults"}; !slices.Equal(run.Args[len(globalArgs):len(globalArgs)+len(want)], want) {
						t.Errorf("probe %q, want it to read the slot's repository without its working copy", run.Args)
					}
					probes = append(probes, "jj config list")
				case run.Tool == "git":
					if dir, _ := filepath.EvalSymlinks(run.Dir); dir != mustEval(t, slot) {
						t.Errorf("git probe ran in %s, want the colocated slot", run.Dir)
					}
					probes = append(probes, "git var "+run.Args[len(run.Args)-1])
				default:
					commands = append(commands, run)
				}
			}
			if !slices.Equal(probes, tc.probes) {
				t.Errorf("probes = %q, want %q, once", probes, tc.probes)
			}
			if len(commands) != 4 {
				t.Fatalf("ran %d commands, want 4: %v", len(commands), commands)
			}
			for i, run := range commands {
				got := identityVars(run.Env)
				switch want := tc.want; {
				case tc.operator:
					want = []string{"JJ_USER=tester", "JJ_EMAIL=tester@example.invalid"}
					if !slices.Equal(got, want) {
						t.Errorf("command %d %q identity = %q, want the operator's %q", i, run.Args, got, want)
					}
				case i == 1 || i == 2:
					if !slices.Equal(got, want) {
						t.Errorf("command %d %q identity = %q, want %q", i, run.Args, got, want)
					}
				default:
					if len(got) != 0 {
						t.Errorf("read %d %q identity = %q, want none", i, run.Args, got)
					}
				}
			}
		})
	}
}

// A new primary's identity is resolved for the primary, not for the system
// temporary directory `jj git init` runs in: git, in the primary's own clone,
// reads its repository config (an includeIf identity, say), while jj, with no
// repository there yet, reads the user's config, outside the primary.
func TestEnsurePrimaryResolvesTheIdentityForThePrimary(t *testing.T) {
	home := fakeTools(t, map[string]string{"jj-version": "jj 0.0.0\n", "git-var": "Repo User <repo@example.com> 1793222148 -0400\n"})
	unsetEnv(t, "JJ_USER", "JJ_EMAIL")
	unsetEnv(t, append(slices.Clone(git.IdentityEnvKeys), "EMAIL")...)
	primary := t.TempDir()
	if err := os.Mkdir(filepath.Join(primary, ".git"), 0o755); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	if err := New(git.New()).EnsurePrimary(context.Background(), primary); err != nil {
		t.Fatalf("EnsurePrimary() error = %v", err)
	}
	runs := fakeRuns(t, home)
	if len(runs) != 3 {
		t.Fatalf("ran %d commands, want the jj probe, the git probe and the init: %v", len(runs), runs)
	}
	temp := mustEval(t, os.TempDir())
	for i, want := range []struct{ tool, dir, arg string }{{"jj", temp, "config"}, {"git", mustEval(t, primary), "var"}, {"jj", temp, "init"}} {
		if dir, _ := filepath.EvalSymlinks(runs[i].Dir); runs[i].Tool != want.tool || dir != want.dir || !slices.Contains(runs[i].Args, want.arg) {
			t.Errorf("run %d = %s %q in %s, want %s %s in %s", i, runs[i].Tool, runs[i].Args, runs[i].Dir, want.tool, want.arg, want.dir)
		}
	}
	if got := identityVars(runs[2].Env); !slices.Equal(got, []string{"JJ_USER=Repo User", "JJ_EMAIL=repo@example.com"}) {
		t.Errorf("jj git init identity = %q, want the primary's git identity", got)
	}
}

func mustEval(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("EvalSymlinks() error = %v", err)
	}
	return resolved
}

// TestIdentityFallbackCommits makes a slot with real jj for an operator whose
// jj has no identity: its working-copy commit records git's configured
// identity, else, with none in git either, angee's placeholder, and a name
// jj's config does give is kept.
func TestIdentityFallbackCommits(t *testing.T) {
	for _, tc := range []struct {
		name, jjConfig, gitConfig, want string
	}{
		{"git's", "", "[user]\n\tname = Git User\n\temail = git@example.com\n", "Git User <git@example.com>"},
		{"the placeholder", "", "", git.PlaceholderName + " <" + git.PlaceholderEmail + ">"},
		{"jj's name, git's email", "[user]\nname = \"JJ User\"\n", "[user]\n\tname = Git User\n\temail = git@example.com\n", "JJ User <git@example.com>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPrimary(t)
			unsetEnv(t, "JJ_USER", "JJ_EMAIL", "EMAIL")
			unsetEnv(t, git.IdentityEnvKeys...)
			gitConfig := filepath.Join(t.TempDir(), "gitconfig")
			f.write(filepath.Dir(gitConfig), filepath.Base(gitConfig), tc.gitConfig)
			t.Setenv("GIT_CONFIG_GLOBAL", gitConfig)
			t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
			f.write(filepath.Dir(os.Getenv("JJ_CONFIG")), filepath.Base(os.Getenv("JJ_CONFIG")), tc.jjConfig)

			s := f.slotSpec("ws/slot", "stack--ws", "")
			if err := f.add(s); err != nil {
				t.Fatalf("Add() error = %v", err)
			}
			f.write(s.Path, "new.txt", "edit\n")
			if _, err := driver().Status(context.Background(), s); err != nil {
				t.Fatalf("Status() error = %v", err)
			}
			got := strings.TrimSpace(f.jj(s.Path, "--ignore-working-copy", "log", "--no-graph", "-r", "@", "-T",
				`author.name() ++ " <" ++ author.email() ++ ">|" ++ committer.name() ++ " <" ++ committer.email() ++ ">"`))
			if want := tc.want + "|" + tc.want; got != want {
				t.Errorf("the slot's working-copy commit is by %q, want %q", got, want)
			}
		})
	}
}

// A jj config jj cannot read adds no identity, even where git has one: a
// guess must never override an identity jj has. A resolution its context
// cut short is not kept, so the next run of the driver resolves it.
func TestIdentityFallbackAddsNothingItCouldNotRead(t *testing.T) {
	home := fakeTools(t, map[string]string{"jj-version": "jj 0.0.0\n", "git-var": "Git User <git@example.com> 1793222148 -0400\n",
		"jj-config-user": "user.email\t\"\"\nuser.name\t\"\"\n", "fail-on": "config\n"})
	unsetEnv(t, "JJ_USER", "JJ_EMAIL")
	unsetEnv(t, append(slices.Clone(git.IdentityEnvKeys), "EMAIL")...)
	slot := t.TempDir()
	for _, dir := range []string{".jj", ".git"} {
		if err := os.Mkdir(filepath.Join(slot, dir), 0o755); err != nil {
			t.Fatalf("Mkdir() error = %v", err)
		}
	}
	id := &identity{git: git.New()}
	if got := id.env(context.Background(), runner{}, slot); got != nil {
		t.Fatalf("env() with jj's config unreadable = %q, want none", got)
	}
	if err := os.Remove(filepath.Join(home, "fail-on")); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	id = &identity{git: git.New()}
	if got := id.env(canceled, runner{}, slot); got != nil {
		t.Fatalf("env() on a cancelled context = %q, want none", got)
	}
	if got, want := id.env(context.Background(), runner{}, slot), []string{"JJ_USER=Git User", "JJ_EMAIL=git@example.com"}; !slices.Equal(got, want) {
		t.Fatalf("env() after a cancelled one = %q, want it resolved again: %q", got, want)
	}
}
