package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/internal/logctx"
	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/runtime"
	"github.com/ang-ee/angee-operator/internal/runtime/compose"
	"github.com/ang-ee/angee-operator/internal/runtime/proccompose"
)

type stubStatusBackend struct {
	statuses []runtime.ServiceStatus
}

func (b stubStatusBackend) Build(context.Context, runtime.Target) error { return nil }
func (b stubStatusBackend) Up(context.Context, runtime.Target) error    { return nil }
func (b stubStatusBackend) UpForeground(context.Context, runtime.Target, io.Writer, io.Writer) error {
	return nil
}
func (b stubStatusBackend) Down(context.Context, runtime.Target) error    { return nil }
func (b stubStatusBackend) Start(context.Context, runtime.Target) error   { return nil }
func (b stubStatusBackend) Stop(context.Context, runtime.Target) error    { return nil }
func (b stubStatusBackend) Restart(context.Context, runtime.Target) error { return nil }
func (b stubStatusBackend) Logs(context.Context, runtime.LogsRequest) (<-chan string, error) {
	ch := make(chan string)
	close(ch)
	return ch, nil
}
func (b stubStatusBackend) StreamLogs(context.Context, runtime.LogsRequest) (<-chan string, error) {
	ch := make(chan string)
	close(ch)
	return ch, nil
}
func (b stubStatusBackend) Status(context.Context, runtime.StatusRequest) ([]runtime.ServiceStatus, error) {
	return b.statuses, nil
}

func TestStackPrepareWritesSecretSafeGeneratedFiles(t *testing.T) {
	root := t.TempDir()
	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "notes",
		SecretsBackend: manifest.SecretsBackend{
			Type: "env-file",
			Path: ".env",
		},
		Secrets: map[string]manifest.Secret{
			"postgres-password": {Required: true, Import: "env:POSTGRES_PASSWORD"},
		},
		Ports: map[string]manifest.Port{
			"postgres": {Value: 5432},
		},
		Services: map[string]manifest.Service{
			"postgres": {
				Runtime: manifest.RuntimeContainer,
				Image:   "postgres:16",
				Env: map[string]string{
					"POSTGRES_PASSWORD": "${secret.postgres-password}",
				},
				Ports: []string{"127.0.0.1:${ports.postgres}:5432"},
			},
		},
	}
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile() error = %v", err)
	}
	t.Setenv("POSTGRES_PASSWORD", "super-secret")

	platform, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	compiled, err := platform.StackPrepare(context.Background())
	if err != nil {
		t.Fatalf("StackPrepare() error = %v", err)
	}
	text, err := compiled.Text()
	if err != nil {
		t.Fatalf("Text() error = %v", err)
	}
	if strings.Contains(text, "super-secret") {
		t.Fatal("compiled runtime files contain resolved secret")
	}
	if !strings.Contains(text, "${ANGEE_SECRET_POSTGRES_PASSWORD}") {
		t.Fatalf("compiled text missing secret env placeholder:\n%s", text)
	}
	envData, err := os.ReadFile(filepath.Join(root, ".env"))
	if err != nil {
		t.Fatalf("ReadFile(.env) error = %v", err)
	}
	if !strings.Contains(string(envData), "ANGEE_SECRET_POSTGRES_PASSWORD") || !strings.Contains(string(envData), "super-secret") {
		t.Fatalf("env file does not contain runtime secret env var: %s", envData)
	}
}

