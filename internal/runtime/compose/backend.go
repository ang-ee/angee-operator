package compose

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ang-ee/angee-operator/internal/logctx"
	"github.com/ang-ee/angee-operator/internal/runtime"
	"gopkg.in/yaml.v3"
)

type Runner interface {
	Run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error)
}

type ExecRunner struct{}

type InputRunner interface {
	RunInput(context.Context, string, []string, []byte, string, ...string) ([]byte, error)
}

func (ExecRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = runtime.ChildEnviron(env)
	trace := logctx.TraceExec(ctx, name, args, dir, slog.Any("env", logctx.EnvKeys(env)))
	out, err := cmd.CombinedOutput()
	trace(out, err)
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, runtime.LimitLogBytes(strings.TrimSpace(string(out)), maxErrorOutputBytes))
	}
	return out, nil
}

// maxErrorOutputBytes bounds the output a failed docker command's error
// carries: its last lines, where docker reports the failure
// (runtime.LimitLogBytes). A failed `docker compose logs` would otherwise put
// its whole output into the error.
const maxErrorOutputBytes = 4 << 10

func (ExecRunner) RunInput(ctx context.Context, dir string, env []string, input []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = runtime.ChildEnviron(env)
	cmd.Stdin = bytes.NewReader(input)
	return cmd.CombinedOutput()
}

type Backend struct {
	Runner Runner
}

func NewBackend() Backend {
	return Backend{Runner: ExecRunner{}}
}

func (b Backend) Build(ctx context.Context, target runtime.Target) error {
	args := b.baseArgs(target.Root, target.EnvFile)
	args = append(args, "build")
	args = append(args, target.Services...)
	_, err := b.run(ctx, target.Root, target.EnvFile, args...)
	return err
}

func (b Backend) Up(ctx context.Context, target runtime.Target) error {
	args := b.baseArgs(target.Root, target.EnvFile)
	args = append(args, "up", "-d")
	if target.Build {
		args = append(args, "--build")
	}
	args = append(args, target.Services...)
	_, err := b.run(ctx, target.Root, target.EnvFile, args...)
	return err
}

func (b Backend) UpForeground(ctx context.Context, target runtime.Target, stdout io.Writer, stderr io.Writer) error {
	args := b.baseArgs(target.Root, target.EnvFile)
	args = append(args, "up")
	// Attached: stay in the foreground streaming container logs (for `angee
	// dev`). Detached: stream the build/pull progress then return (`angee up`).
	if !target.Attached {
		args = append(args, "-d")
	}
	if target.Build {
		args = append(args, "--build")
	}
	args = append(args, target.Services...)
	return b.runForeground(ctx, target.Root, target.EnvFile, stdout, stderr, target.Attached, args...)
}

func (b Backend) Down(ctx context.Context, target runtime.Target) error {
	args := b.baseArgs(target.Root, target.EnvFile)
	args = append(args, "down")
	_, err := b.run(ctx, target.Root, target.EnvFile, args...)
	return err
}

func (b Backend) Start(ctx context.Context, target runtime.Target) error {
	args := b.baseArgs(target.Root, target.EnvFile)
	args = append(args, "start")
	args = append(args, target.Services...)
	_, err := b.run(ctx, target.Root, target.EnvFile, args...)
	return err
}

func (b Backend) Stop(ctx context.Context, target runtime.Target) error {
	args := b.baseArgs(target.Root, target.EnvFile)
	args = append(args, "stop")
	args = append(args, target.Services...)
	_, err := b.run(ctx, target.Root, target.EnvFile, args...)
	return err
}

func (b Backend) Restart(ctx context.Context, target runtime.Target) error {
	args := b.baseArgs(target.Root, target.EnvFile)
	args = append(args, "restart")
	args = append(args, target.Services...)
	_, err := b.run(ctx, target.Root, target.EnvFile, args...)
	return err
}

// maxJobOutputBytes bounds the output RunJob returns to the last 1 MiB a job
// printed.
const maxJobOutputBytes = 1 << 20

