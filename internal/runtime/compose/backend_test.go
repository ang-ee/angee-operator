package compose

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ang-ee/angee-operator/internal/logctx"
	"github.com/ang-ee/angee-operator/internal/runtime"
)

// A failed command's error carries the last lines of its output, within
// maxErrorOutputBytes, cut on a line boundary, rather than all of it: a failed
// `docker compose logs` would otherwise put its whole output into the error.
func TestExecRunnerErrorKeepsTheOutputsLastLines(t *testing.T) {
	const script = `i=0; while [ $i -lt 2000 ]; do echo "line-$i"; i=$((i+1)); done; exit 3`
	out, err := (ExecRunner{}).Run(t.Context(), "", nil, "sh", "-c", script)
	if err == nil {
		t.Fatal("Run() error = nil, want exit status 3")
	}
	if !strings.Contains(string(out), "line-0\n") {
		t.Fatalf("Run() output lost its start: %q", out[:min(len(out), 20)])
	}
	prefix := "sh -c " + script + ": exit status 3: "
	detail, ok := strings.CutPrefix(err.Error(), prefix)
	if !ok {
		t.Fatalf("error = %q, want it to start %q", err.Error()[:min(len(err.Error()), 200)], prefix)
	}
	if len(detail) > maxErrorOutputBytes || !strings.HasPrefix(detail, runtime.LogTruncatedMarker+"line-") || !strings.HasSuffix(detail, "\nline-1999") {
		t.Fatalf("error detail = %d bytes from %q to %q, want at most %d: the marker, then whole lines ending at line-1999",
			len(detail), detail[:min(len(detail), 30)], detail[max(len(detail)-30, 0):], maxErrorOutputBytes)
	}
}

func TestExecRunnerTracesCommandWithRedactedArgs(t *testing.T) {
	var logs bytes.Buffer
	ctx := logctx.With(t.Context(), slog.New(logctx.NewCLIHandler(&logs, slog.LevelDebug)))
	_, err := (ExecRunner{}).Run(ctx, "", []string{"API_TOKEN=env-secret"}, "sh", "-c", "exit 0", "--token", "secret", "https://user:password@example.com/repo")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	got := logs.String()
	if !strings.Contains(got, "exec sh -c exit 0 --token *** https://***@example.com/repo") ||
		!strings.Contains(got, "env=[API_TOKEN]") ||
		!strings.Contains(got, "exec finished duration=") {
		t.Fatalf("trace output = %q", got)
	}
	if strings.Contains(got, "secret") || strings.Contains(got, "password") || strings.Contains(got, "user") {
		t.Fatalf("trace output leaked secret data: %q", got)
	}
	if strings.Contains(got, "env-secret") {
		t.Fatalf("trace output leaked env value: %q", got)
	}
}

type recordingRunner struct {
	name string
	env  []string
	args []string
	out  []byte
}

func (r *recordingRunner) Run(_ context.Context, _ string, env []string, name string, args ...string) ([]byte, error) {
	r.name = name
	r.env = append([]string(nil), env...)
	r.args = append([]string(nil), args...)
	return r.out, nil
}

func TestBackendUpCommand(t *testing.T) {
	runner := &recordingRunner{}
	backend := Backend{Runner: runner}
	err := backend.Up(context.Background(), runtime.Target{Root: "/stack", EnvFile: "/stack/.env", Services: []string{"web"}, Build: true})
	if err != nil {
		t.Fatalf("Up() error = %v", err)
	}
	want := []string{"compose", "-f", "/stack/docker-compose.yaml", "--env-file", "/stack/.env", "up", "-d", "--build", "web"}
	if runner.name != "docker" || !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("command = %s %v, want docker %v", runner.name, runner.args, want)
	}
}

