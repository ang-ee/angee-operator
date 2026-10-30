package service

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/runtime"
	"github.com/ang-ee/angee-operator/internal/runtime/proccompose"
)

// Exercise the boundary the graph fake cannot prove: a job declared after the
// shared supervisor started must not POST a replacement project or relaunch a
// held process. Every supervisor, port and workload here belongs to t.TempDir.
func TestNativeHeldNewJobStaysIsolatedThroughRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("native supervisor integration")
	}
	if _, err := exec.LookPath("process-compose"); err != nil {
		t.Skip("process-compose unavailable")
	}
	backend := proccompose.NewBackend()
	if err := backend.ValidateJob(runtime.JobSpec{Transient: true}); err != nil {
		t.Skip(err)
	}
	root := t.TempDir()
	t.Setenv("PC_LOG_FILE", filepath.Join(root, "supervisor.log"))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	stack := &manifest.Stack{
		Version: manifest.VersionCurrent, Kind: manifest.KindStack, Name: "native-holds",
		Ports: map[string]manifest.Port{"process_compose": {Value: port}},
		Services: map[string]manifest.Service{
			"web":       {Runtime: manifest.RuntimeLocal, Command: []string{"sleep", "60"}, StopGracePeriod: "2s"},
			"unrelated": {Runtime: manifest.RuntimeLocal, Command: []string{"sleep", "60"}, StopGracePeriod: "2s"},
		},
	}
	compiled, err := Compile(stack, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	document, err := proccompose.Marshal(compiled.ProcessCompose)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "process-compose.yaml"), document, 0600); err != nil {
		t.Fatal(err)
	}
	target := runtime.Target{Root: root, ControlPort: port}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = backend.Down(ctx, target)
	})
	if err := backend.Up(t.Context(), target); err != nil {
		t.Fatal(err)
	}
	readStates := func() map[string]runtime.ServiceStatus {
		t.Helper()
		states, err := backend.Status(t.Context(), runtime.StatusRequest{Root: root, ControlPort: port})
		if err != nil {
			t.Fatal(err)
		}
		result := map[string]runtime.ServiceStatus{}
		for _, state := range states {
			result[state.Name] = state
		}
		return result
	}
	waitFor := func(description string, ready func() bool) {
		t.Helper()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		deadline := time.NewTimer(10 * time.Second)
		defer deadline.Stop()
		for !ready() {
			select {
			case <-ticker.C:
			case <-deadline.C:
				t.Fatalf("timed out waiting for %s (port %s)", description, strconv.Itoa(port))
			}
		}
	}
	waitFor("initial native services", func() bool { states := readStates(); return states["web"].PID > 0 && states["unrelated"].PID > 0 })
	before := readStates()
	readyFile, releaseFile := filepath.Join(root, "job-ready"), filepath.Join(root, "job-release")
	stack.Jobs = map[string]manifest.Job{
		"prepare": {Runtime: manifest.RuntimeLocal, Holds: []string{"web"}, Command: []string{"sh", "-c", `printf ready > "$1"; while ! test -f "$2"; do sleep 0.01; done; printf isolated-output`, "sh", readyFile, releaseFile}},
	}
	web := stack.Services["web"]
	web.DependsOn = []string{"prepare"}
	stack.Services["web"] = web
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatal(err)
	}
	p, err := NewWithBackends(root, stubStatusBackend{}, backend)
	if err != nil {
		t.Fatal(err)
	}
	op, err := p.JobRunStart(t.Context(), "prepare", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	waitFor("isolated job entry", func() bool { _, err := os.Stat(readyFile); return err == nil })
	during := readStates()
	if strings.EqualFold(during["web"].State, "running") || strings.EqualFold(during["web"].State, "restarting") {
		t.Fatalf("held service relaunched during job: %#v", during["web"])
	}
	if during["unrelated"].PID != before["unrelated"].PID {
		t.Fatalf("shared project was replaced: before=%#v during=%#v", before["unrelated"], during["unrelated"])
	}
	if _, registered := during["prepare"]; registered {
		t.Fatal("new isolated job was registered in the shared supervisor")
	}
	if err := os.WriteFile(releaseFile, nil, 0600); err != nil {
		t.Fatal(err)
	}
	op = waitBackup(t, p, op.ID)
	waitLeaseFree(t, p)
	if op.Status != api.JobRunSucceeded {
		t.Fatalf("operation = %#v", op)
	}
	after := readStates()
	if !strings.EqualFold(after["web"].State, "running") || after["web"].PID == before["web"].PID {
		t.Fatalf("held service was not recovered: before=%#v after=%#v", before["web"], after["web"])
	}
	if after["unrelated"].PID != before["unrelated"].PID {
		t.Fatal("recovery restarted an unrelated service")
	}
	reloaded, err := NewWithBackends(root, stubStatusBackend{}, backend)
	if err != nil {
		t.Fatal(err)
	}
	status, err := reloaded.StackStatus(t.Context())
	if err != nil || status.Jobs["prepare"].Status != api.JobCompleted {
		t.Fatalf("durable status = %#v, %v", status.Jobs, err)
	}
	logs, err := reloaded.StackLogs(t.Context(), []string{"prepare"}, false, 200)
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	for chunk := range logs {
		output.WriteString(chunk)
	}
	if !strings.Contains(output.String(), "isolated-output") {
		t.Fatalf("durable logs = %q", output.String())
	}
}
