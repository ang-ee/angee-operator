package proccompose

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"

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
		{Name: "build-watch", Runtime: "local", State: "running"},
		{Name: "web", Runtime: "local", State: "running", Health: "healthy"},
		{Name: "migrate", Runtime: "local", State: "completed"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("statuses = %#v, want %#v", got, want)
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
		{Name: "storybook", Runtime: "local", State: "running"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("statuses = %#v, want %#v", got, want)
	}
}

func TestBackendStatusSwallowsErrors(t *testing.T) {
	runner := &stubListRunner{err: errors.New("supervisor offline")}
	backend := Backend{Runner: runner}
	got, err := backend.Status(context.Background(), runtime.StatusRequest{Root: "/stack", ControlPort: 10005})
	if err != nil {
		t.Fatalf("Status() error = %v, want nil", err)
	}
	if got != nil {
		t.Fatalf("statuses = %v, want nil", got)
	}
}