func TestBackendStreamLogsCommandAndReplay(t *testing.T) {
	runner := &recordingRunner{out: []byte("line1\nline2\n")}
	backend := Backend{Runner: runner}
	ch, err := backend.StreamLogs(context.Background(), runtime.LogsRequest{
		Root: "/stack", EnvFile: "/stack/.env", Services: []string{"web"}, Follow: true, NoPrefix: true,
	})
	if err != nil {
		t.Fatalf("StreamLogs() error = %v", err)
	}
	// NoPrefix appends --no-log-prefix; follow appends --follow.
	want := []string{"compose", "-f", "/stack/docker-compose.yaml", "--env-file", "/stack/.env", "logs", "--follow", "--no-log-prefix", "web"}
	if runner.name != "docker" || !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("command = %s %v, want docker %v", runner.name, runner.args, want)
	}
	// A non-exec Runner replays the captured output one line per element.
	var got []string
	for line := range ch {
		got = append(got, line)
	}
	if !reflect.DeepEqual(got, []string{"line1\n", "line2\n"}) {
		t.Fatalf("replayed lines = %q, want line1/line2", got)
	}
}

func TestBackendStreamLogsTail(t *testing.T) {
	runner := &recordingRunner{}
	backend := Backend{Runner: runner}
	if _, err := backend.StreamLogs(context.Background(), runtime.LogsRequest{
		Root: "/stack", Services: []string{"web"}, Follow: true, Tail: 50,
	}); err != nil {
		t.Fatalf("StreamLogs() error = %v", err)
	}
	want := []string{"compose", "-f", "/stack/docker-compose.yaml", "logs", "--follow", "--tail", "50", "web"}
	if !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("args = %v, want %v", runner.args, want)
	}
}

// A buffered read passes the tail as a follow does, so both runtimes return the
// last lines of a log.
func TestBackendLogsTail(t *testing.T) {
	runner := &recordingRunner{}
	backend := Backend{Runner: runner}
	if _, err := backend.Logs(context.Background(), runtime.LogsRequest{
		Root: "/stack", Services: []string{"web"}, Tail: 200,
	}); err != nil {
		t.Fatalf("Logs() error = %v", err)
	}
	want := []string{"compose", "-f", "/stack/docker-compose.yaml", "logs", "--tail", "200", "web"}
	if !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("args = %v, want %v", runner.args, want)
	}
}

func TestBackendStreamLogsOmitsNoLogPrefixWhenUnset(t *testing.T) {
	runner := &recordingRunner{}
	backend := Backend{Runner: runner}
	if _, err := backend.StreamLogs(context.Background(), runtime.LogsRequest{
		Root: "/stack", Services: []string{"web"}, Follow: false,
	}); err != nil {
		t.Fatalf("StreamLogs() error = %v", err)
	}
	want := []string{"compose", "-f", "/stack/docker-compose.yaml", "logs", "web"}
	if !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("args = %v, want %v", runner.args, want)
	}
}

func TestBackendUpForegroundDetached(t *testing.T) {
	runner := &recordingRunner{}
	backend := Backend{Runner: runner}
	err := backend.UpForeground(context.Background(), runtime.Target{Root: "/stack", EnvFile: "/stack/.env"}, nil, nil)
	if err != nil {
		t.Fatalf("UpForeground() error = %v", err)
	}
	want := []string{"compose", "-f", "/stack/docker-compose.yaml", "--env-file", "/stack/.env", "up", "-d"}
	if !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("args = %v, want %v", runner.args, want)
	}
}

func TestBackendUpForegroundAttached(t *testing.T) {
	runner := &recordingRunner{}
	backend := Backend{Runner: runner}
	err := backend.UpForeground(context.Background(), runtime.Target{Root: "/stack", EnvFile: "/stack/.env", Attached: true, Build: true}, nil, nil)
	if err != nil {
		t.Fatalf("UpForeground() error = %v", err)
	}
	// Attached omits -d so the stream stays in the foreground; --build still applies.
	want := []string{"compose", "-f", "/stack/docker-compose.yaml", "--env-file", "/stack/.env", "up", "--build"}
	if !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("args = %v, want %v", runner.args, want)
	}
}