func TestStackPrepareNarratesPhasesInOrder(t *testing.T) {
	root := t.TempDir()
	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "narration-test",
	}
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	platform, err := NewWithBackends(root, stubStatusBackend{}, stubStatusBackend{})
	if err != nil {
		t.Fatalf("NewWithBackends: %v", err)
	}
	var logs bytes.Buffer
	ctx := logctx.With(t.Context(), slog.New(logctx.NewCLIHandler(&logs, slog.LevelDebug)))
	if _, err := platform.StackPrepare(ctx); err != nil {
		t.Fatalf("StackPrepare: %v", err)
	}

	output := logs.String()
	position := 0
	for _, phase := range []string{
		"loading stack",
		"materializing sources",
		"materializing declared workspaces",
		"compiling stack",
		"writing runtime files",
	} {
		start := "angee: " + phase + "\n"
		index := strings.Index(output[position:], start)
		if index < 0 {
			t.Fatalf("missing phase %q after byte %d in logs:\n%s", phase, position, output)
		}
		position += index + len(start)
		finished := "angee: finished " + phase + " duration="
		index = strings.Index(output[position:], finished)
		if index < 0 {
			t.Fatalf("missing duration-bearing completion for %q in logs:\n%s", phase, output)
		}
		position += index + len(finished)
	}
}

func TestEnvFileAtOpenBaoRuntimePathIsNeverScheduledForDeletion(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "run", "secrets.env")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(run): %v", err)
	}
	if err := os.WriteFile(path, []byte("KEEP=value\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(env): %v", err)
	}
	platform, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stack := &manifest.Stack{SecretsBackend: manifest.SecretsBackend{Type: "env-file", Path: "run/secrets.env"}}
	if err := platform.writeRuntimeEnv(stack, nil); err != nil {
		t.Fatalf("writeRuntimeEnv: %v", err)
	}
	_, deletions, _, err := platform.runtimeArtifactDocuments(root, stack, &CompiledStack{}, nil)
	if err != nil {
		t.Fatalf("runtimeArtifactDocuments: %v", err)
	}
	if deletions["run/secrets.env"] {
		t.Fatal("active env-file backend path was scheduled for deletion")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "KEEP=value\n" {
		t.Fatalf("env-file contents = %q, %v", data, err)
	}
	alias := filepath.Join(root, ".env")
	if err := os.Symlink(filepath.Join("run", "secrets.env"), alias); err != nil {
		t.Fatalf("Symlink(env alias): %v", err)
	}
	stack.SecretsBackend.Path = ".env"
	if err := platform.writeRuntimeEnv(stack, nil); err != nil {
		t.Fatalf("writeRuntimeEnv(alias): %v", err)
	}
	_, deletions, _, err = platform.runtimeArtifactDocuments(root, stack, &CompiledStack{}, nil)
	if err != nil {
		t.Fatalf("runtimeArtifactDocuments(alias): %v", err)
	}
	if deletions["run/secrets.env"] {
		t.Fatal("symlinked active env-file backend target was scheduled for deletion")
	}
	if data, err := os.ReadFile(alias); err != nil || string(data) != "KEEP=value\n" {
		t.Fatalf("env-file alias contents = %q, %v", data, err)
	}
}