func (b Backend) RunJob(ctx context.Context, target runtime.Target, job runtime.JobSpec) ([]byte, error) {
	service := job.Name
	runner, ok := b.inputRunner()
	if !ok {
		return nil, fmt.Errorf("compose backend does not support in-memory job overrides")
	}
	if job.Transient {
		return b.runTransientJob(ctx, runner, target, job)
	}
	args := configurationArgs(target.EnvFile)
	args = append(args, "up", "-d", "--force-recreate", "--no-deps", service)
	env, err := runtime.ReadEnvFile(target.EnvFile)
	if err != nil {
		return nil, err
	}
	if _, err := runner.RunInput(ctx, target.Root, env, job.Configuration, "docker", args...); err != nil {
		return nil, err
	}
	waitArgs := configurationArgs(target.EnvFile)
	waitArgs = append(waitArgs, "wait", service)
	_, waitErr := runner.RunInput(ctx, target.Root, env, job.Configuration, "docker", waitArgs...)
	logArgs := configurationArgs(target.EnvFile)
	logArgs = append(logArgs, "logs", "--no-color", "--no-log-prefix", service)
	out, logErr := runner.RunInput(ctx, target.Root, env, job.Configuration, "docker", logArgs...)
	out = jobOutputTail(out)
	if waitErr != nil {
		return out, fmt.Errorf("container job %s failed: %w", service, waitErr)
	}
	if logErr != nil {
		return out, logErr
	}
	return out, nil
}

// runTransientJob runs a job as a one-off container, `docker compose run --rm`
// from the request's in-memory configuration, so the attempt leaves nothing in
// the shared project: no recreated service container that keeps its
// configuration, inputs and environment. --no-deps leaves prerequisites to the
// caller, which checked them, and a one-off container publishes no service
// ports. Its output is what the run printed, without terminal controls and
// redacted before it is cut to its last maxJobOutputBytes so the cut cannot
// leave part of a secret.
//
// Cancelling ctx kills the docker client but not the container, which the
// daemon keeps running, so the container is named up front and removed by that
// name (removeCanceledJob).
func (b Backend) runTransientJob(ctx context.Context, runner InputRunner, target runtime.Target, job runtime.JobSpec) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("container job %s failed: %w", job.Name, err)
	}
	env, err := runtime.ReadEnvFile(target.EnvFile)
	if err != nil {
		return nil, err
	}
	container := oneOffContainerName(job)
	args := configurationArgs(target.EnvFile)
	args = append(args, "run", "--rm", "--no-deps", "-T", "--name", container, job.Name)
	exited := make(chan struct{})
	removed := make(chan error, 1)
	stop := context.AfterFunc(ctx, func() {
		removed <- b.removeCanceledJob(context.WithoutCancel(ctx), target, container, exited)
	})
	out, runErr := runner.RunInput(ctx, target.Root, env, job.Configuration, "docker", args...)
	close(exited)
	out = jobOutputTail([]byte(logctx.RedactText(runtime.StripTerminalControls(string(out)))))
	var removeErr error
	if !stop() {
		if err := <-removed; err != nil {
			removeErr = fmt.Errorf("remove container %s of canceled job %s: %w", container, job.Name, err)
		}
	}
	if runErr != nil {
		if ctx.Err() != nil {
			// The client was killed; why matters more than its exit status.
			runErr = ctx.Err()
		}
		return out, errors.Join(fmt.Errorf("container job %s failed: %w", job.Name, runErr), removeErr)
	}
	// The run succeeded, so --rm has removed its container; a removal that a
	// cancel landing just after it started can only fail on a container that
	// is gone, and must not fail the job.
	return out, nil
}

// transientRemoveInterval is how often removeCanceledJob removes the container
// of a canceled job again while its docker client has yet to exit.
var transientRemoveInterval = time.Second

// transientRemoveTimeout bounds one `docker rm -f` of a canceled job.
const transientRemoveTimeout = 30 * time.Second

// removeCanceledJob force-removes the container of a canceled transient job:
// at once, which stops a running container and so ends the attached client
// too, then every transientRemoveInterval until the client has exited, in case
// the compose plugin outlives the killed docker client and starts the
// container after an earlier removal, and once more after it has exited, when
// nothing is left to start it, for a container a create in flight left behind.
// ctx must outlive the cancellation; each removal has its own timeout. The
// last removal's error is returned.
func (b Backend) removeCanceledJob(ctx context.Context, target runtime.Target, container string, exited <-chan struct{}) error {
	ticker := time.NewTicker(transientRemoveInterval)
	defer ticker.Stop()
	for {
		_ = b.removeContainer(ctx, target, container)
		select {
		case <-exited:
			return b.removeContainer(ctx, target, container)
		case <-ticker.C:
		}
	}
}

// removeContainer runs `docker rm -f` on one container. A container that is
// already gone is no error, as older docker clients report it.
func (b Backend) removeContainer(ctx context.Context, target runtime.Target, container string) error {
	ctx, cancel := context.WithTimeout(ctx, transientRemoveTimeout)
	defer cancel()
	out, err := b.run(ctx, target.Root, target.EnvFile, "rm", "-f", container)
	if err != nil && strings.Contains(string(out), "No such container") {
		return nil
	}
	return err
}

