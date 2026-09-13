package proccompose

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
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

func TestBackendUpCommand(t *testing.T) {
	runner := &recordingRunner{}
	backend := Backend{Runner: runner}
	err := backend.Up(context.Background(), runtime.Target{Root: "/stack", Services: []string{"web"}, ControlPort: 10002})
	if err != nil {
		t.Fatalf("Up() error = %v", err)
	}
	want := []string{"-f", "/stack/process-compose.yaml", "--address", "127.0.0.1", "--port", "10002", "up", "-d", "--tui=false", "web"}
	if runner.name != "process-compose" || !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("command = %s %v, want process-compose %v", runner.name, runner.args, want)
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
// launch lifecycle without a live supervisor.
type scriptedJobRunner struct {
	lists   [][]byte
	logs    []byte
	listIdx int
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/process/info/job":
			_, _ = io.WriteString(w, `{"name":"job","command":"old"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/process":
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		// command differs from the compiled "run", so updateProcess reports a
		// change and the re-run goes through the update path (a fresh end time).
		case r.Method == http.MethodGet && r.URL.Path == "/process/info/job":
			_, _ = io.WriteString(w, `{"name":"job","command":"old"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/process":
			_, _ = io.WriteString(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
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
