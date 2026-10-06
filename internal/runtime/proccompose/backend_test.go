package proccompose

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ang-ee/angee-operator/internal/logctx"
	"github.com/ang-ee/angee-operator/internal/runtime"
)

func TestExecRunnerTracesCommandAndEnvironmentKeys(t *testing.T) {
	var logs bytes.Buffer
	ctx := logctx.With(t.Context(), slog.New(logctx.NewCLIHandler(&logs, slog.LevelDebug)))
	_, err := (ExecRunner{}).Run(ctx, "", []string{"API_TOKEN=env-secret"}, "sh", "-c", "exit 0", "--token", "secret")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	got := logs.String()
	if !strings.Contains(got, "exec sh -c exit 0 --token ***") ||
		!strings.Contains(got, "env=[API_TOKEN]") ||
		!strings.Contains(got, "exec finished duration=") {
		t.Fatalf("trace output = %q", got)
	}
	if strings.Contains(got, "secret") {
		t.Fatalf("trace output leaked secret data: %q", got)
	}
}

type recordingRunner struct {
	name string
	args []string
}

func TestProcessComposeBinaryPromptsAndInstalls(t *testing.T) {
	installed := false
	backend := Backend{
		Stdin: strings.NewReader("yes\n"),
		LookupPath: func(name string) (string, error) {
			if installed {
				return "/tmp/process-compose", nil
			}
			return "", errors.New("not found")
		},
		GoBinPath: func(context.Context) (string, error) {
			return "", errors.New("no gopath")
		},
		InstallProcessCompose: func(context.Context, io.Writer, io.Writer) error {
			installed = true
			return nil
		},
	}
	var stderr bytes.Buffer
	path, err := backend.processComposeBinary(context.Background(), backend.input(), io.Discard, &stderr, true)
	if err != nil {
		t.Fatalf("processComposeBinary() error = %v", err)
	}
	if path != "/tmp/process-compose" {
		t.Fatalf("path = %q, want /tmp/process-compose", path)
	}
	if !installed {
		t.Fatal("installer was not called")
	}
	if !strings.Contains(stderr.String(), "Install it now") {
		t.Fatalf("prompt = %q, want install prompt", stderr.String())
	}
}

func TestProcessComposeBinaryDeclineInstall(t *testing.T) {
	backend := Backend{
		Stdin: strings.NewReader("n\n"),
		LookupPath: func(name string) (string, error) {
			return "", errors.New("not found")
		},
		GoBinPath: func(context.Context) (string, error) {
			return "", errors.New("no gopath")
		},
	}
	_, err := backend.processComposeBinary(context.Background(), backend.input(), io.Discard, io.Discard, true)
	if err == nil || !strings.Contains(err.Error(), "process-compose is required") {
		t.Fatalf("error = %v, want process-compose required", err)
	}
}

func (r *recordingRunner) Run(_ context.Context, _ string, _ []string, name string, args ...string) ([]byte, error) {
	r.name = name
	r.args = append([]string(nil), args...)
	return nil, nil
}

// A command that leaves a background child holding its output pipe, as a
// daemonising `up -D` could, must not block Run for the child's lifetime.
func TestExecRunnerDoesNotWaitForDaemonPipes(t *testing.T) {
	started := time.Now()
	_, err := (ExecRunner{}).Run(t.Context(), "", nil, "sh", "-c", "sleep 8 & echo started")
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("Run() returned after %s, want it bounded by WaitDelay", elapsed)
	}
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("Run() error = %v, want exec.ErrWaitDelay", err)
	}
}

// With no supervisor on the control port, Up launches one. process-compose
// detaches with `-D`; `-d` is `--hide-disabled` and leaves the supervisor in the
// foreground, so Up would block. The daemon reports nothing back, so Up then
// waits until it answers, and a start slower than the first probes still
// succeeds.
func TestBackendUpCommand(t *testing.T) {
	port := freePort(t)
	project := newFakeProject("/stack", nil)
	project.onUp = func() { project.serveLater(t, port, 300*time.Millisecond) }
	backend := Backend{Runner: project, SupervisorReadyTimeout: 5 * time.Second}
	err := backend.Up(t.Context(), runtime.Target{Root: "/stack", Services: []string{"web"}, ControlPort: port})
	if err != nil {
		t.Fatalf("Up() error = %v", err)
	}
	want := [][]string{{"-f", "/stack/process-compose.yaml", "--address", "127.0.0.1", "--port", strconv.Itoa(port), "up", "-D", "--keep-project", "--tui=false", "web"}}
	if got := project.upCommands(); !reflect.DeepEqual(got, want) {
		t.Fatalf("up commands = %v, want %v", got, want)
	}
	if project.liveAnswers() == 0 {
		t.Fatal("Up() returned before the launched supervisor answered")
	}
}

// A detached daemon that dies, on a taken port for one, leaves `up -D` exiting
// 0 all the same. Up must report that the supervisor never answered and where
// its log is, instead of succeeding with nothing running.
func TestUpFailsWhenLaunchedSupervisorNeverAnswers(t *testing.T) {
	port := freePort(t)
	project := newFakeProject("/stack", nil)
	envFile := filepath.Join(t.TempDir(), "runtime.env")
	if err := os.WriteFile(envFile, []byte("PC_LOG_FILE=/srv/stack/run/process-compose.log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	backend := Backend{Runner: project, SupervisorReadyTimeout: 300 * time.Millisecond}
	err := backend.Up(t.Context(), runtime.Target{Root: "/stack", EnvFile: envFile, ControlPort: port})
	if err == nil {
		t.Fatal("Up() succeeded although no supervisor ever answered")
	}
	for _, want := range []string{"did not answer on 127.0.0.1:" + strconv.Itoa(port), "/srv/stack/run/process-compose.log"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Up() error = %v, want it to mention %q", err, want)
		}
	}
	if got := len(project.upCommands()); got != 1 {
		t.Fatalf("ran %d up commands; want the one launch", got)
	}
}

// A control port held by something other than a process-compose supervisor can
// be neither driven nor bound, so Up refuses it instead of launching a daemon
// that cannot listen.
func TestUpRefusesControlPortHeldByAnotherListener(t *testing.T) {
	tests := []struct {
		name   string
		listen func(t *testing.T) int
	}{
		{
			name: "http server",
			listen: func(t *testing.T) int {
				server := httptest.NewServer(http.NotFoundHandler())
				t.Cleanup(server.Close)
				return serverPort(t, server)
			},
		},
		{
			name: "listener that hangs up",
			listen: func(t *testing.T) int {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = listener.Close() })
				go func() {
					for {
						conn, err := listener.Accept()
						if err != nil {
							return
						}
						_ = conn.Close()
					}
				}()
				return listener.Addr().(*net.TCPAddr).Port
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			project := newFakeProject("/stack", nil)
			backend := Backend{Runner: project, SupervisorReadyTimeout: 300 * time.Millisecond}
			err := backend.Up(t.Context(), runtime.Target{Root: "/stack", ControlPort: tt.listen(t)})
			if err == nil || !strings.Contains(err.Error(), "is in use, but no process-compose supervisor answers on it") {
				t.Fatalf("Up() error = %v, want the control port reported as held by another listener", err)
			}
			if got := project.upCommands(); len(got) != 0 {
				t.Fatalf("up commands = %v, want none", got)
			}
		})
	}
}