// oneOffContainerName names one run of a transient job as docker compose names
// its own one-off containers, <project>-<service>-run-<id>, with a random id so
// concurrent and earlier runs never collide.
func oneOffContainerName(job runtime.JobSpec) string {
	var project struct {
		Name string `yaml:"name"`
	}
	// An undecodable configuration fails the run itself; the name only needs
	// a prefix.
	_ = yaml.Unmarshal(job.Configuration, &project)
	if project.Name == "" {
		project.Name = "angee"
	}
	id := make([]byte, 6)
	_, _ = rand.Read(id) // never fails (crypto/rand)
	return project.Name + "-" + job.Name + "-run-" + hex.EncodeToString(id)
}

// jobOutputTail keeps the last maxJobOutputBytes of a job's output.
func jobOutputTail(out []byte) []byte {
	if len(out) > maxJobOutputBytes {
		return out[len(out)-maxJobOutputBytes:]
	}
	return out
}

func (b Backend) Apply(ctx context.Context, target runtime.Target) error {
	args := b.baseArgs(target.Root, target.EnvFile)
	if len(target.Configuration) != 0 {
		args = configurationArgs(target.EnvFile)
	}
	args = append(args, "up", "-d", "--force-recreate", "--no-deps")
	args = append(args, target.Services...)
	if len(target.Configuration) != 0 {
		runner, ok := b.inputRunner()
		if !ok {
			return fmt.Errorf("compose backend does not support in-memory runtime configuration")
		}
		env, err := runtime.ReadEnvFile(target.EnvFile)
		if err != nil {
			return err
		}
		_, err = runner.RunInput(ctx, target.Root, env, target.Configuration, "docker", args...)
		return err
	}
	_, err := b.run(ctx, target.Root, target.EnvFile, args...)
	return err
}

// Logs runs `docker compose logs --tail N` and returns its output, every line
// the tail asks for, labelled by docker compose. A caller's byte limit applies
// to the whole read (runtime.LimitLogBytes), not here.
func (b Backend) Logs(ctx context.Context, req runtime.LogsRequest) (<-chan string, error) {
	args := b.baseArgs(req.Root, req.EnvFile)
	args = append(args, "logs")
	if req.Follow {
		args = append(args, "--follow")
	}
	if req.Tail > 0 {
		args = append(args, "--tail", strconv.Itoa(req.Tail))
	}
	args = append(args, req.Services...)
	out, err := b.run(ctx, req.Root, req.EnvFile, args...)
	if err != nil {
		return nil, err
	}
	ch := make(chan string, 1)
	ch <- string(out)
	close(ch)
	return ch, nil
}

// StreamLogs runs `docker compose logs [--follow]` and streams its combined
// output one line per channel element, closing on process exit or ctx cancel.
// Unlike Logs it never buffers, so a `--follow` stream surfaces lines live.
func (b Backend) StreamLogs(ctx context.Context, req runtime.LogsRequest) (<-chan string, error) {
	args := b.baseArgs(req.Root, req.EnvFile)
	args = append(args, "logs")
	if req.Follow {
		args = append(args, "--follow")
	}
	if req.NoPrefix {
		args = append(args, "--no-log-prefix")
	}
	if req.Tail > 0 {
		args = append(args, "--tail", strconv.Itoa(req.Tail))
	}
	args = append(args, req.Services...)
	// A test Runner can't stream a live process, so capture through it and
	// replay by line. The real ExecRunner streams below. Mirrors runForeground.
	if b.Runner != nil && !isExecRunner(b.Runner) {
		out, err := b.run(ctx, req.Root, req.EnvFile, args...)
		if err != nil {
			return nil, err
		}
		return runtime.ReplayLines(ctx, out), nil
	}
	env, err := runtime.ReadEnvFile(req.EnvFile)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Dir = req.Root
	cmd.Env = runtime.ChildEnviron(env)
	ch, err := runtime.StreamCommand(ctx, cmd, slog.Any("env", logctx.EnvKeys(env)))
	if err != nil {
		return nil, fmt.Errorf("docker %s: %w", strings.Join(args, " "), err)
	}
	return ch, nil
}

