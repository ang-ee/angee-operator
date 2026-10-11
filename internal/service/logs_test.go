package service

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/runtime"
	"github.com/ang-ee/angee-operator/internal/runtime/compose"
	"github.com/ang-ee/angee-operator/internal/runtime/proccompose"
)

func TestInferLogLevel(t *testing.T) {
	cases := []struct {
		line string
		want string // "" means nil (unknown)
	}{
		{`level=warn msg="disk low"`, "WARN"},
		{`level=warning msg=x`, "WARN"},
		{`time=... level=error err=boom`, "ERROR"},
		{`[INFO] server started`, "INFO"},
		{`[DEBUG] cache miss`, "DEBUG"},
		{`level=fatal`, "ERROR"},
		{`level=panic`, "ERROR"},
		{`level=err`, "ERROR"},
		{`GET /health 200 OK`, ""},
		{`no error occurred during startup`, ""}, // mid-sentence word, not a marker
		{`information about the run`, ""},        // must not match "info" inside a word
	}
	for _, c := range cases {
		got := inferLogLevel(c.line)
		if c.want == "" {
			if got != nil {
				t.Errorf("inferLogLevel(%q) = %q, want nil", c.line, *got)
			}
			continue
		}
		if got == nil {
			t.Errorf("inferLogLevel(%q) = nil, want %q", c.line, c.want)
			continue
		}
		if *got != c.want {
			t.Errorf("inferLogLevel(%q) = %q, want %q", c.line, *got, c.want)
		}
	}
}

// envFileRecorder records the env file each status query and down of the
// process-compose backend names.
type envFileRecorder struct {
	stubStatusBackend
	mu       sync.Mutex
	envFiles map[string][]string
}

func (b *envFileRecorder) record(call, envFile string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.envFiles[call] = append(b.envFiles[call], envFile)
}

func (b *envFileRecorder) Status(_ context.Context, req runtime.StatusRequest) ([]runtime.ServiceStatus, error) {
	b.record("status", req.EnvFile)
	return nil, nil
}

func (b *envFileRecorder) Down(_ context.Context, target runtime.Target) error {
	b.record("down", target.EnvFile)
	return nil
}

// Stack status, stack down and `angee dev`'s wait for the supervisor hand the
// process-compose backend the stack's env file, so a token set only there
// reaches `process-compose list` and `down`. Without it they got a 401: status
// showed local services as unknown, down failed, and dev waited forever.
func TestLocalStatusAndDownGetTheStackEnvFile(t *testing.T) {
	root := devFailureStack(t)
	local := &envFileRecorder{envFiles: map[string][]string{}}
	platform, err := NewWithBackends(root, stubStatusBackend{}, local)
	if err != nil {
		t.Fatalf("NewWithBackends: %v", err)
	}
	stack, err := platform.LoadStack()
	if err != nil {
		t.Fatalf("LoadStack: %v", err)
	}
	envFile := platform.runtimeEnvFile(stack)
	if _, err := platform.StackStatus(t.Context()); err != nil {
		t.Fatalf("StackStatus: %v", err)
	}
	if !platform.awaitSupervisor(t.Context(), stack) {
		t.Fatal("awaitSupervisor() = false, want the supervisor answering")
	}
	if err := platform.StackDown(t.Context()); err != nil {
		t.Fatalf("StackDown: %v", err)
	}
	for call, want := range map[string]int{"status": 2, "down": 1} {
		got := local.envFiles[call]
		if len(got) != want {
			t.Fatalf("%s calls = %q, want %d", call, got, want)
		}
		for _, file := range got {
			if file != envFile {
				t.Errorf("%s env file = %q, want %q", call, file, envFile)
			}
		}
	}
}

// logLines are the lines the runtimes hold of two services, oldest first.
var logLines = map[string][]string{
	"api":    {"api-1", "api-2", "api-3", "api-4", "api-5"},
	"worker": {"worker-1"},
}

