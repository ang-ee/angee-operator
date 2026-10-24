package proccompose

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ang-ee/angee-operator/internal/runtime"
)

func TestTransientJobUsesNativeForegroundWithoutRestartingSupervisorProcesses(t *testing.T) {
	if err := transientJobSupport(); err != nil {
		t.Skip(err)
	}
	if _, err := exec.LookPath("process-compose"); err != nil {
		t.Skip("process-compose unavailable")
	}
	if testing.Short() {
		t.Skip("native supervisor integration")
	}
	root := t.TempDir()
	t.Setenv("PC_LOG_FILE", filepath.Join(root, "supervisor.log"))
	document, err := Marshal(File{Version: "0.5", Processes: map[string]Process{"operator": {Command: "sleep 60", Shutdown: &Shutdown{TimeoutSeconds: 2, Signal: 15}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "process-compose.yaml"), document, 0600); err != nil {
		t.Fatal(err)
	}
	backend := NewBackend()
	target := runtime.Target{Root: root, ControlPort: scratchControlPort(t)}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = backend.Down(ctx, target)
	})
	if err := backend.Up(t.Context(), target); err != nil {
		t.Fatal(err)
	}
	var baseline *processListEntry
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		baseline, err = backend.processEntry(t.Context(), target, "operator")
		if err == nil && baseline != nil && baseline.IsRunning && baseline.PID > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if baseline == nil || baseline.PID == 0 {
		t.Fatalf("operator baseline=%+v err=%v", baseline, err)
	}
	for _, test := range []struct {
		name, command string
		cancel        bool
		failure       bool
	}{{"success", "printf transient-ok", false, false}, {"failure", "printf 'capture-failure https://user:token@host.invalid/repo\\n' >&2; exit 23", false, true}, {"canceled", "sleep 60", true, true}} {
		t.Run(test.name, func(t *testing.T) {
			config, err := Marshal(File{Version: "0.5", Processes: map[string]Process{"capture": {Command: test.command, WorkingDir: root, Shutdown: &Shutdown{TimeoutSeconds: 2, Signal: 15}}}})
			if err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			cancel := func() {}
			if test.cancel {
				ctx, cancel = context.WithTimeout(ctx, 200*time.Millisecond)
			}
			defer cancel()
			output, err := backend.RunJob(ctx, target, runtime.JobSpec{Name: "capture", Configuration: config, Transient: true})
			if test.failure && err == nil {
				t.Fatalf("native foreground job returned success; output=%s", output)
			}
			if !test.failure && (err != nil || !strings.Contains(string(output), "transient-ok")) {
				t.Fatalf("output=%s err=%v", output, err)
			}
			if test.failure && !test.cancel && (!strings.Contains(err.Error(), "capture-failure") || !strings.Contains(string(output), "capture-failure") || strings.Contains(err.Error(), "user:token") || strings.Contains(string(output), "user:token")) {
				t.Fatalf("failure diagnostics output=%q err=%v", output, err)
			}
			if test.cancel && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("cancellation=%v", err)
			}
			after, err := backend.processEntry(t.Context(), target, "operator")
			if err != nil || after == nil || after.PID != baseline.PID || !after.IsRunning {
				t.Fatalf("operator restarted: before=%+v after=%+v err=%v", baseline, after, err)
			}
			missing, err := backend.processEntry(t.Context(), target, "capture")
			if err != nil || missing != nil {
				t.Fatalf("transient job altered shared supervisor: %+v %v", missing, err)
			}
		})
	}
}
