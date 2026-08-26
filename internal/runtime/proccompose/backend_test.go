package proccompose

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/internal/runtime"
)

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

func TestBackendStatusSkipsLeadingDebugRecords(t *testing.T) {
	// Verbatim shape of a stock macOS run: process-compose cannot find a
	// config home, so it writes debug records to stderr ahead of the array.
	// Each record embeds a '[' of its own, which is what defeated seeking
	// the first '[' byte and left every local service reported as declared.
	const payload = `{"level":"debug","error":"could not locate ` + "`process-compose`" + ` in any of the following paths: [/Users/u/Library/Application Support /Users/u/.config]","time":"2026-08-14T02:06:56-04:00","message":"Path not found for process compose config home"}
{"level":"debug","error":"could not locate ` + "`process-compose`" + ` in any of the following paths: [/Users/u/Library/Application Support /Users/u/.config]","time":"2026-08-14T02:06:56-04:00","message":"Path not found for process compose config home"}
[
	{"name":"django","status":"Running","is_running":true,"exit_code":0,"is_ready":"-"},
	{"name":"frontend","status":"Running","is_running":true,"exit_code":0,"is_ready":"Ready"}
]`
	runner := &stubListRunner{output: []byte(payload)}
	backend := Backend{Runner: runner}
	got, err := backend.Status(context.Background(), runtime.StatusRequest{Root: "/stack", ControlPort: 10006})
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	want := []runtime.ServiceStatus{
		{Name: "django", Runtime: "local", State: "running"},
		{Name: "frontend", Runtime: "local", State: "running", Health: "healthy"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("statuses = %#v, want %#v", got, want)
	}
}

func TestJSONArrayStart(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{name: "bare array", in: "[1]", want: "[1]"},
		{name: "leading blank lines", in: "\n\n[1]", want: "[1]"},
		{name: "log record embedding a bracket", in: "{\"m\":\"paths: [/a /b]\"}\n[1]", want: "[1]"},
		{name: "no array", in: "{\"m\":\"paths: [/a]\"}\n", want: ""},
		{name: "empty", in: "", want: ""},
		// The scan trims each line rather than matching a bare "\n", so neither
		// CRLF output nor a log line padded with trailing spaces hides the array.
		{name: "crlf line endings", in: "{\"m\":\"paths: [/a]\"}\r\n[1]\r\n", want: "[1]"},
		{name: "trailing whitespace on the log line", in: "{\"m\":\"x\"}   \n  [1]", want: "[1]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(jsonArrayStart([]byte(tc.in))); got != tc.want {
				t.Fatalf("jsonArrayStart() = %q, want %q", got, tc.want)
			}
		})
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
