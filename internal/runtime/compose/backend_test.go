package compose

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/internal/logctx"
	"github.com/ang-ee/angee-operator/internal/runtime"
)

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