// Regression test for the v0.17.0 `up -D` change. A second `up -D` against a
// running supervisor forks a daemon that dies on the taken port while the parent
// exits 0, so the named services were reported up and stayed Disabled. Up must
// start them through the running supervisor instead: a Disabled one after the
// Disabled dependency it waits for, a stopped one again, and a running one not
// at all. A dependency that has already run keeps its result and processes
// nobody asked for stay as they are.
func TestUpStartsNamedProcessesThroughRunningSupervisor(t *testing.T) {
	started := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	root := writeCompiledConfig(t, namedConfiguration)
	project := newFakeProject(root, map[string]processListEntry{
		"web":     {Status: "Running", IsRunning: true, PID: 100, ProcessStartTime: &started},
		"api":     {Status: "Completed", ExitCode: 1, PID: 101, ProcessStartTime: &started},
		"worker":  {Status: "Disabled"},
		"db":      {Status: "Disabled"},
		"migrate": {Status: "Completed", PID: 102, ProcessStartTime: &started},
		"other":   {Status: "Disabled"},
	})
	port := project.serve(t)
	backend := Backend{Runner: project}

	err := backend.Up(t.Context(), runtime.Target{Root: root, Services: []string{"web", "api", "worker"}, ControlPort: port})
	if err != nil {
		t.Fatalf("Up() error = %v", err)
	}
	if got := project.upCommands(); len(got) != 0 {
		t.Fatalf("up commands = %v, want none against a running supervisor", got)
	}
	// db and worker are updated and stay Disabled, so both are started, db
	// first; api's update relaunches it on its own.
	if got, want := project.startedProcesses(), []string{"db", "worker"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("started processes = %v, want %v", got, want)
	}
	if got, want := project.postedProcesses(), []string{"api", "db", "worker"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("updated processes = %v, want %v", got, want)
	}
	for name, want := range map[string]string{"web": "Running", "api": "Running", "worker": "Running", "db": "Running", "migrate": "Completed", "other": "Disabled"} {
		if got := project.state(name); got.Status != want {
			t.Fatalf("%s status = %q, want %q", name, got.Status, want)
		}
	}
	if pid := project.state("web").PID; pid != 100 {
		t.Fatalf("web pid = %d, want 100: a running process must not be restarted", pid)
	}
	if pid := project.state("migrate").PID; pid != 102 {
		t.Fatalf("migrate pid = %d, want 102: a dependency that already ran must not run again", pid)
	}
	if restarts := project.restartCount(); restarts != 0 {
		t.Fatalf("restarted %d processes; want none", restarts)
	}
}

// A service the stack defined after the supervisor started is unknown to it, and
// `process start` answers "no such process". Registering it would post the whole
// project, which process-compose takes as an update of every process its loader
// created, restarting the running ones. Up refuses instead, before it starts or
// updates anything, whether the process was named or is part of the project.
func TestUpRefusesProcessUnknownToRunningSupervisor(t *testing.T) {
	const configuration = "version: \"0.5\"\nprocesses:\n  web:\n    command: run-web\n  worker:\n    command: run-worker\n  api:\n    command: run-api\n"
	for _, services := range [][]string{{"worker", "api"}, nil} {
		t.Run(fmt.Sprintf("services %v", services), func(t *testing.T) {
			started := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
			project := newFakeProject("/stack", map[string]processListEntry{
				"web":    {Status: "Running", IsRunning: true, PID: 100, ProcessStartTime: &started},
				"worker": {Status: "Disabled"},
			})
			port := project.serve(t)
			backend := Backend{Runner: project}

			err := backend.Up(t.Context(), runtime.Target{Root: "/stack", Services: services, ControlPort: port, Configuration: []byte(configuration)})
			if err == nil || !strings.Contains(err.Error(), "has not loaded api, which the stack defined after it started") {
				t.Fatalf("Up() error = %v, want api reported as unknown to the running supervisor", err)
			}
			if got := project.writes(); len(got) != 0 {
				t.Fatalf("control-plane writes = %v, want none", got)
			}
			if got := project.startedProcesses(); len(got) != 0 {
				t.Fatalf("started processes = %v, want none", got)
			}
			if got := project.upCommands(); len(got) != 0 {
				t.Fatalf("up commands = %v, want none against a running supervisor", got)
			}
		})
	}
}

// Up with no process named against a running supervisor brings up what a fresh
// `up` would have run and the supervisor never has: the processes an earlier
// `up <process>...` left Disabled. A process that already ran keeps its result,
// and a running one is not restarted.
func TestUpWithoutServicesStartsProcessesTheSupervisorNeverRan(t *testing.T) {
	started := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	project := newFakeProject("/stack", map[string]processListEntry{
		"web":     {Status: "Running", IsRunning: true, PID: 100, ProcessStartTime: &started},
		"migrate": {Status: "Completed", PID: 102, ProcessStartTime: &started},
		"worker":  {Status: "Disabled"},
	})
	port := project.serve(t)
	backend := Backend{Runner: project}

	err := backend.Up(t.Context(), runtime.Target{
		Root: "/stack", ControlPort: port,
		Configuration: []byte("version: \"0.5\"\nprocesses:\n  web:\n    command: run-web\n  migrate:\n    command: run-migrate\n  worker:\n    command: run-worker\n"),
	})
	if err != nil {
		t.Fatalf("Up() error = %v", err)
	}
	if got := project.upCommands(); len(got) != 0 {
		t.Fatalf("up commands = %v, want none against a running supervisor", got)
	}
	if got, want := project.startedProcesses(), []string{"worker"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("started processes = %v, want %v", got, want)
	}
	for name, want := range map[string]string{"web": "Running", "migrate": "Completed", "worker": "Running"} {
		if got := project.state(name); got.Status != want {
			t.Fatalf("%s status = %q, want %q", name, got.Status, want)
		}
	}
	if web, migrate := project.state("web").PID, project.state("migrate").PID; web != 100 || migrate != 102 {
		t.Fatalf("web pid = %d and migrate pid = %d, want 100 and 102 untouched", web, migrate)
	}
}

// namedConfiguration is the compiled configuration of
// TestUpStartsNamedProcessesThroughRunningSupervisor.
const namedConfiguration = `version: "0.5"
processes:
  web:
    command: run-web
  api:
    command: run-api
  worker:
    command: run-worker
    depends_on:
      db:
        condition: process_started
      migrate:
        condition: process_completed_successfully
  db:
    command: run-db
  migrate:
    command: run-migrate
  other:
    command: run-other
`

// An update the supervisor does not act on leaves the process to be started
// explicitly: one whose definition already matches is not posted at all, and
// one the supervisor judges unchanged is posted and discarded. An update that
// leaves the process on its way to running without relaunching it as such
// needs no start, which `process start` would refuse.
func TestUpStartsProcessTheUpdateDidNotRun(t *testing.T) {
	tests := []struct {
		name      string
		state     processListEntry
		unchanged bool
		afterPost func(processListEntry) processListEntry
		posted    []string
		started   []string
	}{
		{
			name:      "definition unchanged",
			state:     processListEntry{Status: "Disabled"},
			unchanged: true,
			started:   []string{"api"},
		},
		{
			name:      "update discarded",
			state:     processListEntry{Status: "Completed", ExitCode: 1, PID: 101},
			afterPost: func(state processListEntry) processListEntry { return state },
			posted:    []string{"api"},
			started:   []string{"api"},
		},
		{
			name:      "update left it pending",
			state:     processListEntry{Status: "Disabled"},
			afterPost: func(processListEntry) processListEntry { return processListEntry{Name: "api", Status: "Pending"} },
			posted:    []string{"api"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			project := newFakeProject("/stack", map[string]processListEntry{"api": tt.state})
			if tt.unchanged {
				project.processes["api"].config = map[string]any{"name": "api", "command": "run-api", "environment": nil, "workingDir": "", "dependsOn": nil, "readinessProbe": nil}
			}
			project.processes["api"].afterPost = tt.afterPost
			port := project.serve(t)
			backend := Backend{Runner: project}

			err := backend.Up(t.Context(), runtime.Target{
				Root: "/stack", Services: []string{"api"}, ControlPort: port,
				Configuration: []byte("version: \"0.5\"\nprocesses:\n  api:\n    command: run-api\n"),
			})
			if err != nil {
				t.Fatalf("Up() error = %v", err)
			}
			if got := project.postedProcesses(); !slices.Equal(got, tt.posted) {
				t.Fatalf("updated processes = %v, want %v", got, tt.posted)
			}
			if got := project.startedProcesses(); !slices.Equal(got, tt.started) {
				t.Fatalf("started processes = %v, want %v", got, tt.started)
			}
		})
	}
}

