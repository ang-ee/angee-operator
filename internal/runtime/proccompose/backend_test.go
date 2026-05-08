package proccompose

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/fyltr/angee/internal/runtime"
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
	err := backend.Up(context.Background(), runtime.Target{Root: "/stack", Services: []string{"web"}})
	if err != nil {
		t.Fatalf("Up() error = %v", err)
	}
	want := []string{"-f", "/stack/process-compose.yaml", "up", "-d", "--tui=false", "web"}
	if runner.name != "process-compose" || !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("command = %s %v, want process-compose %v", runner.name, runner.args, want)
	}
}