// labelledLogs is a read of api and worker, labelled as both runtimes label
// several services: five 15-byte api lines, then one 18-byte worker line.
const labelledLogs = "api    | api-1\napi    | api-2\napi    | api-3\napi    | api-4\napi    | api-5\nworker | worker-1\n"

// startLogRangeSupervisor answers process-compose's log range endpoint
// (GET /process/logs/{name}/0/{limit}: {"logs": [...]}, newest last) with each
// process's lines, as v1.120.0 does, and returns its port.
func startLogRangeSupervisor(t *testing.T, lines map[string][]string) int {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest, ok := strings.CutPrefix(r.URL.Path, "/process/logs/")
		name, _, _ := strings.Cut(rest, "/")
		process, known := lines[name]
		if !ok || !known {
			http.Error(w, `{"error":"process `+name+` doesn't exist"}`, http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string][]string{"logs": process})
	}))
	t.Cleanup(server.Close)
	return server.Listener.Addr().(*net.TCPAddr).Port
}

// composeLogs answers `docker compose logs` with output, labelled as docker
// compose labels it.
type composeLogs string

func (r composeLogs) Run(context.Context, string, []string, string, ...string) ([]byte, error) {
	return []byte(r), nil
}

// logsStack writes a stack of services, its process-compose supervisor on
// port, with both runtime files generated.
func logsStack(t *testing.T, port int, services map[string]manifest.Service) string {
	t.Helper()
	root := t.TempDir()
	stack := &manifest.Stack{
		Version:  manifest.VersionCurrent,
		Kind:     manifest.KindStack,
		Name:     "logs",
		Ports:    map[string]manifest.Port{"process_compose": {Value: port}},
		Services: services,
	}
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	for _, name := range []string{"docker-compose.yaml", "process-compose.yaml"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("generated\n"), 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", name, err)
		}
	}
	return root
}

// readLimited reads services through StackLogsLimited, within maxBytes.
func readLimited(t *testing.T, platform *Platform, services []string, maxBytes int) string {
	t.Helper()
	logs, err := platform.StackLogsLimited(t.Context(), services, false, 0, maxBytes)
	if err != nil {
		t.Fatalf("StackLogsLimited(%v, %d bytes): %v", services, maxBytes, err)
	}
	var read strings.Builder
	for chunk := range logs {
		read.WriteString(chunk)
	}
	return read.String()
}

// localLogsPlatform is a platform whose api and worker are local services,
// read through the process-compose backend from the supervisor on port.
func localLogsPlatform(t *testing.T, port int) *Platform {
	t.Helper()
	root := logsStack(t, port, map[string]manifest.Service{
		"api":    {Runtime: manifest.RuntimeLocal, Command: []string{"serve"}},
		"worker": {Runtime: manifest.RuntimeLocal, Command: []string{"work"}},
	})
	platform, err := NewWithBackends(root, compose.Backend{Runner: composeLogs("")}, proccompose.Backend{})
	if err != nil {
		t.Fatalf("NewWithBackends: %v", err)
	}
	return platform
}

// A limited read of several services keeps the newest whole, labelled lines of
// the whole read, behind one marker, within the limit (#118). It used to cap
// each process before labelling it, then keep the start of the joined text:
// `stackLogs(services: [api, worker], limit: 60)` was cut mid-line and had
// nothing from worker.
func TestStackLogsLimitedKeepsTheNewestWholeLabelledLines(t *testing.T) {
	platform := localLogsPlatform(t, startLogRangeSupervisor(t, logLines))
	want := "[truncated]\napi    | api-4\napi    | api-5\nworker | worker-1\n"
	if got := readLimited(t, platform, []string{"api", "worker"}, 60); got != want {
		t.Fatalf("read within 60 bytes = %q, want %q", got, want)
	}
	if got := readLimited(t, platform, []string{"api", "worker"}, 0); got != labelledLogs {
		t.Fatalf("read without a limit = %q, want %q", got, labelledLogs)
	}
	// The newest lines are the last in read order, and local lines come
	// grouped by service in the order requested: asked for first, worker is
	// left out.
	if got, want := readLimited(t, platform, []string{"worker", "api"}, 60), "[truncated]\napi    | api-3\napi    | api-4\napi    | api-5\n"; got != want {
		t.Fatalf("read of worker then api within 60 bytes = %q, want %q", got, want)
	}
}

