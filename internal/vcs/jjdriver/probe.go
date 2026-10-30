package jjdriver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ang-ee/angee-operator/internal/git"
	"github.com/ang-ee/angee-operator/internal/vcs"
)

// MinGitVersion is the oldest git the jj driver runs on. Creating a jj slot
// needs git.RelativeWorktreesVersion, which the service checks; doctor warns
// below this one for every stack.
const MinGitVersion = "2.42"

// InstallHint says how to get a jj that can create colocated workspaces.
const InstallHint = "Install a current jj build, e.g. `brew install --HEAD jj`."

// probeTimeout bounds one whole probe. A probe it cuts short is not kept.
var probeTimeout = 10 * time.Second

// Capabilities is what this machine offers jj slots, as Probe found it.
type Capabilities struct {
	// JJ is what `jj --version` printed; empty when jj did not answer.
	JJ string
	// JJErr says why jj cannot create slots: it is not on PATH, it did not
	// answer, or `jj workspace add` has no --colocate. Nil when it can.
	JJErr error
	// Git is what `git --version` printed; empty when git did not answer.
	Git string
	// GitErr says why git falls short: it did not answer, or it is older than
	// MinGitVersion. Nil when it does not.
	GitErr error
	// OSErr is set on Windows, where nobody has verified colocated secondary
	// workspaces yet.
	OSErr error
}

// Err returns nil when this machine supports jj slots, else an
// *UnsupportedError naming every shortfall and what to do about it.
func (c Capabilities) Err() error {
	var causes []error
	var hints []string
	for _, check := range []struct {
		err  error
		hint string
	}{
		{c.OSErr, "Use a git slot mode on Windows."},
		{c.JJErr, InstallHint},
		{c.GitErr, "Upgrade git to " + MinGitVersion + " or later."},
	} {
		if check.err != nil {
			causes = append(causes, check.err)
			hints = append(hints, check.hint)
		}
	}
	if len(causes) == 0 {
		return nil
	}
	return &UnsupportedError{Causes: causes, Hint: strings.Join(hints, " ")}
}

// UnsupportedError reports that this machine cannot run jj slots. It wraps
// its causes (Capabilities' OSErr, JJErr and GitErr, those that are set) and
// vcs.ErrUnsupported; Hint says what would fix them.
type UnsupportedError struct {
	Causes []error
	Hint   string
}

func (e *UnsupportedError) Error() string {
	reasons := make([]string, 0, len(e.Causes))
	for _, cause := range e.Causes {
		reasons = append(reasons, cause.Error())
	}
	return "jj slots are not supported here: " + strings.Join(reasons, "; ")
}

func (e *UnsupportedError) Unwrap() []error {
	return append(slices.Clone(e.Causes), vcs.ErrUnsupported)
}

var probed probeOnce

// Probe reports what this machine offers jj slots: `jj --version`, whether
// `jj workspace add` takes --colocate, `git --version` against
// MinGitVersion, and that the platform is not Windows. The first probe that
// runs to completion, neither cancelled nor timed out, is kept for the life
// of the process.
func Probe(ctx context.Context) Capabilities {
	return probed.get(ctx, func(ctx context.Context) (Capabilities, bool) {
		return probe(ctx, runner{}, git.New(), runtime.GOOS)
	})
}

// probe reads what this machine offers, and whether every check ran to its
// end: complete is false when ctx or probeTimeout cut it short. jj runs in
// the system temporary directory, outside any repository: jj records every
// repository it loads in the user's config directory, even for --version.
func probe(ctx context.Context, jj runner, gitClient git.Client, goos string) (caps Capabilities, complete bool) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	if goos == "windows" {
		caps.OSErr = errors.New("jj slots are not supported on Windows yet")
	}
	caps.JJ, caps.JJErr = probeJJ(ctx, jj, os.TempDir())
	caps.Git, caps.GitErr = probeGit(ctx, gitClient)
	return caps, ctx.Err() == nil
}

// colocateOption matches the --colocate option's own line in `jj workspace
// add --help`, not a mention of it in another option's text.
var colocateOption = regexp.MustCompile(`(?m)^\s+--colocate\b`)

func probeJJ(ctx context.Context, jj runner, dir string) (string, error) {
	out, err := jj.run(ctx, dir, []string{"--version"})
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", errors.New("jj is not on PATH")
		}
		return "", fmt.Errorf("jj --version failed: %w", err)
	}
	version := strings.TrimSpace(string(out))
	help, err := jj.run(ctx, dir, []string{"workspace", "add", "--help"})
	if err != nil {
		return version, fmt.Errorf("jj workspace add --help failed: %w", err)
	}
	if !colocateOption.Match(help) {
		return version, fmt.Errorf("%s has no `jj workspace add --colocate`", version)
	}
	return version, nil
}

func probeGit(ctx context.Context, client git.Client) (string, error) {
	version, err := client.Version(ctx)
	if err != nil {
		return "", fmt.Errorf("git --version failed: %w", err)
	}
	if !GitSupported(version) {
		return version, fmt.Errorf("%s is older than %s", version, MinGitVersion)
	}
	return version, nil
}

// GitSupported reports whether a `git --version` line names MinGitVersion or
// later.
func GitSupported(versionLine string) bool {
	return git.VersionAtLeast(versionLine, MinGitVersion)
}

// probeOnce keeps the first probe that ran to completion. A probe that was
// cancelled or timed out is returned but not kept, so the next caller probes
// again.
type probeOnce struct {
	mu   sync.Mutex
	done bool
	caps Capabilities
}

func (o *probeOnce) get(ctx context.Context, run func(context.Context) (Capabilities, bool)) Capabilities {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.done {
		return o.caps
	}
	caps, complete := run(ctx)
	if complete {
		o.caps, o.done = caps, true
	}
	return caps
}