func TestRuntimeEnvDeletionRollsBackWhenEnvFileBecomesAlias(t *testing.T) {
	root := t.TempDir()
	candidate := filepath.Join(root, "run", "secrets.env")
	configured := filepath.Join(root, ".env")
	if err := os.MkdirAll(filepath.Dir(candidate), 0o755); err != nil {
		t.Fatalf("MkdirAll(run): %v", err)
	}
	if err := os.WriteFile(candidate, []byte("STALE=secret\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(candidate): %v", err)
	}
	if err := os.WriteFile(configured, []byte("ACTIVE=value\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(configured): %v", err)
	}
	platform, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stack := &manifest.Stack{SecretsBackend: manifest.SecretsBackend{Type: "env-file", Path: ".env"}}
	_, deletions, modes, err := platform.runtimeArtifactDocuments(root, stack, &CompiledStack{}, nil)
	if err != nil {
		t.Fatalf("runtimeArtifactDocuments: %v", err)
	}
	if !deletions["run/secrets.env"] {
		t.Fatal("obsolete OpenBao runtime env was not scheduled for deletion")
	}
	opener := targetPathOpener(root, root, nil)
	expectations, err := captureRenderedDocumentExpectations(context.Background(), opener, map[string][]byte{"run/secrets.env": nil})
	if err != nil {
		t.Fatalf("captureRenderedDocumentExpectations: %v", err)
	}
	verifyEnv, closeEnv, err := platform.retainActiveEnvFile(stack, openAbsoluteGuardedPath)
	if err != nil {
		t.Fatalf("retainActiveEnvFile: %v", err)
	}
	defer closeEnv()
	if err := os.Remove(configured); err != nil {
		t.Fatalf("Remove(configured): %v", err)
	}
	if err := os.Symlink(filepath.Join("run", "secrets.env"), configured); err != nil {
		t.Fatalf("Symlink(alias): %v", err)
	}
	rollback, closeRuntime, _, err := applyRenderedDocuments(context.Background(), opener, root, nil, deletions, modes, expectations, false)
	if err != nil {
		t.Fatalf("applyRenderedDocuments: %v", err)
	}
	defer closeRuntime()
	if err := verifyEnv(); err == nil {
		t.Fatal("retained env-file identity accepted a new alias")
	}
	if err := rollback(); err != nil {
		t.Fatalf("rollback runtime deletion: %v", err)
	}
	data, err := os.ReadFile(candidate)
	if err != nil || string(data) != "STALE=secret\n" {
		t.Fatalf("restored candidate = %q, %v", data, err)
	}
}

func TestStackStatusMergesRuntimeStateAndHealth(t *testing.T) {
	root := t.TempDir()
	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "demo",
		Services: map[string]manifest.Service{
			"web":     {Runtime: manifest.RuntimeContainer, Image: "nginx"},
			"db":      {Runtime: manifest.RuntimeContainer, Image: "postgres:16"},
			"unknown": {Runtime: manifest.RuntimeContainer, Image: "alpine"},
		},
	}
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	stub := stubStatusBackend{statuses: []runtime.ServiceStatus{
		{Name: "web", Runtime: "container", State: "running", Health: "healthy"},
		{Name: "db", Runtime: "container", State: "running", Health: "unhealthy"},
	}}
	platform, err := NewWithBackends(root, stub, stubStatusBackend{})
	if err != nil {
		t.Fatalf("NewWithBackends: %v", err)
	}
	resp, err := platform.StackStatus(context.Background())
	if err != nil {
		t.Fatalf("StackStatus: %v", err)
	}
	if got := resp.Services["web"]; got.Status != "running" || got.Health != "healthy" {
		t.Fatalf("web = %+v, want running/healthy", got)
	}
	if got := resp.Services["db"]; got.Status != "running" || got.Health != "unhealthy" {
		t.Fatalf("db = %+v, want running/unhealthy", got)
	}
	if got := resp.Services["unknown"]; got.Status != "declared" || got.Health != "" {
		t.Fatalf("unknown = %+v, want declared/empty health", got)
	}
}

