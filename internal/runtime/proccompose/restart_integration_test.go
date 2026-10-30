package proccompose

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ang-ee/angee-operator/internal/runtime"
)

// TestRestartPolicyHonoursStopAndDown runs a real process-compose supervisor:
// a process under on_failure that exits 1 is restarted, and a stop issued while
// it waits out its restart backoff (Backend.Stop, Backend.Down) leaves it down
// past the next backoff. process-compose marks a process it was asked to stop,
// and its restart loop honours the mark (v1.120.0, isRestartable).
func TestRestartPolicyHonoursStopAndDown(t *testing.T) {
	if _, err := exec.LookPath("process-compose"); err != nil {
		t.Skip("process-compose is not installed")
	}
	if testing.Short() {
		t.Skip("runs a process-compose supervisor")
	}
	for _, tt := range []struct {
		name string
		stop func(context.Context, Backend, runtime.Target) error
	}{
		{name: "stop", stop: func(ctx context.Context, b Backend, target runtime.Target) error {
			target.Services = []string{"crasher"}
			return b.Stop(ctx, target)
		}},
		{name: "down", stop: func(ctx context.Context, b Backend, target runtime.Target) error {
			return b.Down(ctx, target)
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			// Never the shared default log in $TMPDIR, which other stacks'
			// supervisors write to.
			t.Setenv("PC_LOG_FILE", filepath.Join(root, "process-compose.log"))
			marker := fmt.Sprintf("angee-restart-test-%d-%s", os.Getpid(), tt.name)
			document, err := Marshal(File{Version: "0.5", Processes: map[string]Process{
				"crasher": {
					Command:      fmt.Sprintf("sh -c 'echo %s; sleep 1; exit 1'", marker),
					Shutdown:     &Shutdown{TimeoutSeconds: 5, Signal: 15},
					Availability: &Availability{Restart: "on_failure"},
				},
			}})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "process-compose.yaml"), document, 0o644); err != nil {
				t.Fatal(err)
			}
			backend := NewBackend()
			target := runtime.Target{Root: root, ControlPort: scratchControlPort(t)}
			// Registered before Up, so a supervisor Up forked is torn down
			// even when Up then fails.
			t.Cleanup(func() { _ = backend.Down(context.Background(), target) })
			if err := backend.Up(t.Context(), target); err != nil {
				t.Fatalf("Up() error = %v", err)
			}

			restarting := awaitProcessState(t, backend, target, func(entry *processListEntry) bool {
				return entry.Status == "Restarting" && entry.Restarts >= 1
			})
			if err := tt.stop(t.Context(), backend, target); err != nil {
				t.Fatalf("stop error = %v", err)
			}
			// One backoff is a second; a restarted run would also print the
			// marker again and run for a second.
			time.Sleep(3 * time.Second)
			if tt.name == "down" {
				if serving, _ := backend.supervisorServing(t.Context(), target); serving {
					t.Fatal("supervisor still serves after Down")
				}
			} else {
				entry, err := backend.processEntry(t.Context(), target, "crasher")
				if err != nil || entry == nil {
					t.Fatalf("processEntry() = %+v, %v", entry, err)
				}
				if entry.Status != "Completed" || entry.IsRunning || entry.Restarts != restarting.Restarts {
					t.Fatalf("crasher = %+v after Stop, want Completed with %d restarts, not restarted", entry, restarting.Restarts)
				}
			}
			if out, err := exec.Command("pgrep", "-f", marker).Output(); err == nil {
				t.Fatalf("a crasher process still runs: pids %s", strings.TrimSpace(string(out)))
			}
		})
	}
}

// awaitProcessState waits up to ten seconds for the crasher's list entry to
// satisfy ok, and returns it.
func awaitProcessState(t *testing.T, backend Backend, target runtime.Target, ok func(*processListEntry) bool) processListEntry {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		entry, err := backend.processEntry(t.Context(), target, "crasher")
		if err == nil && entry != nil && ok(entry) {
			return *entry
		}
		if time.Now().After(deadline) {
			t.Fatalf("crasher did not reach the expected state: %+v, %v", entry, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// scratchControlPort returns a free loopback port from 28400 up, clear of the
// control ports live stacks use.
func scratchControlPort(t *testing.T) int {
	t.Helper()
	for port := 28400; port < 28500; port++ {
		listener, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			continue
		}
		_ = listener.Close()
		return port
	}
	t.Fatal("no free scratch port in 28400-28499")
	return 0
}
