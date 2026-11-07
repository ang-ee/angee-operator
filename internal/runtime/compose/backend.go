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
	"sync"
	"time"

	"github.com/ang-ee/angee-operator/internal/logctx"
	"github.com/ang-ee/angee-operator/internal/runtime"
	"github.com/ang-ee/angee-operator/internal/runtime/processgroup"
	"gopkg.in/yaml.v3"
)

type Runner interface {
	Run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error)
}

type ExecRunner struct{}

type InputRunner interface {
	RunInput(context.Context, string, []string, []byte, string, ...string) ([]byte, error)
}

// StreamRunner runs a command with its configuration on fd 3 and independent
// stdin and stdout. It returns stderr alone when stdout is set, or combined
// stdout and stderr when it is nil. It is the test seam for streaming one-off
// containers; existing in-memory operations continue through InputRunner.
type StreamRunner interface {
	RunStreams(context.Context, string, []string, []byte, io.Reader, io.Writer, string, ...string) ([]byte, error)
}

type transientInputRunner interface {
	RunTransientInput(context.Context, string, []string, []byte, string, ...string) ([]byte, error)
}

type containerEventRunner interface {
	subscribeContainerEvents(context.Context, string, []string, string) (containerEventSubscription, error)
}

type containerEventSubscription interface {
	waitCreated(context.Context) (bool, error)
	waitDestroyed(context.Context) (bool, error)
	seenCreate() bool
	eventError() error
	stop()
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
	return runInput(ctx, dir, env, input, name, args, false)
}

// RunTransientInput runs an input-bearing transient container client in an
// isolated process group that is killed immediately on cancellation.
func (ExecRunner) RunTransientInput(ctx context.Context, dir string, env []string, input []byte, name string, args ...string) ([]byte, error) {
	return runInput(ctx, dir, env, input, name, args, true)
}

func runInput(ctx context.Context, dir string, env []string, input []byte, name string, args []string, isolated bool) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = runtime.ChildEnviron(env)
	cmd.Stdin = bytes.NewReader(input)
	stopGroup := func() {}
	if isolated {
		var err error
		stopGroup, err = processgroup.ConfigureImmediateKill(cmd)
		if err != nil {
			return nil, err
		}
	}
	out, err := cmd.CombinedOutput()
	stopGroup()
	return out, err
}

// RunStreams runs an input-bearing command with configuration supplied on fd
// 3. When stdout is non-nil, only stderr is returned as diagnostics; otherwise
// stdout and stderr are combined. Diagnostics retain only their bounded tail.
func (ExecRunner) RunStreams(ctx context.Context, dir string, env []string, configuration []byte, stdin io.Reader, stdout io.Writer, name string, args ...string) ([]byte, error) {
	configRead, configWrite, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("open compose configuration pipe: %w", err)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = runtime.ChildEnviron(env)
	cmd.Stdin = stdin
	diagnostics := runtime.NewLimitedLogRead(maxJobOutputBytes)
	var outputErrors *firstErrorWriter
	if stdout == nil {
		cmd.Stdout = diagnostics
	} else {
		outputErrors = &firstErrorWriter{writer: stdout}
		cmd.Stdout = outputErrors
	}
	cmd.Stderr = diagnostics
	cmd.ExtraFiles = []*os.File{configRead}
	stopGroup, err := processgroup.ConfigureImmediateKill(cmd)
	if err != nil {
		_ = configRead.Close()
		_ = configWrite.Close()
		return nil, err
	}
	trace := logctx.TraceExec(ctx, name, args, dir, slog.Any("env", logctx.EnvKeys(env)))
	if err := cmd.Start(); err != nil {
		_ = configRead.Close()
		_ = configWrite.Close()
		stopGroup()
		trace(nil, err)
		return nil, err
	}
	_ = configRead.Close()
	written := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(configWrite, bytes.NewReader(configuration))
		_ = configWrite.Close()
		written <- copyErr
	}()
	runErr := cmd.Wait()
	stopGroup()
	var writeErr error
	select {
	case writeErr = <-written:
	default:
		_ = configWrite.Close()
		writeErr = <-written
	}
	if writeErr != nil {
		runErr = errors.Join(runErr, fmt.Errorf("write compose configuration: %w", writeErr))
	}
	if outputErrors != nil {
		runErr = errors.Join(runErr, outputErrors.err())
	}
	out := []byte(diagnostics.String())
	trace(out, runErr)
	return out, runErr
}

type firstErrorWriter struct {
	mu     sync.Mutex
	writer io.Writer
	first  error
}

func (w *firstErrorWriter) Write(data []byte) (int, error) {
	n, err := w.writer.Write(data)
	if err != nil {
		w.mu.Lock()
		if w.first == nil {
			w.first = err
		}
		w.mu.Unlock()
	}
	return n, err
}