func TestCompileContainerReadinessProbes(t *testing.T) {
	tests := map[string]struct {
		probe    *manifest.ReadyProbe
		wantTest []string
	}{
		"http": {
			probe: &manifest.ReadyProbe{HTTP: &manifest.ReadyHTTP{Port: 8080, Path: "/healthz?token=$TOKEN"}},
			wantTest: []string{
				"CMD-SHELL",
				"wget -qO- 'http://127.0.0.1:8080/healthz?token=$$TOKEN' >/dev/null 2>&1 || curl -fsS 'http://127.0.0.1:8080/healthz?token=$$TOKEN' >/dev/null",
			},
		},
		"tcp": {
			probe:    &manifest.ReadyProbe{TCP: &manifest.ReadyTCP{Port: 5432}},
			wantTest: []string{"CMD-SHELL", "nc -z 127.0.0.1 5432"},
		},
		"cmd": {
			probe:    &manifest.ReadyProbe{Cmd: []string{"sh", "-c", `test "$READY" = yes`}},
			wantTest: []string{"CMD", "sh", "-c", `test "$$READY" = yes`},
		},
		"file": {
			probe:    &manifest.ReadyProbe{File: "state/$ENV/ready file"},
			wantTest: []string{"CMD-SHELL", "test -s 'state/$$ENV/ready file'"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			tc.probe.Interval = "1500ms"
			tc.probe.Timeout = "750ms"
			tc.probe.Retries = readyRetries(4)
			tc.probe.StartPeriod = "2500ms"
			stack := compileReadinessStack(manifest.RuntimeContainer, tc.probe)
			compiled, err := Compile(stack, t.TempDir(), nil)
			if err != nil {
				t.Fatalf("Compile() error = %v", err)
			}
			healthcheck := compiled.Compose.Services["ready"].Healthcheck
			want := &compose.Healthcheck{
				Test:        tc.wantTest,
				Interval:    "1500ms",
				Timeout:     "750ms",
				Retries:     4,
				StartPeriod: "2500ms",
			}
			if !reflect.DeepEqual(healthcheck, want) {
				t.Fatalf("Healthcheck = %#v, want %#v", healthcheck, want)
			}
			if got := compiled.Compose.Services["dependent"].DependsOn["ready"].Condition; got != "service_healthy" {
				t.Fatalf("dependency condition = %q, want service_healthy", got)
			}
			rendered, err := compose.Marshal(compiled.Compose)
			if err != nil {
				t.Fatalf("compose.Marshal() error = %v", err)
			}
			wantFragment := composeHealthcheckFragment(tc.wantTest)
			if !bytes.Contains(rendered, []byte(wantFragment)) {
				t.Fatalf("rendered Compose missing exact healthcheck fragment\ngot:\n%s\nwant fragment:\n%s", rendered, wantFragment)
			}
			if !bytes.Contains(rendered, []byte("condition: service_healthy")) {
				t.Fatalf("rendered Compose missing healthy dependency:\n%s", rendered)
			}
		})
	}
}

func TestCompileLocalReadinessProbes(t *testing.T) {
	tests := map[string]struct {
		probe *manifest.ReadyProbe
		want  *proccompose.Probe
	}{
		"http": {
			probe: &manifest.ReadyProbe{HTTP: &manifest.ReadyHTTP{Port: 8080, Path: "/healthz"}},
			want: &proccompose.Probe{
				HTTPGet: &proccompose.HTTPGet{Host: "127.0.0.1", Port: "8080", Path: "/healthz", Scheme: "http"},
			},
		},
		"tcp": {
			probe: &manifest.ReadyProbe{TCP: &manifest.ReadyTCP{Port: 5432}},
			want:  &proccompose.Probe{Exec: &proccompose.ExecProbe{Command: "nc -z 127.0.0.1 5432"}},
		},
		"cmd": {
			probe: &manifest.ReadyProbe{Cmd: []string{"tool", "two words", "it's", "$HOME", ""}},
			want:  &proccompose.Probe{Exec: &proccompose.ExecProbe{Command: `tool 'two words' 'it'\''s' '$$HOME' ''`}},
		},
		"file": {
			probe: &manifest.ReadyProbe{File: "state/$ENV/ready file"},
			want:  &proccompose.Probe{Exec: &proccompose.ExecProbe{Command: "test -s 'state/$$ENV/ready file'"}},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			tc.probe.Interval = "1500ms"
			tc.probe.Timeout = "750ms"
			tc.probe.Retries = readyRetries(4)
			tc.probe.StartPeriod = "2500ms"
			tc.want.InitialDelaySeconds = 3
			tc.want.PeriodSeconds = 2
			tc.want.TimeoutSeconds = 1
			tc.want.FailureThreshold = 4
			stack := compileReadinessStack(manifest.RuntimeLocal, tc.probe)
			compiled, err := Compile(stack, t.TempDir(), nil)
			if err != nil {
				t.Fatalf("Compile() error = %v", err)
			}
			probe := compiled.ProcessCompose.Processes["ready"].ReadinessProbe
			if !reflect.DeepEqual(probe, tc.want) {
				t.Fatalf("ReadinessProbe = %#v, want %#v", probe, tc.want)
			}
			if got := compiled.ProcessCompose.Processes["dependent"].DependsOn["ready"].Condition; got != "process_healthy" {
				t.Fatalf("dependency condition = %q, want process_healthy", got)
			}
			rendered, err := proccompose.Marshal(compiled.ProcessCompose)
			if err != nil {
				t.Fatalf("proccompose.Marshal() error = %v", err)
			}
			wantFragment := processReadinessFragment(tc.want)
			if !bytes.Contains(rendered, []byte(wantFragment)) {
				t.Fatalf("rendered process-compose missing exact readiness fragment\ngot:\n%s\nwant fragment:\n%s", rendered, wantFragment)
			}
			if !bytes.Contains(rendered, []byte("condition: process_healthy")) {
				t.Fatalf("rendered process-compose missing healthy dependency:\n%s", rendered)
			}
		})
	}
}