// A supervisor on the control port that runs another stack's configuration, as
// when two stacks share the port, is neither driven nor taken for the one Up
// launched: Up would post this stack's definitions into it and report its
// processes as this stack's.
func TestUpRefusesSupervisorOfAnotherStack(t *testing.T) {
	const configuration = "version: \"0.5\"\nprocesses:\n  web:\n    command: run-web\n"
	t.Run("already running", func(t *testing.T) {
		project := newFakeProject("/other", map[string]processListEntry{"web": {Status: "Disabled"}})
		port := project.serve(t)
		backend := Backend{Runner: project}
		err := backend.Up(t.Context(), runtime.Target{Root: "/stack", Services: []string{"web"}, ControlPort: port, Configuration: []byte(configuration)})
		if err == nil || !strings.Contains(err.Error(), "runs /other/process-compose.yaml, not this stack's /stack/process-compose.yaml") {
			t.Fatalf("Up() error = %v, want the supervisor reported as another stack's", err)
		}
		if writes, starts := project.writes(), project.startedProcesses(); len(writes) != 0 || len(starts) != 0 {
			t.Fatalf("control-plane writes = %v and started processes = %v, want none", writes, starts)
		}
	})
	t.Run("answering the launch", func(t *testing.T) {
		port := freePort(t)
		project := newFakeProject("/other", nil)
		project.onUp = func() { project.serveLater(t, port, 0) }
		backend := Backend{Runner: project, SupervisorReadyTimeout: 5 * time.Second}
		err := backend.Up(t.Context(), runtime.Target{Root: "/stack", ControlPort: port})
		if err == nil || !strings.Contains(err.Error(), "runs /other/process-compose.yaml") {
			t.Fatalf("Up() error = %v, want the supervisor reported as another stack's", err)
		}
	})
}

// The supervisor reports its config file exactly as it was passed to -f, so a
// stack reached through a symlink must still recognise its own supervisor.
func TestUpRecognisesItsSupervisorThroughAnotherPath(t *testing.T) {
	root := writeCompiledConfig(t, "version: \"0.5\"\nprocesses:\n  web:\n    command: run-web\n")
	link := filepath.Join(t.TempDir(), "stack")
	if err := os.Symlink(root, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	project := newFakeProject(root, map[string]processListEntry{"web": {Status: "Disabled"}})
	port := project.serve(t)
	backend := Backend{Runner: project}
	if err := backend.Up(t.Context(), runtime.Target{Root: link, Services: []string{"web"}, ControlPort: port}); err != nil {
		t.Fatalf("Up() error = %v", err)
	}
	if got := project.state("web").Status; got != "Running" {
		t.Fatalf("web status = %q, want Running", got)
	}
}

func TestUpOrderPutsDependenciesFirst(t *testing.T) {
	document := File{Processes: map[string]Process{
		"web":     {DependsOn: map[string]ProcessDependency{"api": {}, "postgres": {}}},
		"api":     {DependsOn: map[string]ProcessDependency{"migrate": {}}},
		"migrate": {DependsOn: map[string]ProcessDependency{"api": {}}},
		"other":   {},
	}}
	got, err := upOrder(document, []string{"web"})
	if err != nil {
		t.Fatalf("upOrder() error = %v", err)
	}
	// postgres is a container service; the cycle between api and migrate is
	// visited once.
	if want := []string{"migrate", "api", "web"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("upOrder() = %v, want %v", got, want)
	}
	got, err = upOrder(document, nil)
	if err != nil {
		t.Fatalf("upOrder() error = %v", err)
	}
	if want := []string{"migrate", "api", "other", "web"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("upOrder() without names = %v, want %v", got, want)
	}
	if _, err := upOrder(document, []string{"missing"}); err == nil || !strings.Contains(err.Error(), "absent from the generated process-compose config") {
		t.Fatalf("upOrder() error = %v, want a missing-process error", err)
	}
}

// fakeProject stands in for a process-compose supervisor running several
// processes, as fakeSupervisor does for one: its control plane over HTTP and its
// CLI as a Runner. It answers process-compose's liveness reply on GET /live and
// reports the config file it was started with on GET /project/state. GET
// /process/info/<name> serves a process's native config, which carries
// `disabled` for a Disabled process, and GET /process/<name> its live state;
// both answer the real supervisor's 400 bodies for a process it does not know.
// POST /process applies every update the way process-compose applies a changed
// one: it replaces the process with one that stays Disabled when the posted
// config is disabled and runs under a new pid otherwise, unless the process's
// afterPost decides its state instead. Every other request is not found, and
// every write is recorded. `process start` runs a process that is Disabled or
// has ended and refuses one that is active or unknown, as process-compose does.
// An `up` is recorded and handed to onUp.
type fakeProject struct {
	mu        sync.Mutex
	fileNames []string
	processes map[string]*fakeProcess
	nextPID   int
	writeLog  []string
	posted    []string
	ups       [][]string
	starts    []string
	restarts  int
	live      int
	onUp      func()
}

type fakeProcess struct {
	config map[string]any
	state  processListEntry
	// afterPost, when set, returns the process's state after a posted update
	// from its state before it.
	afterPost func(processListEntry) processListEntry
}

// newFakeProject returns a fake supervisor started on root's
// process-compose.yaml, holding the given process states. Each process's native
// config differs from any compiled definition, so Up posts an update for a
// process it starts.
func newFakeProject(root string, states map[string]processListEntry) *fakeProject {
	project := &fakeProject{
		fileNames: []string{filepath.Join(root, "process-compose.yaml")},
		processes: map[string]*fakeProcess{},
		nextPID:   1000,
	}
	for name, state := range states {
		state.Name = name
		config := map[string]any{"name": name, "command": "old"}
		if state.Status == "Disabled" {
			config["disabled"] = true
		}
		project.processes[name] = &fakeProcess{config: config, state: state}
	}
	return project
}

// serve starts answering the control plane on a loopback port until the test
// ends, and returns the port.
func (p *fakeProject) serve(t *testing.T) int {
	t.Helper()
	server := httptest.NewServer(p)
	t.Cleanup(server.Close)
	return serverPort(t, server)
}

// serveLater starts answering the control plane on 127.0.0.1:port after delay,
// as a supervisor that is slow to start does. The server is closed when the
// test ends, and a port taken meanwhile fails the test there rather than as a
// supervisor that never answered.
func (p *fakeProject) serveLater(t *testing.T, port int, delay time.Duration) {
	t.Helper()
	var (
		mu        sync.Mutex
		server    *httptest.Server
		listenErr error
		stopped   bool
	)
	timer := time.AfterFunc(delay, func() {
		listener, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			listenErr = err
			return
		}
		if stopped {
			_ = listener.Close()
			return
		}
		server = httptest.NewUnstartedServer(p)
		_ = server.Listener.Close()
		server.Listener = listener
		server.Start()
	})
	t.Cleanup(func() {
		timer.Stop()
		mu.Lock()
		defer mu.Unlock()
		stopped = true
		if listenErr != nil {
			t.Errorf("fake supervisor could not listen on control port %d: %v", port, listenErr)
		}
		if server != nil {
			server.Close()
		}
	})
}

func (p *fakeProject) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	route := r.Method + " " + r.URL.Path
	if r.Method != http.MethodGet {
		p.writeLog = append(p.writeLog, route)
	}
	switch {
	case route == "GET /live":
		p.live++
		_, _ = io.WriteString(w, `{"status":"alive"}`)
	case route == "GET /project/state":
		_ = json.NewEncoder(w).Encode(map[string]any{"fileNames": p.fileNames})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/process/info/"):
		process, ok := p.processes[strings.TrimPrefix(r.URL.Path, "/process/info/")]
		if !ok {
			http.Error(w, `{"error":"no such process"}`, http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(process.config)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/process/"):
		process, ok := p.processes[strings.TrimPrefix(r.URL.Path, "/process/")]
		if !ok {
			http.Error(w, `{"error":"can't get state of process: no such process"}`, http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(process.state)
	case route == "POST /process":
		var config map[string]any
		if err := json.NewDecoder(r.Body).Decode(&config); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		name, _ := config["name"].(string)
		process, ok := p.processes[name]
		if !ok {
			http.Error(w, `{"error":"no such process: `+name+`"}`, http.StatusBadRequest)
			return
		}
		p.posted = append(p.posted, name)
		process.config = config
		switch disabled, _ := config["disabled"].(bool); {
		case process.afterPost != nil:
			process.state = process.afterPost(process.state)
		case disabled:
			process.state = processListEntry{Name: name, Status: "Disabled"}
		default:
			process.state = p.launched(name)
		}
		_, _ = io.WriteString(w, `{}`)
	default:
		http.NotFound(w, r)
	}
}

// launched returns the state of a new incarnation of the named process. The
// caller holds p.mu.
func (p *fakeProject) launched(name string) processListEntry {
	p.nextPID++
	now := time.Now()
	return processListEntry{Name: name, Status: "Running", IsRunning: true, PID: p.nextPID, ProcessStartTime: &now}
}

func (p *fakeProject) Run(_ context.Context, _ string, _ []string, _ string, args ...string) ([]byte, error) {
	if hasArg(args, "up") {
		p.mu.Lock()
		p.ups = append(p.ups, append([]string(nil), args...))
		onUp := p.onUp
		p.mu.Unlock()
		if onUp != nil {
			onUp()
		}
		return nil, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case hasArg(args, "start"):
		name := args[len(args)-1]
		process, ok := p.processes[name]
		if !ok {
			return nil, errors.New("no such process: " + name)
		}
		// process-compose refuses every process it still holds as running:
		// all but those Disabled or ended.
		switch process.state.Status {
		case "Disabled", "Completed", "Error", "Skipped":
		default:
			return nil, errors.New("process " + name + " is already running")
		}
		p.starts = append(p.starts, name)
		process.state = p.launched(name)
	case hasArg(args, "restart"):
		p.restarts++
	}
	return nil, nil
}

func (p *fakeProject) state(name string) processListEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	if process, ok := p.processes[name]; ok {
		return process.state
	}
	return processListEntry{}
}

func (p *fakeProject) upCommands() [][]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.ups)
}