func (w *firstErrorWriter) err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.first
}

type dockerContainerEvents struct {
	cancel        context.CancelFunc
	created       chan struct{}
	destroyed     chan struct{}
	done          chan struct{}
	createdOnce   sync.Once
	destroyedOnce sync.Once
	stopOnce      sync.Once
	mu            sync.Mutex
	stopped       bool
	err           error
}

func (ExecRunner) subscribeContainerEvents(ctx context.Context, dir string, env []string, container string) (containerEventSubscription, error) {
	eventCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	args := dockerEventArgs(container, time.Now().Add(-time.Minute).UTC())
	cmd := exec.CommandContext(eventCtx, "docker", args...)
	cmd.Dir = dir
	cmd.Env = runtime.ChildEnviron(env)
	output, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("open docker events output for container %s: %w", container, err)
	}
	diagnostics := runtime.NewLimitedLogRead(maxErrorOutputBytes)
	cmd.Stderr = diagnostics
	stopGroup, err := processgroup.ConfigureImmediateKill(cmd)
	if err != nil {
		cancel()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		stopGroup()
		return nil, fmt.Errorf("start docker events for container %s: %w: %s", container, err, strings.TrimSpace(diagnostics.String()))
	}
	subscription := &dockerContainerEvents{
		cancel: func() {
			// The subscription owns this process. Invoke the configured group
			// cancellation directly instead of depending on CommandContext's
			// asynchronous watcher to notice eventCtx before Scanner can unblock.
			_ = cmd.Cancel()
			cancel()
		},
		created:   make(chan struct{}),
		destroyed: make(chan struct{}),
		done:      make(chan struct{}),
	}
	go subscription.read(cmd, output, diagnostics, stopGroup)
	return subscription, nil
}

func dockerEventArgs(container string, since time.Time) []string {
	return []string{
		"events", "--format", "{{json .}}", "--since", since.Format(time.RFC3339Nano),
		"--filter", "type=container", "--filter", "container=" + container,
	}
}