func TestCompileLocalReadinessProbeRunsInServiceWorkdir(t *testing.T) {
	stack := compileReadinessStack(manifest.RuntimeLocal, &manifest.ReadyProbe{File: "dist/index.html"})
	ready := stack.Services["ready"]
	ready.Workdir = "app"
	stack.Services["ready"] = ready
	root := t.TempDir()
	compiled, err := Compile(stack, root, nil)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	probe := compiled.ProcessCompose.Processes["ready"].ReadinessProbe
	if probe == nil || probe.Exec == nil {
		t.Fatalf("ReadinessProbe = %#v, want an exec probe", probe)
	}
	if want := filepath.Join(root, "app"); probe.Exec.WorkingDir != want {
		t.Fatalf("exec probe working_dir = %q, want the service workdir %q", probe.Exec.WorkingDir, want)
	}
	if got := compiled.ProcessCompose.Processes["ready"].WorkingDir; got != probe.Exec.WorkingDir {
		t.Fatalf("probe working_dir %q differs from process working_dir %q", probe.Exec.WorkingDir, got)
	}
}

func TestReadinessDependencyConditions(t *testing.T) {
	stack := &manifest.Stack{
		Services: map[string]manifest.Service{
			"ready":   {Ready: &manifest.ReadyProbe{TCP: &manifest.ReadyTCP{Port: 8080}}},
			"started": {},
		},
		Jobs: map[string]manifest.Job{"migrate": {}},
	}
	composeDeps := composeDependsOn([]string{"ready", "started", "migrate", "external"}, stack)
	processDeps := processDependsOn([]string{"ready", "started", "migrate", "external"}, stack)
	composeWant := map[string]string{
		"ready": "service_healthy", "started": "service_started",
		"migrate": "service_completed_successfully", "external": "service_started",
	}
	processWant := map[string]string{
		"ready": "process_healthy", "started": "process_started",
		"migrate": "process_completed_successfully", "external": "process_started",
	}
	for name, want := range composeWant {
		if got := composeDeps[name].Condition; got != want {
			t.Errorf("compose dependency %q = %q, want %q", name, got, want)
		}
	}
	for name, want := range processWant {
		if got := processDeps[name].Condition; got != want {
			t.Errorf("process dependency %q = %q, want %q", name, got, want)
		}
	}
}