// The container and local runtimes give the same read of the same labelled
// lines at every limit: both return every line the tail asks for, and the
// limit applies to the read once (#118). The container backend used to keep
// the oldest bytes of its output.
func TestStackLogsLimitedIsTheSameForBothRuntimes(t *testing.T) {
	port := startLogRangeSupervisor(t, logLines)
	local := localLogsPlatform(t, port)
	root := logsStack(t, port, map[string]manifest.Service{
		"api":    {Runtime: manifest.RuntimeContainer, Image: "api:dev"},
		"worker": {Runtime: manifest.RuntimeContainer, Image: "worker:dev"},
	})
	containers, err := NewWithBackends(root, compose.Backend{Runner: composeLogs(labelledLogs)}, proccompose.Backend{})
	if err != nil {
		t.Fatalf("NewWithBackends: %v", err)
	}
	for _, maxBytes := range []int{0, 1 << 20, len(labelledLogs), len(labelledLogs) - 1, 60, 40, 20, 5} {
		fromContainers := readLimited(t, containers, []string{"api", "worker"}, maxBytes)
		fromLocal := readLimited(t, local, []string{"api", "worker"}, maxBytes)
		if fromContainers != fromLocal {
			t.Errorf("within %d bytes: containers read %q, local read %q", maxBytes, fromContainers, fromLocal)
		}
		if maxBytes >= len(runtime.LogTruncatedMarker) && len(fromLocal) > maxBytes {
			t.Errorf("within %d bytes: read is %d bytes", maxBytes, len(fromLocal))
		}
	}
}

// A read of both runtimes joins the container lines and then the local ones,
// and limits the joined read once: one marker, at its start, and the newest
// lines, which are the local ones.
func TestStackLogsLimitedLimitsAMixedReadOnce(t *testing.T) {
	port := startLogRangeSupervisor(t, logLines)
	root := logsStack(t, port, map[string]manifest.Service{
		"db":     {Runtime: manifest.RuntimeContainer, Image: "postgres:16"},
		"api":    {Runtime: manifest.RuntimeLocal, Command: []string{"serve"}},
		"worker": {Runtime: manifest.RuntimeLocal, Command: []string{"work"}},
	})
	const dbLines = "db-1  | ready\ndb-1  | checkpoint\n"
	platform, err := NewWithBackends(root, compose.Backend{Runner: composeLogs(dbLines)}, proccompose.Backend{})
	if err != nil {
		t.Fatalf("NewWithBackends: %v", err)
	}
	services := []string{"db", "api", "worker"}
	if got, want := readLimited(t, platform, services, 0), dbLines+labelledLogs; got != want {
		t.Fatalf("read without a limit = %q, want %q", got, want)
	}
	// Room for the local lines and the container's last line.
	maxBytes := len(runtime.LogTruncatedMarker) + len("db-1  | checkpoint\n") + len(labelledLogs)
	if got, want := readLimited(t, platform, services, maxBytes), "[truncated]\ndb-1  | checkpoint\n"+labelledLogs; got != want {
		t.Fatalf("read within %d bytes = %q, want %q", maxBytes, got, want)
	}
	if got, want := readLimited(t, platform, services, 60), "[truncated]\napi    | api-4\napi    | api-5\nworker | worker-1\n"; got != want {
		t.Fatalf("read within 60 bytes = %q, want %q", got, want)
	}
}
