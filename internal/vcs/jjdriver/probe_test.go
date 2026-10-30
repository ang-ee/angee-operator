package jjdriver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ang-ee/angee-operator/internal/git"
	"github.com/ang-ee/angee-operator/internal/vcs"
)

// colocateHelp is the part of `jj workspace add --help` the probe reads, as
// the jj in .github/jj-rev prints it.
const colocateHelp = `Usage: jj workspace add [OPTIONS] <DESTINATION>

Options:
      --name <NAME>
          A name for the workspace
      --colocate
          Create a colocated Git worktree for the workspace
`

// TestProbe reads jj and git through fakes: what each prints decides whether
// this machine supports jj slots, and every shortfall is named with its fix.
func TestProbe(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		replies map[string]string
		goos    string
		// want lists what Err must say, and hints what its hint must; both
		// empty when jj slots are supported.
		want, hints []string
		jjErr       bool
		gitErr      bool
	}{
		{
			name:    "supported",
			replies: map[string]string{"jj-version": "jj 0.45.1-f8f808f\n", "jj-help": colocateHelp, "git-version": "git version 2.50.1 (Apple Git-155)\n"},
		},
		{
			name:    "a windows build of git at the floor",
			replies: map[string]string{"jj-version": "jj 0.45.1\n", "jj-help": colocateHelp, "git-version": "git version 2.42.0.windows.2\n"},
		},
		{
			name:    "no jj",
			replies: map[string]string{"git-version": "git version 2.50.1\n"},
			want:    []string{"jj is not on PATH"},
			hints:   []string{"brew install --HEAD jj"},
			jjErr:   true,
		},
		{
			name:    "a jj without --colocate",
			replies: map[string]string{"jj-version": "jj 0.34.0\n", "jj-help": strings.ReplaceAll(colocateHelp, "      --colocate\n", ""), "git-version": "git version 2.50.1\n"},
			want:    []string{"jj 0.34.0 has no `jj workspace add --colocate`"},
			hints:   []string{"brew install --HEAD jj"},
			jjErr:   true,
		},
		{
			name:    "a jj that mentions --colocate only in prose",
			replies: map[string]string{"jj-version": "jj 0.34.0\n", "jj-help": "Usage: jj workspace add\n\n  See --colocate in a later release.\n", "git-version": "git version 2.50.1\n"},
			want:    []string{"has no `jj workspace add --colocate`"},
			jjErr:   true,
		},
		{
			name:    "git below the floor",
			replies: map[string]string{"jj-version": "jj 0.45.1\n", "jj-help": colocateHelp, "git-version": "git version 2.41.9\n"},
			want:    []string{"git version 2.41.9 is older than 2.42"},
			hints:   []string{"Upgrade git to 2.42 or later."},
			gitErr:  true,
		},
		{
			name:    "windows",
			replies: map[string]string{"jj-version": "jj 0.45.1\n", "jj-help": colocateHelp, "git-version": "git version 2.50.1\n"},
			goos:    "windows",
			want:    []string{"not supported on Windows yet"},
			hints:   []string{"Use a git slot mode on Windows."},
		},
		{
			name:    "every shortfall at once",
			replies: map[string]string{"git-version": "git version 2.30.0\n"},
			goos:    "windows",
			want:    []string{"Windows", "jj is not on PATH", "git version 2.30.0 is older than 2.42"},
			hints:   []string{"Use a git slot mode", "brew install --HEAD jj", "Upgrade git"},
			jjErr:   true,
			gitErr:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeTools(t, tc.replies)
			goos := tc.goos
			if goos == "" {
				goos = "linux"
			}
			caps, complete := probe(ctx, runner{}, git.New(), goos)
			if !complete {
				t.Fatal("probe() did not complete")
			}
			if (caps.JJErr != nil) != tc.jjErr || (caps.GitErr != nil) != tc.gitErr {
				t.Fatalf("probe() = %#v, want jj error %v, git error %v", caps, tc.jjErr, tc.gitErr)
			}
			if version := strings.TrimSpace(tc.replies["jj-version"]); caps.JJ != version {
				t.Fatalf("probe().JJ = %q, want %q", caps.JJ, version)
			}
			err := caps.Err()
			if len(tc.want) == 0 {
				if err != nil {
					t.Fatalf("Err() = %v, want nil", err)
				}
				return
			}
			var unsupported *UnsupportedError
			if !errors.As(err, &unsupported) || !errors.Is(err, vcs.ErrUnsupported) {
				t.Fatalf("Err() = %v, want an *UnsupportedError wrapping vcs.ErrUnsupported", err)
			}
			for _, cause := range []error{caps.OSErr, caps.JJErr, caps.GitErr} {
				if cause != nil && !errors.Is(err, cause) {
					t.Errorf("Err() = %v does not wrap its cause %v", err, cause)
				}
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Err() = %q, want it to say %q", err, want)
				}
			}
			for _, hint := range tc.hints {
				if !strings.Contains(unsupported.Hint, hint) {
					t.Errorf("Err().Hint = %q, want it to say %q", unsupported.Hint, hint)
				}
			}
		})
	}
}

// TestProbeOnce keeps the first probe that ran to completion, and not one
// that was cut short.
func TestProbeOnce(t *testing.T) {
	var once probeOnce
	runs := 0
	run := func(context.Context) (Capabilities, bool) {
		runs++
		return Capabilities{JJ: "jj"}, runs > 1
	}
	once.get(context.Background(), run)
	once.get(context.Background(), run)
	once.get(context.Background(), run)
	if runs != 2 {
		t.Fatalf("probe ran %d times, want 2: once cut short, then once for good", runs)
	}
}

// TestProbeCutShort reports a probe its timeout or its caller ended as
// incomplete, so Probe does not keep it for the life of the process.
func TestProbeCutShort(t *testing.T) {
	home := fakeTools(t, map[string]string{"jj-version": "jj 0.45.1\n", "jj-help": colocateHelp, "git-version": "git version 2.50.1\n"})
	if _, complete := probe(context.Background(), runner{}, git.New(), "linux"); !complete {
		t.Fatal("probe() of answering tools did not complete")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, complete := probe(canceled, runner{}, git.New(), "linux"); complete {
		t.Fatal("probe() with a cancelled context completed")
	}
	if err := os.WriteFile(filepath.Join(home, "sleep"), nil, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	defer func(timeout time.Duration) { probeTimeout = timeout }(probeTimeout)
	probeTimeout = 200 * time.Millisecond
	caps, complete := probe(context.Background(), runner{}, git.New(), "linux")
	if complete || caps.JJErr == nil {
		t.Fatalf("probe() of a jj that hangs = %#v, complete %v; want an incomplete probe", caps, complete)
	}
}

func TestGitSupported(t *testing.T) {
	for line, want := range map[string]bool{
		"git version 2.42.0":                 true,
		"git version 2.50.1 (Apple Git-155)": true,
		"git version 2.42.0.windows.2":       true,
		"git version 3.0.0":                  true,
		"git version 2.41.9":                 false,
		"git version 1.99.0":                 false,
		"not git":                            false,
	} {
		if got := GitSupported(line); got != want {
			t.Errorf("GitSupported(%q) = %v, want %v", line, got, want)
		}
	}
}