func TestCompileWithoutReadinessIsByteStable(t *testing.T) {
	const root = "/stack"
	stack := &manifest.Stack{
		Name: "demo",
		Services: map[string]manifest.Service{
			"docker": {Runtime: manifest.RuntimeContainer, Image: "nginx:alpine"},
			"local": {
				Runtime: manifest.RuntimeLocal, Command: []string{"go", "run", "./cmd/server"},
				Workdir: "app", After: []string{"docker"},
			},
		},
	}
	stack.Defaults()
	if err := stack.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	compiled, err := Compile(stack, root, nil)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	composeYAML, err := compose.Marshal(compiled.Compose)
	if err != nil {
		t.Fatalf("compose.Marshal() error = %v", err)
	}
	wantCompose := fmt.Sprintf(`name: %s
services:
    docker:
        image: nginx:alpine
        extra_hosts:
            - host.docker.internal:host-gateway
`, composeProjectName("demo", root))
	if !bytes.Equal(composeYAML, []byte(wantCompose)) {
		t.Fatalf("Compose output changed without ready\ngot:\n%s\nwant:\n%s", composeYAML, wantCompose)
	}
	processYAML, err := proccompose.Marshal(compiled.ProcessCompose)
	if err != nil {
		t.Fatalf("proccompose.Marshal() error = %v", err)
	}
	wantProcess := `version: "0.5"
processes:
    local:
        command: go run ./cmd/server
        working_dir: /stack/app
        depends_on:
            docker:
                condition: process_started
`
	if !bytes.Equal(processYAML, []byte(wantProcess)) {
		t.Fatalf("process-compose output changed without ready\ngot:\n%s\nwant:\n%s", processYAML, wantProcess)
	}
}

func TestShellQuote(t *testing.T) {
	tests := map[string]string{
		"":           "''",
		"plain/path": "plain/path",
		"two words":  "'two words'",
		"it's":       `'it'\''s'`,
		"$HOME":      "'$HOME'",
	}
	for input, want := range tests {
		if got := shellQuote(input); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", input, got, want)
		}
	}
}

func composeHealthcheckFragment(healthcheckTest []string) string {
	var fragment strings.Builder
	fragment.WriteString("        healthcheck:\n            test:\n")
	for _, item := range healthcheckTest {
		_, _ = fmt.Fprintf(&fragment, "                - %s\n", item)
	}
	fragment.WriteString("            interval: 1500ms\n            timeout: 750ms\n            retries: 4\n            start_period: 2500ms\n")
	return fragment.String()
}

func processReadinessFragment(probe *proccompose.Probe) string {
	var fragment strings.Builder
	fragment.WriteString("        readiness_probe:\n")
	if probe.HTTPGet != nil {
		fragment.WriteString("            http_get:\n")
		_, _ = fmt.Fprintf(&fragment, "                host: %s\n                port: %q\n                path: %s\n                scheme: %s\n", probe.HTTPGet.Host, probe.HTTPGet.Port, probe.HTTPGet.Path, probe.HTTPGet.Scheme)
	} else {
		fragment.WriteString("            exec:\n")
		_, _ = fmt.Fprintf(&fragment, "                command: %s\n", probe.Exec.Command)
	}
	_, _ = fmt.Fprintf(
		&fragment,
		"            initial_delay_seconds: %d\n            period_seconds: %d\n            timeout_seconds: %d\n            failure_threshold: %d\n",
		probe.InitialDelaySeconds,
		probe.PeriodSeconds,
		probe.TimeoutSeconds,
		probe.FailureThreshold,
	)
	return fragment.String()
}

func readyRetries(value int) *int {
	return &value
}

func compileReadinessStack(runtimeType manifest.Runtime, probe *manifest.ReadyProbe) *manifest.Stack {
	ready := manifest.Service{Runtime: runtimeType, Ready: probe}
	dependent := manifest.Service{Runtime: runtimeType, After: []string{"ready"}}
	if runtimeType == manifest.RuntimeContainer {
		ready.Image = "example/ready:latest"
		dependent.Image = "example/dependent:latest"
	} else {
		ready.Command = []string{"ready-server"}
		dependent.Command = []string{"dependent-server"}
	}
	stack := &manifest.Stack{
		Name: "ready",
		Services: map[string]manifest.Service{
			"ready":     ready,
			"dependent": dependent,
		},
	}
	stack.Defaults()
	return stack
}
