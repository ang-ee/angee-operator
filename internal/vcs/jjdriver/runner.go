package jjdriver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/ang-ee/angee-operator/internal/git"
	"github.com/ang-ee/angee-operator/internal/logctx"
)

// jjEnv is what every jj run gets on top of git.IsolatedEnv: a no-op editor,
// so a command that would open one never waits, and the operator's JJ_CONFIG
// when it sets one. Mutations still pass -m wherever jj would open an editor.
// (`--config ui.editor=true` cannot do this: jj reads the value as a
// boolean.) No identity is set: the commits jj records are the user's, from
// user.name and user.email in their jj config, which HOME, XDG_CONFIG_HOME
// and JJ_CONFIG locate, as the git driver's commits are the user's. Unlike
// the git driver, there is no placeholder for a user with none yet: jj then
// records an empty identity, which `jj git push` refuses. Phase 8 of the plan
// adds the fallback before any jj verb pushes.
func jjEnv() []string {
	env := []string{"JJ_EDITOR=true"}
	if config, ok := os.LookupEnv("JJ_CONFIG"); ok {
		env = append(env, "JJ_CONFIG="+config)
	}
	return env
}

// globalArgs start every jj invocation. snapshot.auto-update-stale lets a
// slot whose working copy another workspace rewrote update itself instead of
// failing (plan D8). It is passed here rather than relied on in the
// repository's config, because jj keeps that config per user (Q9): an
// operator with another HOME would not see it.
var globalArgs = []string{"--no-pager", "--color", "never", "--config", "snapshot.auto-update-stale=true"}

// waitDelay bounds how long a killed jj may hold its output pipes open.
const waitDelay = 100 * time.Millisecond

// runner runs jj from PATH, in git.IsolatedEnv plus jjEnv, the same
// environment git verbs on a slot get, so jj's own fetches and pushes reach
// the same ssh agent and credential helpers.
//
// jj never runs in the process's working directory: a slot command runs with
// the slot as working directory and `-R .`, so jj loads the slot's own
// workspace (`@` is its working-copy commit, and no workspace name is needed)
// and never a repository it would find above the slot, and the probe runs in
// the system temporary directory, outside any repository, since jj records
// every repository it loads in the user's config directory.
//
// snapshot lets jj snapshot the slot's working copy before the command, as
// every slot read and mutation must (plan D7, D8). read adds
// --ignore-working-copy and --at-op, for the reads that follow a snapshot in
// the same call: they all see the operation the snapshot recorded, whatever
// agents do in the repository meanwhile. A primary is only ever named with
// `-R` together with --ignore-working-copy, since otherwise jj would snapshot
// the primary's working copy instead; nothing in this package needs that yet.
type runner struct {
	bin string
}

// snapshot runs a jj command in the slot that snapshots its working copy
// first.
func (r runner) snapshot(ctx context.Context, slot string, args ...string) ([]byte, error) {
	if slot == "" {
		return nil, errNoSlot
	}
	return r.run(ctx, slot, append([]string{"-R", "."}, args...))
}

// read runs a jj command in the slot without snapshotting it, at operation
// op: the one a snapshot by the same caller recorded.
func (r runner) read(ctx context.Context, slot, op string, args ...string) ([]byte, error) {
	if slot == "" {
		return nil, errNoSlot
	}
	if op == "" {
		return nil, errors.New("a jj read without the operation of its snapshot")
	}
	return r.run(ctx, slot, append([]string{"-R", ".", "--ignore-working-copy", "--at-op", op}, args...))
}

// errNoSlot refuses a slot command with no slot, which would otherwise run jj
// in the process's working directory.
var errNoSlot = errors.New("a jj slot command needs the slot's path")

// network runs a jj command in the slot that reaches a remote (`jj git
// fetch`, `jj git push`), snapshotting first, under ANGEE_GIT_TIMEOUT like
// every git network operation. operation names it in a timeout error.
func (r runner) network(ctx context.Context, operation, slot string, args ...string) ([]byte, error) {
	var out []byte
	err := git.RunNetworkOperation(ctx, operation, slot, func(ctx context.Context) error {
		var err error
		out, err = r.snapshot(ctx, slot, args...)
		return err
	})
	return out, err
}

// run runs jj with globalArgs and args in dir and returns its standard
// output. A failure is a *CommandError carrying standard error. Only
// environment key names are traced, never values.
func (r runner) run(ctx context.Context, dir string, args []string) ([]byte, error) {
	if dir == "" {
		return nil, errors.New("jj needs a working directory of its own, not the process's")
	}
	bin := r.bin
	if bin == "" {
		bin = "jj"
	}
	argv := append(append([]string(nil), globalArgs...), args...)
	env := git.IsolatedEnv(jjEnv()...)
	cmd := exec.CommandContext(ctx, bin, argv...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.WaitDelay = waitDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	trace := logctx.TraceExec(ctx, bin, argv, dir, slog.Any("env", logctx.EnvKeys(env)))
	err := cmd.Run()
	trace(logctx.CombinedOutput(stdout.Bytes(), stderr.Bytes()), err)
	if err != nil {
		return stdout.Bytes(), &CommandError{
			Args:   logctx.RedactArgs(argv),
			Stderr: logctx.RedactText(strings.TrimSpace(stderr.String())),
			Err:    err,
		}
	}
	return stdout.Bytes(), nil
}

// CommandError is a jj command that failed to start or exited non-zero. Args
// and Stderr are redacted, so the error can be returned to clients; Stderr is
// jj's own account of the failure.
type CommandError struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *CommandError) Error() string {
	return fmt.Sprintf("jj %v: %v: %s", e.Args, e.Err, e.Stderr)
}

func (e *CommandError) Unwrap() error { return e.Err }