func TestParsePS(t *testing.T) {
	got, err := parsePS([]byte(`{"Service":"web","State":"running","Health":"healthy"}
{"Service":"db","State":"running","Health":"unhealthy"}
{"Service":"worker","State":"exited"}
`))
	if err != nil {
		t.Fatalf("parsePS() error = %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("parsePS() len = %d, want 3: %#v", len(got), got)
	}
	if got[0].Name != "web" || got[0].State != "running" || got[0].Health != "healthy" {
		t.Fatalf("web entry = %#v", got[0])
	}
	if got[1].Name != "db" || got[1].State != "running" || got[1].Health != "unhealthy" {
		t.Fatalf("db entry = %#v", got[1])
	}
	if got[2].Name != "worker" || got[2].State != "exited" || got[2].Health != "" {
		t.Fatalf("worker entry = %#v", got[2])
	}
}

// docker compose reports ExitCode 0 for a container in its restart backoff
// whatever its last exit (its Status reads "Restarting (1) ..."), so the entry
// carries no exit code rather than a false 0. An exited one keeps its own.
func TestParsePSLeavesARestartingContainerWithoutAnExitCode(t *testing.T) {
	got, err := parsePS([]byte(`{"Service":"crasher","State":"restarting","Status":"Restarting (1) 2 seconds ago","ExitCode":0}
{"Service":"worker","State":"exited","Status":"Exited (3) 1 minute ago","ExitCode":3}
`))
	if err != nil {
		t.Fatalf("parsePS() error = %v", err)
	}
	if len(got) != 2 || got[0].State != "restarting" || got[0].ExitCode != nil {
		t.Fatalf("crasher entry = %#v, want restarting without an exit code", got)
	}
	if got[1].ExitCode == nil || *got[1].ExitCode != 3 {
		t.Fatalf("worker entry = %#v, want exit 3", got[1])
	}
}

func TestParsePSSkipsNonJSONLines(t *testing.T) {
	got, err := parsePS([]byte("WARN[0000] Found orphan containers\n{\"Service\":\"web\",\"State\":\"running\"}\nnot json\n"))
	if err != nil {
		t.Fatalf("parsePS() error = %v, want banners skipped", err)
	}
	if len(got) != 1 || got[0].Name != "web" || got[0].State != "running" {
		t.Fatalf("parsePS() = %#v, want the single web record", got)
	}
}

func TestBackendUpExportsEnvFileValues(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")
	if err := os.WriteFile(envFile, []byte("A=1\n# a comment\nB=\"quoted\"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	runner := &recordingRunner{}
	backend := Backend{Runner: runner}
	err := backend.Up(context.Background(), runtime.Target{Root: dir, EnvFile: envFile, Services: []string{"web"}})
	if err != nil {
		t.Fatalf("Up() error = %v", err)
	}
	// The env file's current values are handed to the runner as the child's
	// explicit environment, so they override any stale copy the operator
	// inherited at start.
	wantEnv := []string{"A=1", "B=quoted"}
	if !reflect.DeepEqual(runner.env, wantEnv) {
		t.Fatalf("env = %#v, want %#v", runner.env, wantEnv)
	}
	// The --env-file argument stays so compose still reads the file for keys
	// the operator's environment does not carry.
	if !argsContainPair(runner.args, "--env-file", envFile) {
		t.Fatalf("args = %v, want --env-file %s", runner.args, envFile)
	}
}

func TestExecRunnerEnvFileValueOverridesInheritedEnvironment(t *testing.T) {
	t.Setenv("ANGEE_TEST_STALE", "stale")
	out, err := (ExecRunner{}).Run(context.Background(), "", []string{"ANGEE_TEST_STALE=fresh"}, "sh", "-c", "printf %s \"$ANGEE_TEST_STALE\"")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if string(out) != "fresh" {
		t.Fatalf("output = %q, want fresh", out)
	}
}

func TestBackendStatusUsesEnvFile(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")
	if err := os.WriteFile(envFile, []byte("A=1\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	runner := &recordingRunner{out: []byte(`{"Service":"web","State":"running"}` + "\n")}
	backend := Backend{Runner: runner}
	got, err := backend.Status(context.Background(), runtime.StatusRequest{Root: dir, EnvFile: envFile})
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	// Status interpolates the compose file like every other invocation: the
	// env file is both passed to compose and exported as the child's env.
	if !argsContainPair(runner.args, "--env-file", envFile) {
		t.Fatalf("args = %v, want --env-file %s", runner.args, envFile)
	}
	if !reflect.DeepEqual(runner.env, []string{"A=1"}) {
		t.Fatalf("env = %#v, want [A=1]", runner.env)
	}
	if len(got) != 1 || got[0].Name != "web" || got[0].State != "running" {
		t.Fatalf("statuses = %#v", got)
	}
}

func TestBackendStatusAllListsStoppedContainers(t *testing.T) {
	runner := &recordingRunner{out: []byte(`{"Service":"migrate","State":"exited","ExitCode":3}` + "\n")}
	backend := Backend{Runner: runner}
	if _, err := backend.Status(context.Background(), runtime.StatusRequest{Root: t.TempDir()}); err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if slices.Contains(runner.args, "--all") {
		t.Fatalf("args = %v, want running containers only without All", runner.args)
	}
	got, err := backend.Status(context.Background(), runtime.StatusRequest{Root: t.TempDir(), All: true})
	if err != nil {
		t.Fatalf("Status(All) error = %v", err)
	}
	if !slices.Contains(runner.args, "--all") {
		t.Fatalf("args = %v, want --all", runner.args)
	}
	if len(got) != 1 || got[0].State != "exited" || got[0].ExitCode == nil || *got[0].ExitCode != 3 {
		t.Fatalf("statuses = %#v, want migrate exited 3", got)
	}
}

func argsContainPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// jobRunner fakes the docker client for RunJob: RunInput answers the compose
// commands through answer, Run answers `docker rm -f` through remove, and
// every command is recorded with its input.
type jobRunner struct {
	mu       sync.Mutex
	commands []jobCommand
	answer   func(ctx context.Context, args []string) ([]byte, error)
	remove   func(ctx context.Context, args []string) ([]byte, error)
}

type jobCommand struct {
	name  string
	args  []string
	input []byte
}

func (r *jobRunner) Run(ctx context.Context, _ string, _ []string, name string, args ...string) ([]byte, error) {
	r.record(name, args, nil)
	if r.remove == nil {
		return nil, nil
	}
	return r.remove(ctx, args)
}

func (r *jobRunner) RunInput(ctx context.Context, _ string, _ []string, input []byte, name string, args ...string) ([]byte, error) {
	r.record(name, args, input)
	if r.answer == nil {
		return nil, nil
	}
	return r.answer(ctx, args)
}

func (r *jobRunner) record(name string, args []string, input []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commands = append(r.commands, jobCommand{name: name, args: slices.Clone(args), input: input})
}

func (r *jobRunner) recorded() []jobCommand {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.commands)
}

// captureConfiguration is a compiled compose document with one job service.
const captureConfiguration = "name: notes-1a2b3c4d\nservices:\n  capture:\n    image: postgres:17-alpine\n"

var oneOffNamePattern = regexp.MustCompile(`^notes-1a2b3c4d-capture-run-[0-9a-f]{12}$`)

// An ordinary job recreates its service container, waits for it and reads its
// logs, from the in-memory configuration, leaving the container to report its
// terminal state to native dependency conditions.
func TestBackendRunJobRecreatesTheServiceOfAnOrdinaryJob(t *testing.T) {
	runner := &jobRunner{answer: func(_ context.Context, args []string) ([]byte, error) {
		if slices.Contains(args, "logs") {
			return []byte("migrated\n"), nil
		}
		return nil, nil
	}}
	out, err := Backend{Runner: runner}.RunJob(t.Context(), runtime.Target{Root: "/stack", EnvFile: "/stack/.env"}, runtime.JobSpec{Name: "capture", Configuration: []byte(captureConfiguration)})
	if err != nil || string(out) != "migrated\n" {
		t.Fatalf("RunJob() = %q, %v; want the job's logs", out, err)
	}
	base := []string{"compose", "-f", "-", "--env-file", "/stack/.env"}
	want := [][]string{
		append(slices.Clone(base), "up", "-d", "--force-recreate", "--no-deps", "capture"),
		append(slices.Clone(base), "wait", "capture"),
		append(slices.Clone(base), "logs", "--no-color", "--no-log-prefix", "capture"),
	}
	commands := runner.recorded()
	if len(commands) != len(want) {
		t.Fatalf("commands = %+v, want %v", commands, want)
	}
	for i, command := range commands {
		if command.name != "docker" || !reflect.DeepEqual(command.args, want[i]) || string(command.input) != captureConfiguration {
			t.Fatalf("command %d = %s %v with input %q, want docker %v with the configuration", i, command.name, command.args, command.input, want[i])
		}
	}
}

// A transient job is one `docker compose run --rm` of its service from the
// in-memory configuration, in a container named for that run: nothing is
// recreated in the shared project, and nothing is left to remove.
func TestBackendRunJobRunsATransientJobAsAOneOffContainer(t *testing.T) {
	runner := &jobRunner{answer: func(context.Context, []string) ([]byte, error) {
		return []byte("dumped\n"), nil
	}}
	out, err := Backend{Runner: runner}.RunJob(t.Context(), runtime.Target{Root: "/stack", EnvFile: "/stack/.env"}, runtime.JobSpec{Name: "capture", Configuration: []byte(captureConfiguration), Transient: true})
	if err != nil || string(out) != "dumped\n" {
		t.Fatalf("RunJob() = %q, %v; want the run's output", out, err)
	}
	commands := runner.recorded()
	if len(commands) != 1 {
		t.Fatalf("commands = %+v, want the one run", commands)
	}
	command := commands[0]
	want := []string{"compose", "-f", "-", "--env-file", "/stack/.env", "run", "--rm", "--no-deps", "-T", "--name", "", "capture"}
	if len(command.args) == len(want) {
		want[10] = command.args[10]
	}
	if command.name != "docker" || !reflect.DeepEqual(command.args, want) || string(command.input) != captureConfiguration {
		t.Fatalf("command = %s %v with input %q, want docker %v with the configuration", command.name, command.args, command.input, want)
	}
	if !oneOffNamePattern.MatchString(want[10]) {
		t.Fatalf("container name = %q, want <project>-<service>-run-<id>", want[10])
	}
	if again := oneOffContainerName(runtime.JobSpec{Name: "capture", Configuration: []byte(captureConfiguration)}); again == want[10] {
		t.Fatalf("two runs share the container name %q", again)
	}
}

func TestBackendRunJobReportsATransientJobsFailure(t *testing.T) {
	exit := errors.New("exit status 3")
	runner := &jobRunner{answer: func(context.Context, []string) ([]byte, error) {
		return []byte("pg_dump: error: connection refused\n"), exit
	}}
	out, err := Backend{Runner: runner}.RunJob(t.Context(), runtime.Target{Root: "/stack"}, runtime.JobSpec{Name: "capture", Configuration: []byte(captureConfiguration), Transient: true})
	if !errors.Is(err, exit) || !strings.Contains(err.Error(), "container job capture failed") {
		t.Fatalf("RunJob() error = %v, want the run's exit naming the job", err)
	}
	if string(out) != "pg_dump: error: connection refused\n" {
		t.Fatalf("RunJob() output = %q, want the run's output", out)
	}
	if commands := runner.recorded(); len(commands) != 1 {
		t.Fatalf("commands = %+v, want the run alone, nothing removed", commands)
	}
}

// A transient job's output, which a backup receipt keeps, carries no terminal
// controls: a capture's colours and titles are dropped and its text kept.
func TestBackendRunJobDropsTerminalControlsFromATransientJobsOutput(t *testing.T) {
	runner := &jobRunner{answer: func(context.Context, []string) ([]byte, error) {
		return []byte("\x1b]0;db.internal\apg_dump: \x1b[31merror\x1b[0m: connection refused\n"), errors.New("exit status 1")
	}}
	out, err := Backend{Runner: runner}.RunJob(t.Context(), runtime.Target{Root: "/stack"}, runtime.JobSpec{Name: "capture", Configuration: []byte(captureConfiguration), Transient: true})
	if err == nil {
		t.Fatal("RunJob() error = nil, want the run's failure")
	}
	if string(out) != "pg_dump: error: connection refused\n" {
		t.Fatalf("RunJob() output = %q, want the run's text alone", out)
	}
}

// A transient job's output keeps its last maxJobOutputBytes, redacted before the
// cut, so the cut cannot leave the tail of a secret it went through.
func TestBackendRunJobKeepsTheRedactedTailOfATransientJobsOutput(t *testing.T) {
	run := func(output string) string {
		t.Helper()
		runner := &jobRunner{answer: func(context.Context, []string) ([]byte, error) { return []byte(output), nil }}
		out, err := Backend{Runner: runner}.RunJob(t.Context(), runtime.Target{Root: "/stack"}, runtime.JobSpec{Name: "capture", Configuration: []byte(captureConfiguration), Transient: true})
		if err != nil {
			t.Fatalf("RunJob() error = %v", err)
		}
		return string(out)
	}
	filler := strings.Repeat("x", maxJobOutputBytes)
	if got := run("head\n" + filler); got != filler {
		t.Fatalf("output = %d bytes starting %q, want the last %d", len(got), got[:min(len(got), 10)], maxJobOutputBytes)
	}
	secret := "compose-capture-secret-0123456789abcdefgh"
	logctx.RegisterSecrets(secret)
	got := run(secret + "\n" + filler[:maxJobOutputBytes-20])
	if strings.Contains(got, secret[len(secret)-10:]) || !strings.HasPrefix(got, "***\n") {
		t.Fatalf("output starts %q, want the secret redacted whole", got[:min(len(got), 50)])
	}
}

// Cancelling a transient job kills the docker client, which leaves the
// container running: RunJob removes the container by its name, with a context
// of its own, until the client has exited and once more after.
func TestBackendRunJobRemovesTheContainerOfACanceledTransientJob(t *testing.T) {
	interval := transientRemoveInterval
	transientRemoveInterval = 10 * time.Millisecond
	t.Cleanup(func() { transientRemoveInterval = interval })
	type removal struct {
		args      []string
		ctxErr    error
		afterExit bool
	}
	var (
		mu       sync.Mutex
		removals []removal
		exited   atomic.Bool
	)
	started, gone := make(chan struct{}), make(chan struct{})
	var once sync.Once
	runner := &jobRunner{
		answer: func(ctx context.Context, _ []string) ([]byte, error) {
			once.Do(func() { close(started) })
			<-ctx.Done()
			// An attached client holds on until its container is gone. One that
			// nothing removes would hold on for good, so the fake gives up.
			select {
			case <-gone:
			case <-time.After(5 * time.Second):
			}
			exited.Store(true)
			return []byte("partial\n"), errors.New("signal: killed")
		},
		remove: func(ctx context.Context, args []string) ([]byte, error) {
			mu.Lock()
			defer mu.Unlock()
			removals = append(removals, removal{args: args, ctxErr: ctx.Err(), afterExit: exited.Load()})
			// The first removal finds nothing, as when the container is still
			// being created; the second removes the one the client then started.
			if len(removals) == 2 {
				close(gone)
			}
			if len(removals) == 1 {
				return []byte("Error response from daemon: No such container: x"), errors.New("exit status 1")
			}
			return nil, nil
		},
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		<-started
		cancel()
	}()
	out, err := Backend{Runner: runner}.RunJob(ctx, runtime.Target{Root: "/stack"}, runtime.JobSpec{Name: "capture", Configuration: []byte(captureConfiguration), Transient: true})
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "container job capture failed") {
		t.Fatalf("RunJob() error = %v, want the cancellation naming the job", err)
	}
	if string(out) != "partial\n" {
		t.Fatalf("RunJob() output = %q, want what the run printed", out)
	}
	run := runner.recorded()[0].args
	container := run[slices.Index(run, "--name")+1]
	mu.Lock()
	defer mu.Unlock()
	if len(removals) < 3 || !removals[len(removals)-1].afterExit {
		t.Fatalf("removals = %+v, want removals until the client exited and one after", removals)
	}
	for _, removal := range removals {
		if !reflect.DeepEqual(removal.args, []string{"rm", "-f", container}) || removal.ctxErr != nil {
			t.Fatalf("removal = %+v, want rm -f %s under a live context", removal, container)
		}
	}
}

// A failed removal of a canceled job's container is reported beside the
// cancellation, since the container may still run.
func TestBackendRunJobReportsAFailedRemovalOfACanceledTransientJob(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runner := &jobRunner{
		answer: func(ctx context.Context, _ []string) ([]byte, error) {
			cancel()
			return nil, errors.New("signal: killed")
		},
		remove: func(context.Context, []string) ([]byte, error) {
			return []byte("Cannot connect to the Docker daemon"), errors.New("exit status 1")
		},
	}
	_, err := Backend{Runner: runner}.RunJob(ctx, runtime.Target{Root: "/stack"}, runtime.JobSpec{Name: "capture", Configuration: []byte(captureConfiguration), Transient: true})
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "remove container notes-1a2b3c4d-capture-run-") {
		t.Fatalf("RunJob() error = %v, want the cancellation and the failed removal", err)
	}
}
