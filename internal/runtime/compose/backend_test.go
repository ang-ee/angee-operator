package compose

import (
	"context"
	"reflect"
	"testing"

	"github.com/ang-ee/angee-operator/internal/runtime"
)

type recordingRunner struct {
	name string
	args []string
	out  []byte
}

func (r *recordingRunner) Run(_ context.Context, _ string, name string, args ...string) ([]byte, error) {
	r.name = name
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
	got := parsePS([]byte(`{"Service":"web","State":"running","Health":"healthy"}
{"Service":"db","State":"running","Health":"unhealthy"}
{"Service":"worker","State":"exited"}
`))
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