func (p *fakeProject) startedProcesses() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.starts)
}

func (p *fakeProject) postedProcesses() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.posted)
}

func (p *fakeProject) writes() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.writeLog)
}

func (p *fakeProject) restartCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.restarts
}

func (p *fakeProject) liveAnswers() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.live
}

// freePort returns a loopback port nothing listens on, so a connection to it is
// refused until a test starts serving there.
func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

// writeCompiledConfig writes a generated process-compose.yaml into a new stack
// root and returns the root.
func writeCompiledConfig(t *testing.T, config string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "process-compose.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

// The foreground supervisor of `angee dev` also outlives its processes, so a
// stack whose processes all completed or were skipped keeps the states `angee
// dev` reports.
func TestBackendUpForegroundKeepsProject(t *testing.T) {
	got := Backend{}.upArgs(runtime.Target{Root: "/stack", ControlPort: 10002}, false)
	want := []string{"-f", "/stack/process-compose.yaml", "--address", "127.0.0.1", "--port", "10002", "up", "--keep-project", "--tui=false"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("foreground up args = %v, want %v", got, want)
	}
}

func TestBackendStreamLogsTail(t *testing.T) {
	runner := &recordingRunner{}
	backend := Backend{Runner: runner}
	if _, err := backend.StreamLogs(context.Background(), runtime.LogsRequest{
		Root: "/stack", Services: []string{"web"}, Follow: true, Tail: 50, ControlPort: 10004,
	}); err != nil {
		t.Fatalf("StreamLogs() error = %v", err)
	}
	want := []string{"--address", "127.0.0.1", "--port", "10004", "process", "logs", "--follow", "--tail", "50", "web"}
	if runner.name != "process-compose" || !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("command = %s %v, want process-compose %v", runner.name, runner.args, want)
	}
}

func TestBackendDownUsesControlPort(t *testing.T) {
	runner := &recordingRunner{}
	backend := Backend{Runner: runner}
	err := backend.Down(context.Background(), runtime.Target{Root: "/stack", ControlPort: 10003})
	if err != nil {
		t.Fatalf("Down() error = %v", err)
	}
	want := []string{"--address", "127.0.0.1", "--port", "10003", "down"}
	if runner.name != "process-compose" || !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("command = %s %v, want process-compose %v", runner.name, runner.args, want)
	}
}

type stubListRunner struct {
	args   []string
	output []byte
	err    error
}

func (r *stubListRunner) Run(_ context.Context, _ string, _ []string, _ string, args ...string) ([]byte, error) {
	r.args = append([]string(nil), args...)
	return r.output, r.err
}