func (s *dockerContainerEvents) read(cmd *exec.Cmd, output io.ReadCloser, diagnostics *runtime.LimitedLogRead, stopGroup func()) {
	defer close(s.done)
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		var event struct {
			Action string `json:"Action"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			continue
		}
		action := event.Action
		if action == "" {
			action = event.Status
		}
		switch strings.ToLower(action) {
		case "create":
			s.createdOnce.Do(func() { close(s.created) })
		case "destroy":
			s.destroyedOnce.Do(func() { close(s.destroyed) })
		}
	}
	scanErr := scanner.Err()
	if scanErr != nil {
		// A malformed or oversized daemon event must not leave the subscription
		// process running with nobody reading its output.
		s.cancel()
	}
	waitErr := cmd.Wait()
	stopGroup()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	s.err = errors.Join(scanErr, waitErr)
	if s.err == nil {
		s.err = errors.New("docker events subscription exited unexpectedly")
	}
	if detail := strings.TrimSpace(diagnostics.String()); detail != "" {
		s.err = fmt.Errorf("%w: %s", s.err, detail)
	}
}

func (s *dockerContainerEvents) waitCreated(ctx context.Context) (bool, error) {
	return s.wait(ctx, s.created)
}

func (s *dockerContainerEvents) waitDestroyed(ctx context.Context) (bool, error) {
	return s.wait(ctx, s.destroyed)
}

func (s *dockerContainerEvents) seenCreate() bool {
	select {
	case <-s.created:
		return true
	default:
		return false
	}
}

func (s *dockerContainerEvents) eventError() error {
	select {
	case <-s.done:
		s.mu.Lock()
		err := s.err
		s.mu.Unlock()
		return err
	default:
		return nil
	}
}

func (s *dockerContainerEvents) wait(ctx context.Context, event <-chan struct{}) (bool, error) {
	select {
	case <-event:
		return true, nil
	default:
	}
	select {
	case <-s.done:
		s.mu.Lock()
		err := s.err
		s.mu.Unlock()
		return false, err
	default:
	}
	select {
	case <-event:
		return true, nil
	case <-ctx.Done():
		select {
		case <-event:
			return true, nil
		default:
			return false, ctx.Err()
		}
	case <-s.done:
		select {
		case <-event:
			return true, nil
		default:
		}
		s.mu.Lock()
		err := s.err
		s.mu.Unlock()
		return false, err
	}
}

func (s *dockerContainerEvents) stop() {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.stopped = true
		s.mu.Unlock()
		s.cancel()
		<-s.done
	})
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
	if len(target.Configuration) != 0 {
		args = configurationArgs(target.EnvFile)
	}
	args = append(args, "stop")
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

// RunJobStreams runs a job as a one-off container with the compose document on
// fd 3, leaving stdin available to the job and keeping an explicitly supplied
// stdout stream out of the returned diagnostics.
func (b Backend) RunJobStreams(ctx context.Context, target runtime.Target, job runtime.JobSpec, streams runtime.JobStreams) ([]byte, error) {
	if !job.Transient {
		return nil, &runtime.JobCapabilityError{Reason: "streaming container jobs require a transient job; shared compose jobs cannot expose isolated byte streams"}
	}
	if err := streams.Role.Validate(); err != nil {
		return nil, err
	}
	if err := streamJobSupport(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("container job %s failed: %w", job.Name, err)
	}
	runner, ok := b.streamRunner()
	if !ok {
		return nil, fmt.Errorf("compose backend does not support streaming jobs")
	}
	env, err := runtime.ReadEnvFile(target.EnvFile)
	if err != nil {
		return nil, err
	}
	configuration := job.Configuration
	if streams.Role == runtime.StreamRoleSave {
		configuration, err = configurationWithLoggingDisabled(configuration, job.Name)
		if err != nil {
			return nil, err
		}
	}
	container := oneOffContainerName(job)
	args := streamConfigurationArgs(target.Root, target.EnvFile)
	args = append(args, "run", "--rm", "--no-deps", "-T", "--name", container, job.Name)
	eventRunner, ok := b.containerEventRunner()
	if !ok {
		return nil, fmt.Errorf("compose backend does not support container event subscriptions")
	}
	events, err := eventRunner.subscribeContainerEvents(ctx, target.Root, env, container)
	if err != nil {
		return nil, err
	}
	defer events.stop()
	stdout := streams.Stdout
	if streams.Role == runtime.StreamRoleSave && stdout == nil {
		stdout = io.Discard
	}
	diagnostics, runErr := runner.RunStreams(ctx, target.Root, env, configuration, streams.Stdin, stdout, "docker", args...)
	diagnostics = safeJobOutput(diagnostics)
	if runErr == nil {
		return diagnostics, nil
	}
	killed := ctx.Err() != nil
	if killed {
		runErr = ctx.Err()
	}
	removeErr := b.cleanupFailedContainer(context.WithoutCancel(ctx), target, job.Name, container, killed, events)
	return diagnostics, errors.Join(fmt.Errorf("container job %s failed: %w", job.Name, runErr), removeErr)
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
// The docker client runs in its own process group and is killed immediately on
// cancellation. A Docker event subscription started before it records whether
// the daemon created the named container. After failure, a created container is
// removed exactly once and its destroy event is confirmed without polling.
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
	eventRunner, ok := b.containerEventRunner()
	if !ok {
		return nil, fmt.Errorf("compose backend does not support container event subscriptions")
	}
	events, err := eventRunner.subscribeContainerEvents(ctx, target.Root, env, container)
	if err != nil {
		return nil, err
	}
	defer events.stop()
	var out []byte
	var runErr error
	if isolated, ok := runner.(transientInputRunner); ok {
		out, runErr = isolated.RunTransientInput(ctx, target.Root, env, job.Configuration, "docker", args...)
	} else {
		out, runErr = runner.RunInput(ctx, target.Root, env, job.Configuration, "docker", args...)
	}
	out = safeJobOutput(out)
	if runErr != nil {
		killed := ctx.Err() != nil
		if killed {
			runErr = ctx.Err()
		}
		removeErr := b.cleanupFailedContainer(context.WithoutCancel(ctx), target, job.Name, container, killed, events)
		return out, errors.Join(fmt.Errorf("container job %s failed: %w", job.Name, runErr), removeErr)
	}
	return out, nil
}

// transientRemoveTimeout bounds the single `docker rm -f -v` after a failed job
// and the following wait for Docker's destroy event.
const transientRemoveTimeout = 30 * time.Second

// transientCreateTimeout is the bounded window in which a killed Docker client
// may still complete an in-flight container create. It is a variable so tests
// can cover the timeout without sleeping for the production grace period.
var transientCreateTimeout = runtime.GracefulWaitDelay

func (b Backend) cleanupFailedContainer(ctx context.Context, target runtime.Target, jobName, container string, killed bool, events containerEventSubscription) error {
	var eventErr error
	if killed {
		createCtx, cancelCreate := context.WithTimeout(ctx, transientCreateTimeout)
		created, createErr := events.waitCreated(createCtx)
		cancelCreate()
		if !created {
			if errors.Is(createErr, context.DeadlineExceeded) {
				// No create reached Docker's pushed stream within the bounded
				// in-flight window, so there is no container to remove now. Stop
				// and drain the subscription once, then report any create it saw
				// at the boundary instead of silently losing that evidence.
				events.stop()
				if events.seenCreate() {
					return fmt.Errorf("container %s of failed job %s may remain: Docker reported its create after the bounded %s cleanup window", container, jobName, transientCreateTimeout)
				}
				// A create after the subscription is stopped is outside the
				// bounded proof. Make that residual risk visible as well.
				logctx.From(ctx).WarnContext(ctx, "Docker did not report a container create within the bounded cleanup window; a later create cannot be removed by this attempt",
					"container", container, "job", jobName, "window", transientCreateTimeout)
				return nil
			}
			if createErr == nil {
				createErr = errors.New("event subscription ended without a create event")
			}
			eventErr = fmt.Errorf("docker creation could not be confirmed: %w", createErr)
		}
	} else {
		// Ordinary command failures cannot leave a daemon create in flight. Probe
		// only for a subscription that has already died; never delay removal.
		eventErr = events.eventError()
	}
	alreadyGone, removeErr := b.removeContainer(ctx, target, container)
	if removeErr != nil {
		removeErr = fmt.Errorf("remove container %s of failed job %s: %w", container, jobName, removeErr)
	}
	if eventErr != nil {
		confirmationErr := fmt.Errorf("container %s of failed job %s may remain: removal could not be confirmed from Docker events: %w", container, jobName, eventErr)
		return errors.Join(removeErr, confirmationErr)
	}
	if removeErr != nil && !killed {
		return removeErr
	}
	if alreadyGone && !killed {
		// A normal client failure cannot race an in-flight create. Docker either
		// auto-removed the container (including its anonymous volumes) or never
		// made one, so there will be no later destroy event to await.
		eventErr = events.eventError()
		if eventErr == nil {
			return removeErr
		}
		confirmationErr := fmt.Errorf("container %s of failed job %s may remain: removal could not be confirmed from Docker events: %w", container, jobName, eventErr)
		return errors.Join(removeErr, confirmationErr)
	}
	destroyCtx, cancelDestroy := context.WithTimeout(ctx, transientRemoveTimeout)
	destroyed, destroyErr := events.waitDestroyed(destroyCtx)
	cancelDestroy()
	if !destroyed {
		if destroyErr == nil {
			destroyErr = errors.New("event subscription ended without a destroy event")
		}
		destroyErr = fmt.Errorf("container %s of failed job %s may remain: removal could not be confirmed from Docker events: %w", container, jobName, destroyErr)
	}
	return errors.Join(removeErr, destroyErr)
}

// removeContainer runs `docker rm -f -v` once and reports whether Docker
// removed nothing, so no destroy event of its own will follow. Only an output
// line equal to the container name counts as a removal, because Docker prints
// each removed name alone on stdout. A "No such container" report (Docker 27
// and 28 print it on stderr yet exit 0), Docker 29's silent exit 0, any other
// exit-0 output, and a failure saying removal is already in progress all count
// as already gone. Other failures are returned.
func (b Backend) removeContainer(ctx context.Context, target runtime.Target, container string) (alreadyGone bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, transientRemoveTimeout)
	defer cancel()
	out, err := b.run(ctx, target.Root, target.EnvFile, "rm", "-f", "-v", container)
	text := string(out)
	detail := strings.ToLower(text)
	if strings.Contains(detail, "no such container") {
		return true, nil
	}
	if err == nil {
		for line := range strings.Lines(text) {
			if strings.TrimSpace(line) == container {
				return false, nil
			}
		}
		return true, nil
	}
	if strings.Contains(detail, "already in progress") {
		return true, nil
	}
	return false, err
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

func safeJobOutput(out []byte) []byte {
	return jobOutputTail([]byte(logctx.RedactText(runtime.StripTerminalControls(string(out)))))
}

func configurationWithLoggingDisabled(configuration []byte, service string) ([]byte, error) {
	var document File
	if err := yaml.Unmarshal(configuration, &document); err != nil {
		return nil, fmt.Errorf("decode compose job configuration: %w", err)
	}
	jobService, ok := document.Services[service]
	if !ok {
		return nil, fmt.Errorf("compose job %s is absent from compiled runtime configuration", service)
	}
	jobService.Logging = &Logging{Driver: "none"}
	document.Services[service] = jobService
	encoded, err := Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode compose job configuration: %w", err)
	}
	return encoded, nil
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

func streamConfigurationArgs(root, envFile string) []string {
	args := []string{"compose", "-f", "/dev/fd/3", "--project-directory", root}
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

func (b Backend) streamRunner() (StreamRunner, bool) {
	if b.Runner == nil {
		return ExecRunner{}, true
	}
	runner, ok := b.Runner.(StreamRunner)
	return runner, ok
}

func (b Backend) containerEventRunner() (containerEventRunner, bool) {
	if b.Runner == nil {
		return ExecRunner{}, true
	}
	runner, ok := b.Runner.(containerEventRunner)
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