func (b Backend) Status(ctx context.Context, req runtime.StatusRequest) ([]runtime.ServiceStatus, error) {
	args := b.baseArgs(req.Root, req.EnvFile)
	if len(req.Configuration) != 0 {
		args = configurationArgs(req.EnvFile)
	}
	args = append(args, "ps", "--format", "json")
	if req.All {
		args = append(args, "--all")
	}
	var out []byte
	var err error
	if len(req.Configuration) != 0 {
		runner, ok := b.inputRunner()
		if !ok {
			return nil, fmt.Errorf("compose backend does not support in-memory runtime configuration")
		}
		env, readErr := runtime.ReadEnvFile(req.EnvFile)
		if readErr != nil {
			return nil, readErr
		}
		out, err = runner.RunInput(ctx, req.Root, env, req.Configuration, "docker", args...)
	} else {
		out, err = b.run(ctx, req.Root, req.EnvFile, args...)
	}
	if err != nil {
		return nil, err
	}
	return parsePS(out)
}

func (b Backend) run(ctx context.Context, root string, envFile string, args ...string) ([]byte, error) {
	if b.Runner == nil {
		b.Runner = ExecRunner{}
	}
	env, err := runtime.ReadEnvFile(envFile)
	if err != nil {
		return nil, err
	}
	return b.Runner.Run(ctx, root, env, "docker", args...)
}

func (b Backend) runForeground(ctx context.Context, root string, envFile string, stdout io.Writer, stderr io.Writer, graceful bool, args ...string) error {
	// A test Runner can't stream a live process, so route through it (capturing
	// the command) instead of shelling out. The real ExecRunner streams below.
	if b.Runner != nil && !isExecRunner(b.Runner) {
		_, err := b.run(ctx, root, envFile, args...)
		return err
	}
	env, err := runtime.ReadEnvFile(envFile)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Dir = root
	cmd.Env = runtime.ChildEnviron(env)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if graceful {
		// An attached `docker compose up` is interrupted, not killed, so it
		// gets to stop containers cleanly. WaitDelay bounds the grace period
		// before the process is force-killed. Mirrors the process-compose
		// foreground runner (which is always attached).
		cmd.Cancel = func() error {
			if cmd.Process == nil {
				return nil
			}
			return cmd.Process.Signal(os.Interrupt)
		}
		cmd.WaitDelay = runtime.GracefulWaitDelay
	}
	trace := logctx.TraceExec(ctx, "docker", args, root, slog.Any("env", logctx.EnvKeys(env)))
	runErr := cmd.Run()
	trace(nil, runErr)
	if runErr != nil {
		// A graceful shutdown via context cancellation (Ctrl-C, or a sibling
		// backend exiting) is the expected way out of an attached run — not an
		// error to surface.
		if graceful && ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("docker %s: %w", strings.Join(args, " "), runErr)
	}
	return nil
}

func (b Backend) baseArgs(root, envFile string) []string {
	args := []string{"compose", "-f", filepath.Join(root, "docker-compose.yaml")}
	if envFile != "" {
		args = append(args, "--env-file", envFile)
	}
	return args
}

func configurationArgs(envFile string) []string {
	args := []string{"compose", "-f", "-"}
	if envFile != "" {
		args = append(args, "--env-file", envFile)
	}
	return args
}

func (b Backend) inputRunner() (InputRunner, bool) {
	if b.Runner == nil {
		return ExecRunner{}, true
	}
	runner, ok := b.Runner.(InputRunner)
	return runner, ok
}

func parsePS(data []byte) ([]runtime.ServiceStatus, error) {
	var statuses []runtime.ServiceStatus
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var one struct {
			Service  string `json:"Service"`
			Name     string `json:"Name"`
			State    string `json:"State"`
			Health   string `json:"Health"`
			ExitCode int    `json:"ExitCode"`
		}
		if err := json.Unmarshal([]byte(line), &one); err != nil {
			// Status reads combined output, so docker's stderr notices (orphan
			// containers, deprecations) land here; skip anything that is not a
			// JSON record rather than turning a banner into an unknown state.
			continue
		}
		name := one.Service
		if name == "" {
			name = one.Name
		}
		if name == "" {
			continue
		}
		status := runtime.ServiceStatus{Name: name, Runtime: "container", State: one.State, Health: one.Health, ExitCode: &one.ExitCode}
		// docker compose fills ExitCode only for an exited container: one in
		// its restart backoff reads 0 whatever its last exit, so it has none.
		if strings.EqualFold(one.State, "restarting") {
			status.ExitCode = nil
		}
		statuses = append(statuses, status)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read docker compose status: %w", err)
	}
	return statuses, nil
}

var ErrNoServices = errors.New("no container services selected")

func isExecRunner(r Runner) bool {
	switch r.(type) {
	case ExecRunner, *ExecRunner:
		return true
	default:
		return false
	}
}