func TestBackendStatusParsesProcessList(t *testing.T) {
	const payload = `[
	{"name":"build-watch","status":"Running","is_running":true,"exit_code":0,"is_ready":"-"},
	{"name":"web","status":"Running","is_running":true,"exit_code":0,"is_ready":"Ready"},
	{"name":"migrate","status":"Completed","is_running":false,"exit_code":0,"is_ready":"-"}
]`
	runner := &stubListRunner{output: []byte(payload)}
	backend := Backend{Runner: runner}
	got, err := backend.Status(context.Background(), runtime.StatusRequest{Root: "/stack", ControlPort: 10004})
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	wantArgs := []string{"--address", "127.0.0.1", "--port", "10004", "list", "-o", "json"}
	if !reflect.DeepEqual(runner.args, wantArgs) {
		t.Fatalf("args = %v, want %v", runner.args, wantArgs)
	}
	want := []runtime.ServiceStatus{
		{Name: "build-watch", Runtime: "local", State: "running", ExitCode: intPointer(0), Generation: "0:<nil>:<nil>"},
		{Name: "web", Runtime: "local", State: "running", Health: "healthy", ExitCode: intPointer(0), Generation: "0:<nil>:<nil>"},
		{Name: "migrate", Runtime: "local", State: "completed", ExitCode: intPointer(0), Generation: "0:<nil>:<nil>"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("statuses = %#v, want %#v", got, want)
	}
}

func TestProcessUpdatesIncludesShutdownTimeout(t *testing.T) {
	updates, err := processUpdates(Process{Shutdown: &Shutdown{TimeoutSeconds: 30, Signal: 15}})
	if err != nil {
		t.Fatalf("processUpdates() error = %v", err)
	}
	shutdown, ok := updates["shutDownParams"].(*Shutdown)
	if !ok || shutdown.TimeoutSeconds != 30 || shutdown.Signal != 15 {
		t.Fatalf("shutDownParams = %#v, want timeout 30 and SIGTERM", updates["shutDownParams"])
	}
}

func TestProcessUpdatesPreservesShutdownWhenGraceIsOmitted(t *testing.T) {
	updates, err := processUpdates(Process{})
	if err != nil {
		t.Fatalf("processUpdates() error = %v", err)
	}
	if _, ok := updates["shutDownParams"]; ok {
		t.Fatalf("processUpdates() included shutDownParams: %#v", updates["shutDownParams"])
	}
}

func TestAddMissingProcessSuppliesNativeIdentityDefaults(t *testing.T) {
	var added map[string]any
	var activated map[string]any
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch {
		case r.URL.Path == "/processes":
			_, _ = io.WriteString(w, `{"data":[]}`)
		case r.URL.Path == "/project":
			var project struct {
				Processes map[string]map[string]any `json:"processes"`
			}
			if err := json.NewDecoder(r.Body).Decode(&project); err != nil {
				t.Errorf("decode project: %v", err)
			}
			added = project.Processes["job"]
			_, _ = io.WriteString(w, `{}`)
		case r.Method == http.MethodGet && r.URL.Path == "/process/info/job":
			_ = json.NewEncoder(w).Encode(added)
		case r.Method == http.MethodPost && r.URL.Path == "/process":
			if err := json.NewDecoder(r.Body).Decode(&activated); err != nil {
				t.Errorf("decode activation: %v", err)
			}
			_, _ = io.WriteString(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	_, portText, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portText)
	_, err = (Backend{}).addMissingProcess(t.Context(), runtime.Target{ControlPort: port}, "job", map[string]any{"command": "run"})
	if err != nil {
		t.Fatalf("addMissingProcess() error = %v", err)
	}
	if added["name"] != "job" || added["replicaName"] != "job" || added["namespace"] != "default" || added["replicas"] != float64(1) || added["launchTimeout"] != float64(5) {
		t.Fatalf("added native identity/defaults = %#v", added)
	}
	if shutdown := added["shutDownParams"].(map[string]any); shutdown["signal"] != float64(15) {
		t.Fatalf("added shutdown defaults = %#v", shutdown)
	}
	if added["disabled"] != true || activated["disabled"] != false || activated["command"] != "run" {
		t.Fatalf("bootstrap payloads = added %#v activated %#v", added, activated)
	}
	wantRequests := []string{"GET /processes", "POST /project", "GET /process/info/job", "POST /process"}
	if !reflect.DeepEqual(requests, wantRequests) {
		t.Fatalf("requests = %v, want %v", requests, wantRequests)
	}
}

func TestUpdateProcessPreservesNativeShutdownFields(t *testing.T) {
	var posted map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/process/info/worker":
			_, _ = io.WriteString(w, `{"name":"worker","replicaName":"worker","command":"old","shutDownParams":{"shutDownCommand":"cleanup","signal":2,"parentOnly":true}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/process/worker":
			_, _ = io.WriteString(w, `{"status":"Completed","is_running":false}`)
		case r.Method == http.MethodPost && r.URL.Path == "/process":
			if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
				t.Errorf("decode process: %v", err)
			}
			_, _ = io.WriteString(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	_, portText, _ := net.SplitHostPort(server.Listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	changed, err := (Backend{}).updateProcess(t.Context(), runtime.Target{ControlPort: port}, "worker", map[string]any{
		"command": "new", "shutDownParams": &Shutdown{TimeoutSeconds: 30, Signal: 15},
	})
	if err != nil || !changed {
		t.Fatalf("updateProcess() = %v, %v", changed, err)
	}
	shutdown := posted["shutDownParams"].(map[string]any)
	if shutdown["shutDownCommand"] != "cleanup" || shutdown["signal"] != float64(2) || shutdown["parentOnly"] != true || shutdown["shutDownTimeout"] != float64(30) {
		t.Fatalf("posted shutdown = %#v", shutdown)
	}
}

func TestUpdateProcessRefusesUnboundedRunningLegacyProcess(t *testing.T) {
	posted := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/process/info/worker":
			_, _ = io.WriteString(w, `{"name":"worker","replicaName":"worker","command":"old","shutDownParams":{"signal":15}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/process/worker":
			_, _ = io.WriteString(w, `{"status":"Terminating","is_running":false}`)
		case r.Method == http.MethodPost && r.URL.Path == "/process":
			posted = true
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	_, portText, _ := net.SplitHostPort(server.Listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	_, err := (Backend{}).updateProcess(t.Context(), runtime.Target{ControlPort: port}, "worker", map[string]any{
		"command": "new", "shutDownParams": &Shutdown{TimeoutSeconds: 30, Signal: 15},
	})
	if err == nil || !strings.Contains(err.Error(), "active without a shutdown timeout") {
		t.Fatalf("updateProcess() error = %v", err)
	}
	if posted {
		t.Fatal("updateProcess posted unsafe legacy update")
	}
}

// process-compose prints an ANSI-coloured "new version available" notice to
// stderr, which CombinedOutput folds in ahead of the JSON array. Its escape
// codes (e.g. "\x1b[33m") contain a '[', so a naive "trim to the first '['"
// would slice into the banner and fail to parse — leaving every local service
// reported as "declared". Status must still read the real array.
func TestBackendStatusParsesProcessListWithANSIBanner(t *testing.T) {
	const jsonPart = `[
	{"name":"storybook","status":"Running","is_running":true,"exit_code":0,"is_ready":"-"}
]`
	payload := "\n\x1b[33mInfo:\x1b[0m New version available: v1.120.0 -> v1.122.0. Run 'process-compose version update'\n" + jsonPart
	runner := &stubListRunner{output: []byte(payload)}
	backend := Backend{Runner: runner}
	got, err := backend.Status(context.Background(), runtime.StatusRequest{Root: "/stack", ControlPort: 8080})
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	want := []runtime.ServiceStatus{
		{Name: "storybook", Runtime: "local", State: "running", ExitCode: intPointer(0), Generation: "0:<nil>:<nil>"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("statuses = %#v, want %#v", got, want)
	}
}

func TestBackendStatusIncludesNativeGeneration(t *testing.T) {
	const payload = `[
	{"name":"migrate","status":"Completed","exit_code":0,"restarts":2,"process_start_time":"2026-09-13T10:00:00Z","process_end_time":"2026-09-13T10:00:01Z"}
]`
	backend := Backend{Runner: &stubListRunner{output: []byte(payload)}}
	got, err := backend.Status(context.Background(), runtime.StatusRequest{Root: "/stack", ControlPort: 8080})
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if len(got) != 1 || got[0].ExitCode == nil || *got[0].ExitCode != 0 {
		t.Fatalf("statuses = %#v, want completed zero-exit process", got)
	}
	wantGeneration := "2:2026-09-13 10:00:00 +0000 UTC:2026-09-13 10:00:01 +0000 UTC"
	if got[0].Generation != wantGeneration {
		t.Fatalf("generation = %q, want %q", got[0].Generation, wantGeneration)
	}
}

func TestRunLimitedReturnsOnlyProcessLogStdout(t *testing.T) {
	backend := Backend{LookupPath: func(string) (string, error) { return "/bin/sh", nil }}
	out, err := backend.runLimited(context.Background(), t.TempDir(), "", 1024, "-c", "printf 'job-output\\n'; printf 'update-notice\\n' >&2")
	if err != nil {
		t.Fatalf("runLimited() error = %v", err)
	}
	if got, want := string(out), "job-output\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func intPointer(value int) *int { return &value }

func TestBackendStatusPropagatesErrors(t *testing.T) {
	wantErr := errors.New("supervisor offline")
	runner := &stubListRunner{err: wantErr}
	backend := Backend{Runner: runner}
	got, err := backend.Status(context.Background(), runtime.StatusRequest{Root: "/stack", ControlPort: 10005})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Status() error = %v, want %v", err, wantErr)
	}
	if got != nil {
		t.Fatalf("statuses = %v, want nil", got)
	}
}

// scriptedJobRunner replays a fixed sequence of `list -o json` payloads (one per
// poll) and a canned log body, so a RunJob test can walk a process through its
// launch lifecycle without a live supervisor. It counts `process restart`
// invocations so a test can tell which trigger spawned the re-run.
type scriptedJobRunner struct {
	lists    [][]byte
	logs     []byte
	listIdx  int
	restarts int
}

func (r *scriptedJobRunner) Run(_ context.Context, _ string, _ []string, _ string, args ...string) ([]byte, error) {
	if hasArg(args, "logs") {
		return r.logs, nil
	}
	if hasArg(args, "list") {
		out := r.lists[r.listIdx]
		if r.listIdx < len(r.lists)-1 {
			r.listIdx++
		}
		return out, nil
	}
	if hasArg(args, "restart") {
		r.restarts++
	}
	return nil, nil
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// A re-run driven through UpdateProcess goes Pending -> Launching -> Launched ->
// Running -> Completed, and ProcessStartTime (part of the generation string)
// advances at launch, long before the process actually runs. The old gate treated
// every non-running/non-pending state as terminal, so a poll landing in the launch
// window returned exit 0 with stale logs. RunJob must instead wait for a genuine
// terminal state (Completed/Error/Skipped) whose end time is fresh.
func TestRunJobReportsCompletionNotLaunchWindow(t *testing.T) {
	server := replacingJobServer(t)
	defer server.Close()
	port := serverPort(t, server)

	runner := &scriptedJobRunner{
		logs: []byte("job log line\n"),
		lists: [][]byte{
			// baseline: the previous run's completion, captured before the re-run.
			[]byte(`[{"name":"job","status":"Completed","exit_code":0,"process_start_time":"2026-09-13T08:55:00Z","process_end_time":"2026-09-13T09:00:00Z"}]`),
			// pending: the re-run is queued; no end time yet.
			[]byte(`[{"name":"job","status":"Pending","exit_code":0}]`),
			// launching: ProcessStartTime (and the generation) has already advanced
			// with a stale exit_code 0 — the sample the old gate reported as success.
			[]byte(`[{"name":"job","status":"Launching","exit_code":0,"process_start_time":"2026-09-13T09:04:00Z"}]`),
			// running: still transitional, still not terminal.
			[]byte(`[{"name":"job","status":"Running","is_running":true,"exit_code":0,"process_start_time":"2026-09-13T09:04:00Z"}]`),
			// completed: a genuinely new terminal sample with a fresh end time.
			[]byte(`[{"name":"job","status":"Completed","exit_code":1,"process_start_time":"2026-09-13T09:04:00Z","process_end_time":"2026-09-13T09:05:00Z"}]`),
		},
	}
	backend := Backend{Runner: runner}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	out, err := backend.RunJob(ctx, runtime.Target{ControlPort: port}, runtime.JobSpec{
		Name:          "job",
		Configuration: []byte("version: \"0.5\"\nprocesses:\n  job:\n    command: run\n"),
	})
	if err == nil {
		t.Fatalf("RunJob() reported success from a launch-window sample; want exit-1 completion, out=%q", out)
	}
	if !strings.Contains(err.Error(), "exited with status 1") {
		t.Fatalf("RunJob() error = %v, want exit status 1", err)
	}
	if got := string(out); !strings.Contains(got, "job log line") {
		t.Fatalf("RunJob() logs = %q, want captured job logs", got)
	}
	if runner.listIdx < len(runner.lists)-1 {
		t.Fatalf("RunJob() consumed %d list samples; want it to poll past the launch window", runner.listIdx)
	}
	if runner.restarts != 0 {
		t.Fatalf("RunJob() restarted the job %d times; want the applied update to be its only trigger", runner.restarts)
	}
}

// replacingJobServer stands in for a supervisor whose stored config differs from
// the compiled updates for `command: run` and which applies the posted update
// the way process-compose does, by replacing the process: its state shows the
// previous run until the POST and a blank, not yet launched incarnation after
// it. RunJob therefore re-runs the job through the update path, and a Restart on
// top of it would run the job twice.
func replacingJobServer(t *testing.T) *httptest.Server {
	t.Helper()
	var replaced atomic.Bool
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/process/info/job":
			_, _ = io.WriteString(w, `{"name":"job","command":"old"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/process/job":
			if replaced.Load() {
				_, _ = io.WriteString(w, `{"name":"job","status":"Pending","pid":0}`)
				return
			}
			_, _ = io.WriteString(w, `{"name":"job","status":"Completed","pid":100,"process_start_time":"2026-09-13T08:55:00Z","process_end_time":"2026-09-13T09:00:00Z"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/process":
			replaced.Store(true)
			_, _ = io.WriteString(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
}

// restartFallbackServer stands in for a supervisor whose stored config already
// equals the compiled updates for `command: run`, so updateProcess reports
// changed=false and RunJob falls back to Restart. A POST to /process would mean
// updateProcess wrongly detected a change and skipped the restart path.
func restartFallbackServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/process/info/job":
			_, _ = io.WriteString(w, `{"name":"job","command":"run","workingDir":""}`)
		case r.Method == http.MethodPost && r.URL.Path == "/process":
			t.Errorf("updateProcess posted an update; want changed=false restart fallback")
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
}

func serverPort(t *testing.T, server *httptest.Server) int {
	t.Helper()
	_, portText, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

var jobConfiguration = []byte("version: \"0.5\"\nprocesses:\n  job:\n    command: run\n")

// On the Restart fallback (compiled config unchanged) process-compose reuses the
// existing ProcessState and never resets ProcessEndTime (src/app/process.go:570-572),
// so the re-run's completion carries the PREVIOUS run's end time. The end-time gate
// alone would then poll until the caller's deadline. RunJob must instead recognise the
// fresh completion from the new pid (process.go:153 assigns pid on every launch) or
// from having observed a transitional state.
func TestRunJobDetectsRestartPathCompletionWithUnchangedEndTime(t *testing.T) {
	server := restartFallbackServer(t)
	defer server.Close()
	port := serverPort(t, server)

	runner := &scriptedJobRunner{
		logs: []byte("job log line\n"),
		lists: [][]byte{
			// baseline: the previous run's completion, captured before the re-run.
			[]byte(`[{"name":"job","status":"Completed","exit_code":0,"pid":100,"process_start_time":"2026-09-13T08:55:00Z","process_end_time":"2026-09-13T09:00:00Z"}]`),
			// running: the reused state still carries the old end time; only the pid
			// has advanced. Transitional, so it is not read as a completion.
			[]byte(`[{"name":"job","status":"Running","is_running":true,"pid":200,"process_start_time":"2026-09-13T08:55:00Z","process_end_time":"2026-09-13T09:00:00Z"}]`),
			// completed: same end time as the baseline, but a new pid (and a
			// transitional state already seen) prove this is the re-run's completion.
			[]byte(`[{"name":"job","status":"Completed","exit_code":0,"pid":200,"process_start_time":"2026-09-13T08:55:00Z","process_end_time":"2026-09-13T09:00:00Z"}]`),
		},
	}
	backend := Backend{Runner: runner}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	out, err := backend.RunJob(ctx, runtime.Target{ControlPort: port}, runtime.JobSpec{
		Name:          "job",
		Configuration: jobConfiguration,
	})
	if err != nil {
		t.Fatalf("RunJob() error = %v, want success; out=%q", err, out)
	}
	if got := string(out); !strings.Contains(got, "job log line") {
		t.Fatalf("RunJob() logs = %q, want captured job logs", got)
	}
	if runner.listIdx < len(runner.lists)-1 {
		t.Fatalf("RunJob() consumed %d list samples; want it to poll to the fresh completion", runner.listIdx)
	}
}

// A re-run on the Restart path can finish before the first poll, so no transitional
// state is ever observed and the end time still equals the baseline. The changed pid
// is then the only proof that this terminal sample belongs to the re-run.
func TestRunJobDetectsFastRestartCompletionByPid(t *testing.T) {
	server := restartFallbackServer(t)
	defer server.Close()
	port := serverPort(t, server)

	runner := &scriptedJobRunner{
		logs: []byte("job log line\n"),
		lists: [][]byte{
			// baseline: the previous run's completion.
			[]byte(`[{"name":"job","status":"Completed","exit_code":0,"pid":100,"process_start_time":"2026-09-13T08:55:00Z","process_end_time":"2026-09-13T09:00:00Z"}]`),
			// completed before the first poll: unchanged end time, no transitional
			// state seen — only the new pid marks it as the re-run's completion.
			[]byte(`[{"name":"job","status":"Completed","exit_code":3,"pid":200,"process_start_time":"2026-09-13T08:55:00Z","process_end_time":"2026-09-13T09:00:00Z"}]`),
		},
	}
	backend := Backend{Runner: runner}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	out, err := backend.RunJob(ctx, runtime.Target{ControlPort: port}, runtime.JobSpec{
		Name:          "job",
		Configuration: jobConfiguration,
	})
	if err == nil {
		t.Fatalf("RunJob() reported success; want exit-3 completion, out=%q", out)
	}
	if !strings.Contains(err.Error(), "exited with status 3") {
		t.Fatalf("RunJob() error = %v, want exit status 3", err)
	}
	if runner.listIdx < len(runner.lists)-1 {
		t.Fatalf("RunJob() consumed %d list samples; want it to reach the fresh completion", runner.listIdx)
	}
}

// A terminal sample carrying the baseline's end time and pid, seen without any
// transitional state, is a stale leftover of the previous run and must be skipped.
// Only a sample bearing a genuinely new end time (the update path, here) counts.
func TestRunJobIgnoresStaleTerminalSample(t *testing.T) {
	// The stored command differs from the compiled "run", so updateProcess posts
	// an update and the re-run goes through the update path (a fresh end time).
	server := replacingJobServer(t)
	defer server.Close()
	port := serverPort(t, server)

	runner := &scriptedJobRunner{
		logs: []byte("job log line\n"),
		lists: [][]byte{
			// baseline: the previous run's completion.
			[]byte(`[{"name":"job","status":"Completed","exit_code":0,"pid":100,"process_start_time":"2026-09-13T08:55:00Z","process_end_time":"2026-09-13T09:00:00Z"}]`),
			// stale: identical end time and pid, no transitional seen — must be skipped.
			[]byte(`[{"name":"job","status":"Completed","exit_code":0,"pid":100,"process_start_time":"2026-09-13T08:55:00Z","process_end_time":"2026-09-13T09:00:00Z"}]`),
			// fresh: a new end time proves this is the re-run's completion.
			[]byte(`[{"name":"job","status":"Completed","exit_code":0,"pid":100,"process_start_time":"2026-09-13T08:55:00Z","process_end_time":"2026-09-13T09:05:00Z"}]`),
		},
	}
	backend := Backend{Runner: runner}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	out, err := backend.RunJob(ctx, runtime.Target{ControlPort: port}, runtime.JobSpec{
		Name:          "job",
		Configuration: jobConfiguration,
	})
	if err != nil {
		t.Fatalf("RunJob() error = %v, want success; out=%q", err, out)
	}
	if got := string(out); !strings.Contains(got, "job log line") {
		t.Fatalf("RunJob() logs = %q, want captured job logs", got)
	}
	if runner.listIdx < len(runner.lists)-1 {
		t.Fatalf("RunJob() consumed %d list samples; want the stale sample skipped", runner.listIdx)
	}
	if runner.restarts != 0 {
		t.Fatalf("RunJob() restarted the job %d times; want the applied update to be its only trigger", runner.restarts)
	}
}

// A supervisor that judges a posted update equal to the config it already holds
// answers 200 and runs nothing. RunJob used to take the accepted POST for the
// re-run and then wait out its deadline for a completion that never came; it
// must notice that the job was not relaunched and restart it instead.
func TestRunJobRestartsJobWhenSupervisorDiscardsUpdate(t *testing.T) {
	ended := time.Date(2026, 9, 13, 9, 0, 0, 0, time.UTC)
	supervisor := &fakeSupervisor{
		name: "job",
		// The command is the compiled one. The update is posted all the same,
		// because the supervisor omits the job's empty workingDir.
		config: `{"name":"job","command":"run"}`,
		state:  processListEntry{Name: "job", Status: "Completed", PID: 100, ProcessEndTime: &ended},
		logs:   []byte("job log line\n"),
	}
	server := httptest.NewServer(supervisor)
	defer server.Close()
	backend := Backend{Runner: supervisor}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	out, err := backend.RunJob(ctx, runtime.Target{ControlPort: serverPort(t, server)}, runtime.JobSpec{
		Name:          "job",
		Configuration: jobConfiguration,
	})
	if err != nil {
		t.Fatalf("RunJob() error = %v, want the re-run's completion; out=%q", err, out)
	}
	if got := string(out); !strings.Contains(got, "job log line") {
		t.Fatalf("RunJob() logs = %q, want captured job logs", got)
	}
	posts, restarts, pid := supervisor.observed()
	if posts != 1 {
		t.Fatalf("posted %d updates; want 1, or the fixture no longer exercises a discarded update", posts)
	}
	if restarts != 1 || pid == 100 {
		t.Fatalf("job restarted %d times and has pid %d; want exactly one re-run with a new pid", restarts, pid)
	}
}

// fakeSupervisor stands in for the process-compose control plane and CLI of one
// process. GET /process/info/<name> serves its native config and GET
// /process/<name> its live state. POST /process always answers 200, as the real
// supervisor does whether or not it acts on the update: it then either replaces
// the process, leaving afterUpdate as its state, or, when afterUpdate is nil,
// leaves it untouched — what process-compose does when its own comparison finds
// the posted config equal to the one it holds. While missing is set the process
// is unknown, answered with the real supervisor's 400 bodies, until POST
// /project registers it. As a Runner it relaunches the process on `process
// restart`, reusing the native state under a new pid, and serves that state to
// `list -o json`.
type fakeSupervisor struct {
	mu          sync.Mutex
	name        string
	missing     bool
	config      string
	state       processListEntry
	afterUpdate *processListEntry
	logs        []byte
	posted      []map[string]any
	restarts    int
}

func (s *fakeSupervisor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.missing && r.Method == http.MethodGet && r.URL.Path == "/process/info/"+s.name:
		http.Error(w, `{"error":"no such process: `+s.name+`"}`, http.StatusBadRequest)
	case s.missing && r.Method == http.MethodGet && r.URL.Path == "/process/"+s.name:
		http.Error(w, `{"error":"can't get state of process `+s.name+`: no such process"}`, http.StatusBadRequest)
	case r.Method == http.MethodGet && r.URL.Path == "/processes":
		_, _ = io.WriteString(w, `{"data":[]}`)
	case r.Method == http.MethodPost && r.URL.Path == "/project":
		var project struct {
			Processes map[string]json.RawMessage `json:"processes"`
		}
		if err := json.NewDecoder(r.Body).Decode(&project); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.missing = false
		s.config = string(project.Processes[s.name])
		s.state = processListEntry{Name: s.name, Status: "Disabled"}
		_, _ = io.WriteString(w, `{}`)
	case r.Method == http.MethodGet && r.URL.Path == "/process/info/"+s.name:
		_, _ = io.WriteString(w, s.config)
	case r.Method == http.MethodGet && r.URL.Path == "/process/"+s.name:
		_ = json.NewEncoder(w).Encode(s.state)
	case r.Method == http.MethodPost && r.URL.Path == "/process":
		var config map[string]any
		if err := json.NewDecoder(r.Body).Decode(&config); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.posted = append(s.posted, config)
		if s.afterUpdate != nil {
			s.state = *s.afterUpdate
		}
		_, _ = io.WriteString(w, `{}`)
	default:
		http.NotFound(w, r)
	}
}

func (s *fakeSupervisor) Run(_ context.Context, _ string, _ []string, _ string, args ...string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case hasArg(args, "logs"):
		return s.logs, nil
	case hasArg(args, "list"):
		return json.Marshal([]processListEntry{s.state})
	case hasArg(args, "restart"):
		s.restarts++
		s.state.PID += 1000
	}
	return nil, nil
}

// observed returns how many updates were posted, how many restarts were issued
// and the pid the process ended up with.
func (s *fakeSupervisor) observed() (posts, restarts, pid int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.posted), s.restarts, s.state.PID
}

// postedCommands returns the command of every posted update, in order.
func (s *fakeSupervisor) postedCommands() []any {
	s.mu.Lock()
	defer s.mu.Unlock()
	commands := make([]any, 0, len(s.posted))
	for _, config := range s.posted {
		commands = append(commands, config["command"])
	}
	return commands
}

// webConfiguration is the compiled definition of a probed service.
const webConfiguration = `version: "0.5"
processes:
  web:
    command: serve
    readiness_probe:
      http_get:
        host: 127.0.0.1
        port: "8000"
        path: /
        scheme: http
      initial_delay_seconds: 0
      period_seconds: 5
      timeout_seconds: 3
      failure_threshold: 12
    shutdown:
      timeout_seconds: 10
      signal: 15
`

// nativeWebConfig is the config process-compose v1.122.0 serves for
// webConfiguration running the given command. Zero values are gone
// (initialDelay, workingDir) and the probe carries the supervisor's own defaults
// (numPort, statusCode), so even with the compiled command it never compares
// equal to the compiled definition field by field.
func nativeWebConfig(command string) string {
	return `{"name":"web","command":"` + command + `","restartPolicy":{},"readinessProbe":{"httpGet":{"host":"127.0.0.1","path":"/","scheme":"http","port":"8000","numPort":8000,"statusCode":200},"periodSeconds":5,"timeoutSeconds":3,"successThreshold":1,"failureThreshold":12},"shutDownParams":{"shutDownTimeout":10,"signal":15},"namespace":"default","replicas":1,"vars":{"PC_REPLICA_NUM":0},"launchTimeout":5,"replicaName":"web","executable":"bash","args":["-c","` + command + `"]}`
}

// Regression test for #91. An unchanged probed service is posted on every
// restart because its native config never equals the compiled one field by
// field; process-compose then finds the posted config equal to its own and
// answers 200 without restarting anything. Apply used to take the accepted POST
// for the restart and report success with the old process still running.
func TestApplyRestartsProcessWhenSupervisorDiscardsUpdate(t *testing.T) {
	started := time.Date(2026, 10, 5, 12, 0, 22, 0, time.UTC)
	supervisor := &fakeSupervisor{
		name:   "web",
		config: nativeWebConfig("serve"),
		state:  processListEntry{Name: "web", Status: "Running", IsRunning: true, PID: 100, ProcessStartTime: &started},
	}
	server := httptest.NewServer(supervisor)
	defer server.Close()
	backend := Backend{Runner: supervisor}

	err := backend.Apply(t.Context(), runtime.Target{
		ControlPort: serverPort(t, server), Services: []string{"web"}, Configuration: []byte(webConfiguration),
	})
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	posts, restarts, pid := supervisor.observed()
	if posts != 1 {
		t.Fatalf("posted %d updates; want 1, or the fixture no longer exercises a discarded update", posts)
	}
	if restarts != 1 || pid == 100 {
		t.Fatalf("process restarted %d times and has pid %d; want exactly one restart with a new pid", restarts, pid)
	}
}

// When the supervisor applies a posted update it replaces the process itself, so
// a Restart on top of it would bounce the service twice. The replacement is
// recognised whether or not it has launched by the time its state is read.
func TestApplyDoesNotRestartProcessRelaunchedByUpdate(t *testing.T) {
	started := time.Date(2026, 10, 5, 12, 0, 22, 0, time.UTC)
	relaunched := started.Add(time.Minute)
	running := processListEntry{Name: "web", Status: "Running", IsRunning: true, PID: 100, ProcessStartTime: &started}
	tests := []struct {
		name        string
		state       processListEntry
		afterUpdate processListEntry
	}{
		{
			name:        "replacement not launched yet",
			state:       running,
			afterUpdate: processListEntry{Name: "web", Status: "Pending"},
		},
		{
			name:        "replacement already running",
			state:       running,
			afterUpdate: processListEntry{Name: "web", Status: "Running", IsRunning: true, PID: 200, ProcessStartTime: &relaunched},
		},
		{
			// Skipped at boot: no pid or start time to compare, so only leaving
			// the terminal state shows that the update relaunched it.
			name:        "skipped process relaunched",
			state:       processListEntry{Name: "web", Status: "Skipped", ExitCode: 1, ProcessEndTime: &started},
			afterUpdate: processListEntry{Name: "web", Status: "Pending"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			supervisor := &fakeSupervisor{name: "web", config: nativeWebConfig("old"), state: tt.state, afterUpdate: &tt.afterUpdate}
			server := httptest.NewServer(supervisor)
			defer server.Close()
			backend := Backend{Runner: supervisor}

			err := backend.Apply(t.Context(), runtime.Target{
				ControlPort: serverPort(t, server), Services: []string{"web"}, Configuration: []byte(webConfiguration),
			})
			if err != nil {
				t.Fatalf("Apply() error = %v", err)
			}
			if got, want := supervisor.postedCommands(), []any{"serve"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("posted commands = %v, want the compiled command once: %v", got, want)
			}
			if _, restarts, _ := supervisor.observed(); restarts != 0 {
				t.Fatalf("process restarted %d times after the supervisor relaunched it; want 0", restarts)
			}
		})
	}
}

// process-compose marks every process left out of an `up <process>...` selection
// disabled, and an applied update keeps that flag: it stops a running process
// and installs a replacement the supervisor never launches. Taking that
// replacement for the restart would report success with the service down.
func TestApplyRestartsProcessTheUpdateLeftDisabled(t *testing.T) {
	started := time.Date(2026, 10, 5, 12, 0, 22, 0, time.UTC)
	supervisor := &fakeSupervisor{
		name:        "web",
		config:      strings.Replace(nativeWebConfig("old"), `{"name":"web",`, `{"name":"web","disabled":true,`, 1),
		state:       processListEntry{Name: "web", Status: "Running", IsRunning: true, PID: 100, ProcessStartTime: &started},
		afterUpdate: &processListEntry{Name: "web", Status: "Disabled"},
	}
	server := httptest.NewServer(supervisor)
	defer server.Close()
	backend := Backend{Runner: supervisor}

	err := backend.Apply(t.Context(), runtime.Target{
		ControlPort: serverPort(t, server), Services: []string{"web"}, Configuration: []byte(webConfiguration),
	})
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if got, want := supervisor.postedCommands(), []any{"serve"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("posted commands = %v, want the compiled command once: %v", got, want)
	}
	if _, restarts, _ := supervisor.observed(); restarts != 1 {
		t.Fatalf("process restarted %d times after the update left it disabled; want 1", restarts)
	}
}

// A process the supervisor does not know yet is registered and activated by the
// update itself, which launches it: there is nothing to restart on top.
func TestApplyRegistersMissingProcessWithoutRestartingIt(t *testing.T) {
	supervisor := &fakeSupervisor{
		name:        "web",
		missing:     true,
		afterUpdate: &processListEntry{Name: "web", Status: "Pending"},
	}
	server := httptest.NewServer(supervisor)
	defer server.Close()
	backend := Backend{Runner: supervisor}

	err := backend.Apply(t.Context(), runtime.Target{
		ControlPort: serverPort(t, server), Services: []string{"web"}, Configuration: []byte(webConfiguration),
	})
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if got, want := supervisor.postedCommands(), []any{"serve"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("posted commands = %v, want one activation of the compiled command: %v", got, want)
	}
	if _, restarts, _ := supervisor.observed(); restarts != 0 {
		t.Fatalf("process restarted %d times after being registered and activated; want 0", restarts)
	}
}

// A definition that already equals the supervisor's config is not posted at all,
// so the restart has to be issued explicitly.
func TestApplyRestartsProcessWithUnchangedDefinition(t *testing.T) {
	supervisor := &fakeSupervisor{
		name:   "web",
		config: `{"name":"web","command":"serve","workingDir":"/srv"}`,
		state:  processListEntry{Name: "web", Status: "Running", IsRunning: true, PID: 100},
	}
	server := httptest.NewServer(supervisor)
	defer server.Close()
	backend := Backend{Runner: supervisor}

	err := backend.Apply(t.Context(), runtime.Target{
		ControlPort: serverPort(t, server), Services: []string{"web"},
		Configuration: []byte("version: \"0.5\"\nprocesses:\n  web:\n    command: serve\n    working_dir: /srv\n"),
	})
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	posts, restarts, _ := supervisor.observed()
	if posts != 0 || restarts != 1 {
		t.Fatalf("posted %d updates and restarted %d times; want no update and exactly one restart", posts, restarts)
	}
}

func TestProcessRelaunched(t *testing.T) {
	started := time.Date(2026, 10, 5, 12, 0, 22, 0, time.UTC)
	relaunched := started.Add(time.Minute)
	running := &processListEntry{Status: "Running", IsRunning: true, PID: 100, ProcessStartTime: &started}
	tests := []struct {
		name   string
		before *processListEntry
		after  *processListEntry
		want   bool
	}{
		{"left alone", running, &processListEntry{Status: "Running", IsRunning: true, PID: 100, ProcessStartTime: &started}, false},
		// A crash between the samples keeps the pid and start time: the process
		// still needs its restart.
		{"exited on its own", running, &processListEntry{Status: "Completed", PID: 100, ProcessStartTime: &started}, false},
		{"blank replacement", running, &processListEntry{Status: "Pending"}, true},
		{"relaunched replacement", running, &processListEntry{Status: "Running", IsRunning: true, PID: 200, ProcessStartTime: &relaunched}, true},
		// process-compose v1.120.0 keeps the start time across a restart.
		{"new pid under the old start time", running, &processListEntry{Status: "Running", IsRunning: true, PID: 200, ProcessStartTime: &started}, true},
		{"old pid under a new start time", running, &processListEntry{Status: "Running", IsRunning: true, PID: 100, ProcessStartTime: &relaunched}, true},
		// The replacement of a process whose config is disabled never launches.
		{"stopped and left disabled", running, &processListEntry{Status: "Disabled"}, false},
		{"skipped then pending", &processListEntry{Status: "Skipped"}, &processListEntry{Status: "Pending"}, true},
		{"skipped and still skipped", &processListEntry{Status: "Skipped"}, &processListEntry{Status: "Skipped"}, false},
		{"pending and still pending", &processListEntry{Status: "Pending"}, &processListEntry{Status: "Pending"}, false},
		{"newly registered", nil, &processListEntry{Status: "Pending"}, true},
		{"registered but left disabled", nil, &processListEntry{Status: "Disabled"}, false},
		{"unknown after the update", running, nil, false},
		{"never known", nil, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := processRelaunched(tt.before, tt.after); got != tt.want {
				t.Fatalf("processRelaunched() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBackendStatusReturnsParseError(t *testing.T) {
	backend := Backend{Runner: &stubListRunner{output: []byte("not json")}}
	got, err := backend.Status(t.Context(), runtime.StatusRequest{Root: "/stack", ControlPort: 10005})
	if err == nil || !strings.Contains(err.Error(), "parse process-compose status") {
		t.Fatalf("Status() error = %v, want parse error", err)
	}
	if got != nil {
		t.Fatalf("statuses = %v, want nil", got)
	}
}
